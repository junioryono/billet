package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// The half of `billet acceptance` that runs the deployment and takes it down.
//
// `run` SUPERVISES THE REAL BINARY AS CHILD PROCESSES rather than starting the
// server and node in this one. That is the whole point of an acceptance harness:
// what is under test is `billet server` and `billet node` as an operator invokes
// them — the real flags, the real signals, the real drain on SIGTERM — and an
// in-process stack proves something about a composition nobody deploys.
//
// AND IT ALWAYS TEARS DOWN. Every path out of `run` reaches the teardown,
// including a failed assertion, a cancelled context and a panic in between,
// because the failure mode this command exists to avoid is compute left billing
// after a run nobody watched.

const (
	// acceptanceSettle is how long a service must stay up before `run` believes it
	// started. It is not readiness — that is the server's own `--dry-run`-free
	// startup — it is the difference between "exec succeeded" and "the process is
	// still there a moment later", which is what catches a config the service
	// refuses.
	acceptanceSettle = 3 * time.Second
	// acceptancePoll is how often the ledger is asked whether the jobs have
	// finished. The read is on the query-only pool and the run is minutes long.
	acceptancePoll = 5 * time.Second
	// acceptanceStopGrace bounds how long `run` waits for a service to exit after
	// SIGTERM before it stops waiting and says so. IT DOES NOT KILL: the node's
	// SIGTERM is a drain with no deadline, and turning that into a kill here would
	// be the timer-authorised teardown billet refuses everywhere else. What this
	// bounds is the REPORT, not the process.
	acceptanceStopGrace = 5 * time.Minute
)

func cmdAcceptanceRun(ctx context.Context, args []string) error {
	fs := newFlagSet("billet acceptance run")
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	jobs := fs.Int("jobs", 1, "how many jobs must reach a terminal outcome before this stops waiting")
	wait := fs.Duration("wait", 30*time.Minute,
		"give up waiting for those jobs after this long; the teardown still runs")
	noTeardown := fs.Bool("no-teardown", false,
		"stop the services and write the evidence, but DESTROY NOTHING — this run's scale "+
			"sets and compute are left for `billet acceptance down`, which you then own")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *jobs < 1 {
		return fmt.Errorf("--jobs %d: an acceptance run waits for at least one job; waiting "+
			"for none proves nothing about whether anything can run here", *jobs)
	}

	// AND NOT MORE THAN THE HISTORY CAN SHOW. The wait counts terminal rows from a
	// bounded read, so a request above that bound can never be satisfied — the run
	// would wait out its whole `--wait` against a ledger that already held enough
	// jobs. Refused with the number, rather than becoming a timeout nobody can
	// explain.
	if *jobs > acceptanceJobLimit {
		return fmt.Errorf("--jobs %d: this run reads at most %d job(s) from the ledger, so a "+
			"larger number can never be satisfied and would wait out --wait against a "+
			"deployment that had already finished", *jobs, acceptanceJobLimit)
	}

	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}

	return runAcceptance(ctx, ws, acceptanceRunOptions{
		jobs:       *jobs,
		wait:       *wait,
		noTeardown: *noTeardown,
	})
}

type acceptanceRunOptions struct {
	jobs int
	wait time.Duration
	// noTeardown skips the DESTROY and nothing else. The services are stopped and
	// the evidence is written either way, because the evidence has to be read off
	// a ledger nothing is still writing to.
	noTeardown bool
}

