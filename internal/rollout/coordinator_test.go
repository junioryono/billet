package rollout

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// fakeFleet is what the ledger would say about the hosts.
type fakeFleet struct{ hosts []Host }

func (f *fakeFleet) Hosts(context.Context) ([]Host, error) { return f.hosts, nil }

// set is ONE REGISTRATION, and it bumps that host's epoch for the same reason a
// real one does: that number is the only thing separating a host that came back
// from a host that never left, so a fake where it stood still would let a
// coordinator that ignores it pass every test here.
//
// PER HOST, because the ledger's epoch is a column on the node's own row. A
// counter shared across the fleet makes one machine's registration move another
// machine's fence, which is a system this fake would be testing and billet is not.
//
// AND THE FIRST ONE IS 1, matching what RegisterNode writes. Zero is reserved for
// "no registration has touched this row", and a fake that started there would
// model a state the ledger cannot produce.
// digest makes a host name the manifest that produced it.
//
// IT MOVES THE HOST'S WIRE TOO, because a host below nodeapi.VersionNodeDigest
// cannot supply one — the control plane drops it at registration, since a pairing
// that has no such field cannot mean anything by a value in it. A fixture that
// left the wire behind would be encoding a state the real system refuses to
// produce, and the tests built on it would be proving something about a fleet
// that cannot exist.
func (f *fakeFleet) digest(name, digest string) {
	for i := range f.hosts {
		if f.hosts[i].Name == name {
			f.hosts[i].Digest = digest
			f.hosts[i].Wire = nodeapi.VersionNodeDigest

			return
		}
	}
}

func (f *fakeFleet) set(name, release string, wire int, live bool) {
	for i := range f.hosts {
		if f.hosts[i].Name == name {
			f.hosts[i] = Host{
				Name: name, Release: release, Wire: wire, Live: live,
				Epoch: f.hosts[i].Epoch + 1,
			}

			return
		}
	}

	f.hosts = append(f.hosts, Host{
		Name: name, Release: release, Wire: wire, Live: live, Epoch: 1,
	})
}

// reach changes only whether billet is in contact with a host. It is NOT a
// registration, so the epoch stays where it was — losing contact with a machine
// is something billet observes, not something the machine did.
func (f *fakeFleet) reach(name string, live bool) {
	for i := range f.hosts {
		if f.hosts[i].Name == name {
			f.hosts[i].Live = live

			return
		}
	}
}

// fakeDispatcher records who was told to upgrade.
type fakeDispatcher struct {
	told []string
	fail error
}

func (d *fakeDispatcher) Upgrade(_ context.Context, node, _, _, _ string, _ int64) error {
	if d.fail != nil {
		return d.fail
	}

	d.told = append(d.told, node)

	return nil
}

const targetVersion = "v0.4.0"

func coordinated(t *testing.T, nodes ...string) (*Store, *fakeFleet, *fakeDispatcher, *Coordinator, *Rollout) {
	t.Helper()

	_, s := open(t)

	fleet := &fakeFleet{}
	for _, n := range nodes {
		fleet.set(n, "v0.3.26", 14, true)
	}

	dispatch := &fakeDispatcher{}

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: nodes,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The coordinator runs in a control plane that is ALREADY the target, which is
	// the ordinary case: the controller was upgraded first and this is its
	// successor.
	c := NewCoordinator(s, fleet, dispatch, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	return s, fleet, dispatch, c, r
}

func phaseOf(t *testing.T, s *Store, r *Rollout) Phase {
	t.Helper()

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	// THE FIRST HOST, because every caller has exactly one it is asking about and
	// a name parameter that is always the same string is a parameter that reads
	// like a choice and is not.
	if len(nodes) == 0 {
		t.Fatalf("rollout %s covers no host", r.ID)
	}

	return nodes[0].Phase
}

func tick(t *testing.T, c *Coordinator) {
	t.Helper()

	if err := c.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// THE CONTROL PLANE CONVERGES FIRST, AND NO NODE MOVES UNTIL IT HAS.
//
// The wire bridge runs one way: an old server rejects a new node's registration
// in its strict decoder before any version check can run, so a node rolled ahead
// of its control plane is refused and stays refused. This is the gate that stops
// that.
func TestNoNodeIsToldToUpgradeBeforeTheControlPlaneIsOnTheTarget(t *testing.T) {
	s, fleet, dispatch, _, r := coordinated(t, "epyc-1")

	// A coordinator running in a control plane that is still on the OLD release.
	behind := NewCoordinator(s, fleet, dispatch, "v0.3.26", 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, behind)

	if len(dispatch.told) != 0 {
		t.Errorf("a node was told to upgrade while the control plane was still on the old "+
			"release: %v", dispatch.told)
	}

	if got := phaseOf(t, s, r); got != PhasePending {
		t.Errorf("the node is %s, want pending", got)
	}
}

// AND ONCE IT IS, THE CONTROLLER IS RECORDED AS CONVERGED and the nodes begin.
func TestTheControllerIsRecordedThenTheNodesAreTold(t *testing.T) {
	s, _, dispatch, c, r := coordinated(t, "epyc-1")

	tick(t, c)

	current, err := s.Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !current.ControllerPhase.Converged() {
		t.Fatalf("the controller is %s after a tick on the target version",
			current.ControllerPhase)
	}

	// THE NODES BEGIN ON THE NEXT PASS, not this one. The controller gate returns
	// as soon as it records, so the ordering is observable rather than incidental.
	if len(dispatch.told) != 0 {
		t.Errorf("a node was told on the same tick that recorded the controller: %v",
			dispatch.told)
	}

	tick(t, c)

	if len(dispatch.told) != 1 || dispatch.told[0] != "epyc-1" {
		t.Fatalf("the coordinator told %v, want [epyc-1]", dispatch.told)
	}

	if got := phaseOf(t, s, r); got != PhaseDraining {
		t.Errorf("a host that was told to upgrade is %s, want draining", got)
	}
}

// A HOST THAT COMES BACK AT THE TARGET IS CONVERGED, and that registration is the
// only evidence used. Its own transaction proved readiness before it committed.
func TestAHostThatComesBackAtTheTargetConverges(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	// The host upgrades and registers again reporting the target.
	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Errorf("a host reporting the target is %s, want committed", got)
	}

	// AND THE ROLLOUT CLOSES, because everything required reached the target.
	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("the rollout is still open after every host converged: %v", err)
	}
}

