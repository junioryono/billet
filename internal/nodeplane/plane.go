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

// ErrTakeCustody answers a result for a launch the plane stopped waiting for.
//
// THE REPORT IS NOT REJECTED, IT IS REDIRECTED. The node did the work and told
// the truth; it is simply later than the command timeout, and by then the plane
// had already told the listener that this lease was the node's. Answering 204
// would leave the container running under a lease NOBODY renews — the listener
// stopped because it was told custody, and the node never learned it had any.
// The reaper resells that capacity a TTL later.
var ErrTakeCustody = errors.New("nodeplane: this launch's lease is now the node's to hold")

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
	// owners records which incarnation was given the launch for each lease.
	//
	// A SUPERSEDED PROCESS MAY MAINTAIN WHAT IT HOLDS AND NOTHING ELSE. Permitting
	// every lease route by node name alone let a superseded host read the current
	// one's leases and RELEASE them: same name, same certificate, and an epoch it
	// could simply ask for. The capacity came back while a container was running.
	//
	// HELD ON THE PLANE, NOT ON THE NODE RECORD, because the node record expires.
	// A draining process outlives its replacement by design — that is the whole
	// point of draining — so when the replacement went silent and the node was
	// forgotten, the ownership went with it and the drain lost the right to renew
	// its own lease. Liveness and ownership answer different questions and cannot
	// share a lifetime.
	owners map[string]leaseOwner

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
	name string
	// incarnation is the node PROCESS currently registered under this name.
	//
	// The name is configuration and a certificate can be copied, so the name
	// alone cannot say whether a second registration is the same host restarting
	// or a different host arriving. This can: a restart brings a new value and
	// the old process is gone, while a duplicate brings a new value and the old
	// process keeps talking. Only the second produces requests carrying an
	// incarnation that is no longer current, and those are refused.
	incarnation string
	provider    config.ProviderKind
	guestOS     []config.GuestOS
	lastSeen    time.Time

	// queue holds commands not yet handed to the node.
	queue []*pending
	// inflight holds commands handed over but not yet answered, by command id.
	//
	// Kept SEPARATE from the queue so a redelivery is a deliberate act rather
	// than an accident of leaving things on a list. A command moves back only if
	// the node reconnects and asks for it again.
	inflight map[string]*pending

	// abandoned holds launches the plane stopped waiting for, by command id, with
	// the moment it gave up.
	//
	// A TOMBSTONE, and what it records is the lease changing hands. Abandoning a
	// delivered launch tells the listener the node has custody; if that launch
	// then succeeds and reports, this is the only thing left that can tell the
	// node it owns what it started.
	abandoned map[string]time.Time

	// waiting is signalled when a command arrives for a node that is polling.
	waiting chan struct{}

	// waiters counts pollers parked on this node, so a test can synchronise on a
	// poll that is genuinely blocked rather than on a sleep.
	waiters int
}

// rememberAbandoned records a launch the plane gave up waiting for.
func (n *node) rememberAbandoned(id string, at time.Time) {
	if n.abandoned == nil {
		n.abandoned = make(map[string]time.Time)
	}

	if len(n.abandoned) >= maxAbandoned {
		// Drop the oldest. The newer entries are the launches still likely to
		// report, and an unbounded map on a node whose every launch outlasts the
		// command timeout is a slow leak in the one process that must not fall over.
		var (
			oldest string
			when   time.Time
		)

		for id, at := range n.abandoned {
			if oldest == "" || at.Before(when) {
				oldest, when = id, at
			}
		}

		delete(n.abandoned, oldest)
	}

	n.abandoned[id] = at
}

// pending is a command and the caller waiting for its result.
type pending struct {
	cmd  nodeapi.Command
	done chan nodeapi.CommandResult
	// incarnation is the node PROCESS that took this command.
	//
	// The node NAME is not enough. A superseded process and its replacement share
	// a name and a certificate, so an entitlement looked up by name hands one
	// process the other's work — and the JIT route, which must stay open so a
	// launch already under way can finish, is exactly where that matters.
	incarnation string
	// delivered records that a node took this command. After that point a
	// timeout is AMBIGUOUS rather than a failure, because the node may be acting
	// on it right now.
	delivered bool
}

