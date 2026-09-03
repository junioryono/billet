package config

import (
	"net/url"
	"strings"
	"testing"
)

const nodeCacheBlock = `  cache:
    listen: 172.20.0.1:7718
    guest_endpoint: http://172.20.0.1:7718
`

func withNodeCache(t *testing.T, block string) string {
	t.Helper()

	body := strings.Replace(validConfig, cephBlock, cephBlock+block, 1)
	if body == validConfig {
		t.Fatal("the ceph block in validConfig has changed, so this case patches nothing")
	}

	return body
}

func TestAFirecrackerNodeMayExposeItsCacheOnlyOnTheGuestBridge(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withNodeCache(t, nodeCacheBlock)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.Cache == nil || cfg.Node.Cache.GuestEndpoint != "http://172.20.0.1:7718" {
		t.Fatalf("node cache config = %+v", cfg.Node.Cache)
	}
}

func TestTheGuestCacheListenerRefusesAnAddressOutsideItsBridgeContract(t *testing.T) {
	t.Parallel()

	for name, block := range map[string]string{
		"wildcard": `  cache:
    listen: 0.0.0.0:7718
    guest_endpoint: http://0.0.0.0:7718
`,
		"loopback": `  cache:
    listen: 127.0.0.1:7718
    guest_endpoint: http://127.0.0.1:7718
`,
		"different host": `  cache:
    listen: 172.20.0.1:7718
    guest_endpoint: http://172.21.0.1:7718
`,
		"https without a server certificate": `  cache:
    listen: 172.20.0.1:7718
    guest_endpoint: https://172.20.0.1:7718
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := Load(writeConfig(t, withNodeCache(t, block))); err == nil {
				t.Fatal("Load accepted a cache listener outside the guest bridge contract")
			}
		})
	}
}

func TestGuestCacheConfigurationIsRefusedByProvidersThatCannotAttachIt(t *testing.T) {
	t.Parallel()

	body := withNodeCache(t, nodeCacheBlock)
	body = strings.Replace(body, "  provider: firecracker\n", "  provider: docker\n", 1)
	body = strings.Replace(body, cephBlock, "", 1)
	body = strings.Replace(body, "  firecracker:\n    kernel_image: /var/lib/billet/vmlinux\n    bridge: br0\n", "", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a guest cache service on a docker node")
	}
	if !strings.Contains(err.Error(), "node.cache is set") {
		t.Errorf("the error does not name the inert section: %v", err)
	}
}

func TestEveryTierGetsASettableBuildKitCacheMountCeiling(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if got := cfg.Tiers[0].BuildKitCacheMountLimit; got != DefaultBuildKitCacheMountLimit {
		t.Errorf("default BuildKit cache-mount limit = %s, want %s",
			got, DefaultBuildKitCacheMountLimit)
	}

	body := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n",
		"    image: ubuntu-2404-x64\n    buildkit_cache_mount_limit: 4GiB\n", 1)
	cfg, err = Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load explicit limit: %v", err)
	}
	if got := cfg.Tiers[0].BuildKitCacheMountLimit; got != 4*GiB {
		t.Errorf("explicit BuildKit cache-mount limit = %s, want 4GiB", got)
	}
}

func TestABuildKitCacheMountCeilingCannotExceedAStickyDisk(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n",
		"    image: ubuntu-2404-x64\n    buildkit_cache_mount_limit: 101GiB\n", 1)
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a per-mount ceiling larger than any sticky disk")
	}
	if !strings.Contains(err.Error(), "buildkit_cache_mount_limit") ||
		!strings.Contains(err.Error(), "100GiB") {
		t.Fatalf("oversized mount-limit error = %v", err)
	}
}

func TestAFirecrackerNodeMayNameOnePullThroughCachePerPublicRegistry(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "  provider: firecracker\n", `  provider: firecracker
  registry_mirrors:
    docker.io: https://docker-cache.home.example
    ghcr.io: https://ghcr-cache.home.example
    quay.io: https://quay-cache.home.example
`, 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.RegistryMirrors == nil ||
		cfg.Node.RegistryMirrors.DockerIO != "https://docker-cache.home.example" ||
		cfg.Node.RegistryMirrors.GHCRIO != "https://ghcr-cache.home.example" ||
		cfg.Node.RegistryMirrors.QuayIO != "https://quay-cache.home.example" {
		t.Fatalf("registry mirrors = %+v", cfg.Node.RegistryMirrors)
	}
}

func TestRegistryMirrorsAreACompleteHTTPSSetOfDistinctInstances(t *testing.T) {
	t.Parallel()

	for name, block := range map[string]string{
		"partial": `  registry_mirrors:
    docker.io: https://docker-cache.home.example
`,
		"plain HTTP": `  registry_mirrors:
    docker.io: http://docker-cache.home.example
    ghcr.io: https://ghcr-cache.home.example
    quay.io: https://quay-cache.home.example
`,
		"path": `  registry_mirrors:
    docker.io: https://cache.home.example/docker
    ghcr.io: https://ghcr-cache.home.example
    quay.io: https://quay-cache.home.example
`,
		"invalid port": `  registry_mirrors:
    docker.io: https://docker-cache.home.example:99999
    ghcr.io: https://ghcr-cache.home.example
    quay.io: https://quay-cache.home.example
`,
		"shared instance": `  registry_mirrors:
    docker.io: https://cache.home.example
    ghcr.io: https://cache.home.example
    quay.io: https://cache.home.example
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := strings.Replace(validConfig, "  provider: firecracker\n",
				"  provider: firecracker\n"+block, 1)
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("Load accepted registry mirrors that cannot implement three isolated pull-through caches")
			}
		})
	}
}

