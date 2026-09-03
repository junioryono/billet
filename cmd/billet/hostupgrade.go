package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// upgradeRoot is where recovery journals live.
//
// UNDER THE STATE DIRECTORY'S PARENT rather than in /tmp, because a journal that
// does not survive a reboot is a journal that is missing exactly when it is
// needed: a machine that lost power midway through an upgrade is the case the
// whole mechanism exists for.
// A VAR RATHER THAN A CONST, so a test can own the directory it writes into. The
// bookkeeping under this path is durable by design and a test that wrote to the
// real one would be a test that upgrades the machine running it.
var upgradeRoot = "/var/lib/billet/upgrades"

// activePointer is the claim one upgrade holds for the duration.
//
// A HARD LINK WITH NO-REPLACE SEMANTICS, which is the only filesystem primitive
// that is atomic across every filesystem billet runs on. A lock file opened
// O_EXCL would do as well; a hard link additionally leaves the journal reachable
// from a stable path, so a second run finds the transaction rather than only
// learning that one exists.
const activePointer = "active"

// cmdHostUpgrade replaces billet on this machine, transactionally.
//
// A SEPARATE PROGRAM BECAUSE A CONTROL PLANE CANNOT INSTALL ITS OWN SUCCESSOR.
// The moment it stops, whatever was going to finish the job has stopped too — so
// this is exec'd, detached, by whatever asked for the upgrade, and everything it
// is midway through is on the disk rather than in its memory. The rollout
// controller starts it and does not wait; an operator runs it directly.
//
// IT IS ALSO THE RESUME. Run with no target against a machine that has a journal,
// it continues or unwinds the transaction that is already there — which is what
// makes an interrupted upgrade a thing that recovers rather than a thing somebody
// reconstructs by hand.
func cmdHostUpgrade(ctx context.Context, args []string) error {
	fs := newFlagSet("billet host-upgrade")
	cfgPath := addConfigFlag(fs)
	channel := fs.String("channel", releasesource.ChannelStable,
		"the signed channel to resolve when no --version is given")
	pin := fs.String("version", "", "an exact release to install; it never moves")
	digest := fs.String("manifest-sha256", "",
		"the manifest digest the caller resolved; this run refuses anything else")
	rolloutID := fs.String("rollout", "", "the fleet decision that asked for this upgrade")
	generation := fs.Int64("generation", 0, "that decision's fencing generation")
	skipVerify := fs.Bool("skip-signature-verification", false,
		"trust this source by other means; only for an air-gapped mirror")
	resume := fs.Bool("resume", false,
		"continue or unwind the transaction already on this machine, installing nothing new")
	reinstall := fs.Bool("reinstall", false,
		"install even if this machine already reports the release being installed; "+
			"this is what repairs a host a rollout blocked for running the right version "+
			"from bytes it did not decide on")
	status := fs.Bool("status", false,
		"report what this machine holds — a transaction in progress, its journal, the "+
			"fleet decision it last acted on, and which release manifest produced it")
	ackFD := fs.Int("ack-fd", 0,
		"a descriptor to report acceptance or refusal on; a node passes this so a "+
			"preflight refusal reaches the control plane instead of being invisible")

	if err := parse(fs, args); err != nil {
		return err
	}

	if err := checkFleetInstruction(*rolloutID, *generation); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if *status {
		reportUpgradeStatus()

		return nil
	}

	ack := newUpgradeAck(*ackFD)
	defer ack.close()

	if *resume {
		err := resumeHostUpgrade(ctx, cfg)
		if err != nil {
			ack.refuse(err)
		} else {
			ack.accept()
		}

		return err
	}

	err = startHostUpgrade(ctx, cfg, *cfgPath, hostUpgradeTarget{
		channel:    *channel,
		pin:        *pin,
		digest:     *digest,
		rolloutID:  *rolloutID,
		generation: *generation,
		skipVerify: *skipVerify,
		reinstall:  *reinstall,
	}, ack)

	// REFUSING AFTER THE FACT IS A NO-OP ONCE ACCEPTANCE WAS SENT. What the caller
	// is waiting to hear is whether this updater took the job, and a failure
	// halfway through a transaction it DID take is the journal's business, not the
	// dispatcher's — by then the caller has long stopped listening.
	ack.refuse(err)

	return err
}

type hostUpgradeTarget struct {
	channel string
	pin     string
	// digest is the manifest the CALLER resolved, and it is a fence rather than a
	// convenience.
	//
	// WITHOUT IT THE ROLLOUT'S DECISION IS ONLY A VERSION STRING. The coordinator
	// resolves a channel once, to one immutable manifest, precisely so the whole
	// fleet installs the same bytes — and a node that re-resolved the channel for
	// itself would defeat that the moment the channel advanced mid-rollout, or
	// whenever a tag was moved. Passing the digest makes the node's own resolution
	// a lookup that must AGREE rather than an independent decision.
	digest string
	// rolloutID and generation are the fleet decision this serves, recorded in the
	// journal so an operator who finds a machine mid-upgrade knows what asked for
	// it and a resuming run can see which decision it belongs to.
	rolloutID  string
	generation int64
	skipVerify bool
	// reinstall installs even when this machine already reports the release.
	//
	// THE REPAIR THAT DOES NOT DEPEND ON THE RECORD BEING READABLE. The ordinary
	// shortcut asks whether the installed manifest DISAGREES, and a record that is
	// damaged, or missing, answers "cannot tell" — correctly, because reinstalling
	// on cannot-tell would drain every host in a fleet to fix a diagnostic. But the
	// control plane may already hold a disagreeing digest from before that record
	// went bad, so the host stays blocked while the command it is told to run
	// decides there is nothing to do. This is the way through, and it is a person
	// asserting rather than billet inferring.
	reinstall bool
}

// startHostUpgrade stages a verified candidate and then runs the transaction.
//
// EVERYTHING THAT CAN FAIL WITHOUT CONSEQUENCE HAPPENS FIRST. Resolving a
// channel, verifying a signature, downloading an archive, checking a digest and
// asking whether this release may replace this one are all things that go wrong,
// and every one of them goes wrong here — while the deployment is still running
// normally and the recovery is to do nothing at all.
func startHostUpgrade(ctx context.Context, cfg *config.Config, cfgPath string,
	target hostUpgradeTarget, ack *upgradeAck,
) error {
	policy, err := releasesource.PolicyForRelease(target.skipVerify)
	if err != nil {
		return err
	}

	// THE TRANSACTION LOCK IS TAKEN BEFORE THE NETWORK IS, and that ordering is
	// what bounds a hung updater.
	//
	// Resolving a signed channel talks to a mirror that can be slow or gone, and
	// the node waits ninety seconds for an answer before reporting a refusal — after
	// which the rollout retries every few minutes. With the lock taken after the
	// resolve, each of those retries started ANOTHER process that hung in the same
	// place, and they accumulated for as long as the mirror stayed unreachable.
	// Taken first, the one stuck process holds the lock and every retry is refused
	// immediately, so at most one uncertain updater exists per host.
	tx, err := takeTxLock()
	if err != nil {
		return err
	}

	defer tx.release()

	client := &releasesource.Client{}

	manifest, digest, err := resolveTarget(ctx, client, policy, target.channel, target.pin)
	if err != nil {
		return err
	}

	return actOnResolved(ctx, cfg, cfgPath, target, ack, client, manifest, digest, tx)
}

