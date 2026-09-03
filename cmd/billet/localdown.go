package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
)

// downOptions is what runLocalDown acts on.
type downOptions struct {
	// locked says the caller already holds the host lifecycle lock, so this must
	// not take it again.
	//
	// MEASURED: a second flock on a separate descriptor in the same process is
	// DENIED (EWOULDBLOCK on darwin), so taking it again would refuse the command
	// outright with a message about another billet that does not exist. An
	// earlier version of this comment claimed the opposite — that the second
	// acquisition would silently succeed — which is wrong on this platform and
	// worth correcting, because the fix it argued for is the same either way:
	// `uninstall` holds ONE lock across the whole operation, since a `down` that
	// released it before the agents were removed would leave a window in which a
	// concurrent `up` bootstraps the node whose definition is about to be
	// deleted.
	locked bool

	configPath string
	reason     string
	timeout    time.Duration
	dryRun     bool
	force      bool
	// withoutProof stops after the LEDGER barrier rather than asking every host
	// what its provider is running. It is the named escape from a proof that
	// cannot always be obtained — a host that is off, or too old to answer, never
	// will — and taking it makes this command print a different conclusion.
	withoutProof bool
}

// cmdLocalDown takes this machine's billet services down without failing the
// work they are holding.
//
// IT IS A DRAIN THAT THEN STOPS, and the order is the whole design: seal
// admission so nothing new arrives, WAIT for what is running to finish, and only
// then stop anything. A `down` that stopped first would be `systemctl stop` with
// extra steps — and stopping a node mid-job fails somebody's build, because
// GitHub does not requeue a job whose runner vanished.
//
// THERE IS NO DEFAULT TIME LIMIT, deliberately. A job can run for hours and
// billet imposes no limit on one, so any default this command picked would be a
// guess that ends in killed work. --timeout is available for somebody who knows
// their fleet and wants a bound; without it this waits.
func cmdLocalDown(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local down")
	cfgPath := addServiceConfigFlag(fs)
	reason := fs.String("reason", "",
		"why this host is going down, recorded on the seal for whoever finds it sealed")
	timeout := fs.Duration("timeout", 0,
		"give up waiting for running work after this long (default: wait for as long as "+
			"the jobs take)")
	dryRun := fs.Bool("dry-run", false,
		"report what would happen and change nothing — nothing is sealed, stopped or disabled")
	force := fs.Bool("force", false,
		"stop services that are running a different billet build than this one")
	withoutProof := fs.Bool("without-compute-proof", false,
		"stop once the LEDGER is quiet, without asking each host what it is actually "+
			"running (faster, and it cannot see compute whose lease has already gone)")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *timeout < 0 {
		return fmt.Errorf("--timeout %s is negative; use a positive duration, or omit it to "+
			"wait for as long as the jobs take", *timeout)
	}

	return runLocalDown(ctx, downOptions{
		configPath: *cfgPath, reason: *reason, timeout: *timeout,
		dryRun: *dryRun, force: *force, withoutProof: *withoutProof,
	})
}

func runLocalDown(ctx context.Context, o downOptions) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	req := lifeops.UpRequest{
		ConfigPath: o.configPath,
		WantServer: cfg.Server != nil,
		WantNode:   cfg.Node != nil,
	}
	if cfg.GitHub != nil {
		req.KeyPath = cfg.GitHub.PrivateKeyPath
	}

	if !req.WantServer && !req.WantNode {
		return fmt.Errorf("%s declares neither a server nor a node, so there is nothing on "+
			"this machine to take down", o.configPath)
	}

	c := converge()

	running, err := c.Running(ctx, req)
	if err != nil {
		return err
	}

	// THE IDENTITY REFUSAL COMES FIRST, before anything is sealed. A unit running
	// a different build is a different installation of billet, and this command
	// does more than stop it — it writes a seal into a ledger that build is
	// using, on the assumption that the same binary will later clear it.
	if err := refuseForeignBuilds(running, o.force); err != nil {
		return err
	}

	if o.dryRun {
		printDownPlan(c, req, o)

		fmt.Println("\nNothing was changed (--dry-run).")

		return nil
	}

	// ONE LIFECYCLE COMMAND AT A TIME ON THIS HOST. Two of these interleaved is
	// one sealing while the other resumes, or an `up` starting the services this
	// one has just proved idle and is about to stop. The lock does not reach
	// another machine — a control plane elsewhere can still be resumed, which is
	// what the generation fence below covers.
	// THE CALLER MAY ALREADY HOLD IT. `uninstall` runs this and then removes the
	// agents, and those removals belong INSIDE the same exclusion: between a
	// `down` releasing the lock and an uninstall removing a plist, a concurrent
	// `up` can take the lock and bootstrap the node whose definition is about to
	// be deleted.
	if !o.locked {
		lock, err := lifecycleLock()
		if err != nil {
			return err
		}

		defer func() {
			if err := lock.release(); err != nil {
				fmt.Printf("warn     could not release the lifecycle lock: %v\n", err)
			}
		}()
	}

	generation, proved, err := sealForShutdown(ctx, c, cfg, req, running, o)
	if err != nil {
		return err
	}

	return stopAndDisable(ctx, c, cfg, req, generation, proved)
}

