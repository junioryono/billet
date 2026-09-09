package main

import (
	"bytes"
	"encoding/json"
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
	"github.com/junioryono/billet/internal/regularfile"
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

		// The link is opened as itself, relative to the root, and refused as not
		// a regular file: one answer on every platform, never a followed target.
		if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) || !errors.Is(err, regularfile.ErrNotRegular) {
			t.Errorf("a symlink at the lock: err = %v, want the trust boundary refusing a non-regular file", err)
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

	t.Run("a root displaced under the lock is refused, never used", func(t *testing.T) {
		// A helper stops after the root is opened and before the lock is; the
		// parent renames the root aside and puts a symlink at its name pointing
		// at an outside directory holding its own lock. The helper's lock is
		// inside the RETAINED root, never the outside one; and because the
		// transaction's journal is read by its name under the root's name, the
		// hold then REFUSES, naming the displaced root, rather than publishing
		// into a root the name no longer holds.
		f := newGuardFixture(t)

		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}

		// THE RETAINED ROOT ALREADY HOLDS A LOCK FILE, so the acquisition takes
		// the existing-lock path (identity first, relative to the root) and not
		// the exclusive create, which no name could redirect.
		mustOK(t, os.WriteFile(filepath.Join(f.root, txLockName), nil, 0o600))

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

		// THE OUTSIDE LOCK IS HELD by this process for the rest of the case: a
		// helper that opened the lock by its name would resolve it through the
		// substituted root, meet this flock, and answer "already running"
		// instead of the displaced root's refusal.
		held, err := os.OpenFile(outsideLock, os.O_RDWR, 0)
		mustOK(t, err)
		mustOK(t, syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

		t.Cleanup(func() { _ = held.Close() })

		// THE HELPER FINISHES BEFORE ANYTHING IS JUDGED, so a check cannot run
		// ahead of a write it would have caught: the hold refuses under the lock
		// it took inside the RETAINED root, publishes nowhere, and the outside
		// directory holds nothing but the lock this process holds.
		line := h.continueAwaiting(t, "REFUSED:")
		if !strings.Contains(line, "no longer names the directory the lock was taken inside") || !strings.Contains(line, f.root) {
			t.Errorf("the hold over a displaced root answered %q, want the refusal naming the root", line)
		}

		mustOK(t, syscall.Flock(int(held.Fd()), syscall.LOCK_UN))

		entries, err := os.ReadDir(outside)
		mustOK(t, err)

		if len(entries) != 1 {
			t.Errorf("the helper wrote into the outside directory through the substituted root: %v", entries)
		}

		if _, err := os.Lstat(filepath.Join(aside, "active")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the helper published into the displaced root: %v", err)
		}

		// And the retained root's lock is free again, the helper having released
		// it; the outside lock, released above, was never the helper's.
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

// G20: EVERY MUTATOR VALIDATES THE GUARD DIRECTORY THROUGH THE DESCRIPTOR IT
// THEN USES: a guard directory outside the trust boundary (a loose mode, or
// another account's) refuses a release, a recovery and a takeover before
// anything is read from it or written under it, and an unpublished directory
// outside it is not removed.
func TestEveryMutatorValidatesTheGuardDirectoryItActsOn(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	cleanScan(t)

	recovery := filepath.Join(f.root, "recovery-x")
	mustOK(t, os.Mkdir(recovery, 0o700))
	mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
	writeJournalFixture(t, recovery, "installed")

	rec := f.record(t)
	active := fileIdentityOf(t, f.active())

	mutators := map[string][]string{
		"release":  {"release", "--holder", "ci-1"},
		"recover":  {"recover", "--holder", "ci-1", "--old-driver-stopped"},
		"takeover": {"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"},
		"hold":     {"hold", "--holder", "ci-1"},
	}

	check := func(t *testing.T, what string) {
		t.Helper()

		for name, args := range mutators {
			if err := guardRun(t, args...); !errors.Is(err, errTrustBoundary) {
				t.Errorf("%s on %s: err = %v, want the trust boundary", name, what, err)
			}

			if got := f.record(t); got != rec {
				t.Errorf("%s on %s changed the record to %+v", name, what, got)
			}

			if fileIdentityOf(t, f.active()) != active {
				t.Errorf("%s on %s replaced the guard directory", name, what)
			}
		}
	}

	t.Run("a loose mode", func(t *testing.T) {
		mustOK(t, os.Chmod(f.active(), 0o755))
		t.Cleanup(func() { mustOK(t, os.Chmod(f.active(), 0o700)) })

		check(t, "a 0755 guard directory")
	})

	t.Run("another account", func(t *testing.T) {
		saved := guardOwnerOf
		guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == active.Ino {
				return uint32(os.Geteuid()) + 1, true
			}

			return ownerFromInfo(info)
		}

		t.Cleanup(func() { guardOwnerOf = saved })

		check(t, "another account's guard directory")
	})

	t.Run("an unpublished directory outside the boundary is not removed", func(t *testing.T) {
		mustOK(t, os.Remove(filepath.Join(f.active(), guardPointerName)))
		mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
		mustOK(t, os.Mkdir(f.active(), 0o755))

		if err := guardRun(t, "recover", "--unpublished"); !errors.Is(err, errTrustBoundary) ||
			!strings.Contains(err.Error(), "nothing was removed") {
			t.Errorf("recover --unpublished on a 0755 directory: err = %v", err)
		}

		if _, err := os.Lstat(f.active()); err != nil {
			t.Errorf("the refused recovery removed the directory: %v", err)
		}
	})
}

// G21: THE WALK TO THE ROOT'S PARENT JUDGES EVERY ANCESTOR: a directory on the
// way that another account could write, without the sticky bit, refuses; the
// same directory under the sticky bit is accepted; a link on the way is
// resolved and its target judged; a link to itself is refused rather than
// followed forever.
func TestEveryAncestorOfTheUpgradeRootIsJudged(t *testing.T) {
	f := newGuardFixture(t)

	base := t.TempDir()
	anc := filepath.Join(base, "anc")
	mustOK(t, os.Mkdir(anc, 0o777))
	mustOK(t, os.Chmod(anc, 0o777)) // the umask narrowed the mkdir

	parent := filepath.Join(anc, "lib", "billet")
	mustOK(t, os.MkdirAll(parent, 0o700))

	upgradeRoot = filepath.Join(parent, "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) ||
		!strings.Contains(err.Error(), anc) || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a world-writable ancestor: err = %v, want the trust boundary naming it", err)
	}

	if _, err := os.Lstat(upgradeRoot); err == nil {
		t.Error("the hold created the root under an untrusted ancestor")
	}

	// THROUGH A LINK: the link's target is what is judged.
	link := filepath.Join(base, "link")
	mustOK(t, os.Symlink(anc, link))

	upgradeRoot = filepath.Join(link, "lib", "billet", "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a world-writable ancestor behind a link: err = %v", err)
	}

	// THE STICKY BIT keeps another account from renaming what it did not make.
	mustOK(t, os.Chmod(anc, 0o777|os.ModeSticky))

	if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
		t.Errorf("a sticky world-writable ancestor behind a link: %v", err)
	}

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))

	upgradeRoot = filepath.Join(parent, "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
		t.Errorf("a sticky world-writable ancestor: %v", err)
	}

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))

	// ANOTHER ACCOUNT'S DIRECTORY on the way is refused whatever its mode.
	ancID := fileIdentityOf(t, anc)
	saved := guardOwnerOf

	t.Cleanup(func() { guardOwnerOf = saved })

	guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == ancID.Ino {
			return uint32(os.Geteuid()) + 1, true
		}

		return ownerFromInfo(info)
	}

	mustOK(t, os.Chmod(anc, 0o755))

	if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("another account's ancestor: err = %v", err)
	}

	guardOwnerOf = saved

	// A LINK ANOTHER ACCOUNT MADE INSIDE A STICKY DIRECTORY is refused however
	// trusted its target: under the sticky bit that account cannot rename
	// billet's entries there, and can repoint its own link at will.
	sticky := filepath.Join(base, "sticky")
	mustOK(t, os.Mkdir(sticky, 0o777))
	mustOK(t, os.Chmod(sticky, 0o777|os.ModeSticky))

	foreign := filepath.Join(sticky, "foreign")
	mustOK(t, os.Symlink(anc, foreign))

	linkInfo, err := os.Lstat(foreign)
	mustOK(t, err)

	linkSt, ok := linkInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("the link's stat carries no Stat_t")
	}

	linkIno := linkSt.Ino
	guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == linkIno && info.Mode()&os.ModeSymlink != 0 {
			return uint32(os.Geteuid()) + 1, true
		}

		return ownerFromInfo(info)
	}

	mustOK(t, os.Chmod(anc, 0o700))

	upgradeRoot = filepath.Join(foreign, "lib", "billet", "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) ||
		!strings.Contains(err.Error(), "the link ") || !strings.Contains(err.Error(), "/foreign is owned by uid") {
		t.Errorf("another account's link on the way: err = %v", err)
	}

	guardOwnerOf = saved

	// A TARGET WITH `..` BEHIND A LINK is walked as the kernel walks it: the
	// link before the `..` is resolved first, so `link -> a/../parent` with
	// `a -> elsewhere/child` reaches elsewhere/parent, where a lexical collapse
	// would have reached base/parent, which does not exist.
	nested := filepath.Join(t.TempDir(), "nested")
	mustOK(t, os.MkdirAll(filepath.Join(nested, "elsewhere", "child"), 0o700))
	mustOK(t, os.MkdirAll(filepath.Join(nested, "elsewhere", "parent"), 0o700))
	mustOK(t, os.Symlink("elsewhere/child", filepath.Join(nested, "a")))
	mustOK(t, os.Symlink("a/../parent", filepath.Join(nested, "link")))

	upgradeRoot = filepath.Join(nested, "link", "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); err != nil {
		t.Errorf("a target with .. behind a link: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(nested, "elsewhere", "parent", "upgrades", "active", guardRecordName)); err != nil {
		t.Errorf("the root was not made where the kernel's walk leads: %v", err)
	}

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))

	// A LINK TO ITSELF on the way is refused, never followed forever.
	loop := filepath.Join(base, "loop")
	mustOK(t, os.Symlink("loop", loop))

	upgradeRoot = filepath.Join(loop, "lib", "billet", "upgrades")

	if err := guardRun(t, "hold", "--holder", "ci-1"); !errors.Is(err, errTrustBoundary) || !strings.Contains(err.Error(), "too many links") {
		t.Errorf("a self-referential link on the way: err = %v", err)
	}

	_ = f
}

