package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// geteuid is a seam for the same reason hostOS and converge are: whether this
// process is root decides whether a restore hands its work to the service
// account, and no test can be root.
var geteuid = os.Geteuid

// restoreOptions is what runLocalRestore acts on.
type restoreOptions struct {
	configPath string
	from       string
	dryRun     bool
	fenced     bool
	abandon    bool
	// ledgerAttached is the operator asserting the EXTERNAL ledger this archive's
	// identity belongs to is back. Only an identity-only archive consults it.
	ledgerAttached bool
	// fromBackup names an archive in backup.s3 rather than a directory on this
	// machine — the case the whole off-box path exists for, where this host is
	// new and holds nothing.
	fromBackup string
	deployment string
	into       string
}

// cmdLocalRestore puts a backup back, as one unit or not at all.
//
// WHAT MAKES THIS DIFFERENT FROM COPYING FILES is that it REFUSES. A restore
// that installed whatever was missing and left whatever was there would produce
// a deployment holding one installation's ledger beside another's authority —
// which starts, looks healthy, and cannot see the compute either of them
// launched. So every piece is either absent (installed), byte-identical
// (already done), or different (preserved and refused). There is no flag for
// overwriting one.
//
// THE EXCLUSION IS THE OTHER HALF, and no part of it is optional. See
// deployarchive.Execute for what the three local mechanisms prove; what none of
// them reaches is another MACHINE, which is why --old-controller-fenced exists
// and why it is required rather than defaulted.
func cmdLocalRestore(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local restore")
	cfgPath := addServiceConfigFlag(fs)
	from := fs.String("from", "", "the backup directory written by `billet local backup`")
	fromBackup := fs.String("from-backup", "",
		"fetch an archive from backup.s3 instead: its name, or `latest`")
	deployment := fs.String("deployment", "",
		"which deployment's archives to look at in the bucket, when it holds more than one")
	into := fs.String("into", "",
		"where to put a fetched archive (default: beside the state directory, and it is KEPT)")
	dryRun := fs.Bool("dry-run", false,
		"report exactly what would be installed and every reason it would be refused, and "+
			"change nothing")
	fenced := fs.Bool("old-controller-fenced", false,
		"assert that the control plane this backup came from is stopped and disabled, on every "+
			"host that could start it")
	abandon := fs.Bool("abandon", false,
		"undo an interrupted restore, removing only the files it recorded having created")
	ledgerAttached := fs.Bool("external-ledger-attached", false,
		"assert that the external ledger this backup's identity belongs to has been restored "+
			"and this host's config points at it")

	if err := parse(fs, args); err != nil {
		return err
	}

	switch {
	case *from == "" && *fromBackup == "":
		return errors.New("billet local restore needs --from <dir>, the backup to restore, or " +
			"--from-backup <name|latest> to fetch one from backup.s3")
	case *from != "" && *fromBackup != "":
		return errors.New("billet local restore: --from and --from-backup name two different " +
			"archives; give one")
	}

	if *dryRun && *abandon {
		return errors.New("billet local restore: --dry-run and --abandon are mutually exclusive")
	}

	// --abandon ACTS ON THE TARGET, NOT ON AN ARCHIVE, so fetching one for it
	// would download a deployment's credentials in order to delete some.
	if *abandon && *fromBackup != "" {
		return errors.New("billet local restore: --abandon undoes an interrupted restore on this " +
			"host and needs the archive that restore was from; point --from at the directory it " +
			"used")
	}

	return runLocalRestore(ctx, restoreOptions{
		configPath: *cfgPath, from: *from, dryRun: *dryRun,
		fenced: *fenced, abandon: *abandon, ledgerAttached: *ledgerAttached,
		fromBackup: *fromBackup, deployment: *deployment, into: *into,
	})
}

