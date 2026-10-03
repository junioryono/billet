package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// inspectingRegistry is a registry that also answers InspectRunner from a script
// and records every id WithdrawRunner is asked to delete.
type inspectingRegistry struct {
	fakeRunnerRegistry

	answers  []RunnerState
	errs     []error
	asked    int
	askedFor []string
	// during runs inside an inspection, as a slow lookup would.
	during func()
	// byName, when set, answers each runner from its own queue instead of answers.
	byName map[string][]RunnerState
	// githubIDs is the id GitHub holds for each name, filled into a present
	// answer that scripts none.
	githubIDs map[string]int64

	withdrawn   []int64
	withdrawErr error
}

func (r *inspectingRegistry) InspectRunner(_ context.Context, name string, _ int64) (RunnerState, error) {
	state, err := r.script(name)
	if id, known := r.githubIDs[name]; err == nil && state.Present && state.ID == 0 && known {
		state.ID = id
	}

	return state, err
}

func (r *inspectingRegistry) script(name string) (RunnerState, error) {
	i := r.asked
	r.asked++
	r.askedFor = append(r.askedFor, name)

	if r.during != nil {
		r.during()
	}

	if r.byName != nil {
		queue := r.byName[name]
		if len(queue) == 0 {
			return RunnerState{}, errors.New("unscripted inspection")
		}

		r.byName[name] = queue[1:]

		return queue[0], nil
	}

	if i < len(r.errs) && r.errs[i] != nil {
		return RunnerState{}, r.errs[i]
	}

	if i < len(r.answers) {
		return r.answers[i], nil
	}

	return RunnerState{}, errors.New("unscripted inspection")
}

func (r *inspectingRegistry) WithdrawRunner(_ context.Context, id int64) error {
	r.withdrawn = append(r.withdrawn, id)

	return r.withdrawErr
}

var offlineEpoch = time.Date(2026, 9, 30, 8, 52, 0, 0, time.UTC)

type offlineFixture struct {
	a         *alloc.Allocator
	l         *Listener
	reg       *inspectingRegistry
	lease     *alloc.Lease
	name      string
	names     []string
	desired   int
	now       time.Time
	destroyed int
	// requests is every launch request the runner was told to destroy.
	requests []int64
}

// newOfflineFixture registers one idle pool member the way a launch journals it:
// a name and no runner id, which the pool learns only from a JobStarted this
// member never receives. GitHub holds id 10899 for the name.
func newOfflineFixture(t *testing.T, reg *inspectingRegistry) *offlineFixture {
	t.Helper()

	return newOfflineFixtureOf(t, reg, 1)
}

// newOfflineFixtureOf registers n such members, and sets the frozen assigned
// count to n so none of them is surplus.
func newOfflineFixtureOf(t *testing.T, reg *inspectingRegistry, n int) *offlineFixture {
	t.Helper()

	return newOfflineFixtureWith(t, reg, n, 0)
}

// newOfflineFixtureWith is newOfflineFixtureOf with a runner id journaled for
// each member, poolID+i, or none when poolID is zero.
func newOfflineFixtureWith(t *testing.T, reg *inspectingRegistry, n int, poolID int64) *offlineFixture {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	f := &offlineFixture{reg: reg, now: offlineEpoch, desired: n}
	f.a = newAllocator(t, alloc.Limits{MaxVCPU: 64, MaxMemory: 512 * config.GiB}, tiers)

	if reg.githubIDs == nil {
		reg.githubIDs = map[string]int64{}
	}

	for i := range n {
		lease := poolLeaseForTier(t, f.a, tiers[0].Label)
		request := int64(-53128 - i)

		if err := f.a.Assign(t.Context(), lease.ID, lease.Epoch, 0, request); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		var journaled int64
		if poolID != 0 {
			journaled = poolID + int64(i)
		}

		name := provider.InstanceName(lease.ID)
		if err := f.a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID,
			Tier: tiers[0].Label, LaunchRequestID: request, RunnerID: journaled,
			RunnerName: name}); err != nil {
			t.Fatalf("RegisterPoolRunner: %v", err)
		}

		if _, set := reg.githubIDs[name]; !set {
			reg.githubIDs[name] = int64(10899 + i)
		}

		if i == 0 {
			f.lease, f.name = lease, name
		}

		f.names = append(f.names, name)
	}

	f.l = NewListener(f.a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onDestroy: func(request int64) error {
			f.destroyed++
			f.requests = append(f.requests, request)

			return nil
		},
	}), WithRunnerRegistry(reg))
	f.l.now = func() time.Time { return f.now }

	return f
}

