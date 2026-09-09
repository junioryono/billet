package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/version"
)

// HOST-UPGRADE UNDER A GUARD: every entry classifies the claim immediately after
// the lock and before anything else, the timer holds one lock from its
// classification through its settlement, and the refusal reaches the node's
// acknowledgement through the production command.

// guardedFixture is a guard fixture beside a control-plane ledger holding a
// rollout, with the resolver and the platform check as seams that fail the test
// if reached.
type guardedFixture struct {
	*guardFixture
	cfg     *config.Config
	cfgPath string
	reached []string
}

func newGuardedFixture(t *testing.T) *guardedFixture {
	t.Helper()

	f := &guardedFixture{guardFixture: newGuardFixture(t)}
	stateDir := t.TempDir()
	f.cfgPath = writeCAConfig(t, stateDir)

	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	f.cfg = cfg

	// A Linux host unless a fixture says otherwise: the platform check sits
	// between the lock and the resolver only on darwin.
	savedOS := hostOS
	hostOS = "linux"

	savedResolve, savedCheck, savedBarrier := resolveRelease, checkBinaryDir, timerBarrier
	resolveRelease = func(context.Context, *releasesource.Client, releasesource.Policy, string, string,
	) (*releasesource.Manifest, string, error) {
		f.reached = append(f.reached, "resolver")

		return nil, "", errors.New("the resolver was reached, staged stop")
	}
	checkBinaryDir = func(hostPaths) error {
		f.reached = append(f.reached, "platform check")

		return errors.New("the platform check was reached, staged stop")
	}
	timerBarrier = nil

	t.Cleanup(func() {
		resolveRelease, checkBinaryDir, timerBarrier = savedResolve, savedCheck, savedBarrier
		hostOS = savedOS
	})

	return f
}

// H1: `startHostUpgrade` refuses a guarded host after the lock and before the
// platform check and the resolver, with the guard's words; an unguarded host
// reaches the resolver (the control); a held lock refuses before any
// classification; and the production command carries the refusal to the ack
// socket.
func TestAnUpgradeRefusesAGuardedHostBeforeAnythingElse(t *testing.T) {
	f := newGuardedFixture(t)
	mustHold(t, "ci-1")

	err := startHostUpgrade(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""))
	if err == nil || !strings.Contains(err.Error(), "guarded by ci-1 since 2026-09-09T12:00:00Z") {
		t.Fatalf("a guarded host: err = %v", err)
	}

	if len(f.reached) != 0 {
		t.Errorf("a guarded host reached %v", f.reached)
	}

	if !lockFreeInProcess(t) {
		t.Error("the refused transaction kept the lock")
	}

	// THE ACK, through the production command and the real socket.
	ack, answer := ackReader(t)

	err = cmdHostUpgrade(t.Context(), []string{"--config", f.cfgPath, "--version", "v0.9.4", "--ack-path", ack.path})
	if err == nil || !strings.Contains(err.Error(), "guarded by ci-1") {
		t.Fatalf("the command on a guarded host: err = %v", err)
	}

	if got := answer(); !strings.HasPrefix(got, node.AckRefused+"guarded by ci-1 since") {
		t.Errorf("the ack carried %q", got)
	}

	// A HELD LOCK REFUSES FIRST, whatever the claim says.
	end := startLockHolder(t)

	err = startHostUpgrade(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""))
	if !errors.Is(err, ErrUpgradeInProgress) {
		t.Errorf("under a held lock: err = %v, want ErrUpgradeInProgress", err)
	}

	end()

	// THE CONTROL: released, the same call reaches the resolver.
	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	err = startHostUpgrade(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""))
	if err == nil || !strings.Contains(err.Error(), "the resolver was reached") {
		t.Fatalf("an unguarded host did not reach the resolver: err = %v", err)
	}

	// An unpublished guard refuses naming its recovery.
	if err := os.Mkdir(f.active(), 0o700); err != nil {
		t.Fatal(err)
	}

	err = startHostUpgrade(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""))
	if !errors.Is(err, errGuardUnpublished) {
		t.Errorf("an unpublished guard: err = %v", err)
	}
}

// H5: THE SAME ORDER ON DARWIN, where the platform check sits between the lock
// and the resolver: a guarded host reaches neither.
func TestAnUpgradeOnDarwinClassifiesBeforeThePlatformCheck(t *testing.T) {
	f := newGuardedFixture(t)

	saved := hostOS
	hostOS = "darwin"

	t.Cleanup(func() { hostOS = saved })

	mustHold(t, "ci-1")

	tx := mustLock(t)

	err := startHostUpgradeHolding(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""),
		releasesource.Policy{}, tx)
	if err == nil || !strings.Contains(err.Error(), "guarded by ci-1") {
		t.Fatalf("a guarded darwin host: err = %v", err)
	}

	if len(f.reached) != 0 {
		t.Errorf("a guarded darwin host reached %v", f.reached)
	}

	tx.release()

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	tx = mustLock(t)

	err = startHostUpgradeHolding(t.Context(), f.cfg, f.cfgPath, hostUpgradeTarget{pin: "v0.9.4"}, newUpgradeAck(""),
		releasesource.Policy{}, tx)
	if err == nil || !strings.Contains(err.Error(), "platform check was reached") {
		t.Errorf("an unguarded darwin host did not reach the platform check first: err = %v", err)
	}

	tx.release()
}

