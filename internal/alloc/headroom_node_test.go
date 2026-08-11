package alloc

import (
	"database/sql"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// roomOn asks how many of a tier fit on one host, inside a transaction.
func roomOn(t *testing.T, a *Allocator, node nodeRow, tier config.Tier) int {
	t.Helper()

	var n int

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		var err error
		n, err = a.headroomOn(t.Context(), tx, node, tier)

		return err
	}); err != nil {
		t.Fatalf("headroomOn: %v", err)
	}

	return n
}

// eligible lists the hosts a tier may be placed on, inside a transaction.
func eligible(t *testing.T, a *Allocator, tier config.Tier) []string {
	t.Helper()

	var names []string

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		rows, err := a.eligibleNodes(t.Context(), tx, tier)
		if err != nil {
			return err
		}

		names = names[:0]
		for _, r := range rows {
			names = append(names, r.name)
		}

		return nil
	}); err != nil {
		t.Fatalf("eligibleNodes: %v", err)
	}

	return names
}

// mustRegister puts a host in the ledger with a stated contribution.
func mustRegister(t *testing.T, a *Allocator, reg NodeRegistration) (nodeRow, int64) {
	t.Helper()

	epoch, err := a.RegisterNode(t.Context(), reg)
	if err != nil {
		t.Fatalf("RegisterNode %s: %v", reg.Name, err)
	}

	return nodeRow{
		name: reg.Name, provider: reg.Provider, site: reg.Site,
		vcpu: reg.VCPU, memory: reg.Memory,
	}, epoch
}

// A MACHINE'S CAPACITY IS THE MACHINE'S, which is the entire point of the
// change. The deployment-wide budget said "the fleet has room somewhere"; this
// says which host has it, and a 4 vCPU box cannot answer for a 64 vCPU one.
func TestAHostsHeadroomIsBoundedByWhatItContributes(t *testing.T) {
	small := tier("small", 4, 8*config.GiB)

	// A budget far larger than either host, so the per-node bound is the only
	// thing that can produce these numbers.
	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small})

	for _, tc := range []struct {
		name string
		node NodeRegistration
		want int
	}{
		{
			name: "cores are the constraint",
			node: NodeRegistration{Name: "a", Provider: config.ProviderFirecracker,
				VCPU: 16, Memory: 4000 * config.GiB},
			want: 4,
		},
		{
			// The reason capacity is a vector rather than a number: plenty of cores
			// and not enough memory is a host that fits fewer, and a check that only
			// counted cores would place work it cannot run.
			name: "memory is the constraint",
			node: NodeRegistration{Name: "b", Provider: config.ProviderFirecracker,
				VCPU: 1000, Memory: 24 * config.GiB},
			want: 3,
		},
		{
			name: "a host smaller than one instance fits none",
			node: NodeRegistration{Name: "c", Provider: config.ProviderFirecracker,
				VCPU: 2, Memory: 4000 * config.GiB},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _ := mustRegister(t, a, tc.node)

			if got := roomOn(t, a, n, small); got != tc.want {
				t.Errorf("headroom on %s = %d, want %d", tc.node.Name, got, tc.want)
			}
		})
	}
}

// WORK IS CHARGED TO THE HOST THAT HOLDS IT, and to no other. Two machines are
// two budgets; a lease running on one must not shrink the other.
func TestWorkIsChargedToTheHostHoldingIt(t *testing.T) {
	small := tier("small", 4, 8*config.GiB)
	small.Provider = config.ProviderDocker

	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small})

	busy, _ := mustRegister(t, a, NodeRegistration{
		Name: "busy", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})
	idle, _ := mustRegister(t, a, NodeRegistration{
		Name: "idle", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	before := roomOn(t, a, idle, small)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, busy.name); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if got, want := roomOn(t, a, busy, small), 1; got != want {
		t.Errorf("the host running the job has room for %d more, want %d", got, want)
	}

	if got := roomOn(t, a, idle, small); got != before {
		t.Errorf("a job on another machine changed this one's headroom: %d, was %d", got, before)
	}
}

// A LEASE AIMED AT A HOST IS ALREADY SPENDING IT.
//
// Capacity is escrowed before anything is advertised, so a reservation that
// names a host has committed that host's room whether or not a container has
// started. Charging only BOUND leases would let a tier escrow against a machine
// repeatedly in the window before its first launch — which is the overcommit the
// escrow exists to prevent, moved from the deployment to the host.
func TestAReservationAimedAtAHostSpendsItBeforeItRuns(t *testing.T) {
	small := tier("small", 4, 8*config.GiB)
	small.Provider = config.ProviderDocker
	small.Node = "pinned"

	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small})

	n, _ := mustRegister(t, a, NodeRegistration{
		Name: "pinned", Provider: config.ProviderDocker, VCPU: 8, Memory: 64 * config.GiB})

	before := roomOn(t, a, n, small)

	// Reserved and never bound: target_node is set, node is not.
	if _, err := a.Reserve(t.Context(), "small"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if got := roomOn(t, a, n, small); got != before-1 {
		t.Errorf("an unbound reservation aimed here did not spend the host: %d, was %d",
			got, before)
	}
}