// reconcileAt runs the pool reconciliation GitHub's frozen statistics would ask
// for at the given offset.
func (f *offlineFixture) reconcileAt(t *testing.T, after time.Duration) {
	t.Helper()

	f.now = offlineEpoch.Add(after)
	if err := f.l.reconcilePool(t.Context(), f.desired); err != nil {
		t.Fatalf("reconcilePool at +%s: %v", after, err)
	}
}

func (f *offlineFixture) status(t *testing.T) string {
	t.Helper()

	member, err := f.a.PoolRunnerByLease(t.Context(), f.lease.ID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return "forgotten"
	}
	if err != nil {
		t.Fatalf("PoolRunnerByLease: %v", err)
	}

	return member.Status
}

func TestAMemberGitHubReportsOfflineTwiceIsWithdrawnThenRetired(t *testing.T) {
	offline := RunnerState{Present: true}
	online := RunnerState{Present: true, Online: true}
	// Two members; the second is online throughout and must be untouched.
	reg := &inspectingRegistry{}
	f := newOfflineFixtureOf(t, reg, 2)
	reg.byName = map[string][]RunnerState{
		f.names[0]: {offline, offline},
		f.names[1]: {online, online},
	}

	// The members take turns, whichever goes first, so each has two answers by
	// +22m and the offline one's are at least offlineGrace apart.
	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery)
	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery+offlineGrace)
	f.reconcileAt(t, offlineIdleAfter+2*offlineInspectEvery+offlineGrace)

	if f.reg.asked != 4 {
		t.Fatalf("GitHub was asked %d times, want 4", f.reg.asked)
	}
	if !slices.Equal(f.reg.withdrawn, []int64{10899}) {
		t.Fatalf("withdrawn ids = %v, want exactly the inspected 10899", f.reg.withdrawn)
	}
	if !slices.Equal(f.requests, []int64{-53128}) {
		t.Fatalf("destroyed launch requests = %v, want only the withdrawn member's -53128", f.requests)
	}
	if got := f.status(t); got == alloc.PoolRunnerIdle {
		t.Fatalf("member still idle after GitHub deleted its runner")
	}
}

func TestAMemberIsKeptUnlessEveryAnswerSaysOfflineAndIdle(t *testing.T) {
	offline := RunnerState{Present: true}
	for _, tc := range []struct {
		name   string
		second RunnerState
		err    error
	}{
		{name: "online and idle", second: RunnerState{Present: true, Online: true}},
		{name: "busy", second: RunnerState{Present: true, Online: true, Busy: true}},
		{name: "absent", second: RunnerState{}},
		{name: "could not tell", err: errors.New("github: 502")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &inspectingRegistry{answers: []RunnerState{offline, tc.second}, errs: []error{nil, tc.err}}
			f := newOfflineFixture(t, reg)

			f.reconcileAt(t, 0)
			f.reconcileAt(t, offlineIdleAfter)
			f.reconcileAt(t, offlineIdleAfter+offlineGrace)

			if reg.asked != 2 {
				t.Fatalf("GitHub was asked %d times, want 2", reg.asked)
			}
			if len(reg.withdrawn) != 0 || len(reg.names) != 0 || f.destroyed != 0 {
				t.Fatalf("acted on %s: withdrawn %v, removed %v, destroyed %d",
					tc.name, reg.withdrawn, reg.names, f.destroyed)
			}
			if got := f.status(t); got != alloc.PoolRunnerIdle {
				t.Fatalf("member status = %q, want idle", got)
			}
		})
	}
}

// An answer that is not "offline" between two that are restarts the grace: the
// withdrawal needs two consecutive offline observations, not any two.
func TestAnInterruptedOfflineRunStartsTheGraceAgain(t *testing.T) {
	offline := RunnerState{Present: true}
	online := RunnerState{Present: true, Online: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, online, offline, offline, offline}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)
	f.reconcileAt(t, offlineIdleAfter+2*offlineGrace)
	f.reconcileAt(t, offlineIdleAfter+2*offlineGrace+offlineInspectEvery)

	if len(reg.withdrawn) != 0 {
		t.Fatalf("withdrawn inside the grace restarted by an online answer: %v", reg.withdrawn)
	}

	f.reconcileAt(t, offlineIdleAfter+3*offlineGrace)

	if !slices.Equal(reg.withdrawn, []int64{10899}) {
		t.Fatalf("not withdrawn after two fresh offline answers %s apart: %v", offlineGrace, reg.withdrawn)
	}
}

