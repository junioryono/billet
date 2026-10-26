package config

import (
	"fmt"
	"slices"
	"strings"
)

// Arch is the processor architecture a guest image is built for and a host or a
// shape runs, in billet's spelling: the one tiers, images and `billet ami build
// --arch` already use (ADR-022).
//
// NOT GOARCH AND NOT AWS'S SPELLING. Go says amd64 and EC2 says x86_64, and a
// tier written `arch: amd64` would then compare unequal to a node that reported
// what it is; HostArch and ArchFromEC2 are the one translation from each.
type Arch string

const (
	ArchX64   Arch = "x64"
	ArchARM64 Arch = "arm64"
)

var allArchs = []Arch{ArchX64, ArchARM64}

// Valid reports whether this is an architecture billet builds images for.
func (a Arch) Valid() bool { return slices.Contains(allArchs, a) }

// HostArch is the architecture of a host whose Go runtime reports goarch, and
// false for one billet builds no images for.
func HostArch(goarch string) (Arch, bool) {
	switch goarch {
	case "amd64":
		return ArchX64, true
	case "arm64":
		return ArchARM64, true
	default:
		return "", false
	}
}

// ArchFromEC2 is the architecture EC2 names x86_64 or arm64 (an AMI's
// Architecture, an instance type's supported architectures), and false for
// every other name (i386, the Mac forms), which billet launches no Linux image
// for.
func ArchFromEC2(name string) (Arch, bool) {
	switch name {
	case "x86_64":
		return ArchX64, true
	case "arm64":
		return ArchARM64, true
	default:
		return "", false
	}
}

// SortedArchs is a set of architectures in one order, each once, so a node's
// report and the ledger's record of it compare as text.
func SortedArchs(archs []Arch) []Arch {
	out := slices.Clone(archs)
	slices.Sort(out)

	return slices.Compact(out)
}

// JoinArchs is SortedArchs as the comma list the ledger stores.
func JoinArchs(archs []Arch) string {
	parts := make([]string, 0, len(archs))
	for _, a := range SortedArchs(archs) {
		parts = append(parts, string(a))
	}

	return strings.Join(parts, ",")
}

// SplitArchs reads the comma list JoinArchs wrote, refusing a name billet does
// not know rather than dropping it: a ledger value nothing wrote is not an
// architecture placement may match.
func SplitArchs(s string) ([]Arch, error) {
	if s == "" {
		return nil, nil
	}

	var out []Arch

	for part := range strings.SplitSeq(s, ",") {
		a := Arch(part)
		if !a.Valid() {
			return nil, fmt.Errorf("architecture %q is not one of %v", part, allArchs)
		}

		out = append(out, a)
	}

	return SortedArchs(out), nil
}

// TierArchErrors refuses a tier catalogue that names an architecture on some
// tiers and not on others, and a tier naming one billet does not know.
//
// ALL OR NONE, so the first `arch` is an explicit migration of every tier rather
// than a default an existing tier silently takes (ADR-022): a tier naming no
// architecture places on a node of any, and in a file where some tiers name one
// that tier would run arm64 jobs on x64 hosts or the reverse without anybody
// having said so.
func TierArchErrors(tiers []Tier) []error {
	var errs []error

	named := 0

	for i := range tiers {
		t := &tiers[i]
		where := fmt.Sprintf("tier %q", t.Label)

		switch {
		case t.Arch == "":
		case !t.Arch.Valid():
			errs = append(errs, fmt.Errorf("%s: arch %q is not one of %v", where, t.Arch, allArchs))
			named++
		default:
			named++

			// APPLE SILICON ONLY: Tart runs on it and CodeBuild's macOS is MAC_ARM,
			// so an x64 macOS tier names a machine billet cannot reach.
			if t.GuestOS == GuestMacOS && t.Arch != ArchARM64 {
				errs = append(errs, fmt.Errorf("%s: arch %q with guest_os macos; every macOS "+
					"guest billet launches runs on Apple silicon, so a macOS tier is arch arm64",
					where, t.Arch))
			}
		}
	}

	if named > 0 && named < len(tiers) {
		var without []string

		for i := range tiers {
			if tiers[i].Arch == "" {
				without = append(without, tiers[i].Label)
			}
		}

		errs = append(errs, fmt.Errorf("arch is named on %d of %d tiers; once one tier names an "+
			"architecture every tier must, so a tier that names none is not placed on a host of "+
			"either without anybody having said so (name one on %s)",
			named, len(tiers), strings.Join(without, ", ")))
	}

	return errs
}

// CatalogueNamesArch reports whether any tier names an architecture, which is
// what makes a file one that a billet older than architectures cannot read.
func CatalogueNamesArch(tiers []Tier) bool {
	return slices.ContainsFunc(tiers, func(t Tier) bool { return t.Arch != "" })
}

// CheckNodeArchitectures refuses a node's report of the architectures its
// provider launches that placement could be misled by.
//
// Every name must be one billet builds for, since placement matches the text. An
// EC2 node's list is its shapes' declared architectures and nothing else, because
// placement buys a tier only a shape of its architecture and a node-level claim
// beyond the shapes would place a tier on a node with nothing to buy it. Every
// other backend runs one: the host's, or a CodeBuild environment's. EMPTY IS
// ALWAYS ACCEPTED, as the report of a node that says nothing, which is placed
// only for tiers that name none.
func CheckNodeArchitectures(kind ProviderKind, archs []Arch, shapes []RemoteShape) error {
	if len(archs) == 0 {
		return nil
	}

	for _, a := range archs {
		if !a.Valid() {
			return fmt.Errorf("architecture %q is not one billet builds images for (%v); "+
				"placement matches it as written", a, allArchs)
		}
	}

	switch {
	case kind == ProviderEC2:
		var declared []Arch

		for _, s := range shapes {
			if s.Arch != "" {
				declared = append(declared, s.Arch)
			}
		}

		if JoinArchs(archs) != JoinArchs(declared) {
			return fmt.Errorf("architectures %q and shapes declaring %q; an EC2 node launches "+
				"what its shapes run, so the two are one list", JoinArchs(archs), JoinArchs(declared))
		}

	case len(SortedArchs(archs)) != 1:
		return fmt.Errorf("a %s node reported architectures %q; it launches one, its host's or "+
			"its environment's", kind, JoinArchs(archs))
	}

	return nil
}

// Arch is the architecture builds in this environment run on: arm64 for the ARM
// environments and AWS's Apple silicon, x64 for the rest. Derived for the same
// reason GuestOS is: an environment type and a separately written architecture
// would be two authorities for one fact.
func (e CodeBuildEnvironment) Arch() Arch {
	switch e {
	case CodeBuildARMContainer, CodeBuildARMEC2, CodeBuildMacARM:
		return ArchARM64
	default:
		return ArchX64
	}
}