func runLocalRestore(ctx context.Context, o restoreOptions) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s declares no control plane, and a deployment restores onto one. "+
			"Run this on the host that will run `billet server`", o.configPath)
	}

	// THE FETCH HAPPENS FIRST AND CHANGES NOTHING ON THIS HOST. Everything below
	// is the ordinary local restore, over a directory that is now on disk — one
	// path through the planner, the refusals and the executor, whether the
	// archive arrived on a USB stick or out of a bucket.
	from := o.from
	if o.fromBackup != "" {
		if from, err = fetchFromBackup(ctx, cfg, o); err != nil {
			return err
		}
	}

	archive, err := deployarchive.Open(ctx, from)
	if err != nil {
		return err
	}

	target := deployarchive.Target{
		ConfigPath: o.configPath,
		StateDir:   cfg.Server.IdentityDir,
		// THE BACKEND THIS HOST IS CONFIGURED FOR, so the planner can refuse an
		// identity-only archive landing beside a config that would have the
		// control plane create a ledger of its own.
		LedgerBackend:          string(cfg.Server.LedgerBackend()),
		ExternalLedgerAttached: o.ledgerAttached,
	}

	if cfg.GitHub != nil {
		target.AppKeyPath = cfg.GitHub.PrivateKeyPath
		target.GitHub = deployarchive.GitHubIdentity{
			Org:            cfg.GitHub.Org,
			AppID:          cfg.GitHub.AppID,
			ClientID:       cfg.GitHub.ClientID,
			InstallationID: cfg.GitHub.InstallationID,
		}
	}

	if o.abandon {
		return runRestoreAbandon(ctx, archive, target)
	}

	// THE PLANNER IS PURE AND RUNS FIRST, WHATEVER HAPPENS NEXT. --dry-run and a
	// real restore take the same path to the same decisions, so there is one
	// implementation of the validation rather than two that can disagree about
	// what is safe.
	plan, err := deployarchive.PlanRestore(ctx, archive, target)
	if err != nil {
		return err
	}

	printArchive(archive)
	printRestorePlan(plan)

	if len(plan.Refusals) > 0 {
		return refusedRestore(plan.Refusals)
	}

	if o.dryRun {
		fmt.Println("\nNothing was changed (--dry-run).")

		return nil
	}

	// "NOTHING TO DO" IS NOT A CONCLUSION THIS PLAN MAY REACH. It was computed
	// without a lock — deliberately — and then printed for a person to read, so
	// by the time anything acts on it a credential may have changed and the
	// answer "everything is already in place" is a claim about a moment that has
	// passed. Reporting success on it is a lie an operator relies on.
	//
	// So a no-op goes through the executor like everything else: it takes the
	// locks, raises the fence, re-derives the plan, publishes nothing, and clears
	// the fence again. That also finishes a run interrupted after its LAST
	// publication — every piece in place, the ledger still fenced, and no control
	// plane able to start.
	if plan.Nothing() {
		fmt.Printf("\nEverything in this backup looks to be in place already; confirming that\n")
		fmt.Printf("under the lock rather than on a plan taken without one.\n")
	}

	if !o.fenced {
		return errFleetNotFenced(archive, target)
	}

	// THE SAME LOCK `up` AND `down` TAKE, and taken here rather than at the top:
	// nothing above this changes anything, and a --dry-run mutates nothing so it
	// excludes nothing. Without it, an `up` could start a control plane onto a
	// directory this command is half-way through publishing into.
	lock, err := lifecycleLock()
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Printf("warn     could not release the lifecycle lock: %v\n", err)
		}
	}()

	fmt.Println()

	res, err := deployarchive.Execute(ctx, deployarchive.RestoreRequest{
		Plan:          plan,
		InstallAppKey: installAppKey,
		Now:           time.Now,
		Actor:         actor(),
	})

	printRestoreResult(res)

	if err != nil {
		return partialRestore(target, err)
	}

	repairRestoredOwnership(plan)

	printRestored(archive, target, res)

	return nil
}

