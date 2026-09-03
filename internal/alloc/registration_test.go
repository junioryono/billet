package alloc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// testRegistration is a host big enough that it is never the thing under test.
//
// DELIBERATELY LARGER THAN ANY BUDGET THESE TESTS SET, so the deployment-wide
// ceiling stays the binding constraint and every test written before nodes had
// capacity keeps measuring exactly what it measured before. A test that wants
// per-node capacity to bite says so by registering a specific size.
func testRegistration(name string, kind config.ProviderKind) NodeRegistration {
	reg := NodeRegistration{
		Name:     name,
		Provider: kind,
		VCPU:     1 << 20,
		Memory:   1 << 20 * config.GiB,
	}

	if kind == config.ProviderEC2 {
		for _, vcpu := range []int{1, 2, 4, 6, 8, 16, 32, 64, 1 << 20} {
			for _, memory := range []config.ByteSize{
				config.GiB, 2 * config.GiB, 4 * config.GiB, 8 * config.GiB,
				16 * config.GiB, 24 * config.GiB, 32 * config.GiB,
				64 * config.GiB, 128 * config.GiB, 1 << 20 * config.GiB,
			} {
				reg.EC2Shapes = append(reg.EC2Shapes, config.EC2InstanceType{
					Type: fmt.Sprintf("test-%d-%d", vcpu, memory),
					VCPU: vcpu, Memory: memory,
				})
			}
		}
	}

	return reg
}

// A HOST IS THE AUTHORITY ON ITSELF, and until now the only thing it was the
// authority about was its provider. Capacity is the same kind of fact: the
// machine knows what it has, the operator of that machine knows what it should
// give, and the control plane knows neither.
//
// Recorded rather than derived, for the reason the provider is: a catalogue that
// disagrees with the machine is the thing that should lose.
func TestRegistrationRecordsWhatTheNodeContributes(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	reg := NodeRegistration{
		Name:     "epyc-1",
		Provider: config.ProviderDocker,
		Site:     "home",
		VCPU:     120,
		Memory:   480 * config.GiB,
	}

	if _, err := a.RegisterNode(t.Context(), reg); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	var (
		vcpu   int
		memory int64
		site   string
	)

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT total_vcpu, total_memory, site FROM nodes WHERE name = $1`, reg.Name).
		Scan(&vcpu, &memory, &site); err != nil {
		t.Fatalf("read the node back: %v", err)
	}

	if vcpu != reg.VCPU {
		t.Errorf("total_vcpu = %d, want %d", vcpu, reg.VCPU)
	}

	if config.ByteSize(memory) != reg.Memory {
		t.Errorf("total_memory = %s, want %s", config.ByteSize(memory), reg.Memory)
	}

	if site != reg.Site {
		t.Errorf("site = %q, want %q", site, reg.Site)
	}
}

// A HOST THAT RE-REGISTERS SMALLER IS TAKEN AT ITS WORD.
//
// The operator edited the config and restarted, which is the supported way to
// change what a machine gives. Leases already open keep their capacity charged —
// nothing about them changed — so the effect is simply that no NEW work fits
// until enough of them drain. Refusing the registration instead would take a
// host out of the fleet for saying something true.
func TestAHostMayContributeLessThanItDidBefore(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	first := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker,
		VCPU: 120, Memory: 480 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), first); err != nil {
		t.Fatalf("first RegisterNode: %v", err)
	}

	second := first
	second.VCPU, second.Memory = 8, 32*config.GiB

	if _, err := a.RegisterNode(t.Context(), second); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	// BOTH FIELDS, because they are written by one statement and only one of them
	// was being read. An UPSERT that updated the cores and dropped the memory
	// clause passed this test while leaving the ledger describing a machine that
	// does not exist.
	var (
		vcpu   int
		memory int64
	)

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT total_vcpu, total_memory FROM nodes WHERE name = $1`, first.Name).
		Scan(&vcpu, &memory); err != nil {
		t.Fatalf("read the node back: %v", err)
	}

	if vcpu != second.VCPU {
		t.Errorf("total_vcpu = %d, want %d — the new contribution did not replace the old",
			vcpu, second.VCPU)
	}

	if config.ByteSize(memory) != second.Memory {
		t.Errorf("total_memory = %s, want %s — the new contribution did not replace the old",
			config.ByteSize(memory), second.Memory)
	}
}

