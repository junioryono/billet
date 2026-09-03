package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// drainPollInterval is how often `--wait` asks the ledger.
//
// The barrier is a read on the query-only pool, so the cost of asking is small;
// the cost of asking too often is an operator's terminal filling with a number
// that has not changed. Five seconds is short enough that a job finishing is
// noticed promptly and long enough that a six-hour drain is not thousands of
// queries.
const drainPollInterval = 5 * time.Second

// errStillDraining is the answer "I gave up waiting", which is not the same as
// "billet failed" and must not exit the same way. A monitor running
// `billet drain --wait --timeout 30m` acts on the two differently, and the seal
// is still in place either way.
var errStillDraining = &exitError{
	code: 2,
	msg: "the deployment is still draining; it remains sealed, so nothing new was ADMITTED " +
		"while this waited — which is not the same as nothing new starting, because escrow " +
		"taken before the seal can still become a running job",
}

// errWaitInterrupted is Ctrl-C or a SIGTERM arriving while waiting.
//
// IT IS NOT SUCCESS. The operator stopped watching; the deployment is no more
// drained than it was a moment earlier, and exiting 0 would tell a script that
// it is safe to proceed. It shares the still-draining status because it is the
// same answer: not drained, not broken.
var errWaitInterrupted = &exitError{
	code: 2,
	msg:  "stopped waiting before the deployment finished draining; it remains sealed",
}

// actor names whoever ran the command, for the attribution the seal carries.
//
// SUDO_USER FIRST, because these commands are run through sudo on a packaged
// host and a seal attributed to `root` tells the next operator nothing — the
// question they have when they find a quiet fleet is who to ask, and every
// sudo-using human is root by the time billet sees it.
//
// It is a LABEL, not an identity: nothing authenticates it, and the ledger
// treats it as text. It exists so a person can be found, not so a permission can
// be granted.
func actor() string {
	name := os.Getenv("SUDO_USER")

	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}

	if name == "" {
		name = "unknown"
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		return name
	}

	return name + "@" + host
}

// openLedgerForAdmission opens the control plane's ledger the way every operator
// command does: through OpenAdmin, so it works WHILE a control plane is running.
// Taking the directory lock would make `billet drain` fail against exactly the
// deployment it exists to drain.
func openLedgerForAdmission(ctx context.Context, cfgPath string) (*state.DB, *config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}

	if cfg.Server == nil {
		return nil, nil, errors.New("admission is a property of the control plane, and this " +
			"config has no server section")
	}

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("server state: %w", err)
	}

	return db, cfg, nil
}

