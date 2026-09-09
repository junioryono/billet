package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// G14: RECOVER --HOLDER removes a guard naming the holder that carries no
// pointer, under the assertion and a clean scan; everything else refuses and
// leaves the guard.
func TestRecoverHolderRemovesOnlyAnAssertedCleanGuard(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	cleanScan(t)

	record := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

	for name, args := range map[string][]string{
		"no flag":             {"recover", "--holder", "ci-1"},
		"wrong holder":        {"recover", "--holder", "ci-2", "--old-driver-stopped"},
		"both recoveries":     {"recover", "--holder", "ci-1", "--unpublished"},
		"flag on unpublished": {"recover", "--unpublished", "--old-driver-stopped"},
	} {
		if err := guardRun(t, args...); err == nil {
			t.Errorf("%s: recover succeeded", name)
		}

		if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record {
			t.Fatalf("%s: the record changed", name)
		}
	}

	pointer := filepath.Join(f.active(), guardPointerName)
	if err := os.Symlink(filepath.Join(f.root, "recovery-x"), pointer); err != nil {
		t.Fatal(err)
	}

	if err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"); !errors.Is(err, errGuardPointer) ||
		!strings.Contains(err.Error(), "--recover-from ci-1") {
		t.Errorf("recover across a pointer: err = %v", err)
	}

	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}

	markedScan(t, 4242, "python3 /home/ci/.ansible/tmp/ansible-tmp-1/AnsiballZ_command.py")

	if err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"); !errors.Is(err, errScanRefused) {
		t.Errorf("recover beside a driver: err = %v", err)
	}

	unreadableScan(t)

	if err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"); !errors.Is(err, errScanRefused) {
		t.Errorf("recover with an unreadable scan: err = %v", err)
	}

	if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != record {
		t.Fatal("a refused recovery changed the record")
	}

	cleanScan(t)

	if err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"); err != nil {
		t.Fatalf("recover: %v", err)
	}

	if _, err := os.Lstat(f.active()); err == nil {
		t.Error("the recovered guard remains")
	}
}

func unreadableScan(t *testing.T) {
	t.Helper()

	saved := guardProcesses
	guardProcesses = func() (processTable, error) { return processTable{}, errors.New("proc: staged EACCES") }

	t.Cleanup(func() { guardProcesses = saved })
}

// G15: THE SCAN, through takeover and recovery: the ancestry first with its own
// diagnostic, both markers in both places, an unreadable table, an unreadable
// entry, a malformed parent, a vanished process, and a clean table.
func TestTheProcessScanRefusesADriverAndCouldNotTell(t *testing.T) {
	self := os.Getpid()

	table := func(entries map[int]processEntry) func() (processTable, error) {
		entries[1] = processEntry{PID: 1, PPID: 0, Cmdline: "/sbin/init"}

		return func() (processTable, error) { return processTable{Self: self, Entries: entries}, nil }
	}

	cases := []struct {
		name  string
		table func() (processTable, error)
		want  string
	}{
		{"a marked grandparent", table(map[int]processEntry{
			self: {PID: self, PPID: 900, Cmdline: "billet converge-guard"},
			900:  {PID: 900, PPID: 800, Cmdline: "/bin/sh -c ..."},
			800:  {PID: 800, PPID: 1, Cmdline: "python3 /root/.ansible/tmp/ansible-tmp-9/AnsiballZ_command.py"},
		}), "direct SSH shell"},
		{"a marked parent by the other marker", table(map[int]processEntry{
			self: {PID: self, PPID: 900, Cmdline: "billet converge-guard"},
			900:  {PID: 900, PPID: 1, Cmdline: "sh -c /root/.ansible/tmp/ansible-tmp-9/run"},
		}), "direct SSH shell"},
		{"a marked sibling", table(map[int]processEntry{
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			4242: {PID: 4242, PPID: 1, Cmdline: "python3 AnsiballZ_setup.py"},
		}), "pid 4242"},
		{"a marked sibling by the other marker", table(map[int]processEntry{
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			4243: {PID: 4243, PPID: 1, Cmdline: "sh /home/ci/.ansible/tmp/ansible-tmp-3/x"},
		}), "pid 4243"},
		{"a vanished ancestor", table(map[int]processEntry{
			self: {PID: self, PPID: 777, Cmdline: "billet converge-guard"},
		}), "ancestor pid 777 vanished"},
		{"an unreadable ancestor", table(map[int]processEntry{
			self: {PID: self, PPID: 777, Cmdline: "billet converge-guard"},
			777:  {PID: 777, Err: errors.New("read status: permission denied")},
		}), "ancestor pid 777 could not be read"},
		{"an unreadable sibling", table(map[int]processEntry{
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			555:  {PID: 555, Err: errors.New("read cmdline: permission denied")},
		}), "pid 555 could not be read"},
		{"a malformed parent", table(map[int]processEntry{
			self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			556:  {PID: 556, Err: errors.New("status has a PPid line that is not a number")},
		}), "pid 556 could not be read"},
		{"an unreadable table", func() (processTable, error) { return processTable{}, errors.New("proc: staged EACCES") }, "could not be read"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")

			saved := guardProcesses
			guardProcesses = c.table

			t.Cleanup(func() { guardProcesses = saved })

			err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped")
			if !errors.Is(err, errScanRefused) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("recover: err = %v, want the scan's refusal naming %q", err, c.want)
			}

			if _, err := os.Lstat(filepath.Join(f.active(), guardRecordName)); err != nil {
				t.Error("a refused recovery removed the guard")
			}
		})
	}

	t.Run("a vanished sibling is skipped and the ancestry is walked first", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		order := []string{}
		saved := guardProcesses
		guardProcesses = func() (processTable, error) {
			return processTable{Self: self, Entries: map[int]processEntry{
				1:    {PID: 1, PPID: 0, Cmdline: "/sbin/init"},
				self: {PID: self, PPID: 1, Cmdline: "billet converge-guard"},
			}}, nil
		}

		t.Cleanup(func() { guardProcesses = saved })

		_ = order

		if err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"); err != nil {
			t.Errorf("recover with a clean table: %v", err)
		}

		if _, err := os.Lstat(f.active()); err == nil {
			t.Error("the guard remains")
		}
	})
}

