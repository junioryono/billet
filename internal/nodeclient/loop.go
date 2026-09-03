package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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

type completionAwareCompute interface {
	DestroyCompleted(ctx context.Context, requestID int64, result string) error
}

// upgradableCompute can replace this node's own billet.
//
// OPTIONAL, AND ITS ABSENCE IS A REFUSAL rather than a silent no-op. A build or a
// deployment shape with no updater — a node run out of a working directory, a
// test harness — must say so, because the alternative is a rollout recording a
// node as instructed while nothing ever happens to it, and then waiting forever
// for a convergence that cannot come.
//
// AN OPTIONAL CAPABILITY CANNOT CARRY A SAFETY INVARIANT, which is why this one
// carries none: everything about whether the upgrade is safe — the drain, the
// snapshot, the rollback — lives in the updater it starts, and this interface
// only decides whether there is one.
type upgradableCompute interface {
	// StartUpgrade launches the transactional updater DETACHED and returns as
	// soon as it is running.
	//
	// IT MUST NOT WAIT. A node executes commands one at a time and each command's
	// timeout starts when it is QUEUED, so an upgrade carried out inline would
	// hold the node's single slot for as long as the drain takes — which is as
	// long as the longest job, and there is no bound on that. Every other command
	// to this host would expire in the queue behind it, including the destroys
	// that let the drain finish.
	StartUpgrade(ctx context.Context, spec nodeapi.UpgradeSpec) error
}

