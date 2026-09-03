package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
)

// A DOWNGRADE IS REFUSED UNLESS ASKED FOR BY NAME, AND ONLY A PROVED ONE.
//
// The ledger's release watermark is the backstop; this is the answer before a
// drain. A development build cannot be ordered against a release and is neither
// refused nor waved through on that basis — "could not tell" must not become
// "no" in either direction.
func TestADowngradeIsRefusedUnlessNamed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target, running string
		allowed         bool
		refused         bool
	}{
		{"v0.4.0", "v0.5.0", false, true},
		{"v0.4.0", "v0.5.0", true, false},
		{"v0.5.0", "v0.5.0", false, false},
		{"v0.6.0", "v0.5.0", false, false},
		{"v0.4.0", "(devel)", false, false},
		{"(devel)", "v0.5.0", false, false},
	}

	for _, c := range cases {
		err := checkDowngrade(c.target, c.running, c.allowed)

		if got := errors.Is(err, ErrDowngrade); got != c.refused {
			t.Errorf("checkDowngrade(%q, %q, allowed=%v) refused=%v, want %v (err %v)",
				c.target, c.running, c.allowed, got, c.refused, err)
		}
	}
}

// THE PERMISSION TRAVELS ON THE JOURNAL, AND THE HOST READS IT FROM THERE.
//
// Migrate lowers the watermark only for a transaction that carries the flag, and
// a resumed transaction is built from the journal alone — so a host built from a
// journal that says nothing must lower nothing, and one built from a journal that
// carries the flag must lower to exactly the release it is installing.
func TestTheSystemdHostLowersTheWatermarkOnlyWhenTheJournalSaysSo(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	plain := newSystemdHost(cfg, "", "/staged/billet", &hostupgrade.Journal{ToVersion: "v0.4.0"})
	if plain.downgradeTo != "" {
		t.Errorf("a journal without the flag set downgradeTo = %q", plain.downgradeTo)
	}

	if none := newSystemdHost(cfg, "", "/staged/billet", nil); none.downgradeTo != "" {
		t.Errorf("no journal set downgradeTo = %q", none.downgradeTo)
	}

	asked := newSystemdHost(cfg, "", "/staged/billet",
		&hostupgrade.Journal{ToVersion: "v0.4.0", AllowDowngrade: true})
	if asked.downgradeTo != "v0.4.0" {
		t.Errorf("a journal with the flag set downgradeTo = %q, want v0.4.0", asked.downgradeTo)
	}
}

// THE DOWNGRADE CHECK RUNS BEFORE THE CLAIM. After stageClaim the machine has
// been claimed and a journal written; refusing there leaves a recovery directory
// per refused attempt on a host a rollout retries every few minutes, and the
// whole point of the check is to refuse without touching anything.
func TestTheDowngradeCheckPrecedesTheClaim(t *testing.T) {
	fn := findFunc(t, "actOnResolved")

	check, claim := -1, -1

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch calleeName(call) {
		case "checkDowngrade":
			if check < 0 {
				check = int(call.Pos())
			}
		case "stageClaim":
			if claim < 0 {
				claim = int(call.Pos())
			}
		}

		return true
	})

	if check < 0 || claim < 0 {
		t.Fatalf("actOnResolved calls checkDowngrade at %d and stageClaim at %d; both must be "+
			"present", check, claim)
	}

	if check > claim {
		t.Fatal("actOnResolved claims the machine before it checks for a downgrade")
	}
}

// AN EXTERNAL LEDGER IS KNOWN TO THE HOST FROM THE CONFIG, because the migrate
// step that lowers the mark on a local ledger is skipped there and the probe step
// has to do it instead, through the operator handle rather than the read-only
// probe the maintenance open returns on PostgreSQL.
func TestTheHostKnowsWhetherItsLedgerIsExternal(t *testing.T) {
	t.Parallel()

	local := newSystemdHost(&config.Config{Server: &config.ServerConfig{}}, "", "/staged/billet", nil)
	if local.external {
		t.Error("a SQLite control plane read as an external ledger")
	}

	postgres := newSystemdHost(&config.Config{Server: &config.ServerConfig{
		State: &config.StateConfig{Backend: config.StatePostgres},
	}}, "", "/staged/billet", nil)
	if !postgres.external {
		t.Error("a PostgreSQL control plane read as a local ledger")
	}
}

// THE MARK IS LOWERED BEFORE THE CANDIDATE IS ASKED ANYTHING, at the migrate step
// on a local ledger and at the probe step on an external one, through the handle
// each ledger allows. Asserted by order against a candidate that records when it
// was run, because the failure is the lowering happening after the probe that
// the higher mark refuses, or not at all.
func TestTheMarkIsLoweredBeforeTheCandidateIsProbed(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")

	prevLower, prevBinary := lowerReleaseWatermark, installedBinary
	lowerReleaseWatermark = func(_ context.Context, _ *config.Config, external bool,
		release string,
	) error {
		f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}

		defer func() { _ = f.Close() }()

		_, err = fmt.Fprintf(f, "lower external=%v to %s\n", external, release)

		return err
	}
	installedBinary = filepath.Join(t.TempDir(), "billet")
	t.Cleanup(func() { lowerReleaseWatermark, installedBinary = prevLower, prevBinary })

	if err := os.WriteFile(installedBinary, []byte("#!/bin/sh\nprintf 'candidate %s\\n' \"$1\" >> "+
		record+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	journal := &hostupgrade.Journal{ToVersion: "v0.4.0", AllowDowngrade: true}

	external := newSystemdHost(&config.Config{Server: &config.ServerConfig{
		State: &config.StateConfig{Backend: config.StatePostgres},
	}}, "", "/staged/billet", journal)
	if err := external.ProbeReady(t.Context()); err != nil {
		t.Fatalf("ProbeReady on an external ledger: %v", err)
	}

	local := newSystemdHost(&config.Config{Server: &config.ServerConfig{}}, "", "/staged/billet",
		journal)
	if err := local.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate on a local ledger: %v", err)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	want := "lower external=true to v0.4.0\ncandidate server\n" +
		"lower external=false to v0.4.0\ncandidate check\n"
	if string(body) != want {
		t.Errorf("the steps ran as:\n%s\nwant:\n%s", body, want)
	}
}
