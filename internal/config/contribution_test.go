package config

import (
	"strings"
	"testing"
)

// WHAT A NODE OFFERS IS A DECISION, not a measurement, and this is where the two
// meet: the machine's hardware is the default and the machine's own config
// overrides it, field by field.
//
// Field by field matters. A host that sets max_memory to hold back RAM for a
// database has said nothing about its cores, and reading that as "0 vCPU" would
// register a node nothing can ever be placed on.
func TestAContributionPrefersWhatTheNodeSaid(t *testing.T) {
	t.Parallel()

	const (
		detVCPU = 128
		detMem  = 512 * GiB
	)

	for _, tc := range []struct {
		name       string
		node       NodeConfig
		wantVCPU   int
		wantMemory ByteSize
	}{
		{
			name:     "silent, so the machine answers",
			node:     NodeConfig{Provider: ProviderFirecracker},
			wantVCPU: detVCPU, wantMemory: detMem,
		},
		{
			name:     "both stated",
			node:     NodeConfig{Provider: ProviderFirecracker, MaxVCPU: 120, MaxMemory: 480 * GiB},
			wantVCPU: 120, wantMemory: 480 * GiB,
		},
		{
			// Holding back memory says nothing about cores.
			name:     "only memory stated",
			node:     NodeConfig{Provider: ProviderFirecracker, MaxMemory: 480 * GiB},
			wantVCPU: detVCPU, wantMemory: 480 * GiB,
		},
		{
			name:     "only vcpu stated",
			node:     NodeConfig{Provider: ProviderFirecracker, MaxVCPU: 120},
			wantVCPU: 120, wantMemory: detMem,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.node.Contribution(detVCPU, detMem)

			if got.VCPU != tc.wantVCPU {
				t.Errorf("vcpu = %d, want %d", got.VCPU, tc.wantVCPU)
			}

			if got.Memory != tc.wantMemory {
				t.Errorf("memory = %s, want %s", got.Memory, tc.wantMemory)
			}

			if len(got.Warnings) != 0 {
				t.Errorf("an ordinary contribution warned: %v", got.Warnings)
			}
		})
	}
}

// OVERCOMMITTING IS ALLOWED AND SAID OUT LOUD.
//
// An operator may know something billet does not — that the workload is
// IO-bound, or that this host is deliberately oversubscribed — so a number above
// the hardware is a decision billet has no standing to refuse. It is also the
// exact shape of a typo, and the cost of the typo is a node that accepts work it
// cannot run, which surfaces as jobs failing on one machine rather than as
// anything pointing at the config. So it is permitted, and it is reported.
func TestAContributionAboveTheHardwareIsAllowedAndWarned(t *testing.T) {
	t.Parallel()

	const (
		detVCPU = 8
		detMem  = 16 * GiB
	)

	for _, tc := range []struct {
		name string
		node NodeConfig
		want string
	}{
		{
			name: "more cores than exist",
			node: NodeConfig{Provider: ProviderDocker, MaxVCPU: 64},
			want: "max_vcpu",
		},
		{
			name: "more memory than exists",
			node: NodeConfig{Provider: ProviderDocker, MaxMemory: 64 * GiB},
			want: "max_memory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.node.Contribution(detVCPU, detMem)

			if len(got.Warnings) == 0 {
				t.Fatalf("contributing more than the machine has did not warn: %+v", got)
			}

			if !strings.Contains(strings.Join(got.Warnings, " "), tc.want) {
				t.Errorf("the warning does not name the field: %v", got.Warnings)
			}
		})
	}
}

// Equal to the hardware is not over it, and warning there would train an
// operator to ignore the warning that matters.
func TestAContributionEqualToTheHardwareIsSilent(t *testing.T) {
	t.Parallel()

	n := NodeConfig{Provider: ProviderDocker, MaxVCPU: 8, MaxMemory: 16 * GiB}

	got := n.Contribution(8, 16*GiB)

	if len(got.Warnings) != 0 {
		t.Errorf("contributing exactly what the machine has warned: %v", got.Warnings)
	}
}

// AN ORCHESTRATOR'S OWN HARDWARE IS NOT ITS CONTRIBUTION, AND COMPARING THEM IS
// NOISE.
//
// An ec2 node holds credentials and calls an API; the compute appears somewhere
// else. So the box it runs on — plausibly the smallest instance that will hold
// the process — has no relationship to what the node can offer, and the
// overcommit warning fires on every single boot of a correctly configured
// deployment. That is worse than useless: this exact warning is the one that
// matters on a bare-metal host, and an operator who sees it every morning on the
// cloud node stops reading it on the EPYC box.
//
// The numbers themselves are unaffected — what the node declared is still what it
// contributes. Only the comparison against local hardware is dropped, because
// there is nothing local to compare against.
func TestACloudNodeIsNotComparedAgainstTheMachineItRunsOn(t *testing.T) {
	t.Parallel()

	// A t4g.nano's worth of orchestrator, buying a fleet.
	const (
		detVCPU = 2
		detMem  = 512 * MiB
	)

	n := NodeConfig{Provider: ProviderEC2, MaxVCPU: 512, MaxMemory: 2048 * GiB}

	got := n.Contribution(detVCPU, detMem)

	if len(got.Warnings) != 0 {
		t.Errorf("a cloud node was warned about the hardware of the machine orchestrating "+
			"it: %v", got.Warnings)
	}

	// The declaration still governs, which is the half that must not change.
	if got.VCPU != 512 {
		t.Errorf("vcpu = %d, want the declared 512", got.VCPU)
	}

	if got.Memory != 2048*GiB {
		t.Errorf("memory = %s, want the declared 2048GiB", got.Memory)
	}
}

