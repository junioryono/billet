package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// live reads whether the ledger currently believes a node is reachable.
func live(t *testing.T, a *Allocator, name string) bool {
	t.Helper()

	var n int

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT live FROM nodes WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("read liveness of %s: %v", name, err)
	}

	return n == 1
}

// A REGISTERED NODE IS LIVE, which is the fact placement will need and the one
// nothing recorded. The plane's map knew it; the ledger did not, and the ledger
// is where capacity is counted.
func TestRegistrationMarksANodeLive(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	if _, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker)); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a node that just registered is not live")
	}
}

// A node the plane has given up on stops backing advertisements.
func TestANodeThatIsGoneStopsBeingLive(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	epoch, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if err := a.NodeGone(t.Context(), "n1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if live(t, a, "n1") {
		t.Error("a node the plane forgot is still live in the ledger")
	}
}

// COMING BACK IS THE ORDINARY ENDING, and it is the path a fresh registration
// does not exercise.
//
// A host that goes quiet is forgotten and then, almost always, returns — the
// process restarted, the link healed, the machine rebooted. That return goes
// through the UPDATE half of the upsert, not the INSERT half, so a version that
// set `live` only on first sight would leave every recovered node dead in the
// ledger: registered, reachable, taking commands, and contributing nothing to
// what its tier may advertise. Nothing else in the system would report it.
func TestANodeThatComesBackIsLiveAgain(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	epoch, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if err := a.NodeGone(t.Context(), "n1", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if live(t, a, "n1") {
		t.Fatal("the node did not go away, so this test proves nothing about it coming back")
	}

	if _, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker)); err != nil {
		t.Fatalf("re-registering: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a node that registered again is still recorded as gone; it takes commands " +
			"while contributing nothing to what its tier may advertise")
	}
}

// THE RACE THIS FENCE EXISTS FOR, and it is not hypothetical — the ordering that
// produces it is in the code.
//
// Registration commits to the ledger BEFORE it takes the plane's mutex, and
// expiry holds that mutex while it drops the old entry. So a node that restarts
// quickly can commit its new registration and then be marked dead by the expiry
// of the incarnation it replaced. The ledger would say the fleet has no node
// while the plane happily launches onto one, and every tier would advertise
// zero against a machine that is right there.
//
// The epoch is the fence: re-registration bumps it, so a write carrying the old
// one matches nothing.
func TestAnExpiringOldIncarnationCannotKillTheNewOne(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	stale, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("first RegisterNode: %v", err)
	}

	// The node restarts. Same name, new incarnation, new epoch.
	fresh, err := a.RegisterNode(t.Context(), testRegistration("n1", config.ProviderDocker))
	if err != nil {
		t.Fatalf("second RegisterNode: %v", err)
	}

	if fresh == stale {
		t.Fatalf("re-registration did not move the epoch: still %d", fresh)
	}

	// The old incarnation's expiry lands late, carrying the epoch it knew.
	if err := a.NodeGone(t.Context(), "n1", stale); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	if !live(t, a, "n1") {
		t.Error("a stale expiry killed the incarnation that replaced it; the ledger now says " +
			"there is no node while the plane is launching onto one")
	}
}

// NOTHING IS LIVE UNTIL IT SAYS SO AGAIN.
//
// Liveness is the plane's judgement, and a restarted control plane has no
// judgement yet — its map is empty. Rows left over from the previous process
// would otherwise back advertisements for machines this one has never heard
// from, which is the same over-advertisement in a different disguise. Nodes
// re-register within a poll, so the cost is a brief and correct zero.
func TestARestartedControlPlaneTrustsNoNodeUntilItRegistersAgain(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	for _, name := range []string{"n1", "n2"} {
		if _, err := a.RegisterNode(t.Context(), testRegistration(name, config.ProviderDocker)); err != nil {
			t.Fatalf("RegisterNode %s: %v", name, err)
		}
	}

	if err := a.ForgetEveryNode(t.Context()); err != nil {
		t.Fatalf("ForgetEveryNode: %v", err)
	}

	for _, name := range []string{"n1", "n2"} {
		if live(t, a, name) {
			t.Errorf("%s survived a control-plane restart as live", name)
		}
	}
}