// leaseOwner is the node process responsible for a lease.
type leaseOwner struct {
	node        string
	incarnation string
	// requestID is the job this lease was launched for, so the destroy that ends
	// an ordinary job can end its ownership too. That destroy is the only signal
	// the wire gets: the lease itself is released in-process by the listener.
	requestID int64
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

// OwnsForTest reports whether a process is recorded as a lease's owner.
func (p *Plane) OwnsForTest(leaseID, node, incarnation string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	owner, ok := p.owners[leaseID]

	return ok && owner.node == node && owner.incarnation == incarnation
}

// WaitersForTest reports how many pollers are parked on this node.
//
// Exported for tests so they can synchronise on a poll that is genuinely WAITING
// rather than sleeping and hoping — the difference between testing what happens
// to a woken poll and testing what happens to a poll that never blocked.
func (p *Plane) WaitersForTest(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return 0
	}

	return n.waiters
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

	if n.incarnation != "" && n.incarnation != req.Incarnation {
		// SAID OUT LOUD, because the two causes need different fixes and look
		// identical from here. A restart is ordinary. A SECOND HOST arriving under
		// a name that is already taken is a configuration mistake — one bundle
		// copied to two machines, or one name in two config files — and the first
		// symptom otherwise is compute nobody can attribute.
		p.log.Warn("a different process has registered under this node's name; if the previous "+
			"one is still running, two hosts are sharing one identity and their compute "+
			"cannot be told apart",
			"node", req.Node, "was", n.incarnation, "now", req.Incarnation)
	}

	// A RE-REGISTRATION IS A RESTART, and its in-flight commands are lost.
	//
	// The node that took them is gone, so nothing will ever report their results.
	// They are failed with custody rather than silently dropped or retried: a
	// launch that was handed over may have started something, and the node's own
	// recovery is what finds it. Retrying here would risk a second container for
	// one job.
	for id, pend := range n.inflight {
		// TOMBSTONED, EXACTLY AS A TIMEOUT IS. This lease is being handed to the
		// node, and until now nothing recorded that. A launch that was in flight
		// across the re-registration — a partitioned host still working, or a
		// process that restarted while its provider kept going — would report
		// success afterwards, find no inflight entry and no tombstone, and be
		// answered 204. The listener had already stopped heartbeating on the
		// custody it was told about, so nothing held the lease at all.
		if pend.cmd.Kind == nodeapi.CommandLaunch {
			n.rememberAbandoned(id, p.now())
		}

		p.answerLocked(pend, nodeapi.CommandResult{
			ID:      id,
			Custody: pend.cmd.Kind == nodeapi.CommandLaunch,
			Error: fmt.Sprintf("node %q re-registered while this command was in flight, so its "+
				"outcome is unknown", req.Node),
		})
	}

	n.inflight = map[string]*pending{}

	// AN EMPTY CLAIM DOES NOT TAKE THE NAME. An older node registering beside a
	// current one would otherwise blank the stored incarnation, after which every
	// process passes the check and the fence is gone — the same bypass as an
	// absent header, arriving through registration instead.
	if req.Incarnation != "" {
		n.incarnation = req.Incarnation
	}
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

// ErrSuperseded means the request came from a node process that is no longer the
// registered one.
//
// TWO HOSTS UNDER ONE NAME is what this catches, and it is the shape a copied
// certificate bundle produces. Both authenticate — the certificate is genuine —
// and both claim the same node. Without this the control plane's answer to
// "whose compute is this" is whichever host polled last, and each host's
// reconciliation reasons about leases the other one owns.
var ErrSuperseded = errors.New("nodeplane: another process is registered as this node")

// CheckIncarnation reports whether a request came from the current node process.
//
// COMPATIBILITY IS SCOPED TO NODES THAT HAVE NOT CLAIMED ONE, and the first
// version scoped it to the REQUEST instead — which meant an absent header
// bypassed the check entirely. An older node running beside a current one would
// simply never send the header, and both would take work as the same node
// forever: the fence was disabled by the very thing it exists to catch.
//
// So absence is accepted only while the registered node is also absent — a fleet
// mid-upgrade, or the in-process runner, which has no wire to carry one. Once a
// process has claimed an incarnation, every later request must carry it.
func (p *Plane) CheckIncarnation(name, claimed string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok || n.incarnation == "" || n.incarnation == claimed {
		return nil
	}

	if claimed == "" {
		return fmt.Errorf(
			"%w: node %q is registered by process %s and this request carried no incarnation. "+
				"An older billet is running under the same node name — upgrade it, or check "+
				"for a certificate bundle copied to two machines",
			ErrSuperseded, name, n.incarnation)
	}

	return fmt.Errorf(
		"%w: node %q is registered by process %s, and this request came from %s. Two hosts "+
			"are configured as the same node — check for a certificate bundle copied to both, "+
			"or the same node.name in two config files",
		ErrSuperseded, name, n.incarnation, claimed)
}

// ErrNotEntitled means a node asked for something no command it holds allows.
var ErrNotEntitled = errors.New("nodeplane: this node holds no command that entitles it to that")

// forgetForRequest drops the ownership of whatever lease a request was launched
// under.
//
// The listener destroys a job's compute when it completes, which is the ONLY
// signal for an ordinary successful job: its lease is then released through the
// allocator, in-process, without ever touching this wire. Without this, every
// completed job left an entry for the life of the installation.
func (p *Plane) forgetForRequest(requestID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, owner := range p.owners {
		if owner.requestID == requestID {
			delete(p.owners, id)
		}
	}
}

// tookCommand reports which process was handed this command.
func (p *Plane) tookCommand(pend *pending) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return pend.incarnation
}