// TWO SPELLINGS OF ONE ORIGIN ARE ONE INSTANCE, and comparing the raw strings
// saw three where there was one.
//
// Distribution's proxy mode accepts ONE upstream per process, which is the whole
// reason three instances are required. DNS host names are case-insensitive and
// 443 is https's default port, so a set differing only that way is one cache
// serving three upstreams — accepted, and then wrong in production with nothing
// pointing back at the config.
//
// A TABLE OF SPELLINGS WITH AN EXPECTED VERDICT, so the rule is stated as what
// billet must conclude rather than as the transformation it happens to apply.
//
// WHAT THE ACCEPTING DIRECTION PROVES IS NARROWER than "these are three
// processes", and saying otherwise would be an overclaim: two different origins
// can still be one Distribution behind two DNS names or two forwarded ports, and
// no amount of URL syntax can see that. Distinct ORIGINS is what a config file
// can be held to.
func TestEquivalentRegistryOriginsAreOneInstance(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		docker, ghcr string
		same         bool
	}{
		"identical":                {"https://cache.home.example", "https://cache.home.example", true},
		"DNS host case":            {"https://cache.home.example", "https://CACHE.home.example", true},
		"an explicit default port": {"https://cache.home.example", "https://cache.home.example:443", true},
		"both, at once":            {"https://CACHE.home.example:443", "https://cache.home.example", true},
		"one non-default port":     {"https://cache.home.example:8443", "https://cache.home.example", false},
		"two non-default ports":    {"https://cache.home.example:8443", "https://cache.home.example:9443", false},
		"different hosts":          {"https://a.home.example", "https://b.home.example", false},
		// Case folding must not go so far as to merge two different hosts.
		"different hosts, one uppercased": {"https://a.home.example", "https://B.home.example", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			block := "  registry_mirrors:\n    docker.io: " + tc.docker +
				"\n    ghcr.io: " + tc.ghcr + "\n    quay.io: https://quay-cache.home.example\n"
			body := strings.Replace(validConfig, "  provider: firecracker\n",
				"  provider: firecracker\n"+block, 1)

			_, err := Load(writeConfig(t, body))

			if !tc.same {
				if err != nil {
					t.Fatalf("two distinct origins were refused: %v", err)
				}

				// AND THEY REALLY ARE TWO. Accepting them is only correct if the
				// origins a client would address are different — otherwise this
				// case is the bug, passing.
				if a, b := originOf(t, tc.docker), originOf(t, tc.ghcr); a == b {
					t.Errorf("validation called these distinct, but both address the origin %s", a)
				}

				return
			}

			if err == nil {
				t.Fatal("Load accepted two spellings of one Distribution instance as two")
			}

			// THE OPERATOR'S OWN SPELLINGS, because those are what they have to
			// find in the file. Naming only the canonical origin would print a
			// string that appears in their config nowhere.
			for _, want := range []string{tc.docker, tc.ghcr, "docker.io", "ghcr.io"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// originOf is the service an endpoint addresses, through the production gate.
func originOf(t *testing.T, endpoint string) string {
	t.Helper()

	match := registryMirrorOriginRe.FindStringSubmatch(endpoint)
	if match == nil {
		t.Fatalf("the syntax gate rejected %q", endpoint)
	}

	return registryMirrorOrigin(match[1], match[2])
}

// CANONICALIZATION IS FOR COMPARISON ONLY. The endpoint the guest is handed must
// be the one the operator wrote, or billet has quietly rewritten a URL that is
// about to be put in a container runtime's configuration.
func TestAnAcceptedMirrorReachesTheGuestAsWritten(t *testing.T) {
	t.Parallel()

	block := "  registry_mirrors:\n    docker.io: https://DOCKER-cache.home.example:443\n" +
		"    ghcr.io: https://ghcr-cache.home.example\n" +
		"    quay.io: https://quay-cache.home.example:8443\n"
	body := strings.Replace(validConfig, "  provider: firecracker\n",
		"  provider: firecracker\n"+block, 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("three distinct origins were refused: %v", err)
	}

	if got := cfg.Node.RegistryMirrors.DockerIO; got != "https://DOCKER-cache.home.example:443" {
		t.Errorf("docker.io was rewritten to %q", got)
	}
	if got := cfg.Node.RegistryMirrors.QuayIO; got != "https://quay-cache.home.example:8443" {
		t.Errorf("quay.io was rewritten to %q", got)
	}
}

// A TERMINAL DNS ROOT DOT IS REFUSED RATHER THAN FOLDED INTO THE UNDOTTED FORM.
//
// That is a decision, recorded here so it is not re-argued: the syntax gate
// already requires the host to end alphanumeric, so there is no second accepted
// spelling for canonicalization to reconcile.
func TestARootDottedMirrorHostIsRefused(t *testing.T) {
	t.Parallel()

	block := "  registry_mirrors:\n    docker.io: https://docker-cache.home.example.\n" +
		"    ghcr.io: https://ghcr-cache.home.example\n" +
		"    quay.io: https://quay-cache.home.example\n"
	body := strings.Replace(validConfig, "  provider: firecracker\n",
		"  provider: firecracker\n"+block, 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a mirror host with a terminal root dot")
	}
	if !strings.Contains(err.Error(), "must be an HTTPS origin") {
		t.Errorf("it was not refused as a syntax error: %v", err)
	}
}

// BILLET'S NOTION OF AN ORIGIN AGREES WITH THE URL LIBRARY ANY CLIENT USES.
//
// The canonical form is built from the syntax pattern's own captures, so this is
// what stops that pattern and net/url drifting into reading one endpoint two
// ways — the question is what the guest's container runtime will address, not
// what a regexp matched.
func TestTheCanonicalOriginIsWhatAURLClientWouldAddress(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"https://cache.home.example",
		"https://CACHE.home.example",
		"https://cache.home.example:443",
		"https://cache.home.example:8443",
		"https://a1.b2.c3",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()

			match := registryMirrorOriginRe.FindStringSubmatch(endpoint)
			if match == nil {
				t.Fatalf("the syntax gate rejected %q", endpoint)
			}

			u, err := url.Parse(endpoint)
			if err != nil {
				t.Fatalf("net/url cannot parse an endpoint billet accepted: %v", err)
			}
			if u.Scheme != "https" || u.Path != "" || u.RawQuery != "" || u.User != nil {
				t.Fatalf("net/url reads %q as %+v, which is not a bare https origin", endpoint, u)
			}
			if !strings.EqualFold(u.Hostname(), match[1]) || u.Port() != match[2] {
				t.Fatalf("net/url reads host %q port %q; the pattern captured %q and %q",
					u.Hostname(), u.Port(), match[1], match[2])
			}

			// The default port is the one thing net/url does not fold for us, and
			// folding it is the point.
			want := "https://" + strings.ToLower(u.Hostname())
			if p := u.Port(); p != "" && p != "443" {
				want += ":" + p
			}
			if got := registryMirrorOrigin(match[1], match[2]); got != want {
				t.Errorf("origin of %q = %q, want %q", endpoint, got, want)
			}
		})
	}
}

func TestRegistryMirrorsAreRefusedWhereNoManagedGuestConsumesThem(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "  provider: firecracker\n", `  provider: docker
  registry_mirrors:
    docker.io: https://docker-cache.home.example
    ghcr.io: https://ghcr-cache.home.example
    quay.io: https://quay-cache.home.example
`, 1)
	body = strings.Replace(body, cephBlock, "", 1)
	body = strings.Replace(body, "  firecracker:\n    kernel_image: /var/lib/billet/vmlinux\n    bridge: br0\n", "", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "registry_mirrors") {
		t.Fatalf("inert registry mirrors error = %v", err)
	}
}
