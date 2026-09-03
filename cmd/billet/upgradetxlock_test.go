package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
)

// A LIVE UPDATER IS NOT AN ABANDONED ONE, AND THE CLAIM CANNOT TELL THEM APART.
//
// The claim is a durable pointer that survives a crash — that is its whole job,
// because a machine that lost power mid-upgrade must still be able to find what
// it was running. Precisely because it survives, its presence cannot mean "a
// process is working on this now": `--resume` exists to pick up a claim whose
// owner is gone. So a resume run beside a live detached updater entered the same
// transaction, stopping services it was starting and advancing a journal it was
// writing. A flock is the opposite shape: the kernel drops it when the holder
// dies.
func TestAResumeRefusesWhileAnUpgradeIsRunning(t *testing.T) {
	withUpgradeRoot(t)

	tx, err := takeTxLock()
	if err != nil {
		t.Fatalf("takeTxLock: %v", err)
	}

	defer tx.release()

	err = resumeHostUpgrade(t.Context(), &config.Config{})
	if !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("a resume beside a live upgrade returned %v, want ErrUpgradeInProgress", err)
	}

	// AND IT SAYS WHY WAITING IS THE ANSWER, because the wait it is describing has
	// no bound — the transaction it collided with may be draining somebody's job.
	if !strings.Contains(err.Error(), "draining") {
		t.Errorf("the refusal does not explain the wait: %v", err)
	}

	// ONCE THE HOLDER IS GONE, A RESUME WORKS AGAIN. The lock is the liveness
	// signal; nothing durable is left behind by releasing it.
	tx.release()

	if err := resumeHostUpgrade(t.Context(), &config.Config{}); err != nil {
		t.Errorf("a resume after the upgrade ended: %v", err)
	}
}

// A TRANSACTION INTERRUPTED BEFORE ITS GENERATION WAS RECORDED RECORDS IT ON THE
// WAY BACK IN.
//
// The claim and journal are published before the mark is raised — deliberately,
// so a crash always leaves something resumable — which means a crash in that
// window leaves a resumable transaction the fence has never heard of. Finishing
// it without recording lets a delayed older instruction pass a stale mark
// afterwards and downgrade the host.
func TestAResumedTransactionRecordsTheDecisionItWasActingOn(t *testing.T) {
	withUpgradeRoot(t)

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.3.26", ToVersion: "v0.4.0", Generation: 7,
		Step: hostupgrade.StepStopped,
	})

	// THE UNIT THAT HOLDS THE PROPERTY, not the whole command. Driving this through
	// resumeHostUpgrade works and takes ten seconds, because it goes on to run the
	// real transaction against a machine that has no billet units — so the
	// assertion would be riding on how a systemctl call fails, which is neither the
	// property nor a thing this test should be pinned to.
	abandoned, err := settleResumedDecision(journal)
	if err != nil {
		t.Fatalf("settling the fence for a resumed transaction: %v", err)
	}

	if abandoned {
		t.Fatal("a transaction that had already stopped services was abandoned")
	}

	got, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if got != 7 {
		t.Errorf("the mark is %d after resuming a transaction for decision 7, want 7", got)
	}
}

// AND ONE THE FLEET HAS MOVED PAST, WHICH HAS TOUCHED NOTHING, IS ABANDONED.
//
// Finishing it would install a release the deployment has left behind. It never
// got as far as stopping anything, so abandoning costs nothing — and the claim
// has to go with it, or the machine is stuck refusing every future start.
func TestASupersededTransactionThatTouchedNothingIsAbandoned(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(9); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.3.26", ToVersion: "v0.4.0", Generation: 4,
		Step: hostupgrade.StepClaimed,
	})

	if err := resumeHostUpgrade(t.Context(), &config.Config{}); err != nil {
		t.Fatalf("abandoning a superseded transaction: %v", err)
	}

	if _, err := os.Lstat(activePath()); !os.IsNotExist(err) {
		t.Errorf("the claim survived an abandoned transaction: %v", err)
	}

	// AND THE DIRECTORY WENT WITH IT. releaseClaim keeps a recovery directory on
	// purpose — its journal is what an operator reads after a rollback, and it
	// holds the preserved binary and the ledger snapshot. A transaction that never
	// got past claiming has none of that, and a fleet retrying every few minutes
	// would leave one behind per superseded instruction.
	if _, err := os.Stat(journal.Dir); !os.IsNotExist(err) {
		t.Errorf("an abandoned transaction left its recovery directory behind: %v", err)
	}

	// THE MARK IS NOT LOWERED. What was abandoned is the transaction, not the
	// decision that superseded it.
	got, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if got != 9 {
		t.Errorf("the mark is %d after abandoning decision 4, want it left at 9", got)
	}
}

