package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/lifeops/launchd"
	"github.com/junioryono/billet/internal/state"
)

// rollbackGrace bounds the unwinding of a failed run. It is deliberately short:
// the only work here is `systemctl disable`, and an operator who interrupted
// this is waiting on their terminal.
const rollbackGrace = 30 * time.Second

// orNoAnswer renders a manager's empty answer as what it is.
func orNoAnswer(v string) string {
	if v == "" {
		return "(no answer)"
	}

	return v
}

// converger is what `up` needs from a host. It is an interface rather than the
// concrete type so the ORDER of what this command does — check before start,
// server before node, ownership either side of the check, enablement only after
// readiness — can be asserted against a recording fake. Those orderings are the
// entire safety content of the command, and against a real host none of them
// can be observed without root and a service manager.
//
// It is also what keeps the command layer from being systemd-only. The ORDER is
// shared; the vocabulary is not — so the service identifiers and the commands a
// refusal tells an operator to run come from the backend rather than from
// deploy's unit-name constants.
type converger interface {
	Plan(ctx context.Context, req lifeops.UpRequest) (lifeops.UpPlan, error)
	Services() (server, node string)
	Running(ctx context.Context, req lifeops.UpRequest) ([]lifeops.RunningFacts, error)
	EnablementCmd(units ...string) string
	DisableCmd(unit string) string
	ManagerName() string
	CollateralNote() string
	Identity(req lifeops.UpRequest) (int, int, error)
	ApplyOwnership(changes []lifeops.OwnershipChange, uid, gid int) error
	RepairServerState(dir string, uid, gid int) ([]string, error)
	RepairPaths(dir string, targets []lifeops.RepairTarget, uid, gid int) ([]string, error)
	Revalidate(ctx context.Context, req lifeops.UpRequest, want lifeops.UnitPlan) error
	StartAndProve(ctx context.Context, unit string) (string, error)
	Snapshot(ctx context.Context, unit string) (string, error)
	ProveStable(ctx context.Context, unit string) error
	EnabledNow(ctx context.Context, unit string) (lifeops.Enablement, error)
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	StopAndProve(ctx context.Context, unit string) (lifeops.StopResult, error)
}

// converge and check are the two seams. `up` is the one command in billet that
// starts services on an operator's organization, so what it does and in what
// order is tested rather than reasoned about.
var (
	// THE BACKEND IS CHOSEN BY THE PLATFORM, and only here. Everything above
	// this line is one piece of code that imposes one ORDER on both managers;
	// everything below knows which manager it is.
	converge = func(opts ...lifeops.ConvergeOption) converger {
		if hostOS == "darwin" {
			// NO OPTIONS ARE DROPPED SILENTLY. These configure the systemd
			// converger and have no launchd counterpart; nothing passes one
			// today, and a caller that starts to would otherwise have it
			// discarded here with nothing to say so.
			if len(opts) > 0 {
				panic("billet: lifeops.ConvergeOption has no launchd equivalent; give the " +
					"launchd backend its own option rather than passing this one through")
			}

			return launchd.New()
		}

		return lifeops.NewConverger(lifeops.NewInspector(), opts...)
	}
	check = runCheck
	// lifecycleLock is a seam for the same reason the three above are: the lock
	// lives in a directory only root can create, and every ordering these
	// commands are responsible for is on the far side of taking it. The lock
	// itself is exercised against a real directory in hostlock_test.go.
	lifecycleLock = takeHostLock
)

// cmdLocalUp brings this machine's billet services up.
//
// WHAT IT WILL NOT DO IS THE DESIGN. It writes no unit files, creates no
// account, restarts nothing that is already running, and starts nothing until
// `billet check` has PROVED the GitHub credential rather than merely failing to
// disprove it. Everything it refuses, it refuses with the command that fixes it.
//
// The order is the contract: check first, then the server, then the node — and
// each service is started, proved to have held its process, and only then
// enabled. `enable --now` would commit a unit to every future boot before
// anything established it can run at all.
func cmdLocalUp(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local up")
	cfgPath := addServiceConfigFlag(fs)
	dryRun := fs.Bool("dry-run", false,
		"report what would change and refuse nothing — no service is started, enabled "+
			"or chowned")
	if err := parse(fs, args); err != nil {
		return err
	}

	return runLocalUp(ctx, upOptions{configPath: *cfgPath, dryRun: *dryRun})
}

