package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// collectLeases is every lease the expectations and the power log name, each
// once, in the order first named.
func collectLeases(exps []expectation, rows []powerRow) []string {
	var out []string
	add := func(lease string) {
		if !slices.Contains(out, lease) {
			out = append(out, lease)
		}
	}
	for i := range exps {
		add(exps[i].Lease)
	}
	for _, j := range jobsSeen(rows) {
		add(j.lease)
	}

	return out
}

// collect asks billet for each lease's record and writes it to
// <out>/<lease>.json. It runs where the ledger is: `billet jobs show` opens the
// control plane's, so on a two-host deployment this runs on the control plane.
// A lease billet cannot answer for is reported and counted, and the others are
// still collected.
func collect(ctx context.Context, w io.Writer, billet, config, out string, leases []string) (int, error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, err
	}
	failed := 0
	for _, lease := range leases {
		if !leasePattern.MatchString(lease) || lease == "." || lease == ".." {
			fmt.Fprintf(w, "%s: not a lease id\n", lease)
			failed++

			continue
		}
		args := []string{"jobs", "show"}
		if config != "" {
			args = append(args, "--config", config)
		}
		args = append(args, "--json", lease)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, billet, args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(w, "%s: billet jobs show failed: %v: %s\n", lease, err, strings.TrimSpace(stderr.String()))
			failed++

			continue
		}
		// WHAT IS WRITTEN IS WHAT THE CHECKER CAN READ, for this lease: a record
		// for another lease, or no JSON at all, never reaches the directory.
		var r record
		if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || r.Lease != lease {
			fmt.Fprintf(w, "%s: billet answered something that is not this lease's record\n", lease)
			failed++

			continue
		}
		path := filepath.Join(out, lease+".json")
		if err := os.WriteFile(path+".tmp", stdout.Bytes(), 0o600); err != nil {
			return failed, err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return failed, err
		}
		state := "no usage report yet"
		if r.Usage != nil {
			state = "usage recorded"
		}
		fmt.Fprintf(w, "%s: %s\n", lease, state)
	}

	return failed, nil
}