// A MACHINE THAT SHRINKS STRANDS THE ESCROW IT CAN NO LONGER HOLD.
//
// Losing a host was handled: its reservations are aimed at something that is no
// longer live, so the listener takes them back. Shrinking one was not, and it is
// the same failure with a quieter cause. An operator edits node.max_vcpu from 8
// to 4 and restarts; capacity is deliberately overwritten on re-registration, so
// the ledger now says 4 while two 4-vCPU leases are still escrowed against it.
//
// Nothing in the listener moves. Its held count is what it advertises, and
// Stranded only ever asked whether the target was LIVE — this host is perfectly
// live, it is merely half the size. So billet goes on offering GitHub two slots
// on a machine with room for one, and the second job is acquired and then fails
// to launch, which looks like billet dropping work.
//
// Only the excess is taken back. Shedding both would give up capacity the host
// can still honour and cost a needless round trip to re-escrow it.
func TestEscrowIsReleasedWhenItsMachineShrinksBeneathIt(t *testing.T) {
	small := tier("small", 4, 8*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{small})

	host := NodeRegistration{
		Name: "only", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 64 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	var leases []*Lease

	for range 2 {
		l, err := a.Reserve(t.Context(), "small")
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		leases = append(leases, l)
	}

	ids := []string{leases[0].ID, leases[1].ID}

	// Nothing is wrong yet: the machine holds both.
	if got, err := a.Stranded(t.Context(), ids); err != nil || len(got) != 0 {
		t.Fatalf("Stranded on a healthy fleet = %v, %v; want none", got, err)
	}

	// The operator halves it and restarts.
	shrunk := host
	shrunk.VCPU = 4

	if _, err := a.RegisterNode(t.Context(), shrunk); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	got, err := a.Stranded(t.Context(), ids)
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("Stranded = %v (%d), want exactly 1: the host has room for one of these "+
			"two and billet is still advertising both", got, len(got))
	}

	// AND IT SETTLES. Once the excess is handed back the machine fits what
	// remains, so the next poll sheds nothing — a version that recomputed from
	// the original overcommit would shed the survivor too, and keep doing it.
	shed, kept := leases[0], leases[1]
	if shed.ID != got[0] {
		shed, kept = leases[1], leases[0]
	}

	if err := a.Release(t.Context(), shed.ID, shed.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if again, err := a.Stranded(t.Context(), []string{kept.ID}); err != nil || len(again) != 0 {
		t.Errorf("Stranded = %v, %v after the excess was given back; the host has room for "+
			"this one and shedding it would flap", again, err)
	}
}

// THE EXCESS IS SHED IN THE DIMENSION THAT IS ACTUALLY OVER.
//
// A host runs out of memory and vCPU independently, and taking back the biggest
// lease by vCPU does nothing for a memory overcommit. Sorting on one fixed
// dimension therefore sheds reservations that were not the problem, and keeps
// going until it happens to take the one that was — so a machine short of a
// single fat-memory lease can lose several slim ones first, and every one of
// those is a job GitHub could have sent.
//
// One tier is wide but light, the other narrow but heavy, and only memory
// shrinks. Exactly one lease need go.
func TestOnlyTheDimensionThatIsOverDecidesWhatIsShed(t *testing.T) {
	wide := tier("wide", 8, 2*config.GiB)
	wide.Provider = config.ProviderDocker

	heavy := tier("heavy", 2, 16*config.GiB)
	heavy.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB},
		[]config.Tier{wide, heavy})

	host := NodeRegistration{
		Name: "only", Provider: config.ProviderDocker,
		VCPU: 64, Memory: 64 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	light, err := a.Reserve(t.Context(), "wide")
	if err != nil {
		t.Fatalf("Reserve wide: %v", err)
	}

	fat, err := a.Reserve(t.Context(), "heavy")
	if err != nil {
		t.Fatalf("Reserve heavy: %v", err)
	}

	// MEMORY ALONE SHRINKS. 18 GiB is committed and 8 remain, so the 16 GiB lease
	// covers the shortfall on its own and the 2 GiB one cannot.
	shrunk := host
	shrunk.Memory = 8 * config.GiB

	if _, err := a.RegisterNode(t.Context(), shrunk); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	got, err := a.Stranded(t.Context(), []string{light.ID, fat.ID})
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	if len(got) != 1 || got[0] != fat.ID {
		t.Errorf("Stranded = %v, want just the 16 GiB lease %s: the shortfall is memory, and "+
			"handing back the 8-vCPU/2 GiB reservation frees almost none of it",
			got, fat.ID)
	}
}

// AND THE SAME RULE IN THE OTHER DIRECTION, which is the half a single test
// cannot prove. A version that always ordered by memory passes the case above
// for the wrong reason, so the vCPU shortfall gets its own: here the heaviest
// lease by memory is the one that frees almost no vCPU, and picking it first
// costs the fleet a slot it did not need to give up.
func TestAVCPUShortfallShedsTheWidestLeaseNotTheHeaviest(t *testing.T) {
	wide := tier("wide", 8, 2*config.GiB)
	wide.Provider = config.ProviderDocker

	heavy := tier("heavy", 2, 16*config.GiB)
	heavy.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB},
		[]config.Tier{wide, heavy})

	host := NodeRegistration{
		Name: "only", Provider: config.ProviderDocker,
		VCPU: 64, Memory: 256 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	broad, err := a.Reserve(t.Context(), "wide")
	if err != nil {
		t.Fatalf("Reserve wide: %v", err)
	}

	fat, err := a.Reserve(t.Context(), "heavy")
	if err != nil {
		t.Fatalf("Reserve heavy: %v", err)
	}

	// VCPU ALONE SHRINKS: 10 are committed and 4 remain. The 8-vCPU lease covers
	// the shortfall by itself; the 2-vCPU one leaves the host still over.
	shrunk := host
	shrunk.VCPU = 4

	if _, err := a.RegisterNode(t.Context(), shrunk); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	got, err := a.Stranded(t.Context(), []string{broad.ID, fat.ID})
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	if len(got) != 1 || got[0] != broad.ID {
		t.Errorf("Stranded = %v, want just the 8-vCPU lease %s: the shortfall is vCPU, and "+
			"the 16 GiB reservation frees only two of the six needed", got, broad.ID)
	}
}