// ONE AT A TIME, BY DEFAULT. A fleet updated one host at a time never loses more
// capacity than one host's worth, and the cohort is what enforces that.
func TestTheCohortBoundsHowManyHostsAreDisturbedAtOnce(t *testing.T) {
	_, _, dispatch, c, _ := coordinated(t, "epyc-1", "epyc-2", "epyc-3")

	tick(t, c) // controller
	tick(t, c) // nodes

	if len(dispatch.told) != 1 {
		t.Errorf("the coordinator told %d hosts at once with a cohort of 1: %v",
			len(dispatch.told), dispatch.told)
	}

	// AND A SECOND PASS TELLS NOBODY NEW while the first is still draining.
	tick(t, c)

	if len(dispatch.told) != 1 {
		t.Errorf("the coordinator told another host while one was still draining: %v",
			dispatch.told)
	}
}

// A HOST BILLET CANNOT REACH IS LEFT ALONE. It is not gone: its compute may be
// running and it will come back speaking whatever it spoke before.
func TestAnUnreachableHostIsNotToldAndHoldsTheRolloutOpen(t *testing.T) {
	s, fleet, dispatch, c, r := coordinated(t, "epyc-1")

	fleet.set("epyc-1", "v0.3.26", 14, false)

	tick(t, c)
	tick(t, c)

	if len(dispatch.told) != 0 {
		t.Errorf("an unreachable host was told to upgrade: %v", dispatch.told)
	}

	if got := phaseOf(t, s, r); got != PhasePending {
		t.Errorf("an unreachable host is %s, want pending", got)
	}

	if _, err := s.Open(t.Context()); err != nil {
		t.Errorf("the rollout closed with an unreachable host outstanding: %v", err)
	}
}

