package ceph

import (
	"fmt"
	"regexp"
	"sort"
)

// kernelFilePattern matches a kernel file billet installed.
//
// THE DIGEST IS PART OF THE NAME AND PART OF THE IDENTITY. Two builds can produce
// the same kernel version from different sources, so a version alone does not
// identify a file -- and reaping on version alone would remove a kernel some
// generation is verified against because a different build happened to share its
// number.
var kernelFilePattern = regexp.MustCompile(`^vmlinux-\d[0-9.]*\d-[0-9a-f]{12}$`)

// PlanKernelReap reports which kernel files no surviving generation needs.
//
// A KERNEL AND ITS GENERATION ARE A MATCHED PAIR: a guest booted with a different
// kernel fails in the middle of somebody's job. Every pull installs one, so
// without this a node accumulates a kernel a week forever and nothing says so --
// the disk simply fills, which on this project has already happened once for a
// different reason.
//
// TAKES WHAT IS NEEDED RATHER THAN COMPUTING IT, so the decision is a pure
// function of two lists and can be tested without a cluster. The caller reads the
// needed set out of the generations' metadata.
func PlanKernelReap(
	onDisk []string,
	needed map[string]bool,
	generations, unknown int,
) ([]string, error) {
	// ONE GENERATION WITH AN UNKNOWN KERNEL MAKES EVERY KERNEL UNSAFE TO DELETE.
	//
	// A generation that records no kernel still boots one, and that kernel is on
	// disk -- unnamed, and therefore indistinguishable from an orphan. Deleting it
	// breaks the generation that boots it, and the first symptom is every microVM
	// on that generation failing to start, with no obvious connection to a reap
	// that reported success.
	//
	// AN EARLIER VERSION REFUSED ONLY WHEN *NO* GENERATION NAMED A KERNEL, which is
	// the same mistake one step down: three generations naming kernels and a fourth
	// naming none would have reaped the fourth's. This was found by running a dry
	// run against a real cluster whose generations predated the metadata -- the
	// weaker rule happened to refuse there, for the wrong reason.
	//
	// Generations published by build-guest-image.sh always arrive here unknown; it
	// records no kernel. So a deployment that builds by hand does not reap kernels
	// automatically, which is the correct outcome: nothing can tell which of its
	// kernels are still needed.
	if unknown > 0 {
		return nil, fmt.Errorf("ceph: %d of %d generations record no kernel, so each of them "+
			"boots something on disk that nothing here can name. Any of those files is "+
			"indistinguishable from an orphan, and deleting one breaks the generation that "+
			"boots it. Refusing to reap kernels until every generation names one -- a fresh "+
			"`billet images pull` records it, and reaping the unnamed generations also clears "+
			"this", unknown, generations)
	}

	var reapable []string

	for _, name := range onDisk {
		// A FILE THIS DID NOT WRITE IS LEFT ALONE. The kernel directory is a real path
		// an operator can put things in, and a reaper that removes whatever it does
		// not recognise is one nobody should be willing to point at a directory.
		if !kernelFilePattern.MatchString(name) {
			continue
		}

		if needed[name] {
			continue
		}

		reapable = append(reapable, name)
	}

	// SORTED SO THE PLAN IS THE SAME EVERY TIME. A caller prints this for an operator
	// to approve, and a list whose order changes between a dry run and the real one
	// is one nobody can compare.
	sort.Strings(reapable)

	return reapable, nil
}
