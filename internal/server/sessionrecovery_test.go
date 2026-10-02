package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// fastSessionReopen makes recoverSession's backoff milliseconds for one test.
// It mutates package state, so no test using it runs in parallel.
func fastSessionReopen(t *testing.T) {
	t.Helper()

	first, ceiling := sessionReopenFirst, sessionReopenMax
	sessionReopenFirst, sessionReopenMax = time.Millisecond, 4*time.Millisecond

	t.Cleanup(func() { sessionReopenFirst, sessionReopenMax = first, ceiling })
}

// errorsFor counts the Error records a run logged about one tier.
func (h *capturingHandler) errorsFor(tier string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0

	for _, r := range *h.records {
		if r.Level != slog.LevelError {
			continue
		}

		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "tier" && a.Value.String() == tier {
				n++

				return false
			}

			return true
		})
	}

	return n
}

// errBrokerTimedOut is the shape of the failure that escaped the client's retries
// on 2026-09-23: one target's connection, not the deployment.
var errBrokerTimedOut = errors.New("scaleset: get message: read tcp: connection timed out")

// brokenAndHealthy is a control plane with one tier whose every poll fails with
// pollErr and one tier whose polls answer. It counts the broken tier's session
// opens and the healthy tier's polls, and signals each poll on healthyPolled.
type brokenAndHealthy struct {
	tiers         []config.Tier
	prov          *fakeProvisioner
	brokenOpens   atomic.Int64
	healthyPolls  atomic.Int64
	healthyPolled chan struct{}
}

func newBrokenAndHealthy(pollErr error) *brokenAndHealthy {
	b := &brokenAndHealthy{
		tiers:         []config.Tier{tier("billet-4vcpu-broken"), tier("billet-4vcpu-healthy")},
		healthyPolled: make(chan struct{}, 1),
	}

	b.prov = &fakeProvisioner{newSession: func(label string) Session {
		if label == b.tiers[0].ScaleSetName() {
			b.brokenOpens.Add(1)

			return &fakeSession{onGet: func() (*Message, error) { return nil, pollErr }}
		}

		return &fakeSession{onPoll: func(int) {
			b.healthyPolls.Add(1)

			select {
			case b.healthyPolled <- struct{}{}:
			default:
			}
		}}
	}}

	return b
}

// ONE TIER'S BROKER FAILING PAST THE CLIENT'S RETRIES LEAVES EVERY OTHER TIER
// SERVING (#207).
//
// The vendored client retries a poll for about twenty-five minutes before the
// error reaches billet, and the listener used to return it, which cancelled every
// listener of every target and drained the whole control plane over one target's
// broker. The broken tier must instead reopen its session, more than once, while
// the healthy tier goes on polling, and say so at Error level each time.
func TestOneTiersFailingPollLeavesTheOtherTiersServing(t *testing.T) {
	fastSessionReopen(t)

	b := newBrokenAndHealthy(errBrokerTimedOut)
	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, b.tiers)
	logged := newCapturingHandler()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		done <- New(a, b.prov, b.tiers, "test-owner", slog.New(logged)).Run(ctx)
	}()

	// JOINED ON EVERY EXIT, a failed assertion included, so the control plane is
	// gone before the ledger closes and the pacing is restored.
	t.Cleanup(func() {
		cancel()
		<-finished
	})

	// THE HEALTHY TIER IS STILL POLLING AFTER THE BROKEN ONE HAS REOPENED ITS
	// SESSION THREE TIMES. Counted from a baseline taken once the reopens have
	// happened, so polls made before the broken tier's first failure prove nothing.
	baseline := int64(-1)

	for baseline < 0 || b.healthyPolls.Load() < baseline+3 {
		if baseline < 0 && b.brokenOpens.Load() >= 4 {
			baseline = b.healthyPolls.Load()
		}

		select {
		case err := <-done:
			t.Fatalf("the control plane stopped while one tier's broker was failing: %v "+
				"(broken tier opened %d sessions, healthy tier polled %d times)",
				err, b.brokenOpens.Load(), b.healthyPolls.Load())
		case <-ctx.Done():
			t.Fatalf("gave up: broken tier opened %d sessions, healthy tier polled %d times "+
				"(baseline %d)", b.brokenOpens.Load(), b.healthyPolls.Load(), baseline)
		case <-b.healthyPolled:
		}
	}

	if got := logged.errorsFor(b.tiers[0].Label); got < 3 {
		t.Errorf("the broken tier logged %d Error records over at least three failed polls; "+
			"a tier GitHub is not reaching must be loud", got)
	}

	cancel()

	if err := <-done; err != nil {
		t.Errorf("Run after a clean stop = %v, want nil", err)
	}
}

