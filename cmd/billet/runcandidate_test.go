package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/hostupgrade"
)

// usageHandler is what a candidate script prints for `-h`, the way every
// generation of the real binary does: its flags on stdout and exit zero. Whether
// the hold flag is among them is how the parent tells the two protocols apart,
// so each script declares its generation here and behaves accordingly below.
func usageHandler(listsHold bool) string {
	return usageHandlerThatAlso(listsHold, "")
}

// usageHandlerThatAlso is usageHandler with extra shell run inside the -h branch
// before it exits, for a candidate whose usage misbehaves.
func usageHandlerThatAlso(listsHold bool, extra string) string {
	hold := ""
	if listsHold {
		hold = "  echo '  -" + holdFlagName + "'\n"
	}

	return "if [ \"$2\" = -h ]; then\n" +
		"  echo 'Usage of billet server:'\n" +
		"  echo '  -upgrade-probe'\n" + hold + extra +
		"  exit 0\n" +
		"fi\n"
}

// candidateScript stands the installed binary in for one test: a shell script
// that behaves the way one generation of probe does. EXECUTED, not modelled,
// because the defect this covers lived in the relationship between two real
// processes and a fake of either half would have carried the same assumption.
func candidateScript(t *testing.T, listsHold bool, body string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet")
	script := "#!/bin/sh\n" + usageHandler(listsHold) + body + "\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	previous := installedBinary
	installedBinary = path

	t.Cleanup(func() { installedBinary = previous })
}

// legacyCandidate is a release through v0.9.0: it does not list the hold flag.
func legacyCandidate(t *testing.T, body string) { t.Helper(); candidateScript(t, false, body) }

// fixedCandidate is a release from v0.9.1: it lists the hold flag.
func fixedCandidate(t *testing.T, body string) { t.Helper(); candidateScript(t, true, body) }

func shortDeadline(t *testing.T, d time.Duration) {
	t.Helper()

	previous := candidateProbeDeadline
	candidateProbeDeadline = d

	t.Cleanup(func() { candidateProbeDeadline = previous })
}

func shortOutputGrace(t *testing.T, d time.Duration) {
	t.Helper()

	previous := probeOutputGrace
	probeOutputGrace = d

	t.Cleanup(func() { probeOutputGrace = previous })
}

func recordedPID(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the candidate did not record the pid: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("recorded pid %q: %v", raw, err)
	}

	return pid
}

// killLater kills a process the candidate left behind, because the parent will
// not: it signals nothing by number. A kill that finds nobody is the outcome
// wanted.
func killLater(t *testing.T, pid int) {
	t.Helper()

	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("killing the leftover process %d: %v", pid, err)
		}
	})
}

// A RELEASE THROUGH v0.9.0 SAYS IT IS READY AND THEN WAITS TO BE STOPPED. A parent
// that only waited for exit sat behind it forever with the services stopped; this
// one reads the line, knows from the candidate's own usage that the line means
// "stop me", stops it, and moves on.
func TestRunCandidateStopsALegacyProbeThatReportsReadyAndWaits(t *testing.T) {
	legacyCandidate(t, "echo '"+serverProbeReadyLine+"'\nexec sleep 60")

	started := time.Now()

	if err := (&ledgerHost{}).runCandidate(t.Context(), "server"); err != nil {
		t.Fatalf("a legacy ready probe was refused: %v", err)
	}

	if took := time.Since(started); took > 15*time.Second {
		t.Fatalf("a legacy ready probe held the transaction for %s; it should have been stopped "+
			"at once", took)
	}
}

// A RELEASE FROM v0.9.1 SAYS NOTHING AND EXITS ZERO, and that is the whole answer.
func TestRunCandidateAcceptsAFixedProbeThatExitsZero(t *testing.T) {
	fixedCandidate(t, "exit 0")

	if err := (&ledgerHost{}).runCandidate(t.Context(), "server"); err != nil {
		t.Fatalf("a probe that exited zero was refused: %v", err)
	}
}