func runAcceptance(ctx context.Context, ws acceptanceWorkspace, opts acceptanceRunOptions) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this billet binary, which is what the run supervises: %w", err)
	}

	dir := filepath.Dir(ws.ConfigPath)

	server, err := startAcceptanceService(ctx, self, "server", ws.ConfigPath, dir)
	if err != nil {
		// NOTHING TO TEAR DOWN. The server never came up, so it created no scale
		// set and dispatched no launch — and a teardown here would be scoped to a
		// deployment that has never existed anywhere but on this disk.
		return err
	}

	// FROM HERE EVERY PATH REACHES THE TEARDOWN, and that is what this function
	// is shaped around. The server creates a GitHub scale set on startup, so any
	// return after this point that skipped the teardown would leave scale sets —
	// and therefore runner registrations GitHub routes jobs to — behind. The
	// node-start failure below used to be exactly such a return.
	var waitErr error

	node, nodeErr := startAcceptanceService(ctx, self, "node", ws.ConfigPath, dir)
	if nodeErr != nil {
		waitErr = nodeErr
	} else {
		waitErr = waitForAcceptanceJobs(ctx, ws, opts)
	}

	// THE COMPUTE PROOF IS TAKEN WHILE THE SERVICES ARE STILL UP, and that is the
	// only moment it can be taken at all.
	//
	// The barrier is a durable request that the CONTROL PLANE observes: it queues
	// an inventory command behind everything already in each host's serial queue
	// and records the answer against a fence taken before the question. A stopped
	// control plane observes nothing, so a barrier requested after the services
	// stop waits in silence forever — which is why the teardown's own drain runs
	// `withoutProof`, and why, before this, an acceptance run's evidence always
	// said `requested: false`. The strongest instrument in the report was one the
	// procedure never used.
	//
	// ITS FAILURE IS NOT THE RUN'S. A fleet that will not go quiet is worth
	// reporting and is not a reason to skip the teardown, so the error is
	// collected and everything below happens either way.
	proved, proofErr := proveAcceptanceComputeClear(ctx, ws, opts.wait)
	if proofErr != nil {
		fmt.Fprintf(os.Stderr, "acceptance: the compute barrier did not clear: %v\n", proofErr)
	}

	// THE STOP AND THE TEARDOWN RUN WHATEVER HAPPENED ABOVE, and on a context
	// DETACHED from the caller's: the likeliest way into this path is the
	// operator's own Ctrl-C, and a teardown that inherited that cancellation
	// would return immediately having destroyed nothing — which is the failure
	// this whole command exists to prevent.
	//
	// AND IT IS NOT TIME-BOUNDED HERE. Bounding it looked prudent and was wrong:
	// the teardown's own drain waits up to acceptanceDrainWait for a job to
	// finish, so a shorter deadline on this context would cancel it — and the
	// same deadline would then cut short the decommission and the sweep, leaving
	// compute behind for the sake of a timer. Each step carries its own bound;
	// this context exists only to survive the caller's cancellation.
	teardownCtx := context.WithoutCancel(ctx)

	// EVIDENCE BEFORE THE TEARDOWN, because the teardown destroys the compute the
	// evidence is about — and after the services stop, so what it reads is not
	// moving underneath it.
	stopAcceptanceService(node)
	stopAcceptanceService(server)

	evidencePath := filepath.Join(dir, acceptanceEvidence)
	if err := writeAcceptanceEvidence(teardownCtx, ws, evidencePath); err != nil {
		fmt.Fprintf(os.Stderr, "acceptance: could not write the evidence: %v\n", err)
	}

	if opts.noTeardown {
		// SAID EXACTLY: the services are STOPPED. An earlier version of this said
		// the deployment "was left standing", which was false — both children are
		// signalled above, unconditionally, because the evidence has to be read
		// off a ledger nothing is writing to. What this flag skips is the DESTROY.
		fmt.Printf("\n--no-teardown: the services are stopped and the evidence is written, " +
			"but nothing was destroyed.\n")
		fmt.Printf("This run's scale sets and compute are still there. Destroy them with\n")
		fmt.Printf("  billet acceptance down --workspace %s\n", dir)

		return waitErr
	}

	downErr := tearDownAcceptance(teardownCtx, ws, proved)

	// THE RUN'S OWN FAILURE WINS. A teardown that also failed is reported beside
	// it rather than replacing it, because "the job did not run" and "the cleanup
	// did not finish" are two things an operator has to know and only one of them
	// is what they came for.
	return errors.Join(waitErr, proofErr, downErr)
}