// A POLL FAILURE BILLET CANNOT ACT ON STILL STOPS EVERY TIER.
//
// The recovery is for one target's transport. A response billet cannot act on
// leaves it unable to tell which of its commitments are real, which is the
// deployment's problem rather than a tier's.
func TestAnUntrustworthyPollStillStopsEveryTier(t *testing.T) {
	fastSessionReopen(t)

	b := newBrokenAndHealthy(fmt.Errorf("%w: an id nobody offered for", ErrUntrustworthySession))
	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, b.tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := New(a, b.prov, b.tiers, "test-owner", slog.New(slog.DiscardHandler)).Run(ctx)
	if ctx.Err() != nil {
		t.Fatalf("the control plane kept running past an untrustworthy session: %v", err)
	}

	if !errors.Is(err, ErrUntrustworthySession) {
		t.Errorf("Run = %v, want the untrustworthy session", err)
	}

	if got := b.brokenOpens.Load(); got != 1 {
		t.Errorf("the broken tier opened %d sessions; an untrustworthy one must not be reopened", got)
	}
}

// A CONTROLLER THAT HAS LOST ITS CLAIM DOES NOT REOPEN ANYTHING. Fenced when the
// poll fails, it stops without entering the recovery at all.
func TestAFencedListenerDoesNotRecoverItsSession(t *testing.T) {
	fastSessionReopen(t)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var opens atomic.Int64

	session := &fakeSession{onGet: func() (*Message, error) { return nil, errBrokerTimedOut }}
	l := NewListener(a, tiers[0].Label, session,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithLeadershipLostCheck(func() bool { return true }),
		WithSessionReopen(func(context.Context) (Session, error) {
			opens.Add(1)

			return &fakeSession{}, nil
		}))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := l.Run(ctx)
	if ctx.Err() != nil || !errors.Is(err, errBrokerTimedOut) {
		t.Fatalf("Run = %v, want the poll failure returned at once", err)
	}

	if l.sessionFailures != 0 || opens.Load() != 0 {
		t.Errorf("a fenced listener entered the recovery (%d failures counted, %d opens)",
			l.sessionFailures, opens.Load())
	}
}

// AND ONE FENCED WHILE IT WAITS STOPS AT THE NEXT ATTEMPT rather than going on
// opening sessions in a deployment somebody else now runs.
func TestAListenerFencedDuringItsRecoveryStops(t *testing.T) {
	fastSessionReopen(t)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var (
		lost  atomic.Bool
		opens atomic.Int64
	)

	session := &fakeSession{onGet: func() (*Message, error) { return nil, errBrokerTimedOut }}
	l := NewListener(a, tiers[0].Label, session,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithLeadershipLostCheck(lost.Load),
		WithSessionReopen(func(context.Context) (Session, error) {
			opens.Add(1)
			lost.Store(true)

			return nil, errors.New("scaleset: open session: 503 Service Unavailable")
		}))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := l.Run(ctx)
	if ctx.Err() != nil || err == nil {
		t.Fatalf("Run = %v (context %v), want the fenced recovery to stop the listener", err, ctx.Err())
	}

	if got := opens.Load(); got != 1 {
		t.Errorf("it tried %d opens; after the fence landed it must try none", got)
	}
}

// THE RECOVERY RELEASES NOTHING A RUNNING JOB STANDS ON.
//
// The listener launched a pool runner before its first poll failed; the new
// session must find the same runner in `running`, its lease still charged in
// the ledger, and no second launch, while the failed session was closed once
// before the new one opened.
func TestARecoveredSessionKeepsTheRunningJobAndItsLease(t *testing.T) {
	fastSessionReopen(t)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	first := &fakeSession{
		stats: &Statistics{TotalAssignedJobs: 1},
		onGet: func() (*Message, error) { return nil, errBrokerTimedOut },
	}

	var (
		l                  *Listener
		opens              int
		closedBeforeReopen int
		runningAfter       = -1
		leasesAfter        = -1
		usageErr           error
	)

	second := &fakeSession{stats: &Statistics{TotalAssignedJobs: 1}}
	second.onPoll = func(int) {
		runningAfter = l.Running()

		usage, err := a.Usage(t.Context())
		usageErr = err
		leasesAfter = usage.Leases

		cancel()
	}

	var launched []int64

	l = NewListener(a, tiers[0].Label, first,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithRunner(&fakeRunner{onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)

			return nil
		}}),
		WithRunnerRegistry(&fakeRunnerRegistry{}),
		WithSessionReopen(func(context.Context) (Session, error) {
			opens++
			closedBeforeReopen = first.closes()

			return second, nil
		}),
		stopsWithoutWaiting())

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if opens != 1 || second.polls() == 0 {
		t.Fatalf("opens = %d and the new session was polled %d times; want one reopen "+
			"that is then polled", opens, second.polls())
	}

	if closedBeforeReopen != 1 {
		t.Errorf("the failed session was closed %d times before its replacement opened, want 1",
			closedBeforeReopen)
	}

	if usageErr != nil {
		t.Fatalf("Usage: %v", usageErr)
	}

	if len(launched) != 1 || runningAfter != 1 || leasesAfter < 1 {
		t.Errorf("after the reopen: launched %v, running %d, leases charged %d; want the one "+
			"pool runner kept, its lease charged and nothing launched twice",
			launched, runningAfter, leasesAfter)
	}
}