// cmdDrain stops the deployment admitting new work.
//
// IT DOES NOT STOP ANYTHING. A drain is a seal plus patience: the ledger refuses
// new escrow, listeners hand back what they are not using and decline offers,
// and every job already running finishes. Killing compute is `billet local down`
// and the node's own shutdown, and conflating the two is how a maintenance
// window fails somebody's build.
func cmdDrain(ctx context.Context, args []string) error {
	fs := newFlagSet("billet drain")
	cfgPath := addConfigFlag(fs)
	reason := fs.String("reason", "",
		"why this deployment is not taking work, for whoever finds it sealed")
	wait := fs.Bool("wait", false,
		"keep waiting until everything already running has finished")
	timeout := fs.Duration("timeout", 0,
		"give up waiting after this long (default: wait for as long as the jobs take)")
	withoutProof := fs.Bool("without-compute-proof", false,
		"stop once the LEDGER is quiet, without asking each host what it is actually "+
			"running (faster, and it cannot see compute whose lease has already gone)")

	if err := parse(fs, args); err != nil {
		return err
	}

	// REFUSED RATHER THAN REINTERPRETED. A negative duration would fall through
	// the `> 0` test below and wait forever, which is the opposite of what
	// somebody typing a timeout wants; a timeout without --wait would be accepted
	// and silently ignored, leaving them believing the command was bounded.
	if *timeout < 0 {
		return fmt.Errorf("--timeout %s is negative; use a positive duration, or omit it to "+
			"wait for as long as the jobs take", *timeout)
	}
	if *timeout > 0 && !*wait {
		return errors.New("--timeout only applies to --wait; without it this command seals and " +
			"returns immediately")
	}

	// REFUSED RATHER THAN IGNORED, for the same reason as --timeout: a flag
	// accepted and silently discarded leaves somebody believing they asked for
	// something. Without --wait nothing is proved either way, so waiving a proof
	// that was never going to be taken says nothing true.
	if *withoutProof && !*wait {
		return errors.New("--without-compute-proof only applies to --wait; without it this " +
			"command seals and returns immediately, and proves nothing about any host")
	}

	db, cfg, err := openLedgerForAdmission(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	sealed, err := takeTheSeal(ctx, db, current, *reason)
	if err != nil {
		return err
	}

	if !*wait {
		fmt.Printf("\nJobs already running are unaffected and will finish. Watch what is left\n")
		fmt.Printf("with `billet status`, or re-run this with --wait. Reopen with `billet resume`.\n")

		return nil
	}

	return waitForQuiet(ctx, db, cfg, sealed.Generation, waitOptions{
		timeout:      *timeout,
		withoutProof: *withoutProof,
	})
}

// waitOptions is what a caller of waitForQuiet decided.
type waitOptions struct {
	timeout time.Duration
	// withoutProof stops after the LEDGER barrier, skipping the question put to
	// each host. It exists because the proof cannot always be obtained — a host
	// that is off, or too old to answer, never will — and an operator has to be
	// able to proceed knowing that is what they did.
	withoutProof bool
}

// takeTheSeal records the operator's decision and returns the admission the
// ledger actually holds afterwards.
//
// EVERY PATH GOES THROUGH THE TRANSACTION, including "it was already sealed".
// Deciding that from the read above would be deciding on a snapshot: another
// operator can resume in between, and this command would print "already sealed"
// and exit 0 against a deployment that is now OPEN — after which somebody starts
// maintenance while work is being admitted. state.Seal makes the same-provenance
// no-op inside the write transaction, so the answer here is the ledger's.
func takeTheSeal(ctx context.Context, db *state.DB, current state.Admission,
	reason string,
) (state.Admission, error) {
	// A SHUTDOWN'S SEAL IS ESCALATED, and that is a real change rather than a
	// formality: a local-down seal is cleared by the next successful
	// `billet local up`, so leaving it would reopen admission the moment the
	// services came back — which is not what somebody who ran `billet drain`
	// asked for.
	escalating := current.Mode == state.AdmissionSealed &&
		current.Provenance == state.ProvenanceLocalDown

	sealed, err := db.Seal(ctx, state.SealRequest{
		Expect:     current.Generation,
		Provenance: state.ProvenanceOperator,
		Reason:     reason,
		Actor:      actor(),
		// `billet drain` is "make sure this is sealed", so running it twice must
		// leave the first operator's attribution and fence alone.
		KeepExisting: true,
	})
	if err != nil {
		return state.Admission{}, err
	}

	// UNCHANGED IS HOW A NO-OP IS RECOGNISED, and it is read from what the
	// transaction returned rather than from the snapshot: every write bumps the
	// generation, so a generation that did not move is the ledger saying it
	// already held this.
	if sealed.Generation == current.Generation && current.Mode == state.AdmissionSealed {
		fmt.Printf("Already sealed")

		switch {
		case sealed.Actor != "" && sealed.Reason != "":
			fmt.Printf(" by %s: %s", sealed.Actor, sealed.Reason)
		case sealed.Actor != "":
			fmt.Printf(" by %s", sealed.Actor)
		}

		fmt.Printf(".\n")

		// SAID OUT LOUD, because silently discarding it would leave the operator
		// believing the ledger records their reason.
		if reason != "" && reason != sealed.Reason {
			fmt.Printf("\nThe --reason you gave was NOT recorded; the existing seal is unchanged.\n")
			fmt.Printf("Run `billet resume` first if you mean to replace it.\n")
		}

		return sealed, nil
	}

	switch {
	case escalating:
		fmt.Printf("A shutdown was already holding this deployment sealed. It is now held\n")
		fmt.Printf("deliberately instead, so `billet local up` will NOT reopen it.\n")
	case current.Mode == state.AdmissionUnknown:
		// Fail-closed, and named: billet could not read the previous value, so it
		// is not reporting a transition it cannot vouch for.
		fmt.Printf("Admission read as %s before this, which billet does not recognise.\n",
			current.Mode)
		fmt.Printf("It is now definitively sealed.\n")
	default:
		fmt.Printf("Sealed. This deployment is no longer taking new work.\n")
	}

	fmt.Printf("\nsealed by %s", sealed.Actor)

	if sealed.Reason != "" {
		fmt.Printf(": %s", sealed.Reason)
	}

	fmt.Printf("\ngeneration %d, and it survives a control-plane restart\n", sealed.Generation)

	return sealed, nil
}

// cmdResume lets the deployment admit work again.
func cmdResume(ctx context.Context, args []string) error {
	fs := newFlagSet("billet resume")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	db, _, err := openLedgerForAdmission(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	// UNKNOWN IS HANDLED BEFORE PROVENANCE, because the remedy differs and the
	// provenance of an unreadable row is not to be trusted. state.Resume refuses
	// to open a mode it does not recognise, so naming `billet local up` here —
	// which a stale local-down provenance would otherwise produce — would send the
	// operator to a command that cannot work either.
	if current.Mode == state.AdmissionUnknown {
		return fmt.Errorf("admission reads %q, which billet does not recognise, so it will "+
			"not open it. The admission row has to be repaired before this deployment can "+
			"take work again", current.Mode)
	}

	// REFUSED RATHER THAN CLEARED, with the remedy named. A shutdown's seal is
	// held for the duration of that shutdown, and reopening admission underneath
	// it would admit work onto services that are stopping. The refusal itself is
	// the state layer's, inside the write transaction — this only supplies the
	// command that does what the operator wanted, which the ledger has no way to
	// know.
	resumed, err := db.Resume(ctx, state.ResumeRequest{
		Expect: current.Generation,
		Clears: state.ProvenanceOperator,
		Actor:  actor(),
	})
	if err != nil {
		if errors.Is(err, state.ErrAdmissionProvenance) {
			return fmt.Errorf("%w. `billet local up` is what completes that shutdown and "+
				"reopens admission", err)
		}

		return err
	}

	// UNCHANGED MEANS IT WAS ALREADY OPEN, read from the transaction rather than
	// from the snapshot above: a drain committing in between would otherwise have
	// this command report "already taking work" against a deployment that is now
	// sealed.
	if resumed.Generation == current.Generation && resumed.Mode == state.AdmissionOpen &&
		current.Mode == state.AdmissionOpen {
		fmt.Printf("Already taking work; nothing to do.\n")

		return nil
	}

	fmt.Printf("Taking work again, at generation %d.\n", resumed.Generation)
	fmt.Printf("\nListeners pick this up on their next poll rather than instantly, so the first\n")
	fmt.Printf("job may take a poll to arrive.\n")

	return nil
}

// waitForQuiet blocks until the deployment is holding nothing.
//
// THE BARRIER IS THE AUTHORITY, NOT THE SEAL. Sealing stops new work being
// admitted; it says nothing about what was already running, and it does not take
// effect at the instant it is run — a listener learns about it on its next poll,
// and an offer accepted just before that is real work with real compute behind
// it. So the question "is it safe to stop this now" is answered by asking what
// the deployment is still holding, never by the fact that a seal was taken.
func waitForQuiet(ctx context.Context, db *state.DB, cfg *config.Config, generation int64,
	opts waitOptions,
) error {
	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	if opts.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	fmt.Printf("\nWaiting for what is already running to finish. Ctrl-C stops waiting; it does\n")
	fmt.Printf("not stop the drain, and nothing running is harmed either way.\n\n")

	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	last := ""

	for {
		q, err := allocator.Quiescence(ctx)
		if err != nil {
			// A CANCELLED WAIT IS NOT A LEDGER FAILURE, and the test is on the
			// ERROR rather than on ctx.Err(). Asking the context instead answers a
			// different question — "has the deadline passed by now" — so a genuine
			// SQLite or schema failure that raced the deadline would be reported as
			// "still draining" and exit 2, telling a monitor the deployment is fine
			// while the ledger is broken.
			//
			// SAID PLAINLY: no test distinguishes this from the ctx.Err() version,
			// and the mutation survives. The two diverge only when a real ledger
			// error lands on an iteration where the deadline has ALREADY passed, and
			// that is not reachable on demand — a cancelled context makes the read
			// itself return context.Canceled, and a deadline expiring during the
			// wait returns from the select below without asking the barrier at all.
			// It is kept because it is the correct question to ask, not because
			// something proves it.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stillDraining(err)
			}

			return err
		}

		// SOMEBODY ELSE MOVED ADMISSION. The generation is what says so, and
		// watching `Sealed` alone is not enough: between two samples another
		// operator can resume AND seal again, and a waiter comparing booleans sees
		// "sealed" both times while never learning that admission was open in
		// between — during which this deployment could have taken work. Comparing
		// against the generation this drain established makes that observable.
		if !q.Sealed || q.Generation != generation {
			return fmt.Errorf("admission moved while this was waiting (this drain sealed at "+
				"generation %d, the ledger is now at %d), so what this was waiting for is no "+
				"longer what it sealed; `billet status` says who holds it now",
				generation, q.Generation)
		}

		if q.Quiet() {
			// THE LEDGER BARRIER IS THE FIRST STAGE, NOT THE ANSWER. It cannot see
			// compute whose lease has already gone — an in-memory destroy
			// obligation, or a launch whose lease was reclaimed — so the second stage
			// asks each host what its provider is actually running. The order is
			// load-bearing: while a lease is open a launch may still be dispatched,
			// which moves a host's fence and discards whatever it had proved.
			fmt.Printf("Drained: this deployment is sealed and the ledger records no\n")
			fmt.Printf("outstanding lease.\n\n")

			if opts.withoutProof {
				fmt.Printf("No host was asked what it is running (--without-compute-proof).\n")
				fmt.Printf("Compute whose lease has already gone is not visible to the ledger, so\n")
				fmt.Printf("this does NOT establish that the machines are idle.\n")

				return nil
			}

			return proveComputeClear(ctx, allocator, generation, ticker)
		}

		if summary := outstandingSummary(q); summary != last {
			fmt.Printf("%s\n", summary)
			last = summary
		}

		select {
		case <-ctx.Done():
			return stillDraining(ctx.Err())
		case <-ticker.C:
		}
	}
}

// barrierSilentAfter is how long the fleet may say nothing before this hints at
// why.
//
// A CONTROL PLANE ISSUES THE BARRIER, and this command is not it: the request is
// a durable row and the running server is what puts the question to each host.
// So a deployment whose server is stopped waits here in perfect silence forever,
// with nothing in the output naming the one thing that would fix it. Two poll
// intervals is long enough that a healthy fleet has answered.
const barrierSilentAfter = 4 * drainPollInterval

// proveComputeClear waits for every expected host to say it is running nothing.
//
// THE SECOND STAGE, AND THE REASON THE FIRST IS NOT ENOUGH. A listener that
// loses a running lease keeps an in-memory obligation to destroy what it
// launched, and a launch whose lease was reclaimed can create compute it then
// fails to destroy. Neither is in the ledger and neither survives a restart of
// the process holding it — but both leave an instance carrying the name billet
// gave it, so the host's own provider still lists it.
//
// WHAT MAKES AN ANSWER EVIDENCE rather than telemetry is not this function: the
// command travels through the node's serial queue behind every launch already
// dispatched, and its answer is recorded against a fence captured before it was
// sent. This only asks, and reports what came back.
func proveComputeClear(
	ctx context.Context, allocator *alloc.Allocator, generation int64, ticker *time.Ticker,
) error {
	barrier, err := allocator.RequestComputeBarrier(ctx, generation, actor())
	if err != nil {
		return fmt.Errorf("ask the fleet what it is running: %w", err)
	}

	fmt.Printf("Now asking each host what it is actually running. This is what the ledger\n")
	fmt.Printf("cannot see: compute whose lease has already gone.\n\n")

	var (
		last    string
		started = time.Now()
		heard   bool
	)

	for {
		clearance, err := allocator.ComputeClear(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stillDraining(err)
			}

			return err
		}

		// SOMEBODY ELSE REPLACED THE BARRIER, which can only mean admission moved
		// and a later waiter re-requested under a generation this drain never
		// sealed. Reported rather than followed: what this was waiting for is no
		// longer what it asked.
		if !clearance.Requested || clearance.BarrierID != barrier.ID {
			return fmt.Errorf("the compute barrier this drain asked for was replaced while it "+
				"was waiting (it asked under admission generation %d); `billet status` says who "+
				"holds this deployment now", generation)
		}

		// AND ADMISSION IS RE-READ ON EVERY PASS, not only in the ledger stage
		// above. The plane drops a barrier whose generation has moved, but that is
		// asynchronous cleanup rather than a fence: between a resume, some work,
		// a reseal, and the plane's next pass, every host's run is still stored
		// and still fenced, and this would exit 0 against a deployment that was
		// open in between. ComputeClear reads admission in the same snapshot, so
		// this is asking the answer rather than a second source.
		if clearance.Stale() {
			return fmt.Errorf("admission moved while this was proving the fleet idle (this "+
				"drain sealed at generation %d, the ledger is now at %d), so what each host "+
				"said no longer describes a deployment that has been closed the whole time; "+
				"`billet status` says who holds it now",
				generation, clearance.AdmissionGeneration)
		}

		if clearance.Clear() {
			reportProved(clearance)

			return nil
		}

		heard = heard || anyoneAnswered(clearance)

		if summary := clearanceSummary(clearance); summary != last {
			fmt.Printf("%s\n", summary)
			last = summary
		}

		if !heard && time.Since(started) > barrierSilentAfter {
			fmt.Printf("\n  Nothing has answered yet. A running control plane is what puts this\n")
			fmt.Printf("  question to each host — check that `billet server` is up, or re-run\n")
			fmt.Printf("  with --without-compute-proof to stop at what the ledger knows.\n\n")

			heard = true // said once; the per-host lines carry it from here.
		}

		select {
		case <-ctx.Done():
			return stillDraining(ctx.Err())
		case <-ticker.C:
		}
	}
}