// G16: THE TRUST BOUNDARY, through descriptors: a symlink at the lock, a FIFO,
// a directory, a symlink at the root, a group-writable root, another owner, a
// loose parent; and the substitution windows.
func TestTheTrustBoundaryRefusesWhatOnlyAnotherAccountCouldHaveMade(t *testing.T) {
	t.Run("a symlink at the lock", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
			t.Fatal(err)
		}

		lock := filepath.Join(f.root, txLockName)
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(f.binary, lock); err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) || !errors.Is(err, syscall.ELOOP) {
			t.Errorf("a symlink at the lock: err = %v, want the trust boundary with ELOOP", err)
		}
	})

	t.Run("a FIFO at the lock is never opened", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		if err := syscall.Mkfifo(filepath.Join(f.root, txLockName), 0o600); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)

		go func() { done <- guardRun(t, "hold", "--holder", "ci-1") }()

		select {
		case err := <-done:
			if !errors.Is(err, errTrustBoundary) {
				t.Errorf("a FIFO at the lock: err = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a FIFO at the lock blocked the hold")
		}
	})

	t.Run("a directory at the lock", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.MkdirAll(filepath.Join(f.root, txLockName), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a directory at the lock: err = %v", err)
		}
	})

	t.Run("a symlink at the root", func(t *testing.T) {
		f := newGuardFixture(t)
		target := t.TempDir()

		if err := os.Symlink(target, f.root); err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a symlink at the root: err = %v", err)
		}

		entries, err := os.ReadDir(target)
		mustOK(t, err)

		if len(entries) != 0 {
			t.Errorf("the hold wrote through the symlink: %v", entries)
		}
	})

	t.Run("a group-writable root", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		// Chmod after the mkdir, which the umask would otherwise narrow.
		if err := os.Chmod(f.root, 0o770); err != nil {
			t.Fatal(err)
		}

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a 0770 root: err = %v", err)
		}
	})

	t.Run("a loose parent", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.Chmod(f.parent, 0o777); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(f.parent, 0o700); err != nil {
				t.Error(err)
			}
		})

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("a world-writable parent: err = %v", err)
		}

		if _, err := os.Lstat(f.root); err == nil {
			t.Error("the hold created the root under an untrusted parent")
		}
	})

	t.Run("another account's root", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		rootID := fileIdentityOf(t, f.root)
		saved := guardOwnerOf
		guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == rootID.Ino {
				return uint32(os.Geteuid()) + 1, true
			}

			return ownerFromInfo(info)
		}

		t.Cleanup(func() { guardOwnerOf = saved })

		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) {
			t.Errorf("another account's root: err = %v", err)
		}
	})

	t.Run("the lock stays on the descriptor it was opened on when the name is replaced", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Mkdir(f.root, 0o700))

		lockPath := filepath.Join(f.root, txLockName)
		mustOK(t, os.WriteFile(lockPath, nil, 0o600))

		// A descriptor of the ORIGINAL lock file, held across the hold: a flock
		// on it conflicts with the guard's exactly when the guard locked that inode.
		original, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		mustOK(t, err)

		t.Cleanup(func() { _ = original.Close() })

		substitute := filepath.Join(f.root, "substitute.lock")
		mustOK(t, os.WriteFile(substitute, nil, 0o600))

		var (
			swapped, observed            bool
			heldOriginal, heldSubstitute error
		)

		guardHook = func(op guardOp) error {
			switch {
			case op.Kind == "flock" && strings.HasSuffix(op.Path, txLockName) && !swapped:
				// BETWEEN THE OPEN AND THE FLOCK the name is given to another file:
				// a lock taken on a descriptor reopened by name would land here.
				if err := os.Rename(substitute, lockPath); err != nil {
					return err
				}

				swapped = true
			case op.Kind == "lstat" && strings.HasSuffix(op.Path, activePointer) && swapped && !observed:
				observed = true
				heldOriginal = syscall.Flock(int(original.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

				if heldOriginal == nil {
					if err := syscall.Flock(int(original.Fd()), syscall.LOCK_UN); err != nil {
						return err
					}
				}

				g, err := os.OpenFile(lockPath, os.O_RDWR, 0)
				if err != nil {
					return err
				}

				heldSubstitute = syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

				if heldSubstitute == nil {
					if err := syscall.Flock(int(g.Fd()), syscall.LOCK_UN); err != nil {
						return err
					}
				}

				_ = g.Close()
			}

			return nil
		}

		mustHold(t, "ci-1")

		guardHook = nil

		if !swapped || !observed {
			t.Fatalf("the fixture did not reach its observation (swapped=%v observed=%v)", swapped, observed)
		}

		if !errors.Is(heldOriginal, syscall.EWOULDBLOCK) {
			t.Errorf("the guard did not hold the inode its open produced: a flock on it answered %v", heldOriginal)
		}

		if heldSubstitute != nil {
			t.Errorf("the file substituted at the name was locked by the guard: %v", heldSubstitute)
		}
	})

	t.Run("the lock is judged on the descriptor that is locked", func(t *testing.T) {
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		var lockOps []guardOp

		guardHook = func(op guardOp) error {
			if strings.HasSuffix(op.Path, txLockName) {
				lockOps = append(lockOps, op)
			}

			return nil
		}

		mustHold(t, "ci-1")

		guardHook = nil

		// ONE open of the lock's name, relative to the root, then the flock on
		// the descriptor that open produced, then its unlock: no reopen by name
		// in between.
		kinds := []string{}
		for _, op := range lockOps {
			kinds = append(kinds, op.Kind)
		}

		if !reflect.DeepEqual(kinds, []string{"openat", "flock", "unlock"}) {
			t.Errorf("the lock's operations are %v, want [openat flock unlock]", kinds)
		}

		info, err := os.Lstat(filepath.Join(f.root, txLockName))
		if err != nil {
			t.Fatal(err)
		}

		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Ino != lockOps[1].Ino {
			t.Error("the flock ran on a descriptor that is not the lock file's")
		}
	})

	t.Run("the root validated is the root used", func(t *testing.T) {
		// A helper stops after the root is opened and before the lock is; the
		// parent renames the root aside and puts a symlink at its name pointing
		// at an outside directory holding its own lock. The helper's lock must
		// be inside the RETAINED root, never the outside one.
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.Mkdir(outside, 0o700); err != nil {
			t.Fatal(err)
		}

		outsideLock := filepath.Join(outside, txLockName)
		if err := os.WriteFile(outsideLock, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		env := helperArgs("hold", "--holder", "ci-1")
		env[guardHelperStopEnv] = "openat " + txLockName
		env[guardHelperContinueEnv] = "1"

		h := startGuardHelper(t, "command", env)
		h.await(t, "STOPPED")

		aside := f.root + ".aside"
		if err := os.Rename(f.root, aside); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(outside, f.root); err != nil {
			t.Fatal(err)
		}

		// THE HELPER FINISHES BEFORE ANYTHING IS JUDGED, so a check cannot run
		// ahead of a write it would have caught: the hold completes into the
		// RETAINED root (its lock, its guard), and the outside directory holds
		// nothing but the lock nobody locked.
		h.continueAwaiting(t, "DONE")

		entries, err := os.ReadDir(outside)
		mustOK(t, err)

		if len(entries) != 1 {
			t.Errorf("the helper wrote into the outside directory through the substituted root: %v", entries)
		}

		if _, err := os.Lstat(filepath.Join(aside, "active", guardRecordName)); err != nil {
			t.Errorf("the helper did not publish into the retained root: %v", err)
		}

		// And the retained root's lock is free again, the helper having released
		// it; the outside lock was never locked, so it is free too.
		for _, lock := range []string{filepath.Join(aside, txLockName), outsideLock} {
			g, err := os.OpenFile(lock, os.O_RDWR, 0)
			mustOK(t, err)

			if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("%s is still locked after the helper finished: %v", lock, err)
			}

			_ = g.Close()
		}
	})

	t.Run("the recovery directory validated is the recovery directory read", func(t *testing.T) {
		// Between the open of the recovery directory and the open of the
		// candidate inside it, the directory's NAME is given to a symlink at
		// another directory holding another candidate. The candidate hashed is
		// the one under the validated descriptor, and the name check at the end
		// refuses, because the name no longer holds that file; nothing is
		// recorded. An open by the candidate's full name would follow the link,
		// hash the other file, and record it under its own digest.
		f := newGuardFixture(t)
		candidate := stageGuardCandidate(t, f, "recovery-x", []byte("the validated candidate"))
		other := stageGuardCandidate(t, f, "recovery-other", []byte("another directory's candidate"))
		recovery := filepath.Dir(candidate)
		aside := recovery + ".aside"

		var swapped bool

		guardHook = func(op guardOp) error {
			if op.Kind == "openat" && op.Path == candidate && !swapped {
				if err := os.Rename(recovery, aside); err != nil {
					return err
				}

				if err := os.Symlink(filepath.Dir(other), recovery); err != nil {
					return err
				}

				swapped = true
			}

			return nil
		}

		var hashed string

		guardAfterHash = func(sum string) { hashed = sum }

		err := guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate)

		guardHook, guardAfterHash = nil, nil

		if !swapped {
			t.Fatal("the fixture never reached the candidate's open")
		}

		if err == nil || !strings.Contains(err.Error(), "changed while it was being recorded") {
			t.Errorf("a candidate whose directory was substituted under the hold: err = %v, want the name refusal", err)
		}

		validated, _, herr := hashRegular(filepath.Join(aside, "billet.candidate"), maxExecutableBytes)
		mustOK(t, herr)

		if hashed != validated {
			t.Errorf("the hold hashed %s, want the validated directory's candidate %s", hashed, validated)
		}

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("a refused candidate left a guard")
		}
	})
}

