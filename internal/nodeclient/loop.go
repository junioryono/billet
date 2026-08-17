package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/wirecert"
)

// Compute is the part of internal/node.Runner the loop drives.
//
// An interface so the loop can be tested without a container runtime, and so the
// loop cannot quietly grow a dependency on anything else the runner exposes.
//
// KEEPALIVE AND TEND ARE HERE BECAUSE CUSTODY NEEDS AN OWNER, and leaving them
// out silently broke the whole custody design across the split. In one process
// the runner's janitor renews the leases of compute it could not confirm gone;
// a node that never runs it hands the server ErrCustody — "I am holding this" —
// while holding nothing. The server stops heartbeating, the reaper releases the
// capacity a TTL later, and a container keeps running on capacity that has been
// sold to somebody else. That is precisely the failure custody was built to
// prevent, reintroduced by moving the runner behind a network.
type Compute interface {
	Launch(ctx context.Context, lease *alloc.Lease, tier *nodeapi.TierSpec, job server.Job) error
	Destroy(ctx context.Context, requestID int64) error
	Recover(ctx context.Context) error

	// Instances are the lease ids this host is actually running, read from the
	// provider rather than from anything the control plane said.
	//
	// SENT AT REGISTRATION AS PROOF. A lease whose holder stopped heartbeating is
	// quarantined rather than terminalized, so its capacity stays charged until
	// the compute is confirmed gone — and the sweep that would confirm it only
	// fires for a container that still exists. A host that rebooted has none, so
	// without this its quarantined capacity would never come back.
	Instances(ctx context.Context) ([]string, error)
	Sweep(ctx context.Context) error

	// KeepAlive renews held leases on its own clock until the context ends.
	KeepAlive(ctx context.Context)
	// Tend advances custody entries: adopted work that finished, discarded work
	// whose cleanup is now confirmed.
	Tend(ctx context.Context) error

	// AssumeCustody takes a lease whose launch succeeded but whose result could
	// not be delivered, because the control plane will assume custody too.
	AssumeCustody(ctx context.Context, lease *alloc.Lease, requestID int64) error

	// Holding reports whether this node is still responsible for compute, which
	// is what decides whether a superseded process may stop.
	Holding() bool

	// Superseded moves running work into custody, because after supersession the
	// control plane routes its completions to somebody else.
	Superseded()
}

// defaultDrainTimeout bounds how long a stopping node waits for the compute it
// is holding.
//
// SIX HOURS BECAUSE THAT IS THE LENGTH OF A JOB: GitHub's
// jobs.<job_id>.timeout-minutes defaults to 360. A shorter wait would routinely
// destroy work that was about to finish, which is what the drain exists to stop.
const defaultDrainTimeout = 6 * time.Hour

// LoopOptions configures Run.
type LoopOptions struct {
	Provider   config.ProviderKind
	GuestOS    []config.GuestOS
	Deployment string
	// Site is where this machine is, or empty in a deployment with one place.
	Site string
	// VCPU and Memory are what this host CONTRIBUTES, which is what it detected
	// unless its own config said otherwise.
	//
	// Required. The control plane refuses a registration that offers nothing,
	// because a node contributing zero joins the fleet, is never chosen, and
	// produces no error for anyone to find.
	VCPU   int
	Memory config.ByteSize
	Log    *slog.Logger
	// SweepEvery bounds how often the node looks for compute nothing is asking
	// about. Zero disables it.
	SweepEvery time.Duration
	// Identity is this node's rotating certificate, when it has one. Nil on a
	// loopback wire, where there are no certificates to renew.
	//
	// GIVEN TO THE LOOP RATHER THAN RENEWED BY THE CALLER, because renewal has to
	// happen while the node is RUNNING. A check at startup would leave a host that
	// is up for a year to expire in place, and the failure is total: an expired
	// certificate cannot renew — renewal is authenticated by the certificate being
	// renewed — so the node has to be re-enrolled by hand.
	Identity *wirecert.Rotating

	// Hurry, when closed, ends the drain's wait early. It is the operator's
	// second signal: stop waiting, but still stop properly.
	Hurry <-chan struct{}
	// DrainTimeout bounds how long a stopping node waits for the compute it is
	// still holding before it gives up and lets the process exit. Zero uses a
	// default of six hours, which is how long GitHub lets a job run.
	DrainTimeout time.Duration
	// Backoff is how long to wait after a failed registration or poll. Zero uses
	// a default.
	//
	// It governs BOTH registration and poll failures, so a caller lengthening it
	// to calm a flapping link is not left hammering the poll endpoint.
	Backoff time.Duration
}

