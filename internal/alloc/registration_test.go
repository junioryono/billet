package alloc

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// testRegistration is a host big enough that it is never the thing under test.
//
// DELIBERATELY LARGER THAN ANY BUDGET THESE TESTS SET, so the deployment-wide
// ceiling stays the binding constraint and every test written before nodes had
// capacity keeps measuring exactly what it measured before. A test that wants
// per-node capacity to bite says so by registering a specific size.
func testRegistration(name string, kind config.ProviderKind) NodeRegistration {
	return NodeRegistration{
		Name:     name,
		Provider: kind,
		VCPU:     1 << 20,
		Memory:   1 << 20 * config.GiB,
	}
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
		`SELECT total_vcpu, total_memory, site FROM nodes WHERE name = ?`, reg.Name).
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
		`SELECT total_vcpu, total_memory FROM nodes WHERE name = ?`, first.Name).
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

	a := newAllocator(t, Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{small})

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
