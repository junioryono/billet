package fleetops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/state"
)

// decommissionConfig writes a minimal ec2-node config pointed at `endpoint`, with a
// state directory carrying a minted deployment id (unless mintID is false), and
// returns the config path. Compute-only (no ebs_s3), which is enough to exercise
// the instance-teardown flow; the cache purge is unit-tested against the store.
func decommissionConfig(t *testing.T, endpoint string, mintID bool) string {
	t.Helper()

	stateDir := t.TempDir()
	if mintID {
		if _, err := state.DeploymentID(stateDir); err != nil {
			t.Fatalf("mint deployment id: %v", err)
		}
	}

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  provider: ec2
  state_dir: ` + stateDir + `
  max_vcpu: 64
  max_memory: 256GiB
  ec2:
    region: us-west-2
    endpoint: ` + endpoint + `
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return configPath
}

// A NON-EC2 CONFIG has no out-of-Terraform resources for decommission to remove.
func TestDecommissionRefusesANonEC2Config(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  provider: docker
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := Decommission(t.Context(), processEnv(), []string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "this config has neither") {
		t.Fatalf("decommission did not refuse a non-ec2 config: %v", err)
	}
}

// metalDecommissionConfig writes a Firecracker node's config on AWS metal, with its
// site's ebs-s3 store and a minted deployment id, and returns its path.
func metalDecommissionConfig(t *testing.T) string {
	t.Helper()

	stateDir := t.TempDir()
	if _, err := state.DeploymentID(stateDir); err != nil {
		t.Fatalf("mint deployment id: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: ` + stateDir + `
  site: aws-us-west-2a
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return configPath
}

// A METAL SITE'S CACHE IS DECOMMISSIONED FROM ITS NODE'S CONFIG, with no instances of its
// own to list: its host is the pool's. Without --yes it reports and touches nothing.
func TestDecommissionPurgesAMetalSitesCacheAlone(t *testing.T) {
	configPath := metalDecommissionConfig(t)
	asked := 0
	none := func(_ context.Context, owner, region string, _ awscreds.Source) ([]ec2.HostRecord, error) {
		asked++
		if owner == "" || region != "us-west-2" {
			return nil, fmt.Errorf("the pool hosts were listed for owner %q in %q", owner, region)
		}

		return []ec2.HostRecord{}, nil
	}

	var err error
	out := capture(t, func() {
		err = decommission(t.Context(), processEnv(), []string{"--config", configPath}, none)
	})
	if err != nil {
		t.Fatalf("decommission of a metal node's config: %v", err)
	}
	if !strings.Contains(out, "would purge the ebs-s3 cache") {
		t.Errorf("the metal node's cache was not reported:\n%s", out)
	}
	if asked != 1 {
		t.Errorf("the pool hosts were asked about %d times before the purge, want once", asked)
	}
	if strings.Contains(out, "instance") {
		t.Errorf("a metal node's decommission spoke of instances:\n%s", out)
	}

	// WITHOUT --yes, so a mutant that dropped the refusal reports a dry run rather than
	// purging a real bucket under this machine's own credentials.
	err = decommission(t.Context(), processEnv(), []string{"--config", configPath, "--terminate-instances"}, none)
	if err == nil || !strings.Contains(err.Error(), "--terminate-instances") {
		t.Fatalf("--terminate-instances on a metal node was answered %v", err)
	}
}

// A METAL SITE'S CACHE IS NOT PURGED WHILE ANY OF ITS POOL HOSTS IS LEFT: a job there may
// be reading objects no clone lease names. A list that could not be read refuses too.
func TestDecommissionRefusesAMetalSiteWithAPoolHostLeft(t *testing.T) {
	configPath := metalDecommissionConfig(t)

	for name, tc := range map[string]struct {
		hosts poolHostLister
		want  string
	}{
		"a host left": {func(context.Context, string, string, awscreds.Source) ([]ec2.HostRecord, error) {
			return []ec2.HostRecord{{ID: "i-0123456789abcdef0", State: "stopped", Order: "p1"}}, nil
		}, "1 pool host(s) of this deployment are not terminated"},
		"a list that failed": {func(context.Context, string, string, awscreds.Source) ([]ec2.HostRecord, error) {
			return nil, errors.New("UnauthorizedOperation")
		}, "must all be gone before its cache is purged"},
	} {
		var err error
		out := capture(t, func() {
			err = decommission(t.Context(), processEnv(), []string{"--config", configPath, "--yes"}, tc.hosts)
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decommission answered %v; want a refusal saying %q", name, err, tc.want)
		}
		if strings.Contains(out, "purge") {
			t.Errorf("%s: the cache was reached with a pool host unaccounted for:\n%s", name, out)
		}
	}
}

// WITHOUT A RESOLVABLE IDENTITY decommission cannot tell which resources are this
// deployment's, so it refuses rather than risk touching another deployment's.
func TestDecommissionRefusesWithoutIdentity(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE"})
	configPath := decommissionConfig(t, endpoint, false) // no minted id

	err := Decommission(t.Context(), processEnv(), []string{"--config", configPath})
	if err == nil || !strings.Contains(err.Error(), "cannot resolve this deployment's identity") {
		t.Fatalf("decommission did not refuse without an identity: %v", err)
	}
}

// LIVE INSTANCES ARE FATAL without --terminate-instances: they may be running jobs.
func TestDecommissionRefusesLiveInstancesWithoutForce(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{
		accept:    "AKIDEXAMPLE",
		instances: []fakeInstance{{id: "i-abc123", name: "billet-job1"}},
	})
	configPath := decommissionConfig(t, endpoint, true)

	var err error
	out := capture(t, func() { err = Decommission(t.Context(), processEnv(), []string{"--config", configPath, "--yes"}) })
	if err == nil || !strings.Contains(err.Error(), "still live") {
		t.Fatalf("decommission did not refuse live instances: %v", err)
	}
	if !strings.Contains(out, "i-abc123") {
		t.Errorf("did not report the live instance:\n%s", out)
	}
}

// WITHOUT --yes decommission only reports, deleting nothing.
func TestDecommissionReportsWithoutYes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE"}) // no instances
	configPath := decommissionConfig(t, endpoint, true)

	var err error
	out := capture(t, func() { err = Decommission(t.Context(), processEnv(), []string{"--config", configPath}) })
	if err != nil {
		t.Fatalf("decommission report: %v", err)
	}
	if !strings.Contains(out, "none running for this deployment") {
		t.Errorf("did not report an empty fleet:\n%s", out)
	}
	if !strings.Contains(out, "nothing was deleted") {
		t.Errorf("did not say nothing was deleted:\n%s", out)
	}
}

// --terminate-instances --yes actually tears the leftover instances down.
func TestDecommissionTerminatesWithForceAndYes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{
		accept:    "AKIDEXAMPLE",
		instances: []fakeInstance{{id: "i-xyz789", name: "billet-job1"}},
		runRec:    rec,
	})
	configPath := decommissionConfig(t, endpoint, true)

	var err error
	out := capture(t, func() {
		err = Decommission(t.Context(), processEnv(), []string{
			"--config", configPath, "--terminate-instances", "--yes",
		})
	})
	if err != nil {
		t.Fatalf("decommission terminate: %v", err)
	}
	if !strings.Contains(out, "i-xyz789 terminated") {
		t.Errorf("did not report the termination:\n%s", out)
	}
	found := false
	for _, b := range rec.all() {
		if strings.Contains(b, "TerminateInstances") && strings.Contains(b, "i-xyz789") {
			found = true
		}
	}
	if !found {
		t.Error("the terminate never reached the API")
	}
}

