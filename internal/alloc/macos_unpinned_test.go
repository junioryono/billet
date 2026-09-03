package alloc

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func twoBackendMacOSTier() config.Tier {
	return config.Tier{
		Label: "macos", Providers: []config.ProviderKind{config.ProviderTart, config.ProviderCodeBuild},
		GuestOS: config.GuestMacOS, VCPU: 4, Memory: 8 * config.GiB,
		Launch: map[config.ProviderKind]config.TierLaunch{
			config.ProviderTart:      {Image: "macos-26"},
			config.ProviderCodeBuild: {Image: "aws/codebuild/macos-arm-base:14"},
		},
	}
}

func twoBackendMacOSPolicies(fleetLimit *int) map[string]config.NodePolicy {
	return map[string]config.NodePolicy{
		"mac-1":  {Name: "mac-1", Provider: config.ProviderTart, GuestOS: []config.GuestOS{config.GuestMacOS}},
		"cb-mac": {Name: "cb-mac", Provider: config.ProviderCodeBuild, GuestOS: []config.GuestOS{config.GuestMacOS}, MacOSVMLimit: fleetLimit},
	}
}

// ONE macOS LABEL, TWO BACKENDS. A macOS tier that lists several providers may
// leave its node unnamed, because the licence is counted per host at placement
// whether or not the tier named the host: the owned Mac fills to Apple's two,
// the managed fleet's node to the capacity its policy declares, and the label
// has nowhere else to go after that.
func TestAnUnpinnedMacOSTierCountsEachHostsLicenceSeparately(t *testing.T) {
	one := 1
	tier := twoBackendMacOSTier()
	policies := twoBackendMacOSPolicies(&one)
	// A decoy tart host whose allowlist excludes macOS: it runs a listed backend
	// and must still never be chosen.
	policies["linux-mac"] = config.NodePolicy{
		Name: "linux-mac", Provider: config.ProviderTart, GuestOS: []config.GuestOS{config.GuestLinux}}
	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB, Nodes: policies},
		[]config.Tier{tier})

	mac, _ := mustRegister(t, a, NodeRegistration{
		Name: "mac-1", Provider: config.ProviderTart, VCPU: 64, Memory: 512 * config.GiB})
	cb, _ := mustRegister(t, a, NodeRegistration{
		Name: "cb-mac", Provider: config.ProviderCodeBuild, VCPU: 64, Memory: 512 * config.GiB,
		EC2Shapes: []config.RemoteShape{{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 8, Memory: 24 * config.GiB}}})
	// And a docker host, whose backend the tier does not list at all.
	mustRegister(t, a, NodeRegistration{
		Name: "docker-1", Provider: config.ProviderDocker, VCPU: 64, Memory: 512 * config.GiB})
	mustRegister(t, a, NodeRegistration{
		Name: "linux-mac", Provider: config.ProviderTart, VCPU: 64, Memory: 512 * config.GiB})

	// An unset max_concurrent defaults to what the two hosts permit between them,
	// as config.Load would have set it — and that number, given explicitly, is
	// accepted: the bound is inclusive.
	if got := a.tiers["macos"].MaxConcurrent; got != config.DefaultMacOSVMLimit+1 {
		t.Fatalf("max_concurrent defaulted to %d, want %d", got, config.DefaultMacOSVMLimit+1)
	}
	exact := twoBackendMacOSTier()
	exact.MaxConcurrent = config.DefaultMacOSVMLimit + 1
	if _, err := New(openTestLedger(t), Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB,
		Nodes: twoBackendMacOSPolicies(&one)}, []config.Tier{exact}); err != nil {
		t.Fatalf("an explicit max_concurrent equal to the hosts' total was refused: %v", err)
	}

	got := eligible(t, a, tier)
	slices.Sort(got)
	if !slices.Equal(got, []string{"cb-mac", "mac-1"}) {
		t.Fatalf("eligible = %v, want exactly the two declared macOS hosts", got)
	}
	if got := roomOn(t, a, mac, tier); got != config.DefaultMacOSVMLimit {
		t.Errorf("room on the owned Mac = %d, want Apple's %d", got, config.DefaultMacOSVMLimit)
	}
	if got := roomOn(t, a, cb, tier); got != 1 {
		t.Errorf("room on the fleet's node = %d, want the declared 1", got)
	}

	// Three reservations fit — two on the Mac, one on the fleet — and the fourth
	// is refused, because no host has a guest slot left.
	for i := range 3 {
		if _, err := a.Reserve(t.Context(), "macos"); err != nil {
			t.Fatalf("reservation %d: %v", i+1, err)
		}
	}
	if got := roomOn(t, a, mac, tier) + roomOn(t, a, cb, tier); got != 0 {
		t.Errorf("room after three guests = %d, want 0", got)
	}
	if _, err := a.Reserve(t.Context(), "macos"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a fourth reservation = %v, want %v", err, ErrNoCapacity)
	}
}

// The shapes alloc.New refuses, because it is exported and cannot assume its
// catalogue came through config.Load: one backend with no host named, an
// unpinned reservation, a listed backend with no declared host, and a managed
// fleet's host that declares no limit — which macOSLimit would otherwise read
// as Apple's two.
func TestAnUnpinnedMacOSTierIsHeldToTheConfigRulesByTheAllocatorToo(t *testing.T) {
	one := 1
	single := twoBackendMacOSTier()
	single.Providers, single.Launch = nil, nil
	single.Provider, single.Image = config.ProviderTart, "macos-26"

	reserved := twoBackendMacOSTier()
	reserved.Reserved = 1

	undeclared := twoBackendMacOSPolicies(&one)
	delete(undeclared, "cb-mac")

	overCommitted := twoBackendMacOSTier()
	overCommitted.MaxConcurrent = 4

	for _, tc := range []struct {
		name     string
		tier     config.Tier
		policies map[string]config.NodePolicy
		want     string
	}{
		{"one backend", single, twoBackendMacOSPolicies(&one), "names no node"},
		{"a reservation", reserved, twoBackendMacOSPolicies(&one), "reserves 1 guests"},
		{"a backend with no declared host", twoBackendMacOSTier(), undeclared, "no declared node runs codebuild"},
		{"a fleet whose host declares no limit", twoBackendMacOSTier(), twoBackendMacOSPolicies(nil), "declares no macos_vm_limit"},
		{"more concurrency than the hosts permit", overCommitted, twoBackendMacOSPolicies(&one), "between 1 and 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(openTestLedger(t), Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB, Nodes: tc.policies},
				[]config.Tier{tc.tier})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