// repairRestoredOwnership hands what a PRIVILEGED restore just wrote to the
// account the packaged service runs as.
//
// FOUND BY REHEARSING IT, which is the entire argument for the rehearsal
// existing. A restore on a packaged Linux host runs as ROOT — it has to, because
// the App key lands in root-owned /etc/billet — so every file it publishes is
// root-owned 0600 inside a state directory the service account owns. systemd's
// StateDirectory= does not repair that: measured on 255, it walks the tree only
// when the TOP directory's owner is wrong, and after a restore it is already
// right. The deployment came back perfectly and the control plane could not open
// one file of it.
//
// `billet local up` was the documented answer and was never a complete one: its
// repair names the five files `billet check` can create and no part of the
// authority, so ca/, ca.key, ca.crt and authority-created stayed root's however
// many times it ran.
//
// A FAILURE HERE IS REPORTED, NOT RETURNED, and that is deliberate. The restore
// itself is finished and correct: every credential is in place and verified.
// Returning an error would read as "the restore failed" on the worst day of a
// deployment's life, and the next thing an operator reaches for is --abandon,
// which DELETES what this run installed. So it says exactly what is left to do.
func repairRestoredOwnership(plan deployarchive.Plan) {
	// NOT ON macOS: the launch agent runs as the operator, so a restore they ran
	// already wrote files that account owns.
	if hostOS != "linux" {
		return
	}

	// AND NOT WHEN THIS IS NOT ROOT. A restore run as the service account itself
	// already owns everything it wrote, and chown would fail with EPERM while
	// saying nothing an operator can act on.
	if geteuid() != 0 {
		return
	}

	c := converge()

	uid, gid, err := c.Identity(lifeops.UpRequest{
		ServiceUser:  initconfig.ServiceGroup,
		ServiceGroup: initconfig.ServiceGroup,
	})
	if err != nil {
		// NOT A PROBLEM, AND SAID RATHER THAN SKIPPED. A host with no service
		// account is one where billet runs as whoever invoked it, and that
		// account already owns what was just written.
		fmt.Printf("\nown      left as root: there is no %s service account here, so nothing "+
			"else\n", initconfig.ServiceGroup)
		fmt.Printf("         needs to be able to read this (%v)\n", err)

		return
	}

	fmt.Println()

	repaired, repairErr := c.RepairPaths(plan.Target.StateDir,
		restoredStateTargets(plan), uid, gid)

	for _, path := range repaired {
		fmt.Printf("own      %s given to %s (this restore ran as root)\n",
			path, initconfig.ServiceGroup)
	}

	// THE APP KEY IS REPAIRED BY PATH, because it lives OUTSIDE the state
	// directory — the planner refuses one inside it — and the walk above is
	// confined to that directory on purpose.
	keyErr := c.ApplyOwnership([]lifeops.OwnershipChange{{
		Path: plan.Target.AppKeyPath, Owner: initconfig.ServiceGroup,
		Group: initconfig.ServiceGroup, Mode: 0o600,
		Why: "billet refuses an App key readable beyond its owner, and the service must still " +
			"be able to read it",
	}}, uid, gid)

	if err := errors.Join(repairErr, keyErr); err != nil {
		fmt.Println()
		fmt.Printf("warn     the restore is COMPLETE and correct, but billet could not give all\n")
		fmt.Printf("         of it to the %s account: %v\n", initconfig.ServiceGroup, err)
		fmt.Printf("         Do NOT run --abandon; that would remove what this run installed.\n")
		fmt.Printf("         Fix the ownership by hand, then `billet local up`.\n")

		return
	}

	if len(repaired) > 0 {
		fmt.Printf("own      %s given to %s\n", plan.Target.AppKeyPath, initconfig.ServiceGroup)
	}
}

// restoredStateTargets is every entry inside the state directory this restore
// can have created.
//
// DERIVED FROM THE PLAN rather than from a list kept beside it. A list is the
// mistake this function exists to correct — lifeops carried one naming what
// `billet check` creates, it named no part of the authority, and nothing noticed
// until a restore was rehearsed on a real Linux host. A plan cannot go stale
// when the archive gains an entry.
//
// The three additions are things no action publishes: the directory the
// authority lives in, and the two LOCKS this command created as a side effect of
// taking them. A root-owned 0700 ca/ cannot be traversed by the service account
// at all, and a control plane that cannot open its own lock file does not start.
func restoredStateTargets(plan deployarchive.Plan) []lifeops.RepairTarget {
	var (
		targets []lifeops.RepairTarget
		seen    = map[string]bool{}
	)

	add := func(path string, dir bool) {
		rel, err := filepath.Rel(plan.Target.StateDir, path)
		// OUTSIDE THE STATE DIRECTORY IS NOT AN ERROR — it is the App key, which
		// is repaired by path above — and it must not be passed to a walk that is
		// confined to this directory.
		if err != nil || rel == "." || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}

		if seen[rel] {
			return
		}

		seen[rel] = true
		targets = append(targets, lifeops.RepairTarget{Name: filepath.ToSlash(rel), Dir: dir})
	}

	for _, a := range plan.Actions {
		add(a.Path, false)

		// SQLite keeps these beside the ledger, and anything that opened the
		// restored one as root may have left them.
		if a.Entry == deployarchive.EntryLedger {
			for _, sidecar := range deployarchive.LedgerSidecarPaths(a.Path) {
				add(sidecar, false)
			}
		}
	}

	add(wirecert.CADir(plan.Target.StateDir), true)
	add(state.DirectoryLockPath(plan.Target.StateDir), false)
	add(wirecert.AuthorityLockPath(plan.Target.StateDir), false)

	return targets
}