// A REPLACEMENT THAT FAILS FOR A REASON THE DEPLOYMENT MUST STOP FOR STOPS IT.
//
// The first poll's failure is ordinary transport trouble and enters the
// recovery; what the open then answers is classified again, because a recovery
// that retried these would keep a deposed controller or an untrustworthy session
// alive behind an Error line per attempt.
func TestAFatalReplacementStopsTheRecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fatal error
	}{
		{"untrustworthy session", fmt.Errorf("%w: an id nobody offered for", ErrUntrustworthySession)},
		{"lost controller claim", fmt.Errorf("server: open session for tier a: %w", state.ErrLeadershipLost)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastSessionReopen(t)

			tiers := []config.Tier{tier("billet-4vcpu-a")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

			var opens atomic.Int64

			session := &fakeSession{onGet: func() (*Message, error) { return nil, errBrokerTimedOut }}
			l := NewListener(a, tiers[0].Label, session,
				WithLogger(slog.New(slog.DiscardHandler)),
				WithSessionReopen(func(context.Context) (Session, error) {
					opens.Add(1)

					return nil, tc.fatal
				}))

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			err := l.Run(ctx)
			if ctx.Err() != nil {
				t.Fatalf("the recovery went on retrying a fatal open until the test gave up: %v", err)
			}

			if !errors.Is(err, tc.fatal) {
				t.Errorf("Run = %v, want the fatal open's error", err)
			}

			if got := opens.Load(); got != 1 {
				t.Errorf("it tried %d opens, want exactly the one that answered fatally", got)
			}
		})
	}
}

// AND THE WAIT FOR A HELD SESSION, WHICH A RECOVERY SITS IN TOO, ENDS WHEN THIS
// PROCESS STOPS BEING THE CONTROLLER rather than opening one for a deployment
// somebody else now runs.
func TestTheWaitForAHeldSessionEndsWhenLeadershipIsLost(t *testing.T) {
	original := sessionRetryFor
	sessionRetryFor = time.Millisecond

	t.Cleanup(func() { sessionRetryFor = original })

	held := &heldSessions{refusals: 1_000_000}
	s := &Server{
		prov: held, log: slog.New(slog.DiscardHandler), owner: "billet",
		leadershipLost: func() bool { return held.attempts.Load() >= 1 },
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, err := s.openSession(ctx, &config.Tier{Label: "linux"}, &ScaleSet{ID: 1}, s.prov)
	if !errors.Is(err, state.ErrLeadershipLost) {
		t.Fatalf("openSession = %v, want the lost claim", err)
	}

	if got := held.attempts.Load(); got != 1 {
		t.Errorf("it asked GitHub %d times; after the claim was lost it must ask no more", got)
	}
}

// gatedOpener is a WithSessionReopen whose first open ignores its context, the
// way an open queued on the vendored client's per-target mutex does, until the
// test releases it, and then answers first. Every later open answers later().
type gatedOpener struct {
	entered  chan struct{}
	release  func()
	released chan struct{}
	returned chan struct{}
	opens    atomic.Int64
	first    error
	later    func() (Session, error)
}

func newGatedOpener(t *testing.T, first error, later func() (Session, error)) *gatedOpener {
	t.Helper()

	g := &gatedOpener{
		entered: make(chan struct{}), released: make(chan struct{}),
		returned: make(chan struct{}), first: first, later: later,
	}

	var once sync.Once

	g.release = func() { once.Do(func() { close(g.released) }) }

	// THE STUCK OPEN IS JOINED, so it cannot outlive the test it belongs to.
	t.Cleanup(func() {
		g.release()

		select {
		case <-g.entered:
			<-g.returned
		default:
		}
	})

	return g
}

func (g *gatedOpener) open(context.Context) (Session, error) {
	if g.opens.Add(1) > 1 {
		return g.later()
	}

	defer close(g.returned)

	close(g.entered)
	<-g.released

	return nil, g.first
}

// waitFor fails the test unless ch closes within a bound far beyond what the
// code under test needs.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("%s did not happen", what)
	}
}