// OwnerOfRequest reports which process holds the compute a request was launched
// under, and whether that process is still the node's current one.
//
// A DESTROY IS ONLY CONFIRMED BY THE PROCESS THAT HAS THE CONTAINER. The wire
// broadcasts to whoever is polling, which is the CURRENT incarnation — so a
// superseded process draining its custody is never asked, answers nothing, and
// its replacement reports the destroy as done because it genuinely has nothing
// to remove. Believing that answer releases the lease under a live job.
func (p *Plane) OwnerOfRequest(requestID int64) (RequestOwner, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, owner := range p.owners {
		if owner.requestID != requestID {
			continue
		}

		n, live := p.nodes[owner.node]

		return RequestOwner{
			Node:        owner.node,
			Incarnation: owner.incarnation,
			Current:     live && n.incarnation == owner.incarnation,
		}, true
	}

	return RequestOwner{}, false
}

// RequestOwner is the process holding a request's compute, and whether it is
// still the one its node's commands reach.
type RequestOwner struct {
	Node string
	// Incarnation is the process itself, which is what a destroy's confirmation
	// must be compared against.
	//
	// CURRENCY IS A SNAPSHOT AND CANNOT BE TRUSTED LATER. It is read before the
	// command is dispatched and can change while the command is in flight: a
	// replacement registers, TAKES the destroy, truthfully reports it has nothing
	// to remove, and a decision made on the earlier reading treats that as the
	// owner confirming. The lease is released under a live container.
	Incarnation string
	// Current is false for a superseded process that is draining: it does not
	// poll, so it never sees a destroy and cannot confirm one. Useful for
	// deciding whether to bother asking, never for deciding who answered.
	Current bool
}

// ForgetLease drops the ownership record for a lease that has ended.
//
// BOUNDED, because the alternative is one map entry per job for the life of the
// installation. A node that never goes quiet is never expired, so nothing else
// would ever remove them.
func (p *Plane) ForgetLease(node, leaseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if owner, ok := p.owners[leaseID]; ok && owner.node == node {
		delete(p.owners, leaseID)
	}
}

