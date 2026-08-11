package alloc

import (
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

	if err := a.RegisterNode(t.Context(), reg); err != nil {
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
	if err := a.RegisterNode(t.Context(), first); err != nil {
		t.Fatalf("first RegisterNode: %v", err)
	}

	second := first
	second.VCPU, second.Memory = 8, 32*config.GiB

	if err := a.RegisterNode(t.Context(), second); err != nil {
		t.Fatalf("re-registering smaller: %v", err)
	}

	var vcpu int
	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT total_vcpu FROM nodes WHERE name = ?`, first.Name).Scan(&vcpu); err != nil {
		t.Fatalf("read the node back: %v", err)
	}

	if vcpu != second.VCPU {
		t.Errorf("total_vcpu = %d, want %d — the new contribution did not replace the old",
			vcpu, second.VCPU)
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
			err := a.RegisterNode(t.Context(), tc.reg)
			if err == nil {
				t.Fatal("a node contributing nothing was registered")
			}

			if !strings.Contains(strings.ToLower(err.Error()), tc.field) {
				t.Errorf("the error does not name what was missing: %v", err)
			}
		})
	}
}