// A REFUSAL IS A REFUSAL, and its words reach the operator.
func TestRunCandidateFailsAProbeThatRefuses(t *testing.T) {
	fixedCandidate(t, "echo 'cannot open what it inherited: watermark' >&2\nexit 3")

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a probe that exited 3 passed")
	}

	if !strings.Contains(err.Error(), "watermark") {
		t.Fatalf("the refusal's words did not reach the error: %v", err)
	}
}

// ALL OF A REFUSAL'S WORDS REACH THE OPERATOR, not the part that happened to be
// copied before the exit was noticed: the verdict waits for the output to close.
func TestRunCandidateKeepsTheWholeRefusal(t *testing.T) {
	fixedCandidate(t, "i=0; while [ $i -lt 400 ]; do echo \"refusal line $i of the candidate's account\" >&2; "+
		"i=$((i+1)); done\necho 'LAST WORD: the ledger is fenced' >&2\nexit 3")

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a probe that exited 3 passed")
	}

	if !strings.Contains(err.Error(), "LAST WORD: the ledger is fenced") {
		t.Fatalf("the refusal's last line was lost to the race between exit and drain: %.200s...",
			err.Error())
	}
}

// A FIXED CANDIDATE THAT PRINTS THE READINESS LINE HAS BROKEN ITS OWN PROTOCOL,
// and is refused for it whether it then holds or exits: no stop is sent, because a
// stop is no verdict and this needs one. Deterministic, with no timer to race.
func TestRunCandidateRefusesAFixedProbeThatPrintsTheLineAndHolds(t *testing.T) {
	fixedCandidate(t, "echo '"+serverProbeReadyLine+"'\nexec sleep 60")
	shortDeadline(t, 2*time.Second)
	shortOutputGrace(t, 2*time.Second)

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a fixed candidate that printed the readiness line passed")
	}

	if !strings.Contains(err.Error(), "without being told to hold") {
		t.Fatalf("the broken protocol was misread: %v", err)
	}
}

func TestRunCandidateRefusesAFixedProbeThatPrintsTheLineAndExits(t *testing.T) {
	fixedCandidate(t, "echo '"+serverProbeReadyLine+"'\nexit 3")

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a fixed candidate that printed the readiness line and exited 3 passed")
	}

	if !strings.Contains(err.Error(), "without being told to hold") ||
		!strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the broken protocol was misread: %v", err)
	}
}

// AND A CLEAN EXIT DOES NOT LAUNDER THE LINE. The line is read from the closed
// output after the candidate has gone, not from which channel a select happened
// to pick while it ran, so a candidate that printed it and exited zero in the
// same instant is refused every time, which is what running it many times shows.
func TestRunCandidateRefusesAFixedProbeThatPrintsTheLineAndExitsZero(t *testing.T) {
	fixedCandidate(t, "echo '"+serverProbeReadyLine+"'\nexit 0")

	for i := range 10 {
		err := (&ledgerHost{}).runCandidate(t.Context(), "server")
		if err == nil {
			t.Fatalf("run %d: a fixed candidate that printed the readiness line and exited 0 passed", i)
		}

		if !strings.Contains(err.Error(), "without being told to hold") {
			t.Fatalf("run %d: the broken protocol was misread: %v", i, err)
		}
	}
}

// THE WORDS INSIDE A REFUSAL ARE NOT THE ANSWER. The readiness line is matched as
// the whole line a probe prints, so an error that quotes it cannot pass.
func TestRunCandidateIgnoresTheWordsInsideARefusal(t *testing.T) {
	fixedCandidate(t, "echo 'never reached "+upgradeProbeReady+" because the ledger is fenced' >&2\n"+
		"exec sleep 60")
	shortDeadline(t, 2*time.Second)

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a refusal that quoted the readiness words passed as readiness")
	}

	if !strings.Contains(err.Error(), "neither ready nor finished") {
		t.Fatalf("the quoted words were read as readiness: %v", err)
	}
}

