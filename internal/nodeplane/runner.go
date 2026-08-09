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
	// A DESTROY MUST NOT WAIT ON A CORPSE. This is the broadcast that made stale
	// nodes expensive: each one held the call for the full command timeout and
	// then failed it, and the listener answers a failed destroy by holding its
	// lease forever. liveNodes expires them first.
	targets := r.liveNodes()

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
	type leg struct {
		node string
		err  error
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		legs []leg
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

			legs = append(legs, leg{node: n.name, err: err})
		}()
	}

	wg.Wait()

	if len(legs) == 0 {
		return nil
	}

	// A NODE FORGOTTEN DURING THE DESTROY IS THE SAME AS ONE FORGOTTEN BEFORE IT,
	// and the two disagreeing was the defect. A node already gone at the snapshot
	// above is not asked at all, and Destroy reports success. A node that dies
	// half a second later fails its leg, and a failed destroy makes the listener
	// hold the lease — for good, since nothing retries it and the host never comes
	// back to answer. Identical situations, opposite outcomes, decided by
	// microseconds.
	//
	// So they are made to agree, in the direction the rest of the design already
	// chose: the sweep a returning node runs is what removes compute the server no
	// longer recognises, and a node that never returns took its containers down
	// with it. Holding the capacity forever protects nothing and costs a slot.
	//
	// Note this asks the question AFTER the legs finish, deliberately: a node with
	// a command in flight is exempt from expiry, so it cannot be judged absent
	// until its command has stopped waiting.
	live := r.liveSet()

	kept := legs[:0]

	for _, l := range legs {
		if !live[l.node] {
			r.plane.log.Warn("a destroy went unanswered by a node that has since been forgotten; "+
				"its compute is the sweep's to remove when it returns",
				"node", l.node, "request_id", requestID, "err", l.err)

			continue
		}

		kept = append(kept, l)
	}

	if len(kept) == 0 {
		return nil
	}

	errs := make([]error, 0, len(kept))
	for _, l := range kept {
		errs = append(errs, l.err)
	}

	// EVERY FAILURE, not the first. A destroy that worked on four nodes and
	// failed on one has left compute running somewhere, and the operator needs to
	// know which — reporting only the first would hide the rest behind whichever
	// goroutine happened to finish soonest.
	return errors.Join(errs...)
}

// Sweep asks every node to destroy compute whose lease is no longer open.
//
// THIS IS WHAT KEEPS ORPHAN DETECTION ALIVE ACROSS THE SPLIT. The control plane
// sweeps after each reap, because the lease it has just terminalised is exactly
// what leaves a container unaccounted for — and it cannot enumerate a remote
// host. Without implementing this the server logs that its runner "cannot
// enumerate its compute" and quietly loses the ability to notice a leak at the
// only moment it reliably could.
//
// The node also sweeps on a timer of its own. That is a backstop, not a
// substitute: a timer notices minutes later, and only the server knows when an
// orphan was actually created.
func (r *Runner) Sweep(ctx context.Context) error {
	return r.broadcast(ctx, nodeapi.CommandSweep, "sweep")
}

// Tend asks every node to advance the compute it is holding capacity for.
func (r *Runner) Tend(ctx context.Context) error {
	return r.broadcast(ctx, nodeapi.CommandTend, "tend")
}

// KeepAlive does nothing here, and the nothing is the design.
//
// A NO-OP THAT SAYS WHY, because a silent one is indistinguishable from
// forgetting to implement it — and this interface exists precisely so the server
// can tell whether renewal is happening at all.
//
// Renewal is for leases a RUNNER is holding: compute it could not confirm gone,
// tracked in its custody map. When the runner is remote that map lives on the
// node, and the control plane cannot enumerate it. The node runs the same
// janitor for its own custody on its own clock, which is where it has to be —
// this interface's own comment says renewal must not share a schedule with
// anything that talks to a compute backend, and the only party talking to that
// backend is the node.
//
// So there is genuinely nothing here to renew, and pretending otherwise would
// mean guessing at leases this side cannot see. Blocking until the context ends
// matches the contract the caller relies on: it runs this in a goroutine and
// expects it to live as long as the process.
func (r *Runner) KeepAlive(ctx context.Context) { <-ctx.Done() }

// broadcast sends one whole-host command to every live node.
func (r *Runner) broadcast(ctx context.Context, kind nodeapi.CommandKind, what string) error {
	targets := r.liveNodes()

	if len(targets) == 0 {
		return nil
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, n := range targets {
		wg.Add(1)

		go func() {
			defer wg.Done()

			id, err := commandID()
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				return
			}

			pend := &pending{
				cmd:  nodeapi.Command{ID: id, Kind: kind},
				done: make(chan nodeapi.CommandResult, 1),
			}

			res, err := r.plane.dispatch(ctx, n, pend)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("node %s: %w", n.name, err))
				mu.Unlock()

				return
			}

			if !res.OK {
				mu.Lock()
				errs = append(errs, fmt.Errorf("node %s could not %s: %s", n.name, what, res.Error))
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	return errors.Join(errs...)
}

// liveNodes snapshots the fleet, forgetting anything that has gone silent.
func (r *Runner) liveNodes() []*node {
	r.plane.mu.Lock()
	defer r.plane.mu.Unlock()

	r.plane.expireStaleLocked()

	out := make([]*node, 0, len(r.plane.nodes))
	for _, n := range r.plane.nodes {
		out = append(out, n)
	}

	return out
}

// liveSet names the nodes the plane still knows about, expiring stale ones
// first.
func (r *Runner) liveSet() map[string]bool {
	live := make(map[string]bool)
	for _, n := range r.liveNodes() {
		live[n.name] = true
	}

	return live
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