// upOptions is what runLocalUp acts on.
//
// servicePath exists because the packaged path is a constant a test cannot
// write to, and every ordering this command is responsible for lives on the far
// side of the refusal that compares against it.
type upOptions struct {
	configPath  string
	servicePath string
	dryRun      bool
}

func runLocalUp(ctx context.Context, o upOptions) error {
	if o.servicePath == "" {
		o.servicePath = initconfig.ServiceConfigPathFor(hostOS)
	}

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	req := lifeops.UpRequest{
		ConfigPath:     o.configPath,
		ServiceUser:    initconfig.ServiceGroup,
		ServiceGroup:   initconfig.ServiceGroup,
		ServerStateDir: serverStateDir(cfg),
		NodeStateDir:   nodeStateDir(cfg),
		NodeLockDir:    nodeLockDir(cfg),
		WantServer:     cfg.Server != nil,
		WantNode:       cfg.Node != nil,
	}
	if cfg.GitHub != nil {
		req.KeyPath = cfg.GitHub.PrivateKeyPath
	}

	c := converge()

	// THE CONFIG MUST BE THE ONE THE UNITS READ, and this costs nothing to
	// establish. Computed BEFORE the host is asked anything, so a machine where
	// systemd cannot be reached still gets told what is wrong with its
	// configuration rather than an error about systemctl.
	refusals := profileRefusals(c, o.configPath, o.servicePath, req)

	plan, planErr := c.Plan(ctx, req)
	if planErr != nil {
		if len(refusals) == 0 {
			return planErr
		}

		// NEITHER FINDING HIDES THE OTHER. The systemd failure joins the list
		// rather than replacing it.
		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("billet could not ask %s about its services: %v",
				c.ManagerName(), planErr),
			Remedy: fmt.Sprintf("resolve the above, then run this again on a host running %s",
				c.ManagerName()),
		})

		return refused(refusals)
	}

	refusals = append(refusals, plan.Refusals...)
	if len(refusals) > 0 {
		return refused(refusals)
	}

	printPlan(plan)

	if o.dryRun {
		fmt.Println("\nNothing was changed (--dry-run).")

		return nil
	}

	// THE SAME LOCK `down` TAKES, and taken HERE rather than at the top: nothing
	// above this changes anything, and a config that the packaged units cannot
	// use should be reported as such rather than as a failure to create a lock
	// directory. A --dry-run never reaches it, which is right — it mutates
	// nothing, so it excludes nothing.
	//
	// Without this the exclusion would be one-sided: a `down` that has proved the
	// host idle and is about to stop it could be overtaken by an `up` starting the
	// services again, and neither command has any way to notice.
	lock, err := lifecycleLock()
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Printf("warn     could not release the lifecycle lock: %v\n", err)
		}
	}()

	uid, gid, err := c.Identity(req)
	if err != nil {
		return err
	}

	if err := c.ApplyOwnership(plan.Ownership, uid, gid); err != nil {
		return err
	}

	// PROVEN, NOT MERELY UNREFUTED. `billet check` reports an unreachable GitHub
	// as advisory and exits 0, which is right for a diagnostic and wrong as a
	// precondition for starting a control plane on somebody's organization.
	fmt.Println()
	report, err := check(ctx, checkOptions{configPath: o.configPath})
	if err != nil {
		return err
	}
	if req.WantServer && report.github != githubVerified {
		return fmt.Errorf("the GitHub App was not verified (%s), and `billet local up` will not "+
			"start a control plane on a credential nothing has proved. `billet check` reports "+
			"this as advisory because it is a diagnostic; starting a service is not. Re-run when "+
			"GitHub is reachable, and note that BILLET_MAINTENANCE=1 skips the check entirely",
			report.github)
	}

	// THE CHECK RAN AS THIS PROCESS, and opening the ledger created state as
	// whoever that was. Measured on systemd 255: StateDirectory= repairs
	// ownership recursively when the TOP directory's owner is wrong, and does
	// nothing at all when it is already correct — which is every run after the
	// first. Without this the server is handed a root-owned ledger it cannot
	// write, by the very command that was making the host ready.
	if err := c.ApplyOwnership(plan.Ownership, uid, gid); err != nil {
		return err
	}

	repaired, err := c.RepairServerState(plan.ServerState, uid, gid)
	if err != nil {
		return err
	}
	for _, path := range repaired {
		fmt.Printf("own      %s given back to %s (the preflight opened the ledger as this user)\n",
			path, initconfig.ServiceGroup)
	}

	if err := startUnits(ctx, c, req, plan); err != nil {
		return err
	}

	return clearShutdownSeal(ctx, cfg, req)
}

