package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A SUPERSEDED INSTRUCTION IS REFUSED, AND THE CLAIM CANNOT DO THAT JOB.
//
// The active claim is released the moment an upgrade commits or completes a
// rollback, so it bounds concurrency and nothing else: a delayed instruction from
// a decision the fleet has replaced, arriving one second after the newer one
// finished, finds nothing in its way and dutifully installs the release the
// operator moved away from. The high-water mark is what remains.
func TestAnInstructionOlderThanTheOneThisMachineActedOnIsRefused(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(7); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	err := checkDecision(hostUpgradeTarget{generation: 5})
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("an instruction from decision 5 on a machine that acted on 7 returned "+
			"%v, want ErrSuperseded", err)
	}

	// AND IT NAMES BOTH, because the operator reading this is deciding whether
	// their fleet has two rollouts in flight or one host is behind.
	for _, want := range []string{"7", "5", "nothing was installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// THE SAME DECISION ARRIVING TWICE IS NOT SUPERSEDED. Instructions are delivered
// over a network and retried; refusing a redelivery would turn ordinary
// unreliability into a host the rollout cannot move.
func TestARedeliveryOfTheSameDecisionIsAllowed(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(7); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	if err := checkDecision(hostUpgradeTarget{generation: 7}); err != nil {
		t.Errorf("a redelivery of decision 7 was refused: %v", err)
	}

	// AND A NEWER ONE, obviously — that is the case the fence exists to let through.
	if err := checkDecision(hostUpgradeTarget{generation: 8}); err != nil {
		t.Errorf("a newer decision was refused: %v", err)
	}
}

// AN OPERATOR'S OWN RUN CARRIES NO GENERATION AND IS NEVER FENCED. The mark is a
// fleet mechanism; requiring it on the one path with no fleet behind it would
// leave a person unable to fix the machine the rollout got stuck on.
func TestAnUnfencedRunIsNeverSuperseded(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(99); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	if err := checkDecision(hostUpgradeTarget{}); err != nil {
		t.Errorf("an operator's own run was refused as superseded: %v", err)
	}
}

// THE MARK NEVER WALKS BACKWARDS. A retry of an older decision that got past the
// check for some other reason must not lower the fence behind it.
func TestTheDecisionMarkOnlyMovesForward(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(7); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	if err := recordDecision(3); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	got, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if got != 7 {
		t.Errorf("the mark is %d after an older decision was recorded, want 7", got)
	}
}

// A MACHINE WITH NO MARK REFUSES NOTHING. Every host has none before its first
// fenced upgrade, and one that refused to upgrade because it could not read its
// own bookkeeping is a host no rollout can ever fix.
func TestAMachineWithNoRecordedDecisionRefusesNothing(t *testing.T) {
	withUpgradeRoot(t)

	got, err := readDecision()
	if err != nil {
		t.Fatalf("a fresh machine could not read its own absent mark: %v", err)
	}

	if got != 0 {
		t.Fatalf("a fresh machine reports decision %d, want 0", got)
	}

	if err := checkDecision(hostUpgradeTarget{generation: 1}); err != nil {
		t.Errorf("a first fenced upgrade was refused: %v", err)
	}
}

// A FENCE BILLET CANNOT READ REFUSES A FENCED INSTRUCTION.
//
// Three answers, not two, and collapsing them was a defect. "This machine has
// never taken a fenced upgrade" and "this machine's fence is not working" both
// used to answer zero — so a stale instruction could overwrite the only evidence
// that a newer decision existed and install a release the fleet had left behind.
func TestAnUnreadableFenceRefusesAFencedInstruction(t *testing.T) {
	withUpgradeRoot(t)

	if err := os.WriteFile(decisionPath(), []byte("not a number\n"), 0o600); err != nil {
		t.Fatalf("write a corrupt mark: %v", err)
	}

	err := checkDecision(hostUpgradeTarget{generation: 5})
	if !errors.Is(err, ErrUnreadableDecision) {
		t.Fatalf("a fenced instruction against an unreadable mark returned %v, want "+
			"ErrUnreadableDecision", err)
	}

	if !strings.Contains(err.Error(), decisionPath()) {
		t.Errorf("the refusal does not name the file to look at: %v", err)
	}

	// AND IT REFUSES TO RECORD OVER IT, because writing would destroy the very
	// evidence that says a newer decision may exist.
	if err := recordDecision(5); !errors.Is(err, ErrUnreadableDecision) {
		t.Errorf("recording over an unreadable mark returned %v, want "+
			"ErrUnreadableDecision", err)
	}

	// THE OPERATOR PATH STILL WORKS, which is what stops this wedging a machine.
	// A person running `billet host-upgrade` carries no generation, is not fenced,
	// and is exactly who the message above is addressed to.
	if err := checkDecision(hostUpgradeTarget{}); err != nil {
		t.Errorf("an operator's own run was refused over an unreadable fence: %v", err)
	}
}

// DOING NOTHING IS STILL ACTING ON A DECISION.
//
// When the requested release is already installed the updater has nothing to do
// and says so — but it used to say so BEFORE touching the fence, so a machine
// could satisfy decision 10 without raising its mark. A delayed decision 9 then
// passed a stale fence and DOWNGRADED it. The no-op path raises the mark for the
// same reason the installing path does.
func TestANoOpUpgradeStillRaisesTheFence(t *testing.T) {
	withUpgradeRoot(t)

	// What the already-running branch does, in the order it does it.
	if err := recordDecision(10); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	if err := checkDecision(hostUpgradeTarget{generation: 9}); !errors.Is(err, ErrSuperseded) {
		t.Errorf("a decision older than one this machine satisfied by doing nothing "+
			"returned %v, want ErrSuperseded", err)
	}
}

// A CLAIM WITH NO JOURNAL IS RECOVERED RATHER THAN LEFT TO A PERSON.
//
// The journal is written before anything is staged, stopped or fenced, so a claim
// without one means nothing on the machine was touched — but it is also the worst
// state to be stuck in: `start` refuses because a claim exists and `--resume` has
// nothing to continue. A full disk, or power lost in that window, would take a
// host out of every future rollout until somebody deleted a symlink by hand.
func TestAResumeRecoversAClaimThatNeverWroteAJournal(t *testing.T) {
	withUpgradeRoot(t)

	dir, err := stageClaim()
	if err != nil {
		t.Fatalf("stageClaim: %v", err)
	}

	if err := publishClaim(dir); err != nil {
		t.Fatalf("publishClaim: %v", err)
	}

	if _, err := os.Stat(activePath()); err != nil {
		t.Fatalf("the claim was not taken: %v", err)
	}

	// No journal is written, which is the state under test.
	if err := resumeHostUpgrade(t.Context(), nil); err != nil {
		t.Fatalf("resuming a claim with no journal: %v", err)
	}

	if _, err := os.Lstat(activePath()); !os.IsNotExist(err) {
		t.Errorf("the claim survived a resume that found nothing to continue: %v", err)
	}

	// AND A FRESH UPGRADE CAN START AGAIN, which is the point of recovering it.
	fresh, err := stageClaim()
	if err != nil {
		t.Fatalf("stageClaim: %v", err)
	}

	if err := publishClaim(fresh); err != nil {
		t.Errorf("a new upgrade could not claim after the empty one was recovered: %v", err)
	}

	_ = dir
}

// THE MARK IS TOUCHED ONLY UNDER AN EXCLUSION THAT REALLY EXCLUDES.
//
// The read and the write are separate syscalls, and an atomic rename makes each
// write whole without making the pair atomic — so generation 10 and generation 9
// could both read a mark of 4, 10 could write 10, and 9 could then write 9 over
// it. A mark that can go backwards is a fence a superseded release walks through.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. It proves the operation genuinely takes
// the lock: held from outside, the call blocks, and it completes when the lock is
// released. It does NOT prove that a check and a record share ONE acquisition —
// splitting them into two would still pass, because observing an interleaving
// needs a second writer landing in a window this test cannot schedule, and a
// probabilistic test for it would be flaky rather than useful. That the pair is
// one acquisition is held by checkAndRecordDecision being the only caller-facing
// way to do both; keep it that way.
func TestTheDecisionMarkIsTouchedUnderALockThatExcludes(t *testing.T) {
	withUpgradeRoot(t)

	// Take the same lock from a separate descriptor. Two opens of one file conflict
	// even inside one process; a second flock on the SAME descriptor would not,
	// which is why nothing here caches one.
	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		t.Fatalf("prepare the upgrade root: %v", err)
	}

	held, err := os.OpenFile(filepath.Join(upgradeRoot, decisionLockName),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open the decision lock: %v", err)
	}

	defer func() { _ = held.Close() }()

	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("take the decision lock: %v", err)
	}

	done := make(chan error, 1)

	go func() { done <- checkAndRecordDecision(hostUpgradeTarget{generation: 5}) }()

	select {
	case err := <-done:
		t.Fatalf("the mark was touched while another holder had the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release the decision lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the mark could not be touched once the lock was free: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the mark was never touched after the lock was released")
	}

	got, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if got != 5 {
		t.Errorf("the mark is %d, want 5", got)
	}
}