func TestTwoOfflineAnswersInsideTheGraceDoNotWithdraw(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline, offline}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery)

	if reg.asked != 2 || len(reg.withdrawn) != 0 || f.destroyed != 0 {
		t.Fatalf("after two offline answers %s apart: asked %d, withdrawn %v, destroyed %d",
			offlineInspectEvery, reg.asked, reg.withdrawn, f.destroyed)
	}

	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if !slices.Equal(reg.withdrawn, []int64{10899}) || f.destroyed != 1 {
		t.Fatalf("not retired once the grace had passed: withdrawn %v, destroyed %d", reg.withdrawn, f.destroyed)
	}
}

// The grace runs from when GitHub's first answer ARRIVED: a lookup that took
// most of the grace to return leaves the next one well inside it.
func TestTheGraceRunsFromWhenTheFirstAnswerArrived(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)

	reg.during = func() { f.now = f.now.Add(offlineGrace - time.Second) }
	f.reconcileAt(t, offlineIdleAfter)
	reg.during = nil

	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if reg.asked != 2 {
		t.Fatalf("GitHub was asked %d times, want 2", reg.asked)
	}
	if len(reg.withdrawn) != 0 || f.destroyed != 0 {
		t.Fatalf("withdrawn one second after the first answer arrived: %v, destroyed %d",
			reg.withdrawn, f.destroyed)
	}
}

// GitHub refusing the delete (a job still running on it, or no such runner)
// keeps the member idle and unjournaled, so the JobStarted that explains the
// refusal still binds.
func TestARefusedWithdrawalKeepsTheMemberForItsJob(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline}}
	reg.withdrawErr = errors.New("job still running")
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if len(reg.withdrawn) != 1 {
		t.Fatalf("withdrawal attempted %d times, want 1", len(reg.withdrawn))
	}
	if len(reg.names) != 0 || f.destroyed != 0 {
		t.Fatalf("acted after GitHub refused the delete: removed %v, destroyed %d", reg.names, f.destroyed)
	}
	if got := f.status(t); got != alloc.PoolRunnerIdle {
		t.Fatalf("member status = %q after a refused delete, want idle", got)
	}

	if _, err := f.a.StartPoolRunner(t.Context(), f.lease.ID, "billet-4vcpu-a", 10899, f.name,
		-53200, 777, "job-777", alloc.JobIdentity{}); err != nil {
		t.Fatalf("the job GitHub said was running could not bind: %v", err)
	}
}

// A ledger that fails after GitHub accepted the delete is retried on the next
// reconcile without asking GitHub again, whose answer now could only be absent.
func TestALedgerFailureAfterTheWithdrawalIsRetriedWithoutAskingAgain(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline}}
	f := newOfflineFixture(t, reg)

	failures := 1
	f.l.claimRetirement = func(ctx context.Context, leaseID string) error {
		if failures > 0 {
			failures--
			return errors.New("database is locked")
		}

		return f.a.RetirePoolRunner(ctx, leaseID)
	}

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if f.destroyed != 0 || f.status(t) != alloc.PoolRunnerIdle {
		t.Fatalf("destroyed %d with the claim failed; status %q", f.destroyed, f.status(t))
	}

	f.reconcileAt(t, offlineIdleAfter+offlineGrace+time.Second)

	if reg.asked != 2 {
		t.Fatalf("GitHub was asked %d times, want no inspection after the withdrawal", reg.asked)
	}
	if len(reg.withdrawn) != 1 {
		t.Fatalf("withdrawn %d times, want 1", len(reg.withdrawn))
	}
	if f.destroyed != 1 {
		t.Fatalf("compute destroyed %d times after the ledger recovered, want 1", f.destroyed)
	}
	if !slices.Equal(reg.ids, []int64{10899}) {
		t.Fatalf("the retried cleanup removed ids %v, want the withdrawn 10899, not the row's none", reg.ids)
	}
}

func TestAMemberIdleForLessThanTheBoundIsNotAskedAbout(t *testing.T) {
	reg := &inspectingRegistry{}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter-time.Second)

	if reg.asked != 0 {
		t.Fatalf("GitHub was asked about a member idle for under %s", offlineIdleAfter)
	}
}

// One lookup per interval for the whole tier, not per member, and the members
// take turns.
func TestAListenerAsksAboutOneMemberPerIntervalInTurn(t *testing.T) {
	online := RunnerState{Present: true, Online: true}
	reg := &inspectingRegistry{answers: []RunnerState{online, online, online, online}}
	f := newOfflineFixtureOf(t, reg, 3)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+time.Second)
	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery-time.Second)

	if reg.asked != 1 {
		t.Fatalf("GitHub was asked %d times inside one %s interval across 3 members, want 1",
			reg.asked, offlineInspectEvery)
	}

	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery)
	f.reconcileAt(t, offlineIdleAfter+2*offlineInspectEvery)

	asked := slices.Clone(reg.askedFor)
	slices.Sort(asked)

	want := slices.Clone(f.names)
	slices.Sort(want)

	if !slices.Equal(asked, want) {
		t.Fatalf("three intervals asked about %v, want each of %v once", reg.askedFor, f.names)
	}
}

