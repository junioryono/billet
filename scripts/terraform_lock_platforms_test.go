package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lockPlatforms is how many platforms' h1 checksums every provider in a
// committed lock records: linux_amd64, linux_arm64, darwin_arm64 and
// darwin_amd64.
const lockPlatforms = 4

// EVERY LOCK RECORDS EVERY PLATFORM'S h1, OR CI'S PROVIDER CACHE IS NEVER USED:
// terraform takes a cached provider only when the lock holds the h1 checksum of
// the platform it runs on (measured with 1.14.0 and 1.14.1), and an init on one
// machine writes back that machine's alone. A lock left so passes every other
// gate while each Linux init downloads its providers afresh, which is how the
// terraform legs were cancelled at their limit on 2026-10-17. The fix is
// `terraform providers lock -platform=linux_amd64 -platform=linux_arm64
// -platform=darwin_arm64 -platform=darwin_amd64` in the module.
func TestEveryTerraformLockRecordsEveryPlatformsChecksum(t *testing.T) {
	t.Parallel()

	locks, err := filepath.Glob("../terraform/modules/*/.terraform.lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) == 0 {
		t.Fatal("no terraform lock files found under terraform/modules")
	}

	block := regexp.MustCompile(`(?s)provider "([^"]+)" \{.*?hashes = \[(.*?)\]`)

	for _, lock := range locks {
		body, err := os.ReadFile(lock)
		if err != nil {
			t.Fatal(err)
		}

		providers := block.FindAllStringSubmatch(string(body), -1)
		if len(providers) == 0 {
			t.Errorf("%s: no provider block read", lock)
		}

		for _, p := range providers {
			if got := strings.Count(p[2], `"h1:`); got < lockPlatforms {
				t.Errorf("%s: %s records %d h1 checksum(s), want one for each of %d platforms; run terraform "+
					"providers lock with -platform for linux_amd64, linux_arm64, darwin_arm64 and darwin_amd64",
					lock, p[1], got, lockPlatforms)
			}
		}
	}
}
