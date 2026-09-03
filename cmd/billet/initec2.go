package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/provider/ec2"
)

// ec2InitFlags is the ec2-only flag set `billet init --provider ec2` reads.
type ec2InitFlags struct {
	region          string
	subnet          string
	securityGroups  []string
	untrustedGroups []string
	instanceTypes   []string
	priceOverrides  []string
	maxVCPU         int
	maxMemory       string
}

// ec2InitParams turns the ec2 flags into initconfig inputs, fetching each shape's
// vcpu, memory and on-demand price from AWS. It returns the resolved params and
// the cloud budget (which is the ceiling, not a measurement). The fetches run
// under the operator's own AWS credentials from the environment or an instance
// role — the same the config's future node will not have, which is why these are
// an init-time convenience rather than a node-runtime permission.
func ec2InitParams(
	ctx context.Context, f ec2InitFlags,
) (*initconfig.EC2Params, int, config.ByteSize, error) {
	if f.region == "" {
		return nil, 0, 0, errors.New("--region is required for ec2: billet cannot choose which " +
			"region to launch in")
	}

	// Validated before any AWS call: the region is interpolated into the API
	// endpoint host, so a malformed one would otherwise send a signed request to a
	// host derived from it.
	if err := config.CheckEC2Region(f.region); err != nil {
		return nil, 0, 0, fmt.Errorf("--region: %w", err)
	}

	if f.subnet == "" {
		return nil, 0, 0, errors.New("--subnet is required for ec2: billet cannot choose which " +
			"network a runner should be able to reach")
	}

	if len(f.securityGroups) == 0 {
		return nil, 0, 0, errors.New("--security-group is required at least once for ec2: launching " +
			"with no group lets EC2 pick the VPC default, which usually reaches more than intended")
	}

	if f.maxVCPU <= 0 {
		return nil, 0, 0, errors.New("--max-vcpu is required for ec2 and must be positive: it is the " +
			"cloud budget billet may run at once, and there is no host to detect it from")
	}

	if f.maxMemory == "" {
		return nil, 0, 0, errors.New("--max-memory is required for ec2, e.g. 64GiB: it is the cloud " +
			"budget billet may run at once, and there is no host to detect it from")
	}

	budgetMemory, err := config.ParseByteSize(f.maxMemory)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("--max-memory: %w", err)
	}

	if len(f.instanceTypes) == 0 {
		return nil, 0, 0, errors.New("--instance-type is required at least once for ec2: billet ships " +
			"no table of EC2 shapes, so it must be told which ones it may buy")
	}

	overrides, err := parsePriceOverrides(f.priceOverrides)
	if err != nil {
		return nil, 0, 0, err
	}

	shapes, err := resolveEC2Shapes(ctx, f.region, f.instanceTypes, overrides)
	if err != nil {
		return nil, 0, 0, err
	}

	return &initconfig.EC2Params{
		Region:                  f.region,
		SubnetID:                f.subnet,
		SecurityGroups:          f.securityGroups,
		UntrustedSecurityGroups: f.untrustedGroups,
		Shapes:                  shapes,
	}, f.maxVCPU, budgetMemory, nil
}

// resolveEC2Shapes fetches what each named shape holds and its on-demand price,
// preserving the order the flags were given and refusing a duplicate. A shape's
// price may be overridden (a fetch that is ambiguous or unavailable), but its
// vcpu and memory always come from AWS: billet must not guess a shape's size,
// because a shape smaller than the lease chosen for it overcommits a host the
// allocator escrowed against.
func resolveEC2Shapes(
	ctx context.Context, region string, types []string, overrides map[string]config.USDPerHour,
) ([]config.EC2InstanceType, error) {
	ordered, err := resolveShapeTypes(types, overrides)
	if err != nil {
		return nil, err
	}

	infos, err := ec2.DescribeInstanceTypes(ctx, region, "", nil, ordered)
	if err != nil {
		return nil, fmt.Errorf("look up shape vcpu and memory in %s (this needs AWS credentials in "+
			"the environment or an instance role): %w", region, err)
	}

	byType := make(map[string]ec2.InstanceTypeInfo, len(infos))
	for _, info := range infos {
		byType[info.Type] = info
	}

	shapes := make([]config.EC2InstanceType, 0, len(ordered))
	for _, t := range ordered {
		info, ok := byType[t]
		if !ok {
			return nil, fmt.Errorf("AWS did not return shape %q in region %s; check the name and "+
				"that it is offered there", t, region)
		}
		if info.VCPU <= 0 || info.MemoryMiB <= 0 {
			return nil, fmt.Errorf("AWS reported shape %q as %d vCPU and %d MiB, which billet cannot "+
				"use", t, info.VCPU, info.MemoryMiB)
		}

		price, ok := overrides[t]
		if !ok {
			price, err = ec2.OnDemandPriceUSDPerHour(ctx, region, t, nil)
			if err != nil {
				return nil, fmt.Errorf("%w", err)
			}
		}

		shapes = append(shapes, config.EC2InstanceType{
			Type:            t,
			VCPU:            info.VCPU,
			Memory:          config.ByteSize(info.MemoryMiB) * config.MiB,
			PriceUSDPerHour: price,
		})
	}

	return shapes, nil
}