// A HOST ON A WIRE WITHOUT THE UPGRADE COMMAND IS BLOCKED, ONCE.
//
// billet cannot tell it to move at all, so retrying forever would burn the
// cohort's only slot on a machine that can never accept it. Blocking says so and
// leaves an operator the choice.
func TestAHostOnAnOlderWireIsBlockedRatherThanRetriedForever(t *testing.T) {
	s, fleet, dispatch, c, r := coordinated(t, "epyc-1")

	fleet.set("epyc-1", "v0.3.26", 13, true)

	tick(t, c)
	tick(t, c)

	if len(dispatch.told) != 0 {
		t.Errorf("a host that cannot receive the command was told anyway: %v", dispatch.told)
	}

	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Fatalf("a host on an older wire is %s, want blocked", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	// AND THE BLOCKER NAMES BOTH WAYS OUT, because the operator's next move is one
	// of exactly two things.
	for _, want := range []string{"billet host-upgrade", "billet rollout exempt"} {
		if !strings.Contains(nodes[0].Blocker, want) {
			t.Errorf("the blocker does not name %q: %s", want, nodes[0].Blocker)
		}
	}
}

// A DISPATCH THAT FAILED SAYS NOTHING ABOUT THE HOST except that billet could not
// reach it just then, so it goes back to pending with a durable backoff rather
// than being blocked.
func TestAFailedDispatchBacksOffRatherThanBlocking(t *testing.T) {
	s, _, dispatch, c, r := coordinated(t, "epyc-1")

	dispatch.fail = errors.New("the node did not answer")

	tick(t, c) // controller

	if err := c.Tick(t.Context()); err == nil {
		t.Fatal("a failed dispatch was reported as a successful pass")
	}

	if got := phaseOf(t, s, r); got != PhasePending {
		t.Errorf("a host whose dispatch failed is %s, want pending", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if nodes[0].Attempts != 1 || nodes[0].NextAttemptAt == "" {
		t.Errorf("the failed attempt was not recorded durably: %+v", nodes[0])
	}
}

// AND IT IS NOT RETRIED BEFORE ITS BACKOFF, so a host that keeps refusing does
// not consume every pass.
func TestAHostInBackoffIsNotRetriedEarly(t *testing.T) {
	_, s := open(t)

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	dispatch := &fakeDispatcher{fail: errors.New("unreachable")}

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	store := New(s.db, WithClock(clock))
	c := NewCoordinator(store, fleet, dispatch, targetVersion, 14,
		WithCoordinatorClock(clock), WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller

	// EXPECTED TO FAIL, and asserted so. A discarded error here would keep this
	// test green against a build where the dispatch quietly succeeded, at which
	// point it would be measuring an ordinary pass rather than a backoff.
	if err := c.Tick(t.Context()); err == nil {
		t.Fatal("the injected dispatch failure was not reported")
	}

	dispatch.fail = nil

	// The clock has not moved, so the backoff is still in force.
	if err := c.Tick(t.Context()); err != nil {
		t.Fatalf("a pass that should have skipped a host in backoff failed: %v", err)
	}

	if len(dispatch.told) != 0 {
		t.Errorf("a host in backoff was retried early: %v", dispatch.told)
	}

	// Past the backoff, it is tried again.
	now = now.Add(retryAfter + time.Minute)

	tick(t, c)

	if len(dispatch.told) != 1 {
		t.Errorf("a host past its backoff was not retried: %v", dispatch.told)
	}
}

// NOTHING HAPPENS WITHOUT A ROLLOUT. The coordinator runs on every control plane,
// so an idle deployment must be an idle coordinator.
func TestTheCoordinatorDoesNothingWithoutARollout(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	dispatch := &fakeDispatcher{}

	c := NewCoordinator(s, fleet, dispatch, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c)

	if len(dispatch.told) != 0 {
		t.Errorf("the coordinator acted with no rollout open: %v", dispatch.told)
	}
}

// A HOST THAT CAME BACK ON ITS OLD RELEASE ROLLED BACK, AND SAYING SO IS WHAT
// KEEPS THE ROLLOUT MOVING.
//
// The two states are identical in every field but one. A host still working
// through an unbounded drain and a host that installed the target, failed and
// restored its previous release are both live and both report the old version —
// so the first implementation left the second in `draining` forever, holding the
// cohort's only slot, never counting against the failure budget, and quietly
// stopping the whole rollout with nothing anywhere saying why. The registration
// epoch is the only evidence available, because a registration bumps it and
// nothing else does.
func TestAHostThatComesBackOnItsOldReleaseIsRecordedAsRolledBack(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	if got := phaseOf(t, s, r); got != PhaseDraining {
		t.Fatalf("the host is %s after being told, want draining", got)
	}

	// STILL DRAINING IS NOT A ROLLBACK. Nothing has re-registered, so the
	// coordinator has no evidence and must not invent any.
	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseDraining {
		t.Fatalf("a host that has not re-registered is %s, want draining", got)
	}

	// It comes back on the release it started with.
	fleet.set("epyc-1", "v0.3.26", 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseRolledBack {
		t.Fatalf("a host that re-registered on its old release is %s, want rolled_back", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	// AND THE RECORD SAYS WHAT WAS OBSERVED, not what was assumed. An operator
	// reading this is deciding whether to retry the host or look at it.
	if !strings.Contains(nodes[0].RollbackResult, "v0.3.26") ||
		!strings.Contains(nodes[0].RollbackResult, targetVersion) {
		t.Errorf("the rollback record does not name both releases: %q", nodes[0].RollbackResult)
	}
}

// A HOST THAT REPORTS THE TARGET AND IS GONE IS NOT CONVERGED.
//
// Release is what a host said at its LAST registration, so a node that came up on
// the target, failed its stability check and disappeared reports the target
// forever. Reading convergence off that alone marked a dead machine as done — and
// if it was the last one outstanding, closed the rollout over an offline fleet.
func TestAHostThatReportedTheTargetAndWentAwayIsNotConverged(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.reach("epyc-1", false)

	tick(t, c)

	if got := phaseOf(t, s, r); got == PhaseCommitted {
		t.Error("a host billet cannot reach was recorded as converged on the strength of " +
			"what it said before it vanished")
	}

	if _, err := s.Open(t.Context()); err != nil {
		t.Errorf("the rollout closed while a host was unreachable: %v", err)
	}

	// AND IT CONVERGES WHEN IT COMES BACK, so the check is a wait rather than a
	// verdict.
	fleet.reach("epyc-1", true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Errorf("a reachable host on the target is %s, want committed", got)
	}
}

// A COMPONENT PART OF THE WAY THROUGH ITS WALK CARRIES ON FROM THERE.
//
// Each phase is its own transaction, so a restart or a storage failure partway
// leaves a component at, say, `installing`. Replaying the walk from the start
// then tried to move it BACK to `draining`, which the state machine correctly
// refuses — and that component was wedged on every pass from then on.
func TestAPartlyAdvancedHostResumesRatherThanReplaying(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	// Stand the host part of the way through, as an interrupted pass would leave it.
	for _, phase := range []Phase{PhaseReadyToInstall, PhaseInstalling} {
		if err := s.Advance(t.Context(), AdvanceRequest{
			RolloutID: r.ID, Node: "epyc-1", To: phase,
		}); err != nil {
			t.Fatalf("Advance to %s: %v", phase, err)
		}
	}

	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Errorf("a host resumed from installing is %s, want committed", got)
	}
}

// A ROLLOUT WHOSE LAST HOST WAS RESOLVED BY A PERSON STILL CLOSES.
//
// Completion used to be attempted only when a host had just converged, so an
// operator exempting or decommissioning the final outstanding machine left the
// rollout open forever — and an open rollout refuses the next one.
func TestARolloutClosesWhenAnOperatorResolvesTheLastHost(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1", "epyc-2")

	// The second host is one billet cannot reach, so nothing about it can converge
	// on its own and the rollout's last outstanding component is a decision rather
	// than an observation.
	fleet.reach("epyc-2", false)

	tick(t, c) // controller
	tick(t, c) // tell the reachable host

	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if _, err := s.Open(t.Context()); err != nil {
		t.Fatalf("the rollout closed with an unreachable host outstanding: %v", err)
	}

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-2", To: PhaseExempt,
		ExemptReason: "this host is being retired",
	}); err != nil {
		t.Fatalf("Advance to exempt: %v", err)
	}

	// NOTHING CONVERGES ON THIS PASS. The only thing that changed was a person's
	// decision, and the rollout must close on it anyway.
	tick(t, c)

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("the rollout is still open after an operator resolved the last host: %v", err)
	}
}

// THE FAILURE BUDGET IS NOT EXCEEDED WITHIN ONE PASS.
//
// It used to be counted once, before the loop, so a host blocked DURING a pass
// did not count — and with a budget of one and a cohort above one the coordinator
// blocked a host and then went on disturbing others against a tolerance the
// operator had already spent.
func TestTheFailureBudgetIsNotExceededWithinOnePass(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}

	names := []string{"epyc-1", "epyc-2", "epyc-3"}
	for _, n := range names {
		// Every one is on a wire without the upgrade command, so every one blocks.
		fleet.set(n, "v0.3.26", 13, true)
	}

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: names,
		Policy: Policy{Cohort: 3, FailureBudget: 1},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c := NewCoordinator(s, fleet, &fakeDispatcher{}, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller
	tick(t, c) // nodes

	current, err := s.Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	nodes, err := s.Nodes(t.Context(), current.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	blocked := 0

	for _, n := range nodes {
		if n.Phase == PhaseBlocked {
			blocked++
		}
	}

	if blocked != 1 {
		t.Errorf("%d hosts were blocked in one pass against a failure budget of 1", blocked)
	}
}

// A ROLLED-BACK HOST THAT LATER REPORTS THE TARGET CORRECTS ITSELF.
//
// The rollback observation is an inference, and its known false positive is a
// host that re-registered mid-drain for a reason that was not the upgrade — the
// control plane restarted and forgot it, say. Without this, one such misreading
// left the host in `rolled_back` and the rollout could never complete without an
// operator running `billet rollout retry`, even though the machine had converged.
//
// It recovers from `rolled_back` and NOT from `blocked`, which is the whole
// distinction between them: blocked exists because billet could not prove
// something, and only a person can supply what it could not.
func TestAHostRecordedAsRolledBackConvergesWhenItReportsTheTarget(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	// It re-registers on the old release, which reads as a rollback.
	fleet.set("epyc-1", "v0.3.26", 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseRolledBack {
		t.Fatalf("the host is %s, want rolled_back", got)
	}

	// It was draining all along, and finishes.
	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Errorf("a host that reported the target from rolled_back is %s, want committed", got)
	}

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("the rollout is still open after its only host converged: %v", err)
	}
}

// AND A BLOCKED HOST DOES NOT, because only a person may take it out of that
// phase. Reporting the target settles what the host is running and says nothing
// about whatever billet could not prove when it cordoned it.
func TestABlockedHostIsNotConvergedByReportingTheTarget(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	fleet.set("epyc-1", "v0.3.26", 13, true) // a wire without the upgrade command

	tick(t, c) // controller
	tick(t, c) // blocks it

	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Fatalf("the host is %s, want blocked", got)
	}

	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Errorf("a blocked host was advanced to %s without anybody deciding anything", got)
	}
}

// A SPENT BUDGET STOPS NEW WORK; IT DOES NOT STOP BILLET NOTICING.
//
// The budget bounds how many machines are DISTURBED. Letting it stop the whole
// pass meant a host that had already converged was never recorded as converged
// once the budget was reached — so a fleet where one host failed could never
// finish updating the rest that had already succeeded, and the rollout could
// never complete.
func TestASpentBudgetStillRecordsAHostThatConverged(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 13, true) // a wire without the upgrade command
	fleet.set("epyc-2", "v0.3.26", 14, true)

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: []string{"epyc-1", "epyc-2"},
		Policy: Policy{Cohort: 2, FailureBudget: 1},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c := NewCoordinator(s, fleet, &fakeDispatcher{}, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller
	tick(t, c) // epyc-1 blocks, spending the budget

	current, err := s.Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The second host was upgraded out of band and comes back on the target.
	fleet.set("epyc-2", targetVersion, 14, true)

	tick(t, c)

	nodes, err := s.Nodes(t.Context(), current.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	for _, n := range nodes {
		if n.Node == "epyc-2" && n.Phase != PhaseCommitted {
			t.Errorf("a host reporting the target is %s while the budget was spent, want "+
				"committed", n.Phase)
		}
	}
}

// A ROLLBACK RECORDED HALFWAY IS FINISHED ON THE NEXT PASS.
//
// `rolling_back` and `rolled_back` are two transactions, so a transient ledger
// failure between them leaves a node in `rolling_back` — and the state machine
// allows only `rolled_back` or `blocked` out of that phase, neither of which any
// observation writes. The node was stuck there on every pass from then on,
// holding the cohort's only slot and keeping the rollout open forever.
func TestAHalfRecordedRollbackIsFinishedRatherThanLeft(t *testing.T) {
	s, _, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	// Stand the node where an interrupted rollback record would leave it.
	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseRollingBack,
	}); err != nil {
		t.Fatalf("Advance to rolling_back: %v", err)
	}

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseRolledBack {
		t.Errorf("a half-recorded rollback is %s, want rolled_back", got)
	}
}

