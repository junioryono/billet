package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE CONVERGE GUARD'S FIXTURES, from the list Codex reviewed five times
// before this code was written (scratchpad/pr6b/fixtures-c2.md). Each names the
// wrong implementation it is written against. The tests here set package seams
// (upgradeRoot, installedBinary, guardHook, guardNow, guardHostname,
// guardOwnerOf, guardProcesses) and are NOT parallel.

// guardFixture is a temporary upgrade root with a managed binary beside it,
// the seams pointed at both.
type guardFixture struct {
	parent, root, binary string
	binarySHA            string
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()

	parent := t.TempDir()
	f := &guardFixture{parent: parent, root: filepath.Join(parent, "upgrades")}

	f.binary = filepath.Join(t.TempDir(), "billet")
	if err := os.WriteFile(f.binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	sum, _, err := hashRegular(f.binary, maxExecutableBytes)
	if err != nil {
		t.Fatal(err)
	}

	f.binarySHA = sum

	savedRoot, savedBinary, savedNow, savedHost := upgradeRoot, installedBinary, guardNow, guardHostname
	upgradeRoot, installedBinary = f.root, f.binary
	guardNow = func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }
	guardHostname = func() (string, error) { return "billet-control-01", nil }

	t.Cleanup(func() {
		upgradeRoot, installedBinary, guardNow, guardHostname = savedRoot, savedBinary, savedNow, savedHost
		guardHook = nil
	})

	return f
}

func (f *guardFixture) active() string { return filepath.Join(f.root, "active") }

// openRootForTest opens the upgrade root the way the lock hands it to the
// claim helpers, for a fixture that calls them without taking the lock.
func openRootForTest(t *testing.T) *os.File {
	t.Helper()

	root, err := os.OpenFile(upgradeRoot, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	return root
}

func (f *guardFixture) record(t *testing.T) guardRecord {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	var rec guardRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decode the record: %v", err)
	}

	return rec
}

// hold runs the production entry with these arguments.
func guardRun(t *testing.T, args ...string) error {
	t.Helper()

	return cmdConvergeGuard(t.Context(), args)
}

func mustHold(t *testing.T, holder string) {
	t.Helper()

	if err := guardRun(t, "hold", "--holder", holder); err != nil {
		t.Fatalf("hold %s: %v", holder, err)
	}
}

// fileIdentity is what a fixture compares before and after an operation that must
// touch nothing: inode, size and modification time.
type fileIdentity struct {
	Ino  uint64
	Size int64
	Mod  time.Time
}

func fileIdentityOf(t *testing.T, path string) fileIdentity {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s carries no inode", path)
	}

	return fileIdentity{Ino: st.Ino, Size: info.Size(), Mod: info.ModTime()}
}

// lockFreeInProcess proves the transaction lock is not held by an independent
// open in this process.
func lockFreeInProcess(t *testing.T) bool {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(upgradeRoot, txLockName), os.O_RDWR, 0)
	if err != nil {
		return true
	}

	defer func() { _ = f.Close() }()

	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// G1: A HOLD PUBLISHES EXACTLY THE RECORD, owns the guard, and releases the
// lock: an independent open in a helper process takes it afterwards.
func TestAHoldPublishesTheRecordAndReleasesTheLock(t *testing.T) {
	f := newGuardFixture(t)

	before := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	mustHold(t, "ci-1")

	info, err := os.Lstat(f.active())
	if err != nil {
		t.Fatal(err)
	}

	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("active is %v, want a directory of mode 0700", info.Mode())
	}

	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		t.Errorf("active is not owned by the holder's account")
	}

	body, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	if want := []string{"claimed_at", "holder", "hostname", "release_executable", "release_executable_sha256"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("the record's keys are %v, want %v", keys, want)
	}

	rec := f.record(t)

	if rec.Holder != "ci-1" || rec.Hostname != "billet-control-01" || rec.ReleaseExecutable != f.binary ||
		rec.ReleaseExecutableSHA256 != f.binarySHA {
		t.Errorf("the record is %+v", rec)
	}

	if at, err := time.Parse(time.RFC3339, rec.ClaimedAt); err != nil || !at.Equal(before) {
		t.Errorf("claimed_at is %q, want the clock's %s", rec.ClaimedAt, before.Format(time.RFC3339))
	}

	if _, err := os.Lstat(filepath.Join(f.active(), guardTmpName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary was left behind (lstat err = %v)", err)
	}

	// THE LOCK IS RELEASED, proved by an independent open in another process
	// and by one in this one.
	end := startLockHolder(t)
	end()

	if !lockFreeInProcess(t) {
		t.Error("the transaction lock is still held after the hold returned")
	}
}

// A HOSTNAME THAT CANNOT BE READ REFUSES THE HOLD and leaves nothing.
func TestAHoldRefusesWithoutAHostname(t *testing.T) {
	f := newGuardFixture(t)
	guardHostname = func() (string, error) { return "", errors.New("gethostname: staged failure") }

	if err := guardRun(t, "hold", "--holder", "ci-1"); err == nil || !strings.Contains(err.Error(), "staged failure") {
		t.Fatalf("a hold without a hostname: err = %v", err)
	}

	if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused hold left active (lstat err = %v)", err)
	}
}

// G2: THE PUBLICATION ORDER, by the operations the hook sees, each flush on the
// descriptor of the object named; and a parent flush only when the root was
// created by this run.
func TestAHoldPublishesInOneOrder(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("root exists=%v", existing), func(t *testing.T) {
			f := newGuardFixture(t)

			if existing {
				if err := os.Mkdir(f.root, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			var ops []guardOp

			guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

			mustHold(t, "ci-1")

			guardHook = nil

			var kinds []string

			for _, op := range ops {
				kinds = append(kinds, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))
			}

			want := []string{
				"lstat upgrades", "openat upgrades", "openat upgrades/transaction.lock", "flock upgrades/transaction.lock",
				"open " + f.binary, "hash " + f.binary,
				"lstat upgrades/active", "mkdir upgrades/active", "create upgrades/active/guard.json.tmp",
				"write upgrades/active/guard.json.tmp", "fsync upgrades/active/guard.json.tmp",
				"rename upgrades/active/guard.json", "fsync upgrades/active", "fsync upgrades",
			}

			// THE PARENT IS FLUSHED BY EVERY ACQUISITION, right after the root is
			// found or made and before anything is written under it: an acquisition
			// that made the root and died before its flush leaves a root the next
			// one finds existing, and a flush only the creator performed would then
			// be owed by nobody.
			if existing {
				want = slices.Insert(want, 1, "fsync "+f.parent)
			} else {
				want = slices.Insert(want, 1, "mkdir upgrades", "fsync "+f.parent)
			}

			// THE LOCK IS RELEASED LAST, after the last flush.
			want = append(want, "unlock upgrades/transaction.lock")

			if !reflect.DeepEqual(kinds, want) {
				t.Errorf("the publication's operations:\n got %q\nwant %q", kinds, want)
			}

			// EVERY FLUSH IS ON THE OBJECT IT NAMES: the descriptor's fileIdentity is
			// the path's.
			for _, op := range ops {
				if op.Kind != "fsync" {
					continue
				}

				path := op.Path
				if op.Kind == "fsync" && strings.HasSuffix(path, guardTmpName) {
					path = filepath.Join(f.active(), guardRecordName)
				}

				info, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("lstat %s: %v", path, err)
				}

				if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Ino != op.Ino {
					t.Errorf("the fsync of %s ran on inode %d, not the object's %d", op.Path, op.Ino, st.Ino)
				}
			}
		})
	}
}

