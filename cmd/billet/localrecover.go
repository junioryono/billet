package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/state"
)

// recoverOptions is what runLocalRecover acts on.
type recoverOptions struct {
	configPath string
	from       string
	fromBackup string
	deployment string
	into       string
	dryRun     bool
	fenced     bool
	abandon    bool
	acceptJobs bool
	reason     string
	timeout    time.Duration
}

// cmdLocalRecover puts THIS deployment back over itself.
//
// THE ORDINARY DISASTER SHAPE, and the one `billet local restore` deliberately
// refuses: the controller is sick, the operator has yesterday's archive, and the
// ledger on disk is not one they want to keep. Restore will not touch a
// commissioned deployment — its ledger is a live capacity record — so until this
// existed the documented path was to move the state directory aside by hand,
// which works and gives an operator no help deciding whether it is safe.
//
// WHAT MAKES IT SAFE IS NOT A FLAG. This must be a separate operation, and the
// reason is the same one AdoptDeploymentID gives about relabelling: a restored
// ledger has no lease for compute created after the backup, so node recovery
// treats those instances as orphans and destroys them — and GitHub does not
// requeue a job that already started. So this seals the deployment, waits for it
// to hold nothing, and NAMES every job it would fail before an operator may
// accept losing them.
//
// IT CAN NEVER RELABEL A HOST. The archive's identity must be the one already on
// disk, which the planner establishes by comparing the identity file against the
// manifest — exactly the check a restore makes, unchanged. Everything else about
// this deployment (the authority, the App key) is byte-identical or the whole
// operation is refused, again by the same code.
func cmdLocalRecover(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local recover")
	cfgPath := addServiceConfigFlag(fs)
	from := fs.String("from", "", "the backup directory written by `billet local backup`")
	fromBackup := fs.String("from-backup", "",
		"fetch an archive from backup.s3 instead: its name, or `latest`")
	deployment := fs.String("deployment", "",
		"which deployment's archives to look at in the bucket, when it holds more than one")
	into := fs.String("into", "",
		"where to put a fetched archive (default: beside the state directory, and it is KEPT)")
	dryRun := fs.Bool("dry-run", false,
		"report what would happen and every reason it would be refused, and change nothing")
	fenced := fs.Bool("old-controller-fenced", false,
		"assert that every OTHER controller for this deployment is stopped and disabled")
	acceptJobs := fs.Bool("accept-failing-jobs", false,
		"proceed even though this deployment is still holding work, accepting that every job "+
			"named below fails and GitHub will not requeue it")
	reason := fs.String("reason", "",
		"why this deployment is sealed, for whoever finds it that way")
	timeout := fs.Duration("timeout", 0,
		"give up waiting for the deployment to go quiet after this long (default: wait)")
	abandon := fs.Bool("abandon", false,
		"undo an interrupted recovery, removing only what it created and putting the ledger "+
			"it moved aside back")

	if err := parse(fs, args); err != nil {
		return err
	}

	switch {
	case *from == "" && *fromBackup == "":
		return errors.New("billet local recover needs --from <dir>, the backup to recover from, " +
			"or --from-backup <name|latest> to fetch one from backup.s3")
	case *from != "" && *fromBackup != "":
		return errors.New("billet local recover: --from and --from-backup name two different " +
			"archives; give one")
	}

	if *dryRun && *abandon {
		return errors.New("billet local recover: --dry-run and --abandon are mutually exclusive")
	}

	if *timeout < 0 {
		return fmt.Errorf("--timeout %s is negative; use a positive duration, or omit it to "+
			"wait for as long as the jobs take", *timeout)
	}

	return runLocalRecover(ctx, recoverOptions{
		configPath: *cfgPath, from: *from, fromBackup: *fromBackup, deployment: *deployment,
		into: *into, dryRun: *dryRun, fenced: *fenced, abandon: *abandon,
		acceptJobs: *acceptJobs, reason: *reason, timeout: *timeout,
	})
}

