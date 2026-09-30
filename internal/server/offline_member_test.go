package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// inspectingRegistry is a registry that also answers InspectRunner from a script.
type inspectingRegistry struct {
	fakeRunnerRegistry

	answers  []RunnerState
	errs     []error
	asked    int
	askedFor []string
}

func (r *inspectingRegistry) InspectRunner(_ context.Context, name string, _ int64) (RunnerState, error) {
	i := r.asked
	r.asked++
	r.askedFor = append(r.askedFor, name)

	if i < len(r.errs) && r.errs[i] != nil {
		return RunnerState{}, r.errs[i]
	}

	if i < len(r.answers) {
		return r.answers[i], nil
	}

	return RunnerState{}, errors.New("unscripted inspection")
}

type offlineFixture struct {
	a         *alloc.Allocator
	l         *Listener
	reg       *inspectingRegistry
	lease     *alloc.Lease
	name      string
	now       time.Time
	destroyed int
}

// newOfflineFixture registers one idle pool member with a GitHub id, the shape
// the member on 2026-09-30 had: registered, never given a job.
func newOfflineFixture(t *testing.T, reg *inspectingRegistry) *offlineFixture {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	f := &offlineFixture{reg: reg, now: time.Date(2026, 9, 30, 8, 52, 0, 0, time.UTC)}
	f.a = newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 64 * config.GiB}, tiers)
	f.lease = poolLeaseForTier(t, f.a, tiers[0].Label)

	if err := f.a.Assign(t.Context(), f.lease.ID, f.lease.Epoch, 0, -53128); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	f.name = provider.InstanceName(f.lease.ID)
	if err := f.a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: f.lease.ID,
		Tier: tiers[0].Label, LaunchRequestID: -53128, RunnerID: 10899, RunnerName: f.name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}

	f.l = NewListener(f.a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onDestroy: func(int64) error { f.destroyed++; return nil },
	}), WithRunnerRegistry(reg))
	f.l.now = func() time.Time { return f.now }

	return f
}

// reconcileAt runs the pool reconciliation GitHub's last statistics would ask
// for (one assigned job, the frozen count) at the given offset.
func (f *offlineFixture) reconcileAt(t *testing.T, after time.Duration) {
	t.Helper()

	f.now = time.Date(2026, 9, 30, 8, 52, 0, 0, time.UTC).Add(after)
	if err := f.l.reconcilePool(t.Context(), 1); err != nil {
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

func TestAMemberGitHubReportsOfflineTwiceIsRetired(t *testing.T) {
	offline := RunnerState{Present: true}
	f := newOfflineFixture(t, &inspectingRegistry{answers: []RunnerState{offline, offline}})

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)

	if f.reg.asked != 2 {
		t.Fatalf("GitHub was asked %d times, want 2", f.reg.asked)
	}
	if len(f.reg.names) != 1 || f.reg.names[0] != f.name {
		t.Fatalf("registration removed = %v, want exactly %q", f.reg.names, f.name)
	}
	if f.destroyed != 1 {
		t.Fatalf("compute destroyed %d times, want 1", f.destroyed)
	}
	if got := f.status(t); got == alloc.PoolRunnerIdle {
		t.Fatalf("member still idle after two offline answers %s apart", offlineGrace)
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
			if len(reg.names) != 0 || f.destroyed != 0 {
				t.Fatalf("retired on %s: removed %v, destroyed %d", tc.name, reg.names, f.destroyed)
			}
			if got := f.status(t); got != alloc.PoolRunnerIdle {
				t.Fatalf("member status = %q, want idle", got)
			}
		})
	}
}

// An answer that is not "offline" between two that are restarts the grace: the
// retirement needs two consecutive offline observations, not any two.
func TestAnInterruptedOfflineRunStartsTheGraceAgain(t *testing.T) {
	offline := RunnerState{Present: true}
	online := RunnerState{Present: true, Online: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, online, offline, offline}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineGrace)
	f.reconcileAt(t, offlineIdleAfter+2*offlineGrace)

	if len(reg.names) != 0 {
		t.Fatalf("retired after an online answer interrupted the offline run: %v", reg.names)
	}

	f.reconcileAt(t, offlineIdleAfter+3*offlineGrace)

	if len(reg.names) != 1 {
		t.Fatalf("not retired after two fresh offline answers %s apart", offlineGrace)
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

func TestAListenerAsksAboutAtMostOneMemberPerInterval(t *testing.T) {
	offline := RunnerState{Present: true}
	reg := &inspectingRegistry{answers: []RunnerState{offline, offline, offline}}
	f := newOfflineFixture(t, reg)

	f.reconcileAt(t, 0)
	f.reconcileAt(t, offlineIdleAfter)
	f.reconcileAt(t, offlineIdleAfter+offlineInspectEvery-time.Second)

	if reg.asked != 1 {
		t.Fatalf("GitHub was asked %d times inside one %s interval, want 1", reg.asked, offlineInspectEvery)
	}
}
