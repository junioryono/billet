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
func PlanKernelReap(onDisk []string, needed map[string]bool, generations int) ([]string, error) {
	// NOTHING NEEDED WHILE GENERATIONS EXIST IS A FAILED READ, NOT A DIRECTORY OF
	// ORPHANS.
	//
	// The needed set is assembled from per-generation metadata, and a generation
	// published before billet recorded it -- or by the build script, which does not
	// -- contributes nothing. Acting on an empty set would delete the kernel every
	// running tier boots, and the first symptom would be every microVM failing to
	// start, with no obvious connection to a reap that reported success.
	if generations > 0 && len(needed) == 0 {
		return nil, fmt.Errorf("ceph: no kernel is recorded against any of the %d generations "+
			"of this image, so every kernel on disk looks orphaned. That is far more likely to "+
			"be metadata this could not read than a directory of orphans, and acting on it "+
			"would delete the kernel every running tier boots. Refusing", generations)
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