// G2 (injection): EACH OPERATION CAN FAIL, and the failure leaves the remainder
// the durability table names; the hold reports it.
func TestAHoldInterruptedByAFailureLeavesAClassifiedRemainder(t *testing.T) {
	cases := []struct {
		fail string
		kind claimKind
		tmp  bool
	}{
		{"lstat upgrades/active", claimNone, false},
		{"mkdir upgrades/active", claimNone, false},
		{"create upgrades/active/guard.json.tmp", claimUnpublished, false},
		{"write upgrades/active/guard.json.tmp", claimUnpublished, true},
		{"fsync upgrades/active/guard.json.tmp", claimUnpublished, true},
		{"rename upgrades/active/guard.json", claimUnpublished, true},
		{"fsync upgrades/active", claimGuard, false},
		{"fsync upgrades", claimGuard, false},
	}

	for _, c := range cases {
		t.Run(c.fail, func(t *testing.T) {
			f := newGuardFixture(t)
			injected := errors.New("staged failure at " + c.fail)

			guardHook = func(op guardOp) error {
				if op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/") == c.fail {
					return injected
				}

				return nil
			}

			err := guardRun(t, "hold", "--holder", "ci-1")
			guardHook = nil

			if !errors.Is(err, injected) {
				t.Fatalf("the hold did not report the failure: err = %v", err)
			}

			shape, err := classifyClaim()
			if err != nil {
				t.Fatal(err)
			}

			if shape.Kind != c.kind {
				t.Errorf("the remainder is %s, want %s", shape.Kind, c.kind)
			}

			_, tmpErr := os.Lstat(filepath.Join(f.active(), guardTmpName))
			if hasTmp := tmpErr == nil; hasTmp != c.tmp {
				t.Errorf("a temporary is present=%v, want %v", hasTmp, c.tmp)
			}

			if !lockFreeInProcess(t) {
				t.Error("the failed hold kept the lock")
			}

			switch c.kind {
			case claimUnpublished:
				if err := guardRun(t, "recover", "--unpublished"); err != nil {
					t.Errorf("recover --unpublished on the remainder: %v", err)
				}

				if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
					t.Error("the recovery left active")
				}
			case claimGuard:
				if err := guardRun(t, "recover", "--unpublished"); err == nil || !strings.Contains(err.Error(), "held by ci-1") {
					t.Errorf("recover --unpublished on a published guard: err = %v, want a refusal naming the holder", err)
				}
			}
		})
	}
}

// The helper the process-level fixtures run, selected by an environment
// variable; it announces on stdout and blocks on stdin so the parent decides
// when it ends.
const (
	guardHelperModeEnv = "BILLET_TEST_GUARD_HELPER"
	guardHelperRootEnv = "BILLET_TEST_GUARD_ROOT"
	guardHelperArgsEnv = "BILLET_TEST_GUARD_ARGS"
	guardHelperStopEnv = "BILLET_TEST_GUARD_STOP_BEFORE"
	// guardHelperContinueEnv, when set, makes a stopped helper continue past
	// its stop once the parent writes to its stdin, rather than end there.
	guardHelperContinueEnv = "BILLET_TEST_GUARD_CONTINUE"
	guardHelperBinEnv      = "BILLET_TEST_GUARD_BINARY"
	guardHelperReadyMsg    = "HELD"
)

func TestGuardHelperProcess(t *testing.T) {
	mode := os.Getenv(guardHelperModeEnv)
	if mode == "" {
		t.Skip("not the helper process")
	}

	root := filepath.Clean(os.Getenv(guardHelperRootEnv))
	if !strings.HasPrefix(root, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("the helper was pointed outside the temporary directory: %s", root)
	}

	upgradeRoot = root
	installedBinary = os.Getenv(guardHelperBinEnv)
	guardHostname = func() (string, error) { return "billet-control-01", nil }

	announce := func(line string) {
		if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
			t.Fatalf("announce: %v", err)
		}
	}

	waitForParent := func() {
		if _, err := os.Stdin.Read(make([]byte, 1)); err != nil {
			os.Exit(0)
		}
	}

	switch mode {
	case "lock":
		// An INDEPENDENT open of the lock, held until the parent says otherwise.
		f, err := os.OpenFile(filepath.Join(root, txLockName), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatalf("helper open: %v", err)
		}

		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("helper flock: %v", err)
		}

		announce(fmt.Sprintf("%s:%d", guardHelperReadyMsg, os.Getpid()))
		waitForParent()
		announce("RELEASED")

		return
	case "host-upgrade":
		// The production command a node launches, with the arguments the node
		// passed after the test binary's own flags, and the clock the fixtures use.
		guardNow = func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }

		args := flag.Args()
		if len(args) == 0 || args[0] != "host-upgrade" {
			t.Fatalf("the helper was launched with %v, want host-upgrade ...", args)
		}

		if err := cmdHostUpgrade(t.Context(), args[1:]); err != nil {
			announce("REFUSED:" + err.Error())
			os.Exit(2)
		}

		announce("DONE")

		return
	case "command":
		// The production command, stopping before the named operation when one
		// is named: the parent then classifies the remainder and kills this
		// process, so nothing after the stop ever runs.
		stop := os.Getenv(guardHelperStopEnv)
		if stop != "" {
			// STOPPED, then either ended where it stands (the parent classifies
			// the remainder) or, when the parent asks for it, CONTINUED past the
			// stop with the hook removed, so what the parent changed under it is
			// what the rest of the command meets.
			continues := os.Getenv(guardHelperContinueEnv) != ""

			guardHook = func(op guardOp) error {
				if op.Kind+" "+strings.TrimPrefix(op.Path, root+"/") == stop {
					announce("STOPPED:" + stop)
					waitForParent()

					if continues {
						guardHook = nil

						return nil
					}

					os.Exit(3)
				}

				return nil
			}
		}

		err := cmdConvergeGuard(t.Context(), strings.Split(os.Getenv(guardHelperArgsEnv), "\x1f"))
		if err != nil {
			announce("REFUSED:" + err.Error())
			os.Exit(2)
		}

		announce("DONE")

		return
	}

	t.Fatalf("unknown helper mode %q", mode)
}

// guardHelper is one helper process and the lines it printed.
type guardHelper struct {
	cmd    *exec.Cmd
	stdin  *os.File
	lines  chan string
	pid    int
	reaped bool
}

func startGuardHelper(t *testing.T, mode string, env map[string]string) *guardHelper {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestGuardHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), guardHelperModeEnv+"="+mode, guardHelperRootEnv+"="+upgradeRoot,
		guardHelperBinEnv+"="+installedBinary)

	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd.Stdin = stdinR

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the helper: %v", err)
	}

	_ = stdinR.Close()

	h := &guardHelper{cmd: cmd, stdin: stdinW, lines: make(chan string, 64), pid: cmd.Process.Pid}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "HELD") || strings.HasPrefix(line, "STOPPED") || strings.HasPrefix(line, "DONE") ||
				strings.HasPrefix(line, "REFUSED") || strings.HasPrefix(line, "RELEASED") {
				h.lines <- line
			}
		}

		close(h.lines)
	}()

	t.Cleanup(func() {
		_ = stdinW.Close()

		if h.reaped {
			return
		}

		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Logf("killing the helper: %v", err)
		}

		if err := cmd.Wait(); err != nil && !strings.Contains(err.Error(), "signal: killed") {
			t.Logf("reaping the helper: %v", err)
		}

		h.reaped = true
	})

	return h
}

