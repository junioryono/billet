package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// metalNodeConfig writes a Firecracker node's config on AWS metal, with its site's
// ebs-s3 store, and returns its path.
func metalNodeConfig(t *testing.T, kms string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  name: pool-p1
  provider: firecracker
  state_dir: ` + t.TempDir() + `
  site: aws-us-west-2a
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
    prefix: deployments/example
` + kms + `  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
  cache:
    listen: 10.240.0.1:7718
    guest_endpoint: http://10.240.0.1:7718
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// A FIRECRACKER NODE ON AWS METAL GETS ITS CACHE STORE'S ROLE AND NOTHING THAT LAUNCHES,
// with the instance half of its self-attach scoped to the deployment's pool hosts.
func TestInitIAMPrintsAMetalNodesCacheRole(t *testing.T) {
	path := metalNodeConfig(t, "    kms_key_id: arn:aws:kms:us-west-2:123456789012:key/cache\n")

	var err error
	out := capture(t, func() {
		err = Init(t.Context(), processEnv(), []string{"iam", "--config", path, "--deployment", testDeployID})
	})
	if err != nil {
		t.Fatalf("init iam for a metal node: %v", err)
	}

	got := parsePolicy(t, out)
	for _, sid := range got {
		if strings.HasPrefix(sid, "BilletRuntime") {
			t.Fatalf("a node that launches nothing was granted %s:\n%s", sid, out)
		}
	}
	for _, need := range []string{"BilletCacheCreateVolume", "BilletCacheAttach", "BilletCacheSelfAttach",
		"BilletCacheObjects", "BilletCacheKMSUse", "BilletCacheTag"} {
		if !slices.Contains(got, need) {
			t.Errorf("the metal node's role has no %s: %v", need, got)
		}
	}

	var policy struct {
		Statement []struct {
			Sid       string
			Action    []string
			Resource  []string
			Condition map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(out), &policy); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range policy.Statement {
		switch stmt.Sid {
		case "BilletCacheSelfAttach":
			if !slices.Equal(stmt.Action, []string{"ec2:AttachVolume", "ec2:DetachVolume"}) ||
				!slices.Equal(stmt.Resource, []string{"arn:aws:ec2:*:*:instance/*"}) {
				t.Fatalf("the self-attach grant is %+v", stmt)
			}
			if owner := stmt.Condition["StringEquals"]["aws:ResourceTag/sh.billet.pool-owner"]; owner != testDeployID {
				t.Fatalf("the self-attach grant reaches instances whose pool owner is %v, not this deployment's", owner)
			}
		case "BilletCacheAttach":
			// THE VOLUME ALONE: on "*" this grant would let the node attach to any instance
			// carrying billet's owner tag, an ec2 node's job among them, whatever the
			// self-attach grant's pool-owner condition says.
			if !slices.Equal(stmt.Resource, []string{"arn:aws:ec2:*:*:volume/*"}) {
				t.Fatalf("the cache's attach grant reaches %v, want volumes alone", stmt.Resource)
			}
		case "BilletCacheTag":
			// THE TWO CREATES THE STORE MAKES, AND NO LAUNCH: a node that launches nothing
			// may not tag a launch either.
			if !slices.Equal(stmt.Action, []string{"ec2:CreateTags"}) ||
				!slices.Equal(stmt.Resource, []string{"arn:aws:ec2:*:*:volume/*", "arn:aws:ec2:*::snapshot/*"}) {
				t.Fatalf("the tag grant is %+v", stmt)
			}
			creates, err := json.Marshal(stmt.Condition["StringEquals"]["ec2:CreateAction"])
			if err != nil || string(creates) != `["CreateVolume","CreateSnapshot"]` {
				t.Fatalf("the tag grant is for creates %s, want the store's two alone", creates)
			}
			if owner := stmt.Condition["StringEquals"]["aws:RequestTag/sh.billet.owner"]; owner != testDeployID {
				t.Fatalf("the tag grant tags for owner %v, not this deployment", owner)
			}
		}
	}
}

// AN EC2 OR A BUILDER'S FLAG ON A METAL NODE IS REFUSED, and a host provider with no
// cache store is refused as before.
func TestInitIAMRefusesWhatAMetalNodeDoesNotDo(t *testing.T) {
	path := metalNodeConfig(t, "")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a builder": {[]string{"--config", path, "--deployment", testDeployID, "--builder"},
			"holds its cache store alone"},
		"a role": {[]string{"--config", path, "--deployment", testDeployID, "--role-arn",
			"arn:aws:iam::123456789012:role/x"}, "holds its cache store alone"},
		"an account": {[]string{"--config", path, "--deployment", testDeployID, "--account", "123456789012"},
			"holds its cache store alone"},
		"no owner": {[]string{"--config", path}, "could not determine this deployment's identity"},
		"a key that is not an ARN": {[]string{"--config", metalNodeConfig(t, "    kms_key_id: alias/cache\n"),
			"--deployment", testDeployID}, "not a key ARN"},
	} {
		var err error
		capture(t, func() { err = Init(t.Context(), processEnv(), append([]string{"iam"}, tc.args...)) })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("init iam for a metal node with %s answered %v; want a refusal saying %q", name, err, tc.want)
		}
	}

	// A HOST PROVIDER WITH NO CACHE STORE IS REFUSED AS BEFORE: it calls no AWS API.
	plain := filepath.Join(t.TempDir(), "billet.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.Index(body, []byte("  ebs_s3:"))
	rest := bytes.Index(body, []byte("  firecracker:"))
	if err := os.WriteFile(plain, append(body[:cut:cut], body[rest:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	capture(t, func() {
		err = Init(t.Context(), processEnv(), []string{"iam", "--config", plain, "--deployment", testDeployID})
	})
	if err == nil || !strings.Contains(err.Error(), "describes a REMOTE node's role") {
		t.Errorf("init iam for a Firecracker node with no store answered %v", err)
	}
}