// A HOST DOES NOT MOVE WHILE IT IS RUNNING WORK.
//
// The same rule as a backend change and for the same reason: the leases bound
// here recorded where they are, and a machine that re-registers somewhere else
// relabels compute that has not gone anywhere. Site is where a cache lives, so
// the ledger would start pointing later placements at storage in a different
// building from the containers already running.
func TestAHostMayNotMoveSiteWhileWorkIsBoundToIt(t *testing.T) {
	// The node runs docker, so the tier must accept docker or Bind refuses on the
	// provider before it can reach the site check this test is about.
	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{small})

	home := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, Site: "home",
		VCPU: 120, Memory: 480 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), home); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// An unbound host moves freely; it is the binding that pins it.
	moved := home
	moved.Site = "aws"

	if _, err := a.RegisterNode(t.Context(), moved); err != nil {
		t.Fatalf("an idle host was not allowed to move: %v", err)
	}

	if _, err := a.RegisterNode(t.Context(), home); err != nil {
		t.Fatalf("moving back: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, home.Name); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, err = a.RegisterNode(t.Context(), moved)
	if err == nil {
		t.Fatal("a host moved site while a lease was still bound to it")
	}

	if !errors.Is(err, ErrWrongSite) {
		t.Errorf("want ErrWrongSite, got %v", err)
	}

	// The way back has to be in the message: this fires at startup, so an
	// operator who edited node.site finds billet refusing to boot.
	if !strings.Contains(err.Error(), "home") {
		t.Errorf("the refusal does not say what to put the site back to: %v", err)
	}
}

// A CONTRIBUTION OF NOTHING IS REFUSED, and the reason is that it does not fail
// anywhere else. A node recorded with zero capacity registers cleanly, appears
// in the fleet, and is simply never chosen — so the tier it was meant to serve
// advertises nothing while the machine sits there looking healthy. There is no
// error to find later, which is why there has to be one now.
func TestANodeThatContributesNothingIsRefused(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)

	for _, tc := range []struct {
		name  string
		reg   NodeRegistration
		field string
	}{
		{
			name:  "no cores",
			reg:   NodeRegistration{Name: "n1", Provider: config.ProviderDocker, Memory: 32 * config.GiB},
			field: "vcpu",
		},
		{
			name:  "no memory",
			reg:   NodeRegistration{Name: "n1", Provider: config.ProviderDocker, VCPU: 8},
			field: "memory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.RegisterNode(t.Context(), tc.reg)
			if err == nil {
				t.Fatal("a node contributing nothing was registered")
			}

			if !strings.Contains(strings.ToLower(err.Error()), tc.field) {
				t.Errorf("the error does not name what was missing: %v", err)
			}
		})
	}
}

// A HOST DOES NOT MOVE OUT FROM UNDER CAPACITY THAT HAS ALREADY BEEN PROMISED
// ON ITS BEHALF, which is a wider set of leases than the ones bound to it.
//
// The guard counted `leases.node`, and that column is only filled at bind. A
// reservation is aimed at a machine well before then — escrow picks the host,
// records it in target_node, and billet advertises the capacity to GitHub on
// that basis. Between those two moments the guard saw nothing, so a host could
// change site or backend while work was already inbound for it: the job arrives,
// binds to a machine that has since moved buildings, and the placement that was
// made against a cache in one site is now running in another.
//
// It is the same attribution the arithmetic uses everywhere else —
// COALESCE(node, target_node) — and the guard was the one place still asking
// only half the question.
func TestAHostMayNotMoveWhileWorkIsMerelyAimedAtIt(t *testing.T) {
	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{small})

	home := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, Site: "home",
		VCPU: 120, Memory: 480 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), home); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// RESERVED BUT NOT BOUND. This is the state billet spends most of its time
	// in: the capacity is escrowed, the machine is chosen, and GitHub has been
	// told the slot exists. Nothing has bound because no job has arrived yet.
	if _, err := a.Reserve(t.Context(), "small"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	moved := home
	moved.Site = "aws"

	_, err := a.RegisterNode(t.Context(), moved)
	if err == nil {
		t.Fatal("a host moved site while capacity was already escrowed against it; the job " +
			"that arrives for that slot will run in a different building from the cache it " +
			"was placed against")
	}

	if !errors.Is(err, ErrWrongSite) {
		t.Errorf("want ErrWrongSite, got %v", err)
	}

	// The provider guard reads the same column and was wrong the same way.
	rebadged := home
	rebadged.Provider = config.ProviderTart

	if _, err := a.RegisterNode(t.Context(), rebadged); !errors.Is(err, ErrWrongProvider) {
		t.Errorf("a host changed backend with capacity escrowed against it: %v", err)
	}
}