// anyoneAnswered reports whether any host has given a fenced answer yet.
//
// A RETAINED RUN IS AN ANSWER, WHATEVER THE HOST'S STATE HAS SINCE BECOME.
// EmptySince is set only for a run that is still fenced to this barrier and this
// registration, so a non-empty one is a host that answered — and a host can
// answer and then go away, which now reads as ClearanceUnreachable. Deciding
// from the state alone told an operator "nothing has answered yet, check that
// `billet server` is up" about a fleet that had answered, and sent them to look
// at a control plane that was fine.
func anyoneAnswered(c alloc.ComputeClearance) bool {
	for _, n := range c.Nodes {
		if n.EmptySince != "" {
			return true
		}

		switch n.State {
		case alloc.ClearanceProved, alloc.ClearanceSettling, alloc.ClearanceRunning:
			return true
		case alloc.ClearanceUnknown, alloc.ClearanceWaiting, alloc.ClearanceUnreachable,
			alloc.ClearanceBelowProtocol:
		}
	}

	return false
}

// reportProved says what was established, and separately what was not.
//
// A FORCED EXCLUSION NEVER PRINTS THE PROVEN SENTENCE. A host somebody removed
// from the expected set without proof is billet admitting it does not know what
// is on that machine, and a report that renders it the same as a host that
// answered is the laundering this whole mechanism exists to prevent.
func reportProved(c alloc.ComputeClearance) {
	fmt.Printf("\nEvery host billet expects an answer from says it is running no compute,\n")
	fmt.Printf("and has said so continuously since before this drain finished.\n")

	if len(c.Nodes) == 0 {
		fmt.Printf("\n  (this deployment has no host in its expected set, so that claim is\n")
		fmt.Printf("  about nothing — `billet status` lists the fleet)\n")
	}

	unproven := c.Unproven()
	if len(unproven) == 0 {
		return
	}

	fmt.Printf("\nBut %d host(s) were EXCLUDED from that set without proof, and nothing here\n",
		len(unproven))
	fmt.Printf("says whether they are running anything:\n")

	for _, name := range unproven {
		fmt.Printf("  %s\n", name)
	}

	fmt.Printf("\nA forced `billet nodes decommission` records that, and it stays recorded.\n")
}