func runLocalRecover(ctx context.Context, o recoverOptions) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s declares no control plane, and a deployment is recovered onto "+
			"one. Run this on the host that runs `billet server`", o.configPath)
	}

	from := o.from
	if o.fromBackup != "" {
		if from, err = fetchFromBackup(ctx, cfg, restoreOptions{
			configPath: o.configPath, fromBackup: o.fromBackup,
			deployment: o.deployment, into: o.into,
		}); err != nil {
			return err
		}
	}

	archive, err := deployarchive.Open(ctx, from)
	if err != nil {
		return err
	}

	// LedgerBackend TRAVELS HERE TOO, so the planner's external-ledger refusal
	// can name what this host is configured for. Recover refuses such an archive
	// outright — there is no billet operation that replaces an external ledger in
	// place — and a refusal that could not say which engine would leave an
	// operator guessing which half is wrong.
	target := deployarchive.Target{
		ConfigPath:    o.configPath,
		StateDir:      cfg.Server.IdentityDir,
		LedgerBackend: string(cfg.Server.LedgerBackend()),
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
		return runRecoverAbandon(ctx, archive, target)
	}

	plan, err := deployarchive.PlanRecover(ctx, archive, target)
	if err != nil {
		return err
	}

	printArchive(archive)
	printRecoverPlan(plan)

	if len(plan.Refusals) > 0 {
		return refusedRestore(plan.Refusals)
	}

	if o.dryRun {
		fmt.Println("\nNothing was changed (--dry-run).")

		return nil
	}

	if !o.fenced {
		return errFleetNotFenced(archive, target)
	}

	// THE DEPLOYMENT IS SEALED AND PROVED QUIET BEFORE ANYTHING IS TOUCHED, and
	// that ordering is the operation's whole safety content: the ledger about to
	// be replaced is what says which compute exists, so replacing it while work
	// is running loses the record of that work at the same moment as the work.
	//
	// AFTER THE PLAN IS PRINTED AND AFTER THE FENCING ASSERTION, deliberately.
	// Both alternatives are worse: planning first is what lets --dry-run report
	// on a live deployment without touching it, and sealing before the operator
	// has asserted the other controllers are stopped would close a deployment
	// over an operation that then refuses.
	//
	// THE COST IS ONE CONFUSING MESSAGE IN ONE NARROW CASE, and it self-heals.
	// Sealing WRITES to the ledger, so a deployment whose ledger was still
	// byte-identical to the archive's — a recovery run immediately after a backup,
	// with nothing in between — is planned as "already present" and re-planned
	// inside the executor as a supersede, which Execute correctly refuses as "the
	// target changed". Running it again agrees with itself and proceeds. The
	// refusal is safe; making it impossible would mean giving up one of the two
	// orderings above.
	//
	// A RESUME DOES NOT QUIESCE AGAIN, AND CANNOT. An interrupted recovery leaves
	// the directory FENCED, and quiescing opens the ledger through OpenAdmin,
	// which honours that fence — so a retry that tried would be refused by its own
	// first attempt, and the documented resume would be unreachable. The first
	// attempt sealed this deployment and proved it quiet; the fence has held since,
	// so nothing can have been admitted in between. That is what makes skipping it
	// sound rather than convenient.
	stage, err := recoveryStageOf(target.StateDir, archive)
	if err != nil {
		return err
	}

	switch stage {
	case recoverPublished:
		fmt.Printf("\nresume   an interrupted run of this recovery already published every file\n")
		fmt.Printf("         and moved the old ledger aside — the plan above is what it DID, and\n")
		fmt.Printf("         none of it happens twice. Sealing the restored ledger and lifting\n")
		fmt.Printf("         the fence are all that is left\n")
	case recoverResume:
		fmt.Printf("\nresume   an interrupted recovery is already fenced here; its seal and its\n")
		fmt.Printf("         quiescence still hold, so this continues from where it stopped\n")
	case recoverFresh:
		if err := quiesceForRecovery(ctx, cfg, o); err != nil {
			return err
		}
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

	var res deployarchive.Result

	// A PUBLICATION THAT FINISHED IS NOT RUN AGAIN, and skipping it is the whole
	// point of recording the phase. Executing over it would re-plan against a
	// ledger that is now the archive's plus a seal, decide on a second supersede,
	// find the first one's destination occupied and refuse — a recovery with no
	// way forward, reached by re-running the command its own diagnostic asks for.
	if stage != recoverPublished {
		fmt.Println()

		res, err = deployarchive.Execute(ctx, deployarchive.RestoreRequest{
			Plan:          plan,
			InstallAppKey: installAppKey,
			Now:           time.Now,
			Actor:         actor(),
		})

		printRestoreResult(res)

		for _, path := range res.Superseded {
			fmt.Printf("aside    %s\n", path)
		}

		if err != nil {
			return partialRestore(target, err)
		}
	}

	repairRestoredOwnership(plan)

	// SEALED AGAIN, BEHIND THE FENCE THE EXECUTOR DELIBERATELY LEFT UP — and both
	// halves of that matter.
	//
	// The seal taken before the ledger was replaced lived IN that ledger, and the
	// ledger is now the archive's, whose admission row is whatever it was when the
	// backup was taken: open, almost always. A recovery that stopped at the
	// previous line would hand back a deployment that takes work the moment it
	// starts, while its nodes still hold compute the restored ledger has never
	// heard of — which node recovery destroys as orphans.
	//
	// AND THE WINDOW HAS TO BE CLOSED, not merely short. Clearing the fence first
	// and sealing afterwards leaves an interval in which a control plane can start
	// on an open ledger; the fence is the only thing that refuses one, so it stays
	// up until the seal is durable and Finish takes it down.
	if err := sealRecoveredDeployment(ctx, cfg, o); err != nil {
		return err
	}

	if err := deployarchive.Finish(ctx, plan); err != nil {
		fmt.Println()
		fmt.Printf("warn     this deployment is recovered and SEALED, and %s is still fenced:\n",
			target.StateDir)
		fmt.Printf("         %v\n", err)
		fmt.Printf("         Nothing can start on it until that is cleared. Re-run this command.\n")

		return err
	}

	printRecovered(archive, target, res)

	return nil
}