// The shape that held a node's drain for nine hours (#317): a member journaled at
// launch with no runner id is withdrawn by the id GitHub holds for its name, and
// only that id.
func TestAMemberJournaledWithoutAnIDIsWithdrawnByGitHubsID(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline}}
	f := newOfflineFixture(t, reg)

	member, err := f.a.PoolRunnerByLease(t.Context(), f.lease.ID)
	if err != nil {
		t.Fatalf("PoolRunnerByLease: %v", err)
	}
	if member.RunnerID != 0 {
		t.Fatalf("fixture journaled runner id %d, want none, as a launch journals it", member.RunnerID)
	}

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if !slices.Equal(reg.withdrawn, []int64{10899}) {
		t.Fatalf("withdrawn ids = %v, want GitHub's 10899 for %s", reg.withdrawn, f.name)
	}
	// The cleanup's removal expects the withdrawn id, so a registration that took
	// the name since is refused rather than deleted by name alone.
	if !slices.Equal(reg.ids, []int64{10899}) || !slices.Equal(reg.names, []string{f.name}) {
		t.Fatalf("cleanup removed %v / %v, want the withdrawn 10899 under %s", reg.ids, reg.names, f.name)
	}
	if !slices.Equal(f.requests, []int64{-53128}) {
		t.Fatalf("destroyed launch requests = %v, want -53128", f.requests)
	}
}

// Two offline answers naming different ids are two registrations, not one seen
// twice, so the grace starts again from the second.
func TestOfflineAnswersNamingDifferentIDsStartTheGraceAgain(t *testing.T) {
	reg := &inspectingRegistry{answers: []RunnerState{
		{Present: true, ID: 10899},
		{Present: true, ID: 20411},
		{Present: true, ID: 20411},
	}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if len(reg.withdrawn) != 0 || f.destroyed != 0 {
		t.Fatalf("withdrew on two answers naming different ids: withdrawn %v, destroyed %d",
			reg.withdrawn, f.destroyed)
	}

	f.reconcileAt(t, offlineIdleAfter+2*offlineGrace)

	if !slices.Equal(reg.withdrawn, []int64{20411}) {
		t.Fatalf("withdrawn ids = %v, want the id both later answers named, 20411", reg.withdrawn)
	}
}

func TestAnOfflineAnswerWithoutAnIDOrWithAnotherIsNotEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		poolID int64
		answer RunnerState
		noID   bool
	}{
		{name: "no id", answer: RunnerState{Present: true}, noID: true},
		{name: "a negative id", answer: RunnerState{Present: true, ID: -1}},
		{name: "another id than the pool journaled", poolID: 10899,
			answer: RunnerState{Present: true, ID: 20411}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &inspectingRegistry{answers: []RunnerState{tc.answer, tc.answer, tc.answer}}
			f := newOfflineFixtureWith(t, reg, 1, tc.poolID)
			if tc.noID {
				delete(reg.githubIDs, f.name)
			}

			f.reconcileAt(t, 0)
			f.reconcileAt(t, offlineIdleAfter)
			f.reconcileAt(t, offlineIdleAfter+offlineGrace)
			f.reconcileAt(t, offlineIdleAfter+2*offlineGrace)

			if reg.asked != 3 {
				t.Fatalf("GitHub was asked %d times, want 3", reg.asked)
			}
			if len(reg.withdrawn) != 0 || f.destroyed != 0 {
				t.Fatalf("acted on %s: withdrawn %v, destroyed %d", tc.name, reg.withdrawn, f.destroyed)
			}
			if got := f.status(t); got != alloc.PoolRunnerIdle {
				t.Fatalf("member status = %q, want idle", got)
			}
		})
	}
}

// A remote node's JIT mint journals the registration's id before any job, and a
// member journaled that way is retired by that same id.
func TestAMemberJournaledWithItsIDIsWithdrawnByThatID(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline}}
	f := newOfflineFixtureWith(t, reg, 1, 10899)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if !slices.Equal(reg.withdrawn, []int64{10899}) || f.destroyed != 1 {
		t.Fatalf("withdrawn %v, destroyed %d; want 10899 withdrawn and its compute destroyed",
			reg.withdrawn, f.destroyed)
	}
}