// clearanceSummary is one block naming what is still holding this up.
//
// NAMED AND ORDERED BY WHAT AN OPERATOR CAN DO ABOUT IT. A host that says it is
// running something is the answer worth acting on; a host too old to be asked
// needs an upgrade or a decommission; one that cannot be reached needs somebody
// to go and look. "3 hosts pending" tells them none of that.
func clearanceSummary(c alloc.ComputeClearance) string {
	var b strings.Builder

	blocking := c.Blocking()

	fmt.Fprintf(&b, "waiting on %d host(s)", len(blocking))

	const named = 5

	for i, n := range blocking {
		if i == named {
			fmt.Fprintf(&b, "\n  ... and %d more", len(blocking)-named)

			break
		}

		fmt.Fprintf(&b, "\n  %-24s %s", n.Node, n.State)

		switch n.State {
		case alloc.ClearanceSettling:
			// NOT "clear at": nothing clears because a moment arrives. The proof is
			// an empty answer taken AT OR AFTER this, so a host that stops answering
			// sits here for good — which is exactly what the wording has to survive.
			fmt.Fprintf(&b, " (needs another empty answer at or after %s)", n.ClearAt)
		case alloc.ClearanceBelowProtocol:
			fmt.Fprintf(&b, " (wire %d; it has no inventory command)", n.WireVersion)
		case alloc.ClearanceUnknown, alloc.ClearanceProved, alloc.ClearanceRunning,
			alloc.ClearanceWaiting, alloc.ClearanceUnreachable:
		}
	}

	return b.String()
}