// await is the helper's next announcement with the prefix, within a deadline.
func (h *guardHelper) await(t *testing.T, prefix string) string {
	t.Helper()

	deadline := time.After(30 * time.Second)

	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatalf("the helper ended before announcing %s", prefix)
			}

			if strings.HasPrefix(line, prefix) {
				return line
			}

			t.Fatalf("the helper announced %q while %s was awaited", line, prefix)
		case <-deadline:
			t.Fatalf("the helper never announced %s", prefix)
		}
	}
}

// release lets the helper continue (or end) and reaps it.
func (h *guardHelper) release(t *testing.T) {
	t.Helper()

	_ = h.stdin.Close()

	if err := h.cmd.Wait(); err != nil {
		t.Logf("the helper ended: %v", err)
	}

	h.reaped = true
}

// continueAwaiting tells a stopped helper to CONTINUE past its stop (one byte
// on its stdin; closing it would end the helper where it stands), waits for
// its announcement with the prefix, and only then reaps it: Wait closes the
// stdout pipe, so a Wait before the line was read could lose the very
// announcement awaited.
func (h *guardHelper) continueAwaiting(t *testing.T, prefix string) string {
	t.Helper()

	if _, err := h.stdin.Write([]byte{'\n'}); err != nil {
		t.Fatalf("tell the helper to continue: %v", err)
	}

	line := h.await(t, prefix)

	if err := h.cmd.Wait(); err != nil {
		t.Logf("the helper ended: %v", err)
	}

	h.reaped = true

	return line
}

// kill ends the helper where it stands and reaps it.
func (h *guardHelper) kill(t *testing.T) {
	t.Helper()

	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill the helper: %v", err)
	}

	_ = h.cmd.Wait() //nolint:errcheck // killed on purpose; its status is the signal
	h.reaped = true
}

// startLockHolder runs a helper that holds the transaction lock through its
// own open, and returns the function that ends it.
func startLockHolder(t *testing.T) func() {
	t.Helper()

	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	h := startGuardHelper(t, "lock", nil)
	h.await(t, guardHelperReadyMsg)

	return func() { h.release(t) }
}

func helperArgs(args ...string) map[string]string {
	return map[string]string{guardHelperArgsEnv: strings.Join(args, "\x1f")}
}

// G2b: A ROOT LEFT BY AN ACQUISITION KILLED BETWEEN ITS MKDIR AND ITS PARENT
// FLUSH is flushed by the next acquisition, which finds it existing.
func TestARootAKilledAcquisitionLeftUnflushedIsFlushedByTheNext(t *testing.T) {
	f := newGuardFixture(t)

	env := helperArgs("hold", "--holder", "ci-1")
	env[guardHelperStopEnv] = "fsync " + f.parent

	h := startGuardHelper(t, "command", env)
	h.await(t, "STOPPED")
	h.kill(t)

	if _, err := os.Lstat(f.root); err != nil {
		t.Fatalf("the killed acquisition left no root: %v", err)
	}

	var flushed []string

	guardHook = func(op guardOp) error {
		if op.Kind == "fsync" {
			flushed = append(flushed, op.Path)
		}

		return nil
	}

	mustHold(t, "ci-1")

	guardHook = nil

	if !slices.Contains(flushed, f.parent) {
		t.Errorf("the next acquisition flushed %q, want the root's parent among them", flushed)
	}
}

// G3: INTERRUPTION AT EACH PUBLICATION STEP, by a helper killed where it
// stands: the remainder is what the durability table names and its recovery
// answers.
func TestAHoldKilledAtEachStepLeavesAClassifiedRemainder(t *testing.T) {
	cases := []struct {
		stopBefore string
		kind       claimKind
	}{
		{"mkdir active", claimNone},
		{"create active/guard.json.tmp", claimUnpublished},
		{"write active/guard.json.tmp", claimUnpublished},
		{"fsync active/guard.json.tmp", claimUnpublished},
		{"rename active/guard.json", claimUnpublished},
		{"fsync active", claimGuard},
		{"fsync .", claimGuard},
	}

	for _, c := range cases {
		t.Run(c.stopBefore, func(t *testing.T) {
			f := newGuardFixture(t)

			if err := os.Mkdir(f.root, 0o700); err != nil {
				t.Fatal(err)
			}

			env := helperArgs("hold", "--holder", "ci-1")
			stop := c.stopBefore
			if stop == "fsync ." {
				stop = "fsync " + f.root
			}

			env[guardHelperStopEnv] = stop

			h := startGuardHelper(t, "command", env)
			h.await(t, "STOPPED")
			h.kill(t)

			shape, err := classifyClaim()
			if err != nil {
				t.Fatal(err)
			}

			if shape.Kind != c.kind {
				t.Fatalf("the remainder is %s, want %s", shape.Kind, c.kind)
			}

			if !lockFreeInProcess(t) {
				t.Error("the killed helper's lock survived it")
			}

			switch c.kind {
			case claimUnpublished:
				if err := guardRun(t, "recover", "--unpublished"); err != nil {
					t.Errorf("recover --unpublished: %v", err)
				}
			case claimGuard:
				if shape.Guard.Holder != "ci-1" {
					t.Errorf("the published remainder names %q", shape.Guard.Holder)
				}

				if err := guardRun(t, "recover", "--unpublished"); err == nil {
					t.Error("recover --unpublished removed a published guard")
				}

				// THE SAME HOLDER VALIDATES IT AND COMPLETES ITS DURABILITY: the
				// remainder of a hold killed between its rename and its flushes is
				// in the directory and not yet on the disk, and the retry is what
				// flushes the guard directory and the root.
				var flushed []string

				guardHook = func(op guardOp) error {
					if op.Kind == "fsync" {
						flushed = append(flushed, op.Path)
					}

					return nil
				}

				if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
					t.Errorf("a same-holder hold on the remainder: %v", err)
				}

				guardHook = nil

				if !slices.Contains(flushed, f.active()) || !slices.Contains(flushed, f.root) {
					t.Errorf("the retry flushed %q, want the guard directory and the root", flushed)
				}
			}
		})
	}
}

