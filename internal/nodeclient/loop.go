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
	Launch(ctx context.Context, lease *alloc.Lease, job server.Job) error
	Destroy(ctx context.Context, requestID int64) error
	Recover(ctx context.Context) error
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
}

// LoopOptions configures Run.
type LoopOptions struct {
	Provider   config.ProviderKind
	GuestOS    []config.GuestOS
	Deployment string
	Log        *slog.Logger
	// SweepEvery bounds how often the node looks for compute nothing is asking
	// about. Zero disables it.
	SweepEvery time.Duration
	// Backoff is how long to wait after a failed registration or poll. Zero uses
	// a default.
	//
	// It governs BOTH, which the first version claimed and did not do: poll
	// failures used a hard-coded second, so a caller that lengthened this to calm
	// a flapping link still hammered the poll endpoint.
	Backoff time.Duration
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
	// Custody outlives any single registration: a lease the runner is holding
	// because it could not confirm a container gone must keep being renewed while
	// the node re-registers, reconnects, or waits out a control plane that is
	// restarting. Tying the janitor to each registration would stop renewing
	// exactly when the connection is least reliable, which is when custody is most
	// likely to exist.
	//
	// But it cannot start BEFORE the first one either, which is where it began.
	// The janitor reads the lease TTL to pick its cadence, and that value is
	// learned during registration — starting first meant reading a zero and
	// renewing on a one-second fallback for the process's whole life, while
	// racing the write that would have told it the truth.
	//
	// Its own context, cancelled when Run returns, so a node that stops because
	// it was refused does not leave a goroutine heartbeating behind it.
	janitorCtx, stopJanitor := context.WithCancel(ctx)

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

	for {
		if err := c.Register(ctx, opts.Provider, opts.GuestOS, opts.Deployment); err != nil {
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

		err := serve(ctx, c, compute, log, opts)
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
		// STOPPING IMMEDIATELY IS ALSO WRONG, and that was the first version. This
		// process may be holding compute right now; the control plane keeps its
		// heartbeat and its result routes open precisely so it can finish. Exiting
		// cancels the janitor, and the replacement cannot adopt what it cannot see
		// — the container is on this machine — so the lease is renewed by nobody
		// and its capacity is resold under a running job.
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
	if !compute.Holding() {
		return
	}

	log.Warn("another process has registered as this node; not taking further work, but " +
		"keeping the leases of compute already running here renewed until it finishes. " +
		"Stop whichever host is not meant to be this node")

	every := opts.SweepEvery
	if every <= 0 {
		every = time.Second
	}

	for compute.Holding() {
		if err := compute.Tend(ctx); err != nil {
			log.Error("could not advance custody while draining", "error", err)
		}

		if !sleep(ctx, every) {
			return
		}
	}

	log.Info("everything this node was holding has finished; stopping")
}

// serve polls for commands until something needs the caller to re-register.
func serve(ctx context.Context, c *Client, compute Compute, log *slog.Logger, opts LoopOptions) error {
	sweepAt := time.Now().Add(opts.SweepEvery)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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

		res := execute(ctx, compute, cmd)

		if err := c.Report(ctx, res); err != nil {
			// THE WORK IS DONE AND THE ANSWER DID NOT LAND, and that is not merely
			// survivable — it has to be MADE survivable here, because the control
			// plane's safe assumption and the node's belief would otherwise
			// disagree in the one direction that leaks.
			//
			// Two ways to arrive, one answer. The report may have been LOST, in
			// which case the plane timed the command out and assumed custody. Or it
			// may have ARRIVED TOO LATE and been answered with ErrCustody, which is
			// the plane saying the same thing out loud: it already told the listener
			// to stop heartbeating, so the lease is the node's now. A late success
			// used to be answered with a shrug, which left the container running
			// under a lease nobody renewed at all.
			//
			// The plane times the command out and reports custody, so it stops
			// heartbeating the lease. The node, having launched successfully, holds
			// the instance in its ordinary running set, which nothing renews. The
			// lease is then reaped while the container runs and its capacity is
			// sold twice. So the party that failed to report takes custody: the
			// handoff is caused rather than hoped for.
			//
			// Done before the ErrUnregistered check on purpose. Being unknown to
			// the control plane is exactly when custody matters most, and returning
			// first would skip it.
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
	return execute(ctx, compute, cmd)
}

// execute runs one command and describes what happened.
func execute(ctx context.Context, compute Compute, cmd nodeapi.Command) nodeapi.CommandResult {
	res := nodeapi.CommandResult{ID: cmd.ID}

	switch cmd.Kind {
	case nodeapi.CommandLaunch:
		if cmd.Lease == nil || cmd.Job == nil {
			res.Error = "launch command arrived without a lease or a job"

			return res
		}

		err := compute.Launch(ctx, cmd.Lease, server.Job{
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