// A LINE THAT BEGINS WITH THE SENTENCE AND GOES ON IS NOT THE SENTENCE.
func TestRunCandidateIgnoresAReadinessPrefix(t *testing.T) {
	fixedCandidate(t, "echo '"+serverProbeReadyLine+" but initialisation then failed'\nexec sleep 60")
	shortDeadline(t, 2*time.Second)

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a line that merely began with the readiness sentence passed as readiness")
	}

	if !strings.Contains(err.Error(), "neither ready nor finished") {
		t.Fatalf("the prefix was read as readiness: %v", err)
	}
}

// A PROBE THAT NEITHER ANSWERS NOR EXITS IS A COULD-NOT-TELL, and could-not-tell
// fails the probe inside the deadline rather than holding the host forever.
func TestRunCandidateGivesUpOnASilentProbe(t *testing.T) {
	fixedCandidate(t, "exec sleep 60")
	shortDeadline(t, 500*time.Millisecond)

	started := time.Now()

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a silent probe passed")
	}

	if !strings.Contains(err.Error(), "neither ready nor finished") {
		t.Fatalf("a silent probe failed for the wrong reason: %v", err)
	}

	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("a silent probe held the transaction for %s past a half-second deadline", took)
	}
}

// A LEGACY PROBE THAT IGNORES SIGTERM IS KILLED, and a kill this probe sent is no
// verdict either.
func TestRunCandidateKillsALegacyProbeThatIgnoresTheStop(t *testing.T) {
	legacyCandidate(t, "trap '' TERM\necho '"+serverProbeReadyLine+"'\nwhile :; do sleep 1; done")

	previous := probeStopGrace
	probeStopGrace = 500 * time.Millisecond

	t.Cleanup(func() { probeStopGrace = previous })

	started := time.Now()

	if err := (&ledgerHost{}).runCandidate(t.Context(), "server"); err != nil {
		t.Fatalf("a legacy probe that had to be killed was refused: %v", err)
	}

	if took := time.Since(started); took > 15*time.Second {
		t.Fatalf("killing a legacy probe took %s", took)
	}
}

// A CHILD THAT KEEPS THE CANDIDATE'S OUTPUT CORDONS THE HOST, and is not hunted:
// the parent signals nothing by number, so a process it did not start is not one
// it will kill; and the open output proves that process is alive, so the
// transaction may not restore the ledger over it. The test kills the child
// afterwards.
func TestRunCandidateRefusesAProbeWhoseChildHoldsItsOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("BILLET_TEST_GRANDCHILD_PIDFILE", pidFile)

	legacyCandidate(t, "sleep 60 &\necho $! > \"$BILLET_TEST_GRANDCHILD_PIDFILE\"\n"+
		"echo '"+serverProbeReadyLine+"'\nexec sleep 60")
	shortOutputGrace(t, 2*time.Second)

	// THE CHILD KEEPS THE OUTPUT OPEN, so the stop is followed by the whole stop
	// grace before the kill and the whole output grace before the verdict;
	// shortened here so the test does not sit through them.
	previousStop := probeStopGrace
	probeStopGrace = 500 * time.Millisecond

	t.Cleanup(func() { probeStopGrace = previousStop })

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")

	killLater(t, recordedPID(t, pidFile))

	if err == nil {
		t.Fatal("a probe whose child held its output open passed")
	}

	if !strings.Contains(err.Error(), "holding its output") {
		t.Fatalf("the held output was misread: %v", err)
	}

	if !errors.Is(err, hostupgrade.ErrUnsafeToRestore) {
		t.Fatalf("a live descendant was reported as an ordinary refusal, which rolls back over "+
			"it: %v", err)
	}
}