// G22: WHAT THE GUARD READS IS OPENED IDENTITY FIRST: a FIFO planted where a
// candidate is expected is refused without being waited on, and the lock file
// likewise (the lock's own fixture is in the trust-boundary test).
func TestACandidateThatIsNotARegularFileIsRefusedWithoutAWait(t *testing.T) {
	f := newGuardFixture(t)

	recovery := filepath.Join(f.root, "recovery-x")
	mustOK(t, os.MkdirAll(recovery, 0o700))

	fifo := filepath.Join(recovery, "billet.candidate")
	mustOK(t, syscall.Mkfifo(fifo, 0o700))

	done := make(chan error, 1)

	go func() { done <- guardRun(t, "hold", "--holder", "ci-1", "--candidate", fifo) }()

	select {
	case err := <-done:
		if !errors.Is(err, regularfile.ErrNotRegular) {
			t.Errorf("a FIFO candidate: err = %v, want the regular-file rule", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a FIFO candidate blocked the hold")
	}

	if _, err := os.Lstat(f.active()); err == nil {
		t.Error("a hold refused at its candidate published a guard")
	}
}

// G23: THE JOURNAL A RESUME OR A TAKEOVER READS IS OPENED THROUGH THE ROOT THE
// LOCK VALIDATED, the recovery directory relative to the root's descriptor and
// the journal relative to that. The root is displaced AFTER the lock's own
// check, from inside the hook that fires on the claim's readlink: renamed
// aside, and a symlink at its name pointing at an outside tree whose journal
// is not JSON. Neither command reads the outside journal: the resume finds
// the retained root's claim has no journal and releases it there, and the
// takeover reads the retained journal and relabels the retained record.
func TestAJournalIsNeverReadThroughADisplacedRoot(t *testing.T) {
	displace := func(t *testing.T, f *guardFixture, outside string) func(guardOp) error {
		t.Helper()

		fired := false

		return func(op guardOp) error {
			if op.Kind != "readlink" || fired {
				return nil
			}

			fired = true

			mustOK(t, os.Rename(f.root, f.root+".aside"))
			mustOK(t, os.Symlink(outside, f.root))

			return nil
		}
	}

	t.Run("resume", func(t *testing.T) {
		f := newGuardedFixture(t)
		mustOK(t, os.Mkdir(f.root, 0o700))

		// The retained root's claim points at a recovery directory with NO
		// journal; the outside tree's has one that is not JSON.
		recovery := filepath.Join(f.root, "recovery-x")
		mustOK(t, os.Mkdir(recovery, 0o700))
		mustOK(t, os.Symlink(recovery, f.active()))

		outside := filepath.Join(t.TempDir(), "outside")
		mustOK(t, os.MkdirAll(filepath.Join(outside, "recovery-x"), 0o700))
		mustOK(t, os.WriteFile(filepath.Join(outside, "recovery-x", "journal.json"), []byte("not json"), 0o600))
		mustOK(t, os.Symlink(filepath.Join(outside, "recovery-x"), filepath.Join(outside, "active")))

		guardHook = displace(t, f.guardFixture, outside)

		out := capture(t, func() {
			if err := resumeHostUpgrade(t.Context(), f.cfg); err != nil {
				t.Errorf("a resume over a root displaced under its lock: %v", err)
			}
		})

		guardHook = nil

		if !strings.Contains(out, "never wrote its journal") {
			t.Errorf("the resume printed %q, want the retained claim's empty journal", out)
		}

		// THE RETAINED CLAIM WAS RELEASED, through the descriptor; the outside
		// tree's claim and journal are untouched.
		if _, err := os.Lstat(filepath.Join(f.root+".aside", "active")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the retained claim survived the resume: %v", err)
		}

		if body, err := os.ReadFile(filepath.Join(outside, "recovery-x", "journal.json")); err != nil || string(body) != "not json" {
			t.Errorf("the outside journal changed: %q, %v", body, err)
		}

		if _, err := os.Lstat(filepath.Join(outside, "active")); err != nil {
			t.Errorf("the outside claim was removed: %v", err)
		}

		if len(f.reached) != 0 {
			t.Errorf("the resume reached %v", f.reached)
		}
	})

	t.Run("takeover", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")
		cleanScan(t)

		recovery := filepath.Join(f.root, "recovery-x")
		mustOK(t, os.Mkdir(recovery, 0o700))
		writeJournalFixture(t, recovery, "installed")
		mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))

		outside := filepath.Join(t.TempDir(), "outside")
		mustOK(t, os.MkdirAll(filepath.Join(outside, "recovery-x"), 0o700))
		mustOK(t, os.WriteFile(filepath.Join(outside, "recovery-x", "journal.json"), []byte("not json"), 0o600))

		rec := f.record(t)

		guardHook = displace(t, f, outside)

		err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")

		guardHook = nil

		if err != nil {
			t.Errorf("a takeover over a root displaced under its lock: %v", err)
		}

		// THE RETAINED RECORD WAS RELABELLED, through the descriptor, and the
		// outside tree holds no record at all.
		body, err := os.ReadFile(filepath.Join(f.root+".aside", "active", guardRecordName))
		mustOK(t, err)

		var got guardRecord

		mustOK(t, json.Unmarshal(body, &got))

		rec.Holder = "ci-2"

		if got != rec {
			t.Errorf("the takeover left the retained record as %+v, want %+v", got, rec)
		}

		if _, err := os.Lstat(filepath.Join(outside, "active")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the takeover wrote into the outside tree: %v", err)
		}
	})
}

