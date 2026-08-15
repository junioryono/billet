package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/store/ceph"
)

// reapKernelDir removes kernel files in dir that no surviving generation needs.
//
// THE DECISION IS NOT MADE HERE. PlanKernelReap decides, from two lists, and is
// tested without a filesystem; this does the reading and the deleting. Keeping
// them apart is what makes the rule -- including its refusal to reap everything --
// testable in isolation rather than only through a directory.
func reapKernelDir(
	dir string,
	needed map[string]bool,
	generations, unknown int,
	configured string,
	dryRun bool,
) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A NODE THAT HAS NEVER PULLED HAS NO KERNEL DIRECTORY, and a reap that
		// treated that as a failure would fail on every fresh deployment.
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("billet images reap: cannot read %s: %w", dir, err)
	}

	var onDisk []string

	for _, entry := range entries {
		// DIRECTORIES ARE NOT KERNELS. The name filter would reject one anyway, but
		// saying so here means a future filter change cannot start recursing into
		// somebody's backup directory.
		if entry.IsDir() {
			continue
		}

		onDisk = append(onDisk, entry.Name())
	}

	reapable, err := ceph.PlanKernelReap(onDisk, needed, generations, unknown, configured)
	if err != nil {
		return nil, err
	}

	if dryRun {
		return reapable, nil
	}

	var removed []string

	for _, name := range reapable {
		// THE NAME IS PROVED TO BE A BASE NAME BEFORE IT IS JOINED TO A PATH THIS
		// DELETES.
		//
		// It came from os.ReadDir and therefore already is one, and PlanKernelReap
		// only returns names matching a strict pattern -- so this is unreachable
		// today. It is here because the thing on the other side of it is os.Remove
		// running as root, and "unreachable today" is a property of two functions
		// that could each change without the other noticing.
		if name != filepath.Base(name) || name == "." || name == ".." {
			return removed, fmt.Errorf("billet images reap: refusing to remove %q, which is "+
				"not a plain file name", name)
		}

		// The guard above establishes that name is a plain base name, which is what
		// makes this join safe.
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			// REPORTS WHAT WENT BEFORE IT FAILS, so a partial reap is legible: the
			// caller has already been told which files are gone.
			return removed, fmt.Errorf("billet images reap: could not remove %s: %w", name, err)
		}

		removed = append(removed, name)
	}

	return removed, nil
}