// clearShutdownSeal reopens admission if — and only if — a shutdown is what
// closed it.
//
// THE PROVENANCE IS THE WHOLE POINT. An operator's seal survives this: somebody
// quiesced that deployment deliberately, and reopening it because a service was
// restarted would admit work into a maintenance window, silently, with the
// operator's evidence being a job running during it. state.Resume enforces that
// inside its own transaction; this reports it.
//
// IT RUNS LAST, after the services are up and proved. Reopening admission before
// there is anything to serve it would advertise capacity to GitHub that nothing
// is listening for.
//
// A LEDGER FAILURE HERE IS A PARTIAL SUCCESS, NOT A SUCCESS. The services really
// are running, so this is not "the host did not come up" — but exiting 0 with
// admission still sealed is the exact "up and taking nothing" state provenance
// exists to prevent, and a script that brings a host up and moves on would move
// on. It exits non-zero and says which half happened.
//
// AN OPERATOR'S SEAL IS THE EXCEPTION, and it is a real success: leaving it is
// the correct outcome, decided deliberately, and nothing is left half done.
func clearShutdownSeal(ctx context.Context, cfg *config.Config, req lifeops.UpRequest) error {
	if !req.WantServer {
		return nil
	}

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return upButSealed(fmt.Errorf("open the ledger to reopen admission: %w", err))
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return upButSealed(fmt.Errorf("read admission: %w", err))
	}

	if current.Mode == state.AdmissionOpen {
		return nil
	}

	resumed, err := db.Resume(ctx, state.ResumeRequest{
		Expect: current.Generation,
		Clears: state.ProvenanceLocalDown,
		Actor:  actor(),
	})
	if err != nil {
		if errors.Is(err, state.ErrAdmissionProvenance) {
			fmt.Printf("\nseal     this deployment is still sealed, and NOT by a shutdown%s, so\n",
				byWhom(current))
			fmt.Printf("         `billet local up` has left it alone. It takes no work until\n")
			fmt.Printf("         somebody runs `billet resume`.\n")

			return nil
		}

		return upButSealed(err)
	}

	fmt.Printf("\nseal     the shutdown seal is cleared at generation %d; this deployment is\n",
		resumed.Generation)
	fmt.Printf("         taking work again\n")

	return nil
}

// upButSealed is "the services are up and this deployment still takes nothing".
//
// Its own status because a caller acts on it differently from both outcomes it
// sits between: the host is not broken, and it is not finished either.
func upButSealed(cause error) error {
	fmt.Printf("\nseal     the services are up, but admission could not be reopened: %v\n", cause)
	fmt.Printf("         this deployment takes no work until `billet resume` runs\n")

	return &exitError{
		code: 2,
		msg: "the services are running but admission is still sealed, so this deployment " +
			"takes no work: " + cause.Error(),
	}
}