// defaultDrainTimeout is when a stopping node starts REPORTING that its wait is
// long. It bounds nothing: the node waits for the compute it is holding for as
// long as that compute runs.
//
// SIX HOURS BECAUSE THAT IS THE LENGTH OF A JOB: GitHub's
// jobs.<job_id>.timeout-minutes defaults to 360, so a wait shorter than that is
// unremarkable and a longer one is worth a line in the log.
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
	// EC2Shapes are the ordered purchasable shapes of whichever REMOTE backend this
	// node runs. Empty for a host-backed provider.
	EC2Shapes []config.RemoteShape
	// CodeBuildFleet is the reserved-capacity CodeBuild fleet this node draws on,
	// or empty for on-demand compute.
	CodeBuildFleet string
	// CodeBuildJITParameterPath and CodeBuildRegion are where a codebuild node
	// stages runner registrations, reported so the control plane can sweep the
	// ones a dead node left behind. Empty for every other backend.
	CodeBuildJITParameterPath string
	CodeBuildRegion           string
	Log                       *slog.Logger
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
	// DrainTimeout is when a stopping node starts REPORTING that it is still
	// waiting for the compute it holds. It is not a deadline — nothing bounds
	// that wait but the work finishing or a second signal. Zero uses a default of
	// six hours, which is how long GitHub lets a job run.
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
		EC2Shapes:  o.EC2Shapes,
		// CARRIED ON EVERY RE-REGISTRATION, not just the first, for the reason this
		// function exists: a node that reconnected without it would look like an
		// on-demand host to the uniqueness check and free the fleet for a second
		// node while still drawing on it itself.
		CodeBuildFleet: o.CodeBuildFleet,
		// AND THE PATH, on every re-registration for the same reason: the control
		// plane overwrites it with whatever the host last said, so a reconnect that
		// omitted it would take the path out of the sweep.
		CodeBuildJITParameterPath: o.CodeBuildJITParameterPath,
		CodeBuildRegion:           o.CodeBuildRegion,
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
		watcherOnce sync.Once
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

	startWatcher := func() {
		watcher, ok := compute.(interface{ WatchInterruptions(ctx context.Context) })
		if !ok {
			return
		}
		watcherOnce.Do(func() {
			janitor.Add(1)

			go func() {
				defer janitor.Done()
				watcher.WatchInterruptions(janitorCtx)
			}()
		})
	}

	err := register(ctx, c, compute, log, opts, backoff, startJanitor, startWatcher)

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
	// A SUPERSEDED NODE HAS ALREADY DRAINED, AND DRAINING AGAIN NEVER ENDS.
	//
	// The superseded branch waits on the CALLER's context, so a first signal ends
	// that wait and register returns. Falling through to stopGracefully then
	// starts a SECOND drain on a context built with WithoutCancel — and since
	// nothing bounds a drain any more, that one waits forever for completions
	// routed to the process that replaced this one. The operator's stop is
	// answered by a hang until the supervisor SIGKILLs it, which is exactly the
	// bookkeeping loss the drain exists to avoid.
	//
	// It was survivable while a drain had a deadline; removing the deadline is
	// what made this path a wedge.
	//
	// NIL, because this is a deliberate stop that did what it was asked: the work
	// is in custody, Tend advanced what it could, and anything still running is
	// re-adopted when this node starts again. A non-zero exit would additionally
	// have systemd mark the unit failed — the same reasoning as stopGracefully's
	// own returns.
	if errors.Is(err, ErrSuperseded) && ctx.Err() != nil {
		return nil
	}

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
	startWatcher func(),
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

		// THE NEGOTIATED VERSION IS IN THIS LINE, because it is the one place a
		// node says which protocol it is actually serving on. A fleet mid-rollout
		// is otherwise only visible from the control plane.
		log.Info("registered with the control plane",
			"node", c.node, "provider", opts.Provider, "lease_ttl", c.LeaseTTL(),
			"protocol", c.WireVersion())

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
		// Only after recovery: before this point an interruption for an instance
		// left by an earlier process has no in-memory owner and would be discarded
		// as unrelated.
		startWatcher()

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

	grace := opts.DrainTimeout
	if grace <= 0 {
		grace = defaultDrainTimeout
	}

	if waitForHolding(ctx, compute, log, opts, grace,
		"stop this process; it has been superseded and is only finishing what it holds") {
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
	//
	// AND IT SAYS IT IS LEAVING. Stopping is the one moment the control plane
	// cannot tell from a partition on its own, and until it is told it goes on
	// placing work here for a whole silence window.
	//
	// Nil for the same reason as the two paths below: this is the shutdown
	// succeeding, and the caller turns what comes back into a process exit status.
	if !compute.Holding() {
		withdraw(ctx, c, log, opts)

		return nil
	}

	grace := opts.DrainTimeout
	if grace <= 0 {
		grace = defaultDrainTimeout
	}

	// NO DEADLINE. This waits for the compute already running here for as long as
	// it runs. A job may run for days, billet imposes no limit on one, and elapsed
	// time is not evidence that a job stopped making progress — so a timer here
	// was deciding when to stop answering the control plane, which is when a
	// FINISHED job's Destroy can no longer be delivered and its container is left
	// for the next start to re-adopt.
	//
	// `grace` survives as the point where this starts SAYING the drain is long;
	// see the warning inside waitForHolding.
	drainCtx, endDrain := context.WithCancel(context.WithoutCancel(ctx))
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

	log.Info("draining: not taking new work, waiting for the compute already running "+
		"here for as long as it takes", "report_after", grace)

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

	// superseded records that another process took the name during the drain,
	// after which this one has nothing to withdraw: the plane places on the
	// replacement, and asking would only be refused.
	var superseded atomic.Bool

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
				superseded.Store(true)

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

	drained := waitForHolding(drainCtx, compute, log, opts, grace,
		"send a second signal; the wait ends and the compute is left running")

	// Stop serving before returning, and JOIN it: a command still in flight would
	// otherwise outlive Run and act on a node that has stopped.
	endDrain()
	serving.Wait()

	// AFTER THE WAIT, NEVER DURING IT, and on both ways out of it. A withdrawal
	// says this process will not poll again, which is only true once serving has
	// stopped; and it removes the host from placement without touching a lease,
	// so it is as right for a drain the operator cut short — the compute stays
	// running, its leases stay charged, and nothing new should be aimed here —
	// as for one that finished. A superseded process is the exception: the name
	// is somebody else's now and the plane would refuse it.
	if !superseded.Load() {
		withdraw(ctx, c, log, opts)
	}

	if drained {
		log.Info("everything running here has finished; stopping")

		// NIL, NOT ctx.Err(). A cancelled context is how this stops ON PURPOSE, and
		// returning it makes the process exit 1 for a shutdown that did exactly what
		// it was asked. Measured on a packaged host: `systemctl stop billet-node`
		// left the unit Result=exit-code, ExecMainStatus=1, ActiveState=failed, so
		// `systemctl is-failed` answered yes after every clean drain and any
		// monitoring watching unit state reported a crash. The control plane already
		// takes this position for the same reason — see Server.Run.
		return nil
	}

	// NO TEARDOWN FOLLOWS THIS, and saying so plainly is better than implying one.
	// A node does not destroy its own compute on the way out: the containers
	// outlive this process, Recover re-adopts them when it starts again, and the
	// control plane's reaper reclaims the leases once they expire. Destroying them
	// here would kill jobs that are still running to save capacity that comes back
	// on its own.
	log.Warn("stopped waiting for the compute still running here; it keeps running and " +
		"will be re-adopted when this node starts again, and its capacity is held " +
		"until the reaper reclaims it")

	// ALSO NIL, and this is the less obvious half. Compute was left running, which
	// reads like a failure — but it is the documented outcome of an operator
	// escalating a drain, the containers are re-adopted on the next start, and the
	// warning above is how it is reported. Exiting non-zero would additionally
	// have systemd mark the unit failed, which is a claim about the SERVICE rather
	// than about the compute, and the service did what it was told.
	return nil
}