// A SUPERSEDED TRANSACTION THAT HAS ALREADY STOPPED SERVICES IS FINISHED, NOT
// ABANDONED. The host is half-applied; walking away leaves it down.
func TestASupersededTransactionThatStoppedServicesIsNotAbandoned(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(9); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.3.26", ToVersion: "v0.4.0", Generation: 4,
		Step: hostupgrade.StepStopped,
	})

	abandoned, err := settleResumedDecision(journal)
	if err != nil {
		t.Fatalf("settling the fence for a half-applied transaction: %v", err)
	}

	if abandoned {
		t.Fatal("a transaction that had already stopped services was abandoned; the host " +
			"is left down and nothing is coming back for it")
	}

	if _, err := os.Lstat(activePath()); err != nil {
		t.Errorf("a half-applied transaction's claim was released: %v", err)
	}
}

// stageJournal puts a recovery journal and its claim where a resume will find it.
func stageJournal(t *testing.T, journal *hostupgrade.Journal) *hostupgrade.Journal {
	t.Helper()

	dir, err := stageClaim()
	if err != nil {
		t.Fatalf("stageClaim: %v", err)
	}

	journal.Dir = dir

	if err := journal.Write(); err != nil {
		t.Fatalf("write the journal: %v", err)
	}

	if err := publishClaim(dir); err != nil {
		t.Fatalf("publishClaim: %v", err)
	}

	return journal
}

// A TRANSACTION THAT HAD STAGED IS NOT KNOWN TO HAVE TOUCHED NOTHING, AND MUST
// NOT BE ABANDONED.
//
// THIS IS THE MOST EXPENSIVE MISTAKE AVAILABLE IN THIS AREA, and the first
// version made it. A step records what COMPLETED, so the work after it is already
// in flight: preserving the installed binary, stopping the node, stopping the
// server and hiding the binary all run, and only then is `stopped` written. A
// journal sitting at `staged` may therefore describe a host with both services
// down and no billet on the path — and abandoning it releases the claim, leaving
// that host stopped with nothing on it that can be resumed and nothing anywhere
// recording why.
//
// `Reached(StepStopped)` reads exactly like the right question and is the wrong
// one. Only `claimed` is conclusive.
func TestASupersededTransactionThatHadStagedIsNotAbandoned(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(9); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.3.26", ToVersion: "v0.4.0", Generation: 4,
		Step: hostupgrade.StepStaged,
	})

	abandoned, err := settleResumedDecision(journal)
	if err != nil {
		t.Fatalf("settling the fence for a staged transaction: %v", err)
	}

	if abandoned {
		t.Fatal("a transaction that had staged was abandoned; the services it may " +
			"already have stopped are not coming back, and nothing is left to resume")
	}

	if _, err := os.Lstat(activePath()); err != nil {
		t.Errorf("a staged transaction's claim was released: %v", err)
	}

	if _, err := os.Stat(journal.Dir); err != nil {
		t.Errorf("a staged transaction's recovery directory was removed: %v", err)
	}
}

// A SECOND START IS REFUSED BEFORE IT REACHES THE NETWORK.
//
// The order matters as much as the lock. Resolving a signed channel talks to a
// mirror that can be slow or gone, and the node waits ninety seconds for an
// answer before reporting a refusal — after which the rollout retries every few
// minutes. With the lock taken AFTER the resolve, each retry started another
// process that hung in the same place and they accumulated for as long as the
// mirror stayed unreachable; worse, a retry could overtake the original, complete
// an upgrade, and let the original wake and run a second transaction over it.
func TestASecondStartIsRefusedBeforeItResolvesAnything(t *testing.T) {
	withUpgradeRoot(t)

	tx, err := takeTxLock()
	if err != nil {
		t.Fatalf("takeTxLock: %v", err)
	}

	defer tx.release()

	ack, _ := ackReader(t)

	// A channel that does not exist. Reaching the network at all would fail with
	// something about resolving it; being refused for the lock proves the order.
	err = startHostUpgrade(t.Context(), &config.Config{}, "", hostUpgradeTarget{
		channel: "no-such-channel-should-never-be-fetched",
	}, ack)

	if !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("a second start returned %v, want ErrUpgradeInProgress; if this names a "+
			"channel or a network failure, the lock is being taken after the resolve", err)
	}
}

