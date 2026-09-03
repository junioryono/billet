package rollout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// Host is what the coordinator knows about one machine in the fleet.
//
// READ FROM THE LEDGER, NOT FROM THE PLANE'S MEMORY. The release a host reports
// and the wire it negotiated are recorded at registration and survive a control
// plane restart; the plane's in-memory view does not, and a successor that had
// to wait for every host to reconnect before it could resume a rollout would
// stall for as long as the quietest node's poll interval.
type Host struct {
	Name string
	// Release is what the host said it was running at its last registration.
	//
	// EMPTY IS NOT "OLD". A build below VersionNodeRelease has no release to give,
	// which is the entire installed fleet on the day this ships. A host that says
	// nothing is one the coordinator cannot prove converged, so it stays pending
	// and blocks completion rather than being guessed about in either direction.
	Release string
	// Digest is the signed release manifest that produced this host's binary, or
	// empty when nothing on that machine could say.
	//
	// THE ONLY THING THAT DISTINGUISHES BYTES FROM A NAME. Release is the version
	// the host's binary was BUILT as, which two builds can share and which a moved
	// tag makes identical — so a rollout comparing versions alone converges on
	// evidence weaker than the decision it is converging. Empty is the ordinary
	// case and is read as "cannot tell"; a value that DISAGREES is a fact that
	// could not previously exist.
	Digest string
	// Wire is the protocol version its registration settled on.
	Wire int
	// Live is whether this deployment is currently in contact with it.
	Live bool
	// Epoch is the fencing token this host's CURRENT registration holds.
	//
	// THE ONLY THING THAT PROVABLY POSTDATES AN INSTRUCTION. Release and Live are
	// both true of a host that never left and of one that went away and came back
	// on the same binary, so without this the coordinator cannot tell a host still
	// draining from one that rolled itself back — and would leave the second in
	// the cohort forever.
	Epoch int64
}

// Fleet is where the coordinator learns what the hosts are.
type Fleet interface {
	Hosts(ctx context.Context) ([]Host, error)
}

// Dispatcher tells one node to replace its own billet.
//
// IT RETURNS WHEN THE UPDATER HAS STARTED, not when the upgrade is done. The
// node execs a detached transaction that outlives the command, so there is
// nothing to wait for — and waiting would hold the node's single command slot
// for the length of a drain, which has no bound.
type Dispatcher interface {
	Upgrade(ctx context.Context, node, version, manifestSHA256, rolloutID string,
		generation int64) error
}

// Coordinator drives one durable rollout to convergence.
//
// WITHOUT IT THE ROLLOUT IS A RECORD NOBODY ACTS ON. `billet rollout start` says
// the control plane picks the decision up and converges the fleet; a review
// pointed out that nothing did, so every started rollout stayed open forever and
// blocked the next one. This is what makes that sentence true.
//
// IT DOES NOT REPLACE THE CONTROL PLANE ITSELF. A process cannot install its own
// successor — that is `billet host-upgrade`, a detached program that outlives the
// process that started it. What the coordinator does about the controller is
// OBSERVE: when the running binary is the target, it records that, and only then
// does it begin on the nodes. That is what makes the rollout server-first, and it
// is an observation rather than an action precisely because the acting half
// cannot live in the process being replaced. What it MAY do is START that
// program, exactly as a node does when told to upgrade, when it has been given a
// SelfUpgrader: `release.automatic` wires one, so a deployment following a
// channel needs nobody at the controller's keyboard either.
type Coordinator struct {
	store      *Store
	fleet      Fleet
	dispatch   Dispatcher
	log        *slog.Logger
	now        func() time.Time
	minWire    int
	ourVersion string
	// self starts the transactional upgrade of this control plane's own host,
	// or is nil where an operator does that by hand. selfAsked is when it was
	// last asked per fleet decision, so a refused updater is retried on a
	// backoff rather than on every tick.
	self      SelfUpgrader
	selfAsked map[string]time.Time
	// warnedAt is when each host was last reported as out of contact, so a 30
	// second tick does not turn one stalled machine into a log nobody reads.
	//
	// IN MEMORY, AND THAT IS RIGHT. It bounds a diagnostic and authorises nothing;
	// a restarted control plane saying it once more is the correct behaviour, since
	// whoever is reading the log has just restarted it.
	warnedAt map[string]time.Time
}

// CoordinatorOption configures a Coordinator.
type CoordinatorOption func(*Coordinator)

// WithCoordinatorClock replaces the clock, so a test can drive backoff.
func WithCoordinatorClock(now func() time.Time) CoordinatorOption {
	return func(c *Coordinator) { c.now = now }
}

// WithCoordinatorLogger sets where the coordinator reports.
func WithCoordinatorLogger(log *slog.Logger) CoordinatorOption {
	return func(c *Coordinator) { c.log = log }
}

