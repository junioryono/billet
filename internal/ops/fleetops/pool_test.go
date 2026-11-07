package fleetops

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

func poolEnv() (cli.Env, *bytes.Buffer) {
	var out bytes.Buffer

	return cli.Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader(""), Getenv: os.Getenv}, &out
}

// `billet pool` LISTS EVERY ORDER, says what an uncertain one asks of a person, and
// `billet pool abandon` closes only an uncertain order, with the reason recorded.
func TestThePoolCommandListsOrdersAndClosesOnlyAnUncertainOne(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil || !strings.Contains(out.String(), "placed no order") {
		t.Fatalf("an empty pool: %v\n%s", err, out)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"p-placed", "p-lost"} {
		if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
			ID: id, Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
			Location: "us-west-2a", Arch: config.ArchX64,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := a.PoolOrderPlaced(ctx, "p-placed", "i-0abc"); err != nil {
		t.Fatalf("placed: %v", err)
	}
	if err := a.PoolOrderUncertain(ctx, "p-lost", "the answer was\nlost"); err != nil {
		t.Fatalf("uncertain: %v", err)
	}
	closeDB()

	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = f
		}
	}
	if r := rows["p-placed"]; len(r) < 9 || r[1] != "x64" || r[2] != "aws-metal" ||
		r[3] != "aws:us-west-2:subnet-0abc:us-west-2a" || r[6] != "placed" || r[7] != "i-0abc" {
		t.Errorf("the p-placed row = %q", r)
	}
	if r := rows["p-lost"]; len(r) < 9 || r[6] != "uncertain" || r[7] != "-" {
		t.Errorf("the p-lost row = %q", r)
	}
	for _, want := range []string{"the answer was lost", "1 uncertain", "billet pool abandon"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing has no %q:\n%s", want, out)
		}
	}

	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "--reason", "no such instance", "p-placed"}); err == nil ||
		!strings.Contains(err.Error(), "not uncertain") {
		t.Fatalf("abandoning a placed order: %v\n%s", err, out)
	}
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "p-lost"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("an order abandoned without a reason answered %v", err)
	}
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "--reason", "no such instance", "p-lost"}); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil ||
		!strings.Contains(out.String(), "abandoned") || !strings.Contains(out.String(), "no such instance") ||
		strings.Contains(out.String(), "uncertain:") {
		t.Fatalf("after abandoning: %v\n%s", err, out)
	}
}

// A HELD JOIN IS LISTED WITH ITS REASON AND WHAT A PERSON DOES ABOUT IT, on one line
// whatever the reason held; an order held for no one is not listed as held.
func TestThePoolCommandSaysWhichJoinsAreHeld(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()
	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"p-held", "p-fine"} {
		if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
			ID: id, Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
			Location: "us-west-2a", Arch: config.ArchX64,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := a.PoolOrderPlaced(ctx, id, "i-"+id); err != nil {
			t.Fatalf("placed: %v", err)
		}
	}
	if err := a.HoldPoolJoin(ctx, "p-held", "the console names\nanother key", heldAt(t, a, "p-held")); err != nil {
		t.Fatalf("hold: %v", err)
	}
	closeDB()

	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"held p-held: the console names another key", "billet nodes approve"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "held p-fine") {
		t.Errorf("an order held for no one is listed as held:\n%s", out)
	}
}

func heldAt(t *testing.T, a *alloc.Allocator, order string) time.Time {
	t.Helper()
	o, err := a.ReadPoolOrder(t.Context(), order)
	if err != nil {
		t.Fatal(err)
	}

	return o.Updated
}