// startUnits starts, proves and enables each unit in order, and unwinds the
// enablement this run performed if a later one fails.
func startUnits(ctx context.Context, c converger, req lifeops.UpRequest,
	plan lifeops.UpPlan,
) error {
	var enabled []string

	// THE UNWINDING MUST OUTLIVE WHAT CAUSED IT. A context cancelled by the
	// operator's own Ctrl-C is the likeliest way to land here, and rolling back
	// on that context would fail every disable and leave the host committed to
	// booting services nothing proved.
	// asFound is the enablement this command arrived to, so the unwinding can
	// prove it put the host back rather than assume it.
	asFound, err := enablementOfBoth(ctx, c)
	if err != nil {
		return err
	}

	rollback := func(cause error) error { return unwind(ctx, c, enabled, asFound, cause) }

	for _, unit := range plan.Units {
		fmt.Println()

		// THE PLAN IS OLD BY NOW. `billet check` spent as long on the network as
		// the network took, and in that time a unit can be edited and
		// daemon-reloaded into something billet never validated, or a drain can
		// have begun. Asked again immediately before acting, because acting on
		// the older answer is how a converge starts a service it did not check.
		if err := c.Revalidate(ctx, req, unit); err != nil {
			return rollback(err)
		}

		// SOME MANAGERS CANNOT START A SERVICE THAT IS NOT ENABLED, and where
		// that is so these two steps happen in the other order. The backend
		// declares it rather than this deciding; what is decided here is that it
		// is SAID, because it costs a real guarantee — on this path the service
		// is committed to boot before anything proves it can run, and only the
		// unwinding puts that back.
		if unit.Enable && unit.EnableBeforeStart {
			if err := enableUnit(ctx, c, unit, &enabled); err != nil {
				return rollback(err)
			}

			fmt.Printf("enable   %s %s\n", unit.Name, orDefaultDetail(unit))
			fmt.Printf("         (this manager cannot start a service that is not enabled, so " +
				"this had to happen BEFORE the start below rather than after it; if the start " +
				"fails, this is undone)\n")
		}

		if unit.Start {
			// WHAT ELSE IS RUNNING, BEFORE THIS START. A unit billet refused to
			// let name the other one can still reach it at a remove — through
			// something it pulls in — and no amount of property-reading models
			// systemd's transaction. Comparing before and after does not have to:
			// a service that moved while billet was starting a different one is a
			// service billet disturbed, whatever the mechanism.
			bystanders, err := snapshotOthers(ctx, c, unit.Name)
			if err != nil {
				return rollback(err)
			}

			fmt.Printf("start    %s\n", unit.Name)

			// WHAT WAS PROVED IS THE BACKEND'S SENTENCE, because the two managers
			// prove different things and the difference matters. systemd's units
			// are Type=notify, so a successful start means billet's own process
			// reached READY=1 and then held its pid through a settle window.
			// launchd has no readiness notification at all, so all that can be
			// established there is that one process survived the window — and
			// printing "ready" for both would tell a Mac operator something
			// nothing checked.
			proof, err := c.StartAndProve(ctx, unit.Name)
			if err != nil {
				return rollback(err)
			}
			if err := proveOthersUndisturbed(ctx, c, bystanders, unit.Name); err != nil {
				return rollback(err)
			}
			fmt.Printf("         %s\n", proof)
		} else {
			fmt.Printf("start    %s is already running; left alone (a restart is a drain)\n", unit.Name)

			// THE PLAN'S SAMPLE IS OLD BY NOW: `billet check` talked to GitHub in
			// between. Enabling on it would commit a service that has since begun
			// crash-looping to every future boot.
			if unit.Enable {
				if err := c.ProveStable(ctx, unit.Name); err != nil {
					return rollback(err)
				}
			}
		}

		switch {
		case !unit.Enable:
			fmt.Printf("enable   %s is already enabled\n", unit.Name)

			continue

		case unit.EnableBeforeStart:
			// Done above, before the start, and reported there.
			continue
		}

		if err := enableUnit(ctx, c, unit, &enabled); err != nil {
			return rollback(err)
		}

		fmt.Printf("enable   %s %s\n", unit.Name, orDefaultDetail(unit))
	}

	fmt.Println("\nUp. `billet local status` reports what this machine is running.")

	return nil
}