// serverIsRunning reports whether the control-plane unit has a live process.
//
// THE BARRIER IS ISSUED BY THE CONTROL PLANE, not by this command: the request
// is a durable row and the running server is what puts the question to each
// host. So on a host whose server is already stopped, waiting for the proof is
// waiting for nothing — and saying that is the difference between an operator
// who reaches for --without-compute-proof and one who watches a blank screen.
//
// "COULD NOT TELL" IS NOT "RUNNING". Unknown here means the proof may or may not
// be obtainable, and the honest thing is to try: a wait that names its blockers
// is recoverable, while skipping the proof on a guess is not.
func serverIsRunning(c converger, running []lifeops.RunningFacts) bool {
	server, _ := c.Services()

	for _, s := range running {
		if s.Name == server {
			return s.Active != lifeops.No
		}
	}

	return false
}

// refuseForeignBuilds stops this command acting on somebody else's installation.
// refuseForeignBuilds stops this command acting on somebody else's installation.
func refuseForeignBuilds(services []lifeops.RunningFacts, force bool) error {
	var refusals []lifeops.Refusal

	// ONLY WHAT IS PROVED NOT RUNNING IS WAVED THROUGH. A service billet has
	// established is inactive has no process whose identity could differ, and
	// refusing on one would block a `down` on a host that is already half
	// stopped — which is exactly when somebody reaches for this command.
	//
	// "Could not tell whether it is running" is NOT that. It was a bool once,
	// so an unanswered query became "not running" and skipped the check
	// entirely; the whole point of this refusal is the case where billet cannot
	// vouch for the process it is about to stop.
	for _, s := range services {
		if s.Active == lifeops.No || s.IsThisBuild == lifeops.Yes {
			continue
		}

		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("%s is running a build this is not sure is the same as this "+
				"billet (%s)", s.Name, orNoAnswer(s.Why)),
			Remedy: "check `billet local status`; run this from the build that is running, or " +
				"pass --force if you mean to stop somebody else's",
		})
	}

	if len(refusals) == 0 || force {
		return nil
	}

	return refused(refusals)
}

