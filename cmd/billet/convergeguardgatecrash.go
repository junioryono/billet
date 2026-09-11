//go:build billetgatecrash

package main

import (
	"os"
	"strconv"
	"strings"
)

// THE GATE'S CRASH SEAM, compiled only under the `billetgatecrash` tag the
// collection's gate builds with, and into no shipped binary: the structural
// test in convergeguardprepare_test.go holds this file to its constraint and
// the rest of the package to never reading the variable.
//
// BILLET_GUARD_CRASH_AT names an operation the guard hook sees, `<kind>
// <path suffix>[:n]`, and the process exits 137 where it stands on the n-th
// (default first) occurrence, before the operation runs, so a gate case can
// leave the exact remainder an interruption leaves at that boundary.
func init() {
	spec := os.Getenv("BILLET_GUARD_CRASH_AT")
	if spec == "" {
		return
	}

	want, count := spec, 1

	if i := strings.LastIndex(spec, ":"); i > 0 {
		if n, err := strconv.Atoi(spec[i+1:]); err == nil && n > 0 {
			want, count = spec[:i], n
		}
	}

	seen := 0
	previous := guardHook

	guardHook = func(op guardOp) error {
		if previous != nil {
			if err := previous(op); err != nil {
				return err
			}
		}

		if strings.HasPrefix(want, op.Kind+" ") && strings.HasSuffix(op.Path, strings.TrimPrefix(want, op.Kind+" ")) {
			seen++

			if seen == count {
				os.Exit(137)
			}
		}

		return nil
	}
}
