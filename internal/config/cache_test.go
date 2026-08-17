package config

import (
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