// SelfUpgrader starts the transactional upgrade of the host this control plane
// runs on. It is the node's Upgrader, given to the controller: the same detached
// `billet host-upgrade`, the same recovery journal, claim and decision fence,
// answering on the same inherited descriptor.
type SelfUpgrader interface {
	StartUpgrade(ctx context.Context, spec nodeapi.UpgradeSpec) error
}

// WithSelfUpgrader lets the coordinator start its own host's upgrade when a
// rollout's target is not the binary it is running.
func WithSelfUpgrader(u SelfUpgrader) CoordinatorOption {
	return func(c *Coordinator) { c.self = u }
}

// selfRetryAfter is how long a refused or failed self-upgrade waits before it is
// asked for again. An accepted one replaces this process, so nothing here ever
// sees it succeed; what this bounds is a refusal — a lock somebody else holds,
// a channel that would not resolve — repeating on every thirty-second tick.
const selfRetryAfter = 10 * time.Minute

// NewCoordinator builds the driver for a rollout.
//
// ourVersion is what this binary reports itself as, and minWire is the node-wire
// version below which a host cannot be told to upgrade. Both are passed in rather
// than read here, for the reason releasesource.Current is: a package that read
// them itself could only ever be tested against the build running the test.
func NewCoordinator(store *Store, fleet Fleet, dispatch Dispatcher,
	ourVersion string, minWire int, opts ...CoordinatorOption,
) *Coordinator {
	c := &Coordinator{
		store:      store,
		fleet:      fleet,
		dispatch:   dispatch,
		log:        slog.Default(),
		now:        time.Now,
		minWire:    minWire,
		ourVersion: ourVersion,
		warnedAt:   make(map[string]time.Time),
		selfAsked:  make(map[string]time.Time),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// retryAfter is how long a failed dispatch waits before being tried again.
//
// BOUNDED AND MODEST. A node that refuses an upgrade will not accept one sooner
// for being asked more often, and the point of retrying at all is that a host
// which was briefly unreachable converges without an operator noticing. The
// attempt count is durable, so this is a pace rather than a limit.
const retryAfter = 5 * time.Minute

// Run drives the open rollout until the context ends.
func (c *Coordinator) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		// A TICK THAT FAILS IS NOT A COORDINATOR THAT STOPS. Everything it reads
		// can be transiently unavailable — a busy ledger, a node mid-registration —
		// and a rollout that gave up on the first error would need an operator to
		// restart the control plane to resume it.
		if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("a rollout pass did not complete; retrying on the next tick",
				"error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick advances the open rollout by as much as it can prove.
//
// EVERY TRANSITION IS DRIVEN BY AN OBSERVATION, never by elapsed time. A host is
// converged because it registered reporting the target release; it is rolled back
// because it came back reporting the one before. Nothing here times a host out —
// a drain has no bound, and a coordinator that gave up on a slow one would be the
// timer this whole area refuses.
func (c *Coordinator) Tick(ctx context.Context) error {
	current, err := c.store.Open(ctx)
	if err != nil {
		if errors.Is(err, ErrNoRollout) {
			return nil
		}

		return err
	}

	hosts, err := c.fleet.Hosts(ctx)
	if err != nil {
		return fmt.Errorf("rollout: read the fleet: %w", err)
	}

	// SERVER FIRST, AND IT IS A GATE RATHER THAN A STEP. The control plane must be
	// on the target before any node is told to move, because the wire bridge runs
	// one way: an old server rejects a new node's registration in its strict
	// decoder before any version check can run, so a node rolled ahead of its
	// control plane is refused and stays refused.
	if !current.ControllerPhase.Converged() {
		return c.observeController(ctx, current)
	}

	byName := make(map[string]Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}

	nodes, err := c.store.Nodes(ctx, current.ID)
	if err != nil {
		return err
	}

	return c.advanceNodes(ctx, current, nodes, byName)
}

// observeController records that this control plane is the target, once it is.
//
// AN OBSERVATION, NOT AN UPGRADE. Nothing here replaces the binary: a process
// cannot install its own successor, and by the time a successor is running it is
// the one making this observation. What it does is notice — and until it can, no
// node is told to move.
func (c *Coordinator) observeController(ctx context.Context, current *Rollout) error {
	if c.ourVersion != current.TargetVersion {
		c.log.Debug("the control plane is not yet on the rollout's target",
			"running", c.ourVersion, "target", current.TargetVersion,
			"rollout", current.ID)

		c.upgradeSelf(ctx, current)

		return nil
	}

	// THE WHOLE SEQUENCE IS RECORDED, not skipped to the end.
	//
	// This process IS the successor: it started, opened the ledger it inherited,
	// migrated it if it had to, and is now serving. That is the evidence for every
	// phase between pending and committed, and walking them records the reasoning
	// rather than asserting the conclusion. It also keeps the state machine the one
	// authority on what may follow what — a jump straight to committed would need
	// an edge that lets anything reach it, which is exactly the edge the table
	// exists to refuse.
	// NO DIGEST FOR THE CONTROLLER. What proves this half is that THIS PROCESS is
	// the successor — it started, opened the ledger it inherited and is serving —
	// and a manifest digest read from its own machine would be this process
	// vouching for itself rather than a host reporting to somebody else.
	if err := c.walkTo(ctx, current.ID, "", current.ControllerPhase, PhaseCommitted,
		""); err != nil {
		return err
	}

	c.log.Info("the control plane is on the rollout's target; the nodes converge next",
		"version", current.TargetVersion, "rollout", current.ID)

	return nil
}

// upgradeSelf starts this host's own transactional upgrade toward the rollout's
// target, once per fleet decision within the backoff, when a SelfUpgrader was
// given.
//
// THE SAME INSTRUCTION A NODE GETS: version, manifest digest, rollout and
// generation, so `host-upgrade` fences it against a superseded decision exactly
// as it would on a node, and an updater that refuses says why on the answer
// channel. Nothing is recorded in the rollout: the controller's phases are an
// OBSERVATION made by the successor, and this process cannot be its own witness.
func (c *Coordinator) upgradeSelf(ctx context.Context, current *Rollout) {
	if c.self == nil {
		return
	}

	key := current.ID + "@" + strconv.FormatInt(current.Generation, 10)
	if last, ok := c.selfAsked[key]; ok && c.now().Sub(last) < selfRetryAfter {
		return
	}

	c.selfAsked[key] = c.now()

	spec := nodeapi.UpgradeSpec{
		Version:        current.TargetVersion,
		ManifestSHA256: current.TargetDigest,
		RolloutID:      current.ID,
		Generation:     current.Generation,
	}

	if err := c.self.StartUpgrade(ctx, spec); err != nil {
		c.log.Warn("the control plane's own host did not take the upgrade; retrying later",
			"target", current.TargetVersion, "rollout", current.ID, "error", err)

		return
	}

	c.log.Info("the control plane's own host is upgrading; this process will be replaced",
		"target", current.TargetVersion, "rollout", current.ID)
}

// convergenceWalk is the sequence a component takes from pending to committed.
var convergenceWalk = []Phase{
	PhaseDraining, PhaseReadyToInstall, PhaseInstalling, PhaseVerifying, PhaseCommitted,
}

// walkTo advances a component through the phases it has not yet reached.
//
// THE REMAINING SUFFIX, NOT THE WHOLE LIST, and replaying the whole list was a
// defect a review caught. Each Advance is its own transaction, so a restart or a
// storage failure partway leaves the component at, say, `installing` — and the
// next pass then tried to move it BACK to `draining`, which the state machine
// correctly refuses and which wedged that component permanently.
//
// Starting from where the component actually is makes the walk resumable, which
// is the property every other durable step in this issue already has.
// provedBy, when non-empty, is recorded with the step that reaches `to`.
//
// CARRIED THROUGH THE WALK RATHER THAN WRITTEN AFTER IT, so the commit and what
// proved the commit are one transaction. Writing it separately was the first
// version and it was wrong in a way a test caught immediately: the walk
// deliberately does nothing for a component that is not on it — a blocked host
// reporting the target — and a second Advance afterwards ignored that refusal and
// tried to commit it anyway.
func (c *Coordinator) walkTo(ctx context.Context, rolloutID, node string,
	from, to Phase, provedBy string,
) error {
	begin, ok := resumePoint(from)
	if !ok {
		// NOT ON THE WALK AT ALL. A blocked or rolled-back component reporting the
		// target is a real thing — somebody upgraded it out of band — but it is not
		// something billet may quietly convert into success: `blocked` exists
		// because billet could not prove something, and only a person can supply
		// what it could not. So this reports and leaves the decision where the phase
		// table already puts it.
		c.log.Info("this component reports the target but is not in a phase a rollout may "+
			"advance from; `billet rollout retry` returns it to the rollout",
			"component", componentName(node), "phase", from, "rollout", rolloutID)

		return nil
	}

	for _, phase := range convergenceWalk[begin:] {
		req := AdvanceRequest{RolloutID: rolloutID, Node: node, To: phase}
		if phase == to {
			req.ConvergedDigest = provedBy
		}

		if err := c.store.Advance(ctx, req); err != nil {
			return fmt.Errorf("rollout: advance %s to %s: %w", componentName(node), phase, err)
		}

		if phase == to {
			return nil
		}
	}

	return nil
}

// resumePoint is where in the walk a component with this phase carries on.
//
// `pending` starts at the beginning; a phase ON the walk starts after itself;
// anything else — blocked, rolling back, rolled back — is not resumable here and
// says so, rather than being replayed from the start into a transition the state
// machine refuses.
func resumePoint(from Phase) (int, bool) {
	if from == PhasePending {
		return 0, true
	}

	for i, phase := range convergenceWalk {
		if phase == from {
			return i + 1, true
		}
	}

	return 0, false
}

func componentName(node string) string {
	if node == "" {
		return "the controller"
	}

	return node
}

// advanceNodes moves as many hosts as the policy allows.
//
// TWO PASSES, AND THE SPLIT IS THE SAFETY CONTENT. The first settles every host
// billet has already acted on; the second dispatches to hosts it has not. Doing
// both in one loop made the failure budget depend on the ALPHABETICAL ORDER of
// the fleet: with a cohort of two and a budget of one, an earlier pending host
// was dispatched before a later draining host's rollback — which was already in
// the same snapshot — had been observed. billet held the evidence that its
// tolerance was spent and disturbed another machine anyway.
//
// Splitting them means everything knowable at the start of a pass is known before
// anything new is started.
func (c *Coordinator) advanceNodes(ctx context.Context, current *Rollout, nodes []Node,
	fleet map[string]Host,
) error {
	var problems []error

	for i := range nodes {
		n := &nodes[i]

		host, known := fleet[n.Node]

		if err := c.settleNode(ctx, current, n, host, known); err != nil {
			problems = append(problems, err)
		}
	}

	// A PASS THAT COULD NOT SETTLE WHAT IT SAW DOES NOT START ANYTHING NEW.
	//
	// The failure budget is derived from what the settled phases say, so an
	// observation that failed to record leaves it understated — a rollback stuck
	// halfway counts as in flight rather than as a failure, and the coordinator
	// would disturb another machine against a tolerance it had already spent. The
	// evidence was in this pass's own snapshot; the honest response to not having
	// been able to write it down is to wait for the next one.
	//
	// COMPLETION IS STILL ATTEMPTED, because Finish re-derives the whole answer
	// inside its own transaction and refuses while anything is unresolved.
	if len(problems) == 0 {
		// RE-READ RATHER THAN REASONED ABOUT. The first pass moved phases, and the
		// copies in `nodes` are what they were before it ran; the cohort and the
		// budget are decisions about the fleet AS IT NOW STANDS. Deriving them from
		// the stale copies is how a host that had just been recorded as rolled back
		// went on counting as in flight.
		settled, err := c.store.Nodes(ctx, current.ID)
		if err != nil {
			problems = append(problems, err)
		} else if err := c.dispatchPending(ctx, current, settled, fleet); err != nil {
			problems = append(problems, err)
		}
	}

	// ATTEMPTED AFTER EVERY PASS, not only when a host has just converged. The last
	// outstanding host can become terminal because an OPERATOR exempted or
	// decommissioned it, and nothing in that path runs `converge` — so a rollout
	// whose final resolution was a human decision stayed open forever, blocking
	// every later one. A transient Finish failure had the same shape: the committed
	// node was skipped on every subsequent pass and nothing retried.
	if err := c.completeIfConverged(ctx, current); err != nil {
		problems = append(problems, err)
	}

	return errors.Join(problems...)
}

// dispatchPending tells as many untouched hosts to upgrade as policy allows.
func (c *Coordinator) dispatchPending(ctx context.Context, current *Rollout, nodes []Node,
	fleet map[string]Host,
) error {
	inFlight, failed := tally(nodes)

	// THE FAILURE BUDGET STOPS NEW WORK, IT DOES NOT UNDO OLD WORK. A fleet of
	// fifty should not halt for one bad host; a fleet of two should not lose both.
	// What it bounds is how many more machines are disturbed — so it gates this
	// loop and nothing above it. A spent budget must still leave billet free to
	// record what the hosts already underway have done, or one failure leaves a
	// fleet unable to finish updating the ones that succeeded.
	budget := &failureBudget{
		failed: failed,
		limit:  current.Policy.FailureBudget,
		log:    c.log,
		id:     current.ID,
	}

	var problems []error

	for i := range nodes {
		n := &nodes[i]

		if n.Phase != PhasePending {
			continue
		}

		if inFlight >= current.Policy.Cohort || budget.spent() {
			break
		}

		host, known := fleet[n.Node]
		if !known || !host.Live {
			continue
		}

		if n.NextAttemptAt != "" {
			when, err := time.Parse(time.RFC3339Nano, n.NextAttemptAt)
			if err == nil && c.now().Before(when) {
				continue
			}
		}

		if err := c.dispatchUpgrade(ctx, current, n, host, &inFlight, budget); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// tally counts what the fleet is doing, for the cohort and the budget.
//
// IN FLIGHT MEANS DISTURBED, NOT MERELY UNFINISHED. Counting `pending` made the
// cohort full before anything was dispatched — one pending host against a cohort
// of one, so the coordinator refused to start it and the rollout never moved.
// What the cohort bounds is how many machines have been taken out of the
// deployment at once, and a host nobody has told anything is not one of them.
func tally(nodes []Node) (int, int) {
	var inFlight, failed int

	for i := range nodes {
		switch n := &nodes[i]; {
		case n.Phase.Terminal(), n.Phase == PhasePending:
		case n.Phase == PhaseBlocked || n.Phase == PhaseRolledBack:
			failed++
		default:
			inFlight++
		}
	}

	return inFlight, failed
}

// failureBudget tracks how much of the operator's tolerance a pass has spent.
type failureBudget struct {
	failed int
	limit  int
	log    *slog.Logger
	id     string
	warned bool
}

func (b *failureBudget) spent() bool {
	if b.limit <= 0 || b.failed < b.limit {
		return false
	}

	if !b.warned {
		b.warned = true

		b.log.Warn("this rollout has reached its failure budget and is starting no more "+
			"hosts; the ones already converged stay converged",
			"rollout", b.id, "failed", b.failed, "budget", b.limit)
	}

	return true
}

func (b *failureBudget) record() { b.failed++ }

// settleNode advances one host by whatever it can prove about it.
func (c *Coordinator) settleNode(ctx context.Context, current *Rollout, n *Node,
	host Host, known bool,
) error {
	if n.Phase.Terminal() {
		return nil
	}

	// A HOST THIS DEPLOYMENT CANNOT REACH STAYS WHERE IT IS, and this check comes
	// FIRST — putting it after the convergence test was a defect.
	//
	// Release is what the host said at its LAST registration, so a node that came
	// up on the target, failed its stability check and went away still reports the
	// target forever. Concluding "converged" from that marked a dead host as done
	// and, if it was the last one, closed the rollout on an offline fleet.
	//
	// It is not gone either: its compute may be running and it will come back
	// speaking whatever it spoke before. Only an operator proving compute absence
	// moves it, which is what `billet rollout decommission` records.
	if !known || !host.Live {
		c.warnOutOfContact(n)

		return nil
	}

	// A HALF-RECORDED ROLLBACK IS FINISHED FIRST, BEFORE ANYTHING ELSE IS ASKED.
	//
	// `rolling_back` is written and `rolled_back` follows it in a second
	// transaction, so a transient ledger failure between the two leaves a node
	// here — and the state machine allows only `rolled_back` or `blocked` out of
	// it, neither of which any other branch writes.
	//
	// IT COMES BEFORE THE TARGET CHECK, and a review caught it sitting after.
	// A host in `rolling_back` that then reports the TARGET — the half-written
	// record combined with the rollback inference's known false positive — went to
	// `converge`, which cannot resume from a phase that is not on the walk, so
	// every later pass repeated the same no-op and the rollout never closed.
	// Finishing the record first puts the host in `rolled_back`, from which the
	// correction below recovers it on the next pass.
	if n.Phase == PhaseRollingBack {
		return c.recordRolledBack(ctx, current, n, host)
	}

	// CONVERGED IS PROVED BY WHAT A LIVE HOST SAID. A host reporting the target
	// has been through its own transaction, which proved readiness before it
	// committed; that registration, plus the fact that billet is still in contact
	// with it, is the fleet-level evidence.
	if host.Release == current.TargetVersion {
		return c.settleAtTarget(ctx, current, n, host)
	}

	if n.Phase == PhaseDraining {
		return c.settleDrainingHost(ctx, current, n, host)
	}

	// EVERYTHING ELSE IS A HOST NOTHING HAS BEEN DONE TO. Starting on one is the
	// second pass's job, under the cohort and the failure budget.
	return nil
}

// settleAtTarget decides what a host reporting the target version has proved.
//
// THE VERSION IS THE HOST'S NAME FOR ITS BINARY; THE DIGEST IS THE BYTES. A
// rollout resolves a channel once, to one immutable signed manifest, and persists
// that manifest's digest precisely so every host installs the same bytes — and
// then read convergence off the version string, which two builds can share and
// which a moved tag makes identical. A host upgraded out of band, or rebuilt
// under the same name, converged a rollout on evidence weaker than the decision
// it was converging.
//
// THREE ANSWERS, AND THE MIDDLE ONE IS WHY THIS IS NOT A REFUSAL.
//
//   - The host names the manifest this rollout decided on: converged, proved.
//   - The host names NOTHING: converged on the version, and the rollout records
//     that nothing proved it. This is the entire installed fleet on the day the
//     field ships — including the hosts that would deliver the build able to name
//     one — so refusing here would be a rollout that can never complete, which is
//     a worse answer than a weaker one honestly recorded.
//   - The host names a DIFFERENT manifest: blocked. It is running the right
//     version from bytes this decision did not name, which is billet failing to
//     prove something rather than a host that failed — and `blocked` is the phase
//     that says so and names the ways out.
func (c *Coordinator) settleAtTarget(ctx context.Context, current *Rollout, n *Node,
	host Host,
) error {
	if host.Digest != "" && host.Digest != current.TargetDigest {
		c.log.Warn("a host reports the target version but a different release manifest; "+
			"it is running bytes this rollout did not decide on",
			"node", n.Node, "version", host.Release, "installed", host.Digest,
			"decided", current.TargetDigest, "rollout", current.ID)

		return c.store.Advance(ctx, AdvanceRequest{
			RolloutID: current.ID, Node: n.Node, To: PhaseBlocked,
			// BOTH STEPS, BECAUSE THE FIRST ONE ALONE LEAVES THE ROLLOUT WHERE IT WAS.
			// `blocked` exists because billet could not prove something, and nothing
			// automatic leaves it — a repaired host reaching settleAtTarget is refused
			// by the walk, which correctly declines to advance a phase only a person
			// may leave. An instruction that fixes the machine and leaves the rollout
			// stuck is an instruction somebody follows and then reports as not working.
			Blocker: fmt.Sprintf("this host reports %s, which is the target version, but "+
				"says it was installed from manifest %s and this rollout decided on %s — so "+
				"it is running bytes nobody here chose. Repair it in two steps: `billet "+
				"host-upgrade --version %s --manifest-sha256 %s --reinstall` on that "+
				"machine, then `billet rollout retry %s` here. Or record a decision about "+
				"it with `billet rollout exempt %s`",
				host.Release, short(host.Digest), short(current.TargetDigest),
				current.TargetVersion, current.TargetDigest, n.Node, n.Node),
		})
	}

	return c.converge(ctx, current, n, host.Digest)
}

// short renders a digest for a sentence an operator reads.
func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}

	return digest[:12]
}

// settleDrainingHost decides whether a host that was told to upgrade is still
// working on it or has rolled itself back.
//
// THE TWO ARE IDENTICAL IN EVERY FIELD BUT ONE. A host that has not started yet
// and a host that upgraded, failed and restored its previous release are both
// live and both reporting the old version. What separates them is the
// REGISTRATION EPOCH: a registration bumps it and nothing else does, so an epoch
// higher than the one the instruction was sent against provably postdates it.
//
// WITHOUT THIS THE ROLLED-BACK HOST STAYS IN `draining` FOREVER, holding the
// cohort's only slot and never counting against the failure budget — so a single
// failed host silently stops the whole rollout. A review found exactly that.
//
// AN UNRECORDED EPOCH CONCLUDES NOTHING. A row written before the column existed
// carries zero, and "I cannot tell" must not become "it rolled back" — that would
// record a rollback about a host that is quietly getting on with its drain.
func (c *Coordinator) settleDrainingHost(ctx context.Context, current *Rollout, n *Node,
	host Host,
) error {
	if n.DispatchEpoch == 0 || host.Epoch <= n.DispatchEpoch {
		// STILL WORKING ON IT. The updater drains for as long as the work on that
		// host takes, and nothing here bounds that.
		return nil
	}

	c.log.Warn("a host came back on its previous release, so its upgrade rolled back",
		"node", n.Node, "release", host.Release, "target", current.TargetVersion,
		"rollout", current.ID)

	if err := c.store.Advance(ctx, AdvanceRequest{
		RolloutID: current.ID, Node: n.Node, To: PhaseRollingBack,
		RollbackResult: rollbackResult(host, current),
	}); err != nil {
		return fmt.Errorf("rollout: record that %s rolled back: %w", n.Node, err)
	}

	return c.recordRolledBack(ctx, current, n, host)
}

// recordRolledBack completes a rollback that has been observed.
//
// SEPARATE FROM OBSERVING IT, because the two are separate transactions and the
// second can fail on its own. Reached both immediately after the observation and
// on a later pass for a node a failure left in `rolling_back`.
func (c *Coordinator) recordRolledBack(ctx context.Context, current *Rollout, n *Node,
	host Host,
) error {
	if err := c.store.Advance(ctx, AdvanceRequest{
		RolloutID: current.ID, Node: n.Node, To: PhaseRolledBack,
		RollbackResult: rollbackResult(host, current),
	}); err != nil {
		return fmt.Errorf("rollout: record that %s finished rolling back: %w", n.Node, err)
	}

	// NOT COUNTED HERE. The budget is derived from the DURABLE phases after every
	// observation has been settled, which is what makes it independent of the order
	// the hosts happen to be named in — and what stops a rollback recorded across
	// two passes being counted twice.
	return nil
}

func rollbackResult(host Host, current *Rollout) string {
	return fmt.Sprintf("re-registered on %s after being told to install %s",
		host.Release, current.TargetVersion)
}

// stalledEvery is how often a rollout says that it is waiting on a host it
// cannot reach.
//
// A THRESHOLD CROSSED SILENTLY IS A FLEET THAT LOOKS WEDGED, which is the same
// argument warnDrainOverrun makes on the listener. A host told to upgrade that
// then goes out of contact holds its cohort slot indefinitely and nothing times
// it out — deliberately, because its compute may be running — so the only thing
// standing between an operator and a rollout that has silently stopped moving is
// billet saying so. Repeated rather than said once, because the operator who
// needs it is usually not the one who was watching when it started.
const stalledEvery = 15 * time.Minute

// warnOutOfContact reports a host the rollout has acted on and can no longer
// reach.
//
// ONLY FOR A HOST PAST `pending`. An untouched host that billet cannot reach is
// the ordinary case an operator already sees in `billet rollout status`; one that
// was TOLD to upgrade and then went quiet is the one holding everything up.
func (c *Coordinator) warnOutOfContact(n *Node) {
	if n.Phase == PhasePending || n.Phase.Terminal() {
		return
	}

	if c.warnedAt == nil {
		c.warnedAt = make(map[string]time.Time)
	}

	now := c.now()

	if last, ok := c.warnedAt[n.Node]; ok && now.Sub(last) < stalledEvery {
		return
	}

	c.warnedAt[n.Node] = now

	c.log.Warn("this rollout is waiting on a host it cannot reach; nothing will time it "+
		"out, because its compute may still be running. `billet rollout decommission` "+
		"records that the machine is gone, and `billet rollout exempt` records a decision "+
		"to skip it",
		"node", n.Node, "phase", n.Phase, "rollout-waiting-since", n.UpdatedAt)
}

// dispatchUpgrade tells one host to replace its billet.
func (c *Coordinator) dispatchUpgrade(ctx context.Context, current *Rollout, n *Node,
	host Host, inFlight *int, budget *failureBudget,
) error {
	// THE WIRE GATE, AND IT BLOCKS RATHER THAN FAILS. A host that negotiated a
	// version without the upgrade command cannot be told to move by billet at all,
	// and retrying forever would burn a slot on a machine that can never accept it.
	// Blocking it says so once, keeps its capacity out of the rollout's way, and
	// leaves an operator the choice of upgrading it out of band or exempting it.
	if host.Wire > 0 && host.Wire < c.minWire {
		if err := c.store.Advance(ctx, AdvanceRequest{
			RolloutID: current.ID, Node: n.Node, To: PhaseBlocked,
			Blocker: fmt.Sprintf("this host negotiated node wire %d and the upgrade command "+
				"needs %d, so billet cannot tell it to move; upgrade it out of band with "+
				"`billet host-upgrade` on that machine, or record a decision with "+
				"`billet rollout exempt`", host.Wire, c.minWire),
		}); err != nil {
			return err
		}

		// COUNTED IMMEDIATELY, so the budget this pass checks reflects what this
		// pass has done. Counting only at the top of a tick let one pass block a
		// host and then disturb another against a budget already exhausted.
		budget.record()

		return nil
	}

	// THE PRIOR RELEASE IS RECORDED BEFORE THE HOST IS TOLD, because after this it
	// may never say it again: a host that installs the target and never comes back
	// leaves nothing to read it from, and a rollback needs somewhere to go.
	//
	// A SELF-TRANSITION, so it records the fact without claiming the host has moved.
	if err := c.store.Advance(ctx, AdvanceRequest{
		RolloutID: current.ID, Node: n.Node, To: PhasePending,
		PriorRelease: host.Release,
	}); err != nil {
		return fmt.Errorf("rollout: record what %s is running: %w", n.Node, err)
	}

	c.log.Info("telling a host to upgrade; it drains first, for as long as the work on it "+
		"takes", "node", n.Node, "from", host.Release, "to", current.TargetVersion,
		"rollout", current.ID)

	// TOLD FIRST, RECORDED SECOND, and the reverse order was a defect.
	//
	// Marking a host `draining` and then failing to reach it left it draining with
	// no updater running — and nothing retries a draining host, because a drain is
	// exactly the state that legitimately takes as long as it takes. The rollout
	// would have waited forever on a machine that never heard anything.
	//
	// This order has its own window, and it is the survivable one: a crash between
	// the dispatch and the record leaves a host pending while its updater runs, so
	// the coordinator tells it again — and the second updater is refused by the
	// claim on /var/lib/billet/upgrades/active, which exists for exactly that.
	if err := c.dispatch.Upgrade(ctx, n.Node, current.TargetVersion, current.TargetDigest,
		current.ID, current.Generation); err != nil {
		// A BACKOFF, NOT A BLOCK. A dispatch that failed says nothing about the host
		// except that billet could not reach it just then, and the attempt count is
		// durable so a host that keeps refusing becomes visible without a timer
		// deciding anything about it.
		if advanceErr := c.store.Advance(ctx, AdvanceRequest{
			RolloutID: current.ID, Node: n.Node, To: PhasePending,
			Backoff: retryAfter,
		}); advanceErr != nil {
			return errors.Join(err, advanceErr)
		}

		return fmt.Errorf("rollout: tell %s to upgrade: %w", n.Node, err)
	}

	*inFlight++

	// THE EPOCH THE INSTRUCTION WAS SENT AGAINST is recorded with the phase, and
	// it is the only thing that later distinguishes a host still draining from one
	// that rolled itself back onto the same release.
	if err := c.store.Advance(ctx, AdvanceRequest{
		RolloutID: current.ID, Node: n.Node, To: PhaseDraining,
		DispatchEpoch: host.Epoch,
	}); err != nil {
		return fmt.Errorf("rollout: mark %s draining: %w", n.Node, err)
	}

	return nil
}

// converge records that a host reached the target.
//
// THE WHOLE SEQUENCE IS WALKED, for the reason observeController walks it: the
// host's own transaction proved readiness before it committed, and its
// registration at the target is the evidence for every step. Recording them keeps
// the state machine the one authority on what may follow what.
func (c *Coordinator) converge(ctx context.Context, current *Rollout, n *Node,
	digest string,
) error {
	from := n.Phase

	// A ROLLED-BACK HOST THAT REPORTS THE TARGET WAS NOT ROLLED BACK, and letting
	// it correct itself is what keeps one wrong record from stalling a rollout.
	//
	// The rollback observation is inference from the strongest evidence available
	// and it has a known false positive: a host re-registering mid-drain for a
	// reason that is not the upgrade — the control plane restarted and forgot it,
	// say — looks exactly like one that came back on its old release. When that
	// host then reports the TARGET while billet is in contact with it, that is the
	// same evidence that authorises `committed` anywhere else, and it settles the
	// question the earlier inference could only guess at.
	//
	// `rolled_back` is a phase the table says a component may LEAVE, which is
	// exactly what separates it from `blocked` — blocked exists because billet
	// could not prove something, and only a person can supply what it could not.
	// So this recovers from one and never from the other.
	if from == PhaseRolledBack {
		if err := c.store.Advance(ctx, AdvanceRequest{
			RolloutID: current.ID, Node: n.Node, To: PhasePending,
		}); err != nil {
			return fmt.Errorf("rollout: return %s to the rollout after it reported the "+
				"target: %w", n.Node, err)
		}

		c.log.Info("a host recorded as rolled back reports the target, so the rollback was "+
			"a misreading of a restart; returning it to the rollout",
			"node", n.Node, "rollout", current.ID)

		from = PhasePending
	}

	// THE DIGEST TRAVELS WITH THE COMMIT. A host's CURRENT digest answers what it
	// is running now; this answers what this rollout accepted as evidence, and the
	// two stop agreeing the moment anything upgrades that host again. Without it a
	// completed rollout cannot say which of its hosts were proved against the
	// manifest it decided on and which were taken on their version alone.
	if err := c.walkTo(ctx, current.ID, n.Node, from, PhaseCommitted, digest); err != nil {
		return err
	}

	if digest == "" {
		c.log.Info("a host converged on its version alone; nothing on it could say which "+
			"release manifest produced it",
			"node", n.Node, "version", current.TargetVersion, "rollout", current.ID)

		return nil
	}

	c.log.Info("a host converged", "node", n.Node, "version", current.TargetVersion,
		"manifest", digest, "rollout", current.ID)

	return nil
}

// completeIfConverged closes the rollout once nothing is outstanding.
//
// ATTEMPTED RATHER THAN DECIDED HERE. `Finish` re-derives the answer inside its
// own transaction and refuses if anything is still live, so a coordinator that
// asked at the wrong moment is told no rather than closing a rollout that has
// work left. That is the same arrangement `billet local restore` uses: the plan
// is re-derived under the exclusion that acts on it.
func (c *Coordinator) completeIfConverged(ctx context.Context, current *Rollout) error {
	err := c.store.Finish(ctx, current.ID, StateCompleted,
		"every required component reports the target")
	if err == nil {
		c.log.Info("rollout complete", "rollout", current.ID,
			"version", current.TargetVersion)

		return nil
	}

	// EXACTLY ONE ERROR IS SUPPRESSED, AND IT IS THE ONE THAT SAYS SO.
	//
	// "Something is still outstanding" is the ordinary answer on every tick but the
	// last, and reporting it would fill an operator's log with a failure that is
	// the mechanism working. Suppressing every error instead was a defect a review
	// caught: a corrupted row, a cancelled context or a broken ledger all came back
	// as a successful pass, so a Finish that could never succeed was
	// indistinguishable from one that had simply not succeeded yet.
	if !errors.Is(err, ErrOutstanding) {
		return err
	}

	c.log.Debug("the rollout is not complete yet", "rollout", current.ID, "reason", err)

	return nil
}