// recoveryStage is how far an earlier attempt at THIS recovery got.
type recoveryStage int

const (
	// recoverFresh: nothing of this recovery is here. Seal, prove it quiet,
	// publish.
	recoverFresh recoveryStage = iota
	// recoverResume: an interrupted attempt holds the fence and has not finished
	// publishing. Its seal and its quiescence still stand, so publication
	// continues from where it stopped.
	recoverResume
	// recoverPublished: every file is down and the old ledger is aside. Only
	// sealing the restored ledger and lifting the fence remain.
	recoverPublished
)

// recoveryStageOf reads what an earlier attempt left behind.
//
// WHAT IT DECIDES IS WHETHER TO SKIP SEALING AND PROVING THE DEPLOYMENT QUIET —
// the operation's whole safety content — and whether to publish at all, which is
// what keeps a crash from becoming a dead end. So each of the three facts it
// consults is load-bearing:
//
// THE JOURNAL MUST BE THIS ARCHIVE'S, by manifest digest rather than by
// pathname: an archive fetched from a bucket lands wherever the second run put
// it, and half of one backup beside half of another is not a deployment.
//
// THE PHASE SAYS WHETHER THE PUBLICATION FINISHED. A recovery that got that far
// has already moved the operator's ledger aside, and re-planning would find that
// destination occupied and refuse — so a re-run of the very command billet's own
// diagnostic asks for would have no way forward.
//
// AND THE FENCE MUST BE A RECOVERY'S. A fence raised by a RESTORE satisfies the
// journal check while saying nothing about a seal: an operator who interrupted
// `billet local restore --from X` and then ran `billet local recover --from X`
// would otherwise skip the quiesce on the strength of somebody else's fence.
// Execute would still refuse — it cannot replace a fence carrying another
// reason — but that is a second mechanism catching a first one's wrong answer,
// and the first one should not be wrong.
func recoveryStageOf(stateDir string, a *deployarchive.Archive) (recoveryStage, error) {
	prog, err := deployarchive.InProgress(stateDir)
	if err != nil {
		return recoverFresh, err
	}

	// THIS ARCHIVE AND THIS OPERATION. A restore's journal for the same archive is
	// not a recovery to resume: it records a different Created list and no
	// superseded ledger, and treating it as one would skip the seal and the
	// quiesce on the strength of somebody else's work.
	if !prog.IsFor(a) || !prog.IsFrom(deployarchive.ReplaceLedger) {
		return recoverFresh, nil
	}

	// THE FENCE IS NOT CONSULTED HERE, deliberately. A published recovery is
	// finished the same way whether or not its fence is still standing: the last
	// two steps take the fence down and then remove the journal, so a crash
	// between them leaves exactly this — published, unfenced, journal present —
	// and it is the record, not the fence, that says what is left to do.
	if prog.Published() {
		return recoverPublished, nil
	}

	reason, fenced, err := state.MaintenanceFenceReason(stateDir)
	if err != nil {
		return recoverFresh, err
	}

	if fenced && reason == deployarchive.RecoverFenceReason {
		return recoverResume, nil
	}

	return recoverFresh, nil
}

