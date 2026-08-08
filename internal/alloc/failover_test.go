package alloc

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// ONE LABEL, TWO KINDS OF MACHINE.
//
// The point of the whole change: a tier that lists several backends may be
// placed on a host running any of them, so losing the machine at home does not
// take the `runs-on` label down with it. Before this, a tier named exactly one
// provider and placement compared it for equality — which made a tier's backend
// a property of its reservation and pinned every lease before anything knew
// where it would run.
func TestATierWithSeveralProvidersBindsToEither(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	for name, host := range map[string]struct {
		node     string
		provider config.ProviderKind
	}{
		"the preferred backend": {"epyc-1", config.ProviderFirecracker},
		"the fallback":          {"ec2-spot-1", config.ProviderEC2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
				[]config.Tier{tier})

			if err := a.RegisterNode(t.Context(), host.node, host.provider); err != nil {
				t.Fatalf("RegisterNode: %v", err)
			}

			lease, err := a.Reserve(t.Context(), tier.Label)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}

			if err := a.Bind(t.Context(), lease.ID, lease.Epoch, host.node); err != nil {
				t.Fatalf("a tier that accepts %v would not bind to a %s host: %v",
					tier.Providers, host.provider, err)
			}

			// AND THE LEDGER RECORDS WHICH ONE IT ACTUALLY LANDED ON. "May run on"
			// and "is running on" are different facts, and collapsing them is what
			// made a fallback impossible to express.
			bound, err := a.Lease(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("read the bound lease: %v", err)
			}

			if bound.Provider != host.provider {
				t.Errorf("the lease records provider %q, want the backend it is on (%q)",
					bound.Provider, host.provider)
			}
		})
	}
}

// A lease chooses nothing until it is placed.
//
// The distinction the single column could not make: a reserved lease has not
// been anywhere, so claiming a backend for it is a guess that later reads as a
// fact.
func TestAReservedLeaseHasNotChosenABackend(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if lease.Provider != "" {
		t.Errorf("a reserved lease already claims to be on %q", lease.Provider)
	}

	if len(lease.Providers) != 2 {
		t.Errorf("the lease records %v as acceptable, want both of the tier's", lease.Providers)
	}
}

// A host running something the tier does not accept is still refused.
//
// Failover widens what is allowed; it does not remove the check. A firecracker
// tier must not land on a docker host just because the tier now holds a list.
func TestAProviderOutsideTheListIsStillRefused(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	if err := a.RegisterNode(t.Context(), "laptop", config.ProviderDocker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	err = a.Bind(t.Context(), lease.ID, lease.Epoch, "laptop")
	if !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("bind to a backend the tier does not accept = %v, want ErrWrongProvider", err)
	}
}

// The ORDER is recorded even though nothing acts on it yet.
//
// Preference is a choice among candidates, and choosing needs a chooser — which
// arrives when the node runs in its own process and the control plane picks
// rather than the node binding itself. Recording the order now is what makes
// that a scheduling change rather than a schema change.
func TestThePreferenceOrderIsPreserved(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderEC2, config.ProviderFirecracker},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	reloaded, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease back: %v", err)
	}

	want := []config.ProviderKind{config.ProviderEC2, config.ProviderFirecracker}

	for i := range want {
		if i >= len(reloaded.Providers) || reloaded.Providers[i] != want[i] {
			t.Fatalf("preference read back as %v, want %v — the order is the whole meaning "+
				"of the list", reloaded.Providers, want)
		}
	}
}

// A single `provider:` still works, unchanged.
//
// It is what almost every deployment wants and what every existing config says.
func TestASingleProviderStillPlaces(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:    "billet-2vcpu",
		Provider: config.ProviderDocker,
		VCPU:     2,
		Memory:   4 * config.GiB,
		GuestOS:  config.GuestLinux,
	}

	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, []config.Tier{tier})

	if err := a.RegisterNode(t.Context(), "laptop", config.ProviderDocker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "laptop"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	bound, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the bound lease: %v", err)
	}

	if bound.Provider != config.ProviderDocker {
		t.Errorf("chose %q, want docker", bound.Provider)
	}
}

// alloc.New applies the same provider rules config.Load does.
//
// Its own doc says it cannot assume its catalogue came through Load — a caller
// can build tiers in code — and it was accepting tiers that package refuses. The
// macOS case is not tidiness: placement only tests list MEMBERSHIP, so a
// [tart, ec2] macOS tier would bind a macOS lease to an EC2 node quite happily,
// which is the Apple-hardware invariant gone.
func TestNewRefusesTiersConfigWouldRefuse(t *testing.T) {
	t.Parallel()

	for name, tier := range map[string]config.Tier{
		"both spellings": {
			Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB, GuestOS: config.GuestLinux,
			Provider:  config.ProviderFirecracker,
			Providers: []config.ProviderKind{config.ProviderFirecracker},
		},
		"a duplicate": {
			Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB, GuestOS: config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderEC2, config.ProviderEC2},
		},
		"no provider at all": {
			Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB, GuestOS: config.GuestLinux,
		},
		"an unknown backend": {
			Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB, GuestOS: config.GuestLinux,
			Providers: []config.ProviderKind{"quantum"},
		},
		"a macos tier that can leave apple hardware": {
			Label: "billet-6vcpu-macos", VCPU: 6, Memory: 16 * config.GiB, GuestOS: config.GuestMacOS,
			// PINNED, because alloc.New refuses an unpinned macOS tier for a
			// different reason — the licence cap needs a host to count against.
			// Without the pin this case never reached the check it is named for,
			// and passed on the strength of the other rule.
			Node:      "mac-mini-1",
			Providers: []config.ProviderKind{config.ProviderTart, config.ProviderEC2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db := openState(t)

			if _, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
				[]config.Tier{tier}); err == nil {
				t.Fatalf("accepted a tier with %s", name)
			}
		})
	}
}