// actOnResolved decides and does everything that follows knowing which release is
// being asked for.
//
// SPLIT FROM THE RESOLUTION SO IT CAN BE TESTED, and the split is at the only
// boundary that helps: everything above it reaches the network to resolve a
// signed channel, and everything below it is the decision-making a review found
// two defects in. Asserting on the SHAPE of this code — which is what the gate
// here used to do — could not tell a call that runs from one that is skipped, and
// broke on a rename that changed nothing.
//
// THE HELD TRANSACTION LOCK IS AN ARGUMENT, WHICH IS THE POINT OF PASSING IT.
// Every branch below either starts a transaction or decides that this machine is
// where it was asked to be, and both are answers no process may give while
// another is midway through moving the host. Taking it here instead would be
// worse than not taking it: a flock is held by the OPEN FILE DESCRIPTION, so a
// second acquisition from a second descriptor in this same process conflicts
// exactly as another process would, and the probe that tried it refused every
// ordinary upgrade against its own caller's lock. Naming it in the signature is
// how a caller is told, since nothing else can enforce it.
func actOnResolved(ctx context.Context, cfg *config.Config, cfgPath string,
	target hostUpgradeTarget, ack *upgradeAck, client *releasesource.Client,
	manifest *releasesource.Manifest, digest string, _ *txLock,
) error {
	if err := checkResolvedDigest(target, digest); err != nil {
		return err
	}

	current := hostCompatibility(cfg)

	// THE PREFLIGHT REFUSES BEFORE ANYTHING STOPS. A candidate that shares no wire
	// version with this build, or expects a ledger schema behind the installed
	// one, cannot be discovered after the switch: by then the old binary is hidden
	// and both services are down.
	warnings, err := releasesource.Compatibility(manifest, current)
	for _, w := range warnings {
		fmt.Printf("NOTE: %v\n\n", w)
	}

	// A REFUSAL IS A REFUSAL WHATEVER ELSE CAME BACK. The warnings are printed
	// first so an operator sees both, and then the error ends it — an earlier
	// version picked the guest-contract change out of a joined error and carried
	// on, which waved through a candidate that also shared no wire version.
	if err != nil {
		return err
	}

	// THE FENCE IS ASKED BEFORE ANYTHING ELSE IS DECIDED, INCLUDING WHETHER THERE
	// IS ANYTHING TO DO. Putting the already-running fast path first was a defect a
	// review caught: a machine could satisfy decision 10 without ever raising its
	// mark, after which a delayed decision 9 passed a stale fence and DOWNGRADED it.
	// Doing nothing is still acting on a decision.
	//
	// This asking is cheap and touches nothing, so a superseded instruction is
	// refused without taking an exclusion it is not entitled to — which would lock
	// out the decision that replaced it for the length of a drain.
	if err := checkDecision(target); err != nil {
		return err
	}

	// THE VERSION MATCHING IS NOT ENOUGH TO DO NOTHING, and treating it as enough
	// left the one repair a blocked host has with no effect.
	//
	// A rollout BLOCKS a host that reports the target version from a different
	// manifest, and tells the operator to run this command — which then saw the
	// versions agree, declared there was nothing to do, and left the machine
	// exactly as it was. The rollout blocked it again on the next pass, and the
	// only ways out were an exemption or a reinstall nothing documented.
	//
	// POSITIVE DISAGREEMENT ONLY. A host with no record is not reinstalled: that is
	// every host in the field before one billet-driven upgrade has run, and turning
	// each of their no-op upgrades into a real transaction would stop services and
	// drain compute to fix a diagnostic.
	if manifest.Version == version.Version() && !target.reinstall && !installedDisagrees(digest) {
		fmt.Printf("This machine is already running %s.\n", manifest.Version)

		// THE MARK IS RAISED EVEN THOUGH NOTHING IS INSTALLED, because what it
		// records is which decision this machine has ACTED ON, and concluding "there
		// is nothing to do" is acting on one.
		//
		// CHECKED AND RECORDED AS ONE STEP. This path takes no claim — there is no
		// transaction to exclude — so the mark's own lock is the only thing making
		// the pair atomic, and doing them separately is what let a newer decision
		// land between them and be overwritten.
		if err := checkAndRecordDecision(target); err != nil {
			return err
		}

		// ACCEPTED, BECAUSE NOTHING IS OWED. The caller asked for a machine to be on
		// a release and it is; reporting this as a refusal would have a coordinator
		// back off and ask again forever about a host that is already where it was
		// told to be.
		ack.accept()

		return nil
	}

	dir, err := stageClaim()
	if err != nil {
		return err
	}

	journal := &hostupgrade.Journal{
		Dir:          dir,
		FromVersion:  version.Version(),
		ToVersion:    manifest.Version,
		TargetDigest: digest,
		RolloutID:    target.rolloutID,
		Generation:   target.generation,
		PID:          os.Getpid(),
		Step:         hostupgrade.StepClaimed,
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	// THE JOURNAL IS ON THE DISK BEFORE THE POINTER EXISTS, so no reader can ever
	// find a claim with nothing behind it. Nothing outside this process knows the
	// directory yet, so a failure here takes it away and leaves no trace.
	if err := journal.Write(); err != nil {
		_ = os.RemoveAll(dir)

		return err
	}

	if err := publishClaim(dir); err != nil {
		return err
	}

	// THE FENCE IS ASKED AGAIN, AND THE CHECK AND THE RECORD ARE ONE STEP.
	//
	// The asking before the claim reads a file nothing was holding still: another
	// upgrade could have finished, raising the mark, in the window between that
	// read and this claim. And separating this check from the record leaves the
	// same window one layer down — the mark's own lock is what closes it, so both
	// happen inside one acquisition.
	//
	// RECORDED BEFORE ANY OF THE TRANSACTION HAPPENS. What it protects against is a
	// SECOND instruction, and the window that matters is the whole length of this
	// one — which is unbounded, because it contains a drain.
	if err := checkAndRecordDecision(target); err != nil {
		// THE DIRECTORY GOES TOO. This transaction was never accepted and has touched
		// nothing outside it, so the journal records a decision that was refused
		// rather than an upgrade somebody needs to read about — and a rollout retries
		// every few minutes, which would otherwise leave one behind each time.
		claimErr := releaseClaim(dir)
		if claimErr == nil {
			_ = os.RemoveAll(dir)
		}

		return errors.Join(err, claimErr)
	}

	// THE JOB IS TAKEN, AND THIS IS THE MOMENT IT BECAME TRUE.
	//
	// Everything that can refuse without touching the deployment has now run — the
	// channel resolved, the digest agreed, the candidate was found compatible, the
	// generation was not superseded, the claim was taken, and the recovery journal
	// is on the disk and fsynced. From here the transaction is recoverable by a
	// resume even if this process dies, so there is nothing left for the caller to
	// wait for and a great deal it must not wait for: the very next step downloads
	// an archive, and the one after that drains a host for as long as somebody's
	// job takes.
	ack.accept()

	staged, err := stageCandidate(ctx, client, manifest, dir)
	if err != nil {
		return fmt.Errorf("staging %s: %w", manifest.Version, err)
	}

	if err := journal.Advance(hostupgrade.StepStaged); err != nil {
		return err
	}

	fmt.Printf("Upgrading %s -> %s\n", journal.FromVersion, journal.ToVersion)
	fmt.Printf("  recovery journal %s\n", dir)
	fmt.Printf("  candidate %s\n\n", staged)
	fmt.Printf("The node stops first so its compute drains, and that wait is unbounded:\n")
	fmt.Printf("a job may run for days and nothing here ends one.\n\n")

	return finishHostUpgrade(ctx, journal, newSystemdHost(cfg, cfgPath, staged))
}

// resumeHostUpgrade continues or unwinds the transaction already on this machine.
func resumeHostUpgrade(ctx context.Context, cfg *config.Config) error {
	// A LIVE UPDATER IS NOT AN ABANDONED ONE, and nothing here could tell the
	// difference before this lock existed. The claim survives a crash on purpose,
	// so its presence proves a transaction was STARTED and says nothing about
	// whether a process is working on it — which is why a resume run beside a live
	// detached updater used to enter the same transaction, stopping services it was
	// starting and advancing a journal it was writing.
	tx, err := takeTxLock()
	if err != nil {
		return err
	}

	defer tx.release()

	dir, err := os.Readlink(activePath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No upgrade is in progress on this machine.\n")

			return nil
		}

		return fmt.Errorf("read the upgrade pointer: %w", err)
	}

	journal, err := hostupgrade.ReadJournal(dir)
	if errors.Is(err, hostupgrade.ErrNoJournal) {
		// A CLAIM WITH NO JOURNAL IS A TRANSACTION THAT NEVER BEGAN, and it is
		// recoverable precisely because of what the ordering guarantees: the journal
		// is on the disk BEFORE the pointer is published, so a claim without one
		// means nothing on this machine was touched.
		//
		// THAT ORDERING IS ALSO WHY THIS CANNOT RELEASE A LIVE UPGRADE'S CLAIM. When
		// the pointer was published first there was a window in which a running
		// updater held a claim and had not yet written its journal, and a resume
		// landing in it would clear the claim of an upgrade that was about to stop
		// services — leaving a second one free to run on top. What remains here is a
		// backstop for a directory something else emptied, or one an older billet
		// left.
		//
		// Left alone it is the worst of both — `start` refuses because a claim exists
		// and `--resume` has nothing to continue — so a machine that lost power in
		// that window, or hit a full disk, would need a person to delete a symlink
		// before any rollout could move it again.
		fmt.Printf("An upgrade claimed %s and never wrote its journal, so nothing on this\n",
			dir)
		fmt.Printf("machine was touched. Releasing the claim; nothing else is needed.\n")

		return releaseClaim(dir)
	}

	if err != nil {
		return err
	}

	// THE DIRECTORY IS THE ONE THIS RESUME FOUND, NOT THE ONE THE FILE NAMES.
	//
	// `dir` came from the claim pointer; `journal.Dir` is a string inside a file on
	// disk, and everything downstream acts on it — releaseClaim compares it against
	// the pointer, and abandonClaim REMOVES it. A journal that was truncated,
	// hand-edited or restored from elsewhere could name a different path, or none,
	// and the failures are asymmetric: a wrong name makes releaseClaim quietly
	// decide the claim belongs to somebody else and leave the machine stuck, and it
	// makes a removal act somewhere nobody intended.
	journal.Dir = dir

	fmt.Printf("Resuming the upgrade %s -> %s, which reached %s.\n",
		journal.FromVersion, journal.ToVersion, journal.Step)

	if done, err := settleResumedDecision(journal); err != nil || done {
		return err
	}

	return finishHostUpgrade(ctx, journal,
		newSystemdHost(cfg, "", filepath.Join(dir, "billet")))
}