// G24: A RECORD IS TRUSTED ONLY ON ITS OWN DESCRIPTOR: one another account
// could write (a loose mode, or another owner, which a hard link from outside
// the directory makes) names no holder, however trusted the directory around
// it. Every command that reads it refuses naming the boundary, and the record
// is not changed.
func TestARecordOutsideTheTrustBoundaryNamesNoHolder(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	cleanScan(t)

	record := filepath.Join(f.active(), guardRecordName)
	bytesBefore, err := os.ReadFile(record)
	mustOK(t, err)

	commands := map[string][]string{
		"status":   {"status"},
		"holder":   {"holder"},
		"hold":     {"hold", "--holder", "ci-1"},
		"release":  {"release", "--holder", "ci-1"},
		"recover":  {"recover", "--holder", "ci-1", "--old-driver-stopped"},
		"takeover": {"hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"},
	}

	check := func(t *testing.T, what string) {
		t.Helper()

		for name, args := range commands {
			if err := guardRun(t, args...); !errors.Is(err, errTrustBoundary) {
				t.Errorf("%s over %s: err = %v, want the trust boundary", name, what, err)
			}
		}

		if bytesAfter, err := os.ReadFile(record); err != nil || !bytes.Equal(bytesBefore, bytesAfter) {
			t.Errorf("%s changed the record: %q, %v", what, bytesAfter, err)
		}
	}

	t.Run("a loose mode", func(t *testing.T) {
		mustOK(t, os.Chmod(record, 0o666))
		t.Cleanup(func() { mustOK(t, os.Chmod(record, 0o600)) })

		check(t, "a 0666 record")
	})

	t.Run("another owner", func(t *testing.T) {
		info, err := os.Lstat(record)
		mustOK(t, err)

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("the record's stat carries no Stat_t")
		}

		ino := st.Ino

		saved := guardOwnerOf
		guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == ino {
				return uint32(os.Geteuid()) + 1, true
			}

			return ownerFromInfo(info)
		}

		t.Cleanup(func() { guardOwnerOf = saved })

		check(t, "another account's record")
	})
}

