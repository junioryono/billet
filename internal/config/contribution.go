package config

import "fmt"

// Contribution is what one node offers the deployment, and how that was decided.
type Contribution struct {
	// VCPU and Memory are what the allocator may place work against.
	VCPU   int
	Memory ByteSize

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
	c := Contribution{VCPU: detectedVCPU, Memory: detectedMemory}

	// ZERO IS "I DID NOT SAY". Negative is refused when the config is validated,
	// so by here the only values are unset and meant.
	if n.MaxVCPU > 0 {
		c.VCPU = n.MaxVCPU
	}

	if n.MaxMemory > 0 {
		c.Memory = n.MaxMemory
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

	if c.Memory > detectedMemory {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"node.max_memory is %s but this machine has %s; billet will place work as if the "+
				"larger number were real", c.Memory, detectedMemory))
	}

	return c
}