// resolveShapeTypes normalizes the requested shape names and validates the price
// overrides against them, BEFORE any AWS call. It refuses an empty or duplicate
// name, and a price override naming a shape that is not requested — a typo, not a
// no-op: ignoring it would let billet fetch a price the operator believed pinned.
func resolveShapeTypes(types []string, overrides map[string]config.USDPerHour) ([]string, error) {
	seen := make(map[string]bool, len(types))

	ordered := make([]string, 0, len(types))
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, errors.New("--instance-type: an empty shape name")
		}
		if seen[t] {
			return nil, fmt.Errorf("--instance-type %q is listed twice", t)
		}
		seen[t] = true
		ordered = append(ordered, t)
	}

	for name := range overrides {
		if !seen[name] {
			return nil, fmt.Errorf("--price names shape %q, which is not in --instance-type", name)
		}
	}

	return ordered, nil
}

// parsePriceOverrides reads the repeatable --price type=usd into a map, refusing a
// malformed entry rather than silently ignoring it.
func parsePriceOverrides(raw []string) (map[string]config.USDPerHour, error) {
	out := make(map[string]config.USDPerHour, len(raw))

	for _, entry := range raw {
		typeName, priceText, ok := strings.Cut(entry, "=")
		typeName = strings.TrimSpace(typeName)
		priceText = strings.TrimSpace(priceText)
		if !ok || typeName == "" || priceText == "" {
			return nil, fmt.Errorf("--price %q is not type=usd, e.g. c7i.xlarge=0.17", entry)
		}

		price, err := config.ParseUSDPerHour(priceText)
		if err != nil {
			return nil, fmt.Errorf("--price %q: %w", entry, err)
		}
		if price <= 0 {
			return nil, fmt.Errorf("--price %q must be more than zero", entry)
		}
		if _, dup := out[typeName]; dup {
			return nil, fmt.Errorf("--price for %q is given twice", typeName)
		}

		out[typeName] = price
	}

	return out, nil
}

// ec2OnlyFlagNames are the flags meaningful only to --provider ec2.
var ec2OnlyFlagNames = []string{
	"region", "subnet", "security-group", "untrusted-security-group",
	"instance-type", "price", "max-vcpu", "max-memory",
}

// refuseEC2OnlyFlags stops an ec2-only flag from being silently ignored on a
// docker or firecracker init, where it would read as configured and do nothing.
//
// PRESENCE, NOT VALUE. `set` is the flags the operator actually passed (from
// flag.FlagSet.Visit), so `--max-vcpu 0` and `--max-vcpu -1` are caught the same
// as a positive one — a value check would let a zero or a negative through as if
// it had been omitted.
// declaredCapacityFlags are the two ec2 flags that ALSO mean something on a
// host-run backend: what the machine being described has.
//
// billet measures a host-run ceiling from the machine `billet init` runs on,
// which is right when it can measure — and it is what forced an emission to run
// on the target, so the target needed a billet binary before there was an
// inventory to install one from. Declaring the numbers is the operator taking
// responsibility for them, exactly as ec2 already requires because there is no
// host under it to measure.
var declaredCapacityFlags = []string{"max-vcpu", "max-memory"}

// ec2PlacementFlagNames are the ec2 flags that describe a NETWORK billet
// launches into.
//
// SEPARATE FROM ec2OnlyFlagNames, because the codebuild backend shares half of
// that list and none of this one: it declares a region, a budget and a shape
// catalogue exactly as ec2 does, and it has no subnet and no security groups at
// all — the build runs on a machine in AWS's own account. So a subnet written
// into a codebuild invocation is somebody configuring a network for compute that
// will never be in it, and it is refused rather than discarded.
var ec2PlacementFlagNames = []string{
	"subnet", "security-group", "untrusted-security-group", "instance-type", "price",
}

// refuseEC2PlacementFlags rejects those on a backend that launches into no
// network of yours.
//
// SELF-GUARDING, for the reason refuseCodeBuildOnlyFlags is: a refusal whose
// correctness depends on an `if` at the call site is one a second call site gets
// wrong.
func refuseEC2PlacementFlags(kind config.ProviderKind, set map[string]bool) error {
	if kind == config.ProviderEC2 {
		return nil
	}

	var used []string

	for _, name := range ec2PlacementFlagNames {
		if set[name] {
			used = append(used, "--"+name)
		}
	}

	if len(used) > 0 {
		return fmt.Errorf("%s can only be used with --provider ec2, but this is a %s config: "+
			"its compute runs on machines in AWS's own account, so there is no subnet or "+
			"security group of yours for billet to launch into",
			strings.Join(used, ", "), kind)
	}

	return nil
}

// refuseEC2OnlyFlags rejects cloud placement flags on a backend that has no
// cloud. declaredCapacity exempts the two capacity flags, for an emission that
// describes a machine it is not running on.
func refuseEC2OnlyFlags(kind config.ProviderKind, set map[string]bool, declaredCapacity bool) error {
	exempt := make(map[string]bool, len(declaredCapacityFlags))
	if declaredCapacity {
		for _, name := range declaredCapacityFlags {
			exempt[name] = true
		}
	}

	var used []string
	for _, name := range ec2OnlyFlagNames {
		if set[name] && !exempt[name] {
			used = append(used, "--"+name)
		}
	}

	if len(used) > 0 {
		return fmt.Errorf("%s can only be used with --provider ec2, but this is a %s config",
			strings.Join(used, ", "), kind)
	}

	return nil
}