// finishHostUpgrade runs the transaction and releases the claim only once the
// machine is in a state another upgrade may start from.
//
// THE CLAIM IS RELEASED FOR EXACTLY TWO OUTCOMES, and an earlier version released
// it for everything except a cordon — which a review caught. `Run` can also fail
// AFTER committing, because clearing the fence, starting the services or proving
// them stable all happen past the commit record. That machine may still be fenced
// or stopped, and releasing the pointer made `host-upgrade --resume` answer "no
// upgrade is in progress" about a host that very much has one.
//
// So: a clean success and a completed rollback release it. Everything else — a
// cordon, a committed upgrade whose services did not come back, a failure this
// build cannot classify — keeps it, because keeping it is what makes `--resume`
// find the transaction and what stops a rollout starting a second one on top.
func finishHostUpgrade(ctx context.Context, journal *hostupgrade.Journal,
	host hostupgrade.Host,
) error {
	err := hostupgrade.Run(ctx, hostupgrade.Request{Journal: journal, Host: host})

	if errors.Is(err, hostupgrade.ErrCordoned) {
		fmt.Printf("\nThis machine is CORDONED. Its recovery journal is %s and is left in\n",
			journal.Dir)
		fmt.Printf("place; nothing about which release it is on, which schema its ledger\n")
		fmt.Printf("carries, or whether its compute exists is known. Look at it before\n")
		fmt.Printf("starting anything else here.\n")

		return err
	}

	if err != nil && !errors.Is(err, hostupgrade.ErrRolledBack) {
		fmt.Printf("\nThis upgrade did not finish. Its recovery journal is %s and the claim\n",
			journal.Dir)
		fmt.Printf("is kept, so `billet host-upgrade --resume` continues it; the machine may\n")
		fmt.Printf("still be fenced or have its services stopped.\n")

		return err
	}

	if claimErr := releaseClaim(journal.Dir); claimErr != nil {
		return errors.Join(err, claimErr)
	}

	return err
}

// checkResolvedDigest refuses a release that moved underneath the decision.
//
// THE CALLER'S DIGEST WINS, AND A DISAGREEMENT IS FATAL. Reached when a channel
// advanced between the coordinator resolving it and this node acting, or when a
// tag moved: either way the bytes this run would install are not the bytes the
// fleet decided on, and installing them anyway is how one host ends up on a
// release nothing recorded.
//
// AN ABSENT DIGEST IS NOT A MISMATCH. An operator running `billet host-upgrade`
// by hand pins nothing, and refusing them would make the fleet's fence a
// requirement for the one path that has no fleet behind it.
func checkResolvedDigest(target hostUpgradeTarget, resolved string) error {
	if target.digest == "" || strings.EqualFold(target.digest, resolved) {
		return nil
	}

	return fmt.Errorf("this upgrade was asked for manifest %s but resolving %s here "+
		"produced %s, so the release moved underneath the decision; nothing was installed",
		target.digest, describeTarget(target), resolved)
}