// G17: THE RECORDED EXECUTABLE IS NEVER RUN, whatever its digest says.
func TestTheRecordedExecutableIsNeverRun(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprintf("digest matches=%v", matching), func(t *testing.T) {
			f := newGuardFixture(t)

			marker := filepath.Join(t.TempDir(), "ran")
			script := "#!/bin/sh\ntouch " + marker + "\n"
			if err := os.WriteFile(f.binary, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			sum, _, err := hashRegular(f.binary, maxExecutableBytes)
			if err != nil {
				t.Fatal(err)
			}

			if !matching {
				sum = strings.Repeat("0", 64)
			}

			if err := os.Mkdir(f.root, 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.Mkdir(f.active(), 0o700); err != nil {
				t.Fatal(err)
			}

			record := fmt.Sprintf(`{"holder":"ci-1","claimed_at":"2026-09-09T11:00:00Z","hostname":"h","release_executable":%q,"release_executable_sha256":%q}`, f.binary, sum)
			if err := os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte(record), 0o600); err != nil {
				t.Fatal(err)
			}

			cleanScan(t)

			out := capture(t, func() {
				if err := guardRun(t, "status", "--json"); err != nil {
					t.Errorf("status: %v", err)
				}
			})

			want := fmt.Sprintf(`"release_executable_verified": %v`, matching)
			if !strings.Contains(out, want) {
				t.Errorf("status --json does not say %s:\n%s", want, out)
			}

			if err := guardRun(t, "holder"); err != nil {
				t.Errorf("holder: %v", err)
			}

			if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
				t.Errorf("same-holder hold: %v", err)
			}

			_ = capture(t, func() { reportUpgradeStatus() })

			if err := guardRun(t, "release", "--holder", "ci-1"); err != nil {
				t.Errorf("release: %v", err)
			}

			if _, err := os.Lstat(marker); err == nil {
				t.Fatal("the recorded executable was run")
			}
		})
	}
}