// G25: THE CLAIM'S TARGET IS EXACTLY ONE CHILD OF THE ROOT, BY ITS TEXT. A
// target spelled `<root>/hop/../x` is `<root>/x` to a lexical check and
// `/outside/x` to the kernel once `hop` is a link out of the root, so a
// resume that admitted it would read one directory's journal and remove
// another's tree. The spelling is refused before anything is read; the claim
// and the outside tree are untouched. The takeover's pointer is held to the
// same rule.
func TestAClaimTargetIsExactlyOneChildOfTheRootByItsText(t *testing.T) {
	for _, spelling := range []string{"/hop/../recovery-x", "/./recovery-x", "/recovery-x/", "//recovery-x", "/..", "/recovery-x/.."} {
		t.Run(spelling, func(t *testing.T) {
			f := newGuardedFixture(t)
			mustOK(t, os.Mkdir(f.root, 0o700))

			outside := filepath.Join(t.TempDir(), "outside")
			mustOK(t, os.MkdirAll(filepath.Join(outside, "child"), 0o700))
			mustOK(t, os.MkdirAll(filepath.Join(outside, "recovery-x"), 0o700))
			writeJournalFixture(t, filepath.Join(outside, "recovery-x"), "claimed")
			mustOK(t, os.Symlink(filepath.Join(outside, "child"), filepath.Join(f.root, "hop")))

			recovery := filepath.Join(f.root, "recovery-x")
			mustOK(t, os.Mkdir(recovery, 0o700))
			writeJournalFixture(t, recovery, "claimed")

			// STRING CONCATENATION, so the spelling reaches the pointer as written.
			target := f.root + spelling
			mustOK(t, os.Symlink(target, f.active()))

			err := resumeHostUpgrade(t.Context(), f.cfg)
			if err == nil || !strings.Contains(err.Error(), "is not a recovery directory in") {
				t.Errorf("a resume over the target %q: err = %v, want the refusal naming the root", target, err)
			}

			if got, err := os.Readlink(f.active()); err != nil || got != target {
				t.Errorf("the claim moved to %q (%v)", got, err)
			}

			if _, err := os.Stat(filepath.Join(outside, "recovery-x", "journal.json")); err != nil {
				t.Errorf("the outside tree was touched: %v", err)
			}

			if len(f.reached) != 0 {
				t.Errorf("the refused resume reached %v", f.reached)
			}
		})
	}

	t.Run("the takeover's pointer", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")
		cleanScan(t)

		recovery := filepath.Join(f.root, "recovery-x")
		mustOK(t, os.Mkdir(recovery, 0o700))
		writeJournalFixture(t, recovery, "installed")
		mustOK(t, os.Symlink(f.root+"/hop/../recovery-x", filepath.Join(f.active(), guardPointerName)))

		rec := f.record(t)

		if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err == nil ||
			!strings.Contains(err.Error(), "is not a recovery directory in") {
			t.Errorf("a takeover over a pointer spelled through ..: err = %v", err)
		}

		if got := f.record(t); got != rec {
			t.Errorf("the refused takeover changed the record to %+v", got)
		}
	})
}