// sealForShutdown stops the deployment admitting work and waits for what is
// running, returning the admission generation this shutdown established.
//
// A NODE-ONLY HOST CANNOT DO EITHER, and says so rather than pretending. The
// seal is a row in the CONTROL PLANE's ledger and this machine does not have
// one, so nothing here can stop work being assigned to it: the node's own drain
// on SIGTERM is what protects the jobs already running, and only a
// `billet drain` against the control plane stops new ones arriving. Saying that
// plainly is the difference between an operator who runs the right command next
// and one who believes this host is safely fenced.
func sealForShutdown(ctx context.Context, c converger, cfg *config.Config, req lifeops.UpRequest,
	running []lifeops.RunningFacts, o downOptions,
) (int64, bool, error) {
	if !req.WantServer {
		fmt.Printf("seal     SKIPPED: this host runs a node and no control plane, so it has\n")
		fmt.Printf("         no admission ledger to seal. Stopping the node below drains the\n")
		fmt.Printf("         work already on it, and NOTHING HERE stops the control plane\n")
		fmt.Printf("         assigning more in the meantime — run `billet drain` against the\n")
		fmt.Printf("         control plane first if that matters.\n\n")
		fmt.Printf("         Nothing here can ask this host what it is running either: that\n")
		fmt.Printf("         question goes through the control plane, which is on another\n")
		fmt.Printf("         machine. `billet drain --wait` there covers this host too.\n\n")

		return 0, false, nil
	}

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return 0, false, fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("read admission: %w", err)
	}

	// A ROW BILLET CANNOT READ IS NOT ONE IT MAY REWRITE. Unknown is the
	// fail-closed value on purpose, and sealing over it would turn "I could not
	// tell what this says" into a local-down seal — which the next
	// `billet local up` then CLEARS, opening a deployment on the strength of a
	// row nothing understood. The same goes for a sealed row whose provenance is
	// not one this build issues: it belongs to something else.
	if current.Mode == state.AdmissionUnknown ||
		(current.Mode == state.AdmissionSealed &&
			current.Provenance != state.ProvenanceOperator &&
			current.Provenance != state.ProvenanceLocalDown) {
		return 0, false, fmt.Errorf("admission reads mode %q provenance %q, which billet does not "+
			"recognise, so it will not seal over it — a seal written here would be cleared by "+
			"the next `billet local up`. Resolve the admission row first; `billet status` "+
			"shows it", current.Mode, current.Provenance)
	}

	// AN OPERATOR'S SEAL IS LEFT ALONE. Replacing it with a local-down one would
	// mean the next successful `billet local up` cleared a seal somebody took
	// deliberately, silently reopening a deployment they had quiesced for their
	// own reasons.
	if current.Mode == state.AdmissionSealed && current.Provenance == state.ProvenanceOperator {
		fmt.Printf("seal     already held by an operator%s; leaving it as it is, so bringing\n",
			byWhom(current))
		fmt.Printf("         this host back up will NOT reopen admission\n\n")
	} else {
		reason := o.reason
		if reason == "" {
			reason = "billet local down"
		}

		sealed, err := db.Seal(ctx, state.SealRequest{
			Expect:       current.Generation,
			Provenance:   state.ProvenanceLocalDown,
			Reason:       reason,
			Actor:        actor(),
			KeepExisting: true,
		})
		if err != nil {
			return 0, false, err
		}

		fmt.Printf("seal     admission sealed at generation %d; `billet local up` clears it\n\n",
			sealed.Generation)

		current = sealed
	}

	// THE PROOF IS SKIPPED ONLY WHERE IT CANNOT BE OBTAINED, or where somebody
	// said to skip it. The barrier is issued by the CONTROL PLANE — the request is
	// a durable row and the running server is what asks each host — so on a
	// machine whose server is already stopped there is nobody to ask, and waiting
	// would be waiting for silence.
	withoutProof := o.withoutProof

	switch {
	case o.withoutProof:
		fmt.Printf("prove    SKIPPED by --without-compute-proof: no host will be asked what it\n")
		fmt.Printf("         is running, so the ledger being empty is all this establishes\n\n")
	case !serverIsRunning(c, running):
		fmt.Printf("prove    SKIPPED: the control plane on this host is not running, and it is\n")
		fmt.Printf("         what asks each host what it is holding. Start it and re-run to\n")
		fmt.Printf("         prove this machine idle, or accept what the ledger knows.\n\n")

		withoutProof = true
	}

	if err := waitForQuiet(ctx, db, cfg, current.Generation, waitOptions{
		timeout:      o.timeout,
		withoutProof: withoutProof,
	}); err != nil {
		return 0, false, err
	}

	return current.Generation, !withoutProof, nil
}