// AdoptOwnership records that this process is responsible for leases the ledger
// already places on its node.
//
// A CONTROL PLANE RESTART FORGETS EVERYTHING, and a superseded process then
// cannot finish. The sequence: a node is holding compute, the plane restarts, the
// node re-registers and adopts what it finds, a second host supersedes it — and
// the new plane never saw the launch, so it has no owner for that lease. The
// draining process is refused its own release, custody is never given up, and
// the drain runs forever.
//
// The ledger knows what it forgot: a lease bound to this node and still open is
// this node's, and the process registering now is the one holding it.
func (p *Plane) AdoptOwnership(node, incarnation string, leaseIDs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.owners == nil {
		p.owners = make(map[string]leaseOwner, len(leaseIDs))
	}

	open := make(map[string]bool, len(leaseIDs))

	for _, id := range leaseIDs {
		open[id] = true

		if incarnation == "" {
			continue
		}

		// GAPS ONLY, never a claim. Every registration runs this, so overwriting
		// would let a REPLACEMENT take ownership of the leases the process it
		// superseded is still draining — which is the exact situation this exists
		// to survive. A lease already attributed to a process stays with it; the
		// current incarnation is permitted anyway, by name.
		if _, taken := p.owners[id]; !taken {
			p.owners[id] = leaseOwner{node: node, incarnation: incarnation}
		}
	}

	// AND STALE ADOPTIONS ARE DROPPED — but ONLY adoptions.
	//
	// An entry adopted from a snapshot carries no request id, so nothing can ever
	// name it again: no destroy matches it, and this map outlives node expiry. A
	// lease that ends between the read and the adopt leaves exactly that, and
	// repeated races accumulate them forever.
	//
	// A record created by DELIVERY is a different thing and must not be touched
	// here. Absence from the snapshot does not prove terminality: LaunchedLeaseIDs
	// reports only launching, online and busy, so a lease that was delivered and is
	// still `assigned` is legitimately missing — and deleting its owner would let
	// somebody else answer a destroy for a container that is about to exist.
	//
	// Runs even for an empty snapshot, because one-to-zero is exactly the shape
	// that used to strand an entry for the life of the process.
	for id, owner := range p.owners {
		if owner.node == node && owner.requestID == 0 && !open[id] {
			delete(p.owners, id)
		}
	}
}

// MayMutateLease reports whether this process may change a lease's fate.
//
// THE CURRENT PROCESS, OR THE ONE THAT WAS GIVEN THE LAUNCH. Anything else is a
// host acting on work it was never handed — which, between a superseded
// incarnation and its replacement, means releasing capacity that another host's
// container is still using.
func (p *Plane) MayMutateLease(node, incarnation, leaseID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// OWNERSHIP IS CHECKED BEFORE MEMBERSHIP, and that order is the fix. A
	// draining process outlives its replacement by design; when the replacement
	// goes silent the node record is forgotten, and requiring membership first
	// refused the drain the right to renew its own lease at exactly the moment
	// nothing else could. The lease then expired under compute that was still
	// running.
	if owner, ok := p.owners[leaseID]; ok && owner.node == node && owner.incarnation == incarnation {
		return nil
	}

	n, ok := p.nodes[node]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnregistered, node)
	}

	// COMPATIBILITY IS SCOPED TO THE NODE, NOT THE REQUEST — the same correction
	// CheckIncarnation needed last round, and I did not apply it here or to
	// EntitledToLaunch. Accepting an empty claim unconditionally makes the header
	// optional, and an optional fence is no fence: a superseded process simply
	// stops sending it, reads a lease id and epoch through the open routes, and
	// releases the replacement's lease.
	if n.incarnation == "" || n.incarnation == incarnation {
		return nil
	}

	return fmt.Errorf(
		"%w: node %q is registered by process %s, this request came from %s, and that process "+
			"was not given lease %s. A superseded process may maintain what it already holds "+
			"and nothing else",
		ErrSuperseded, node, n.incarnation, incarnation, leaseID)
}

// EntitledToLaunch reports whether a node is currently executing a launch for
// this lease.
//
// A REGISTERED NODE IS NOT AN ENTITLED ONE, and conflating the two left the JIT
// endpoint open to anything holding a node certificate. A registration proves
// which host you are; it says nothing about what work you were given. Without
// this, a compromised host could ask for runner registrations in a loop — for
// any scale set, under any name — and start runners that billet never escrowed
// capacity for, never tracked, and never tears down. That contradicts the one
// containment property the design claims: that compromising a compute host does
// not let it mint runners.
//
// The lease id carries the entitlement because it is already in the runner name
// billet chooses (see provider.InstanceName), so a node can only ask for the
// registration belonging to the launch it was actually told to perform.
func (p *Plane) EntitledToLaunch(node, incarnation, leaseID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[node]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnregistered, node)
	}

	for _, pend := range n.inflight {
		if pend.cmd.Kind != nodeapi.CommandLaunch || pend.cmd.Lease == nil {
			continue
		}

		// THE PROCESS THAT WAS GIVEN THE COMMAND, not merely the node that was.
		// The JIT route stays open to a superseded process on purpose — a launch it
		// began must be able to finish — and looking the entitlement up by name
		// alone would hand it the CURRENT process's work instead. A long poll
		// admitted before supersession can still wake afterwards holding a command,
		// so this is reachable without anything hostile happening.
		//
		// An EMPTY claim does not match a command that has an owner. Treating it as
		// a wildcard made the header optional, which let a process mint the
		// registration for somebody else's in-flight launch by simply omitting it.
		if pend.incarnation != "" && pend.incarnation != incarnation {
			continue
		}

		if pend.cmd.Lease.ID == leaseID {
			// THE TIER COMES BACK, because the lease id alone is not the whole
			// entitlement. A node holding an ordinary launch could otherwise ask for
			// a registration in ANOTHER scale set — the lease check passes, and the
			// runner it starts joins a tier with different labels, different jobs and
			// possibly different secrets. The caller resolves this tier's own set and
			// refuses anything else.
			return pend.cmd.Lease.Tier, nil
		}
	}

	return "", fmt.Errorf(
		"%w: node %q was not given a launch for lease %s, so it may not mint a runner "+
			"registration for it", ErrNotEntitled, node, leaseID)
}