// checkFleetInstruction refuses an instruction that is fleet-shaped but unfenced.
//
// TWO MODES, AND THEY HAVE TO BE TOLD APART EXPLICITLY. A person running this
// command carries no generation and is deliberately not fenced — the mark is a
// fleet mechanism and requiring it on the one path with no fleet behind it would
// leave nobody able to fix the machine a rollout got stuck on. Everything the
// mark protects therefore rests on "generation > 0 means this came from a
// rollout", and that inference is only sound if the two modes cannot be mixed.
//
// An instruction naming a rollout with no generation is neither: it would install
// over an arbitrarily high mark while looking like an operator at the keyboard. A
// negative one is refused everywhere for the same reason — it is not a smaller
// decision, it is a number no rollout produces.
func checkFleetInstruction(rolloutID string, generation int64) error {
	if generation < 0 {
		return fmt.Errorf("--generation %d is not a fleet decision; generations count up "+
			"from one and an operator's own run carries none", generation)
	}

	if rolloutID != "" && generation == 0 {
		return fmt.Errorf("--rollout %s names a fleet decision but --generation is not "+
			"set, so nothing here could tell whether the decision has been superseded. "+
			"Pass both, or neither for a run of your own", rolloutID)
	}

	return nil
}

// reportUpgradeStatus says what this machine is holding, and changes nothing.
//
// REPORTING ONLY, AND THAT IS THE WHOLE DESIGN. An updater that hung before it
// could answer leaves a process nothing reclaims — the transaction lock bounds it
// to one per host, because every retry is refused at once, but nothing may kill
// that one: it may already be past its claim and installing, and a machine with a
// half-replaced binary and no process finishing the job is the worst state this
// area can produce. So this puts what is knowable in front of a person and stops.
func reportUpgradeStatus() {
	fmt.Printf("upgrade root  %s\n", upgradeRoot)

	// THE LOCK IS THE LIVENESS SIGNAL. The claim is durable and survives a crash on
	// purpose, so its presence proves a transaction was STARTED and says nothing
	// about whether a process is working on it; the flock says the opposite, since
	// the kernel drops it when its holder dies.
	if tx, err := takeTxLock(); err == nil {
		tx.release()

		fmt.Printf("transaction   none is running\n")
	} else if errors.Is(err, ErrUpgradeInProgress) {
		fmt.Printf("transaction   ONE IS RUNNING NOW\n")
	} else {
		fmt.Printf("transaction   could not be determined: %v\n", err)
	}

	reportUpgradeJournal()

	if acted, err := readDecision(); err != nil {
		fmt.Printf("decision      unreadable: %v\n", err)
	} else if acted == 0 {
		fmt.Printf("decision      this machine has acted on no fleet decision\n")
	} else {
		fmt.Printf("decision      %d is the newest fleet decision acted on here\n", acted)
	}

	reportProvenance()
}

// reportUpgradeJournal describes the transaction on this machine, if there is one.
func reportUpgradeJournal() {
	dir, err := os.Readlink(activePath())
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("journal       no upgrade has claimed this machine\n")

			return
		}

		fmt.Printf("journal       the claim could not be read: %v\n", err)

		return
	}

	journal, err := hostupgrade.ReadJournal(dir)
	if err != nil {
		fmt.Printf("journal       %s holds a claim billet cannot read: %v\n", dir, err)

		return
	}

	fmt.Printf("journal       %s -> %s, reached %s\n",
		journal.FromVersion, journal.ToVersion, journal.Step)
	fmt.Printf("              started %s in %s\n", journal.StartedAt, dir)

	if journal.PID > 0 {
		// NAMED, NOT ACTED ON. A pid is a number the kernel reuses, so this is
		// something for a person to look at rather than something to signal.
		fmt.Printf("              claimed by pid %d\n", journal.PID)
	}

	if journal.RolloutID != "" {
		fmt.Printf("              for rollout %s, decision %d\n",
			journal.RolloutID, journal.Generation)
	}

	if journal.Failure != "" {
		fmt.Printf("              FAILED: %s\n", journal.Failure)
	}
}

// reportProvenance says which release manifest produced the binary here.
func reportProvenance() {
	record, err := provenance.Read()

	switch {
	case errors.Is(err, provenance.ErrNoRecord):
		fmt.Printf("installed     nothing here records which release manifest produced " +
			"this binary\n")
	case err != nil:
		fmt.Printf("installed     the record is unreadable: %v\n", err)
	default:
		fmt.Printf("installed     %s from manifest %s\n", record.Version, record.ManifestDigest)

		// WHETHER IT STILL DESCRIBES THIS BINARY IS A SEPARATE QUESTION, and the
		// answer an operator needs: a record left behind by a hand-replaced binary
		// looks identical to a good one until the hashes are compared.
		if _, err := provenance.Installed(); err != nil {
			fmt.Printf("              BUT IT NO LONGER DESCRIBES THE BINARY HERE: %v\n", err)
		}
	}
}

// installedDisagrees reports whether this machine says it came from a DIFFERENT
// manifest than the one being installed.
//
// THREE ANSWERS COLLAPSED TO ONE BOOLEAN, DELIBERATELY, and only one of them is
// true: a record naming another manifest. No record at all, and a record that no
// longer describes the running binary, both answer false — they are cases where
// billet cannot tell, and reinstalling on "cannot tell" would drain every host in
// the fleet the first time anything asked.
func installedDisagrees(digest string) bool {
	// THE ERROR IS THE WHOLE GUARD, and a second check on the value was dead code
	// dressed as one: provenance.Read refuses a record naming no manifest, so a nil
	// error always comes with a digest. Leaving the redundant clause in made the
	// real guard look optional.
	installed, err := provenance.Installed()
	if err != nil {
		return false
	}

	return !strings.EqualFold(installed, digest)
}

// describeTarget names how this run resolved its release, for a digest mismatch.
func describeTarget(target hostUpgradeTarget) string {
	if target.pin != "" {
		return target.pin
	}

	return "the " + target.channel + " channel"
}

