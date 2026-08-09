// Package nodeplane is the control plane's half of the node wire.
//
// It holds what the server knows about each registered node, hands out commands
// to nodes that are long-polling for them, and collects the results. The
// Runner it exposes is the same server.Runner the in-process path implements, so
// the listener cannot tell whether the compute it is driving is a goroutine away
// or a continent away.
package nodeplane

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
)

// ErrNoNode means no registered node can run a lease.
//
// Distinct from a launch failure: nothing was attempted, nothing is running, and
// the caller may release the lease without ambiguity. That certainty is the
// whole reason it is its own error.
var ErrNoNode = errors.New("nodeplane: no registered node can run this lease")

// ErrUnregistered means the node making a request is not known.
var ErrUnregistered = errors.New("nodeplane: node is not registered")

// ErrRefused means a registration was understood and rejected on its merits.
//
// PERMANENT, AND THAT IS THE DISTINCTION THAT MATTERS. A node told this stops
// rather than retrying, so it must never carry a failure that could heal. A
// protocol version mismatch and a foreign deployment identity qualify: they will
// be refused identically forever. A ledger that could not write the node row
// does NOT — that is an outage, and a node that gave up on it would stay down
// after the database came back.
var ErrRefused = errors.New("nodeplane: registration refused")

// defaultCommandTimeout bounds how long a launch waits for a node to answer.
//
// It is NOT a statement that nothing started. A command already handed to a node
// may be executing while this expires, so the caller is told the outcome is
// unknown and the lease is kept — see Runner.Launch.
const defaultCommandTimeout = 10 * time.Minute

// defaultPollTimeout bounds a command long-poll.
//
// Short enough that a node notices a dead connection promptly, long enough that
// an idle fleet is not generating constant traffic. The node is told this value
// at registration rather than choosing it, so both sides agree on when silence
// means "nothing to do".
const defaultPollTimeout = 50 * time.Second

// Plane tracks nodes and routes commands to them.
type Plane struct {
	log  *slog.Logger
	now  func() time.Time
	ttl  time.Duration
	poll time.Duration
	// commandTimeout bounds a caller's wait, NOT the node's work. Expiring it
	// says the outcome is unknown, never that nothing happened.
	commandTimeout time.Duration

	mu    sync.Mutex
	nodes map[string]*node

	registrar Registrar

	// deployment is the identity this control plane belongs to. A node carrying a
	// different one is refused: it would label its compute with an identity this
	// installation does not recognise, and the orphan sweeper would then find
	// containers it cannot attribute.
	deployment string
}

// node is one registered compute host.
type node struct {
	name     string
	provider config.ProviderKind
	guestOS  []config.GuestOS
	lastSeen time.Time

	// queue holds commands not yet handed to the node.
	queue []*pending
	// inflight holds commands handed over but not yet answered, by command id.
	//
	// Kept SEPARATE from the queue so a redelivery is a deliberate act rather
	// than an accident of leaving things on a list. A command moves back only if
	// the node reconnects and asks for it again.
	inflight map[string]*pending

	// waiting is signalled when a command arrives for a node that is polling.
	waiting chan struct{}
}

// pending is a command and the caller waiting for its result.
type pending struct {
	cmd  nodeapi.Command
	done chan nodeapi.CommandResult
	// delivered records that a node took this command. After that point a
	// timeout is AMBIGUOUS rather than a failure, because the node may be acting
	// on it right now.
	delivered bool
}

// Option configures a Plane.
type Option func(*Plane)

// WithClock replaces the clock, for tests.
func WithClock(now func() time.Time) Option { return func(p *Plane) { p.now = now } }

// ForgetForTest drops a node, as a control-plane restart would.
//
// Exported for tests only. It stages the one state a node cannot produce for
// itself: being unknown to a server that is still answering, which is what makes
// its next write fail with "register again" rather than with a transport error.
func (p *Plane) ForgetForTest(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.nodes, name)
}

// QueuedForTest reports how many commands are waiting to be taken.
//
// Exported for tests only. Expiry and dispatch race unless a test can see that
// the command it queued has actually arrived, and polling for that is the only
// way to make the ordering deterministic without inventing a hook production
// would never use.
func (p *Plane) QueuedForTest(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return 0
	}

	return len(n.queue)
}

// SetPollWindowForTest shortens the long-poll window.
//
// Exported for tests only, and named so nobody mistakes it for configuration:
// the window is part of the contract a node is told at registration, so a
// deployment that wants a different one changes it in one place rather than
// having each side pick.
func (p *Plane) SetPollWindowForTest(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.poll = d
}

