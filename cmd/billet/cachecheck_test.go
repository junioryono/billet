package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A STORE WITH NO LISTENER IS A CACHE NOTHING CAN REACH, and it used to be
// silent in every direction: the config loaded, the node started, the jobs ran,
// and `billet check` reported a healthy deployment while every job pulled cold.
//
// Both directions of the guard are covered, because a report that fires on a
// correct deployment is one an operator learns to scroll past: the listener case
// must say the cache is served, and the case with neither a store nor a listener
// must say nothing about caches at all.
func TestJudgeNodeCache(t *testing.T) {
	ebsNode := func() *config.NodeConfig {
		return &config.NodeConfig{
			Name:     "aws-1",
			Provider: config.ProviderEC2,
			Site:     "aws",
			EC2:      &config.EC2Config{Region: "us-west-2", SubnetID: "subnet-0abc"},
			EBSS3: &config.EBSS3Config{
				Region:           "us-west-2",
				AvailabilityZone: "us-west-2a",
				Bucket:           "billet-cache-example",
			},
		}
	}
	cephNode := func() *config.NodeConfig {
		return &config.NodeConfig{
			Name:     "epyc-1",
			Provider: config.ProviderFirecracker,
			Site:     "home",
			Ceph:     &config.CephConfig{ImagePool: "billet-images", CachePool: "billet-cache"},
		}
	}
	// THE TIERS ARE PRESENT SO THE VERDICT CAN BE SHOWN NOT TO NAME THEM. Which
	// tiers can land on a node is placement's question — a node pin, a site, the
	// registered backend, the guest-OS policy and whether a shape fits — and
	// answering it here from a config would be a second copy of that rule, wrong
	// in both directions: it would name a tier pinned to a SIBLING node at the
	// same site, and miss one pinned to this node with no site at all.
	tiers := []config.Tier{
		{Label: "billet-ec2-8vcpu", Site: "aws"},
		{Label: "billet-4vcpu", Site: "home"},
		{Label: "billet-elsewhere", Site: "edge"},
		{Label: "billet-sibling-pinned", Site: "aws", Node: "aws-2"},
		{Label: "billet-node-pinned", Node: "aws-1"},
	}

	cases := map[string]struct {
		cfg     *config.Config
		fatal   bool
		want    []string
		unwant  []string
		silent  bool
		wantErr []string
	}{
		"an ebs-s3 store with no listener is refused, naming what the jobs get": {
			cfg:   &config.Config{Node: ebsNode(), Tiers: tiers},
			fatal: true,
			want: []string{
				"node.ebs_s3 names a cache store and this node offers no node.cache endpoint",
				"keeps /var/lib/docker on the instance's ROOT VOLUME",
				// SCOPED TO THIS NODE AND TO BILLET'S CACHE, both of them. An AMI
				// with images baked in makes "pulls every image cold" false, and a
				// bucket other deployments write to makes "no state object is ever
				// created in it" false — either one is a sentence an operator can
				// disprove by looking, in a report they have to trust.
				"starting at whatever the AMI baked in",
				"no state object under this deployment's prefix in billet-cache-example",
				"add node.cache",
			},
			// THE VERDICT IS ABOUT THIS NODE AND NAMES NO TIER. Placement decides
			// which tiers land here, and a list built from the config would name a
			// tier pinned to a sibling node at this site while missing one pinned
			// to this node with no site — an operator would then go and edit a
			// tier that is running perfectly well somewhere else.
			unwant: []string{"billet-ec2-8vcpu", "billet-sibling-pinned", "billet-node-pinned"},
			wantErr: []string{
				"node.ebs_s3", "node.cache", "root volume",
				"billet decommission", "billet init iam",
			},
		},
		"a fleet node file, whose tiers live on the control plane, is refused too": {
			cfg:   &config.Config{Node: ebsNode()},
			fatal: true,
			want:  []string{"NONE: node.ebs_s3", "instance's ROOT VOLUME"},
		},
		"a store with a listener reports the endpoint it serves": {
			cfg: func() *config.Config {
				n := ebsNode()
				n.Cache = &config.NodeCacheConfig{
					Listen:        "10.0.1.10:7718",
					GuestEndpoint: "https://cache.aws.example:7718",
				}

				return &config.Config{Node: n, Tiers: tiers}
			}(),
			want: []string{
				"guests are handed https://cache.aws.example:7718",
				"ebs-s3 store in us-west-2a (bucket billet-cache-example)",
				// SAYS WHAT IT ESTABLISHED. Nothing here dials the listener, and
				// a line that read as though it had is the failure this command
				// avoids everywhere else.
				"(configured, not probed)",
			},
			unwant: []string{"NONE", "ROOT VOLUME"},
		},
		"a listener with no store behind it is not reported as working": {
			cfg: &config.Config{Node: &config.NodeConfig{
				Name:     "aws-1",
				Provider: config.ProviderEC2,
				Cache: &config.NodeCacheConfig{
					Listen:        "10.0.1.10:7718",
					GuestEndpoint: "https://cache.aws.example:7718",
				},
			}},
			want: []string{"NO STORE", "will refuse to start"},
		},
		"a ceph store with no listener is reported and NOT refused": {
			cfg: &config.Config{Node: cephNode(), Tiers: tiers},
			want: []string{
				"node.ceph.cache_pool billet-cache is a cache nothing can reach",
				"gets no billet cache volume",
				"no published generation",
				// THE LOSS IS BILLET'S CACHE, NOT GITHUB'S. Interception is off
				// without the listener, so actions/cache behaves exactly as it does
				// on a hosted runner — and a report that called it unavailable
				// would send an operator to debug a step that works.
				"actions/cache still reaches GitHub",
				"image_pool billet-images is unaffected",
			},
			unwant: []string{"NONE", "billet-4vcpu"},
		},
		"a ceph store with a listener reports the endpoint it serves": {
			cfg: func() *config.Config {
				n := cephNode()
				n.Cache = &config.NodeCacheConfig{
					Listen:        "10.200.0.1:7719",
					GuestEndpoint: "http://10.200.0.1:7719",
				}

				return &config.Config{Node: n}
			}(),
			want:   []string{"guests are handed http://10.200.0.1:7719, served from the ceph pool billet-cache"},
			unwant: []string{"nothing can reach"},
		},
		"a node with no store at all says nothing about caches": {
			cfg: &config.Config{Node: &config.NodeConfig{
				Name: "trial", Provider: config.ProviderDocker,
			}, Tiers: tiers},
			silent: true,
		},
		"a control-plane-only file has no node to judge": {
			cfg:    &config.Config{Tiers: tiers, Sites: []config.SiteConfig{{Name: "aws"}}},
			silent: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lines, err := judgeNodeCache(tc.cfg)

			switch {
			case tc.fatal && err == nil:
				t.Fatalf("a store nothing can reach was not refused:\n%s", strings.Join(lines, "\n"))
			case !tc.fatal && err != nil:
				t.Fatalf("a deployment that works was refused: %v", err)
			}

			if tc.silent && len(lines) > 0 {
				t.Fatalf("a config with nothing to say about caches said:\n%s",
					strings.Join(lines, "\n"))
			}

			report := strings.Join(lines, "\n")
			for _, want := range tc.want {
				if !strings.Contains(report, want) {
					t.Errorf("the report does not say %q:\n%s", want, report)
				}
			}
			for _, unwant := range tc.unwant {
				if strings.Contains(report, unwant) {
					t.Errorf("the report says %q and should not:\n%s", unwant, report)
				}
			}
			for _, want := range tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}

// THE JUDGE IS REACHED BY THE COMMAND, which is the half a unit test on the
// function cannot establish: the verdict existed and nothing called it would
// leave `billet check` exactly as silent as the deployment this fixes.
//
// It also proves the refusal is DEFERRED rather than returned in place — the
// tier section still prints after it — for the same reason the GitHub verdict is:
// a check an operator ran because something is wrong must not answer with one
// finding and hide the rest.
func TestCheckRefusesAnEBSStoreWithNoCacheListener(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available"})

	// WITHOUT the listener: refused, and the report names what the jobs get.
	configPath := writeCacheCheckConfig(t, endpoint, "")

	var err error
	out := capture(t, func() { err = cmdCheck(t.Context(), []string{"--config", configPath}) })

	if err == nil {
		t.Fatalf("a cache store nothing can reach passed the check:\n%s", out)
	}
	for _, want := range []string{"node.ebs_s3", "node.cache"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	printedOnce(t, out, "cache    NONE: node.ebs_s3 names a cache store")
	if !strings.Contains(out, "instance's ROOT VOLUME") {
		t.Errorf("the report does not say what the jobs get instead:\n%s", out)
	}
	if !strings.Contains(out, "tiers    ") {
		t.Errorf("the cache verdict aborted the sections after it:\n%s", out)
	}

	// WITH the listener: the same deployment passes, and the cache is reported as
	// served. A guard that cannot pass is one an operator routes around.
	served := `
  cache:
    listen: 10.0.1.10:7718
    guest_endpoint: https://cache.aws.example:7718
    tls_cert: /etc/billet/cache/tls.crt
    tls_key: /etc/billet/cache/tls.key`
	configPath = writeCacheCheckConfig(t, endpoint, served)

	out = capture(t, func() { err = cmdCheck(t.Context(), []string{"--config", configPath}) })

	if err != nil {
		t.Fatalf("a node with a cache listener failed the check: %v\n%s", err, out)
	}
	printedOnce(t, out, "cache    guests are handed https://cache.aws.example:7718")
	if strings.Contains(out, "NONE") {
		t.Errorf("a working cache was reported as absent:\n%s", out)
	}
}

// NEITHER FINDING HIDES THE OTHER, on the host where both are most likely: one
// still being commissioned, which commonly has no usable AWS credentials and no
// readable certificate bundle either. Both of those end the run before the rest
// of the node section, so the verdict has to be decided first and carried out
// with them — otherwise the operator fixes AWS, re-runs, fixes the certificate,
// re-runs, and only on the third pass learns the cache was never wired up.
//
// THE PRINTED REPORT IS ASSERTED AS WELL AS THE ERROR. Checking the error alone
// stays green if the report moves below the failing probe, which is exactly the
// regression this guards.
func TestCheckCarriesTheCacheVerdictPastAnEarlierFailure(t *testing.T) {
	t.Run("credentials the api refuses", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

		// A fake that accepts some OTHER identity, so the credential probe fails
		// the way an expired key or a role for another account does.
		configPath := writeCacheCheckConfig(t, fakeEC2(t, "SOME-OTHER-KEY"), "")

		var err error
		out := capture(t, func() { err = cmdCheck(t.Context(), []string{"--config", configPath}) })

		if err == nil {
			t.Fatalf("credentials the api refuses passed the check:\n%s", out)
		}
		for _, want := range []string{"node.ebs_s3", "could not call the ec2 api"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the check's error does not carry %q: %v", want, err)
			}
		}
		printedOnce(t, out, "cache    NONE: node.ebs_s3 names a cache store")
	})

	// THE IDENTITY IS THE HARDER HALF: the node's name is filled in FROM the
	// certificate, so the header cannot be printed before the bundle loads — and
	// the verdict must be decided before it anyway, since it depends on none of
	// it.
	t.Run("a certificate bundle that cannot be read", func(t *testing.T) {
		dir := t.TempDir()
		tls := fmt.Sprintf(`
  tls:
    cert: %s
    key: %s
    ca: %s`,
			filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"),
			filepath.Join(dir, "ca.crt"))
		configPath := writeCacheCheckConfig(t, fakeEC2(t, "AKIDEXAMPLE"), "", tls)

		var err error
		out := capture(t, func() { err = cmdCheck(t.Context(), []string{"--config", configPath}) })

		if err == nil {
			t.Fatalf("an unreadable node certificate passed the check:\n%s", out)
		}
		if !strings.Contains(err.Error(), "node identity") {
			t.Errorf("the identity failure is not reported: %v", err)
		}
		if !strings.Contains(err.Error(), "node.ebs_s3") {
			t.Errorf("the identity failure swallowed the cache verdict: %v", err)
		}
		printedOnce(t, out, "cache    NONE: node.ebs_s3 names a cache store")
	})
}

// ONE FINDING, NOT FOUR. The verdict is several sentences, and the report's own
// convention is that a label opens a finding and its continuations are indented
// under it — so a renderer that labelled every line would turn one cache verdict
// into four, in a report an operator scans by label.
//
// COMPARED WHOLE rather than by substring, because that is exactly what the
// containment assertions elsewhere in this file cannot see: each continuation
// could acquire its own label and every one of them would stay green.
func TestPrintNodeCacheLabelsTheFindingOnce(t *testing.T) {
	out := capture(t, func() {
		printNodeCache([]string{"the finding", "what it means", "the remedy"})
	})

	const want = "cache    the finding\n" +
		"         what it means\n" +
		"         the remedy\n"

	if out != want {
		t.Errorf("the verdict does not render as one labelled finding:\ngot:\n%s\nwant:\n%s",
			out, want)
	}

	// A NODE WITH NOTHING TO SAY SAYS NOTHING, and prints no bare label either.
	if empty := capture(t, func() { printNodeCache(nil) }); empty != "" {
		t.Errorf("an empty verdict printed %q", empty)
	}
}

// printedOnce asserts a report line appears EXACTLY once.
//
// THE VERDICT HAS TWO PRINT SITES, because the node header's name comes from the
// certificate and the identity-failure path has no header to print — so the two
// are mutually exclusive by construction, and `strings.Contains` cannot tell
// that construction from one that prints twice. Moving the ordinary call above
// the bundle would double the identity path's output and pass every containment
// assertion in this file.
func printedOnce(t *testing.T, out, line string) {
	t.Helper()

	if n := strings.Count(out, line); n != 1 {
		t.Errorf("%q appears %d times, want exactly 1:\n%s", line, n, out)
	}
}

// writeCacheCheckConfig writes the shape the issue measured: an ec2 node with an
// ebs-s3 store, a site whose store is that one, and a tier pinned to it. Each
// extra argument is appended inside the node block, so a caller can add the
// cache listener or a certificate bundle to the same fixture.
//
// LOOPBACK AND NO CERTIFICATE BY DEFAULT, deliberately. `authorizeOwner` then
// finds no deployment identity, so ec2Preflight SKIPS its bucket probe — which
// is what keeps this test off the network and out of somebody's S3 account.
func writeCacheCheckConfig(t *testing.T, ec2Endpoint string, nodeBlocks ...string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
node:
  name: aws-1
  server_addr: 127.0.0.1:7717
  provider: ec2
  state_dir: ` + t.TempDir() + `
  site: aws
  max_vcpu: 64
  max_memory: 256GiB
  ec2:
    region: us-west-2
    endpoint: ` + ec2Endpoint + `
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    untrusted_security_group_ids: [sg-0def]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example` + strings.Join(nodeBlocks, "") + `
sites:
  - name: aws
    store: ebs-s3
tiers:
  - label: billet-ec2-8vcpu
    provider: ec2
    site: aws
    vcpu: 8
    memory: 16GiB
    image: ami-good
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}