// settleResumedDecision brings the fence up to date with a transaction that was
// interrupted, and abandons one the fleet has moved past.
//
// A CRASH BETWEEN PUBLISHING A CLAIM AND RECORDING ITS GENERATION LEAVES A
// RESUMABLE TRANSACTION THE FENCE HAS NEVER HEARD OF. Finishing it without
// recording lets a delayed older instruction pass a stale mark afterwards and
// downgrade the host, which is the exact thing the mark exists to stop.
//
// WHETHER TO FINISH IT AT ALL DEPENDS ON WHAT IT HAS ALREADY DONE, and getting
// that boundary wrong is the most expensive mistake available here.
//
// A STEP RECORDS WHAT COMPLETED, SO THE WORK AFTER IT IS ALREADY IN FLIGHT.
// `Reached(StepStopped)` reads like "has it stopped anything yet" and is not:
// preserving the installed binary, stopping the node, stopping the server and
// hiding the binary ALL run, and only then is StepStopped written. A journal
// sitting at `staged` may therefore have a host with both services down and no
// billet on the path — and abandoning it releases the claim, which leaves that
// host stopped with nothing on it that can be resumed. A review caught it; the
// first version of this used exactly that boundary.
//
// SO ONLY `claimed` IS CONCLUSIVE. The work between `claimed` and `staged` is a
// download into this transaction's own directory and nothing else, so a journal
// at `claimed` provably has not touched the machine. Everything later is
// ambiguous, and the ambiguity is resolved toward FINISHING: a superseded release
// installed on one host is a rollout that dispatches again, while a host left
// stopped is one a person has to go and find.
func settleResumedDecision(journal *hostupgrade.Journal) (bool, error) {
	if journal.Generation <= 0 {
		return false, nil
	}

	acted, err := readDecisionFenced()
	if err != nil {
		return false, err
	}

	if journal.Generation < acted && journal.Step == hostupgrade.StepClaimed {
		fmt.Printf("\nThis machine has since acted on fleet decision %d and this "+
			"transaction is %d, so finishing it would install a release the deployment\n",
			acted, journal.Generation)
		fmt.Printf("has left behind. It never got past claiming, so nothing here was\n")
		fmt.Printf("touched. Abandoning it.\n")

		return true, abandonClaim(journal.Dir)
	}

	// RECORDED NOW, because the crash is what stopped it being recorded before.
	return false, recordDecision(journal.Generation)
}

// abandonClaim releases a claim and removes the directory behind it.
//
// ONLY FOR A TRANSACTION PROVEN TO HAVE TOUCHED NOTHING. releaseClaim keeps the
// directory on purpose — its journal is what an operator reads after a rollback
// and it holds the preserved binary and the ledger snapshot. None of that exists
// for a transaction that never got past claiming, and the journal there records a
// decision that was refused rather than an upgrade anybody needs to read about.
// Keeping them accumulates one directory per superseded instruction on a fleet
// that retries every few minutes.
func abandonClaim(dir string) error {
	// IT ONLY EVER REMOVES SOMETHING UNDER THE UPGRADE ROOT. The path arrives from
	// a claim pointer or a journal field, and this is the one operation here that
	// deletes a tree — so it is bounded by construction rather than by everything
	// upstream being right. An empty or outside path is a corrupted record, not an
	// instruction.
	if err := underUpgradeRoot(dir); err != nil {
		return err
	}

	if err := releaseClaim(dir); err != nil {
		return err
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the abandoned recovery directory %s: %w", dir, err)
	}

	return nil
}

// readDecisionFenced reads the mark under its lock.
func readDecisionFenced() (int64, error) {
	var acted int64

	err := withDecisionLock(func() error {
		got, err := readDecision()
		acted = got

		return err
	})

	return acted, err
}

// underUpgradeRoot refuses a path that is not a recovery directory.
func underUpgradeRoot(dir string) error {
	if dir == "" {
		return errors.New("this transaction's recovery directory is not recorded, so " +
			"billet will not act on it")
	}

	// A DIRECT CHILD, not merely something underneath. Recovery directories are
	// created by MkdirTemp directly in the root, so anything deeper is a path this
	// code did not make — and the operation on the other side of this check deletes
	// a tree.
	rel, err := filepath.Rel(upgradeRoot, filepath.Clean(dir))
	if err != nil || rel == "." || strings.Contains(rel, string(filepath.Separator)) ||
		strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not a recovery directory in %s, so billet will not "+
			"act on it", dir, upgradeRoot)
	}

	return nil
}

// hostCompatibility describes the running deployment for the preflight.
func hostCompatibility(cfg *config.Config) releasesource.Current {
	contract := ""
	if cfg.Node != nil && cfg.Node.Provider == config.ProviderFirecracker {
		contract = firecracker.GuestContract
	}

	return releasesource.Host(version.Version(),
		releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		state.LatestSchemaVersion(), contract)
}

// claimUpgrade takes the exclusion one upgrade holds for its whole duration.
//
// A SYMLINK, AND NOT A HARD LINK TO THE JOURNAL. The first version linked the
// journal FILE, which reads as tidier and is wrong: Journal.Write publishes
// through a rename, and a rename replaces the directory entry rather than the
// inode — so after the very first step the pointer named a stale copy while the
// real record moved on. Pointing at the DIRECTORY keeps the pointer correct for
// the whole transaction, and every read goes to the live journal inside it.
//
// os.Symlink FAILS IF THE DESTINATION EXISTS, atomically, which is what makes
// this an exclusion rather than a check followed by a hopeful write. Two upgrades
// on one machine would each stop services the other was starting, and the two
// journals would each describe half of what happened.
// THE POINTER IS PUBLISHED LAST, AFTER THE JOURNAL IS ALREADY IN THE DIRECTORY.
//
// Claiming first and writing the journal second left a window in which the
// machine had a claim and no journal — and that state is one `--resume` treats as
// a transaction that never began and CLEARS, because the journal precedes
// anything being staged, stopped or fenced. A resume landing in that window would
// release a claim belonging to an upgrade that was about to stop services, after
// which a second one could claim and run on top of it: two updaters starting and
// stopping the same units, which is the exact thing the claim exists to prevent.
//
// Publishing the journal first makes that state unreachable rather than
// recoverable, which is the better of the two. Two racing claims are still
// correct: both stage a directory, both write a journal, one wins the symlink and
// the loser removes its own and has touched nothing.
func stageClaim() (string, error) {
	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		return "", fmt.Errorf("prepare %s: %w", upgradeRoot, err)
	}

	dir, err := os.MkdirTemp(upgradeRoot, "upgrade-")
	if err != nil {
		return "", fmt.Errorf("create a recovery directory: %w", err)
	}

	return dir, nil
}

// publishClaim takes the exclusion, for a directory that already holds a journal.
func publishClaim(dir string) error {
	if err := os.Symlink(dir, activePath()); err != nil {
		// THE STAGED DIRECTORY GOES WITH IT. Nothing outside it knows the name, and
		// this run has touched nothing else, so leaving it would accumulate a
		// directory per refused attempt on a machine a rollout retries every few
		// minutes.
		_ = os.RemoveAll(dir)

		return fmt.Errorf("an upgrade is already in progress on this machine (%s "+
			"exists). Resume it with `billet host-upgrade --resume`, or remove the pointer "+
			"once you have read its journal: %w", activePath(), err)
	}

	// FLUSHED, because this pointer is how a resume finds the transaction at all.
	// A claim that did not survive a power cut leaves a machine mid-upgrade with
	// `--resume` answering "no upgrade is in progress" — and a second `start` then
	// takes a fresh claim and begins a new transaction on top of the first.
	return syncUpgradeDir(upgradeRoot)
}