// WHEN BOTH RESOURCES ARE SHORT, THE LEASE THAT COVERS BOTH IS THE ONE TO TAKE.
//
// Picking a fixed dimension to lead by is still wrong when both are over, and it
// is wrong in the expensive direction. A host short 2 vCPU and 10 GiB has one
// reservation that settles the whole thing on its own — 2 vCPU and 16 GiB — and
// another that settles only the vCPU half. Leading with vCPU takes the wide one
// first, which leaves the memory deficit untouched, and then has to take the
// heavy one anyway: two slots surrendered where one would have done.
//
// So each step scores a candidate by how much of what is ACTUALLY still missing
// it covers, in both dimensions at once.
func TestBothShortfallsPreferTheLeaseThatSettlesBoth(t *testing.T) {
	wide := tier("wide", 8, 2*config.GiB)
	wide.Provider = config.ProviderDocker

	heavy := tier("heavy", 2, 16*config.GiB)
	heavy.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB},
		[]config.Tier{wide, heavy})

	host := NodeRegistration{
		Name: "only", Provider: config.ProviderDocker,
		VCPU: 64, Memory: 256 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	broad, err := a.Reserve(t.Context(), "wide")
	if err != nil {
		t.Fatalf("Reserve wide: %v", err)
	}

	fat, err := a.Reserve(t.Context(), "heavy")
	if err != nil {
		t.Fatalf("Reserve heavy: %v", err)
	}

	// 10 vCPU and 18 GiB are committed. Leaving 8 and 8 puts the host over by 2
	// vCPU and 10 GiB — and the heavy lease alone covers both.
	shrunk := host
	shrunk.VCPU = 8
	shrunk.Memory = 8 * config.GiB

	if _, err := a.RegisterNode(t.Context(), shrunk); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	got, err := a.Stranded(t.Context(), []string{broad.ID, fat.ID})
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	if len(got) != 1 || got[0] != fat.ID {
		t.Errorf("Stranded = %v, want just %s: it settles both shortfalls by itself, so "+
			"handing back the other one as well gives up a slot for nothing", got, fat.ID)
	}
}

// A RESERVATION IS WORTH WHAT IT SETTLES, NOT WHAT IT IS.
//
// Scoring by raw size lets a huge lease win on a tiny shortfall. A host over by
// 6 vCPU and 1 GiB has a 200 GiB reservation whose memory dwarfs the deficit —
// counted at face value it outscores everything, so it is taken first, frees 199
// GiB nobody asked for, barely touches the vCPU deficit, and the lease that
// would actually have settled the host has to go too.
//
// Surplus is worth nothing, so coverage is capped at what is missing: a 200 GiB
// lease and a 1 GiB lease are worth exactly the same against a 1 GiB shortfall,
// and the vCPU deficit decides between them.
func TestSurplusDoesNotMakeALeaseWorthShedding(t *testing.T) {
	wide := tier("wide", 8, 2*config.GiB)
	wide.Provider = config.ProviderDocker

	huge := tier("huge", 2, 200*config.GiB)
	huge.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 2048 * config.GiB},
		[]config.Tier{wide, huge})

	host := NodeRegistration{
		Name: "only", Provider: config.ProviderDocker,
		VCPU: 64, Memory: 512 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	broad, err := a.Reserve(t.Context(), "wide")
	if err != nil {
		t.Fatalf("Reserve wide: %v", err)
	}

	enormous, err := a.Reserve(t.Context(), "huge")
	if err != nil {
		t.Fatalf("Reserve huge: %v", err)
	}

	// 10 vCPU and 202 GiB committed; 4 and 201 remain. Short 6 vCPU and 1 GiB —
	// and the 8-vCPU/2 GiB lease covers both of those by itself.
	shrunk := host
	shrunk.VCPU = 4
	shrunk.Memory = 201 * config.GiB

	if _, err := a.RegisterNode(t.Context(), shrunk); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	got, err := a.Stranded(t.Context(), []string{broad.ID, enormous.ID})
	if err != nil {
		t.Fatalf("Stranded: %v", err)
	}

	if len(got) != 1 || got[0] != broad.ID {
		t.Errorf("Stranded = %v, want just %s: the 200 GiB reservation is only worth the 1 "+
			"GiB actually missing, and taking it back frees almost none of the vCPU that is "+
			"the real shortfall", got, broad.ID)
	}
}