// A FLEET INSTRUCTION MUST CARRY A USABLE GENERATION, OR IT IS NOT ONE.
//
// The mark protects only fenced instructions, and everything it protects rests on
// "generation > 0 means this came from a rollout". That inference is sound only
// while the two modes cannot be mixed: an instruction naming a rollout with no
// generation is neither an operator at the keyboard nor a fleet decision, and it
// would install over an arbitrarily high mark while looking like the former.
func TestAFleetInstructionWithoutAGenerationIsRefused(t *testing.T) {
	t.Parallel()

	err := checkFleetInstruction("rollout-abc", 0)
	if err == nil {
		t.Fatal("an instruction naming a rollout with no generation was accepted")
	}

	if !strings.Contains(err.Error(), "rollout-abc") ||
		!strings.Contains(err.Error(), "superseded") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	// A NEGATIVE ONE IS NOT A SMALLER DECISION. Generations count up from one, so
	// this is a number no rollout produces.
	if err := checkFleetInstruction("", -1); err == nil {
		t.Error("a negative generation was accepted")
	}

	// AND BOTH LEGITIMATE MODES PASS: an operator with neither, a rollout with both.
	if err := checkFleetInstruction("", 0); err != nil {
		t.Errorf("an operator's own run was refused: %v", err)
	}

	if err := checkFleetInstruction("rollout-abc", 3); err != nil {
		t.Errorf("a properly fenced fleet instruction was refused: %v", err)
	}
}

// withUpgradeRoot points the upgrade bookkeeping at a directory this test owns.
func withUpgradeRoot(t *testing.T) {
	t.Helper()

	original := upgradeRoot
	upgradeRoot = t.TempDir()

	t.Cleanup(func() { upgradeRoot = original })
}