// AND THE BUDGET IS SPENT ONCE, not once per pass that touches the rollback.
//
// The observation and its completion are separate transactions; counting in both
// would let one failed host exhaust a budget of two.
func TestARollbackSpendsTheFailureBudgetOnce(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)
	fleet.set("epyc-2", "v0.3.26", 14, true)

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: []string{"epyc-1", "epyc-2"},
		Policy: Policy{Cohort: 1, FailureBudget: 2},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dispatch := &fakeDispatcher{}

	c := NewCoordinator(s, fleet, dispatch, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller
	tick(t, c) // tell epyc-1

	fleet.set("epyc-1", "v0.3.26", 14, true) // it comes back on the old release

	tick(t, c) // records the rollback, freeing the cohort
	tick(t, c) // and the second host is told

	if len(dispatch.told) != 2 {
		t.Errorf("the coordinator told %v; a rollback that spent the budget twice would "+
			"have stopped before the second host", dispatch.told)
	}
}

// countingHandler counts the warnings a coordinator emits.
type countingHandler struct {
	warnings int
	match    string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn && strings.Contains(r.Message, h.match) {
		h.warnings++
	}

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// A ROLLOUT WAITING ON A HOST IT CANNOT REACH SAYS SO, AND KEEPS SAYING SO.
//
// A host told to upgrade that then goes out of contact holds its cohort slot
// indefinitely and nothing times it out — deliberately, because its compute may
// still be running. The only thing between that and a fleet that looks wedged is
// billet saying which host it is waiting on and what resolves it. Said once every
// stalledEvery rather than on every 30-second tick, because a log nobody reads is
// the same as no log.
func TestARolloutSaysWhichHostItIsWaitingOn(t *testing.T) {
	_, s := open(t)

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := &countingHandler{match: "cannot reach"}

	c := NewCoordinator(New(s.db, WithClock(clock)), fleet, &fakeDispatcher{}, targetVersion, 14,
		WithCoordinatorClock(clock), WithCoordinatorLogger(slog.New(handler)))

	tick(t, c) // controller
	tick(t, c) // tell the host

	// It goes out of contact while draining, holding the cohort.
	fleet.reach("epyc-1", false)

	tick(t, c)

	if handler.warnings != 1 {
		t.Fatalf("the rollout warned %d times about a host it is waiting on, want 1",
			handler.warnings)
	}

	// NOT ON EVERY TICK. The interval is 30 seconds; the warning is not.
	now = now.Add(time.Minute)

	tick(t, c)

	if handler.warnings != 1 {
		t.Errorf("the rollout warned %d times within one interval, want 1", handler.warnings)
	}

	// AND AGAIN ONCE THE INTERVAL HAS PASSED, because whoever needs to read it is
	// usually not the person who was watching when it started.
	now = now.Add(stalledEvery)

	tick(t, c)

	if handler.warnings != 2 {
		t.Errorf("the rollout warned %d times after the interval passed, want 2",
			handler.warnings)
	}
}

// AND IT DOES NOT WARN ABOUT A HOST NOTHING HAS BEEN DONE TO. An untouched host
// billet cannot reach is the ordinary case `billet rollout status` already shows;
// warning about it would bury the one that is actually holding things up.
func TestARolloutIsQuietAboutAnUntouchedHostItCannotReach(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, false)

	if _, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	handler := &countingHandler{match: "cannot reach"}

	c := NewCoordinator(s, fleet, &fakeDispatcher{}, targetVersion, 14,
		WithCoordinatorLogger(slog.New(handler)))

	tick(t, c)
	tick(t, c)

	if handler.warnings != 0 {
		t.Errorf("the rollout warned about a host it has not acted on: %d", handler.warnings)
	}
}