// A BACKEND THAT RUNS WORK HERE IS STILL MEASURED AGAINST HERE.
//
// The companion to the test above, and the reason the rule is keyed on the
// backend rather than applied everywhere: silencing the comparison for a host
// backend would remove the warning from the only place it means anything.
func TestEveryHostBackendIsStillComparedAgainstItsMachine(t *testing.T) {
	t.Parallel()

	for _, p := range []ProviderKind{ProviderDocker, ProviderFirecracker, ProviderTart} {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()

			n := NodeConfig{Provider: p, MaxVCPU: 64}

			got := n.Contribution(8, 16*GiB)

			if len(got.Warnings) == 0 {
				t.Errorf("%s runs work on this machine, so contributing 64 of its 8 cores "+
					"should have been reported", p)
			}
		})
	}
}

// THE RESERVE COMES OFF WHAT THE NODE WOULD OTHERWISE OFFER (#755): the machine,
// or max_memory where it is stated, and never the vCPU.
func TestAReserveComesOffWhatTheNodeWouldOffer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		node         NodeConfig
		wantMemory   ByteSize
		wantReserved ByteSize
	}{
		{
			name:       "off the machine",
			node:       NodeConfig{Provider: ProviderFirecracker, ReserveMemory: 69 * GiB},
			wantMemory: 443 * GiB, wantReserved: 69 * GiB,
		},
		{
			name:       "off a stated max_memory",
			node:       NodeConfig{Provider: ProviderFirecracker, MaxMemory: 480 * GiB, ReserveMemory: 50 * GiB},
			wantMemory: 430 * GiB, wantReserved: 50 * GiB,
		},
		{
			// Never negative: the node refuses this at startup (Offered).
			name:       "all of it",
			node:       NodeConfig{Provider: ProviderFirecracker, ReserveMemory: 600 * GiB},
			wantMemory: 0, wantReserved: 512 * GiB,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.node.Contribution(128, 512*GiB)

			if got.Memory != tc.wantMemory || got.Reserved != tc.wantReserved {
				t.Fatalf("memory %s, reserved %s; want %s and %s", got.Memory, got.Reserved,
					tc.wantMemory, tc.wantReserved)
			}

			if got.VCPU != 128 {
				t.Errorf("vcpu = %d: a memory reserve said nothing about cores", got.VCPU)
			}

			if got.DetectedMemory != 512*GiB {
				t.Errorf("detected = %s, want the machine's 512GiB", got.DetectedMemory)
			}
		})
	}
}

// A RESERVE THAT LEAVES NOTHING IS REFUSED, and one that leaves something is not:
// a node registered with no memory looks healthy and can never be given work.
func TestAReserveThatLeavesNothingIsRefused(t *testing.T) {
	t.Parallel()

	all := (&NodeConfig{Provider: ProviderFirecracker, ReserveMemory: 16 * GiB}).Contribution(8, 16*GiB)
	if err := all.Offered(); err == nil || !strings.Contains(err.Error(), "node.reserve_memory holds back 16GiB") {
		t.Fatalf("Offered = %v, want the reserve refused by its size", err)
	}

	some := (&NodeConfig{Provider: ProviderFirecracker, ReserveMemory: 4 * GiB}).Contribution(8, 16*GiB)
	if err := some.Offered(); err != nil {
		t.Fatalf("Offered = %v for a reserve that leaves 12GiB", err)
	}

	// No reserve and nothing detected is a cloud node's contribution, not this.
	if err := (&NodeConfig{Provider: ProviderEC2, MaxMemory: 64 * GiB}).Contribution(0, 0).Offered(); err != nil {
		t.Fatalf("Offered = %v with no reserve at all", err)
	}
}

// THE TYPO WARNING READS WHAT WAS DECLARED. max_memory above the machine is the
// operator's own number, and a reserve that brings the offer under the machine does
// not unsay it.
func TestAReserveDoesNotHideAnOvercommittedMaxMemory(t *testing.T) {
	t.Parallel()

	got := (&NodeConfig{Provider: ProviderFirecracker, MaxMemory: 600 * GiB, ReserveMemory: 200 * GiB}).
		Contribution(128, 512*GiB)

	if got.Memory != 400*GiB {
		t.Fatalf("memory = %s, want 600GiB less 200GiB", got.Memory)
	}

	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "node.max_memory is 600GiB but this machine has 512GiB") {
		t.Fatalf("warnings = %q, want max_memory's own overcommit named", got.Warnings)
	}
}

// A CLOUD NODE HAS NO HOST MEMORY TO HOLD BACK: the reserve is left out of its
// offer, and config validation refuses it outright.
func TestAReserveIsNoPartOfACloudNodesOffer(t *testing.T) {
	t.Parallel()

	got := (&NodeConfig{Provider: ProviderEC2, MaxMemory: 64 * GiB, ReserveMemory: 8 * GiB}).Contribution(0, 0)
	if got.Memory != 64*GiB || got.Reserved != 0 {
		t.Fatalf("memory %s, reserved %s; want all 64GiB and nothing reserved", got.Memory, got.Reserved)
	}
}
