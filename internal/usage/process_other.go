//go:build !darwin

package usage

import "errors"

// errNoProcessCounters is what a platform without per-process accounting
// answers.
var errNoProcessCounters = errors.New("usage: this platform keeps no per-process accounting billet reads")

// readProcessCounters has nothing to read: a Linux job is measured by its own
// cgroup, which a process's lifetime totals would only duplicate.
func readProcessCounters(int) (ProcessCounters, error) {
	return ProcessCounters{}, errNoProcessCounters
}

// ProcessPath has nothing to read on a platform billet measures by cgroup.
func ProcessPath(int) (string, error) { return "", errNoProcessCounters }