// WithCommandTimeout bounds how long a launch waits for its result.
func WithCommandTimeout(d time.Duration) Option {
	return func(p *Plane) {
		if d > 0 {
			p.commandTimeout = d
		}
	}
}

// New builds a Plane.
func New(log *slog.Logger, deployment string, leaseTTL time.Duration, opts ...Option) *Plane {
	p := &Plane{
		log:            log,
		now:            time.Now,
		ttl:            leaseTTL,
		poll:           defaultPollTimeout,
		nodes:          map[string]*node{},
		deployment:     deployment,
		commandTimeout: defaultCommandTimeout,
	}

	for _, o := range opts {
		o(p)
	}

	return p
}

// Registrar is the ledger's node table.
//
// A NODE IS NOT REGISTERED UNTIL THE LEDGER SAYS SO. The plane's own map decides
// where commands go; the allocator's node row is what Bind checks before it will
// place a lease. Registering in one and not the other produced a node that took
// commands and then had every Bind refused — which looked like a broken node
// rather than a missing row.
type Registrar interface {
	RegisterNode(ctx context.Context, name string, kind config.ProviderKind) error
}

// WithRegistrar makes registration durable in the ledger as well as in memory.
func WithRegistrar(r Registrar) Option { return func(p *Plane) { p.registrar = r } }

