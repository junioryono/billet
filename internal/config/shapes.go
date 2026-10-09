package config

import (
	"fmt"
	"strings"
)

// RemoteShape is what a remote backend's ordered shape list holds, whatever that
// backend calls its shapes — an EC2 instance type, a CodeBuild compute type.
//
// AN ALIAS RATHER THAN A DEFINED TYPE, and that is the whole point: a defined type
// would inherit none of the methods and none of the validation, so there would be
// two shape catalogues to keep in step and two validators to keep in agreement.
// The ledger column, the wire field and the validator are all one thing; this name
// exists so a codebuild config field does not have to be spelled `EC2InstanceType`
// and read as a copy-paste mistake.
type RemoteShape = EC2InstanceType

// EC2InstanceType is one shape billet may buy, and what it holds.
//
// The vCPU and memory are DECLARED rather than looked up, because the allocator
// has already escrowed a size against this node before any of this is consulted:
// a shape that turns out smaller than the lease it was chosen for over-commits a
// machine nobody can see.
//
// It carries the ORIGINAL name because the ledger column, the registration field
// and every existing config say `ec2`, and renaming a shipped on-disk spelling to
// tidy a Go identifier is a flag day for nothing. `RemoteShape` above is the alias
// a second remote backend uses.
type EC2InstanceType struct {
	Type   string   `yaml:"type" json:"type"`
	VCPU   int      `yaml:"vcpu" json:"vcpu"`
	Memory ByteSize `yaml:"memory" json:"memory"`
	// PriceUSDPerHour is the operator-audited compute rate used to report the
	// maximum configured exposure. It is required because the answer has to be in
	// the config, not fetched from a mutable service when a job arrives. It is not
	// an admission gate and cannot make an already-accepted job wait because a
	// copied price went stale.
	PriceUSDPerHour USDPerHour `yaml:"price_usd_per_hour" json:"price_usd_per_hour"`
}

// CheckRemoteShapes validates one remote backend's ordered shape catalogue.
//
// Exported because the allocator receives the same catalogue over node
// registration and cannot assume it came through Config.Load.
//
// THE PROVIDER RATHER THAN A FIELD NAME, so the messages name the key the operator
// actually wrote. It used to be `CheckEC2InstanceTypes`, whose prose was hard-coded
// to `node.ec2.instance_types` — which was fine while there was one remote backend
// and became a diagnostic pointing at a field that is not in a codebuild
// operator's file the moment there were two. `ProviderKind.ShapeField` is the one
// place that mapping lives, and the allocator has the registered provider to hand.
func CheckRemoteShapes(p ProviderKind, types []RemoteShape) []error {
	var errs []error
	field := p.ShapeField()

	if len(types) == 0 {
		errs = append(errs, fmt.Errorf("%s needs at least one shape; billet ships no table of "+
			"%s, so a shape it may buy has to be declared along with what it holds",
			field, p.shapeNoun()))

		return errs
	}

	seen := make(map[string]struct{}, len(types))

	for i := range types {
		it := &types[i]
		where := fmt.Sprintf("%s[%d]", field, i)

		if name := strings.TrimSpace(it.Type); name == "" {
			errs = append(errs, fmt.Errorf("%s: type is required", where))
		} else {
			// A repeat is a typo rather than a stronger preference, exactly as it
			// is in a tier's provider list: billet picks one shape per launch, so
			// collapsing a duplicate silently would hide the mistake.
			if _, dup := seen[name]; dup {
				errs = append(errs, fmt.Errorf("%s: type %q is listed twice", where, name))
			}

			seen[name] = struct{}{}
		}

		if it.VCPU <= 0 {
			errs = append(errs, fmt.Errorf("%s: vcpu must be more than zero; it is what billet "+
				"matches an already-reserved lease against", where))
		}

		if it.Memory <= 0 {
			errs = append(errs, fmt.Errorf("%s: memory must be more than zero; it is what "+
				"billet matches an already-reserved lease against", where))
		}
	}

	return errs
}

