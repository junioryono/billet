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
type Compute interface {
	Launch(ctx context.Context, lease *alloc.Lease, job server.Job) error
	Destroy(ctx context.Context, requestID int64) error
	Recover(ctx context.Context) error
	Sweep(ctx context.Context) error
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
	Backoff time.Duration
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

	for {
		if err := c.Register(ctx, opts.Provider, opts.GuestOS, opts.Deployment); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// A REFUSAL AND AN OUTAGE LOOK THE SAME FROM HERE, and both are retried
			// — but the log has to say which, because one is fixed by waiting and
			// the other never is. A node quietly retrying a version mismatch
			// forever is a node nobody notices is broken.
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

			sweepAt = time.Now().Add(opts.SweepEvery)
		}

		cmd, ok, err := c.Poll(ctx)
		if err != nil {
			if errors.Is(err, ErrUnregistered) || ctx.Err() != nil {
				return err
			}

			// A POLL THAT FAILS IS ORDINARY. The control plane may be restarting or
			// the network may have blinked, and the node's job is to keep asking.
			log.Warn("command poll failed; retrying", "error", err)

			if !sleep(ctx, time.Second) {
				return ctx.Err()
			}

			continue
		}

		if !ok {
			continue
		}

		res := execute(ctx, compute, cmd)

		if err := c.Report(ctx, res); err != nil {
			if errors.Is(err, ErrUnregistered) {
				return err
			}

			// THE WORK IS DONE AND THE ANSWER IS LOST. The control plane will time
			// the command out and treat a launch as custody, which is why this is
			// survivable: the outcome it assumes is the safe one. Nothing is
			// retried here, because retrying a launch risks a second container.
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