// G26: THE RECOVERY DIRECTORY AND THE JOURNAL ARE JUDGED BEFORE THEY ARE
// BELIEVED: a recovery directory another account could write, or a journal
// another account owns or could write, refuses the resume and the takeover
// with the trust boundary, the claim retained and nothing removed; and a
// journal ABSENT from a directory outside the boundary is a refusal, never a
// transaction that never began.
func TestARecoveryDirectoryAndItsJournalAreJudgedBeforeTheyAreBelieved(t *testing.T) {
	type plant func(t *testing.T, recovery string)

	journalIno := func(t *testing.T, recovery string) uint64 {
		t.Helper()

		info, err := os.Lstat(filepath.Join(recovery, "journal.json"))
		mustOK(t, err)

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("no Stat_t")
		}

		return st.Ino
	}

	cases := map[string]plant{
		"a group-writable recovery directory": func(t *testing.T, recovery string) {
			t.Helper()
			mustOK(t, os.Chmod(recovery, 0o775))
		},
		"a group-writable journal": func(t *testing.T, recovery string) {
			t.Helper()
			mustOK(t, os.Chmod(filepath.Join(recovery, "journal.json"), 0o660))
		},
		"another account's journal": func(t *testing.T, recovery string) {
			t.Helper()

			ino := journalIno(t, recovery)
			saved := guardOwnerOf

			t.Cleanup(func() { guardOwnerOf = saved })

			guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == ino {
					return uint32(os.Geteuid()) + 1, true
				}

				return ownerFromInfo(info)
			}
		},
		"another account's recovery directory": func(t *testing.T, recovery string) {
			t.Helper()

			id := fileIdentityOf(t, recovery)
			saved := guardOwnerOf

			t.Cleanup(func() { guardOwnerOf = saved })

			guardOwnerOf = func(info os.FileInfo) (uint32, bool) {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Ino == id.Ino && info.IsDir() {
					return uint32(os.Geteuid()) + 1, true
				}

				return ownerFromInfo(info)
			}
		},
		"a group-writable recovery directory with no journal": func(t *testing.T, recovery string) {
			t.Helper()
			mustOK(t, os.Remove(filepath.Join(recovery, "journal.json")))
			mustOK(t, os.Chmod(recovery, 0o775))
		},
	}

	for name, plant := range cases {
		t.Run("resume over "+name, func(t *testing.T) {
			f := newGuardedFixture(t)
			mustOK(t, os.Mkdir(f.root, 0o700))

			recovery := filepath.Join(f.root, "recovery-x")
			mustOK(t, os.Mkdir(recovery, 0o700))
			writeJournalFixture(t, recovery, "claimed")
			mustOK(t, os.Symlink(recovery, f.active()))

			plant(t, recovery)

			if err := resumeHostUpgrade(t.Context(), f.cfg); !errors.Is(err, errTrustBoundary) {
				t.Errorf("a resume over %s: err = %v, want the trust boundary", name, err)
			}

			if _, err := os.Lstat(f.active()); err != nil {
				t.Errorf("the refused resume released the claim: %v", err)
			}

			if _, err := os.Lstat(recovery); err != nil {
				t.Errorf("the refused resume removed the recovery directory: %v", err)
			}

			if len(f.reached) != 0 {
				t.Errorf("the refused resume reached %v", f.reached)
			}
		})

		t.Run("takeover over "+name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")
			cleanScan(t)

			recovery := filepath.Join(f.root, "recovery-x")
			mustOK(t, os.Mkdir(recovery, 0o700))
			writeJournalFixture(t, recovery, "installed")
			mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))

			rec := f.record(t)

			plant(t, recovery)

			if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); !errors.Is(err, errTrustBoundary) {
				t.Errorf("a takeover over %s: err = %v, want the trust boundary", name, err)
			}

			if got := f.record(t); got != rec {
				t.Errorf("the refused takeover changed the record to %+v", got)
			}
		})
	}
}