// sealRecoveredDeployment closes the RESTORED ledger to new work.
//
// AN OPERATOR'S SEAL, not a shutdown's: `billet local up` clears a `local-down`
// seal on its next successful run, and a deployment whose nodes hold unaccounted
// compute must not be reopened by starting a service. Provenance is what carries
// that, and takeTheSeal already writes the operator one.
//
// A FAILURE HERE IS A PARTIAL SUCCESS, and it exits non-zero saying which half
// happened. The deployment IS recovered — every credential is in place — and it
// is also open for business, which is the one state this command must not leave
// behind silently.
func sealRecoveredDeployment(ctx context.Context, cfg *config.Config, o recoverOptions) error {
	// THE ONE HANDLE THAT CROSSES A FENCE, and this process is the one that
	// raised it. OpenAdmin honours the fence — correctly, since its whole job is
	// keeping other commands out — so it cannot be used here; OpenMaintenance is
	// the typed crossing, and it takes the directory lock, which Execute has by
	// now released. It also migrates, which is what the next `billet server`
	// start would do to an older archive's ledger anyway.
	db, err := openStateMaintenance(ctx, cfg)
	if err != nil {
		return recoveredButOpen(fmt.Errorf("open the restored ledger to seal it: %w", err))
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return recoveredButOpen(fmt.Errorf("read the restored ledger's admission: %w", err))
	}

	reason := o.reason
	if reason == "" {
		reason = "billet local recover"
	}

	if _, err := takeTheSeal(ctx, db, current, reason); err != nil {
		return recoveredButOpen(err)
	}

	return nil
}

// recoveredButOpen is the half-and-half answer, said in those words.
//
// THE DIRECTORY IS STILL FENCED WHEN THIS IS REACHED, and that is what makes the
// failure safe rather than the worst outcome available: the ledger is back and
// its admission row is the archive's — open — but nothing can start on a fenced
// directory, so no work can be admitted against it. Re-running the command
// resumes, seals, and clears the fence.
func recoveredButOpen(cause error) error {
	fmt.Println()
	fmt.Printf("warn     this deployment IS recovered, and it is NOT yet sealed: %v\n", cause)
	fmt.Printf("         The restored ledger carries the admission it had when the backup was\n")
	fmt.Printf("         taken, which is open — so a control plane starting on it would take\n")
	fmt.Printf("         new work while its nodes still hold compute it has never heard of.\n")
	fmt.Printf("         The state directory is STILL FENCED, so nothing can start on it.\n")
	fmt.Printf("         Run this command again; it resumes, seals, and lifts the fence.\n")

	return &exitError{code: 1, msg: "recovered and fenced, but the deployment could not be sealed"}
}

// quiesceForRecovery seals this deployment and establishes that it is holding
// nothing — or names exactly what it is holding.
//
// SEALING IS NOT THE AUTHORITY; THE BARRIER IS. A seal stops new work being
// ADMITTED and says nothing about what is already running, and it does not take
// effect at the instant it is run — a listener learns about it on its next poll,
// and an offer accepted just before that is real work with real compute behind
// it. So what decides is `alloc.Quiescence`, which is what `billet drain --wait`
// already asks.
//
// THE SEAL IS NOT LIFTED AFTERWARDS, deliberately. A recovered deployment must
// not start taking work the moment this returns: its nodes still hold compute
// the restored ledger has never heard of, and that compute has to be proved gone
// first. `billet resume` is the operator saying they have done that.
func quiesceForRecovery(ctx context.Context, cfg *config.Config, o recoverOptions) error {
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	reason := o.reason
	if reason == "" {
		reason = "billet local recover"
	}

	sealed, err := takeTheSeal(ctx, db, current, reason)
	if err != nil {
		return err
	}

	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	q, err := allocator.Quiescence(ctx)
	if err != nil {
		return err
	}

	if q.Quiet() {
		fmt.Printf("\nThis deployment is sealed and the ledger records no outstanding lease.\n")

		return nil
	}

	// STILL HOLDING WORK, and there are exactly two ways forward: wait for it, or
	// accept losing it BY NAME. Neither is chosen for the operator.
	if !o.acceptJobs {
		fmt.Printf("\n%s\n", outstandingSummary(q))

		// THE LEDGER BARRIER ONLY, DELIBERATELY. `drain --wait` and `local down`
		// take the compute proof as a second stage; this command is the disaster
		// path — the controller is sick and hosts may be unreachable — and a proof
		// no host can produce would make it unusable in exactly the situation it
		// exists for. What protects an operator here is the same thing it always
		// was: every job this would strand is NAMED, and losing them has to be
		// accepted by name.
		return waitForQuiet(ctx, db, cfg, sealed.Generation, waitOptions{
			timeout: o.timeout, withoutProof: true,
		})
	}

	fmt.Printf("\n--accept-failing-jobs: this recovery will strand the following, and GitHub\n")
	fmt.Printf("does not requeue a job that has already started:\n\n")

	for _, held := range q.Outstanding {
		fmt.Printf("  %s  tier %s", held.ID, held.Tier)

		if held.Node != "" {
			fmt.Printf("  node %s", held.Node)
		}

		if held.RunID != "" {
			fmt.Printf("  run %s", held.RunID)
		}

		fmt.Printf("  (%s since %s)\n", held.Phase, held.Since)
	}

	// WHAT THE BARRIER CANNOT SEE, said here because this is the moment somebody
	// acts on it. A lease that has already gone leaves compute nothing in the
	// ledger accounts for, and the list above cannot include it.
	fmt.Printf("\nCompute whose lease has already gone is NOT in that list — the ledger cannot\n")
	fmt.Printf("see it. `billet leases` and each node's own inventory are what confirm a host\n")
	fmt.Printf("is idle.\n")

	return nil
}

