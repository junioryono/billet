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
	// WHO HOLDS THIS, AND IS THAT PROCESS STILL LISTENING? A destroy reaches
	// whoever is polling, which is the CURRENT incarnation. A superseded process
	// draining its custody is never asked — so if it is the one with the
	// container, an answer from its replacement means nothing.
	owner, known := r.plane.OwnerOfRequest(requestID)

	// A DESTROY MUST NOT WAIT ON A CORPSE. This is the broadcast that made stale
	// nodes expensive: each one held the call for the full command timeout and
	// then failed it, and the listener answers a failed destroy by holding its
	// lease forever. liveNodes expires them first.
	targets := r.liveNodes()

	if len(targets) == 0 {
		// UNLESS SOMEBODY IS KNOWN TO BE HOLDING IT. A draining process does not
		// poll, so it is never in this list — and an empty fleet says nothing about
		// a container it is still running.
		if known && !owner.Current {
			return heldByADrainingProcess(owner.Node, requestID)
		}

		// NOT AN ERROR otherwise. There is nowhere for the compute to be, so there
		// is nothing to remove. Reporting failure here would make every shutdown
		// path on a fleet-less control plane look broken.
		r.plane.forgetForRequest(requestID)

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
		// NOT CONFIRMED BY THE PROCESS THAT HAS IT. The replacement answered, and
		// it answered honestly: it has nothing to remove. The container is on the
		// superseded process, which is renewing that lease and will destroy it when
		// its own tend confirms the job done. Custody is exactly that statement —
		// somebody is holding this, do not release it.
		if known && !owner.Current {
			return heldByADrainingProcess(owner.Node, requestID)
		}

		// THE COMPUTE IS ENDING, SO ITS OWNERSHIP IS TOO — and only here, where
		// every leg succeeded and the process that holds it was among them. An
		// unconditional forget dropped the record on a FAILED destroy too, which
		// left a process that was later superseded unable to renew or release what
		// it was holding, and its drain unable to end.
		r.plane.forgetForRequest(requestID)

		return nil
	}

	// A NODE'S ABSENCE IS NOT PROOF THAT ITS COMPUTE IS GONE, and for one review
	// round this code assumed it was. The reasoning looked sound: a node forgotten
	// BEFORE the snapshot above is never asked and Destroy reports success, so a
	// node forgotten half a second later ought to land the same way rather than
	// failing forever with nothing to retry it.
	//
	// It is false for every provider whose runtime outlives the billet process,
	// which is all of them. A node that takes a destroy and then crashes leaves a
	// Docker daemon holding a container that is still running, still executing a
	// job, and still using the capacity this lease represents. Forgiving that leg
	// releases the lease, and the capacity is sold twice.
	//
	// So the failure stands and the listener keeps holding the lease, which is the
	// safe direction: never release capacity while compute may be running. It
	// leaks a slot when a host never comes back, and that trade is deliberate — a
	// leak costs capacity, and the alternative costs correctness.
	//
	// The proper repair is to stop broadcasting. Destroy fans out because the
	// plane does not record WHERE a request's compute was placed, and the lease
	// already knows — it is bound to a node. Addressing the destroy there answers
	// both cases exactly, instead of inferring compute from fleet membership.
	errs := make([]error, 0, len(legs))
	for _, l := range legs {
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

// heldByADrainingProcess says that the only process able to confirm this destroy
// was never asked.
//
// Custody is exactly that statement: somebody is holding this, so its capacity
// must not be released. The draining process renews the lease and destroys the
// compute once its own tend confirms the job finished.
func heldByADrainingProcess(node string, requestID int64) error {
	return fmt.Errorf(
		"%w: request %d was launched by a process on %s that has since been superseded and "+
			"is draining; it holds that lease until its compute is confirmed gone",
		server.ErrCustody, requestID, node)
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
		return p.settle(n, pend, ctx.Err())
	case <-timer.C:
		return p.settle(n, pend,
			fmt.Errorf("node %s did not answer within %s", n.name, p.commandTimeout))
	}
}

// settle decides what a timed-out wait actually means.
//
// THE ANSWER MAY HAVE ARRIVED WHILE WE WERE WAKING UP, and treating that as a
// timeout was a way to lose a launch. Both branches are live at once: the timer
// fires, and Result takes the mutex first — deletes the inflight entry, sends
// the answer, and replies 204 to a node that is now certain its report landed.
// The timeout branch then declared custody to the listener, which stopped
// heartbeating. Nobody was holding the lease, the container was running, and
// both sides believed the other had it.
//
// Draining happens UNDER THE MUTEX, which is what makes it exact rather than
// hopeful: Result sends on this channel while holding the same lock, so once
// this goroutine holds it either the send has completed or it has not started.
// There is no third state to lose a result in.
func (p *Plane) settle(n *node, pend *pending, cause error) (nodeapi.CommandResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case res := <-pend.done:
		return res, nil
	default:
	}

	return nodeapi.CommandResult{}, p.abandonLocked(n, pend, cause)
}

// abandon stops waiting for a command and says what that means.
//
// WHETHER IT WAS DELIVERED IS THE WHOLE QUESTION. A command still sitting in the
// queue never reached a node, so nothing started and the caller is told plainly.
// One already handed over may be running, and the only safe answer is custody —
// the same answer the in-process runner gives when it cannot confirm a launch
// failed cleanly.
func (p *Plane) abandonLocked(n *node, pend *pending, cause error) error {
	if pend.delivered {
		delete(n.inflight, pend.cmd.ID)

		if pend.cmd.Kind == nodeapi.CommandLaunch {
			// REMEMBERED, because this is the moment the lease changes hands. The
			// listener is about to be told the node has custody and will stop
			// heartbeating. If the launch later succeeds and reports, the node has to
			// be told it now owns what it started — and the only thing that can tell
			// it is this record.
			n.rememberAbandoned(pend.cmd.ID, p.now())

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