// registration is what this node tells the control plane about itself.
//
// Built in one place so the first registration and every re-registration after a
// reconnect describe the same machine. A drain re-registers too, and a node that
// came back claiming different capacity would move the fleet's arithmetic
// underneath work it is still holding.
func (o LoopOptions) registration() Registration {
	return Registration{
		Provider:   o.Provider,
		GuestOS:    o.GuestOS,
		Deployment: o.Deployment,
		Site:       o.Site,
		VCPU:       o.VCPU,
		Memory:     o.Memory,
	}
}

// registrationWithInventory is this node's registration, carrying what it is
// actually running when it can say.
//
// AN UNREADABLE PROVIDER SENDS NOTHING RATHER THAN AN EMPTY LIST, because those
// mean opposite things. A host genuinely running nothing must be able to free
// the capacity its quarantined leases hold — that is the reboot case. A host
// that could not reach its provider knows nothing, and letting its silence read
// as "running nothing" would free capacity for containers that are still there.
func registrationWithInventory(
	ctx context.Context, compute Compute, log *slog.Logger, opts LoopOptions,
) Registration {
	reg := opts.registration()

	ids, err := compute.Instances(ctx)
	if err != nil {
		log.Warn("could not read what this host is running, so the control plane keeps "+
			"charging for anything it cannot account for here until the next sweep",
			"error", err)

		return reg
	}

	reg.Instances, reg.InventoryKnown = ids, true

	return reg
}

// pollBackoff is how long to wait after a failed poll.
//
// Shorter than the registration backoff on purpose — a poll failure is usually a
// blink and the node should be back in line quickly — but derived from the
// caller's setting rather than fixed, so raising Backoff raises this too.
func (o LoopOptions) pollBackoff() time.Duration {
	if o.Backoff <= 0 {
		return time.Second
	}

	if o.Backoff < 5*time.Second {
		return o.Backoff
	}

	return o.Backoff / 5
}

// Run registers this node and serves commands until the context ends.
//
// RECOVERY HAPPENS BEFORE THE FIRST POLL, and the order is not incidental. A
// node that starts taking new work while its previous incarnation's containers
// are unaccounted for will double-count the host: the ledger believes that
// capacity is free, and it is not. Recover adopts what is still running so the
// leases behind it stay alive.
func Run(ctx context.Context, c *Client, compute Compute, opts LoopOptions) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	backoff := opts.Backoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}

	// STARTED ONCE, AFTER THE FIRST SUCCESSFUL REGISTRATION, and stopped with Run.
	//
	// Custody outlives any single registration: a lease held because a container could
	// not be confirmed gone must keep being renewed while the node re-registers or waits
	// out a restarting control plane. But it cannot start BEFORE the first one either —
	// the janitor reads the lease TTL to pick its cadence, and starting first means
	// reading a zero and renewing on a one-second fallback forever.
	//
	// AND IT DOES NOT INHERIT THE CALLER'S CANCELLATION, which is what makes the drain
	// mean anything: as a child of ctx it would stop renewing at the exact moment the
	// drain began waiting on that compute, and another tier could escrow capacity while
	// the container was still here. Cancelled when Run RETURNS instead.
	janitorCtx, stopJanitor := context.WithCancel(context.WithoutCancel(ctx))

	var (
		janitor     sync.WaitGroup
		startedOnce sync.Once
	)

	// ONE DEFER, IN ONE ORDER, because two were silently the wrong way round.
	// Deferred calls run last-in-first-out, so `defer stopJanitor()` followed by
	// `defer janitor.Wait()` waits FIRST and cancels afterwards — and Run hangs
	// forever on any exit its parent context did not cause. A registration refused
	// after the janitor had started is exactly that exit, and it is the one a node
	// takes when a control plane is replaced.
	defer func() {
		stopJanitor()
		janitor.Wait()
	}()

	startJanitor := func() {
		startedOnce.Do(func() {
			janitor.Add(1)

			go func() {
				defer janitor.Done()

				compute.KeepAlive(janitorCtx)
			}()
		})
	}

	err := register(ctx, c, compute, log, opts, backoff, startJanitor)

	// ONE PLACE DECIDES THAT A STOP MEANS A DRAIN, and it is here because there
	// are five ways out of the loop below and only one of them used to.
	//
	// A node notices its context has ended wherever it happens to be: inside the
	// registration call, inside a backoff after one failed, inside the backoff
	// after Recover failed, inside serve, or in the backoff after serve returned
	// something else. Only the serve path drained, and the rest are not idle
	// states — the ordinary route into them is a control plane restarting, which
	// leaves a node with containers running and a registration it cannot renew.
	// Stopping there renewed nothing, so the reaper reclaimed those leases at the
	// TTL and the capacity was sold to a second job while the first still ran.
	//
	// Draining when the node is holding nothing costs nothing: stopGracefully
	// returns immediately.
	if ctx.Err() != nil {
		return stopGracefully(ctx, c, compute, log, opts)
	}

	return err
}