// activePath is the pointer to the transaction in progress.
func activePath() string { return filepath.Join(upgradeRoot, activePointer) }

// releaseClaim removes the pointer once a transaction has reached a durable
// decision.
//
// THE RECOVERY DIRECTORY STAYS. Its journal is the record of what happened and
// the only place the preserved binary, units and configuration live; removing it
// with the pointer would delete the evidence an operator reads after a rollback,
// and the snapshot a second attempt might still need. Ordinary retention is a
// person's decision, not this command's.
func releaseClaim(dir string) error {
	// IT REMOVES THE CLAIM IT WAS GIVEN, NOT WHATEVER CLAIM EXISTS.
	//
	// Unlinking the pointer blind was a defect a review caught: by the time a
	// finishing transaction gets here, another upgrade may already have claimed —
	// and removing ITS pointer leaves a live updater running with no exclusion,
	// after which a third can claim and stop the same services underneath it.
	//
	// A readlink and a compare is not atomic with the unlink, and nothing in POSIX
	// offers "unlink this symlink only if it points there". What it removes is the
	// window: from the whole length of a transaction down to two syscalls, and the
	// only thing that can land inside it is another billet publishing a claim in
	// exactly that instant.
	if dir == "" {
		// NOT A LICENCE TO UNLINK WHATEVER IS THERE. Every caller knows which
		// transaction it is finishing; an empty name means a record billet could not
		// read, and releasing on that basis is how one transaction removes another's
		// exclusion.
		return errors.New("release the upgrade claim: no transaction was named, so " +
			"billet cannot tell whose claim this is")
	}

	{
		switch held, err := os.Readlink(activePath()); {
		case os.IsNotExist(err):
			return nil
		case err != nil:
			return fmt.Errorf("read the upgrade claim before releasing it: %w", err)
		case held != dir:
			// SOMEBODY ELSE'S CLAIM, AND LEAVING IT IS THE ONLY SAFE ANSWER. This is
			// not an error either: this transaction's own claim is already gone, which
			// is the state it was trying to reach.
			return nil
		}
	}

	if err := os.Remove(activePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("release the upgrade claim: %w", err)
	}

	// FLUSHED FOR THE SAME REASON THE CLAIM IS. A removal that did not survive a
	// power cut leaves a pointer to a finished transaction, and the next start is
	// refused against an upgrade that is over.
	return syncUpgradeDir(upgradeRoot)
}

// stageCandidate downloads and unpacks the candidate binary.
func stageCandidate(ctx context.Context, client *releasesource.Client,
	manifest *releasesource.Manifest, dir string,
) (string, error) {
	// GO'S SPELLING, NOT uname's, AND THE TWO ARE BOTH IN THIS PACKAGE. hostArch()
	// answers "aarch64" because a GUEST IMAGE manifest records what uname says;
	// a release artifact is named from Go's GOOS/GOARCH, because that is what
	// GoReleaser's name template interpolates and what the install script derives.
	// Using the wrong one here selects nothing and reports the platform as
	// unpublished.
	artifact, err := manifest.Select(hostOS, runtime.GOARCH, releasesource.KindArchive)
	if err != nil {
		return "", err
	}

	archive, err := client.Download(ctx, manifest.Version, artifact, dir)
	if err != nil {
		return "", err
	}

	// UNPACKED WITH tar, WHICH IS ON EVERY HOST BILLET RUNS ON. The archive's
	// digest has already been proved against the signed manifest, so what comes
	// out of it is what was published.
	cmd := exec.CommandContext(ctx, "tar", "-xzf", archive, "-C", dir, "billet")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("unpack %s: %w: %s", filepath.Base(archive), err, out)
	}

	staged := filepath.Join(dir, "billet")
	if err := os.Chmod(staged, 0o755); err != nil {
		return "", fmt.Errorf("make the candidate executable: %w", err)
	}

	return staged, nil
}

// systemdHost is the real machine.
//
// EVERY METHOD COMPOSES SOMETHING THAT ALREADY EXISTS — lifeops for the services,
// internal/state for the fence and the snapshot — rather than reimplementing it.
// The ORDER these are called in is internal/hostupgrade's, and that is where the
// safety content lives; this file is the vocabulary.
//
// NOT UNIT TESTED, AND IT CANNOT BE. Every one of these stops a service, replaces
// a binary or migrates a database. What IS tested is the sequence, against a fake
// that records what it was asked to do — see internal/hostupgrade.
type systemdHost struct {
	cfg      *config.Config
	cfgPath  string
	staged   string
	inspect  *lifeops.Inspector
	converge *lifeops.Converger
}

func newSystemdHost(cfg *config.Config, cfgPath, staged string) *systemdHost {
	inspector := lifeops.NewInspector()

	return &systemdHost{
		cfg:      cfg,
		cfgPath:  cfgPath,
		staged:   staged,
		inspect:  inspector,
		converge: lifeops.NewConverger(inspector),
	}
}

const (
	nodeUnit   = "billet-node.service"
	serverUnit = "billet-server.service"
	installed  = "/usr/bin/billet"
)

// StopNode stops the node and proves it stopped.
//
// UNBOUNDED BY ANYTHING HERE. The node's SIGTERM is a drain that waits for the
// compute already running for as long as it runs, and `TimeoutStopSec` in the
// unit is what eventually bounds it — losing billet's bookkeeping rather than the
// jobs. Nothing in this command imposes a shorter one.
func (h *systemdHost) StopNode(ctx context.Context) error {
	return h.stop(ctx, nodeUnit)
}

// StopServer stops the control plane, after the node's custody has settled.
func (h *systemdHost) StopServer(ctx context.Context) error {
	return h.stop(ctx, serverUnit)
}

func (h *systemdHost) stop(ctx context.Context, unit string) error {
	how, err := h.converge.StopAndProve(ctx, unit)
	if err != nil {
		return fmt.Errorf("stopping %s: %w", unit, err)
	}

	fmt.Printf("  stopped %s (%s)\n", unit, how)

	return nil
}

// PreserveCurrent copies what is installed into the recovery directory.
//
// THE INSTALLED FILES, NOT WHAT A PACKAGE WOULD REINSTALL. Those are not the same
// thing on any host somebody has ever edited a unit on, and a rollback that
// restored the package's idea of the units would silently discard an operator's
// drop-in as part of recovering from an unrelated failure.
func (h *systemdHost) PreserveCurrent(_ context.Context, dir string) error {
	preserved := filepath.Join(dir, "preserved")
	if err := os.MkdirAll(preserved, 0o700); err != nil {
		return fmt.Errorf("prepare the preserved directory: %w", err)
	}

	for _, path := range preservedPaths() {
		if err := copyIfPresent(path, filepath.Join(preserved, filepath.Base(path))); err != nil {
			return err
		}
	}

	return nil
}