// enableUnit commits ONE service to start at boot, and records whether this run
// is what committed it.
//
// EXTRACTED SO IT CAN HAPPEN ON EITHER SIDE OF THE START. systemd can start a
// disabled unit, so `up` proves the service runs and only then commits it;
// launchd cannot, so there the same steps happen in the other order. Everything
// this does — refusing a state billet has no rule for, reading back what the
// enable actually changed, and recording only a transition this run performed —
// is identical either way, and duplicating it for the second order is how the
// two copies would drift.
func enableUnit(ctx context.Context, c converger, unit lifeops.UnitPlan,
	enabled *[]string,
) error {
	// WHOSE ENABLEMENT IS THIS? `systemctl enable` is idempotent, so its
	// success says nothing about who performed the transition. Another
	// operator may have enabled the unit while `billet check` was talking to
	// GitHub, and rolling THAT back would disable a service somebody else
	// just committed. Only a unit observed not-enabled immediately before is
	// this run's to undo.
	was, err := c.EnabledNow(ctx, unit.Name)
	if err != nil {
		return (err)
	}

	// ONLY "disabled" IS PERMISSION TO ENABLE. Anything else — masked,
	// static, linked, enabled-runtime, an empty answer, or a state systemd
	// adds after this was written — is a unit whose enablement billet cannot
	// account for, and the plan's older answer is not evidence about it.
	// ONLY A DEFINITE "not enabled" IS PERMISSION. Every state billet has no
	// rule for — systemd's masked, static, linked, enabled-runtime, an
	// empty answer, a word a future version adds, or a launchd label whose
	// override database billet could not read — arrives here as Unknown,
	// and Unknown refuses rather than being guessed at.
	if was.Enabled != lifeops.No {
		return (fmt.Errorf("%s became %q between the plan and now, and billet only "+
			"enables a service it finds disabled; nothing was committed for it. Resolve it "+
			"with `%s` and run this again",
			unit.Name, orNoAnswer(was.How), c.EnablementCmd(unit.Name)))
	}

	// AND WHAT ELSE IS ENABLED. `[Install] Also=` makes `systemctl enable`
	// write links for another unit, which on a node-only host would commit an
	// unverified control plane to every future boot — and is invisible in
	// every property systemd reports about the unit itself.
	enablementBefore, err := enablementOfBoth(ctx, c)
	if err != nil {
		return (err)
	}

	enableErr := c.Enable(ctx, unit.Name)

	// WHAT ACTUALLY CHANGED, AFTER EVERY OUTCOME. An enable that FAILED can
	// still have written links — for this unit and, through `[Install]
	// Also=`, for the other one — so the state of both is read back rather
	// than inferred from an exit status. On a live context, because the
	// likeliest cause of a failure here is the operator's own interrupt,
	// which is exactly when the caller's context can no longer answer.
	undo, cancel := liveFor(ctx)
	after, readErr := enablementOfBoth(undo, c)
	cancel()

	if readErr != nil {
		server, node := c.Services()

		return (errors.Join(enableErr, fmt.Errorf("and billet could not tell what "+
			"that changed: %w. Check with `%s`",
			readErr, c.EnablementCmd(server, node))))
	}

	// Only a unit that WENT from disabled to enabled is this run's to undo —
	// including the other one, if `Also=` took it along.
	for name, was := range enablementBefore {
		if was.Enabled == lifeops.No && after[name].Enabled == lifeops.Yes {
			*enabled = append(*enabled, name)
		}
	}

	if enableErr != nil {
		if after[unit.Name].Enabled == lifeops.Unknown {
			enableErr = errors.Join(enableErr, fmt.Errorf("and %s is now %q, which billet "+
				"did not put it in and will not undo", unit.Name, after[unit.Name].How))
		}

		return (enableErr)
	}

	// AND THE ONE THAT WAS ASKED FOR ACTUALLY TOOK. A zero exit from
	// `systemctl enable` is not the same fact as the unit being enabled: a
	// unit left `disabled`, `indirect`, or an answer billet cannot read
	// would otherwise be reported as "will start at boot" and the run would
	// succeed having committed nothing.
	if after[unit.Name].Enabled != lifeops.Yes {
		return (fmt.Errorf("enabling %s succeeded but it is %q rather than enabled, "+
			"so nothing was committed to boot. Check it with `%s`",
			unit.Name, orNoAnswer(after[unit.Name].How), c.EnablementCmd(unit.Name)))
	}

	if err := changedBeyondTheOneAsked(c, enablementBefore, after, unit.Name); err != nil {
		return (err)
	}

	return nil
}

// orDefaultDetail renders what enabling a service does, in this manager's own
// words where it has any.
//
// The systemd sentence is not the launchd one: a unit gains links that make
// systemd start it at boot, while a launch agent gains a file in
// ~/Library/LaunchAgents and loses a disabled override. Telling a Mac operator
// about links describes a mechanism their machine does not have.
func orDefaultDetail(unit lifeops.UnitPlan) string {
	if unit.Detail != "" {
		return unit.Detail
	}

	return "will start at boot"
}