// G18: THE FLAG SET PER SUBCOMMAND, pinned, and the invalid combinations.
func TestTheGuardsFlagsArePinned(t *testing.T) {
	want := map[string][]string{
		"hold":    {"candidate", "holder", "old-driver-stopped", "recover-from"},
		"release": {"holder"},
		"status":  {"json"},
		"recover": {"holder", "old-driver-stopped", "unpublished"},
		"holder":  {},
	}

	for sub, flags := range want {
		out := capture(t, func() {
			if err := guardRun(t, sub, "--no-such-flag"); err == nil {
				t.Errorf("%s accepted --no-such-flag", sub)
			}
		})

		var got []string

		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "  -") {
				got = append(got, strings.Fields(strings.TrimPrefix(line, "  -"))[0])
			}
		}

		if got == nil {
			got = []string{}
		}

		if !reflect.DeepEqual(got, flags) {
			t.Errorf("%s's flags are %v, want %v", sub, got, flags)
		}
	}

	f := newGuardFixture(t)
	mustHold(t, "ci-1")

	for name, args := range map[string][]string{
		"takeover without the assertion": {"hold", "--holder", "ci-2", "--recover-from", "ci-1"},
		"the assertion on a plain hold":  {"hold", "--holder", "ci-2", "--old-driver-stopped"},
		"a candidate on a takeover":      {"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped", "--candidate", "/x"},
		"holder with unpublished":        {"recover", "--holder", "ci-1", "--unpublished"},
		"json on a mutator":              {"release", "--holder", "ci-1", "--json"},
	} {
		if err := guardRun(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if got := f.record(t).Holder; got != "ci-1" {
		t.Errorf("an invalid combination changed the guard to %q", got)
	}
}

// V1: `billet status` reports the host's own guard, read from the upgrade root
// and never from the ledger.
func TestStatusReportsTheHostsOwnGuard(t *testing.T) {
	f := newGuardFixture(t)
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfgPath}); err != nil {
			t.Errorf("status without a guard: %v", err)
		}
	})

	if strings.Contains(out, "guard ") {
		t.Errorf("status without a guard printed a guard line:\n%s", out)
	}

	mustHold(t, "ci-42")

	out = capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfgPath}); err != nil {
			t.Errorf("status with a guard: %v", err)
		}
	})

	if !strings.Contains(out, "guard     held by ci-42 for 0s (since 2026-09-09T12:00:00Z)") {
		t.Errorf("status did not name the host's guard:\n%s", out)
	}

	_ = f
}