// restoreUnfinished reports whether a state directory is still mid-restore.
//
// EITHER MARK COUNTS. The journal is written just after the fence, so there is a
// window where the fence stands alone — a barrier that failed leaves exactly
// that — and a directory with either one is a directory no control plane may
// start on.
func restoreUnfinished(stateDir string) (bool, error) {
	prog, err := deployarchive.InProgress(stateDir)
	if err != nil {
		return false, err
	}

	if prog.Present {
		return true, nil
	}

	switch _, err := os.Lstat(state.MaintenanceFencePath(stateDir)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: %w", state.MaintenanceFencePath(stateDir), err)
	}
}

// installAppKey publishes the App key through the machinery that already owns
// that job.
//
// NOT A SECOND IMPLEMENTATION. reserveKeyFile creates a SIBLING of the
// destination — never the destination itself — and writeKeyAtomically links it
// into place with a call that fails rather than replaces, then reports where the
// bytes are if anything goes wrong instead of declaring a credential lost while
// it is still in memory. GitHub issues this key exactly once, and four review
// rounds went into those rules; a restore installs the same kind of file and
// must use the same code.
func installAppKey(path string, pem []byte) error {
	reserved, err := reserveKeyFile(path)
	if err != nil {
		return err
	}

	return writeKeyAtomically(reserved, path, pem, func() {})
}

// errFleetNotFenced is the refusal that no lock on this machine can substitute
// for.
func errFleetNotFenced(a *deployarchive.Archive, t deployarchive.Target) error {
	return fmt.Errorf(
		"this restore was not run. Billet can prove no control plane holds %s on THIS machine, "+
			"and it cannot prove anything about another one — restoring deployment %s here while "+
			"the controller it came from is still able to start produces two authoritative "+
			"controllers sharing one identity, one certificate authority and one App credential, "+
			"with ledgers that diverge from the moment both are up.\n\n"+
			"Stop that controller AND disable it, everywhere it could start, then re-run with "+
			"--old-controller-fenced.\n\n"+
			"Note also that a restored ledger has no lease for compute created after the backup "+
			"was taken (%s): reconnect nodes only once that compute is proved gone, or node "+
			"recovery destroys those jobs as orphans",
		t.StateDir, a.Manifest.DeploymentID, a.Manifest.CreatedAt)
}

// refusedRestore renders every reason at once, in the shape `up` uses.
func refusedRestore(refusals []lifeops.Refusal) error {
	var b strings.Builder

	b.WriteString("this deployment was NOT restored:\n")

	for _, r := range refusals {
		b.WriteString("\n  - ")
		b.WriteString(r.What)
		b.WriteString("\n    ")
		b.WriteString(r.Remedy)
	}

	b.WriteString("\n\nNothing was changed.")

	return errors.New(b.String())
}

// printArchive reports what the backup holds before anything is decided about
// it.
func printArchive(a *deployarchive.Archive) {
	m := a.Manifest

	fmt.Printf("backup   %s\n", a.Dir)
	fmt.Printf("         taken %s by billet %s\n", m.CreatedAt, m.BilletVersion)
	fmt.Printf("         deployment %s\n", m.DeploymentID)
	fmt.Printf("         app        %s\n", m.GitHub)
	fmt.Printf("         ca         %s, expires %s\n", m.Authority.Fingerprint, m.Authority.NotAfter)

	if m.Authority.Rotating {
		fmt.Printf("         ROTATING:  the previous authority %s is in this backup\n",
			m.Authority.PreviousFingerprint)
	}

	fmt.Printf("         ledger     schema %d (%d migrations)\n",
		m.Ledger.HighestVersion(), len(m.Ledger.Migrations))

	if m.Source.Host != "" {
		fmt.Printf("         from       %s:%s\n", m.Source.Host, m.Source.StateDir)
	}

	fmt.Println()
}

// printRestorePlan reports what would happen, item by item.
func printRestorePlan(p deployarchive.Plan) {
	for _, a := range p.Actions {
		switch a.Disposition {
		case deployarchive.AlreadyPresent:
			fmt.Printf("plan     %s is already in place at %s\n", a.What, a.Path)
		case deployarchive.ReplaceEmptyLedger:
			fmt.Printf("plan     replace the empty preflight ledger at %s\n", a.Path)
			fmt.Printf("         (it has no deployment data in it; `billet check` creates one)\n")
		case deployarchive.Install:
			fmt.Printf("plan     install %s at %s\n", a.What, a.Path)
		}
	}

	for _, path := range p.LedgerSidecars {
		fmt.Printf("plan     remove %s with it (a stray write-ahead log is replayed into the\n", path)
		fmt.Printf("         restored ledger, which is corruption rather than a stale file)\n")
	}
}