// A SECOND SIGNAL ENDS A LISTENER WHOSE REPLACEMENT OPEN IS STUCK.
//
// The open ignores its context, as one queued behind another call on the
// vendored client's per-target mutex does, so a recovery that waited for it
// inline would hold the listener, and the control plane's join, until that call
// gave up. The old session was closed once, by the recovery, and the teardown
// must not close it again.
func TestASecondSignalEndsAListenerWhoseReopenIsStuck(t *testing.T) {
	fastSessionReopen(t)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	g := newGatedOpener(t, errors.New("unused"), nil)
	first := &fakeSession{onGet: func() (*Message, error) { return nil, errBrokerTimedOut }}
	l := NewListener(a, tiers[0].Label, first,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithSessionReopen(g.open), stopsWithoutWaiting())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runDone := make(chan struct{})

	var runErr error

	go func() {
		defer close(runDone)

		runErr = l.Run(ctx)
	}()

	waitFor(t, g.entered, "the replacement open")
	cancel()
	waitFor(t, runDone, "the listener stopping while its replacement open was stuck")

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run = %v, want a clean stop", runErr)
	}

	if got := first.closes(); got != 1 {
		t.Errorf("the failed session was closed %d times, want once", got)
	}
}

// drainScenario is a listener holding one pool runner, so a drain does not end
// at once, whose first session fails its every poll and whose first replacement
// open is stuck in g. Closing hurry ends its drain.
func drainScenario(t *testing.T, g *gatedOpener) (*Listener, *fakeSession, chan struct{}) {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	first := &fakeSession{
		stats: &Statistics{TotalAssignedJobs: 1},
		onGet: func() (*Message, error) { return nil, errBrokerTimedOut },
	}
	hurry := make(chan struct{})

	l := NewListener(a, tiers[0].Label, first,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithRunner(&fakeRunner{}),
		WithRunnerRegistry(&fakeRunnerRegistry{}),
		WithSessionReopen(g.open),
		WithHurrySignal(hurry))

	return l, first, hurry
}

// A RECOVERY THE DRAIN'S START CUT SHORT RESUMES BEFORE THE NEXT POLL.
//
// The session in hand is the one the recovery closed, so polling it again would
// spend another round of the client's retries against a session that no longer
// exists before the drain could hear about its own jobs.
func TestADrainResumesItsRecoveryBeforePollingTheClosedSession(t *testing.T) {
	fastSessionReopen(t)

	var (
		hurry chan struct{}
		once  sync.Once
	)

	second := &fakeSession{}
	second.onPoll = func(int) { once.Do(func() { close(hurry) }) }

	g := newGatedOpener(t, errors.New("scaleset: open session: cut short"),
		func() (Session, error) { return second, nil })

	l, first, hurry := drainScenario(t, g)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runDone := make(chan struct{})

	var runErr error

	go func() {
		defer close(runDone)

		runErr = l.Run(ctx)
	}()

	waitFor(t, g.entered, "the replacement open")
	cancel()
	g.release()
	waitFor(t, runDone, "the drain ending")

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("Run = %v, want a clean stop", runErr)
	}

	if got := first.polls(); got != 1 {
		t.Errorf("the failed session was polled %d times; the drain must reopen before polling", got)
	}

	if second.polls() == 0 || g.opens.Load() != 2 {
		t.Errorf("the drain opened %d sessions and polled the replacement %d times; want "+
			"the recovery resumed and its session polled", g.opens.Load(), second.polls())
	}
}

// AND A FATAL ANSWER TO THE REPLACEMENT OPEN IS REPORTED AS ITSELF, even when the
// drain is what was waiting for it, rather than as the shutdown's cancellation.
func TestAFatalReopenDuringADrainIsReportedAsItself(t *testing.T) {
	fastSessionReopen(t)

	fatal := fmt.Errorf("%w: an id nobody offered for", ErrUntrustworthySession)

	var (
		hurry chan struct{}
		once  sync.Once
	)

	// REACHED ONLY IF THE FATAL ANSWER WAS LOST: a session that ends the drain,
	// so the failure is a wrong error rather than a hang.
	second := &fakeSession{}
	second.onPoll = func(int) { once.Do(func() { close(hurry) }) }

	g := newGatedOpener(t, fatal, func() (Session, error) { return second, nil })

	l, _, hurry := drainScenario(t, g)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runDone := make(chan struct{})

	var runErr error

	go func() {
		defer close(runDone)

		runErr = l.Run(ctx)
	}()

	waitFor(t, g.entered, "the replacement open")
	cancel()
	g.release()
	waitFor(t, runDone, "the listener stopping")

	if !errors.Is(runErr, ErrUntrustworthySession) {
		t.Errorf("Run = %v, want the untrustworthy session the open answered", runErr)
	}

	if got := g.opens.Load(); got != 1 {
		t.Errorf("it opened %d sessions after a fatal answer, want none beyond it", got)
	}
}