// A HOST DISPATCHED ON ITS VERY FIRST REGISTRATION IS STILL WATCHED.
//
// This is the ORDINARY sequence — a node registers, an operator starts a rollout,
// the host is told — and it was the one the mechanism could not see. The ledger
// took the epoch column's default, so a first registration was epoch zero, which
// the coordinator reads as "nothing was recorded" and refuses to conclude
// anything from. Every such host would have held its cohort slot forever.
func TestAHostDispatchedOnItsFirstRegistrationCanStillRollBack(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	if fleet.hosts[0].Epoch == 0 {
		t.Fatal("the fake models a first registration as epoch zero, which the ledger " +
			"no longer produces; this test would prove nothing")
	}

	tick(t, c) // controller
	tick(t, c) // tell the node, at its FIRST epoch

	fleet.set("epyc-1", "v0.3.26", 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseRolledBack {
		t.Errorf("a host dispatched at its first epoch is %s after coming back on its old "+
			"release, want rolled_back", got)
	}
}

// A HALF-RECORDED ROLLBACK ON A HOST THAT REPORTS THE TARGET STILL RESOLVES.
//
// The two acknowledged imperfections combine here: the rollback record is written
// in two transactions, and the rollback inference has a known false positive. A
// host left in `rolling_back` that then reports the target went to `converge`,
// which cannot resume from a phase that is not on the walk — so every later pass
// repeated the same no-op and the rollout never closed. Reconciling
// `rolling_back` before the target check is what makes it resolvable.
func TestAHalfRecordedRollbackOnAConvergedHostStillResolves(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhaseRollingBack,
	}); err != nil {
		t.Fatalf("Advance to rolling_back: %v", err)
	}

	// And the host was fine all along.
	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c) // finishes the rollback record
	tick(t, c) // and the target registration corrects it

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Errorf("a host that reported the target from a half-recorded rollback is %s, "+
			"want committed", got)
	}

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("the rollout is still open after its only host converged: %v", err)
	}
}

