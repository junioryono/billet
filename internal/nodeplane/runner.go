package nodeplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// Runner drives compute on a remote node.
//
// It implements server.Runner, so the listener cannot tell a remote node from an
// in-process one — which is the point of the split, and also why the ambiguity
// below has to be mapped onto the SAME errors the in-process runner returns
// rather than onto new ones the listener has never heard of.
type Runner struct {
	plane *Plane
}

// NewRunner returns the plane's server.Runner.
func (p *Plane) NewRunner() *Runner { return &Runner{plane: p} }

// Launch asks a node to start compute for a lease.
//
// THE THREE OUTCOMES ARE NOT TWO. A local launch either worked or failed; a
// remote one can also be UNKNOWN, and conflating unknown with failed is how
// capacity gets released while a container is still running.
//
//   - Nothing was sent: ErrNoNode. Nothing started, and the caller may release
//     the lease with a clear conscience.
//   - The node answered: its answer, translated. A clean failure releases; a
//     failure the node marked custody keeps the lease, because the node has
//     taken it into its own janitor.
//   - The node took the command and said nothing: server.ErrCustody. This is the
//     one that matters. The command may be executing right now, so the lease is
//     kept and the node's own recovery is what finds whatever started. Treating
//     silence as failure would re-advertise capacity that is in use.
func (r *Runner) Launch(ctx context.Context, lease *alloc.Lease, job server.Job) error {
	n, err := r.plane.pick(lease)
	if err != nil {
		return err
	}

	id, err := commandID()
	if err != nil {
		return err
	}

	pend := &pending{
		cmd: nodeapi.Command{
			ID:    id,
			Kind:  nodeapi.CommandLaunch,
			Lease: lease,
			Job: &nodeapi.Job{
				RequestID: job.RequestID,
				RunID:     job.RunID,
				Event:     job.Event,
			},
		},
		done: make(chan nodeapi.CommandResult, 1),
	}

	res, err := r.plane.dispatch(ctx, n, pend)
	if err != nil {
		return err
	}

	if res.OK {
		return nil
	}

	if res.Custody {
		return fmt.Errorf("%w: node %s: %s", server.ErrCustody, n.name, res.Error)
	}

	return fmt.Errorf("node %s could not launch lease %s: %s", n.name, lease.ID, res.Error)
}

// Destroy asks whichever node holds a request's compute to remove it.
//
// BROADCAST, because the server does not track which node holds which request
// and inventing that map here would be a second authority for a fact the ledger
// already owns. Destroy is idempotent by contract and a node with nothing for
// the request answers immediately, so asking all of them is correct rather than
// merely convenient.
//
// The result is the FIRST failure, if any: a destroy that only partly succeeded
// has left compute running somewhere and must not report success.
func (r *Runner) Destroy(ctx context.Context, requestID int64) error {
	r.plane.mu.Lock()

	targets := make([]*node, 0, len(r.plane.nodes))
	for _, n := range r.plane.nodes {
		targets = append(targets, n)
	}

	r.plane.mu.Unlock()

	if len(targets) == 0 {
		// NOT AN ERROR. There is nowhere for the compute to be, so there is
		// nothing to remove. Reporting failure here would make every shutdown
		// path on a fleet-less control plane look broken.
		return nil
	}

	// CONCURRENTLY, because sequentially is a trap. Every dispatch waits up to
	// the command timeout, so one wedged node in a fleet of twenty would hold a
	// destroy for that timeout before the next node was even asked — and destroy
	// runs on shutdown and on completion paths, where blocking for minutes turns
	// one bad host into a stalled control plane.
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		failed int
	)

	for _, n := range targets {
		wg.Add(1)

		go func() {
			defer wg.Done()

			err := r.destroyOn(ctx, n, requestID)
			if err == nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			failed++

			errs = append(errs, err)
		}()
	}

	wg.Wait()

	if failed == 0 {
		return nil
	}

	// EVERY FAILURE, not the first. A destroy that worked on four nodes and
	// failed on one has left compute running somewhere, and the operator needs to
	// know which — reporting only the first would hide the rest behind whichever
	// goroutine happened to finish soonest.
	return errors.Join(errs...)
}

// destroyOn asks one node to remove a request's compute.
func (r *Runner) destroyOn(ctx context.Context, n *node, requestID int64) error {
	id, err := commandID()
	if err != nil {
		return err
	}

	pend := &pending{
		cmd:  nodeapi.Command{ID: id, Kind: nodeapi.CommandDestroy, RequestID: requestID},
		done: make(chan nodeapi.CommandResult, 1),
	}

	res, err := r.plane.dispatch(ctx, n, pend)
	if err != nil {
		return fmt.Errorf("node %s: %w", n.name, err)
	}

	if !res.OK {
		return fmt.Errorf("node %s could not destroy request %d: %s", n.name, requestID, res.Error)
	}

	return nil
}

// dispatch queues a command and waits for its result.
func (p *Plane) dispatch(ctx context.Context, n *node, pend *pending) (nodeapi.CommandResult, error) {
	p.mu.Lock()

	n.queue = append(n.queue, pend)

	// Non-blocking, because the channel is a signal rather than a queue: a node
	// already awake will drain everything, and one that is not will find the work
	// on its next poll.
	select {
	case n.waiting <- struct{}{}:
	default:
	}

	p.mu.Unlock()

	timer := time.NewTimer(p.commandTimeout)
	defer timer.Stop()

	select {
	case res := <-pend.done:
		return res, nil
	case <-ctx.Done():
		return nodeapi.CommandResult{}, p.abandon(n, pend, ctx.Err())
	case <-timer.C:
		return nodeapi.CommandResult{}, p.abandon(n, pend,
			fmt.Errorf("node %s did not answer within %s", n.name, p.commandTimeout))
	}
}

// abandon stops waiting for a command and says what that means.
//
// WHETHER IT WAS DELIVERED IS THE WHOLE QUESTION. A command still sitting in the
// queue never reached a node, so nothing started and the caller is told plainly.
// One already handed over may be running, and the only safe answer is custody —
// the same answer the in-process runner gives when it cannot confirm a launch
// failed cleanly.
func (p *Plane) abandon(n *node, pend *pending, cause error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if pend.delivered {
		delete(n.inflight, pend.cmd.ID)

		if pend.cmd.Kind == nodeapi.CommandLaunch {
			return fmt.Errorf("%w: %w, so whether compute started is unknown",
				server.ErrCustody, cause)
		}

		return cause
	}

	for i, queued := range n.queue {
		if queued == pend {
			n.queue = append(n.queue[:i], n.queue[i+1:]...)

			break
		}
	}

	if pend.cmd.Kind == nodeapi.CommandLaunch {
		return fmt.Errorf("%w: %w before any node took the command, so nothing started",
			ErrNoNode, cause)
	}

	return cause
}

// commandID mints an identifier for one command.
//
// Random rather than sequential: a node reconnecting to a RESTARTED control
// plane must not be able to answer a fresh command with a stale result because
// the counter began again at one.
func commandID() (string, error) {
	var raw [16]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("nodeplane: mint a command id: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// Compile-time proof that a remote node is substitutable for a local one. If
// server.Runner grows a method, this fails here rather than at the wiring.
var _ server.Runner = (*Runner)(nil)

var _ = errors.Is