// G4: A SAME-HOLDER HOLD WRITES NOTHING AND COMPLETES DURABILITY: it flushes
// the guard directory and the root (a retry of a hold killed after its rename
// and before its flushes owes exactly that), touches no file, and validates: a
// stale temporary, a wrong mode, another owner, or a different candidate
// refuses.
func TestASameHolderHoldValidatesAndTouchesNothing(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	active, record := fileIdentityOf(t, f.active()), fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))
	bytesBefore, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	var ops []guardOp

	guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

	if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
		t.Fatalf("same-holder hold: %v", err)
	}

	guardHook = nil

	var flushed []string

	for _, op := range ops {
		switch op.Kind {
		case "write", "rename", "unlink", "mkdir", "create", "rmdir", "truncate":
			if strings.Contains(op.Path, "active") {
				t.Errorf("a same-holder hold performed %s %s", op.Kind, op.Path)
			}
		case "fsync":
			flushed = append(flushed, op.Path)
		}
	}

	// THE ACQUISITION'S PARENT FLUSH, then the two directory flushes in
	// publication order, and no file's.
	if want := []string{f.parent, f.active(), f.root}; !reflect.DeepEqual(flushed, want) {
		t.Errorf("a same-holder hold flushed %q, want the parent, the guard directory then the root", flushed)
	}

	bytesAfter, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	if fileIdentityOf(t, f.active()) != active || fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record ||
		!bytes.Equal(bytesBefore, bytesAfter) {
		t.Error("a same-holder hold changed the guard")
	}

	t.Run("a flush that fails fails the hold", func(t *testing.T) {
		guardHook = func(op guardOp) error {
			if op.Kind == "fsync" && op.Path == f.active() {
				return errors.New("injected: the guard directory's flush refused")
			}

			return nil
		}

		t.Cleanup(func() { guardHook = nil })

		if err := guardRun(t, "hold", "--holder", "ci-1"); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Errorf("a same-holder hold whose flush failed: err = %v, want the failure", err)
		}
	})

	t.Run("a stale temporary refuses", func(t *testing.T) {
		tmp := filepath.Join(f.active(), guardTmpName)
		if err := os.WriteFile(tmp, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = os.Remove(tmp) })

		if err := guardRun(t, "hold", "--holder", "ci-1"); err == nil || !strings.Contains(err.Error(), guardTmpName) {
			t.Errorf("a same-holder hold beside a temporary: err = %v", err)
		}
	})

	t.Run("a wrong mode refuses", func(t *testing.T) {
		if err := os.Chmod(f.active(), 0o755); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(f.active(), 0o700); err != nil {
				t.Error(err)
			}
		})

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a same-holder hold on a 0755 guard: err = %v, want the trust boundary", err)
		}
	})

	t.Run("another owner refuses", func(t *testing.T) {
		saved := guardOwnerOf
		guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
			if info.IsDir() && info.Mode().Perm() == 0o700 {
				st, ok := info.Sys().(*syscall.Stat_t)
				if ok && st.Ino == active.Ino {
					return uint32(os.Geteuid()) + 1, true
				}
			}

			return ownerFromInfo(info)
		}

		t.Cleanup(func() { guardOwnerOf = saved })

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a same-holder hold on another account's guard: err = %v, want the trust boundary", err)
		}
	})

	t.Run("a different candidate refuses", func(t *testing.T) {
		candidate := stageGuardCandidate(t, f, "recovery-x", []byte("other"))

		if err := guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate); err == nil ||
			!strings.Contains(err.Error(), "cannot change it") {
			t.Errorf("a same-holder hold with another candidate: err = %v", err)
		}

		if got := f.record(t); got.ReleaseExecutable != f.binary {
			t.Errorf("the recorded executable moved to %s", got.ReleaseExecutable)
		}
	})
}

// stageGuardCandidate puts a candidate into a recovery directory under the root.
func stageGuardCandidate(t *testing.T, f *guardFixture, dir string, body []byte) string {
	t.Helper()

	recovery := filepath.Join(f.root, dir)
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(recovery, "billet.candidate")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// G5: A FOREIGN HOLDER IS REFUSED, EXACTLY, AND NEVER EXPIRES; a holder that is
// not a name is refused before the lock is taken.
func TestAForeignHolderIsRefusedExactlyAndNeverExpires(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	record := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

	for _, holder := range []string{"ci-2", "ci-1x", "CI-1", "ci-"} {
		err := guardRun(t, "hold", "--holder", holder)
		if !errors.Is(err, errGuardHeld) || !strings.Contains(err.Error(), "held by ci-1") ||
			!strings.Contains(err.Error(), "recover") {
			t.Errorf("hold by %q over ci-1's guard: err = %v", holder, err)
		}
	}

	guardNow = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }

	if err := guardRun(t, "hold", "--holder", "ci-2"); !errors.Is(err, errGuardHeld) || !strings.Contains(err.Error(), "720h0m0s ago") {
		t.Errorf("a month later the guard did not refuse with its age: err = %v", err)
	}

	if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record {
		t.Error("a refused hold changed the record")
	}

	// NOT A NAME: refused before the lock, so the hook sees no lock operation.
	for _, holder := range []string{"", " ", "ci 1", "ci\n1", "ci/1", "ci\x001", strings.Repeat("x", 201)} {
		var ops []guardOp

		guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

		err := guardRun(t, "hold", "--holder", holder)

		guardHook = nil

		if err == nil || !strings.Contains(err.Error(), "--holder") {
			t.Errorf("hold with holder %q: err = %v", holder, err)
		}

		if len(ops) != 0 {
			t.Errorf("hold with holder %q touched the root: %v", holder, ops)
		}
	}
}

// G6: RELEASE IS THE REVERSE ORDER, by the holder only, never across a pointer
// (dangling included), never over another shape, and "no guard" is a refusal.
func TestAReleaseIsOrderedAndRefusesWhatItDoesNotOwn(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	pointer := filepath.Join(f.active(), guardPointerName)
	record := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

	if err := guardRun(t, "release", "--holder", "ci-2"); !errors.Is(err, errGuardHeld) {
		t.Errorf("release by ci-2: err = %v", err)
	}

	if err := os.Symlink(filepath.Join(f.root, "recovery-gone"), pointer); err != nil {
		t.Fatal(err)
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); !errors.Is(err, errGuardPointer) {
		t.Errorf("release across a DANGLING pointer: err = %v, want the pointer refusal", err)
	}

	if err := os.Mkdir(filepath.Join(f.root, "recovery-gone"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); !errors.Is(err, errGuardPointer) {
		t.Errorf("release across a live pointer: err = %v", err)
	}

	if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record {
		t.Error("a refused release changed the record")
	}

	if _, err := os.Lstat(pointer); err != nil {
		t.Error("a refused release removed the pointer")
	}

	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}

	var ops []string

	guardHook = func(op guardOp) error {
		if strings.Contains(op.Path, "active") || op.Path == f.root {
			ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.root+"/"))
		}

		return nil
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	guardHook = nil

	want := []string{"lstat " + f.root, "openat " + f.root, "unlink active/guard.json", "fsync active", "rmdir active", "fsync " + f.root}
	if !reflect.DeepEqual(ops, want) {
		t.Errorf("the release's operations:\n got %q\nwant %q", ops, want)
	}

	if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the released guard remains")
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); !errors.Is(err, errNoGuard) {
		t.Errorf("a second release: err = %v, want the no-guard refusal", err)
	}

	for _, shape := range []string{"symlink", "file"} {
		var err error
		if shape == "symlink" {
			err = os.Symlink(filepath.Join(f.root, "recovery-x"), f.active())
		} else {
			err = os.WriteFile(f.active(), []byte("legacy"), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "release", "--holder", "ci-1"); err == nil {
			t.Errorf("release over a %s claim succeeded", shape)
		}

		if _, err := os.Lstat(f.active()); err != nil {
			t.Errorf("release removed a %s claim", shape)
		}

		_ = os.Remove(f.active())
	}
}