func mustLock(t *testing.T) *txLock {
	t.Helper()

	tx, err := takeTxLock()
	if err != nil {
		t.Fatalf("takeTxLock: %v", err)
	}

	t.Cleanup(tx.release)

	return tx
}

// H2: `--resume` takes the lock, then classifies, before any journal: a held
// lock refuses first, a guard refuses naming it, an unpublished guard names its
// recovery, and no host action runs.
func TestAResumeClassifiesAfterTheLockAndBeforeAnyJournal(t *testing.T) {
	f := newGuardedFixture(t)

	end := startLockHolder(t)

	if err := resumeHostUpgrade(t.Context(), f.cfg); !errors.Is(err, ErrUpgradeInProgress) {
		t.Errorf("resume under a held lock: err = %v", err)
	}

	end()

	mustHold(t, "ci-1")

	if err := resumeHostUpgrade(t.Context(), f.cfg); !errors.Is(err, errGuardHeld) {
		t.Errorf("resume over a guard: err = %v", err)
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(f.active(), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := resumeHostUpgrade(t.Context(), f.cfg); !errors.Is(err, errGuardUnpublished) {
		t.Errorf("resume over an unpublished guard: err = %v", err)
	}

	if err := os.Remove(f.active()); err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() {
		if err := resumeHostUpgrade(t.Context(), f.cfg); err != nil {
			t.Errorf("resume with no claim: %v", err)
		}
	})

	if !strings.Contains(out, "No upgrade is in progress") {
		t.Errorf("resume with no claim printed %q", out)
	}

	// THE ORDER, BY OUTCOME: a claim whose classification FAILS (a guard
	// directory whose record is a FIFO, which the classifier refuses as not a
	// regular file, without waiting on it) beside a held lock. The lock is taken
	// first, so the lock's refusal is the answer; a resume that classified before
	// locking would answer with the classifier's error instead.
	mustOK(t, os.Mkdir(f.active(), 0o700))
	mustOK(t, syscall.Mkfifo(filepath.Join(f.active(), guardRecordName), 0o600))

	end = startLockHolder(t)

	err := resumeHostUpgrade(t.Context(), f.cfg)

	end()

	if !errors.Is(err, ErrUpgradeInProgress) || strings.Contains(err.Error(), "record") {
		t.Errorf("resume under a held lock beside an unreadable claim: err = %v, want the lock's refusal first", err)
	}

	// And without the lock held, the classifier's answer is the resume's.
	if err := resumeHostUpgrade(t.Context(), f.cfg); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("resume over a guard whose record is a FIFO: err = %v, want the classifier's refusal", err)
	}

	mustOK(t, os.RemoveAll(f.active()))
}

