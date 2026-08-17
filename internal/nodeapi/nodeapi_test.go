package nodeapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// EVERY PHASE HAS A WIRE SPELLING, and the check is exhaustive rather than a
// sample.
//
// A phase added to the ledger without one would be silently unrepresentable:
// ParsePhase would reject it, the node could never advance a lease into it, and
// the failure would surface as a stuck lease rather than as a missing case. The
// list is written out on purpose — deriving it from the same source ParsePhase
// uses would make this test agree with any bug it contains.
func TestEveryPhaseSurvivesTheWire(t *testing.T) {
	t.Parallel()

	for _, p := range []alloc.Phase{
		alloc.PhaseCapacity,
		alloc.PhaseAssigned,
		alloc.PhaseLaunching,
		alloc.PhaseOnline,
		alloc.PhaseBusy,
		alloc.PhaseCustody,
		alloc.PhaseTeardown,
		alloc.PhaseDone,
		alloc.PhaseFailed,
	} {
		got, ok := ParsePhase(string(p))
		if !ok {
			t.Errorf("phase %q has no wire spelling, so no node can ever move a lease into it", p)

			continue
		}

		if got != p {
			t.Errorf("phase %q round-tripped as %q", p, got)
		}
	}
}

// A phase billet does not know is refused AT THE BOUNDARY.
func TestAnUnknownPhaseIsRefused(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "lauching", "Capacity", "done ", "deleted"} {
		if _, ok := ParsePhase(s); ok {
			t.Errorf("%q was accepted as a phase", s)
		}
	}
}

// THE LEASE CROSSES THE WIRE WITH THE FIELDS A LAUNCH NEEDS.
//
// A launch that arrives without vCPU, memory, guest OS or the acceptable
// providers cannot start anything, and the failure would look like a provider
// bug rather than a serialisation one. alloc.Lease is an internal type whose
// fields are free to change, so this pins the ones the protocol depends on.
func TestALaunchCommandCarriesWhatALaunchNeeds(t *testing.T) {
	t.Parallel()

	cmd := Command{
		ID:   "c1",
		Kind: CommandLaunch,
		Lease: &alloc.Lease{
			ID:        "l1",
			Tier:      "billet-2vcpu",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     3,
			RequestID: 77,
		},
		Tier: TierSpecOf(config.Tier{BuildKitCacheMountLimit: 7 * config.GiB}),
		Job:  &Job{RequestID: 77, RunID: 88, Event: "push"},
	}

	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Command
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Lease == nil {
		t.Fatal("the lease did not survive the round trip")
	}

	if got.Lease.VCPU != 2 || got.Lease.Memory != 8*config.GiB {
		t.Errorf("size lost: vcpu=%d memory=%v", got.Lease.VCPU, got.Lease.Memory)
	}

	if got.Lease.GuestOS != config.GuestLinux {
		t.Errorf("guest OS lost: %q", got.Lease.GuestOS)
	}

	if len(got.Lease.Providers) != 1 || got.Lease.Providers[0] != config.ProviderDocker {
		t.Errorf("acceptable providers lost: %v", got.Lease.Providers)
	}

	if got.Lease.Epoch != 3 {
		t.Errorf("epoch lost: %d — without it the node cannot fence its own writes", got.Lease.Epoch)
	}

	if got.Job == nil || got.Job.Event != "push" {
		t.Errorf("the job's event was lost, and it is the only thing that says how far the "+
			"workload can be trusted: %+v", got.Job)
	}
	if got.Tier == nil || got.Tier.BuildKitCacheMountLimit != 7*config.GiB {
		t.Errorf("the tier's BuildKit cache-mount ceiling was lost: %+v", got.Tier)
	}
}

// A DESTROY NAMES A REQUEST, NOT A LEASE, and must survive without one.
//
// Destroy has to work for compute whose lease is already gone — that is the
// whole reason it is keyed by request. A command shape that required a lease
// would make the important case unrepresentable.
func TestADestroyCommandNeedsNoLease(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(Command{ID: "c2", Kind: CommandDestroy, RequestID: 99})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(raw), `"lease"`) {
		t.Errorf("a destroy carried a lease field: %s", raw)
	}

	var got Command
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.RequestID != 99 || got.Kind != CommandDestroy {
		t.Errorf("destroy did not round-trip: %+v", got)
	}
}

// The error codes a node BRANCHES on are distinct strings.
//
// Two codes that collide would silently merge two responses the node must treat
// differently — letting go of a fenced lease is not the same as retrying.
func TestErrorCodesAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}

	for _, c := range []string{
		CodeFenced, CodeNotFound, CodeNoCapacity, CodeForceRelease, CodeRefused,
		CodeUnregistered,
	} {
		if c == "" {
			t.Error("an empty error code cannot be branched on")
		}

		if seen[c] {
			t.Errorf("duplicate error code %q", c)
		}

		seen[c] = true
	}
}
