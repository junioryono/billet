package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// STARTED ONCE, FOR THE PROCESS'S WHOLE LIFE, and outside the register loop.
	//
	// Custody outlives any single registration: a lease the runner is holding
	// because it could not confirm a container gone must keep being renewed while
	// the node re-registers, reconnects, or waits out a control plane that is
	// restarting. Tying the janitor to a registration would stop renewing exactly
	// when the connection is least reliable, which is when custody is most likely
	// to exist.
	//
	// It renews on its own clock rather than on the poll's, because the poll can
	// block for the whole poll window and a lease TTL is shorter than that window
	// is allowed to be.
	go compute.KeepAlive(ctx)

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
			if errors.Is(err, ErrUnregistered) || ctx.Err() != nil {
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
			// THE WORK IS DONE AND THE ANSWER IS LOST, and that is not merely
			// survivable — it has to be MADE survivable here, because the control
			// plane's safe assumption and the node's belief would otherwise
			// disagree in the one direction that leaks.
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
				if custodyErr := compute.AssumeCustody(ctx, cmd.Lease, cmd.RequestIDOf()); custodyErr != nil {
					log.Error("could not take custody of a launch whose result was lost; its "+
						"lease will be reaped while the container runs",
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