// preservedPaths is what a rollback puts back.
//
// ONE LIST, READ BY BOTH HALVES, because two copies of it is a rollback gap with
// no symptom. PreserveCurrent and RestorePreserved each used to carry their own,
// so a path added to one and not the other would be saved and never restored —
// or restored from a copy nobody made — and nothing would fail until a rollback
// that needed it.
//
// THE PROVENANCE RECORD IS IN HERE, which is what makes a rolled-back host report
// the manifest it is ACTUALLY on rather than the one it briefly had. Without it a
// rollback would leave the new record beside the old binary, which is precisely
// the stale-record case the node-side hash refuses — so the host would report
// nothing, and a rollout would lose the one fact it had about it.
func preservedPaths() []string {
	return []string{
		installed,
		"/etc/systemd/system/" + nodeUnit,
		"/etc/systemd/system/" + serverUnit,
		"/etc/billet/billet.yaml",
		provenance.Path,
	}
}

// HideBinary removes the installed executable.
func (h *systemdHost) HideBinary(_ context.Context) error {
	if err := os.Remove(installed); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hide %s: %w", installed, err)
	}

	return nil
}

// Fence makes already-open operator handles refuse transactions.
func (h *systemdHost) Fence(ctx context.Context, reason string) error {
	if h.cfg.Server == nil {
		return nil
	}

	if _, err := state.WriteMaintenanceFence(h.cfg.Server.IdentityDir, reason); err != nil {
		return err
	}

	// AND THE BARRIER, because the fence stops NEW transactions and says nothing
	// about one that began before it. Migrating underneath a transaction already
	// in flight is what the barrier exists to prevent.
	return state.WriterBarrier(ctx, h.cfg.Server.IdentityDir)
}

func (h *systemdHost) ClearFence(_ context.Context, reason string) error {
	if h.cfg.Server == nil {
		return nil
	}

	return state.ClearMaintenanceFence(h.cfg.Server.IdentityDir, reason)
}

// SnapshotLedger writes a complete copy of the ledger.
func (h *systemdHost) SnapshotLedger(ctx context.Context, dest string) error {
	if h.cfg.Server == nil {
		return nil
	}

	// OpenMaintenance, NOT OpenAdmin, AND THE DIFFERENCE IS THE WHOLE STEP.
	//
	// The transaction fences the ledger immediately before this, and OpenAdmin
	// REFUSES a fenced state directory — that is what the fence is for. Snapshotting
	// through it would fail on every server-bearing upgrade billet ever performed,
	// at the step whose entire purpose is making the rest reversible. A review
	// caught it; nothing here could, because this method has never run.
	//
	// The typed entry is the authorised crossing: it exists precisely so
	// fence-crossing comes from code that KNOWS it is the transaction, rather than
	// from an ambient environment variable any inherited shell could carry.
	db, err := openStateMaintenance(ctx, h.cfg)
	if err != nil {
		return fmt.Errorf("open the ledger to snapshot it: %w", err)
	}

	defer func() { _ = db.Close() }()

	// VACUUM INTO REFUSES AN EXISTING DESTINATION, which is a free no-clobber: a
	// resumed run cannot silently overwrite the snapshot it is about to restore
	// from.
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear a previous snapshot: %w", err)
	}

	return db.SnapshotInto(ctx, dest)
}

// RestoreLedger puts the snapshot back.
func (h *systemdHost) RestoreLedger(_ context.Context, from string) error {
	if h.cfg.Server == nil {
		return nil
	}

	return copyFile(from, filepath.Join(h.cfg.Server.IdentityDir, "billet.db"))
}

// InstallCandidate puts the staged binary in place.
func (h *systemdHost) InstallCandidate(_ context.Context) error {
	return copyFile(h.staged, installed)
}

// RecordInstalled writes which manifest produced the binary now in place.
//
// THE HASH IS TAKEN FROM THE FILE THAT WAS JUST INSTALLED rather than from the
// staged copy, so the record describes what a reader will actually find. They are
// the same bytes; hashing the destination is what makes that a fact rather than
// an assumption about the copy.
func (h *systemdHost) RecordInstalled(_ context.Context, digest, release string) error {
	if digest == "" {
		// AN OPERATOR'S OWN RUN PINS NOTHING, and a record naming no manifest would
		// be a file every reader has to distrust. Leaving the previous record in
		// place is wrong too — it describes bytes that are gone — so it goes, and
		// the host reports nothing until something installs it with a digest.
		if err := os.Remove(provenance.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear the stale provenance record: %w", err)
		}

		return nil
	}

	sum, err := provenance.HashFile(installed)
	if err != nil {
		return err
	}

	return provenance.Write(provenance.Record{
		Version: release, ManifestDigest: digest, BinarySHA256: sum,
	})
}

// Migrate opens the ledger with the candidate as its only writer.
//
// THROUGH THE CANDIDATE, NOT THIS PROCESS. This binary is the OLD one; opening
// the ledger here would apply the old build's migrations, which is the opposite
// of what an upgrade needs. The candidate's own `--upgrade-probe` is what runs.
func (h *systemdHost) Migrate(ctx context.Context) error {
	// --maintenance-probe FOR THE SAME REASON THE SNAPSHOT USES OpenMaintenance.
	// The ledger is fenced at this point, and an ordinary `billet check` is an
	// operator command: it opens through OpenAdmin and is refused. The flag is what
	// says "I am the transaction", and `billet check` is its one caller.
	return h.runCandidate(ctx, "check", "--config", h.configPath(), "--maintenance-probe")
}

// PrepareImages pulls a guest generation the candidate accepts, for every image
// this host's firecracker tiers boot.
//
// THE CANDIDATE DECIDES, FROM THE STAGED BINARY. `images compatible` is the
// question and its exit status the answer: 0 is nothing to do, 2 names the
// images that need a generation in the result file, and anything else could not
// tell, which refuses the upgrade rather than reading as "nothing to do". The
// pull is the same `images pull --verify` an operator runs, so the generation is
// imported, booted and promoted under the same rules. This is the Go half of what
// the Ansible host role's transaction already did for itself.
func (h *systemdHost) PrepareImages(ctx context.Context) error {
	if h.cfg.Node == nil || h.cfg.Node.Provider != config.ProviderFirecracker ||
		h.cfg.Node.Ceph == nil {
		return nil
	}

	result := filepath.Join(filepath.Dir(h.staged), "guest-images-to-refresh")

	err := h.runStaged(ctx, "images", "compatible", "--config", h.configPath(),
		"--result-file", result)
	switch code := commandExitCode(err); code {
	case 0:
		return nil
	case 2:
	default:
		return fmt.Errorf("the candidate could not say whether this host's guest images are "+
			"compatible with it: %w", err)
	}

	names, err := readImageResults(result)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := h.runStaged(ctx, "images", "pull", "--verify", "--config", h.configPath(),
			name); err != nil {
			return fmt.Errorf("pulling a generation of %s the candidate accepts: %w", name, err)
		}
	}

	return nil
}

