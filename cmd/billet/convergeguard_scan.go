package main

import (
	"fmt"
	"sort"
	"strings"
)

// THE PROCESS SCAN IS AN EXTRA REFUSAL; THE OPERATOR'S ASSERTION CARRIES THE
// WEIGHT. `recover --holder` and a takeover are run under `--old-driver-stopped`,
// the assertion that the driver which held the guard is stopped or cannot
// dispatch further work and that its remote work has finished. The scan cannot
// prove that: an unmarked driver, a process in another pid namespace, or work
// dispatched after the scan are all invisible to it. What it can do is refuse
// when the evidence in front of it contradicts the assertion: a process on this
// host carrying Ansible's markers (`AnsiballZ_` in a module's argv, `ansible-tmp-`
// in its working files), including the scanning process's own ancestry, which
// is the shape of an operator running the recovery from the very driver they
// assert is stopped. An unreadable table, an unreadable entry, an ancestor that
// cannot be followed each refuse as could-not-tell; a process that vanished
// between the enumeration and its read is skipped, because a process that is
// gone is not a driver.

// The markers a driver leaves in a process's command line.
var driverMarkers = []string{"AnsiballZ_", "ansible-tmp-"}

// processEntry is one process as the table reads it: its parent and its
// command line, or the error that stopped either from being read.
type processEntry struct {
	PID     int
	PPID    int
	Cmdline string
	// Err is a read that failed for a reason other than the process vanishing;
	// an entry that vanished is not in the table at all.
	Err error
}

// processTable is the host's processes at one enumeration, and which one is
// the scanner.
type processTable struct {
	Self    int
	Entries map[int]processEntry
}

// guardProcesses reads the process table; a seam so the scan's decisions can be
// tested against a fixture table on every platform.
var guardProcesses = readProcessTable

// scanForDrivers refuses when the process table shows a driver, or cannot be
// read well enough to say it does not.
func scanForDrivers() error {
	table, err := guardProcesses()
	if err != nil {
		return fmt.Errorf("%w: the process table could not be read (%w); a driver may still be running", errScanRefused, err)
	}

	// THE SCANNER'S OWN ANCESTRY FIRST, and with its own diagnostic: a recovery
	// run from inside an Ansible task is the driver recovering from itself.
	seen := map[int]bool{}
	pid := table.Self

	for pid > 1 {
		if seen[pid] {
			return fmt.Errorf("%w: the scanner's ancestry loops at pid %d", errScanRefused, pid)
		}

		seen[pid] = true

		entry, ok := table.Entries[pid]

		switch {
		case !ok:
			return fmt.Errorf("%w: the scanner's ancestor pid %d vanished during the scan, so the ancestry "+
				"cannot be followed", errScanRefused, pid)
		case entry.Err != nil:
			return fmt.Errorf("%w: the scanner's ancestor pid %d could not be read: %w", errScanRefused, pid, entry.Err)
		case marked(entry.Cmdline) && pid != table.Self:
			return fmt.Errorf("%w: this process runs under an Ansible driver (pid %d: %s); run this over a "+
				"direct SSH shell, not from the driver whose work it asserts is finished",
				errScanRefused, pid, summarizeCmdline(entry.Cmdline))
		}

		pid = entry.PPID
	}

	pids := make([]int, 0, len(table.Entries))
	for pid := range table.Entries {
		pids = append(pids, pid)
	}

	sort.Ints(pids)

	for _, pid := range pids {
		if seen[pid] {
			continue
		}

		entry := table.Entries[pid]

		if entry.Err != nil {
			return fmt.Errorf("%w: pid %d could not be read (%w), so the scan cannot say no driver is running",
				errScanRefused, pid, entry.Err)
		}

		if marked(entry.Cmdline) {
			return fmt.Errorf("%w: pid %d carries an Ansible driver's marker (%s); the old driver is not "+
				"stopped, or another converge is running", errScanRefused, pid, summarizeCmdline(entry.Cmdline))
		}
	}

	return nil
}

func marked(cmdline string) bool {
	for _, m := range driverMarkers {
		if strings.Contains(cmdline, m) {
			return true
		}
	}

	return false
}

// summarizeCmdline is the first hundred bytes of a command line, for a
// diagnostic that names the process without quoting a whole module.
func summarizeCmdline(cmdline string) string {
	const limit = 100

	if len(cmdline) <= limit {
		return cmdline
	}

	return cmdline[:limit] + "..."
}