// Register records a node's claim about itself.
//
// EVERY FIELD IS A CLAIM. What a node says about its provider and guest-OS
// support decides only what the server will ASK it to do; it can never widen the
// capacity ledger, whose limits come from the server's own configuration. A node
// that lies gets commands it cannot execute and fails them, which is a bad node
// rather than an over-committed host.
func (p *Plane) Register(
	ctx context.Context, req nodeapi.RegisterRequest,
) (nodeapi.RegisterResponse, error) {
	if req.Version != nodeapi.Version {
		return nodeapi.RegisterResponse{}, fmt.Errorf(
			"%w: node %q speaks protocol version %d, this control plane speaks %d; "+
				"upgrade whichever is older — they are separately deployed on purpose and a "+
				"mismatch must be a refusal rather than a decode error mid-launch",
			ErrRefused, req.Node, req.Version, nodeapi.Version)
	}

	if req.Node == "" {
		return nodeapi.RegisterResponse{}, fmt.Errorf("%w: a node must have a name", ErrRefused)
	}

	if req.Deployment != p.deployment {
		return nodeapi.RegisterResponse{}, fmt.Errorf(
			"%w: node %q belongs to deployment %s, this control plane is %s; a node labels its "+
				"compute with its own identity, so accepting it would produce containers this "+
				"installation cannot attribute",
			ErrRefused, req.Node, req.Deployment, p.deployment)
	}

	// THE LEDGER FIRST, and outside the mutex. A node that appears in the plane's
	// map but not in the allocator's node table takes commands and then has every
	// Bind refused — the failure looks like a broken node instead of a missing
	// row. Doing it first means a ledger that refuses leaves the plane unchanged,
	// so the node retries registration rather than believing it succeeded.
	if p.registrar != nil {
		if err := p.registrar.RegisterNode(ctx, req.Node, req.Provider); err != nil {
			// NOT WRAPPED IN ErrRefused. A ledger that cannot write is an outage,
			// not a verdict: the same node with the same config will succeed once
			// the database answers again, and a node that gave up here would stay
			// down after it recovered.
			return nodeapi.RegisterResponse{}, fmt.Errorf(
				"nodeplane: the ledger could not record node %q: %w", req.Node, err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[req.Node]
	if !ok {
		n = &node{name: req.Node, inflight: map[string]*pending{}, waiting: make(chan struct{}, 1)}
		p.nodes[req.Node] = n
	}

	// A RE-REGISTRATION IS A RESTART, and its in-flight commands are lost.
	//
	// The node that took them is gone, so nothing will ever report their results.
	// They are failed with custody rather than silently dropped or retried: a
	// launch that was handed over may have started something, and the node's own
	// recovery is what finds it. Retrying here would risk a second container for
	// one job.
	for id, pend := range n.inflight {
		p.answerLocked(pend, nodeapi.CommandResult{
			ID:      id,
			Custody: pend.cmd.Kind == nodeapi.CommandLaunch,
			Error: fmt.Sprintf("node %q re-registered while this command was in flight, so its "+
				"outcome is unknown", req.Node),
		})
	}

	n.inflight = map[string]*pending{}
	n.provider = req.Provider
	n.guestOS = req.GuestOS
	n.lastSeen = p.now()

	return nodeapi.RegisterResponse{
		Version:         nodeapi.Version,
		LeaseTTLSeconds: int(p.ttl.Seconds()),
		PollSeconds:     int(p.poll.Seconds()),
	}, nil
}

// answerLocked delivers a result to whoever is waiting, without blocking.
//
// The channel is buffered and written once; a caller that has already given up
// leaves nobody reading, and this must not stall the plane's mutex on that.
func (p *Plane) answerLocked(pend *pending, res nodeapi.CommandResult) {
	select {
	case pend.done <- res:
	default:
	}
}

// Nodes reports the registered node names, for diagnostics.
func (p *Plane) Nodes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.expireStaleLocked()

	out := make([]string, 0, len(p.nodes))
	for name := range p.nodes {
		out = append(out, name)
	}

	return out
}

// Poll blocks until a command is available for this node, the context ends, or
// the poll window closes.
//
// THE "NOTHING TO DO" CASE IS A BOOL, NOT A NIL. Returning a nil command with a
// nil error would make the ordinary outcome of an idle fleet indistinguishable
// from a bug at every call site, and this codebase has already rejected that
// shape twice — in the provider's Find and in the deployment lock — for the same
// reason: it is the return value that gets logged as a shrug.
//
// An empty command with ok=false is how a quiet poll ends. The node re-polls
// immediately; that is not an error and must not be treated as one.
func (p *Plane) Poll(ctx context.Context, nodeName string) (nodeapi.Command, bool, error) {
	p.mu.Lock()

	n, ok := p.nodes[nodeName]
	if !ok {
		p.mu.Unlock()

		return nodeapi.Command{}, false, ErrUnregistered
	}

	n.lastSeen = p.now()

	if cmd, took := p.takeLocked(n); took {
		p.mu.Unlock()

		return cmd, true, nil
	}

	wait := n.waiting
	p.mu.Unlock()

	timer := time.NewTimer(p.poll)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nodeapi.Command{}, false, ctx.Err()
	case <-timer.C:
		return nodeapi.Command{}, false, nil
	case <-wait:
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok = p.nodes[nodeName]
	if !ok {
		return nodeapi.Command{}, false, ErrUnregistered
	}

	cmd, took := p.takeLocked(n)

	return cmd, took, nil
}

// takeLocked moves the head of the queue into flight.
func (p *Plane) takeLocked(n *node) (nodeapi.Command, bool) {
	if len(n.queue) == 0 {
		return nodeapi.Command{}, false
	}

	pend := n.queue[0]
	n.queue = n.queue[1:]
	pend.delivered = true
	n.inflight[pend.cmd.ID] = pend

	return pend.cmd, true
}

// Result records what a node made of a command.
func (p *Plane) Result(nodeName string, res nodeapi.CommandResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[nodeName]
	if !ok {
		return ErrUnregistered
	}

	n.lastSeen = p.now()

	pend, ok := n.inflight[res.ID]
	if !ok {
		// A RESULT FOR SOMETHING NOBODY IS WAITING FOR IS NOT AN ERROR. The
		// caller may have timed out, or the node may be reporting again after a
		// reconnect. Both are ordinary, and refusing would make a node retry
		// something already accounted for.
		return nil
	}

	delete(n.inflight, res.ID)
	p.answerLocked(pend, res)

	return nil
}

// staleWindows is how many poll windows of silence mean a node is gone.
//
// A healthy node re-polls the moment its window closes, so several missed
// windows is real absence rather than idleness. Generous by that measure on
// purpose: forgetting a live node makes its next request fail with "register
// again", which recovers in one round trip, while forgetting too slowly leaves a
// corpse in every broadcast — and it was the corpse that caused the damage.
const staleWindows = 4

// staleAfter is how long a node may be silent before the plane forgets it.
//
// DERIVED FROM THE WINDOW THE NODE WAS ACTUALLY TOLD, not from the default. A
// deployment that lengthens the poll window would otherwise have healthy nodes
// expired during the very window the server instructed them to wait out.
func (p *Plane) staleAfter() time.Duration {
	window := p.poll
	if window <= 0 {
		window = defaultPollTimeout
	}

	return staleWindows * window
}

// expireStaleLocked drops nodes that have gone silent.
//
// NODES USED TO LIVE FOREVER, and lastSeen was written and never read. One host
// that was unplugged a week ago stayed in the fleet, so every Destroy broadcast
// waited out the full command timeout against it and returned an error — and the
// listener answers a destroy error by holding its lease and heartbeating it
// indefinitely. A single dead machine therefore made every subsequent completed
// job leak its capacity, permanently.
//
// In-flight commands are answered exactly as a re-registration answers them: a
// launch becomes custody, because the node may have started something before it
// went quiet, and a destroy becomes a plain failure the caller can retry.
// Dropping them silently would leave callers waiting on a machine that is gone.
func (p *Plane) expireStaleLocked() {
	cutoff := p.now().Add(-p.staleAfter())

	for name, n := range p.nodes {
		if !n.lastSeen.Before(cutoff) {
			continue
		}

		// A NODE WITH WORK IN FLIGHT IS NOT SILENT, IT IS BUSY.
		//
		// The loop executes commands synchronously, so a node pulling a five-minute
		// image does not poll while it works — and expiring it there was actively
		// harmful rather than merely wrong. Expiry answers an in-flight launch with
		// custody, the listener stops heartbeating on that, and the lease is reaped
		// before the provider has even returned. The launch then starts a runner on
		// capacity that has already been sold to somebody else.
		//
		// The command timeout is what bounds this instead, and it is the right
		// instrument: it is already the thing that decides how long an unanswered
		// command may run before its outcome is called unknown. A node that is
		// genuinely dead has its commands timed out first and becomes expirable
		// immediately afterwards.
		if len(n.inflight) > 0 {
			continue
		}

		for id, pend := range n.inflight {
			p.answerLocked(pend, nodeapi.CommandResult{
				ID:      id,
				Custody: pend.cmd.Kind == nodeapi.CommandLaunch,
				Error: fmt.Sprintf("node %q went silent for %s, so the outcome of this command "+
					"is unknown", name, p.staleAfter()),
			})
		}

		// Queued commands never reached it, so they are unambiguous: nothing
		// started, and the caller may act on that certainty.
		for _, pend := range n.queue {
			p.answerLocked(pend, nodeapi.CommandResult{
				ID: pend.cmd.ID,
				Error: fmt.Sprintf("node %q went silent before taking this command, so nothing "+
					"started", name),
			})
		}

		p.log.Warn("forgetting a node that stopped polling; it will have to register again",
			"node", name, "silent_for", p.now().Sub(n.lastSeen))

		delete(p.nodes, name)
	}
}

// pick chooses a node for a lease.
//
// The lease's own recorded constraints decide, not the live catalogue: TargetNode
// pins it, Providers says what it may run on. That is the same rule Bind
// enforces, and it is here for the same reason — a tier edited while a lease is
// open must not move the lease.
func (p *Plane) pick(lease *alloc.Lease) (*node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// EXPIRED LAZILY, at the two places staleness can do harm: choosing where to
	// launch, and broadcasting a destroy. A background sweeper would need its own
	// lifecycle and would only ever run between these; doing it here means a
	// stale node cannot be picked even once.
	p.expireStaleLocked()

	if lease.TargetNode != "" {
		n, ok := p.nodes[lease.TargetNode]
		if !ok {
			return nil, fmt.Errorf("%w: lease %s is pinned to node %q, which is not registered",
				ErrNoNode, lease.ID, lease.TargetNode)
		}

		if !acceptsProvider(n, lease) {
			return nil, fmt.Errorf(
				"%w: lease %s is pinned to node %q, which runs %s and the lease may not use it",
				ErrNoNode, lease.ID, n.name, n.provider)
		}

		return n, nil
	}

	// IN THE LEASE'S OWN ORDER OF PREFERENCE. Providers is most-preferred-first,
	// so walking it rather than the node map is what makes the preference mean
	// anything — iterating a map would pick by hash order.
	for _, want := range lease.Providers {
		for _, n := range p.nodes {
			if n.provider == want && acceptsGuestOS(n, lease) {
				return n, nil
			}
		}
	}

	return nil, fmt.Errorf("%w: lease %s needs one of %v and no registered node offers it",
		ErrNoNode, lease.ID, lease.Providers)
}

func acceptsProvider(n *node, lease *alloc.Lease) bool {
	for _, want := range lease.Providers {
		if n.provider == want {
			return acceptsGuestOS(n, lease)
		}
	}

	return false
}

// acceptsGuestOS reports whether a node claims to boot this lease's guest.
//
// An EMPTY allowlist means the node did not say, which is treated as "anything".
// The authoritative check is Bind, against the server's own node policy; this
// only avoids sending a command that is certain to fail.
func acceptsGuestOS(n *node, lease *alloc.Lease) bool {
	if len(n.guestOS) == 0 {
		return true
	}

	for _, os := range n.guestOS {
		if os == lease.GuestOS {
			return true
		}
	}

	return false
}