// printRestoreResult says what actually happened, whether or not it finished.
func printRestoreResult(res deployarchive.Result) {
	if res.Resumed {
		fmt.Printf("resume   an interrupted restore was already in progress here; continuing it\n\n")
	}

	for _, path := range res.Removed {
		fmt.Printf("remove   %s\n", path)
	}

	for _, a := range res.Skipped {
		fmt.Printf("skip     %s was already in place\n", a.What)
	}

	for _, a := range res.Installed {
		fmt.Printf("install  %s -> %s\n", a.What, a.Path)
	}

	// NEVER SWALLOWED. Publication links a staged file into place and then drops
	// the staging name; one that could not be dropped is a second copy of a
	// restored ledger, and nothing else would ever mention it.
	for _, path := range res.Strays {
		fmt.Printf("warn     %s is a second copy of a file this restore installed and could\n",
			path)
		fmt.Printf("         not be removed. The restore itself is fine; delete it once you\n")
		fmt.Printf("         have checked it.\n")
	}
}

// partialRestore says where a failure left this host.
//
// THE FENCE IS STILL UP, AND THAT IS THE POINT WORTH MAKING FIRST. A
// half-restored directory holds some of the pieces that make it this deployment
// and not the others, so a control plane starting on it would mint whatever is
// missing — a fresh identity, a fresh authority — and that is exactly the
// failure this whole command exists to prevent.
func partialRestore(t deployarchive.Target, cause error) error {
	fmt.Println()

	// READ, NOT ASSUMED — the same rule `billet local down`'s partial report
	// follows. A refusal BEFORE anything was published takes its own fence back
	// down, so asserting "still fenced" would tell an operator their control
	// plane is offline when it is not, and send them to clear a file that is not
	// there. Whether it is fenced is a fact about the host, and this is the
	// moment it matters most.
	switch _, err := os.Lstat(state.MaintenanceFencePath(t.StateDir)); {
	case err == nil:
		fmt.Printf("state    this restore did NOT finish, and %s is still fenced against every\n",
			t.StateDir)
		fmt.Printf("         billet: nothing can start a control plane on a half-restored\n")
		fmt.Printf("         deployment. The fence is %s\n", state.MaintenanceFencePath(t.StateDir))
	case errors.Is(err, os.ErrNotExist):
		fmt.Printf("state    this restore did NOT run, and %s is NOT fenced — it is exactly\n",
			t.StateDir)
		fmt.Printf("         as it was before this command started.\n")
	default:
		fmt.Printf("state    this restore did NOT finish, and billet could not tell whether\n")
		fmt.Printf("         %s is fenced (%v). Check that path before starting a\n",
			state.MaintenanceFencePath(t.StateDir), err)
		fmt.Printf("         control plane on this directory.\n")
	}
	// NAMED ONLY IF IT IS THERE. A restore can fail BEFORE the journal exists —
	// the writer barrier can time out, the re-derived plan can disagree with the
	// printed one — and pointing an operator at a file that does not exist, on
	// the day they are recovering a deployment, is its own small cruelty.
	if _, err := os.Lstat(deployarchive.JournalPath(t.StateDir)); err == nil {
		fmt.Printf("state    %s records what was published\n",
			deployarchive.JournalPath(t.StateDir))
		fmt.Println()
		fmt.Printf("         run the same command again to continue from where it stopped, or\n")
		fmt.Printf("         add --abandon to remove only the files this run created\n")
	} else {
		fmt.Printf("state    nothing was published — this stopped before it began writing\n")
		fmt.Println()
		fmt.Printf("         resolve the above and run the same command again\n")
	}

	return cause
}

