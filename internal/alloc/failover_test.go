package alloc

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/config"
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