// Seen records that a node just spoke, whatever it said.
//
// EVERY REQUEST IS EVIDENCE OF LIFE, and taking it only from Poll and Result was
// a silent way to kill a working node. The node's command loop is synchronous:
// if Recover, Sweep or Tend wedges — a hung Docker call is enough — the loop
// never reaches Poll again. Its custody janitor is a separate goroutine and
// keeps heartbeating perfectly well, but each heartbeat asked whether the node
// was registered, that question ran expiry, and the same call that proved the
// node alive was the one that declared it dead. Every later heartbeat was then
// refused as unregistered, and the leases it held — for compute that may still
// be running — expired.
//
// Command eligibility is bounded by the command timeout, which is the right
// instrument for a node that takes work and never answers. Membership is bounded
// by silence, which is the right instrument for a node that has gone.
// maxAbandoned bounds what one node's tombstones can cost.
//
// A node whose launches all outlast the command timeout would otherwise grow
// this without limit. Losing the OLDEST entry is the right sacrifice: the newer
// ones are the launches still likely to report.
const maxAbandoned = 1024

func (p *Plane) Seen(name, incarnation string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[name]
	if !ok {
		return
	}

	// ONLY THE CURRENT PROCESS VOUCHES FOR THE NODE. A superseded one draining its
	// custody keeps talking for as long as its job runs, and counting that as
	// liveness kept a DEAD replacement in the fleet: the plane went on choosing
	// that node, every launch it sent waited out the command timeout, and the
	// tier was effectively down for the length of somebody else's job.
	//
	// The draining process is not being disbelieved — its heartbeats and results
	// are still accepted. It simply is not evidence that the node is available
	// for work, because it is the one process that has been told it is not.
	if !currentLocked(n, incarnation) {
		return
	}

	n.lastSeen = p.now()
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
func (p *Plane) Poll(ctx context.Context, nodeName, incarnation string) (nodeapi.Command, bool, error) {
	p.mu.Lock()

	n, ok := p.nodes[nodeName]
	if !ok {
		p.mu.Unlock()

		return nodeapi.Command{}, false, ErrUnregistered
	}

	// CHECKED BEFORE EVERY HANDOVER, not only after a wait. The HTTP guard runs
	// before this function takes the mutex, so a supersession can land in between
	// and the fast path would hand a queued command to a process that no longer
	// owns the name — which then becomes that lease's recorded owner and holds a
	// genuine entitlement to mint its runner.
	if err := supersededLocked(n, nodeName, incarnation); err != nil {
		p.mu.Unlock()

		return nodeapi.Command{}, false, err
	}

	n.lastSeen = p.now()

	if cmd, took := p.takeLocked(n, incarnation); took {
		p.mu.Unlock()

		return cmd, true, nil
	}

	wait := n.waiting
	n.waiters++
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()

		if n, ok := p.nodes[nodeName]; ok {
			n.waiters--
		}

		p.mu.Unlock()
	}()

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

	// AND AGAIN AFTER WAITING, because a long poll is admitted at the start and
	// answered at the end, and a supersession can land between the two.
	if err := supersededLocked(n, nodeName, incarnation); err != nil {
		return nodeapi.Command{}, false, err
	}

	cmd, took := p.takeLocked(n, incarnation)

	return cmd, took, nil
}