// printRecoverPlan reports what would happen, item by item.
func printRecoverPlan(p deployarchive.Plan) {
	for _, a := range p.Actions {
		switch a.Disposition {
		case deployarchive.AlreadyPresent:
			fmt.Printf("plan     %s is already in place at %s\n", a.What, a.Path)
		case deployarchive.SupersedeLedger:
			fmt.Printf("plan     REPLACE the ledger at %s\n", a.Path)
			fmt.Printf("         the one there now moves to %s and is never deleted — it is the\n",
				p.Superseded)
			fmt.Printf("         only record of the work this recovery fails\n")
		case deployarchive.ReplaceEmptyLedger:
			fmt.Printf("plan     replace the empty preflight ledger at %s\n", a.Path)
		case deployarchive.Install:
			fmt.Printf("plan     install %s at %s\n", a.What, a.Path)
		}
	}
}

// printRecovered reports a finished recovery, and the two things it does not
// settle.
func printRecovered(a *deployarchive.Archive, t deployarchive.Target,
	res deployarchive.Result,
) {
	fmt.Println()
	fmt.Printf("Recovered deployment %s in %s from the backup taken %s.\n\n",
		a.Manifest.DeploymentID, t.StateDir, a.Manifest.CreatedAt)

	for _, path := range res.Superseded {
		fmt.Printf("  the ledger that was there is at %s\n", path)
	}

	fmt.Printf("\nTHIS DEPLOYMENT IS STILL SEALED, and that is deliberate. Its nodes hold\n")
	fmt.Printf("compute the restored ledger has never heard of, and node recovery destroys an\n")
	fmt.Printf("instance with no lease as an orphan. Prove that compute gone, then:\n\n")
	fmt.Printf("  billet resume\n\n")
	fmt.Printf("`billet local up` starts the services; admission stays closed until you\n")
	fmt.Printf("reopen it, because this seal is an operator's and `up` does not clear one.\n")
}

// runRecoverAbandon undoes an interrupted recovery.
//
// THE LEDGER IT MOVED ASIDE COMES BACK. An abandon that only removed what the
// run created would take away the ledger it installed and leave the operator's
// own under a name nothing looks at, in a directory whose fence is about to come
// down — a deployment with no capacity record at all.
func runRecoverAbandon(ctx context.Context, a *deployarchive.Archive,
	t deployarchive.Target,
) error {
	pending, err := restoreUnfinished(t.StateDir)
	if err != nil {
		return err
	}

	if !pending {
		fmt.Printf("No recovery is in progress in %s; there is nothing to abandon.\n", t.StateDir)

		return nil
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

	res, err := deployarchive.Abandon(ctx, a, t, deployarchive.ReplaceLedger)

	for _, path := range res.Removed {
		fmt.Printf("remove   %s\n", path)
	}

	for _, path := range res.Restored {
		fmt.Printf("restore  %s put back\n", path)
	}

	for _, path := range res.Kept {
		fmt.Printf("keep     %s was left alone; billet could not prove it is one of its own\n", path)
	}

	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("Abandoned. %s is no longer fenced, and this deployment is still SEALED —\n",
		t.StateDir)
	fmt.Printf("`billet resume` is what reopens it.\n")

	return nil
}