// proveAcceptanceComputeClear seals the run and waits for every host to say it is
// running nothing, through exactly the code `billet drain --wait` runs.
//
// SEALING HERE IS NOT PREMATURE. The jobs this run was waiting for have already
// finished, and the teardown is about to seal anyway — doing it now is what lets
// the still-running control plane observe the barrier and put the question to
// each host. `takeTheSeal` is a no-op against a deployment that is already sealed.
func proveAcceptanceComputeClear(
	ctx context.Context, ws acceptanceWorkspace, wait time.Duration,
) (bool, error) {
	db, cfg, err := openLedgerForAdmission(ctx, ws.ConfigPath)
	if err != nil {
		return false, fmt.Errorf("open this run's ledger: %w", err)
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return false, fmt.Errorf("read admission: %w", err)
	}

	sealed, err := takeTheSeal(ctx, db, current, "billet acceptance run")
	if err != nil {
		return false, fmt.Errorf("seal this run: %w", err)
	}

	if err := waitForQuiet(ctx, db, cfg, sealed.Generation, waitOptions{timeout: wait}); err != nil {
		return false, err
	}

	return true, nil
}

// acceptanceService is a supervised child.
//
// THE Wait IS STARTED IMMEDIATELY, AND ITS RESULT IS THE ONLY WAY THIS ASKS
// WHETHER THE CHILD IS ALIVE. That is not a style choice; the two obvious
// alternatives were both MEASURED and both wrong:
//
//	Signal(nil)          "os: unsupported signal type" — for a LIVE child too.
//	                     Go rejects a nil signal outright, so a check built on it
//	                     answers "exited" for everything, and every service would
//	                     have been reported dead three seconds after starting.
//	Signal(Signal(0))    nil for a live child AND for a dead one that has not been
//	                     reaped, because kill(pid, 0) succeeds against a zombie. It
//	                     only begins answering once Wait has run. With the goroutine
//	                     below in place it would usually be right — because that Wait
//	                     reaps within milliseconds — which is the trap: it would be
//	                     correct by RACING the reaper rather than by asking it.
//
// A goroutine in Wait reaps the child and reports the truth, in that order.
type acceptanceService struct {
	name string
	cmd  *exec.Cmd

	// done closes when the child has been reaped, and result holds what it
	// exited with. Buffered, so the goroutine never blocks on a caller that
	// stopped listening.
	done   chan struct{}
	result error
}

// startAcceptanceService execs `billet <role> --config <derived>` and proves it
// is still there a moment later.
func startAcceptanceService(
	ctx context.Context, self, role, cfgPath, dir string,
) (*acceptanceService, error) {
	logPath := filepath.Join(dir, role+".log")

	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}

	//nolint:noctx // NOT CommandContext: a cancelled context must SIGTERM these
	// rather than kill them, and this is the one place in billet where that
	// distinction decides whether somebody's job dies. The stop is explicit.
	cmd := exec.Command(self, role, "--config", cfgPath)
	cmd.Stdout = log
	cmd.Stderr = log

	if err := cmd.Start(); err != nil {
		_ = log.Close()

		return nil, fmt.Errorf("start billet %s: %w", role, err)
	}

	// THE PARENT'S COPY IS CLOSED AFTER Start, because the child has its own now.
	// Holding it open leaks a descriptor for the whole run and, on the failure
	// path below, for nothing at all.
	if err := log.Close(); err != nil {
		return nil, fmt.Errorf("close this process's copy of %s: %w", logPath, err)
	}

	svc := &acceptanceService{name: role, cmd: cmd, done: make(chan struct{})}

	go func() {
		svc.result = cmd.Wait()
		close(svc.done)
	}()

	// A PID IS NOT A RUNNING PROCESS, which this repository has measured on three
	// other backends. `exec.Start` returns as soon as the fork succeeds, so a
	// service that refuses its config exits milliseconds later and every assertion
	// after this would be about a deployment that is not there. The settle window
	// is what turns that into a failure naming the log.
	select {
	case <-svc.done:
		return nil, fmt.Errorf("billet %s exited within %s of starting (%w); see %s",
			role, acceptanceSettle, svc.result, logPath)
	case <-time.After(acceptanceSettle):
	case <-ctx.Done():
		// THE CHILD IS STOPPED, not left behind. A cancelled start still forked a
		// process, and returning without it is how a control plane ends up polling
		// GitHub with nothing tracking it.
		stopAcceptanceService(svc)

		return nil, fmt.Errorf("cancelled while starting billet %s: %w", role, ctx.Err())
	}

	fmt.Printf("started billet %s (pid %d), logging to %s\n", role, cmd.Process.Pid, logPath)

	return svc, nil
}