// G8: RECOVER --UNPUBLISHED EXAMINES EVERY ENTRY BEFORE REMOVING ANY.
func TestRecoverUnpublishedExaminesBeforeItRemoves(t *testing.T) {
	f := newGuardFixture(t)

	makeActive := func(t *testing.T) {
		t.Helper()

		_ = os.RemoveAll(f.active())

		if err := os.MkdirAll(f.active(), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a tmp alone is removed", func(t *testing.T) {
		makeActive(t)

		if err := os.WriteFile(filepath.Join(f.active(), guardTmpName), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "recover", "--unpublished"); err != nil {
			t.Fatalf("recover: %v", err)
		}

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("active remains")
		}
	})

	for name, plant := range map[string]func(t *testing.T){
		"a regular file of another name": func(t *testing.T) {
			t.Helper()

			if err := os.WriteFile(filepath.Join(f.active(), "other"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink named as the tmp": func(t *testing.T) {
			t.Helper()

			if err := os.Symlink(f.binary, filepath.Join(f.active(), guardTmpName)); err != nil {
				t.Fatal(err)
			}
		},
		"a nested directory": func(t *testing.T) {
			t.Helper()

			if err := os.Mkdir(filepath.Join(f.active(), "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a FIFO named as the tmp": func(t *testing.T) {
			t.Helper()

			if err := syscall.Mkfifo(filepath.Join(f.active(), guardTmpName), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name+" refuses and removes nothing", func(t *testing.T) {
			makeActive(t)

			// A legitimate tmp first in directory order, so a recover that
			// removed as it walked would have removed it.
			if name != "a symlink named as the tmp" && name != "a FIFO named as the tmp" {
				if err := os.WriteFile(filepath.Join(f.active(), guardTmpName), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			plant(t)

			entriesBefore, err := os.ReadDir(f.active())
			mustOK(t, err)

			var opened []string

			guardHook = func(op guardOp) error {
				if op.Kind == "unlink" || op.Kind == "rmdir" {
					opened = append(opened, op.Kind)
				}

				return nil
			}

			done := make(chan error, 1)

			go func() { done <- guardRun(t, "recover", "--unpublished") }()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the recovery succeeded over a foreign entry")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the recovery blocked (a FIFO was opened)")
			}

			guardHook = nil

			if len(opened) != 0 {
				t.Errorf("the refused recovery performed %v", opened)
			}

			entriesAfter, err := os.ReadDir(f.active())
			mustOK(t, err)

			if len(entriesAfter) != len(entriesBefore) {
				t.Errorf("the refused recovery removed entries: %d before, %d after", len(entriesBefore), len(entriesAfter))
			}
		})
	}

	t.Run("a published guard refuses naming the holder", func(t *testing.T) {
		_ = os.RemoveAll(f.active())
		mustHold(t, "ci-1")

		if err := guardRun(t, "recover", "--unpublished"); err == nil || !strings.Contains(err.Error(), "held by ci-1") {
			t.Errorf("recover --unpublished on a guard: err = %v", err)
		}

		if _, err := os.Lstat(filepath.Join(f.active(), guardRecordName)); err != nil {
			t.Error("the guard was removed")
		}
	})
}

// G9: STATUS AND HOLDER TAKE NO LOCK AND CREATE NOTHING, and name every shape.
func TestStatusNamesEveryShapeWithoutLockingOrCreating(t *testing.T) {
	f := newGuardFixture(t)

	var ops []guardOp

	guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

	out := capture(t, func() {
		if err := guardRun(t, "status"); err != nil {
			t.Errorf("status on an absent root: %v", err)
		}
	})

	guardHook = nil

	if !strings.HasPrefix(out, "none:") {
		t.Errorf("status on an absent root printed %q", out)
	}

	if _, err := os.Lstat(f.root); !errors.Is(err, fs.ErrNotExist) {
		t.Error("status created the upgrade root")
	}

	for _, op := range ops {
		if op.Kind == "flock" || op.Kind == "mkdir" || op.Kind == "create" {
			t.Errorf("status performed %s %s", op.Kind, op.Path)
		}
	}

	if err := guardRun(t, "holder"); !errors.Is(err, errNoGuard) {
		t.Errorf("holder with no guard: err = %v", err)
	}

	mustHold(t, "ci-1")

	end := startLockHolder(t)

	out = capture(t, func() {
		if err := guardRun(t, "status", "--json"); err != nil {
			t.Errorf("status --json under a held lock: %v", err)
		}
	})

	end()

	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, out)
	}

	guard, ok := doc["guard"].(map[string]any)
	if !ok {
		t.Fatalf("guard is %T, want an object", doc["guard"])
	}

	want := map[string]any{
		"holder": "ci-1", "claimed_at": "2026-09-09T12:00:00Z", "hostname": "billet-control-01",
		"recovery_pointer": false, "release_executable": f.binary, "release_executable_sha256": f.binarySHA,
		"release_executable_verified": true,
	}

	if doc["active"] != "converge-guard" || !reflect.DeepEqual(guard, want) {
		t.Errorf("status --json:\n got %v\nwant active=converge-guard guard=%v", doc, want)
	}

	out = capture(t, func() {
		if err := guardRun(t, "holder"); err != nil {
			t.Errorf("holder: %v", err)
		}
	})

	if out != "ci-1\n" {
		t.Errorf("holder printed %q", out)
	}

	// Every other shape.
	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	shapes := map[string]func(){
		"host-upgrade":      func() { mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active())) },
		"legacy-role":       func() { mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600)) },
		"unpublished-guard": func() { mustOK(t, os.Mkdir(f.active(), 0o700)) },
	}

	for want, plant := range shapes {
		_ = os.RemoveAll(f.active())
		plant()

		out := capture(t, func() {
			if err := guardRun(t, "status"); err != nil {
				t.Errorf("status over %s: %v", want, err)
			}
		})

		if !strings.HasPrefix(out, want+":") {
			t.Errorf("status over %s printed %q", want, out)
		}

		if err := guardRun(t, "holder"); !errors.Is(err, errNoGuard) {
			t.Errorf("holder over %s: err = %v", want, err)
		}
	}

	_ = os.RemoveAll(f.active())
	mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-gone"), f.active()))

	out = capture(t, func() {
		if err := guardRun(t, "status"); err != nil {
			t.Errorf("status over a dangling claim: %v", err)
		}
	})
	if !strings.Contains(out, "does not exist") {
		t.Errorf("a dangling claim is not reported dangling: %q", out)
	}

	// DANGLING IS A POSITIVE ABSENCE: a target that cannot be examined (a link
	// to itself, which the kernel refuses with ELOOP) is could-not-tell, and
	// status says so rather than reporting a claim whose target does not exist.
	_ = os.RemoveAll(f.active())
	mustOK(t, os.Symlink(activePointer, f.active()))

	if err := guardRun(t, "status"); err == nil || !strings.Contains(err.Error(), "examine the claim's target") ||
		strings.Contains(err.Error(), "does not exist") {
		t.Errorf("status over a claim whose target cannot be examined: err = %v, want could-not-tell", err)
	}

	if err := guardRun(t, "holder"); err == nil || !strings.Contains(err.Error(), "examine the claim's target") {
		t.Errorf("holder over a claim whose target cannot be examined: err = %v", err)
	}

	_ = os.RemoveAll(f.active())
	mustOK(t, os.Mkdir(f.active(), 0o700))
	mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte("not json"), 0o600))

	// A RECORD THAT CANNOT BE READ NAMES NO HOLDER: every command that would
	// act on the holder refuses, naming the record, and the record stays.
	for name, args := range map[string][]string{
		"holder":   {"holder"},
		"release":  {"release", "--holder", "ci-1"},
		"recover":  {"recover", "--holder", "ci-1", "--old-driver-stopped"},
		"takeover": {"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"},
		"hold":     {"hold", "--holder", "ci-1"},
	} {
		if err := guardRun(t, args...); !errors.Is(err, errGuardHeld) || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("%s over an unreadable record: err = %v, want the guard's refusal naming the record", name, err)
		}

		if body, err := os.ReadFile(filepath.Join(f.active(), guardRecordName)); err != nil || string(body) != "not json" {
			t.Errorf("%s changed the unreadable record: %q, %v", name, body, err)
		}
	}

	out = capture(t, func() {
		if err := guardRun(t, "status", "--json"); err != nil {
			t.Errorf("status --json over an unreadable record: %v", err)
		}
	})
	if !strings.Contains(out, `"release_executable_verified": {`) || !strings.Contains(out, "not JSON") {
		t.Errorf("an unreadable record is not reported unknown:\n%s", out)
	}
}