// stopAndDisable stops the node before the server, then commits both to staying
// down across a reboot.
//
// THE NODE GOES FIRST. It reports completions and custody to the control plane,
// so a server stopped first leaves the node talking to nothing — its work
// finishes into a void, its leases are reclaimed by a reaper that is not
// running, and what it holds becomes the control plane's problem on the next
// start. There is little left to report by this point, since the barrier is
// already quiet, but the ordering costs nothing and the failure it prevents is
// silent.
func stopAndDisable(ctx context.Context, c converger, cfg *config.Config, req lifeops.UpRequest,
	generation int64, proved bool,
) error {
	// THE FENCE, and it is the reason the barrier's answer is still worth
	// anything by the time we act on it. Between the wait returning and the first
	// unit stopping, somebody can resume admission and a listener can take a job
	// — so what was proved idle a moment ago is running a build. Re-reading the
	// generation is what makes "it was quiet" into "it is still the same quiet".
	//
	if req.WantServer {
		if err := stillOurs(ctx, cfg, generation); err != nil {
			return err
		}
	}

	// WHAT WAS ESTABLISHED, said before anything stops rather than after, and
	// never in the same words for the two cases. `billet drain --wait` carries the
	// same distinction and it matters more here: that command only waits, and this
	// one is about to stop the services.
	if proved {
		fmt.Printf("\nnote     the ledger records nothing outstanding, and every host billet\n")
		fmt.Printf("         expects an answer from says it is running no compute.\n\n")
	} else {
		fmt.Printf("\nnote     the ledger records nothing outstanding. NO HOST WAS ASKED what it\n")
		fmt.Printf("         is actually running, and compute whose lease has already gone is not\n")
		fmt.Printf("         visible to the ledger — so this does not establish that the machines\n")
		fmt.Printf("         are idle. `billet leases` and the node's own inventory are what\n")
		fmt.Printf("         confirm that.\n\n")
	}

	// THE COMPUTE PROOF IS RE-READ LAST, IMMEDIATELY BEFORE THE FIRST STOP.
	//
	// A launch dispatched after the wait returned moves the host's fence and
	// discards its run, and a registration beginning does the same — so re-asking
	// turns "it proved clear a moment ago" into "it is still proved clear now".
	// It sits BELOW the report deliberately: printing takes time, and every line
	// between the read and the stop is window.
	//
	// WHAT IT DOES NOT CLOSE, stated rather than implied: this is a read in one
	// process and a stop in another's world, so a registration landing between
	// them is still possible. Narrowing is not closing. Shutting that completely
	// needs the control plane to refuse registrations for the duration of the
	// stop and hold that refusal until its node wire is closed, which is a
	// protocol feature rather than a tighter loop here.
	if req.WantServer && proved {
		if err := stillClear(ctx, cfg); err != nil {
			return err
		}
	}

	// WHAT GOT DONE IS REPORTED WHEREVER THIS STOPS. A `down` is not atomic —
	// each unit is a separate systemd job — so a failure part-way through leaves
	// a host that is neither up nor down, and the operator's next decision
	// depends entirely on WHICH half happened. Returning the bare error tells
	// them which command failed and nothing about the machine they are holding.
	var (
		stoppedUnits  []string
		disabledUnits []string
	)

	for _, unit := range downOrder(c, req) {
		// OBSERVED, NOT PREDICTED. A stop is a systemd TRANSACTION: `Conflicts=`,
		// `PartOf=` and `BindsTo=` all reach other units, and the closure is
		// systemd's to compute through units billet has never heard of. `up`
		// settled this the same way after four rounds of trying to model it —
		// sample the other unit before and after, and refuse if it moved.
		//
		// It matters most on a SINGLE-ROLE host, which is the case that looks
		// safest: a node-only `down` that reaches the server through a dependency
		// takes the control plane down for the whole fleet, and the report would
		// otherwise name only the node.
		others, err := snapshotOthers(ctx, c, unit)
		if err != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits, err)
		}

		stopped, stopErr := c.StopAndProve(ctx, unit)

		// OBSERVED WHATEVER THE COMMAND ANSWERED. A manager can move a
		// transaction and THEN fail — an interrupted stop, one that stopped
		// answering after doing the work — so returning on the error without
		// looking is how `down` takes a control plane down and reports that
		// nothing was stopped.
		//
		// ON A LIVE CONTEXT, for the reason `up` gives at the same point: the
		// likeliest cause of a failure here is the operator's own interrupt, and
		// that is exactly when the caller's context can no longer answer.
		observeCtx, endObserve := liveFor(ctx)
		disturbed, collErr := othersDisturbed(observeCtx, c, others, unit)

		endObserve()

		if stopErr == nil {
			stoppedUnits = append(stoppedUnits, unit)
		} else if len(disturbed) > 0 || stopped.Gone != lifeops.Unknown {
			// It failed, and something moved anyway — or the manager told billet
			// something about the service despite failing. Say the unit was
			// touched rather than leaving the report claiming it was not.
			stoppedUnits = append(stoppedUnits, unit+" (uncertain)")
		}

		stoppedUnits = append(stoppedUnits, disturbed...)

		if stopErr != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits, stopErr)
		}

		if collErr != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits, collErr)
		}

		// THE VERDICT, NOT THE ABSENCE OF AN ERROR. A backend that answered "not
		// proved gone" with a nil error would otherwise be recorded as stopped,
		// after which this goes on to stop the SERVER — which tears down every
		// lease its listener holds, including the running ones. The systemd
		// backend happens to return an error alongside every non-Yes, and relying
		// on that is relying on a convention every future backend has to remember
		// rather than on the answer itself.
		if stopped.Gone != lifeops.Yes {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits,
				fmt.Errorf("%s %s, so its process is not proved gone and this host is not down",
					unit, stopped.How))
		}

		// THE MANAGER'S OWN ACCOUNT, PRINTED VERBATIM. Rendering it here would
		// mean this line knowing which service manager it is talking about, and
		// the two do not describe an ending in the same words: systemd has a
		// state and a Result, while launchd has a domain the service is no
		// longer in.
		fmt.Printf("stop     %s %s\n", unit, stopped.How)
	}

	// THE SAME SHAPE FOR ENABLEMENT, because `systemctl disable` follows
	// `[Install] Also=` — measured, and recorded in the billet-lifecycle skill.
	// A node-only `down` whose node unit carries an `Also=` would otherwise
	// disable the server too, and nothing about the node reports that.
	enablementBefore, err := enablementOfBoth(ctx, c)
	if err != nil {
		return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits, err)
	}

	for _, unit := range downOrder(c, req) {
		disableErr := c.Disable(ctx, unit)

		// AGAIN, WHATEVER IT ANSWERED. An interrupted disable can remove some
		// links and still exit non-zero.
		observeCtx, endObserve := liveFor(ctx)
		enablementAfter, readErr := enablementOfBoth(observeCtx, c)

		endObserve()

		if readErr != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits,
				errors.Join(disableErr, readErr))
		}

		if enablementAfter[unit] != enablementBefore[unit] || disableErr == nil {
			disabledUnits = append(disabledUnits, unit)
		}

		collErr := disabledBeyondTheOneAsked(enablementBefore, enablementAfter, unit,
			downOrder(c, req))

		if disableErr != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits,
				errors.Join(disableErr, collErr))
		}

		if collErr != nil {
			return partialDown(ctx, c, cfg, req, stoppedUnits, disabledUnits, collErr)
		}

		enablementBefore = enablementAfter

		fmt.Printf("disable  %s will not start at boot\n", unit)
	}

	fmt.Printf("\nThis host is down. `billet local up` starts it again")

	if req.WantServer {
		fmt.Printf(" and clears the seal")
	}

	fmt.Printf(".\n")

	return nil
}