// A CANDIDATE THAT CANNOT PRINT ITS OWN USAGE IS REFUSED BEFORE IT IS PROBED, and
// the refusal names the question it could not answer.
func TestRunCandidateRefusesACandidateWhoseUsageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'no such command' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	previous := installedBinary
	installedBinary = path

	t.Cleanup(func() { installedBinary = previous })

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a candidate whose -h exited 2 was probed and passed")
	}

	if !strings.Contains(err.Error(), "usage could not be read") {
		t.Fatalf("the usage failure was misread: %v", err)
	}
}

// A RELEASE THROUGH v0.9.0 PROVES READINESS ONLY BY SAYING SO. One that exits zero
// without the line has proved nothing, and a zero here is a could-not-tell, which
// never passes.
func TestRunCandidateRefusesALegacyProbeThatExitsWithoutTheLine(t *testing.T) {
	legacyCandidate(t, "exit 0")

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")
	if err == nil {
		t.Fatal("a legacy candidate that exited zero without the readiness line passed")
	}

	if !strings.Contains(err.Error(), "proves readiness only by printing") {
		t.Fatalf("the silent exit was misread: %v", err)
	}
}

// THE USAGE QUESTION IS BOUNDED TOO. A candidate whose -h leaves a process on its
// output would otherwise hold the transaction before the probe even ran, with the
// services already stopped. The test kills the holder afterwards.
func TestRunCandidateRefusesACandidateWhoseUsageLeavesAProcessOnItsOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Setenv("BILLET_TEST_GRANDCHILD_PIDFILE", pidFile)

	path := filepath.Join(t.TempDir(), "billet")
	script := "#!/bin/sh\n" + usageHandlerThatAlso(true,
		"  sleep 60 &\n  echo $! > \"$BILLET_TEST_GRANDCHILD_PIDFILE\"\n") + "exit 0\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	previous, previousDelay := installedBinary, probeUsageWaitDelay
	installedBinary, probeUsageWaitDelay = path, 1*time.Second

	t.Cleanup(func() { installedBinary, probeUsageWaitDelay = previous, previousDelay })

	started := time.Now()

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")

	killLater(t, recordedPID(t, pidFile))

	if err == nil {
		t.Fatal("a candidate whose -h left a process on its output was probed and passed")
	}

	if !strings.Contains(err.Error(), "still holds its output") {
		t.Fatalf("the held usage output was misread: %v", err)
	}

	if !errors.Is(err, hostupgrade.ErrUnsafeToRestore) {
		t.Fatalf("a live descendant of the usage was reported as an ordinary refusal: %v", err)
	}

	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("the usage question held the transaction for %s past a one-second bound", took)
	}
}

// AND A FAILED USAGE WITH SOMETHING STILL ALIVE BEHIND IT CORDONS TOO. The
// non-zero exit is the smaller fact; the live descendant is the one the
// transaction has to act on.
func TestRunCandidateCordonsWhenAFailedUsageLeavesAProcessOnItsOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Setenv("BILLET_TEST_GRANDCHILD_PIDFILE", pidFile)

	path := filepath.Join(t.TempDir(), "billet")
	script := "#!/bin/sh\nif [ \"$2\" = -h ]; then\n  sleep 60 &\n" +
		"  echo $! > \"$BILLET_TEST_GRANDCHILD_PIDFILE\"\n  echo 'no such command' >&2\n  exit 2\nfi\nexit 0\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	previous, previousDelay := installedBinary, probeUsageWaitDelay
	installedBinary, probeUsageWaitDelay = path, 1*time.Second

	t.Cleanup(func() { installedBinary, probeUsageWaitDelay = previous, previousDelay })

	err := (&ledgerHost{}).runCandidate(t.Context(), "server")

	killLater(t, recordedPID(t, pidFile))

	if !errors.Is(err, hostupgrade.ErrUnsafeToRestore) {
		t.Fatalf("a failed usage with a live descendant did not cordon: %v", err)
	}
}