// APPLE'S LIMIT BELONGS TO THE MAC, NOT TO THE TIER THAT ASKED, and it has to
// travel with the per-node arithmetic rather than being applied beside it.
//
// A macOS tier is always pinned — alloc.New refuses one that names no node,
// because the per-host licence cannot be enforced without knowing the host — so
// the host this counts against is never in doubt. What matters is that the count
// crosses TIERS: two individually legal macOS tiers on one Mac still share one
// machine and one licence, so the limit belongs to the node's headroom and not
// to any tier's.
func TestAMacsLicenceBoundsItsHeadroomAcrossTiers(t *testing.T) {
	mac := func(label string) config.Tier {
		return config.Tier{
			Label: label, Provider: config.ProviderTart, GuestOS: config.GuestMacOS,
			Node: "mac-mini-1", VCPU: 4, Memory: 8 * config.GiB, Image: "macos-26",
		}
	}

	first, second := mac("macos-a"), mac("macos-b")

	a := newAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{first, second})

	// Big enough that only the licence can be the constraint.
	n, _ := mustRegister(t, a, NodeRegistration{
		Name: "mac-mini-1", Provider: config.ProviderTart, VCPU: 64, Memory: 512 * config.GiB})

	if got, want := roomOn(t, a, n, first), config.DefaultMacOSVMLimit; got != want {
		t.Fatalf("headroom on an idle Mac = %d, want Apple's per-host limit of %d", got, want)
	}

	// One guest booted by the OTHER tier still spends this Mac's licence.
	if _, err := a.Reserve(t.Context(), "macos-b"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if got, want := roomOn(t, a, n, first), config.DefaultMacOSVMLimit-1; got != want {
		t.Errorf("headroom = %d, want %d — a guest from another tier did not count against "+
			"this Mac's licence", got, want)
	}
}

// A TIER MAY ONLY BE PLACED WHERE IT COULD ACTUALLY RUN, and every one of these
// filters is a way for a fleet to contain a host that cannot serve it.
func TestOnlyHostsThatCouldServeATierAreEligible(t *testing.T) {
	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB}, nil)

	epochs := map[string]int64{}

	for _, reg := range []NodeRegistration{
		{Name: "docker-home", Provider: config.ProviderDocker, Site: "home", VCPU: 8, Memory: 64 * config.GiB},
		{Name: "docker-aws", Provider: config.ProviderDocker, Site: "aws", VCPU: 8, Memory: 64 * config.GiB},
		{Name: "tart", Provider: config.ProviderTart, Site: "home", VCPU: 8, Memory: 64 * config.GiB},
	} {
		_, epoch := mustRegister(t, a, reg)
		epochs[reg.Name] = epoch
	}

	base := func() config.Tier {
		return config.Tier{
			Label: "t", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 4, Memory: 8 * config.GiB, Image: "ubuntu",
		}
	}

	t.Run("the provider has to match", func(t *testing.T) {
		got := eligible(t, a, base())
		if len(got) != 2 {
			t.Errorf("eligible = %v, want only the docker hosts", got)
		}
	})

	t.Run("a pin excludes everything else", func(t *testing.T) {
		tr := base()
		tr.Node = "docker-aws"

		if got := eligible(t, a, tr); len(got) != 1 || got[0] != "docker-aws" {
			t.Errorf("eligible = %v, want only the pinned host", got)
		}
	})

	t.Run("a site confines without pinning", func(t *testing.T) {
		tr := base()
		tr.Site = "home"

		if got := eligible(t, a, tr); len(got) != 1 || got[0] != "docker-home" {
			t.Errorf("eligible = %v, want only the host at that site", got)
		}
	})

	t.Run("a host that is gone is not a candidate", func(t *testing.T) {
		if err := a.NodeGone(t.Context(), "docker-aws", epochs["docker-aws"]); err != nil {
			t.Fatalf("NodeGone: %v", err)
		}

		got := eligible(t, a, base())
		for _, n := range got {
			if n == "docker-aws" {
				t.Errorf("a host the plane gave up on is still a candidate: %v", got)
			}
		}
	})
}