// G10: EXCLUSION AGAINST THE TRANSACTION LOCK AND THE GO CLAIM, staged.
func TestEveryMutatorIsExcludedByTheTransactionLock(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	record := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

	end := startLockHolder(t)

	for _, args := range [][]string{
		{"hold", "--holder", "ci-1"},
		{"hold", "--holder", "ci-2"},
		{"release", "--holder", "ci-1"},
		{"recover", "--unpublished"},
		{"recover", "--holder", "ci-1", "--old-driver-stopped"},
		{"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"},
	} {
		if err := guardRun(t, args...); !errors.Is(err, ErrUpgradeInProgress) {
			t.Errorf("%v under another process's lock: err = %v, want ErrUpgradeInProgress", args, err)
		}
	}

	if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record {
		t.Error("a mutator refused by the lock changed the record")
	}

	end()

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Errorf("release once the lock is free: %v", err)
	}
}

// G10 reverse: THE GO CLAIM'S OWN HELPERS PRESERVE A GUARD, reached directly
// under the lock a helper holds; and the entry point never reaches publishClaim
// on a guarded host.
func TestTheGoClaimHelpersPreserveAGuard(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	recovery := filepath.Join(f.root, "recovery-x")
	if err := os.Mkdir(recovery, 0o700); err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(recovery, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)); err != nil {
		t.Fatal(err)
	}

	before := map[string]fileIdentity{}
	for _, p := range []string{f.active(), filepath.Join(f.active(), guardRecordName), filepath.Join(f.active(), guardPointerName), sentinel} {
		before[p] = fileIdentityOf(t, p)
	}

	tx, err := takeTxLock()
	if err != nil {
		t.Fatal(err)
	}

	defer tx.release()

	// publishClaim is handed the transaction's OWN staged directory, never the
	// guard's pointer target; a refused publication may remove that staging.
	staged := filepath.Join(f.root, "recovery-staged")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := publishClaim(tx.dir, staged); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("publishClaim over a guard: err = %v, want the in-progress refusal", err)
	}

	for _, dir := range []string{recovery, filepath.Join(f.root, "recovery-other")} {
		if err := releaseClaim(tx.dir, dir); err == nil || !strings.Contains(err.Error(), "converge-guard") {
			t.Errorf("releaseClaim(%s) over a guard: err = %v, want a refusal naming the guard", dir, err)
		}

		if err := refuseClaimed(tx.dir, dir, errors.New("refused")); err == nil {
			t.Errorf("refuseClaimed(%s) over a guard succeeded", dir)
		}

		if err := abandonClaim(tx.dir, dir); err == nil {
			t.Errorf("abandonClaim(%s) over a guard succeeded", dir)
		}
	}

	for p, id := range before {
		if fileIdentityOf(t, p) != id {
			t.Errorf("%s changed under the claim helpers", p)
		}
	}

	if body, err := os.ReadFile(sentinel); err != nil || string(body) != "keep" {
		t.Errorf("the recovery directory's sentinel was lost: %q, %v", body, err)
	}
}

// G11: TWO SIMULTANEOUS HOLDS, released from one barrier: exactly one guard,
// its holder the one that reported success, and the loser wrote nothing.
func TestTwoSimultaneousHoldsLeaveOneGuard(t *testing.T) {
	for round := range 5 {
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		// BOTH HELPERS STOP AT THE FLOCK ITSELF, each holding its open of the
		// lock file, and are released together, so the race is the lock's and
		// not the process start's.
		helpers := map[string]*guardHelper{}

		for _, holder := range []string{"ci-b", "ci-c"} {
			env := helperArgs("hold", "--holder", holder)
			env[guardHelperStopEnv] = "flock " + txLockName
			env[guardHelperContinueEnv] = "1"
			helpers[holder] = startGuardHelper(t, "command", env)
		}

		for _, h := range helpers {
			h.await(t, "STOPPED")
		}

		for _, h := range helpers {
			if _, err := h.stdin.Write([]byte{'\n'}); err != nil {
				t.Fatalf("round %d: release a helper: %v", round, err)
			}
		}

		outcomes := map[string]string{}

		for holder, h := range helpers {
			outcomes[holder] = h.await(t, "")

			if err := h.cmd.Wait(); err != nil {
				t.Logf("round %d: %s ended: %v", round, holder, err)
			}

			h.reaped = true
		}

		var winners, losers []string

		for holder, line := range outcomes {
			switch {
			case line == "DONE":
				winners = append(winners, holder)
			case strings.HasPrefix(line, "REFUSED:"):
				losers = append(losers, holder)

				if !strings.Contains(line, "held by") && !strings.Contains(line, "already running") {
					t.Errorf("round %d: the loser %s refused with %q", round, holder, line)
				}
			default:
				t.Errorf("round %d: helper %s answered %q", round, holder, line)
			}
		}

		if len(winners) != 1 || len(losers) != 1 {
			t.Fatalf("round %d: winners %v and losers %v", round, winners, losers)
		}

		// THE GUARD NAMES THE HELPER THAT SUCCEEDED, not either.
		shape, err := classifyClaim()
		if err != nil || shape.Kind != claimGuard {
			t.Fatalf("round %d: the remainder is %v (%v)", round, shape.Kind, err)
		}

		if shape.Guard.Holder != winners[0] {
			t.Errorf("round %d: the guard names %q, want the winner %s", round, shape.Guard.Holder, winners[0])
		}

		if _, err := os.Lstat(filepath.Join(f.active(), guardTmpName)); err == nil {
			t.Errorf("round %d: a temporary remains", round)
		}
	}
}