// printRestored reports a finished restore, and the two things it does not
// settle.
func printRestored(a *deployarchive.Archive, t deployarchive.Target, res deployarchive.Result) {
	fmt.Println()

	// SAID DIFFERENTLY WHEN NOTHING MOVED, because "restored" over a run that
	// installed nothing reads as though work was done.
	if len(res.Installed) == 0 && len(res.Removed) == 0 {
		fmt.Printf("Deployment %s was already in place in %s, and that is now confirmed\n",
			a.Manifest.DeploymentID, t.StateDir)
		fmt.Printf("under the lock: every piece matches this backup byte for byte.\n\n")
	} else {
		fmt.Printf("Restored deployment %s into %s.\n\n", a.Manifest.DeploymentID, t.StateDir)
	}

	// THE LEDGER MAY BE OLDER THAN THIS BINARY, and that is ordinary rather than
	// a problem — but an operator who is not told will read the first start's
	// migration log as something going wrong.
	//
	// AND FOR AN EXTERNAL LEDGER THIS SENTENCE WOULD BE A CLAIM BILLET CANNOT
	// MAKE. Nothing here installed or examined that database; the number is what
	// the source recorded when the backup was taken, and the operator has just
	// restored the ledger themselves — quite possibly to a different point.
	// Printing it as the state of the thing they are about to start reinforces
	// confidence in exactly the half billet asked THEM to vouch for.
	if a.Manifest.Ledger.IsExternal() {
		fmt.Printf("  this backup recorded the external ledger at schema %d when it was taken;\n",
			a.Manifest.Ledger.HighestVersion())
		fmt.Printf("  billet did not inspect the database you restored, and a newer\n")
		fmt.Printf("  `billet server` migrates whatever it finds there on its first start\n\n")
	} else {
		fmt.Printf("  the ledger is at schema %d; a newer `billet server` migrates it forward on\n",
			a.Manifest.Ledger.HighestVersion())
		fmt.Printf("  its first start, exactly as it would any older ledger\n\n")
	}

	// OWNERSHIP IS THIS COMMAND'S JOB NOW, and the lines above say what it did.
	// It used to be deferred to `billet local up`, whose repair names the five
	// files a preflight creates and no part of the authority — so a restored ca/
	// stayed root-owned however many times it ran, and the control plane could
	// not start.
	fmt.Printf("  `billet local up` starts the services — it is what to run next\n\n")

	fmt.Printf("BEFORE you reconnect nodes: this ledger has no lease for compute created after\n")
	fmt.Printf("%s. Node recovery treats an instance with no lease as an orphan and destroys\n",
		a.Manifest.CreatedAt)
	fmt.Printf("it, and GitHub does not requeue a job that already started. Inventory that\n")
	fmt.Printf("compute and prove it gone first.\n")
}

// runRestoreAbandon undoes an interrupted restore.
func runRestoreAbandon(ctx context.Context, a *deployarchive.Archive,
	t deployarchive.Target,
) error {
	// THE FENCE COUNTS EVEN WITH NO JOURNAL. The journal is written just after
	// the fence, so a barrier that failed leaves the fence standing alone — and
	// reporting "nothing to abandon" there would leave the directory closed to
	// every billet with nothing on the host explaining why.
	pending, err := restoreUnfinished(t.StateDir)
	if err != nil {
		return err
	}

	if !pending {
		fmt.Printf("No restore is in progress in %s; there is nothing to abandon.\n", t.StateDir)

		return nil
	}

	prog, err := deployarchive.InProgress(t.StateDir)
	if err != nil {
		return err
	}

	if prog.Present {
		fmt.Printf("abandon  an interrupted restore from %s\n\n", prog.ArchiveDir)
	} else {
		fmt.Printf("abandon  %s is fenced by a restore that did not get as far as recording\n",
			t.StateDir)
		fmt.Printf("         anything; nothing was published, so only the fence comes down\n\n")
	}

	lock, err := lifecycleLock()
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Printf("warn     could not release the lifecycle lock: %v\n", err)
		}
	}()

	res, err := deployarchive.Abandon(ctx, a, t, deployarchive.RestoreFresh)

	for _, path := range res.Removed {
		fmt.Printf("remove   %s\n", path)
	}

	// KEPT IS THE INTERESTING HALF. A recorded path that no longer holds what
	// this restore put there is somebody else's file now, and the one thing this
	// command must never do is delete a credential it cannot prove is a
	// duplicate of one the backup still holds.
	for _, path := range res.Kept {
		fmt.Printf("keep     %s is no longer the file this restore wrote, so it was left alone\n",
			path)
	}

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("Abandoned. %s is no longer fenced.\n", t.StateDir)

	if len(res.Kept) > 0 {
		fmt.Println()
		fmt.Println("Look at the files above before restoring again: billet keeps anything it")
		fmt.Println("cannot prove it wrote, and one of them may be a credential.")
	}

	return nil
}