// othersDisturbed reports which other units moved when one was stopped, and why
// that is refused.
//
// A PLANNED STOP TAKEN EARLY IS STILL A REFUSAL, and that is the deliberate
// asymmetry with the disable check below, which does skip units this run planned
// to disable. The ORDER is the safety here: the node stops before the server so
// it can report completions and custody to a control plane that is still there.
// A transaction that collapses the two takes the server down first or with it,
// which is the thing the ordering exists to prevent — so "billet was going to
// stop it anyway" is not a reason to continue. Enablement has no such ordering,
// which is why disabling both by plan is fine.
func othersDisturbed(ctx context.Context, c converger, before map[string]string,
	stopped string,
) ([]string, error) {
	var moved []string

	for name, was := range before {
		now, err := c.Snapshot(ctx, name)
		if err != nil {
			return moved, fmt.Errorf("look at %s after stopping %s: %w", name, stopped, err)
		}

		if now != was {
			moved = append(moved, name)

			return moved, fmt.Errorf("stopping %s disturbed %s, which was %s and is now %s. "+
				"billet asked to stop only %s: something in its transaction reaches %s — a "+
				"Conflicts=, PartOf= or BindsTo= on it or on something it pulls in. If %s was "+
				"a control plane, the whole fleet lost it. The node stops before the server "+
				"so it can report completions to one that is still running, and a transaction "+
				"that collapses the two defeats that ordering",
				stopped, name, was, now, stopped, name, name)
		}
	}

	return moved, nil
}