// currentLocked reports whether this request speaks for the node's live process.
//
// AN EMPTY CLAIM IS NOT CURRENT once a process has claimed the name — the third
// place this needed saying. Treating it as eligible let a superseded process
// drop the header and keep a dead replacement schedulable indefinitely, simply
// by submitting results nobody asked for.
func currentLocked(n *node, incarnation string) bool {
	return n.incarnation == "" || n.incarnation == incarnation
}

// supersededLocked reports whether this process has lost the node's name.
//
// One place, so the fast path and the woken path cannot drift — they already had,
// which is how a queued command reached a process that no longer owned the name.
func supersededLocked(n *node, name, incarnation string) error {
	if n.incarnation == "" || n.incarnation == incarnation {
		return nil
	}

	return fmt.Errorf(
		"%w: node %q is registered by process %s and this request came from %s",
		ErrSuperseded, name, n.incarnation, incarnation)
}

// takeLocked moves the head of the queue into flight, recording who took it.
func (p *Plane) takeLocked(n *node, incarnation string) (nodeapi.Command, bool) {
	if len(n.queue) == 0 {
		return nodeapi.Command{}, false
	}

	pend := n.queue[0]
	n.queue = n.queue[1:]
	pend.delivered = true
	pend.incarnation = incarnation
	n.inflight[pend.cmd.ID] = pend

	// THE LEASE FOLLOWS THE PROCESS THAT WAS GIVEN IT. This is what lets a
	// superseded incarnation keep maintaining the launch it began — and stops it
	// touching one it was not given, which shares its node name and its
	// certificate and is otherwise indistinguishable.
	if pend.cmd.Kind == nodeapi.CommandLaunch && pend.cmd.Lease != nil && incarnation != "" {
		if p.owners == nil {
			p.owners = make(map[string]leaseOwner)
		}

		p.owners[pend.cmd.Lease.ID] = leaseOwner{
			node:        n.name,
			incarnation: incarnation,
			requestID:   pend.cmd.RequestIDOf(),
		}
	}

	return pend.cmd, true
}

// Result records what a node made of a command.
func (p *Plane) Result(nodeName, incarnation string, res nodeapi.CommandResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	n, ok := p.nodes[nodeName]
	if !ok {
		return ErrUnregistered
	}

	// THE RESULT IS ACCEPTED FROM ANY PROCESS — it is the handover itself — but
	// only the CURRENT one is evidence that this node can be given work. A
	// superseded process draining its custody reports for as long as its job runs,
	// and counting that as liveness kept a dead replacement in the fleet.
	if currentLocked(n, incarnation) {
		n.lastSeen = p.now()
	}

	pend, ok := n.inflight[res.ID]
	if !ok {
		// A LAUNCH THE PLANE GAVE UP ON IS DIFFERENT FROM ONE NOBODY IS WAITING
		// FOR. Abandoning a delivered launch tells the listener "the node has
		// custody", and the listener stops heartbeating on the strength of it. If
		// that launch then succeeds and reports, answering with a shrug leaves the
		// container running under a lease nothing renews.
		if _, abandoned := n.abandoned[res.ID]; abandoned {
			delete(n.abandoned, res.ID)

			if res.OK {
				return ErrTakeCustody
			}

			// A FAILED LATE REPORT NEEDS NO HANDOFF. Nothing is running, so there is
			// nothing to hold, and telling the node to take custody of a launch that
			// failed would have it renew a lease for compute that does not exist.
			return nil
		}

		// Otherwise ordinary: the caller may have timed out on a command that is
		// not a launch, or the node may be reporting again after a reconnect.
		// Refusing would make a node retry something already accounted for.
		return nil
	}

	delete(n.inflight, res.ID)

	// OWNERSHIP OUTLIVES THE COMMAND, and tying it to the command was a way to
	// lose a container. A launch that SUCCEEDS leaves a container running; deleting
	// the owner then let a second host register, take the lease through
	// AdoptOwnership, and refuse the process that is actually running it when it
	// later takes custody. A completion routed to the new owner finds nothing,
	// reports success, and the lease is released under a live job.
	//
	// So a successful launch KEEPS its owner, and the record ends where the
	// compute does: when the listener destroys it (destroyLocked below) or the
	// node releases it over the wire.
	//
	// A launch that FAILED without custody started nothing, so there is nothing
	// left to own.
	if pend.cmd.Kind == nodeapi.CommandLaunch && pend.cmd.Lease != nil &&
		!res.OK && !res.Custody {
		delete(p.owners, pend.cmd.Lease.ID)
	}

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