// stillDraining turns a cancelled wait into the answer it is.
//
// NEITHER OUTCOME IS SUCCESS. An earlier version returned nil for Ctrl-C on the
// reasoning that the operator asked to stop watching — true, and not what the
// exit status means. Exit 0 here says "drained", so a script that seals, waits,
// and proceeds on success would proceed while jobs were still running, and the
// process-wide signal handler cancels this context, so a SIGTERM in a pipeline
// reaches it without anybody pressing anything.
func stillDraining(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errStillDraining
	}

	fmt.Printf("\nStopped waiting. The deployment is still sealed; `billet resume` reopens it.\n")

	return errWaitInterrupted
}

// outstandingSummary is one line an operator can act on: how much is left, and
// what it is.
//
// NAMED RATHER THAN COUNTED, because a report somebody cannot recognise their
// own work in is not a report — "3 leases" tells an operator nothing about
// whether to keep waiting, and a run id tells them exactly whose build it is.
func outstandingSummary(q alloc.Quiescence) string {
	var b strings.Builder

	fmt.Fprintf(&b, "waiting on %d", len(q.Outstanding))

	// ESCROW IS REPORTED SEPARATELY, because it is a different wait. Running work
	// ends by finishing; escrow ends when a listener hands it back, which it does
	// on its next poll. An operator who sees only a count cannot tell a drain that
	// is progressing from one that is stuck.
	if escrowed := q.Escrowed(); escrowed > 0 {
		fmt.Fprintf(&b, " (%d of them capacity no job has started on yet)", escrowed)
	}

	const named = 5

	for i, o := range q.Outstanding {
		if i == named {
			fmt.Fprintf(&b, "\n  ... and %d more", len(q.Outstanding)-named)

			break
		}

		fmt.Fprintf(&b, "\n  %s %s", o.Tier, o.Phase)

		if o.RunID != "" {
			fmt.Fprintf(&b, " run %s", o.RunID)
		}

		if o.Node != "" {
			fmt.Fprintf(&b, " on %s", o.Node)
		}
	}

	return b.String()
}