// disabledBeyondTheOneAsked refuses when disabling one unit disabled another
// this run was not going to disable anyway.
func disabledBeyondTheOneAsked(before, after map[string]lifeops.Enablement, asked string,
	planned []string,
) error {
	for name, was := range before {
		// A unit this run disables itself is not collateral: it is the plan. That
		// covers `asked` too, which is always one of them.
		if slices.Contains(planned, name) {
			continue
		}

		if now := after[name]; now != was {
			return fmt.Errorf("disabling %s also made %s %q, where it was %q. billet did not "+
				"ask for that — an `[Install] Also=` reaches units nothing here checked — and "+
				"%s will not start at the next boot. Restore it with `systemctl enable %s`",
				asked, name, now, was, name, name)
		}
	}

	return nil
}

// partialDown says where a failure left this host, because "down" is several
// systemd jobs rather than one.
//
// THE DANGEROUS HALF IS STOPPED-BUT-ENABLED: the service is not running now and
// WILL come back at the next boot, which is the state most likely to surprise
// somebody who has just been told their host is down. It is called out by name
// rather than left to be inferred from two lists.
func partialDown(ctx context.Context, c converger, cfg *config.Config, req lifeops.UpRequest,
	stopped, disabled []string, cause error,
) error {
	fmt.Printf("\n")

	if len(stopped) == 0 {
		fmt.Printf("state    nothing was stopped; this host is as it was\n")
	} else {
		fmt.Printf("state    stopped: %s\n", strings.Join(stopped, ", "))
	}

	if len(disabled) > 0 {
		fmt.Printf("state    disabled: %s\n", strings.Join(disabled, ", "))
	}

	// READ, NOT INFERRED FROM WHICH COMMANDS SUCCEEDED. Deriving this from the
	// bookkeeping gets it wrong in both directions: a unit disabled as collateral
	// through `[Install] Also=` never entered `disabled`, and a unit that was
	// already disabled before this run never will either — so the most important
	// line in this report, the one saying a service comes back at the next boot,
	// would be printed about units that do not.
	//
	// On a live context for the same reason as everything else here: the
	// commonest reason to be writing this report is an interrupt.
	reportCtx, cancel := liveFor(ctx)
	defer cancel()

	if now, err := enablementOfBoth(reportCtx, c); err != nil {
		fmt.Printf("state    enablement could NOT be read (%v); `billet local status` says\n", err)
		fmt.Printf("         which units start at boot\n")
	} else {
		for _, unit := range downOrder(c, req) {
			if now[unit].Enabled == lifeops.Yes {
				fmt.Printf("state    %s is STILL ENABLED, so a reboot starts it\n", unit)
			}
		}
	}

	// READ, NOT ASSUMED. The previous version said "admission is still sealed",
	// which this command has no way to know by then: a resume elsewhere can have
	// cleared it while these stops were running, and reporting a seal that is not
	// there is worse than reporting nothing — it is the one fact an operator
	// would rely on before walking away from a half-stopped host.
	//
	// ON A CONTEXT THAT OUTLIVES THE CANCELLATION, because the commonest reason
	// to be here is an interrupt, and a report that cannot be gathered because
	// the caller pressed Ctrl-C is a report nobody gets.
	if req.WantServer {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		switch now, err := readAdmission(reportCtx, cfg); {
		case err != nil:
			fmt.Printf("state    admission could NOT be read (%v); `billet status` is what\n", err)
			fmt.Printf("         says whether this deployment is taking work\n")
		case now.Mode == state.AdmissionOpen:
			fmt.Printf("state    admission is OPEN — this deployment is taking work again,\n")
			fmt.Printf("         onto a host that is part way down. `billet drain` seals it\n")
		default:
			fmt.Printf("state    admission is %s; `billet local up` clears a shutdown's seal,\n",
				now.Mode)
			fmt.Printf("         and `billet status` says what the deployment holds\n")
		}
	}

	return cause
}