// stopAcceptanceService sends SIGTERM and waits, bounded, without ever escalating.
//
// NO SIGKILL, EVER. `billet node`'s SIGTERM is a DRAIN: it stops taking work and
// waits for the jobs already running, which may be long. A harness that killed it
// would fail somebody's build to save itself a wait — the exact thing billet's own
// shutdown ordering refuses — so what is bounded here is how long this command
// WATCHES, and an overrun is reported rather than escalated.
func stopAcceptanceService(s *acceptanceService) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}

	// ALREADY GONE IS NOT A FAILURE TO STOP IT. The Wait goroutine has already
	// reaped it, so there is nothing to signal and nothing to wait for.
	select {
	case <-s.done:
		return
	default:
	}

	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		// It exited between the check above and this signal. The goroutine has it.
		<-s.done

		return
	}

	select {
	case <-s.done:
		fmt.Printf("billet %s stopped\n", s.name)
	case <-time.After(acceptanceStopGrace):
		fmt.Fprintf(os.Stderr,
			"acceptance: billet %s is still draining after %s. It has NOT been killed — a "+
				"drain has no deadline and stopping it would fail whatever job it is waiting "+
				"for. This command has stopped watching; the process is still there.\n",
			s.name, acceptanceStopGrace)
	}
}

// waitForAcceptanceJobs waits until the ledger records enough terminal jobs.
//
// THE LEDGER RATHER THAN GITHUB'S API, because what an acceptance run is proving
// is what BILLET observed: the job history row carries GitHub's own reported
// result beside billet's own conclusion, and the two disagreeing is exactly the
// finding worth having. Asking GitHub instead would answer a question about
// GitHub.
func waitForAcceptanceJobs(ctx context.Context, ws acceptanceWorkspace, opts acceptanceRunOptions) error {
	deadline := time.Now().Add(opts.wait)

	// THE JOBS THIS RUN CAUSED, NOT THE JOBS IN THE LEDGER.
	//
	// A workspace is resumable, so the ledger may already hold finished jobs from
	// an earlier `run` against it — and counting those meant a second `run --jobs
	// 1` returned success on its FIRST POLL, having proved nothing and with no
	// workflow ever reaching the deployment. An acceptance run that passes without
	// a job is worse than one that fails.
	//
	// A BASELINE OF LEASE IDS rather than a timestamp: a lease is what a job is
	// recorded against, its id is unique, and comparing ids needs no clock to be
	// right about — where a `finished_at` cutoff would have to reason about the
	// ledger's clock, this process's clock, and a row written while the baseline
	// was being read.
	before, err := readAcceptanceJobs(ctx, ws)
	if err != nil {
		return err
	}

	baseline := make(map[string]bool, len(before))

	for i := range before {
		if before[i].Billet != "" {
			baseline[before[i].LeaseID] = true
		}
	}

	if len(baseline) > 0 {
		fmt.Printf("\n%d job(s) already finished in this workspace before now; they do not "+
			"count towards --jobs.\n", len(baseline))
	}

	fmt.Printf("\nWaiting for %d NEW job(s) to finish on %s (up to %s).\n",
		opts.jobs, acceptanceScaleSets(ws.Tiers), opts.wait)
	fmt.Printf("Nothing here dispatches one — start a workflow that names one of those labels.\n\n")

	ticker := time.NewTicker(acceptancePoll)
	defer ticker.Stop()

	reported := 0

	for {
		jobs, err := readAcceptanceJobs(ctx, ws)
		if err != nil {
			return err
		}

		terminal := newTerminalJobs(jobs, baseline)
		if len(terminal) > reported {
			for i := range terminal[reported:] {
				j := &terminal[reported+i]
				fmt.Printf("  %s  tier=%s run=%d github=%q billet=%q\n",
					j.LeaseID, j.Tier, j.RunID, j.GitHub, j.Billet)
			}

			reported = len(terminal)
		}

		if len(terminal) >= opts.jobs {
			fmt.Printf("\n%d job(s) finished.\n", len(terminal))

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s with %d of %d NEW job(s) finished; the "+
				"teardown still runs", opts.wait, len(terminal), opts.jobs)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting: %w", ctx.Err())
		}
	}
}