// THE FAILURE BUDGET DOES NOT DEPEND ON WHAT THE HOSTS ARE CALLED.
//
// Observations and dispatches used to be interleaved in node-name order, so with
// a cohort of two and a budget of one an alphabetically earlier PENDING host was
// dispatched before a later DRAINING host's rollback — already present in the
// same snapshot — had been looked at. billet held the evidence that its tolerance
// was spent and disturbed another machine anyway. Settling every disturbed host
// before starting on any new one is what removes the ordering from the answer.
func TestTheFailureBudgetDoesNotDependOnNodeOrder(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("aaa", "v0.3.26", 14, true)
	fleet.set("zzz", "v0.3.26", 14, true)

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: []string{"aaa", "zzz"},
		Policy: Policy{Cohort: 2, FailureBudget: 1},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// zzz sorts LAST and is the one already disturbed: told to upgrade at its
	// current epoch, and now back on its old release with a newer one.
	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "zzz", To: PhaseDraining, DispatchEpoch: 1,
	}); err != nil {
		t.Fatalf("Advance zzz to draining: %v", err)
	}

	fleet.set("zzz", "v0.3.26", 14, true) // epoch 2, still the old release

	dispatch := &fakeDispatcher{}

	c := NewCoordinator(s, fleet, dispatch, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller
	tick(t, c) // the pass under test

	if len(dispatch.told) != 0 {
		t.Errorf("the coordinator disturbed %v in a pass where the evidence that its "+
			"failure budget was spent was already in the snapshot", dispatch.told)
	}
}

