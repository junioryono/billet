package firecracker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The pid checks read /proc, so they can only be exercised where /proc is — which
// is where this backend runs. Elsewhere pidIsVMM answers "gone" for everything,
// which is the fail-closed direction: Destroy signals nothing rather than signalling
// a number it cannot check.
func requireProc(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skip("no /proc on this platform, so a pid cannot be tied to a microVM")
	}
}

// A PID IS ONLY THIS MICROVM'S IF ITS COMMAND LINE SAYS SO.
//
// A pid file holds a number, and the kernel reuses numbers. This backend signals
// as root, so "the file said 4321" is not grounds to signal 4321 — the jailer's
// `--id` in the argument vector is.
func TestAPidIsOnlyTheMicroVMIfItsCommandLineSaysSo(t *testing.T) {
	t.Parallel()
	requireProc(t)

	marker := "billet-" + strings.Repeat("a", leaseIDLength)

	// A REAL PROCESS CARRYING THE MARKER IN ITS ARGV, which is exactly the shape
	// the jailer produces.
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	cmd.Args = []string{"sleep", "--id", marker, "30"}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start a stand-in vmm: %v", err)
	}

	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "finished") {
			t.Errorf("kill the stand-in: %v", err)
		}

		//nolint:errcheck // the wait is only here so the child is reaped
		_ = cmd.Wait()
	})

	pid := cmd.Process.Pid

	owns, err := pidIsVMM(pid, marker)
	if err != nil {
		t.Fatalf("pidIsVMM: %v", err)
	}

	if !owns {
		t.Error("a process carrying the jail id in its argv was not recognised as the microVM")
	}

	// AND A DIFFERENT ID IS NOT THIS ONE. Without this the check passes for every
	// process on the machine, which is the failure it exists to prevent.
	other, err := pidIsVMM(pid, "billet-"+strings.Repeat("b", leaseIDLength))
	if err != nil {
		t.Fatalf("pidIsVMM: %v", err)
	}

	if other {
		t.Error("a process was claimed by a jail id it does not carry")
	}
}

// AND A PARTIAL MATCH IS NOT A MATCH. Arguments are NUL-separated, and lease ids
// are fixed-length hex — so a substring search over the raw bytes would let one
// jail id claim a process belonging to another whose id merely contains it.
func TestAJailIdMustMatchAWholeArgument(t *testing.T) {
	t.Parallel()
	requireProc(t)

	full := "billet-" + strings.Repeat("c", leaseIDLength)

	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	cmd.Args = []string{"sleep", "--id", full, "30"}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start a stand-in vmm: %v", err)
	}

	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "finished") {
			t.Errorf("kill the stand-in: %v", err)
		}

		//nolint:errcheck // the wait is only here so the child is reaped
		_ = cmd.Wait()
	})

	owns, err := pidIsVMM(cmd.Process.Pid, full[:len(full)-4])
	if err != nil {
		t.Fatalf("pidIsVMM: %v", err)
	}

	if owns {
		t.Error("a prefix of a jail id claimed a process that carries the full one")
	}
}

// A PID THAT IS GONE IS GONE, which is the ordinary answer and the one teardown
// depends on to proceed.
func TestAnExitedPidIsNotTheMicroVM(t *testing.T) {
	t.Parallel()
	requireProc(t)

	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a process that exits: %v", err)
	}

	owns, err := pidIsVMM(cmd.Process.Pid, "billet-"+strings.Repeat("d", leaseIDLength))
	if err != nil {
		t.Fatalf("pidIsVMM: %v", err)
	}

	if owns {
		t.Error("an exited pid was reported as a running microVM")
	}
}

// DESTROY ACTUALLY STOPS THE PROCESS.
//
// The API has no way to kill a microVM — its only shutdown action is a keyboard
// event the guest may ignore, measured against a real guest that ignored it for
// twenty seconds — so this asserts the thing that replaced it.
func TestDestroySignalsTheVMMItself(t *testing.T) {
	requireProc(t)

	h := newHarness(t)

	var pid int

	h.onJailer = func(id string) {
		h.serveVMM(t, id)

		// THE JAILER WRITES THIS, so the fake does too — and it points at a real
		// process carrying the jail id, which is what Destroy has to be able to
		// prove before it signals anything.
		cmd := exec.CommandContext(t.Context(), "sleep", "60")
		cmd.Args = []string{"sleep", "--id", id, "60"}

		if err := cmd.Start(); err != nil {
			t.Errorf("start a stand-in vmm: %v", err)

			return
		}

		pid = cmd.Process.Pid

		go func() {
			//nolint:errcheck // reaping the child; the assertion is on whether it died
			_ = cmd.Wait()
		}()

		j := h.p.jailFor(id)
		if err := os.WriteFile(j.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Errorf("write the pid file: %v", err)
		}
	}

	if _, err := h.p.Launch(t.Context(), aSpec()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if pid == 0 {
		t.Fatal("no stand-in vmm was started")
	}

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// The process is gone, checked the way Destroy itself checks.
	owns, err := pidIsVMM(pid, theInstance)
	if err != nil {
		t.Fatalf("pidIsVMM after Destroy: %v", err)
	}

	if owns {
		t.Error("Destroy returned while the vmm process was still running")
	}
}

// A PID FILE THAT IS NOT A PID IS AN ERROR, NOT A ZERO. Reading it as "nothing is
// running" would let teardown go on to unmap a block device a live VMM still holds.
func TestAPidFileThatIsNotAPidStopsTeardown(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	if err := os.WriteFile(j.pidFile(), []byte("not a number"), 0o600); err != nil {
		t.Fatalf("stage a corrupt pid file: %v", err)
	}

	if err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Error("Destroy proceeded although it could not tell whether the vmm was running")
	}
}

// AND AN ABSENT PID FILE IS "NOTHING IS RUNNING", which is the idempotent case
// teardown runs into constantly — a second Destroy, or one after a launch that
// never got as far as starting a VMM.
func TestAnAbsentPidFileIsNotAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	// The ordinary harness never writes one — only the jailer does, and this
	// harness's jailer is a recorder.
	if _, err := os.Stat(h.p.jailFor(theInstance).pidFile()); err == nil {
		t.Fatal("the harness wrote a pid file, so its absence is not what is being tested")
	}

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Errorf("Destroy refused a jail with no vmm behind it: %v", err)
	}
}

// THE PID FILE IS NAMED AFTER THE RESOLVED BINARY, like the chroot directory and
// for the same measured reason. Looking for `firecracker.pid` beside a jailer that
// wrote `firecracker-v1.16.1.pid` finds nothing, which reads as "already stopped"
// — so Destroy would unmap the block device of a running guest.
func TestThePidFileIsNamedAfterTheResolvedBinary(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if base := filepath.Base(h.p.jailFor(theInstance).pidFile()); base != "firecracker-v1.16.1.pid" {
		t.Errorf("billet looks for the pid at %q, not where the jailer writes it", base)
	}
}

// A SIGNAL TO A PROCESS THAT HAS ALREADY EXITED IS SUCCESS. It is the outcome the
// caller wanted, and losing that race is the ordinary case rather than an unusual
// one — the check and the signal cannot be atomic.
func TestSignallingAnAlreadyExitedProcessIsSuccess(t *testing.T) {
	t.Parallel()
	requireProc(t)

	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a process that exits: %v", err)
	}

	if err := signalVMM(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Errorf("signalling an exited process was reported as a failure: %v", err)
	}
}