// G12: A CANDIDATE IS VALIDATED THROUGH DESCRIPTORS AND DIGESTED HERE.
func TestACandidateIsValidatedAndDigestedThroughDescriptors(t *testing.T) {
	f := newGuardFixture(t)
	candidate := stageGuardCandidate(t, f, "recovery-x", []byte("candidate bytes"))

	sum, _, err := hashRegular(candidate, maxExecutableBytes)
	if err != nil {
		t.Fatal(err)
	}

	if err := guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate); err != nil {
		t.Fatalf("hold with a candidate: %v", err)
	}

	if rec := f.record(t); rec.ReleaseExecutable != candidate || rec.ReleaseExecutableSHA256 != sum {
		t.Errorf("the record is %+v, want the candidate and its digest", rec)
	}

	if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "billet.candidate")
	mustOK(t, os.WriteFile(outside, []byte("x"), 0o755))

	directChild := filepath.Join(f.root, "billet.candidate")
	mustOK(t, os.WriteFile(directChild, []byte("x"), 0o755))

	notRecovery := stageGuardCandidate(t, f, "staging-x", []byte("x"))
	nested := filepath.Join(f.root, "recovery-x", "deeper", "billet.candidate")
	mustOK(t, os.MkdirAll(filepath.Dir(nested), 0o700))
	mustOK(t, os.WriteFile(nested, []byte("x"), 0o755))

	linkToCandidate := filepath.Join(f.root, "recovery-x", "link")
	mustOK(t, os.Symlink(candidate, linkToCandidate))

	looseDir := stageGuardCandidate(t, f, "recovery-loose", []byte("x"))
	mustOK(t, os.Chmod(filepath.Dir(looseDir), 0o777))

	looseFile := stageGuardCandidate(t, f, "recovery-loosefile", []byte("x"))
	mustOK(t, os.Chmod(looseFile, 0o777))

	fifo := filepath.Join(f.root, "recovery-fifo", "billet.candidate")
	mustOK(t, os.MkdirAll(filepath.Dir(fifo), 0o700))
	mustOK(t, syscall.Mkfifo(fifo, 0o600))

	linkedDir := filepath.Join(f.root, "recovery-linkeddir")
	mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), linkedDir))

	for name, path := range map[string]string{
		"outside the root":                outside,
		"a direct child of the root":      directChild,
		"not a recovery directory":        notRecovery,
		"nested deeper":                   nested,
		"a symlink to a valid candidate":  linkToCandidate,
		"a sibling-prefix escape":         filepath.Join(f.root, "recovery-x-other", "..", "..", "billet.candidate"),
		"a group-writable recovery dir":   looseDir,
		"a group-writable candidate":      looseFile,
		"a FIFO":                          fifo,
		"a symlink at the recovery dir":   filepath.Join(linkedDir, "billet.candidate"),
		"a dot-dot escape spelled inside": filepath.Join(f.root, "recovery-x", "..", "billet.candidate"),
		"a relative path":                 "recovery-x/billet.candidate",
	} {
		done := make(chan error, 1)

		go func() { done <- guardRun(t, "hold", "--holder", "ci-1", "--candidate", path) }()

		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s (%s) was accepted", name, path)

				mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s (%s) blocked the hold", name, path)
		}

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s left a guard", name)

			_ = os.RemoveAll(f.active())
		}
	}

	// THE FLAG SET admits no digest.
	if err := guardRun(t, "hold", "--holder", "ci-1", "--sha256", sum); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("a --sha256 flag was accepted: err = %v", err)
	}
}

// G12 check-then-use: THE DIGEST IS THE OPENED FILE'S, and a name that no
// longer holds that file refuses; the saved original restored before the final
// check succeeds with the original digest.
func TestACandidateReplacedUnderTheHoldIsHashedAsOpenedAndRefusedByName(t *testing.T) {
	f := newGuardFixture(t)
	candidate := stageGuardCandidate(t, f, "recovery-x", []byte("original bytes"))
	replacement := stageGuardCandidate(t, f, "recovery-y", []byte("replacement bytes"))

	original, _, err := hashRegular(candidate, maxExecutableBytes)
	if err != nil {
		t.Fatal(err)
	}

	for _, restore := range []bool{true, false} {
		t.Run(fmt.Sprintf("restored=%v", restore), func(t *testing.T) {
			_ = os.RemoveAll(f.active())

			var hashed string
			saved := filepath.Join(f.root, "recovery-x", "saved")

			guardAfterHash = func(sum string) { hashed = sum }
			t.Cleanup(func() { guardAfterHash = nil })

			guardHook = func(op guardOp) error {
				if op.Kind == "hash" && op.Path == candidate {
					// FSTATTED: the descriptor is open; swap the name.
					if err := os.Rename(candidate, saved); err != nil {
						t.Fatal(err)
					}

					if err := os.Rename(replacement, candidate); err != nil {
						t.Fatal(err)
					}
				}

				return nil
			}

			var restored bool

			guardAfterHash = func(sum string) {
				hashed = sum

				if restore {
					// HASHED: put the original back before the final check.
					mustOK(t, os.Rename(candidate, replacement))
					mustOK(t, os.Rename(saved, candidate))

					restored = true
				}
			}

			err := guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate)

			guardHook = nil

			if hashed != original {
				t.Errorf("the hold hashed %s, want the original descriptor's %s", hashed, original)
			}

			if restore {
				if err != nil || !restored {
					t.Fatalf("the hold with the original restored: err = %v", err)
				}

				if rec := f.record(t); rec.ReleaseExecutableSHA256 != original {
					t.Errorf("the record carries %s, want the original digest", rec.ReleaseExecutableSHA256)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "changed while it was being recorded") {
					t.Fatalf("the hold with the name replaced: err = %v", err)
				}

				// Put things back for the next round.
				mustOK(t, os.Rename(candidate, replacement))
				mustOK(t, os.Rename(saved, candidate))
			}
		})
	}
}

