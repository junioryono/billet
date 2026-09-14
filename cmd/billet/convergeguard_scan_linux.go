package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The table is read under the inspector's procRoot seam (releaseinspect.go),
// so one fixture tree stands in for /proc for both.

// procReadFile reads one file of the table; a seam so a read can be made to
// fail with an error a mode cannot produce for root.
var procReadFile = os.ReadFile

// readProcessTable reads every numeric entry of /proc.
//
// `cmdline` IS NUL-TERMINATED AND AN EMPTY ARGUMENT IS ONE EMPTY ELEMENT
// (measured 2026-09-09: `exe\0AnsiballZ_marker\0\0arg with space\0`), so exactly
// one terminator is removed and nothing is trimmed. A pid whose `status` and
// `cmdline` BOTH answer ENOENT vanished between the enumeration and the read and
// is left out; any other outcome, one of the two missing included, is an entry
// with an error, because a process half-read is not a process known not to be a
// driver.
func readProcessTable() (processTable, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return processTable{}, err
	}

	table := processTable{Self: os.Getpid(), Entries: map[int]processEntry{}}

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}

		entry, vanished := readProcessEntry(pid)
		if vanished {
			continue
		}

		table.Entries[pid] = entry
	}

	return table, nil
}

func readProcessEntry(pid int) (processEntry, bool) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	entry := processEntry{PID: pid}

	status, statusErr := procReadFile(filepath.Join(dir, "status"))
	cmdline, cmdErr := procReadFile(filepath.Join(dir, "cmdline"))

	if errors.Is(statusErr, fs.ErrNotExist) && errors.Is(cmdErr, fs.ErrNotExist) {
		return entry, true
	}

	switch {
	case statusErr != nil:
		entry.Err = fmt.Errorf("read status: %w", statusErr)

		return entry, false
	case cmdErr != nil:
		entry.Err = fmt.Errorf("read cmdline: %w", cmdErr)

		return entry, false
	}

	ppid, err := parsePPid(status)
	if err != nil {
		entry.Err = err

		return entry, false
	}

	entry.PPID = ppid
	entry.Cmdline = decodeCmdline(cmdline)

	return entry, false
}

func parsePPid(status []byte) (int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}

		ppid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		if err != nil {
			return 0, fmt.Errorf("status has a PPid line that is not a number: %q", line)
		}

		return ppid, nil
	}

	return 0, errors.New("status has no PPid line")
}

// decodeCmdline is the command line's arguments joined by one space, every
// argument kept, empty ones included.
func decodeCmdline(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	raw = bytes.TrimSuffix(raw, []byte{0})

	return string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))
}