// ESCROW LEFT BY A CRASH MUST NOT LOCK THE HOST OUT OF THE FLEET FOREVER.
//
// The guards refuse a site or backend change while work is outstanding, and that
// is right for work that exists. An EXPIRED lease is not that: its holder
// stopped heartbeating, and the reaper's whole job is to fail it. Counting it as
// outstanding turns a crash into a permanent refusal to start.
//
// It is permanent, not merely slow, and the ordering is why. billet registers
// the node BEFORE the server runs, and the reaper only runs inside the server —
// so a registration refused on the strength of expired leases prevents the very
// process that would have cleared them. Every restart meets the same wall, and
// the only ways out are editing the config back or repairing the ledger by hand.
//
// So the guards ask about live work, using the same definition of expiry the
// reaper does. A heartbeating lease still pins the host; an abandoned one does
// not get a vote.
func TestACrashedHostsExpiredEscrowDoesNotLockItOut(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{small},
		WithClock(func() time.Time { return clock() }),
		WithLeaseTTL(30*time.Second))

	home := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, Site: "home",
		VCPU: 64, Memory: 256 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), home); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if _, err := a.Reserve(t.Context(), "small"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	moved := home
	moved.Site = "aws"

	// While the escrow is live the host is pinned, which is the rule working.
	if _, err := a.RegisterNode(t.Context(), moved); !errors.Is(err, ErrWrongSite) {
		t.Fatalf("a live reservation stopped pinning the host: %v", err)
	}

	// THE PROCESS DIES. Nothing heartbeats, and the lease ages out — but nothing
	// reaps it either, because the reaper lives in the server this registration
	// is blocking.
	now = now.Add(31 * time.Second)

	if _, err := a.RegisterNode(t.Context(), moved); err != nil {
		t.Errorf("a host could not rejoin the fleet because of escrow abandoned by a crash: "+
			"%v — and nothing will ever clear it, because the reaper runs inside the server "+
			"this registration has to succeed to start", err)
	}

	// The backend guard reads the same column and deadlocks the same way.
	rebadged := moved
	rebadged.Provider = config.ProviderTart

	if _, err := a.RegisterNode(t.Context(), rebadged); err != nil {
		t.Errorf("a host could not change backend after a crash left expired escrow: %v", err)
	}
}

// EXPIRY IS NOT PROOF THAT A CONTAINER IS GONE, and only the idle case may act
// as though it were.
//
// A lease ages out when nothing is heartbeating it, which says the control-plane
// holder stopped — not that the compute stopped. For escrow nobody ever launched
// that distinction does not exist, and letting it expire out of the way is what
// keeps a crashed deployment restartable. For a lease in a RUNNING phase there
// is, or may be, a container on that host right now.
//
// Reading them the same way lets a host change its backend out from under work
// the new backend cannot see: the Docker container keeps running, Tart
// reconciliation cannot enumerate it, the reaper frees the lease, and the next
// escrow puts a second job on a machine that is still running the first.
//
// The way out is unchanged and does not deadlock: the reaper lives in the
// server, the server starts without this node, and a terminalised lease stops
// counting here.
func TestAnExpiredRunningLeaseStillPinsTheHostsBackend(t *testing.T) {
	now := time.Now().UTC()

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{{
			Label: "small", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "img",
		}},
		WithClock(func() time.Time { return now }),
		WithLeaseTTL(30*time.Second))

	home := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker,
		VCPU: 64, Memory: 256 * config.GiB,
	}
	if _, err := a.RegisterNode(t.Context(), home); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	fresh, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	// A container is running on this host under the docker backend.
	advanceTo(t, a, fresh, PhaseBusy)

	// The control plane dies. Nothing heartbeats, and the lease ages out — while
	// the container carries on.
	now = now.Add(31 * time.Second)

	moved := home
	moved.Provider = config.ProviderTart

	if _, err := a.RegisterNode(t.Context(), moved); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("a host with a container still running changed its backend because the "+
			"lease holding that container had aged out: %v", err)
	}
}