// NOTHING HERE DELETES A PATH A FILE ON DISK NAMED.
//
// `abandonClaim` is the one operation in this area that removes a tree, and the
// path reaches it from a claim pointer or from a field inside a journal — a file
// that can be truncated, hand-edited, or restored from another machine. An empty
// name would make releaseClaim unlink whatever claim happened to exist, and an
// absolute one outside the upgrade root would have RemoveAll act somewhere nobody
// intended. Both are corrupted records rather than instructions.
func TestAbandoningRefusesAPathThatIsNotARecoveryDirectory(t *testing.T) {
	withUpgradeRoot(t)

	outside := t.TempDir()

	keep := filepath.Join(outside, "please-do-not-delete-me")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatalf("write the bystander file: %v", err)
	}

	for _, dir := range []string{"", outside, filepath.Join(upgradeRoot, "..")} {
		if err := abandonClaim(dir); err == nil {
			t.Errorf("abandoning %q was allowed", dir)
		}
	}

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file outside the upgrade root was removed: %v", err)
	}
}

// AND A RESUME ACTS ON THE DIRECTORY IT FOUND, NOT THE ONE THE JOURNAL NAMES.
//
// The two disagree only when something has gone wrong with the file, and the
// failures are asymmetric: releaseClaim compares the name against the pointer and
// quietly decides a mismatch belongs to somebody else — which leaves the machine
// stuck with a claim nothing will ever release.
func TestAResumeUsesTheDirectoryItFoundRatherThanTheOneRecorded(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(9); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.3.26", ToVersion: "v0.4.0", Generation: 4,
		Step: hostupgrade.StepClaimed,
	})

	claimed := journal.Dir

	// The journal on disk names somewhere else entirely.
	if err := os.WriteFile(filepath.Join(claimed, hostupgrade.JournalName),
		[]byte(`{"dir":"/nowhere","from_version":"v0.3.26","to_version":"v0.4.0",`+
			`"generation":4,"step":"claimed"}`), 0o600); err != nil {
		t.Fatalf("write a journal naming somewhere else: %v", err)
	}

	// Superseded and untouched, so this abandons — using the directory the claim
	// pointed at, which is the only one it has any evidence about.
	if err := resumeHostUpgrade(t.Context(), &config.Config{}); err != nil {
		t.Fatalf("resuming a journal that names the wrong directory: %v", err)
	}

	if _, err := os.Lstat(activePath()); !os.IsNotExist(err) {
		t.Errorf("the claim survived: %v", err)
	}

	if _, err := os.Stat(claimed); !os.IsNotExist(err) {
		t.Errorf("the directory the claim actually pointed at is still there: %v", err)
	}
}

// AND releaseClaim REFUSES TO ACT WITHOUT A NAME.
//
// abandonClaim happens to reject an empty path first, so this guard is reached
// only through the other caller — finishHostUpgrade, which passes the journal's
// own directory. Every caller knows which transaction it is finishing; an empty
// name means a record billet could not read, and releasing on that basis is how
// one transaction removes another's exclusion.
func TestReleasingAClaimWithoutANameIsRefused(t *testing.T) {
	withUpgradeRoot(t)

	dir, err := stageClaim()
	if err != nil {
		t.Fatalf("stageClaim: %v", err)
	}

	if err := publishClaim(dir); err != nil {
		t.Fatalf("publishClaim: %v", err)
	}

	if err := releaseClaim(""); err == nil {
		t.Error("releasing a claim with no name was allowed")
	}

	if _, err := os.Lstat(activePath()); err != nil {
		t.Errorf("an unnamed release removed a live claim: %v", err)
	}
}