// liveFor is a context for work that must happen BECAUSE the caller's context
// ended. An operator's Ctrl-C is the likeliest way into unwinding, and a
// disable — or the query that decides whether to issue one — inherits that
// cancellation and fails, leaving the host committed to booting a service
// nothing proved.
func liveFor(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackGrace)
}

// unwind undoes the enablement one run performed, newest first.
func unwind(ctx context.Context, c converger, enabled []string,
	found map[string]lifeops.Enablement, cause error,
) error {
	if len(enabled) == 0 {
		return cause
	}

	undo, cancel := liveFor(ctx)
	defer cancel()

	errs := []error{cause}
	for i := len(enabled) - 1; i >= 0; i-- {
		unit := enabled[i]
		if err := c.Disable(undo, unit); err != nil {
			errs = append(errs, fmt.Errorf("could not undo enabling %s: %w", unit, err))

			continue
		}
		fmt.Printf("         (undid enabling %s)\n", unit)
	}

	// AND WHAT THE UNDOING ITSELF DID. `systemctl disable` follows `[Install]
	// Also=` as well, so undoing one unit's enablement can remove another's —
	// including one an operator had before this command ran. billet cannot stop
	// that; it can refuse to leave them unaware of it.
	if after, err := enablementOfBoth(undo, c); err != nil {
		errs = append(errs, fmt.Errorf("and billet could not confirm what the undo left "+
			"behind: %w", err))
	} else {
		// THE WHOLE STATE, not just the verdict. `masked` and `static` are both
		// states billet has no rule for, so comparing verdicts alone reports
		// `masked -> static` as nothing having happened — and the point of
		// comparing before and after is to notice a change billet cannot model.
		for name, was := range found {
			if after[name] != was {
				errs = append(errs, fmt.Errorf("undoing this run left %s %q, where it was %q "+
					"before the command ran, so it removed more than this run had committed",
					name, after[name].How, was.How))
			}
		}
	}

	return errors.Join(errs...)
}

// snapshotOthers records every OTHER service in the plan that is running now.
func snapshotOthers(ctx context.Context, c converger, starting string) (map[string]string, error) {
	seen := map[string]string{}

	// BOTH OF BILLET'S UNITS, not only the ones this run planned. A node-only
	// host does not plan the server at all — which is precisely the case where a
	// transitive `Requires=` starting it matters, because the GitHub proof was
	// skipped on the grounds that no control plane was going to start.
	server, node := c.Services()

	for _, other := range []string{server, node} {
		if other == starting {
			continue
		}

		snap, err := c.Snapshot(ctx, other)
		if err != nil {
			return nil, fmt.Errorf("look at %s before starting %s: %w", other, starting, err)
		}
		seen[other] = snap
	}

	return seen, nil
}

// enablementOfBoth reads the enablement of both billet units.
func enablementOfBoth(ctx context.Context, c converger) (map[string]lifeops.Enablement, error) {
	seen := map[string]lifeops.Enablement{}

	server, node := c.Services()

	for _, unit := range []string{server, node} {
		enablement, err := c.EnabledNow(ctx, unit)
		if err != nil {
			return nil, fmt.Errorf("look at whether %s is enabled: %w", unit, err)
		}
		seen[unit] = enablement
	}

	return seen, nil
}

// changedBeyondTheOneAsked refuses if enabling one unit changed another.
func changedBeyondTheOneAsked(c converger, before, after map[string]lifeops.Enablement,
	enabled string,
) error {
	for name, was := range before {
		if name == enabled {
			continue
		}

		if now := after[name]; now != was {
			return fmt.Errorf("enabling %s also made %s %q, where it was %q. billet did not ask "+
				"for that: %s. Undo it with `%s` and restore the packaged service definition",
				enabled, name, now.How, was.How, c.CollateralNote(), c.DisableCmd(name))
		}
	}

	return nil
}

// proveOthersUndisturbed refuses if starting one service moved another.
func proveOthersUndisturbed(ctx context.Context, c converger, before map[string]string,
	started string,
) error {
	for name, was := range before {
		now, err := c.Snapshot(ctx, name)
		if err != nil {
			return fmt.Errorf("look at %s after starting %s: %w", name, started, err)
		}

		if now != was {
			return fmt.Errorf("starting %s disturbed %s, which was %s and is now %s. billet did "+
				"not ask for that and will not continue: if %s was holding jobs, they are "+
				"gone. Something in %s's transaction reaches %s — check its dependencies "+
				"and the dependencies of what it pulls in",
				started, name, was, now, name, started, name)
		}
	}

	return nil
}

