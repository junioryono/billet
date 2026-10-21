package alloc

import (
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// whyNoRoom asserts the read succeeded before returning the refusal.
func whyNoRoom(t *testing.T, a *Allocator, tier string) RoomRefusal {
	t.Helper()

	r, err := a.WhyNoRoom(t.Context(), tier)
	if err != nil {
		t.Fatalf("WhyNoRoom(%s): %v", tier, err)
	}

	return r
}

// A REFUSAL NAMES EVERY LIMIT THAT BINDS, AND ONLY THOSE (#248). Each case
// below is one limit, with the others left room, so a reason that named the
// wrong one, or named one that does not bind, is what fails.
func TestWhyNoRoomNamesTheLimitThatBinds(t *testing.T) {
	t.Run("hosts", func(t *testing.T) {
		pinned := tier("pinned", 8, 16*config.GiB)
		pinned.Node = "small-host"
		a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{pinned})
		mustRegister(t, a, NodeRegistration{
			Name: "small-host", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB,
		})

		if got := whyNoRoom(t, a, "pinned"); got.Binds() {
			t.Fatalf("an empty host reports %+v; this case cannot tell a full host from an empty one", got)
		}

		reserve(t, a, "pinned")

		got := whyNoRoom(t, a, "pinned")
		if !slices.Equal(got.HostsFull, []string{"small-host"}) || got.CeilingBound || got.AtCap != 0 ||
			got.Sealed || got.NoHost {
			t.Errorf("a full host reports %+v, want only its host named", got)
		}

		if !strings.Contains(got.String(), "no room on its hosts (small-host)") {
			t.Errorf("the reason %q does not name the full host", got)
		}
	})

	t.Run("ceiling", func(t *testing.T) {
		a := newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 128 * config.GiB},
			[]config.Tier{tier("work", 4, 8*config.GiB)})
		mustRegister(t, a, NodeRegistration{
			Name: "host", Provider: config.ProviderFirecracker, VCPU: 64, Memory: 128 * config.GiB,
		})

		reserve(t, a, "work")
		reserve(t, a, "work")

		got := whyNoRoom(t, a, "work")
		if !got.CeilingBound || len(got.HostsFull) != 0 {
			t.Errorf("a spent ceiling over a roomy host reports %+v, want the ceiling and not the host", got)
		}

		if !strings.Contains(got.String(), "deployment ceiling") {
			t.Errorf("the reason %q does not name the ceiling", got)
		}
	})

	t.Run("cap", func(t *testing.T) {
		capped := tier("capped", 4, 8*config.GiB)
		capped.MaxConcurrent = 1
		a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{capped})
		mustRegister(t, a, NodeRegistration{
			Name: "host", Provider: config.ProviderFirecracker, VCPU: 64, Memory: 128 * config.GiB,
		})

		reserve(t, a, "capped")

		got := whyNoRoom(t, a, "capped")
		if got.AtCap != 1 || got.CeilingBound || len(got.HostsFull) != 0 {
			t.Errorf("a tier at its cap on a roomy fleet reports %+v, want only its cap", got)
		}

		if !strings.Contains(got.String(), "max_concurrent of 1") {
			t.Errorf("the reason %q does not name the cap", got)
		}
	})

	t.Run("share", func(t *testing.T) {
		a := shareAllocator(t)

		reserve(t, a, "repo-16")
		reserve(t, a, "repo-8")

		got := whyNoRoom(t, a, "repo-8")
		if !got.ShareBound || got.CeilingBound || len(got.HostsFull) != 0 {
			t.Errorf("a spent share on a roomy fleet reports %+v, want only the share", got)
		}

		if !strings.Contains(got.String(), "target's share") {
			t.Errorf("the reason %q does not name the share", got)
		}
	})

	t.Run("no host", func(t *testing.T) {
		pinned := tier("pinned", 4, 8*config.GiB)
		pinned.Node = "gone"
		a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{pinned})
		mustRegister(t, a, NodeRegistration{
			Name: "here", Provider: config.ProviderFirecracker, VCPU: 64, Memory: 128 * config.GiB,
		})

		if got := whyNoRoom(t, a, "pinned"); !got.NoHost || len(got.HostsFull) != 0 {
			t.Errorf("a tier pinned to a host that is not here reports %+v, want no host", got)
		}
	})

	t.Run("sealed", func(t *testing.T) {
		a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
			[]config.Tier{tier("work", 4, 8*config.GiB)})
		mustRegister(t, a, NodeRegistration{
			Name: "host", Provider: config.ProviderFirecracker, VCPU: 64, Memory: 128 * config.GiB,
		})

		if got := whyNoRoom(t, a, "work"); got.Sealed {
			t.Fatalf("an open deployment reports %+v", got)
		}

		if _, err := a.db.Seal(t.Context(), state.SealRequest{
			Provenance: state.ProvenanceOperator, Reason: "maintenance", Actor: "ops@example.com",
		}); err != nil {
			t.Fatalf("Seal: %v", err)
		}

		got := whyNoRoom(t, a, "work")
		if !got.Sealed || !strings.Contains(got.String(), "admission is sealed") {
			t.Errorf("a sealed deployment reports %+v (%q), want the seal named", got, got)
		}
	})
}
