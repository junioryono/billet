package scripts_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/scripts"
)

// EVERY REGION THE EC2 IMAGE IS CUT FROM IS STILL MARKED, and holds what it is cut for,
// so moving a line out of one in the guest's agent fails here rather than on an image.
func TestEveryRegionTheEC2ImageUsesIsMarkedAndWhole(t *testing.T) {
	t.Parallel()
	for marker, wants := range map[string][]string{
		"BILLET_SHARED_PREPARE": {"docker_gateway=172.17.0.1", "fetch actions-proxy", `guest_caches=$(fetch guest-caches`},
		"BILLET_SHARED_START":   {"python_runtime=", "billet-actions-proxy", "/usr/sbin/dnsmasq"},
		"BILLET_CACHE_RELAY":    {"--mode node-relay"},
		"BILLET_SHARED_ENV":     {"GOCACHEPROG=", "ACTIONS_RUNNER_CONTAINER_HOOKS="},
		"BILLET_SHARED_INSTALL": {"install_container_hook() {"},
		"BILLET_SHARED_SUDOERS": {"install_cache_sudoers() {"},
	} {
		block, err := scripts.GuestImageBlock(marker)
		if err != nil {
			t.Errorf("%s: %v", marker, err)

			continue
		}
		for _, want := range wants {
			if !strings.Contains(block, want) {
				t.Errorf("%s no longer holds %q", marker, want)
			}
		}
		if strings.Contains(block, marker+"_END") {
			t.Errorf("%s ran past its own end marker", marker)
		}
		if !strings.HasSuffix(block, "\n") {
			t.Errorf("%s does not end in whole lines", marker)
		}
	}

	if _, err := scripts.GuestImageBlock("BILLET_NO_SUCH_REGION"); err == nil {
		t.Error("a region the guest build does not mark was found")
	}
}

// NOTHING IN A SHARED REGION REACHES FOR MMDS BUT `fetch`, which the EC2 image answers
// from its user data: a direct read would fail there, in a way only a real boot shows.
func TestNoSharedRegionReadsTheMetadataServiceDirectly(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{"BILLET_SHARED_PREPARE", "BILLET_SHARED_START", "BILLET_CACHE_RELAY", "BILLET_SHARED_ENV"} {
		block, err := scripts.GuestImageBlock(marker)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, banned := range []string{"$MMDS", "169.254.169.254", "$token"} {
				if strings.Contains(line, banned) {
					t.Errorf("%s reads the metadata service directly: %s", marker, trimmed)
				}
			}
		}
	}
}
