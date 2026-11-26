package config

import "fmt"

// Contribution is what one node offers the deployment, and how that was decided.
type Contribution struct {
	// VCPU and Memory are what the allocator may place work against.
	VCPU   int
	Memory ByteSize
	// Reserved is what node.reserve_memory took off Memory: held back for the
	// host and its storage, never offered.
	Reserved ByteSize
	// DetectedMemory is what the machine has, as Contribution was told; zero
	// where the guests run elsewhere and nothing was measured.
	DetectedMemory ByteSize

	// Warnings are things the operator should know but that are not errors —
	// billet is doing what it was told, and what it was told looks like a typo.
	Warnings []string
}

// Contribution resolves what this node offers from what it declared and what the
// machine turned out to have.
//
// PURE, AND THE DETECTED VALUES ARE ARGUMENTS, so the decision can be tested
// against hardware this host does not have. Detection is a syscall; which number
// wins is a rule, and a rule that can only be exercised on the machine running
// the tests is a rule that is tested on exactly one configuration.
//
// FIELD BY FIELD, NOT ALL OR NOTHING. A host that sets max_memory to hold RAM
// back for a database has said nothing about its cores, and treating the pair as
// one decision would read that as "0 vCPU" and register a node that can never be
// given work.
func (n *NodeConfig) Contribution(detectedVCPU int, detectedMemory ByteSize) Contribution {
	c := Contribution{VCPU: detectedVCPU, Memory: detectedMemory, DetectedMemory: detectedMemory}

	// ZERO IS "I DID NOT SAY". Negative is refused when the config is validated,
	// so by here the only values are unset and meant.
	if n.MaxVCPU > 0 {
		c.VCPU = n.MaxVCPU
	}

	if n.MaxMemory > 0 {
		c.Memory = n.MaxMemory
	}

	// THE TYPO WARNING READS WHAT WAS DECLARED, before the reserve: max_memory
	// above the machine is the operator's own number, and a reserve under it does
	// not make that number any less a decision to overcommit.
	declared := c.Memory

	// THE RESERVE COMES OFF LAST and never makes an offer negative: a reserve that
	// leaves nothing is refused by the node at startup (Offered), and a negative
	// offer would be a ceiling every comparison passes.
	if n.ReserveMemory > 0 && n.Provider.RunsOnHost() {
		c.Reserved = min(n.ReserveMemory, c.Memory)
		c.Memory -= c.Reserved
	}

	// NOTHING TO COMPARE AGAINST WHEN THE WORK RUNS SOMEWHERE ELSE.
	//
	// An ec2 node is an orchestrator: it holds credentials and calls an API, and
	// the compute appears in a region rather than in this box. So the hardware
	// underneath it is unrelated to what it can offer, and every warning below
	// would fire on every boot of a correctly configured cloud node — which costs
	// more than it sounds, because this is the one warning that means something
	// real on a bare-metal host, and an operator who sees it daily on the cloud
	// node stops reading it on the EPYC box.
	//
	// What the node DECLARED still governs; only the comparison is dropped.
	if !n.Provider.RunsOnHost() {
		return c
	}

	// ALLOWED, AND SAID OUT LOUD. An operator may know something billet does not
	// — that the workload is IO-bound, or that this host is deliberately
	// oversubscribed — so a number above the hardware is a decision billet has no
	// standing to refuse. It is also exactly what a typo looks like, and the cost
	// of the typo is a node accepting work it cannot run, which surfaces as jobs
	// failing on one machine rather than as anything pointing at the config.
	if c.VCPU > detectedVCPU {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"node.max_vcpu is %d but this machine has %d; billet will place work as if the "+
				"larger number were real", c.VCPU, detectedVCPU))
	}

	if declared > detectedMemory {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"node.max_memory is %s but this machine has %s; billet will place work as if the "+
				"larger number were real", declared, detectedMemory))
	}

	return c
}

// Offered refuses a contribution whose reserve left the node no memory to offer.
// A node registered with nothing is one no tier can ever place work on, and it
// would look healthy; refused, the operator reads the two numbers instead.
func (c Contribution) Offered() error {
	if c.Reserved > 0 && c.Memory <= 0 {
		return fmt.Errorf("node.reserve_memory holds back %s, all the memory this node would "+
			"offer; lower it, or raise node.max_memory", c.Reserved)
	}

	return nil
}

// checkReserveMemory refuses a reserve that cannot mean what it says, before the
// node detects anything: on a node whose guests run elsewhere there is no host
// memory to hold back, and a reserve at or above a declared max_memory leaves
// nothing to offer whatever the machine has.
func (n *NodeConfig) checkReserveMemory() []error {
	if n.ReserveMemory == 0 {
		return nil
	}

	if !n.Provider.RunsOnHost() {
		return []error{fmt.Errorf("node.reserve_memory is set, but with node.provider %s the guests do not run "+
			"on this host, so it has no memory of its own to hold back; leave it unset", n.Provider)}
	}

	if n.MaxMemory > 0 && n.ReserveMemory >= n.MaxMemory {
		return []error{fmt.Errorf("node.reserve_memory %s is not less than node.max_memory %s, so "+
			"this node would offer no memory at all", n.ReserveMemory, n.MaxMemory)}
	}

	return nil
}
