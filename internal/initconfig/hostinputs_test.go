package initconfig_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// firecrackerParams is a generation that should succeed, so a case can break
// exactly one thing.
func firecrackerParams() initconfig.Params {
	return initconfig.Params{
		Org:      "acme",
		Provider: config.ProviderFirecracker,
		VCPU:     32,
		Memory:   64 * config.GiB,
		GOOS:     "linux",
	}
}

// THE SIX VALUES A REAL HOST HAD SET AND A GENERATION COULD NOT KNOW.
//
// The 2026-08-26 measurement: a generated block diffed against an inventory
// written by hand months earlier reproduced everything billet could know —
// listen, both state directories, the App identity, the provider, the lock
// directory, both bridges, both Ceph pools, and a tier ladder unprompted. This
// was the remainder, and until now none of it had a flag: the operator answered
// the question and then had to answer it again by editing the file.
func TestAFirecrackerGenerationCarriesWhatOnlyTheHostKnows(t *testing.T) {
	t.Parallel()

	p := firecrackerParams()
	p.Host = initconfig.HostInputs{
		KernelImage:        "/var/lib/billet/kernels/vmlinux-6.1.155-abc123",
		CephUser:           "billet-node1",
		CephKeyringPath:    "/etc/ceph/ceph.client.billet-node1.keyring",
		CacheListen:        "10.0.0.5:8080",
		CacheGuestEndpoint: "http://10.0.0.5:8080",
	}

	cfg := parseGenerated(t, generateAny(t, p))

	switch {
	case cfg.Node.Firecracker == nil:
		t.Fatal("no firecracker block")
	case cfg.Node.Firecracker.KernelImage != p.Host.KernelImage:
		t.Errorf("the pinned kernel is %q; a real host names a version and the conventional "+
			"vmlinux is only a fallback", cfg.Node.Firecracker.KernelImage)
	}

	switch {
	case cfg.Node.Ceph == nil:
		t.Fatal("no ceph block")
	case cfg.Node.Ceph.User != "billet-node1":
		t.Errorf("the RADOS identity is %q", cfg.Node.Ceph.User)
	case cfg.Node.Ceph.KeyringPath != p.Host.CephKeyringPath:
		t.Errorf("the keyring is %q", cfg.Node.Ceph.KeyringPath)
	}

	switch {
	case cfg.Node.Cache == nil:
		t.Fatal("no cache block, so a host that answered the question has to answer it again")
	case cfg.Node.Cache.Listen != "10.0.0.5:8080":
		t.Errorf("the cache listens on %q", cfg.Node.Cache.Listen)
	case cfg.Node.Cache.GuestEndpoint != "http://10.0.0.5:8080":
		t.Errorf("the guest endpoint is %q", cfg.Node.Cache.GuestEndpoint)
	}
}

// AND A GENERATION THAT SUPPLIES NONE OF THEM IS UNCHANGED.
//
// The other direction, and the one that keeps this from being a behaviour change
// for every existing user: omitted, each key means something specific — no user
// is `billet`, no keyring is Ceph's own search path, no cache block is no cache —
// so writing them blank would put three decisions in the file that nobody made.
func TestAFirecrackerGenerationWithoutHostInputsIsUnchanged(t *testing.T) {
	t.Parallel()

	cfg := parseGenerated(t, generateAny(t, firecrackerParams()))

	if cfg.Node.Cache != nil {
		t.Errorf("a cache block was written for a generation that named no cache: %+v",
			cfg.Node.Cache)
	}

	if cfg.Node.Ceph.KeyringPath != "" {
		t.Errorf("a keyring was written where Ceph's own search path was meant: %q",
			cfg.Node.Ceph.KeyringPath)
	}

	// The kernel falls back to the conventional path rather than to nothing.
	if !strings.HasSuffix(cfg.Node.Firecracker.KernelImage, "vmlinux") {
		t.Errorf("the fallback kernel is %q", cfg.Node.Firecracker.KernelImage)
	}
}

// HALF A CACHE IS REFUSED. The guest endpoint is the origin placed in a guest's
// metadata and must name the same address the cache listens on, so one without
// the other is a cache that is configured and unreachable.
func TestHalfACacheIsRefused(t *testing.T) {
	t.Parallel()

	for name, host := range map[string]initconfig.HostInputs{
		"a listener with no endpoint": {CacheListen: "10.0.0.5:8080"},
		"an endpoint with no listener": {
			CacheGuestEndpoint: "http://10.0.0.5:8080",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := firecrackerParams()
			p.Host = host

			_, _, err := initconfig.Generate(p)
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}

			if !strings.Contains(err.Error(), "--cache-listen") {
				t.Errorf("the refusal does not name the pair: %v", err)
			}
		})
	}
}

// HOST INPUTS ON ANOTHER BACKEND ARE REFUSED, NOT DISCARDED.
//
// A pinned kernel, a Ceph identity and a guest-reachable cache are
// node.firecracker, node.ceph and node.cache on a microVM host — and config
// validation refuses node.ceph outright on every other backend, so accepting
// them elsewhere would either write a file that cannot load or discard them in
// silence.
func TestHostInputsOnAnotherBackendAreRefused(t *testing.T) {
	t.Parallel()

	p := initconfig.Params{
		Org:         "acme",
		Provider:    config.ProviderDocker,
		RunnerGroup: "billet",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		VCPU:        16,
		Memory:      32 * config.GiB,
		GOOS:        "linux",
		Host:        initconfig.HostInputs{CephUser: "billet-node1"},
	}

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("host inputs were silently discarded on a docker generation")
	}

	if !strings.Contains(err.Error(), "host inputs are set") {
		t.Errorf("the refusal does not name what would have been discarded: %v", err)
	}
}

// generateAny is `generate` without the trusted assertion, since a firecracker
// generation is legitimately untrusted: a microVM isolates the kernel, so this
// is the backend that runs work billet cannot vouch for.
func generateAny(t *testing.T, p initconfig.Params) string {
	t.Helper()

	body, _, err := initconfig.Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return body
}