// A stored provider list billet cannot fully read authorizes NOTHING.
//
// Dropping the entries it does not recognise and keeping the rest was fail-open:
// "bogus,docker" still authorized a docker node, so a corrupted or truncated
// placement fact silently became a narrower but still-valid one.
func TestAMalformedProviderListFailsClosed(t *testing.T) {
	t.Parallel()

	for name, stored := range map[string]string{
		"an unknown entry": "bogus,docker",
		"an empty element": ",docker",
		"a trailing comma": "docker,",
		"only junk":        "bogus",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := decodeProviders(stored); got != nil {
				t.Errorf("decoded %q as %v; a placement fact billet cannot fully read must "+
					"authorize nothing", stored, got)
			}
		})
	}

	// And a well-formed one still round-trips, in order.
	want := []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}

	got := decodeProviders(encodeProviders(want))
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("a valid list round-tripped as %v, want %v", got, want)
	}
}

// A host may not change its backend while it is running work.
//
// Re-registration overwrote the provider freely, which falsified every lease
// already bound there: each recorded the backend it chose at bind, and after the
// change the ledger said a job ran on firecracker while the host called itself
// docker. Later checks read the NODE's row, so they went on authorizing the
// lease — the fact that had become wrong was the one nothing re-read.
func TestANodeCannotChangeBackendWhileRunningWork(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderDocker},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})

	if err := a.RegisterNode(t.Context(), "shapeshifter", config.ProviderFirecracker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), tier.Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "shapeshifter"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Both backends are acceptable to the tier, so nothing downstream would
	// object — which is exactly why the refusal has to happen here.
	err = a.RegisterNode(t.Context(), "shapeshifter", config.ProviderDocker)
	if !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("a busy host changed its backend: %v", err)
	}

	// The lease still says what it actually chose.
	bound, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease: %v", err)
	}

	if bound.Provider != config.ProviderFirecracker {
		t.Errorf("the lease now claims %q; its compute is still on firecracker", bound.Provider)
	}
}

// An IDLE host changes freely — the refusal is about running work, not about
// pinning a machine forever.
func TestAnIdleNodeMayChangeBackend(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{{
			Label: "billet-8vcpu", Providers: []config.ProviderKind{config.ProviderDocker},
			VCPU: 8, Memory: 32 * config.GiB, GuestOS: config.GuestLinux,
		}})

	if err := a.RegisterNode(t.Context(), "spare", config.ProviderFirecracker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if err := a.RegisterNode(t.Context(), "spare", config.ProviderDocker); err != nil {
		t.Fatalf("an idle host could not change its backend: %v", err)
	}
}

// AN UPGRADED LEASE STILL PLACES.
//
// The claim the state package's migration test could not make: it can only read
// columns back, because alloc imports state and not the other way round. This is
// the behaviour anyone actually cares about — a job that was in flight when the
// operator upgraded still gets a host — and the migration is only useful insofar
// as it produces a lease that binds.
//
// The pre-upgrade row is written directly, which is the point: it never went
// through Reserve, so nothing in this test can accidentally supply the value the
// migration is supposed to have backfilled.
func TestAnUpgradedLeaseStillBinds(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label:     "billet-8vcpu-ubuntu-2404",
		Providers: []config.ProviderKind{config.ProviderFirecracker},
		VCPU:      8,
		Memory:    32 * config.GiB,
		GuestOS:   config.GuestLinux,
	}

	db := openState(t)

	a, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if err := a.RegisterNode(t.Context(), "epyc-1", config.ProviderFirecracker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// A row exactly as migration 9 leaves one: providers backfilled from the old
	// single column, nothing chosen because it was never bound.
	const leaseID = "upgraded-lease"

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO leases
			   (id, tier, node, macos_slot, guest_os, provider, providers, chosen_provider,
			    phase, vcpu, memory, epoch, created_at, heartbeat_at, expires_at)
			 VALUES (?, ?, NULL, 0, 'linux', 'firecracker', 'firecracker', '',
			         'capacity', 8, 34359738368, 0, '2026-01-01T00:00:00Z',
			         '2026-01-01T00:00:00Z', '2999-01-01T00:00:00Z')`,
			leaseID, tier.Label)

		return err
	}); err != nil {
		t.Fatalf("write a migrated lease: %v", err)
	}

	if err := a.Bind(t.Context(), leaseID, 0, "epyc-1"); err != nil {
		t.Fatalf("a lease carried across the upgrade could not be placed: %v", err)
	}

	bound, err := a.Lease(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("read the bound lease: %v", err)
	}

	if bound.Provider != config.ProviderFirecracker {
		t.Errorf("the upgraded lease recorded %q as its backend", bound.Provider)
	}
}

// openState opens a throwaway ledger.
func openState(t *testing.T) *state.DB {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}