// register keeps this node registered and serving until its context ends or it
// meets something no retry can fix.
func register(
	ctx context.Context,
	c *Client,
	compute Compute,
	log *slog.Logger,
	opts LoopOptions,
	backoff time.Duration,
	startJanitor func(),
) error {
	for {
		if err := c.Register(ctx, registrationWithInventory(ctx, compute, log, opts)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// A REFUSAL IS NOT AN OUTAGE, and treating them alike was the bug. A
			// version mismatch or a foreign deployment identity is refused the same
			// way forever; retrying it every few seconds produces a process that
			// looks alive, never works, and crashes nothing that would draw
			// attention. Stopping is the honest outcome — a supervisor restarting
			// it will meet the same wall and say so again.
			if errors.Is(err, ErrRefused) {
				return fmt.Errorf("this node cannot join that control plane: %w", err)
			}

			log.Error("could not register with the control plane; retrying",
				"error", err, "retry_in", backoff)

			if !sleep(ctx, backoff) {
				return ctx.Err()
			}

			continue
		}

		log.Info("registered with the control plane",
			"node", c.node, "provider", opts.Provider, "lease_ttl", c.LeaseTTL())

		// Now that the TTL is known, the janitor can pick a cadence that matches
		// the deadline the reaper on the other side actually enforces.
		startJanitor()

		if err := compute.Recover(ctx); err != nil {
			log.Error("could not reconcile with what is already running; not taking work "+
				"until this succeeds, because starting fresh work while the previous "+
				"incarnation's containers are unaccounted for double-counts the host",
				"error", err)

			if !sleep(ctx, backoff) {
				return ctx.Err()
			}

			continue
		}

		err := serve(ctx, c, compute, log, opts, false)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// SUPERSEDED IS NOT SOMETHING TO RETRY, and re-registering is the specific
		// wrong move. It would take the name back from whichever host holds it now,
		// that host would take it back in turn, and the control plane's accounting
		// would follow neither while both ran containers against the same leases.
		// Two hosts under one node name is a configuration mistake — a certificate
		// bundle copied to both, or the same node.name in two files — and an
		// operator has to fix it.
		//
		// STOPPING IMMEDIATELY IS ALSO WRONG. This process may be holding compute
		// right now, and the control plane keeps its heartbeat and result routes
		// open precisely so it can finish. Exiting cancels the janitor, and the
		// replacement cannot adopt what it cannot see — the container is on this
		// machine — so the lease is renewed by nobody and its capacity is resold
		// under a running job.
		if errors.Is(err, ErrSuperseded) {
			drain(ctx, compute, log, opts)

			return fmt.Errorf("this node has been superseded: %w", err)
		}

		// RE-REGISTERING IS THE ONLY ANSWER to being unknown, and it is what a node
		// meets after the control plane restarts. Anything else is retried in
		// place by serve itself.
		if errors.Is(err, ErrUnregistered) {
			log.Warn("the control plane no longer knows this node; registering again")

			continue
		}

		log.Error("the command loop stopped; registering again", "error", err)

		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// drain keeps a superseded node's obligations alive until it has none.
//
// It takes no new work — the control plane refuses it — and does the two things
// that let compute finish and be accounted for: renew what is held, and tend it
// so a lease is released once its container is confirmed gone. When nothing is
// held, this process has nothing left that anybody depends on.
//
// Bounded by the caller's context and nothing else, deliberately. A time limit
// here would be a limit on how long a job may run, which billet does not impose
// anywhere: the operator's fix is to stop the duplicate host, and until they do,
// holding the lease is the safe direction.
func drain(ctx context.Context, compute Compute, log *slog.Logger, opts LoopOptions) {
	// EVERYTHING RUNNING HERE IS NOW UNACCOUNTED FOR, which is what custody
	// means. The completion of a job running on this host is routed to whichever
	// process owns the name — the replacement — which cannot see it, reports the
	// destroy as done, and lets the lease go. Nothing would ever finish this work
	// otherwise: Tend is what confirms compute gone and releases its lease, and
	// Tend only looks at custody.
	compute.Superseded()

	if !compute.Holding() {
		return
	}

	log.Warn("another process has registered as this node; not taking further work, but " +
		"keeping the leases of compute already running here renewed until it finishes. " +
		"Stop whichever host is not meant to be this node")

	if waitForHolding(ctx, compute, log, opts) {
		log.Info("everything this node was holding has finished; stopping")
	}
}

// stopGracefully waits for the compute this node is holding to finish before
// letting the process exit.
//
// THE SAME WAIT AS A SUPERSEDED NODE'S, WITHOUT THE HAND-OVER, and the missing
// call is the whole difference. drain opens with compute.Superseded() because
// after supersession the control plane routes those completions to whichever
// process now holds the name; moving the work into custody is what stops this
// process waiting for reports that will never come to it.
//
// A SIGTERM is not that. Nobody is taking over, the completions still belong
// here, and calling Superseded would strand the very reports this wait exists
// for. The work stays exactly where it is and Holding() drains it.
//
// Nothing is served during this wait, so no new launch can arrive: serve has
// already returned, which is what brought us here.
func stopGracefully(ctx context.Context, c *Client, compute Compute, log *slog.Logger, opts LoopOptions) error {
	// A node holding nothing stops at once. The drain is for work in flight, not
	// a delay every restart pays.
	if !compute.Holding() {
		return ctx.Err()
	}

	grace := opts.DrainTimeout
	if grace <= 0 {
		grace = defaultDrainTimeout
	}

	drainCtx, endDrain := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer endDrain()

	// A SECOND SIGNAL ENDS THE WAIT, not the process. The goroutine also selects
	// on drainCtx so it cannot outlive the drain — one parked on a hurry channel
	// nobody closes would leak in every node that drains normally, which is all
	// of them.
	if opts.Hurry != nil {
		go func() {
			select {
			case <-opts.Hurry:
				endDrain()
			case <-drainCtx.Done():
			}
		}()
	}

	log.Info("draining: not taking new work, waiting for the compute already running here",
		"grace", grace)

	// IT KEEPS ANSWERING THE CONTROL PLANE, without which the drain is useless in
	// the ordinary case.
	//
	// Tend advances CUSTODY: work this node adopted or could not account for. A
	// job running normally is not custody, and what removes it is a Destroy, which
	// arrives over this command poll after the control plane learns from GitHub
	// that the job finished. Stop polling and that message can never be delivered,
	// so Holding() stays true until the whole grace expires — a node that always
	// waits its maximum, which is the opposite of draining.
	//
	// Launches are refused while this runs; see execute.
	var serving sync.WaitGroup

	serving.Add(1)

	go func() {
		defer serving.Done()

		for drainCtx.Err() == nil {
			err := serve(drainCtx, c, compute, log, opts, true)
			if drainCtx.Err() != nil {
				return
			}

			// SUPERSEDED DURING A DRAIN IS STILL SUPERSESSION. Another process now
			// owns this name, so the completions this wait depends on are being
			// routed there instead. Handing the work to custody is what lets Tend
			// finish it here rather than waiting for reports that will never come.
			if errors.Is(err, ErrSuperseded) {
				log.Warn("another process registered as this node while it was draining; " +
					"keeping its leases renewed until what is running here finishes")
				compute.Superseded()

				return
			}

			if errors.Is(err, ErrUnregistered) {
				log.Warn("the control plane no longer knows this node; registering again " +
					"so it can still be told to destroy what is running here")

				if err := c.Register(drainCtx, opts.registration()); err != nil {
					log.Error("could not register again while draining", "error", err)

					if !sleep(drainCtx, backoffFor(opts)) {
						return
					}
				}

				continue
			}

			if !sleep(drainCtx, backoffFor(opts)) {
				return
			}
		}
	}()

	drained := waitForHolding(drainCtx, compute, log, opts)

	// Stop serving before returning, and JOIN it: a command still in flight would
	// otherwise outlive Run and act on a node that has stopped.
	endDrain()
	serving.Wait()

	if drained {
		log.Info("everything running here has finished; stopping")

		return ctx.Err()
	}

	// NO TEARDOWN FOLLOWS THIS, and saying so plainly is better than implying one.
	// A node does not destroy its own compute on the way out: the containers
	// outlive this process, Recover re-adopts them when it starts again, and the
	// control plane's reaper reclaims the leases once they expire. Destroying them
	// here would kill jobs that are still running to save capacity that comes back
	// on its own.
	log.Warn("stopped waiting for the compute still running here; it keeps running and "+
		"will be re-adopted when this node starts again, and its capacity is held "+
		"until the reaper reclaims it",
		"grace", grace)

	return ctx.Err()
}

// backoffFor is the pause after a failed registration or poll.
func backoffFor(opts LoopOptions) time.Duration {
	if opts.Backoff > 0 {
		return opts.Backoff
	}

	return 5 * time.Second
}

// waitForHolding tends custody until this node is holding nothing, reporting
// whether it got there before the context ended.
//
// Shared by the supersession drain and the signal drain so the two cannot drift.
// What they must NOT share is the step before it — see stopGracefully.
func waitForHolding(ctx context.Context, compute Compute, log *slog.Logger, opts LoopOptions) bool {
	every := opts.SweepEvery
	if every <= 0 {
		every = time.Second
	}

	for compute.Holding() {
		if err := compute.Tend(ctx); err != nil {
			log.Error("could not advance custody while draining", "error", err)
		}

		if !sleep(ctx, every) {
			return false
		}
	}

	return true
}

// renewIfDue replaces this node's certificate before it expires.
//
// BEST EFFORT, AND QUIET WHEN IT FAILS. The window is a third of the
// certificate's remaining life — months, not hours — so a control plane that is
// down for a week costs nothing but a retry on the next pass. Making a renewal
// failure fatal would take a node out of the fleet over something it has ample
// time to do later.
//
// It is loud when it SUCCEEDS, because a certificate changing under a running
// host is exactly the kind of thing an operator wants in the log when they are
// working out why a fingerprint moved.
func renewIfDue(ctx context.Context, c *Client, log *slog.Logger, opts LoopOptions) {
	if opts.Identity == nil {
		return
	}

	left, due := wirecert.RenewalDue(opts.Identity.Leaf())
	if !due {
		return
	}

	certPEM, keyPEM, caPEM, err := c.Renew(ctx, c.node)
	if err != nil {
		log.Warn("could not renew this node's certificate; will try again",
			"expires_in", left.Round(time.Hour), "error", err)

		return
	}

	if err := opts.Identity.Replace(certPEM, keyPEM, caPEM); err != nil {
		// The OLD certificate is still in force: Replace verifies before it
		// installs, so a bad renewal leaves a working node working.
		log.Error("the control plane signed a certificate this node cannot use; keeping the "+
			"current one", "expires_in", left.Round(time.Hour), "error", err)

		return
	}

	// THE CLEANUP IS SEPARATE FROM THE RENEWAL. Installing succeeded; failing to
	// delete the generation it replaced leaves a second copy of this node's
	// private key on disk, which is worth saying now rather than at the next
	// restart — the process may not restart for months.
	if stale := opts.Identity.StaleCopies(); stale != nil {
		log.Warn("renewed this node's certificate, but the generation it replaced could not "+
			"be removed; a second copy of this node's private key is still on disk. Delete it",
			"error", stale)
	}

	log.Info("renewed this node's certificate",
		"not_after", opts.Identity.Leaf().NotAfter)
}

// serve polls for commands until something needs the caller to re-register.
func serve(ctx context.Context, c *Client, compute Compute, log *slog.Logger, opts LoopOptions, draining bool) error {
	sweepAt := time.Now().Add(opts.SweepEvery)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// ON THE SAME PASS AS THE SWEEP, so renewal needs no clock of its own. The
		// window is a third of the certificate's life, so a node that polls at all
		// in that time renews; one that does not is not running anyway.
		renewIfDue(ctx, c, log, opts)

		if opts.SweepEvery > 0 && time.Now().After(sweepAt) {
			if err := compute.Sweep(ctx); err != nil {
				// NOT FATAL. A sweep that fails leaves orphans for the next one; a
				// node that stopped serving because of it would leave every job
				// queued instead.
				log.Error("sweep failed; orphaned compute may be left behind", "error", err)
			}

			// TENDED ON THE SAME CADENCE. Custody entries are what let go of a
			// lease once the compute behind it is confirmed gone; without this the
			// janitor renews them forever and the capacity is never returned. The
			// keepalive goroutine only holds them — this is what ends them.
			if err := compute.Tend(ctx); err != nil {
				log.Error("could not advance custody; leases for compute that may already be "+
					"gone will keep being renewed until this succeeds", "error", err)
			}

			sweepAt = time.Now().Add(opts.SweepEvery)
		}

		cmd, ok, err := c.Poll(ctx)
		if err != nil {
			if errors.Is(err, ErrUnregistered) || errors.Is(err, ErrSuperseded) || ctx.Err() != nil {
				return err
			}

			// A POLL THAT FAILS IS ORDINARY. The control plane may be restarting or
			// the network may have blinked, and the node's job is to keep asking.
			log.Warn("command poll failed; retrying", "error", err, "retry_in", opts.pollBackoff())

			if !sleep(ctx, opts.pollBackoff()) {
				return ctx.Err()
			}

			continue
		}

		if !ok {
			continue
		}

		res := execute(ctx, compute, cmd, draining)

		if err := c.Report(ctx, res); err != nil {
			// THE WORK IS DONE AND THE ANSWER DID NOT LAND. The report was lost and the
			// plane timed the command out, or it arrived late and was answered with
			// ErrCustody — either way the plane has stopped heartbeating the lease while
			// the node holds the instance in its ordinary running set, which nothing
			// renews. The lease is then reaped while the container runs and its capacity
			// is sold twice, so the party that failed to report takes custody.
			//
			// Done before the ErrUnregistered check on purpose: being unknown to the
			// control plane is when custody matters most, and returning first would skip it.
			if res.OK && cmd.Kind == nodeapi.CommandLaunch && cmd.Lease != nil {
				// A FAILURE HERE IS RENEWAL FAILING, NOT CUSTODY FAILING. The
				// runner records the lease before it tries to renew it, precisely
				// because this call happens during the outage that lost the report
				// — so the janitor already owns it and will keep retrying.
				if custodyErr := compute.AssumeCustody(ctx, cmd.Lease, cmd.RequestIDOf()); custodyErr != nil {
					log.Warn("took custody of a launch whose result was lost, but could not "+
						"renew its lease yet; the janitor will keep trying",
						"command", cmd.ID, "lease", cmd.Lease.ID, "error", custodyErr)
				}
			}

			if errors.Is(err, ErrUnregistered) {
				return err
			}

			// Nothing is retried here: retrying a launch risks a second container
			// for one job, and the destroy path is idempotent but has no queue yet.
			log.Error("could not report a command's outcome; the control plane will treat "+
				"it as unknown, and a launch as custody",
				"command", cmd.ID, "kind", cmd.Kind, "error", err)
		}
	}
}

// ExecuteForTest runs one command, for tests that need the command semantics
// without a control plane to deliver them.
//
// Exported for tests only. The refusal paths — an unknown command kind, a launch
// with no lease — are reachable only from a server that is newer or broken, and
// staging either over a real wire would mean building a deliberately wrong
// control plane to test a node.
func ExecuteForTest(ctx context.Context, compute Compute, cmd nodeapi.Command) nodeapi.CommandResult {
	return execute(ctx, compute, cmd, false)
}

// execute runs one command and describes what happened.
func execute(ctx context.Context, compute Compute, cmd nodeapi.Command, draining bool) nodeapi.CommandResult {
	res := nodeapi.CommandResult{ID: cmd.ID}

	switch cmd.Kind {
	case nodeapi.CommandLaunch:
		// NO NEW WORK ON A NODE THAT IS STOPPING. It answers commands during a
		// drain so the control plane can still destroy what is here and hear that
		// it is gone; accepting a launch as well would mean the drain never
		// converges, since each new job extends the wait it is trying to finish.
		//
		// Refused rather than silently dropped: the control plane already knows
		// how to handle a launch that failed — it hands the capacity back and
		// GitHub reassigns — and no custody is claimed, because nothing started.
		if draining {
			res.Error = "this node is draining and will not start new work"

			return res
		}

		if cmd.Lease == nil || cmd.Job == nil {
			res.Error = "launch command arrived without a lease or a job"

			return res
		}

		err := compute.Launch(ctx, cmd.Lease, cmd.Tier, server.Job{
			RequestID: cmd.Job.RequestID,
			RunID:     cmd.Job.RunID,
			Event:     cmd.Job.Event,
		})
		if err == nil {
			res.OK = true

			return res
		}

		res.Error = err.Error()

		// CUSTODY IS CARRIED ACROSS AS A FLAG, not left in the message. The server
		// branches on it to decide whether the lease may be released, and matching
		// that out of prose is how a reworded error re-advertises capacity that a
		// container is still using.
		res.Custody = errors.Is(err, server.ErrCustody)

		return res

	case nodeapi.CommandDestroy:
		if err := compute.Destroy(ctx, cmd.RequestID); err != nil {
			res.Error = err.Error()

			// CUSTODY IS CARRIED HERE TOO, and its absence was #46's other half.
			//
			// A teardown a backend only ACCEPTED leaves the node holding the lease
			// — the guest is still shutting down and its capacity must stay charged
			// until it is provably gone. Without this flag the plane reads a plain
			// failure, which it treats as "the compute may still exist, keep the
			// lease and retry the destroy". That is safe in the same direction, but
			// it is wrong about who owns the lease: BOTH sides then hold it, the
			// listener heartbeats a lease the node's janitor is also renewing and
			// will release, and the retry re-issues a terminate on every pass for
			// the life of the process.
			res.Custody = errors.Is(err, server.ErrCustody)

			return res
		}

		res.OK = true

		return res

	case nodeapi.CommandSweep:
		if err := compute.Sweep(ctx); err != nil {
			res.Error = err.Error()

			return res
		}

		res.OK = true

		return res

	case nodeapi.CommandTend:
		if err := compute.Tend(ctx); err != nil {
			res.Error = err.Error()

			return res
		}

		res.OK = true

		return res

	default:
		// A COMMAND FROM A NEWER SERVER, refused rather than ignored. Ignoring it
		// would leave the caller waiting for a result that never comes, and its
		// launch would eventually be assumed to be in custody — holding capacity
		// for compute that was never started.
		res.Error = fmt.Sprintf("unknown command kind %q; this node speaks protocol version %d",
			cmd.Kind, nodeapi.Version)

		return res
	}
}

// sleep waits, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