// --terminate-instances is destructive, so it also needs --yes.
func TestDecommissionTerminateNeedsYes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{
		accept:    "AKIDEXAMPLE",
		instances: []fakeInstance{{id: "i-xyz789", name: "billet-job1"}},
		runRec:    rec,
	})
	configPath := decommissionConfig(t, endpoint, true)

	var err error
	capture(t, func() {
		err = Decommission(t.Context(), processEnv(), []string{"--config", configPath, "--terminate-instances"})
	})
	if err == nil || !strings.Contains(err.Error(), "needs --yes") {
		t.Fatalf("--terminate-instances without --yes was not refused: %v", err)
	}
	if len(rec.all()) != 0 {
		t.Error("an instance was terminated without --yes")
	}
}

// sentinelCreds records whether it was ever asked for credentials, so a test can
// prove the cache purge path never runs without --yes.
type sentinelCreds struct{ reached *bool }

func (s sentinelCreds) Credentials(context.Context) (awssig.Credentials, error) {
	*s.reached = true

	return awssig.Credentials{}, errors.New("credentials must not be resolved without --yes")
}

// THE CACHE --yes GATE: without --yes, decommissionCache reports only and never
// builds the store or resolves credentials. (The command tests are compute-only,
// so this exercises the guard directly.)
func TestDecommissionCacheYesGate(t *testing.T) {
	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderEC2,
		Site:     "home",
		EBSS3: &config.EBSS3Config{
			Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-x",
		},
	}}

	reached := false
	var err error
	out := capture(t, func() {
		err = decommissionCache(t.Context(), processEnv(), cfg, "deployment", sentinelCreds{&reached}, false)
	})
	if err != nil {
		t.Fatalf("decommissionCache without --yes errored: %v", err)
	}
	if !strings.Contains(out, "would purge") {
		t.Errorf("did not report the cache would be purged:\n%s", out)
	}
	if reached {
		t.Error("decommissionCache resolved credentials / touched the store without --yes")
	}
}