// runStaged runs the staged candidate, which is the binary this transaction is
// about to install; the installed one is hidden by the time this is called.
func (h *systemdHost) runStaged(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, h.staged, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", args[0], args[1], err, out)
	}

	return nil
}

// commandExitCode is the status a command exited with, or -1 for a command that
// did not run or was killed, which must never read as a status the caller acts on.
func commandExitCode(err error) int {
	if err == nil {
		return 0
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		return exit.ExitCode()
	}

	return -1
}

// readImageResults reads the bare image names `images compatible --result-file`
// wrote, one per line.
func readImageResults(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the candidate said images need a generation but left no list: %w", err)
	}

	var names []string

	for line := range strings.SplitSeq(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}

	if len(names) == 0 {
		return nil, errors.New("the candidate said images need a generation and named none")
	}

	return names, nil
}

// ProbeReady starts the candidate under units that poll nothing.
func (h *systemdHost) ProbeReady(ctx context.Context) error {
	if h.cfg.Server != nil {
		if err := h.runCandidate(ctx, "server", "--config", h.configPath(),
			"--upgrade-probe"); err != nil {
			return fmt.Errorf("the candidate could not open what it inherited: %w", err)
		}
	}

	if h.cfg.Node != nil {
		if err := h.runCandidate(ctx, "node", "--config", h.configPath(),
			"--upgrade-probe"); err != nil {
			return fmt.Errorf("the candidate's node could not initialise its provider: %w", err)
		}
	}

	return nil
}

func (h *systemdHost) runCandidate(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, installed, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", args[0], err, out)
	}

	return nil
}

func (h *systemdHost) configPath() string {
	if h.cfgPath != "" {
		return h.cfgPath
	}

	return "/etc/billet/billet.yaml"
}

// StartServices starts the steady-state units and proves each came up.
func (h *systemdHost) StartServices(ctx context.Context) error {
	// THE SERVER FIRST, because the node registers against it. Starting the node
	// first means its first registrations are refused for as long as the control
	// plane takes to come up — harmless and noisy, and the reverse of the stop
	// order for the same reason it is the reverse of the stop order.
	for _, unit := range []string{serverUnit, nodeUnit} {
		if !h.wanted(unit) {
			continue
		}

		if _, err := h.converge.StartAndProve(ctx, unit); err != nil {
			return fmt.Errorf("starting %s: %w", unit, err)
		}
	}

	return nil
}

// ProveStable reports whether the services stayed up.
//
// A SECOND LOOK RATHER THAN A LONGER FIRST ONE. A unit that starts and exits
// immediately is `active` for a moment, and StartAndProve can catch it in that
// moment; asking again after a pause is what tells a service that came up from
// one that is crash-looping.
func (h *systemdHost) ProveStable(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(stabilityWait):
	}

	report, err := h.inspect.Inspect(ctx, h.configPath(), "")
	if err != nil {
		return fmt.Errorf("inspecting the restarted services: %w", err)
	}

	services := []struct {
		unit  string
		facts *lifeops.ServiceFacts
	}{
		{serverUnit, &report.Server},
		{nodeUnit, &report.Node},
	}

	for i := range services {
		svc := &services[i]

		if !h.wanted(svc.unit) {
			continue
		}

		if !svc.facts.Active() {
			return fmt.Errorf("%s is %s after %s, so it did not stay up",
				svc.unit, svc.facts.ActiveState, stabilityWait)
		}
	}

	return nil
}

// stabilityWait is how long a service has to stay up to be believed.
//
// SHORT, BECAUSE IT IS NOT A HEALTH CHECK. What it catches is a unit that starts
// and exits — a missing binary, an unreadable config, a refused ledger — which
// happens in the first seconds or not at all. A longer wait would delay every
// upgrade to catch a class of failure this cannot see anyway.
const stabilityWait = 10 * time.Second

// RestorePreserved puts back what PreserveCurrent copied.
func (h *systemdHost) RestorePreserved(_ context.Context, dir string) error {
	preserved := filepath.Join(dir, "preserved")

	for _, target := range preservedPaths() {
		source := filepath.Join(preserved, filepath.Base(target))
		if _, err := os.Stat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return fmt.Errorf("read the preserved %s: %w", filepath.Base(target), err)
		}

		if err := copyFile(source, target); err != nil {
			return err
		}
	}

	return nil
}

// wanted reports whether this deployment runs a unit at all.
func (h *systemdHost) wanted(unit string) bool {
	switch unit {
	case serverUnit:
		return h.cfg.Server != nil
	case nodeUnit:
		return h.cfg.Node != nil
	}

	return false
}

// copyIfPresent copies a file, treating an absent source as nothing to do.
//
// ABSENT IS A VALUE HERE. A control-plane-only host has no node unit and a
// node-only host has no server unit, and preserving "everything that exists" is
// how a rollback restores the right set on both.
func copyIfPresent(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("read %s: %w", from, err)
	}

	return copyFile(from, to)
}

// copyFile writes through a staging name and flushes, so a reader never finds a
// half-written binary under the name it is about to execute.
func copyFile(from, to string) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}

	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("stat %s: %w", from, err)
	}

	staging := to + ".billet-upgrade"

	// EVERY DESTINATION IS A CONSTANT IN THIS FILE OR A PATH UNDER THE RECOVERY
	// DIRECTORY THIS PROCESS CREATED. Nothing here comes from a manifest, a
	// command line or a node — the archive's contents are extracted elsewhere,
	// under a directory chosen here — so there is no attacker-influenced component
	// to traverse with.
	//nolint:gosec // the destinations are this file's own constants and directories it created; see above
	if err := os.WriteFile(staging, body, info.Mode().Perm()); err != nil {
		return fmt.Errorf("stage %s: %w", to, err)
	}

	f, err := os.Open(staging)
	if err != nil {
		return fmt.Errorf("reopen %s to flush it: %w", staging, err)
	}

	syncErr := f.Sync()
	_ = f.Close()

	if syncErr != nil {
		return fmt.Errorf("flush %s: %w", staging, syncErr)
	}

	if err := os.Rename(staging, to); err != nil {
		return fmt.Errorf("publish %s: %w", to, err)
	}

	// THE DIRECTORY TOO, AND OMITTING IT DEFEATED THE JOURNAL.
	//
	// Syncing the staging file makes its CONTENTS durable; the rename that gives it
	// a name is a directory operation, and an unsynced directory can lose that
	// entry across a power cut. So the journal could record "installed" while the
	// filesystem still had the old binary under that name — and a resume, reading a
	// step it believed complete, would skip the install and start a machine on the
	// release it was supposed to be leaving. Power loss is the exact case the
	// journal exists for, so its ordering has to outlive one.
	return syncUpgradeDir(filepath.Dir(to))
}

// syncUpgradeDir flushes a directory entry, so a rename into it survives a power
// cut.
func syncUpgradeDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to flush it: %w", dir, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", dir, err)
	}

	return nil
}