// V2: `billet check` reports a guard, warns past a day, and never reads an
// unparsable time as fresh; the guard is unchanged by the check.
func TestCheckWarnsOnAGuardHeldPastADay(t *testing.T) {
	f := newGuardFixture(t)
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)
	mustHold(t, "ci-42")

	record := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))
	claimed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		age  time.Duration
		want string
	}{
		{23*time.Hour + 59*time.Minute, "guard    held by ci-42 for 23h59m0s"},
		{24 * time.Hour, "guard    WARNING: held by ci-42 for 24h0m0s"},
		{24*time.Hour + time.Minute, "guard    WARNING: held by ci-42 for 24h1m0s"},
	} {
		guardNow = func() time.Time { return claimed.Add(c.age) }

		out := capture(t, func() {
			_, _ = runCheck(t.Context(), checkOptions{configPath: cfgPath, maintenanceProbe: true}) //nolint:errcheck // the verdict is not what this fixture reads; the printed guard line is
		})

		if !strings.Contains(out, c.want) {
			t.Errorf("at %s the check printed:\n%s\nwant a line %q", c.age, out, c.want)
		}

		if strings.Contains(c.want, "WARNING") && !strings.Contains(out, "recover --holder ci-42") {
			t.Errorf("the warning does not name the recovery:\n%s", out)
		}
	}

	if err := os.WriteFile(filepath.Join(f.active(), guardRecordName),
		[]byte(`{"holder":"ci-42","claimed_at":"yesterday","hostname":"h"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() {
		_, _ = runCheck(t.Context(), checkOptions{configPath: cfgPath, maintenanceProbe: true}) //nolint:errcheck // the verdict is not what this fixture reads; the printed guard line is
	})

	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "not a time") {
		t.Errorf("an unparsable claim time was not reported as could-not-tell:\n%s", out)
	}

	if strings.Contains(out, "for 0s") {
		t.Errorf("an unparsable claim time was reported fresh:\n%s", out)
	}

	_ = record
	_ = config.GiB
	_ = state.LedgerPath
}