// G13: A TAKEOVER KEEPS THE POINTER AND THE EXECUTABLE, needs the assertion,
// the pointer, a clean scan and the exact old holder.
func TestATakeoverRelabelsAndKeepsEverythingElse(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	recovery := filepath.Join(f.root, "recovery-x")
	if err := os.Mkdir(recovery, 0o700); err != nil {
		t.Fatal(err)
	}

	cleanScan(t)

	rec := f.record(t)

	// Without a pointer: refused naming recover.
	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err == nil ||
		!strings.Contains(err.Error(), "recover --holder ci-1") {
		t.Errorf("a takeover without a pointer: err = %v", err)
	}

	pointer := filepath.Join(f.active(), guardPointerName)
	if err := os.Symlink(recovery, pointer); err != nil {
		t.Fatal(err)
	}

	// A pointer whose journal cannot be read: refused.
	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err == nil ||
		!strings.Contains(err.Error(), "journal cannot be read") {
		t.Errorf("a takeover over an unreadable journal: err = %v", err)
	}

	writeJournalFixture(t, recovery, "installed")

	pointerID := fileIdentityOf(t, pointer)

	for name, args := range map[string][]string{
		"without the flag":     {"hold", "--holder", "ci-2", "--recover-from", "ci-1"},
		"the wrong old holder": {"hold", "--holder", "ci-2", "--recover-from", "ci-3", "--old-driver-stopped"},
		"a trailing space":     {"hold", "--holder", "ci-2", "--recover-from", "ci-1 ", "--old-driver-stopped"},
		"with a candidate":     {"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped", "--candidate", filepath.Join(recovery, "billet.candidate")},
	} {
		if err := guardRun(t, args...); err == nil {
			t.Errorf("%s: the takeover succeeded", name)
		}

		if got := f.record(t); got != rec {
			t.Errorf("%s: the record changed to %+v", name, got)
		}
	}

	// A STALE TEMPORARY THE TRUST BOUNDARY REFUSES IS NOT REPLACED, and keeps
	// its bytes: a loose-mode regular file, a FIFO (never waited on), and a
	// symlink, each planted at the temporary's name before the takeover.
	tmp := filepath.Join(f.active(), guardTmpName)

	for name, plant := range map[string]func(){
		"a group-writable temporary": func() {
			// Chmod after the write, which the umask would otherwise narrow.
			mustOK(t, os.WriteFile(tmp, []byte("stale bytes"), 0o600))
			mustOK(t, os.Chmod(tmp, 0o666))
		},
		"a FIFO at the temporary":    func() { mustOK(t, syscall.Mkfifo(tmp, 0o600)) },
		"a symlink at the temporary": func() { mustOK(t, os.Symlink(f.binary, tmp)) },
	} {
		_ = os.Remove(tmp)
		plant()

		before, err := os.Lstat(tmp)
		mustOK(t, err)

		done := make(chan error, 1)

		go func() {
			done <- guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
		}()

		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "cannot be replaced") {
				t.Errorf("%s: the takeover answered %v, want the stale temporary's refusal", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s blocked the takeover", name)
		}

		after, err := os.Lstat(tmp)
		mustOK(t, err)

		if !os.SameFile(before, after) || after.Mode() != before.Mode() || after.Size() != before.Size() {
			t.Errorf("%s was touched by the refused takeover: %v -> %v", name, before.Mode(), after.Mode())
		}

		if got := f.record(t); got != rec {
			t.Errorf("%s: the record changed to %+v", name, got)
		}
	}

	mustOK(t, os.Remove(tmp))

	// A HARD-LINKED TEMPORARY IS NEVER TRUNCATED: a temporary that is another
	// name of the record, or of a preserved binary, would be emptied in place
	// under its other name. Both are refused before the truncation, and the
	// other name keeps its bytes.
	preserved := filepath.Join(recovery, "billet.previous")
	mustOK(t, os.WriteFile(preserved, []byte("the previous binary"), 0o700))

	for name, target := range map[string]string{
		"the record":         filepath.Join(f.active(), guardRecordName),
		"a preserved binary": preserved,
	} {
		bytesBefore, err := os.ReadFile(target)
		mustOK(t, err)

		mustOK(t, os.Link(target, tmp))

		if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); !errors.Is(err, errTrustBoundary) ||
			!strings.Contains(err.Error(), "links") {
			t.Errorf("a temporary hard-linked to %s: err = %v, want the trust boundary naming the links", name, err)
		}

		if bytesAfter, err := os.ReadFile(target); err != nil || !bytes.Equal(bytesBefore, bytesAfter) {
			t.Errorf("a temporary hard-linked to %s: the other name's bytes are %q, %v", name, bytesAfter, err)
		}

		if got := f.record(t); got != rec {
			t.Errorf("a temporary hard-linked to %s: the record changed to %+v", name, got)
		}

		mustOK(t, os.Remove(tmp))
	}

	// A TRUSTED STALE TEMPORARY IS EXAMINED THROUGH AN IDENTITY DESCRIPTOR AND
	// THEN REMOVED BY ITS NAME, never opened for writing and never truncated:
	// with the removal refused by the hook, the bytes are still there, and no
	// operation opened the temporary for writing or truncated it.
	mustOK(t, os.WriteFile(tmp, []byte("the previous takeover's half-written record"), 0o600))

	var kinds []string

	guardHook = func(op guardOp) error {
		if strings.HasSuffix(op.Path, guardTmpName) {
			kinds = append(kinds, op.Kind)
		}

		if op.Kind == "unlink" && strings.HasSuffix(op.Path, guardTmpName) {
			return errors.New("injected: the removal refused")
		}

		return nil
	}

	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err == nil ||
		!strings.Contains(err.Error(), "injected") {
		t.Errorf("the injected removal failure: err = %v", err)
	}

	if !reflect.DeepEqual(kinds, []string{"create", "unlink"}) {
		t.Errorf("the operations on the stale temporary were %v, want the exclusive create's refusal then the unlink", kinds)
	}

	guardHook = nil

	if body, err := os.ReadFile(tmp); err != nil || string(body) != "the previous takeover's half-written record" {
		t.Errorf("the stale temporary's bytes after a refused truncation: %q, %v", body, err)
	}

	mustOK(t, os.Remove(tmp))

	// A marker in the scan refuses.
	markedScan(t, 4242, "python3 /home/ci/.ansible/tmp/ansible-tmp-1/AnsiballZ_command.py")

	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); !errors.Is(err, errScanRefused) ||
		!strings.Contains(err.Error(), "4242") {
		t.Errorf("a takeover beside a driver: err = %v", err)
	}

	cleanScan(t)

	var ops []string

	guardHook = func(op guardOp) error {
		if strings.Contains(op.Path, "active") || op.Path == f.root {
			ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.root+"/"))
		}

		return nil
	}

	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err != nil {
		t.Fatalf("the takeover: %v", err)
	}

	guardHook = nil

	want := []string{"lstat " + f.root, "openat " + f.root, "readlink active/recovery", "create active/guard.json.tmp",
		"write active/guard.json.tmp", "fsync active/guard.json.tmp", "rename active/guard.json", "fsync active", "fsync " + f.root}
	if !reflect.DeepEqual(ops, want) {
		t.Errorf("the takeover's operations:\n got %q\nwant %q", ops, want)
	}

	got := f.record(t)
	rec.Holder = "ci-2"

	if got != rec {
		t.Errorf("after the takeover the record is %+v, want %+v", got, rec)
	}

	if fileIdentityOf(t, pointer) != pointerID {
		t.Error("the takeover moved the pointer")
	}

	out := capture(t, func() { mustOK(t, guardRun(t, "holder")) })
	if out != "ci-2\n" {
		t.Errorf("holder after the takeover: %q", out)
	}
}

// writeJournalFixture writes a journal a takeover can read.
func writeJournalFixture(t *testing.T, dir, step string) {
	t.Helper()

	body := fmt.Sprintf(`{"schema": 1, "dir": %q, "from_version": "v0.9.3", "to_version": "v0.9.4", "step": %q, "started_at": "2026-09-09T12:00:00Z"}`, dir, step)

	if err := os.WriteFile(filepath.Join(dir, "journal.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cleanScan(t *testing.T) {
	t.Helper()

	saved := guardProcesses
	guardProcesses = func() (processTable, error) {
		self := os.Getpid()

		return processTable{Self: self, Entries: map[int]processEntry{
			1:    {PID: 1, PPID: 0, Cmdline: "/sbin/init"},
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
		}}, nil
	}

	t.Cleanup(func() { guardProcesses = saved })
}

func markedScan(t *testing.T, pid int, cmdline string) {
	t.Helper()

	saved := guardProcesses
	guardProcesses = func() (processTable, error) {
		self := os.Getpid()

		return processTable{Self: self, Entries: map[int]processEntry{
			1:    {PID: 1, PPID: 0, Cmdline: "/sbin/init"},
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			pid:  {PID: pid, PPID: 1, Cmdline: cmdline},
		}}, nil
	}

	t.Cleanup(func() { guardProcesses = saved })
}

// mustOK fails the test on an error a fixture's own step returned.
func mustOK(tb testing.TB, err error) {
	tb.Helper()

	if err != nil {
		tb.Fatal(err)
	}
}