// withdrawAttempts bounds how many times a stopping node asks to be taken out of
// placement.
//
// A FEW, NOT FOREVER. The node is exiting, and a control plane that cannot
// record the withdrawal falls back to forgetting the host by silence — the
// behaviour this replaces, not a new failure. Three attempts cover a ledger
// blip; anything longer would hold up a stop to shave minutes off a window
// that closes on its own.
const withdrawAttempts = 3

// withdraw tells the control plane this node will not poll again.
//
// ONLY ONCE THE NODE HOLDS NOTHING IT IS STILL SERVING, which is why it is
// called from stopGracefully alone and after its wait. A node that says it is
// leaving while a launch is in flight would have its queued commands answered
// "nothing started" by the plane while its own report was about to say
// otherwise.
//
// BEST EFFORT, AND SAID SO. What a withdrawal buys is placement moving to the
// other hosts at once rather than after the silence window; what it can never
// change is the exit status, because a stop that did what it was asked is not a
// failure — so every outcome here ends in a log line and a return.
func withdraw(ctx context.Context, c *Client, log *slog.Logger, opts LoopOptions) {
	wire := c.WireVersion()

	// NEVER REGISTERED, so nothing to withdraw: the plane does not know this
	// process, and asking would only be answered "register again".
	if wire == 0 {
		return
	}

	// CHECKED WHERE IT IS EMITTED. An older control plane has no route for this
	// and answers it with a bare 404, which would read here as a decode failure
	// on every clean stop of every node in the fleet.
	if wire < nodeapi.VersionNodeWithdrawal {
		log.Info("this control plane is too old to be told that this node is leaving, so it "+
			"will keep placing work here until it forgets the node by silence",
			"protocol", wire, "needs", nodeapi.VersionNodeWithdrawal)

		return
	}

	// THE CALLER'S CONTEXT IS THE CANCELLED ONE — that is what brought the node
	// here. Each request still carries the client's own deadline.
	withdrawCtx := context.WithoutCancel(ctx)

	var err error

	for attempt := 1; attempt <= withdrawAttempts; attempt++ {
		err = c.Withdraw(withdrawCtx)

		switch {
		case err == nil:
			log.Info("withdrew from placement; the control plane will not aim work here " +
				"until this node registers again")

			return

		case errors.Is(err, ErrUnregistered):
			// The plane forgot this node already, so there is nothing to withdraw.
			return

		case errors.Is(err, ErrSuperseded):
			log.Info("another process is registered as this node, so this one has nothing "+
				"to withdraw", "error", err)

			return

		case errors.Is(err, ErrRefused):
			// A verdict rather than an outage; asking again cannot change it.
			log.Warn("the control plane refused this node's withdrawal; it keeps placing "+
				"work here until it forgets the node by silence", "error", err)

			return
		}

		if attempt < withdrawAttempts {
			sleep(withdrawCtx, opts.pollBackoff())
		}
	}

	log.Warn("could not withdraw from placement; the control plane keeps placing work "+
		"here until it forgets this node by silence",
		"attempts", withdrawAttempts, "error", err)
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
// stopHint names what ends this particular wait, because the two callers differ
// and neither answer is right for both: a drain is ended by an operator's second
// signal, while a superseded process has had no signal at all and is ended by the
// first one. A warning that prescribes the wrong lever is worse than one that
// prescribes none.
func waitForHolding(
	ctx context.Context, compute Compute, log *slog.Logger, opts LoopOptions,
	reportAfter time.Duration, stopHint string,
) bool {
	every := opts.SweepEvery
	if every <= 0 {
		every = time.Second
	}

	// A DRAIN THAT CANNOT END ITSELF HAS TO BE AUDIBLE. Nothing bounds this wait,
	// so the only thing that can tell an operator their node has been draining for
	// a day is the node saying so on a cadence — and a node that looks wedged is
	// one somebody kills, which is the outcome the whole change exists to avoid.
	started := time.Now()

	var warnedAt time.Time

	for compute.Holding() {
		err := compute.Tend(ctx)

		// CANCELLATION FIRST, BECAUSE BOTH LINES BELOW WOULD BE WRONG. A Tend that
		// was still running when the wait ended returns context.Canceled, which is
		// not a custody failure — and the overrun warning would tell an operator to
		// send a signal they have already sent. Neither is a lie worth logging at
		// the moment somebody is watching a shutdown.
		if ctx.Err() != nil {
			return false
		}

		if err != nil {
			log.Error("could not advance custody while draining", "error", err)
		}

		if waited := time.Since(started); waited >= reportAfter &&
			(warnedAt.IsZero() || time.Since(warnedAt) >= drainWarnEvery) {
			warnedAt = time.Now()

			log.Warn("still waiting for the compute running here, and this will not "+
				"time out: a node waits for as long as the work runs. Nothing is "+
				"destroyed by waiting",
				"waited", waited.Truncate(time.Second), "report_after", reportAfter,
				"stop_waiting", stopHint)
		}

		if !sleep(ctx, every) {
			return false
		}
	}

	return true
}

// drainWarnEvery bounds how often an overrunning node drain repeats itself.
//
// Often enough that an operator watching a log learns the node is still waiting,
// rarely enough that a multi-day drain does not write a book. The control plane
// uses the same interval for the same reason.
const drainWarnEvery = 15 * time.Minute

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
			// A LONG POLL IS ALSO THE CUSTODY CLOCK. Whole-host orphan inventory is
			// intentionally sparse, but known held entries are cheap to check and may
			// be the only thing preventing this node from advertising capacity. EC2
			// commonly finishes an asynchronous termination between two sweeps; waiting
			// for the next five-minute sweep keeps a slot charged after its instance is
			// already gone.
			if compute.Holding() {
				if err := compute.Tend(ctx); err != nil {
					log.Error("could not advance custody after the command poll; leases for "+
						"compute that may already be gone will keep being renewed until this succeeds",
						"error", err)
				}
			}

			continue
		}

		// A COMMAND CAN WIN THE SAME SELECT AS CANCELLATION. The HTTP server may
		// wake an already-parked poll while the caller is stopping; net/http then
		// returns the command even though this context has just become cancelled.
		// Treat that boundary as part of the drain: refuse a launch, but let a
		// destroy run under a fresh, bounded context so shutdown can still converge.
		res := executePolled(ctx, compute, cmd, draining, c.reqTimeout, c.WireVersion())

		// REPORT AN OUTCOME EVEN IF SHUTDOWN LANDED WHILE THE COMMAND RAN. Using
		// the cancelled polling context here made a successful destroy disappear;
		// the plane waited its whole command timeout while the node had already done
		// exactly what it asked. Report applies its own ordinary request deadline.
		reportCtx := ctx
		if ctx.Err() != nil {
			reportCtx = context.WithoutCancel(ctx)
		}

		if err := c.Report(reportCtx, res); err != nil {
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
	return executePolled(ctx, compute, cmd, false, requestTimeout, nodeapi.Version)
}