// localRemoteShapes reports this node's own ordered shape catalogue, and which
// backend it belongs to.
//
// ONE READER FOR "WHAT MAY THIS HOST BUY", so the tier-fits check below does not
// have to know how many remote backends exist. A host-backed provider has no shape
// list at all — its capacity is the machine — which is why the answer is empty
// rather than a zero-length list with a provider attached.
func (c *Config) localRemoteShapes() (ProviderKind, []RemoteShape) {
	if c.Node == nil {
		return "", nil
	}

	switch c.Node.Provider {
	case ProviderEC2:
		if c.Node.EC2 != nil {
			return ProviderEC2, c.Node.EC2.InstanceTypes
		}

	case ProviderCodeBuild:
		if c.Node.CodeBuild != nil {
			return ProviderCodeBuild, c.Node.CodeBuild.ComputeTypes
		}

	case ProviderDocker, ProviderFirecracker, ProviderTart:
		return "", nil
	}

	return "", nil
}

// validateRemoteShapes refuses a tier this node could be given and could not buy.
//
// A TIER LARGER THAN EVERY DECLARED SHAPE QUEUES FOREVER WITH NOTHING SAYING WHY.
// The allocator escrows it happily, because the node's budget covers it — the
// failure appears only after GitHub has assigned the job, as a launch error on
// one host. billet already refuses a tier pinned to a host that cannot run its
// guest OS at load time, for exactly this reason.
//
// Only checkable when one file holds both the node and the tiers, which is the
// single-machine shape. In a fleet the node's file has no tiers, and the launch
// path's error is what remains — it names the size asked for and every shape
// declared, so it is actionable wherever it is read.
func (c *Config) validateRemoteShapes() []error {
	kind, shapes := c.localRemoteShapes()
	if kind == "" {
		return nil
	}

	if len(shapes) == 0 {
		// Already reported as a missing field; saying it twice helps nobody.
		return nil
	}

	var errs []error

	for i := range c.Tiers {
		t := &c.Tiers[i]

		if !t.AcceptsProvider(kind) {
			continue
		}

		// A TIER PINNED ELSEWHERE IS NOT THIS NODE'S PROBLEM. It names a different
		// machine, so the shapes this one may buy say nothing about whether it can
		// run.
		if t.Node != "" && t.Node != c.Node.Name {
			continue
		}

		// NOR IS A TIER THIS NODE COULD NEVER BE GIVEN. Every node in a fleet reads
		// the same tier catalogue, so a small cloud node sees the tiers meant for a
		// large one — and refusing them would make one deployment's config
		// unloadable on half its machines. The allocator will never place work here
		// that exceeds this node's own contribution, so a shape that cannot hold it
		// is not a contradiction.
		//
		// What remains refused is the case that really is broken: a tier this node
		// IS eligible for, and no declared shape can buy.
		// A PINNED TIER IS NOT LET THROUGH BY THAT, and the distinction is the
		// whole point: pinned means this node or nowhere, so oversize is not
		// "another machine's job" — it is a tier that can never run at all.
		if t.Node == "" && ((c.Node.MaxVCPU > 0 && t.VCPU > c.Node.MaxVCPU) ||
			(c.Node.MaxMemory > 0 && t.Memory > c.Node.MaxMemory)) {
			continue
		}

		fits := false

		for _, shape := range shapes {
			if shape.VCPU >= t.VCPU && shape.Memory >= t.Memory {
				fits = true

				break
			}
		}

		if fits {
			continue
		}

		declared := make([]string, 0, len(shapes))
		for _, shape := range shapes {
			declared = append(declared,
				fmt.Sprintf("%s (%d vCPU, %s)", shape.Type, shape.VCPU, shape.Memory))
		}

		errs = append(errs, fmt.Errorf(
			"tier %q requests %d vCPU and %s, which no shape in %s can hold (%s); a job on this "+
				"tier would be admitted and then fail to launch",
			t.Label, t.VCPU, t.Memory, kind.ShapeField(), strings.Join(declared, ", ")))
	}

	return errs
}