// H3: THE TIMER HOLDS ONE LOCK FROM CLASSIFICATION THROUGH SETTLEMENT: exactly
// one acquisition, no unlock before the settlement, an independent helper
// refused at both barriers, and the ledger read after the lock; a guarded host
// is nothing to do without a ledger read; a held lock is nothing to decide.
func TestTheTimerHoldsOneLockFromClassificationThroughSettlement(t *testing.T) {
	f := newGuardedFixture(t)

	// A ledger with a rollout whose target this binary already runs, so the
	// timer's path is classify, read, settle.
	runningAs(t, "v0.9.4")

	cfg, _ := ledgerWithRollout(t, "v0.9.4")
	cfgPath := writeConfigFor(t, cfg)

	_ = f

	var ops []string

	guardHook = func(op guardOp) error {
		if op.Kind == "flock" || op.Kind == "unlock" {
			ops = append(ops, op.Kind)
		}

		return nil
	}

	timerBarrier = func(step string) {
		ops = append(ops, "barrier:"+step)

		// AN INDEPENDENT OPEN CANNOT TAKE THE LOCK HERE.
		if lockFreeInProcess(t) {
			t.Errorf("at the %s barrier the transaction lock was free", step)
		}
	}

	out := capture(t, func() {
		if err := hostUpgradeFromRollout(t.Context(), cfg, cfgPath, false); err != nil {
			t.Errorf("the timer: %v", err)
		}
	})

	guardHook, timerBarrier = nil, nil

	if got := strings.Join(ops, " "); got != "flock barrier:instruction barrier:settlement unlock" {
		t.Errorf("the timer's lock operations were %q, want one flock, both barriers, then the unlock", got)
	}

	if !strings.Contains(out, "nothing to do") {
		t.Errorf("the settled timer printed %q", out)
	}

	if !lockFreeInProcess(t) {
		t.Error("the timer kept the lock")
	}

	// GUARDED: exit 0 with the guard's words, and the ledger is never read.
	mustHold(t, "ci-1")

	read := false
	timerBarrier = func(string) { read = true }

	out = capture(t, func() {
		if err := hostUpgradeFromRollout(t.Context(), cfg, cfgPath, false); err != nil {
			t.Errorf("the timer on a guarded host: %v", err)
		}
	})

	timerBarrier = nil

	if !strings.Contains(out, "guarded by ci-1 since") || !strings.Contains(out, "nothing to do") {
		t.Errorf("the timer on a guarded host printed %q", out)
	}

	if read {
		t.Error("the timer read the ledger on a guarded host")
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	// A HELD LOCK: nothing to decide, exit 0, no read.
	end := startLockHolder(t)
	timerBarrier = func(string) { read = true }

	out = capture(t, func() {
		if err := hostUpgradeFromRollout(t.Context(), cfg, cfgPath, false); err != nil {
			t.Errorf("the timer under a held lock: %v", err)
		}
	})

	timerBarrier = nil
	end()

	if !strings.Contains(out, "nothing to decide") || read {
		t.Errorf("the timer under a held lock printed %q (read=%v)", out, read)
	}
}

// writeConfigFor writes a config file naming the ledger's identity directory,
// so the command wrappers can load it.
func writeConfigFor(t *testing.T, cfg *config.Config) string {
	t.Helper()

	return writeCAConfig(t, cfg.Server.IdentityDir)
}

// H4: `--status` names every shape and changes nothing.
func TestUpgradeStatusNamesEveryClaimShape(t *testing.T) {
	f := newGuardedFixture(t)

	shapes := map[string]func(){
		"none":                         func() {},
		"converge-guard: held by ci-1": func() { mustHold(t, "ci-1") },
		"unpublished-guard":            func() { mustOK(t, os.MkdirAll(f.active(), 0o700)) },
		"legacy-role": func() {
			mustOK(t, os.MkdirAll(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("x"), 0o600))
		},
		"which does not exist": func() {
			mustOK(t, os.MkdirAll(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-gone"), f.active()))
		},
	}

	for want, plant := range shapes {
		_ = os.RemoveAll(f.active())
		plant()

		out := capture(t, func() { reportUpgradeStatus() })

		if want == "none" {
			if !strings.Contains(out, "no upgrade has claimed this machine") {
				t.Errorf("status with no claim printed %q", out)
			}

			continue
		}

		if !strings.Contains(out, want) {
			t.Errorf("status over %s printed %q", want, out)
		}
	}

	_ = os.RemoveAll(f.active())
	mustHold(t, "ci-1")

	before := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))
	_ = capture(t, func() { reportUpgradeStatus() })

	if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != before {
		t.Error("--status changed the guard")
	}
}

// H6: END TO END THROUGH THE NODE: the node's own updater launcher runs the
// production `billet host-upgrade --ack-path` against a guarded root, and the
// refusal it reads from the socket is ErrUpgradeRefused carrying the guard's
// words, the way the coordinator then records it.
func TestTheNodeReadsTheGuardsRefusalFromTheUpdaterItLaunched(t *testing.T) {
	f := newGuardedFixture(t)
	mustHold(t, "ci-1")

	// The test binary, re-executed as the command by a wrapper the launcher
	// execs.
	wrapper := filepath.Join(t.TempDir(), "billet")
	script := "#!/bin/sh\n" +
		guardHelperModeEnv + "=host-upgrade " + guardHelperRootEnv + "=" + upgradeRoot + " " + guardHelperBinEnv + "=" + installedBinary +
		" exec " + os.Args[0] + " -test.run='^TestGuardHelperProcess$' -- \"$@\"\n"

	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ackDir, err := os.MkdirTemp("/tmp", "billet-ack-") //nolint:usetesting // t.TempDir exceeds darwin's socket address limit
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(ackDir) })

	upgrader := node.ExecUpgrader{Binary: wrapper, ConfigPath: f.cfgPath, AckDir: ackDir}

	err = upgrader.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.9.4"})
	if !errors.Is(err, node.ErrUpgradeRefused) || !strings.Contains(err.Error(), "guarded by ci-1 since 2026-09-09T12:00:00Z") {
		t.Fatalf("the node's launch: err = %v, want ErrUpgradeRefused carrying the guard", err)
	}

	if got := f.record(t).Holder; got != "ci-1" {
		t.Errorf("the refused updater changed the guard to %q", got)
	}

	_ = version.Version
}