// profileRefusals rejects a config whose paths the packaged units cannot use.
//
// THE STATE DIRECTORIES ARE NOT CHECKED HERE: each backend knows which
// directories its own manager creates and makes writable — systemd declares them
// in the unit, and launchd declares none at all and creates nothing — so that
// comparison belongs where the manager's own answer is in hand rather than
// against a constant that could drift from it.
func profileRefusals(c converger, cfgPath, servicePath string,
	req lifeops.UpRequest,
) []lifeops.Refusal {
	var refusals []lifeops.Refusal

	if cfgPath != servicePath {
		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("the services read %s, and this is %s", servicePath, cfgPath),
			Remedy: fmt.Sprintf("install this config at %s, or generate one there with "+
				"`billet init --profile local-service`", servicePath),
		})
	}

	// THE APP KEY HAS TO BE WHERE THE SERVICE READS IT, and the reason it must
	// is not the same on both platforms — which matters, because a refusal an
	// operator cannot make sense of is one they work around.
	//
	// On Linux the units set ProtectHome=true, so a key under a home directory
	// is literally invisible to the process that needs it while `billet check`
	// — running as the operator — reads it happily. A launch agent has no such
	// sandbox and runs as that same operator, so telling a Mac operator about
	// ProtectHome describes a mechanism their machine does not have. There the
	// reason is policy: the deployment's credential belongs beside the config
	// the services read, not in whichever home directory happened to run `init`.
	confDir := filepath.Dir(servicePath)
	if req.WantServer && req.KeyPath != "" && !lifeops.Contained(confDir, req.KeyPath) {
		why := "the units set ProtectHome=true, so a key under a home directory is readable " +
			"by you and invisible to the service — which fails at startup rather than here"
		if c.ManagerName() == "launchd" {
			why = "the services read their credential from beside their config, and a key " +
				"in a home directory belongs to whoever ran `init` rather than to the " +
				"deployment"
		}

		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("github.private_key_path is %s, which is outside %s",
				req.KeyPath, confDir),
			Remedy: fmt.Sprintf("move the App key into %s; %s", confDir, why),
		})
	}

	return refusals
}

func serverStateDir(cfg *config.Config) string {
	if cfg.Server == nil {
		return ""
	}

	return cfg.Server.IdentityDir
}

func nodeStateDir(cfg *config.Config) string {
	if cfg.Node == nil {
		return ""
	}

	return cfg.Node.StateDir
}

func nodeLockDir(cfg *config.Config) string {
	if cfg.Node == nil {
		return ""
	}

	return cfg.Node.LockDir
}

// refused renders every reason at once. An operator who has to re-run a command
// to discover the next thing wrong with their host is paying for a diagnostic
// that already knew.
func refused(refusals []lifeops.Refusal) error {
	var b strings.Builder

	b.WriteString("this host is not ready to bring billet up:\n")
	for _, r := range refusals {
		b.WriteString("\n  - ")
		b.WriteString(r.What)
		b.WriteString("\n    ")
		b.WriteString(r.Remedy)
	}

	return errors.New(b.String())
}

// printPlan reports what will change before anything does.
func printPlan(plan lifeops.UpPlan) {
	if len(plan.Ownership) == 0 && len(plan.Units) == 0 {
		fmt.Println("plan     nothing to change")

		return
	}

	for _, change := range plan.Ownership {
		fmt.Printf("plan     %s -> %s:%s %04o\n", change.Path, change.Owner, change.Group, change.Mode.Perm())
		fmt.Printf("         %s\n", change.Why)
	}
	for _, unit := range plan.Units {
		switch {
		case unit.Start && unit.Enable:
			fmt.Printf("plan     start and enable %s\n", unit.Name)
		case unit.Start:
			fmt.Printf("plan     start %s (already enabled)\n", unit.Name)
		case unit.Enable:
			fmt.Printf("plan     enable %s (already running)\n", unit.Name)
		default:
			fmt.Printf("plan     %s is already running and enabled\n", unit.Name)
		}
	}
}