// executePolled settles the shutdown boundary before a command starts.
func executePolled(
	ctx context.Context,
	compute Compute,
	cmd nodeapi.Command,
	draining bool,
	shutdownTimeout time.Duration,
	negotiated int,
) nodeapi.CommandResult {
	if ctx.Err() == nil {
		return execute(ctx, compute, cmd, draining, negotiated)
	}

	commandCtx, finishCommand := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer finishCommand()

	return execute(commandCtx, compute, cmd, true, negotiated)
}

// execute runs one command and describes what happened.
//
// negotiated is the wire version this node's registration settled on. It is
// carried rather than read from the package constant because what a node may be
// asked to do is decided by that agreement, not by what this build prefers.
func execute(
	ctx context.Context, compute Compute, cmd nodeapi.Command, draining bool, negotiated int,
) nodeapi.CommandResult {
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
			RequestID:   cmd.Job.RequestID,
			RunID:       cmd.Job.RunID,
			Event:       cmd.Job.Event,
			Owner:       cmd.Job.Owner,
			Repository:  cmd.Job.Repository,
			WorkflowRef: cmd.Job.WorkflowRef,
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
		var err error
		if completed, ok := compute.(completionAwareCompute); ok && cmd.JobResult != "" {
			err = completed.DestroyCompleted(ctx, cmd.RequestID, cmd.JobResult)
		} else {
			err = compute.Destroy(ctx, cmd.RequestID)
		}
		if err != nil {
			res.Error = err.Error()

			// CUSTODY IS CARRIED HERE TOO, and its absence was the other half of the
			// unconfirmed-teardown gap.
			//
			// An unconfirmed teardown leaves the node holding the lease because the
			// compute may still exist and its capacity must stay charged until it is
			// provably gone. Without this flag the plane reads a plain
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

	case nodeapi.CommandUpgrade:
		if cmd.Upgrade == nil || cmd.Upgrade.Version == "" {
			res.Error = "upgrade command arrived without a release to install"

			return res
		}

		upgradable, ok := compute.(upgradableCompute)
		if !ok {
			// SAID OUT LOUD. A rollout that recorded this node as instructed while
			// nothing happened to it would wait forever for a convergence that
			// cannot come.
			res.Error = "this node has no transactional updater, so it cannot replace " +
				"its own billet; upgrade it out of band"

			return res
		}

		// DETACHED, AND THE RESULT SAYS ONLY THAT IT STARTED. Where the upgrade got
		// to is read from the rollout rather than from this command: the updater
		// outlives the process that started it, which is the whole reason it is a
		// separate program.
		if err := upgradable.StartUpgrade(ctx, *cmd.Upgrade); err != nil {
			res.Error = err.Error()

			return res
		}

		res.OK = true

		return res

	case nodeapi.CommandInventory:
		// ANSWERED WHILE DRAINING, unlike a launch. A stopping node is exactly the
		// one a barrier is asking about, and a drain that refused the question
		// would make `billet local down` unable to prove the machine it is about
		// to stop.
		//
		// FROM THE PROVIDER, never from anything the control plane said: the whole
		// point is to report compute the ledger has lost sight of, which by
		// definition is compute the control plane cannot enumerate for itself.
		ids, err := compute.Instances(ctx)
		if err != nil {
			// NOT OK, AND THAT IS THE ANSWER. "I could not read my provider" and "I
			// am running nothing" must never arrive as the same message; the plane
			// treats anything but OK as an interruption and starts the host's
			// continuous-empty run again.
			res.Error = err.Error()

			return res
		}

		res.OK = true
		res.BarrierID = cmd.BarrierID
		res.Instances = ids

		return res

	default:
		// A COMMAND FROM A NEWER SERVER, refused rather than ignored. Ignoring it
		// would leave the caller waiting for a result that never comes, and its
		// launch would eventually be assumed to be in custody — holding capacity
		// for compute that was never started.
		// THE NEGOTIATED VERSION, NOT THIS BUILD'S PREFERENCE. A command outside
		// what registration settled on is the control plane sending something this
		// pairing never agreed to, and naming the agreed version is what says so.
		res.Error = fmt.Sprintf(
			"unknown command kind %q; this registration negotiated protocol version %d "+
				"and this node speaks %s",
			cmd.Kind, negotiated, nodeapi.Self())

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