// A HOST THAT NAMES THE MANIFEST THIS ROLLOUT DECIDED ON IS PROVED, AND THE
// PROOF IS RECORDED.
//
// The version a host reports is the name its binary was BUILT with; two builds
// can share one, and a moved tag makes them identical. The digest is the bytes.
// Recording which one converged the host is what lets a completed rollout say
// whether its fleet was proved against the decision or taken on a name.
func TestAHostThatNamesTheDecidedManifestIsRecordedAsProved(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", targetDigest)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Fatalf("a host naming the decided manifest is %s, want committed", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if nodes[0].ConvergedDigest != targetDigest {
		t.Errorf("the rollout recorded %q as what proved this host, want %q",
			nodes[0].ConvergedDigest, targetDigest)
	}
}

// A HOST THAT NAMES NOTHING STILL CONVERGES, AND THE ROLLOUT SAYS SO.
//
// THIS IS THE CASE THAT MAKES THE WHOLE THING SHIPPABLE. Every host in the field
// on the day the field exists names no manifest — including the ones that would
// deliver the build able to name one — so refusing here would be a rollout that
// can never complete. A weaker answer honestly recorded beats a mechanism nobody
// can adopt.
func TestAHostThatNamesNoManifestConvergesAndIsRecordedAsUnproved(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	// It comes back on the target and says nothing about which bytes it installed.
	fleet.set("epyc-1", targetVersion, 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Fatalf("a host that named no manifest is %s, want committed", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if nodes[0].ConvergedDigest != "" {
		t.Errorf("the rollout recorded %q as proof from a host that named nothing",
			nodes[0].ConvergedDigest)
	}

	if _, err := s.Open(t.Context()); !errors.Is(err, ErrNoRollout) {
		t.Errorf("the rollout did not complete: %v", err)
	}
}

// A HOST RUNNING THE RIGHT VERSION FROM THE WRONG BYTES IS BLOCKED.
//
// This is the fact that could not previously exist. Before the digest there was
// nothing to disagree with, so a host upgraded out of band — or rebuilt under the
// same name from a moved tag — converged a rollout on evidence weaker than the
// decision it was converging. It is billet failing to prove something rather than
// a host that failed, which is what `blocked` means and why only a person leaves
// it.
func TestAHostRunningTheRightVersionFromTheWrongBytesIsBlocked(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	other := strings.Repeat("f", 64)

	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", other)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Fatalf("a host on the target version from a different manifest is %s, want "+
			"blocked", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	// THE BLOCKER NAMES BOTH MANIFESTS AND EVERY STEP OF THE REPAIR, because an
	// operator reading it has to decide whether their fleet or their channel is
	// wrong — and because the reinstall alone leaves the rollout exactly where it
	// was. `blocked` is a phase only a person leaves, so an instruction that fixes
	// the machine and omits `rollout retry` is one somebody follows and then
	// reports as not working.
	for _, want := range []string{other[:12], targetDigest[:12],
		"billet host-upgrade", "--reinstall", "billet rollout retry",
		"billet rollout exempt"} {
		if !strings.Contains(nodes[0].Blocker, want) {
			t.Errorf("the blocker does not carry %q: %s", want, nodes[0].Blocker)
		}
	}

	// AND NOTHING RECORDED IT AS PROVED. A blocked host has converged on nothing.
	if nodes[0].ConvergedDigest != "" {
		t.Errorf("a blocked host recorded %q as proof", nodes[0].ConvergedDigest)
	}
}

// A MIXED FLEET SETTLES EACH HOST ON ITS OWN EVIDENCE.
//
// The three answers are not alternatives to choose between — a real fleet mid-
// adoption has all of them at once, and the rollout has to hold them apart
// without letting the weakest one decide the whole thing.
func TestAMixedFleetSettlesEachHostOnItsOwnEvidence(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}

	names := []string{"proved", "silent", "wrong"}
	for _, n := range names {
		fleet.set(n, "v0.3.26", 14, true)
	}

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", CreatedBy: "ops", Nodes: names,
		Policy: Policy{Cohort: 3, FailureBudget: 0},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	c := NewCoordinator(s, fleet, &fakeDispatcher{}, targetVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // controller
	tick(t, c) // tell them

	fleet.set("proved", targetVersion, 14, true)
	fleet.digest("proved", targetDigest)
	fleet.set("silent", targetVersion, 14, true)
	fleet.set("wrong", targetVersion, 14, true)
	fleet.digest("wrong", strings.Repeat("e", 64))

	tick(t, c)

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	want := map[string]struct {
		phase  Phase
		digest string
	}{
		"proved": {PhaseCommitted, targetDigest},
		"silent": {PhaseCommitted, ""},
		"wrong":  {PhaseBlocked, ""},
	}

	for i := range nodes {
		expect, ok := want[nodes[i].Node]
		if !ok {
			t.Errorf("unexpected host %q", nodes[i].Node)

			continue
		}

		if nodes[i].Phase != expect.phase {
			t.Errorf("%s is %s, want %s", nodes[i].Node, nodes[i].Phase, expect.phase)
		}

		if nodes[i].ConvergedDigest != expect.digest {
			t.Errorf("%s recorded %q as proof, want %q",
				nodes[i].Node, nodes[i].ConvergedDigest, expect.digest)
		}
	}
}

// A BLOCKED HOST CONVERGES ONLY AFTER SOMETHING RETURNS IT TO THE ROLLOUT.
//
// This is the COORDINATOR's half: a host blocked for running the right version
// from the wrong bytes does not leave `blocked` on its own, however correct it
// becomes — the walk refuses to advance a phase only a person may leave — and it
// converges on the pass after it is returned to pending.
//
// WHAT RETURNS IT IS `billet rollout retry`, and that is a different half, tested
// where it lives: this drives Advance directly rather than the command, so it
// asserts the phase machine rather than claiming to join two commands it never
// runs. See TestRolloutRetryReturnsABlockedHostToTheRollout in cmd/billet, which
// runs the command against a ledger and reads back the phase it wrote.
func TestABlockedHostConvergesOnceSomethingReturnsItToTheRollout(t *testing.T) {
	s, fleet, _, c, r := coordinated(t, "epyc-1")

	tick(t, c) // controller
	tick(t, c) // tell the node

	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", strings.Repeat("f", 64))

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Fatalf("the host is %s, want blocked", got)
	}

	// The operator reinstalls, so the host now names the decided manifest.
	fleet.set("epyc-1", targetVersion, 14, true)
	fleet.digest("epyc-1", targetDigest)

	tick(t, c)

	// STILL BLOCKED, AND THAT IS CORRECT. Nothing automatic leaves this phase:
	// billet could not prove something, and only a person can supply what it could
	// not.
	if got := phaseOf(t, s, r); got != PhaseBlocked {
		t.Fatalf("a repaired host left `blocked` on its own; it is %s", got)
	}

	// The second half of the instruction.
	if err := s.Advance(t.Context(), AdvanceRequest{
		RolloutID: r.ID, Node: "epyc-1", To: PhasePending,
	}); err != nil {
		t.Fatalf("returning the host to the rollout: %v", err)
	}

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Fatalf("a repaired and retried host is %s, want committed", got)
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if nodes[0].ConvergedDigest != targetDigest {
		t.Errorf("it converged on %q, want the decided manifest %q",
			nodes[0].ConvergedDigest, targetDigest)
	}
}

type fakeSelfUpgrader struct {
	specs []nodeapi.UpgradeSpec
	fail  error
}

func (u *fakeSelfUpgrader) StartUpgrade(_ context.Context, spec nodeapi.UpgradeSpec) error {
	u.specs = append(u.specs, spec)

	return u.fail
}

// A CONTROL PLANE THAT IS NOT THE TARGET STARTS ITS OWN HOST'S UPGRADE, once per
// decision, with the whole instruction a node would get, and tells no node
// anything until its successor observes the result. Without a self-upgrader it
// only waits, which is what an operator-driven deployment asked for.
func TestTheControllerUpgradesItsOwnHostWhenGivenASelfUpgrader(t *testing.T) {
	t.Parallel()

	_, s := open(t)
	fleet := &fakeFleet{}
	fleet.set("n1", "v0.3.26", 14, true)
	dispatch := &fakeDispatcher{}
	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"n1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	self := &fakeSelfUpgrader{}
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	c := NewCoordinator(s, fleet, dispatch, "v0.3.26", 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)),
		WithSelfUpgrader(self),
		WithCoordinatorClock(func() time.Time { return now }))

	tick(t, c)
	tick(t, c)

	if len(self.specs) != 1 {
		t.Fatalf("the self-upgrader was asked %d times across two ticks, want once", len(self.specs))
	}
	want := nodeapi.UpgradeSpec{Version: targetVersion, ManifestSHA256: targetDigest, RolloutID: r.ID, Generation: r.Generation}
	if self.specs[0] != want {
		t.Errorf("asked with %+v, want %+v", self.specs[0], want)
	}
	if len(dispatch.told) != 0 {
		t.Errorf("a node was told to upgrade before the control plane was on the target: %v", dispatch.told)
	}

	// A refusal is retried after the backoff, not before.
	self.fail = errors.New("the updater refused: another upgrade holds this host")
	now = now.Add(selfRetryAfter)
	tick(t, c)
	if len(self.specs) != 2 {
		t.Fatalf("after the backoff the self-upgrader was asked %d times in total, want 2", len(self.specs))
	}
	now = now.Add(time.Minute)
	tick(t, c)
	if len(self.specs) != 2 {
		t.Fatalf("a refused updater was asked again a minute later, %d times in total", len(self.specs))
	}

	// AND WITHOUT ONE, ONLY WAITING.
	waiting := NewCoordinator(s, fleet, dispatch, "v0.3.26", 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))
	tick(t, waiting)
	if len(dispatch.told) != 0 {
		t.Errorf("a coordinator with no self-upgrader told a node: %v", dispatch.told)
	}
}
