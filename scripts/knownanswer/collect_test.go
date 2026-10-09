package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBillet answers `billet jobs show [--config C] --json <lease>` from
// $FAKE_RECORDS/<lease>.json, records every argv in $FAKE_LOG, and fails for a
// lease it has no file for, the way billet does for an unknown lease.
const fakeBillet = `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LOG"
[ "$1 $2" = "jobs show" ] || { echo "fake billet: unexpected $*" >&2; exit 90; }
for last; do :; done
if [ ! -f "$FAKE_RECORDS/$last.json" ]; then
	echo "alloc: lease not found: no job was recorded for lease $last" >&2
	exit 1
fi
cat "$FAKE_RECORDS/$last.json"
`

// COLLECT ASKS BILLET FOR EVERY LEASE THE INPUTS NAME, through the command line
// an operator would type, and writes only what is that lease's record.
func TestCollectAsksBilletForEveryLeaseTheInputsName(t *testing.T) {
	root := t.TempDir()
	bin, records, out := filepath.Join(root, "billet"), filepath.Join(root, "answers"), filepath.Join(root, "out")
	if err := forkSafeWriteFile(bin, []byte(fakeBillet), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "argv.log")
	t.Setenv("FAKE_RECORDS", records)
	t.Setenv("FAKE_LOG", log)

	exps, recs := fixtureRun(100)
	expDir := t.TempDir()
	writeRun(t, expDir, records, exps, recs)
	// THE CPU LEASE ANSWERS WITH ANOTHER LEASE'S RECORD, and the memory lease
	// has no record at all; the power log names one more lease than the
	// expectations do.
	if err := os.WriteFile(filepath.Join(records, "lease-100-cpu.json"),
		[]byte(`{"lease":"lease-100-idle","run_id":100}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(records, "lease-100-memory.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, "lease-x.json"), []byte(`{"lease":"lease-x","usage":null}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "power.csv")
	if err := os.WriteFile(logPath, []byte("epoch_s,uptime_s,rapl_delta_uj,instances\n1,1.00,,billet-lease-x\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"collect", "--out", out, "--expectations", expDir, "--power-log", logPath,
		"--billet", bin, "--config", "/etc/billet/billet.yaml"}, &stdout, &stderr)
	if code != exitFail {
		t.Errorf("exit %d with two leases uncollected, want %d:\n%s%s", code, exitFail, stdout.String(), stderr.String())
	}
	for _, line := range []string{
		"lease-100-cpu: billet answered something that is not this lease's record",
		"lease-100-memory: billet jobs show failed: exit status 1: alloc: lease not found",
		"lease-x: no usage report yet",
		"lease-100-idle: usage recorded",
		"collected 5 of 7",
	} {
		if !strings.Contains(stdout.String(), line) {
			t.Errorf("collect did not say %q:\n%s", line, stdout.String())
		}
	}
	for _, lease := range []string{"lease-100-baseline", "lease-100-idle", "lease-100-network", "lease-100-disk", "lease-x"} {
		if _, err := os.Stat(filepath.Join(out, lease+".json")); err != nil {
			t.Errorf("%s was not collected: %v", lease, err)
		}
	}
	for _, lease := range []string{"lease-100-cpu", "lease-100-memory"} {
		if _, err := os.Stat(filepath.Join(out, lease+".json")); !os.IsNotExist(err) {
			t.Errorf("%s was written although billet did not answer for it (stat: %v)", lease, err)
		}
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// THE FLAGS PRECEDE THE LEASE: billet's flag set stops at its first
	// positional argument.
	if !strings.Contains(string(argv), "jobs show --config /etc/billet/billet.yaml --json lease-100-idle\n") {
		t.Errorf("billet was asked:\n%s", argv)
	}
}