// readAdmission is the one-shot read the reporting paths use.
func readAdmission(ctx context.Context, cfg *config.Config) (state.Admission, error) {
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return state.Admission{}, err
	}

	defer db.Close()

	return db.Admission(ctx)
}

// stillOurs refuses to stop anything if admission moved while this was getting
// ready to.
func stillOurs(ctx context.Context, cfg *config.Config, generation int64) error {
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	now, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	if now.Generation != generation {
		return fmt.Errorf("admission moved between this host draining and stopping (drained "+
			"at generation %d, the ledger is now at %d), so what was proved idle may be "+
			"running a job again; nothing was stopped. `billet status` says who holds it now",
			generation, now.Generation)
	}

	return nil
}

// stillClear refuses to stop anything if the fleet stopped being proved idle.
//
// THE GAP BETWEEN PROVING AND ACTING IS WHERE A LAUNCH WOULD LAND, and the
// answer is the same one stillOurs gives about admission: re-read, do not
// assume. A launch dispatched in that window moves the host's dispatch fence and
// discards its run, so this reads false and nothing is stopped.
func stillClear(ctx context.Context, cfg *config.Config) error {
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	clearance, err := allocator.ComputeClear(ctx)
	if err != nil {
		return fmt.Errorf("re-read what the fleet is running: %w", err)
	}

	if clearance.Clear() {
		return nil
	}

	blocking := clearance.Blocking()

	names := make([]string, 0, len(blocking))
	for _, n := range blocking {
		names = append(names, fmt.Sprintf("%s (%s)", n.Node, n.State))
	}

	return fmt.Errorf("the fleet stopped being proved idle between this host draining and "+
		"stopping: %s. Nothing was stopped; re-run to wait again, or pass "+
		"--without-compute-proof if you mean to stop without that proof",
		strings.Join(names, ", "))
}

// downOrder is the node before the server, and only the roles this host runs.
//
// The names come from the BACKEND rather than from deploy's constants, so this
// ordering — which is the safety content of the whole command — is one piece of
// code serving whichever service manager the host runs.
func downOrder(c converger, req lifeops.UpRequest) []string {
	server, node := c.Services()

	var units []string

	if req.WantNode {
		units = append(units, node)
	}

	if req.WantServer {
		units = append(units, server)
	}

	return units
}

// byWhom renders an attribution for a sentence that reads correctly without one.
func byWhom(a state.Admission) string {
	if a.Actor == "" {
		return ""
	}

	return " (" + a.Actor + ")"
}

func printDownPlan(c converger, req lifeops.UpRequest, o downOptions) {
	fmt.Printf("plan     what `billet local down` would do on this host:\n")

	if req.WantServer {
		fmt.Printf("         1. seal admission (local-down), so nothing new is taken\n")
		fmt.Printf("         2. wait for work already running to finish")

		if o.timeout > 0 {
			fmt.Printf(", giving up after %s", o.timeout)
		}

		fmt.Println()

		if o.withoutProof {
			fmt.Printf("         3. NOT ask any host what it is running " +
				"(--without-compute-proof)\n")
		} else {
			fmt.Printf("         3. ask every host what it is actually running, and wait until\n")
			fmt.Printf("            each says it is running nothing\n")
		}
	} else {
		fmt.Printf("         1. nothing to seal: this host runs no control plane, and nothing\n")
		fmt.Printf("            here can ask it what it is running either\n")
	}

	for _, unit := range downOrder(c, req) {
		fmt.Printf("         .  stop %s\n", unit)
	}

	for _, unit := range downOrder(c, req) {
		fmt.Printf("         .  disable %s\n", unit)
	}
}
