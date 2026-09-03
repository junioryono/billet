package rollout

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeResolver answers with one target, or refuses.
type fakeResolver struct {
	target Target
	err    error
	calls  int
}

func (r *fakeResolver) Resolve(context.Context) (Target, error) {
	r.calls++

	if r.err != nil {
		return Target{}, r.err
	}

	return r.target, nil
}

const newerVersion = "v0.5.0"

// starting builds a starter over a real store, a fleet of the named hosts on
// v0.4.0, and a resolver naming v0.5.0 — the ordinary shape of "the channel
// advanced".
func starting(t *testing.T, policy StartPolicy, nodes ...string) (*Store,
	*fakeResolver, *Starter,
) {
	t.Helper()

	_, s := open(t)

	fleet := &fakeFleet{}
	for _, n := range nodes {
		fleet.set(n, targetVersion, 19, true)
	}

	resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}

	if policy.Channel == "" && policy.Pin == "" {
		policy.Channel = "stable"
	}

	starter := NewStarter(s, fleet, resolve, policy, targetVersion,
		WithStarterLogger(slog.New(slog.DiscardHandler)))

	return s, resolve, starter
}

func openRollout(t *testing.T, s *Store) *Rollout {
	t.Helper()

	r, err := s.Open(t.Context())
	if err != nil && !errors.Is(err, ErrNoRollout) {
		t.Fatalf("Open: %v", err)
	}

	return r
}

// A HEALTHY TICK RECORDS EXACTLY ONE ROLLOUT, to the resolved digest, covering
// every host, marked as the channel's decision rather than a person's.
func TestAnAdvancedChannelStartsOneRollout(t *testing.T) {
	t.Parallel()

	s, _, starter := starting(t, StartPolicy{Enabled: true}, "epyc-1", "epyc-2")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	r := openRollout(t, s)
	if r == nil {
		t.Fatal("no rollout was started")
	}

	if r.TargetVersion != newerVersion || r.TargetDigest != otherDigest {
		t.Errorf("the rollout targets %s (%s), want %s (%s)", r.TargetVersion, r.TargetDigest,
			newerVersion, otherDigest)
	}

	if r.PriorVersion != targetVersion || r.Channel != "stable" {
		t.Errorf("the rollout records prior %q on channel %q", r.PriorVersion, r.Channel)
	}

	if !strings.HasPrefix(r.CreatedBy, "automatic") {
		t.Errorf("the rollout was recorded as started by %q, want the channel", r.CreatedBy)
	}

	if r.Policy.AllowDowngrade {
		t.Error("an automatic rollout was recorded as allowing a downgrade")
	}

	nodes, err := s.Nodes(t.Context(), r.ID)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}

	if len(nodes) != 2 {
		t.Errorf("the rollout covers %d hosts, want 2", len(nodes))
	}

	// AND A SECOND TICK STARTS NOTHING MORE.
	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}

	if again := openRollout(t, s); again == nil || again.ID != r.ID {
		t.Errorf("a second tick replaced the open rollout")
	}
}

// DISABLED STARTS NOTHING AND ASKS NOTHING. `automatic: false` is the one
// sentence that turns this off, and a starter that still resolved the channel
// would be a deployment reaching the network for a decision it may not make.
func TestADisabledStarterNeitherResolvesNorStarts(t *testing.T) {
	t.Parallel()

	s, resolve, starter := starting(t, StartPolicy{Enabled: false}, "epyc-1")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if resolve.calls != 0 {
		t.Errorf("a disabled starter resolved the channel %d time(s)", resolve.calls)
	}

	if openRollout(t, s) != nil {
		t.Error("a disabled starter started a rollout")
	}
}

// A CLOSED WINDOW STARTS NOTHING. The window bounds when a rollout may BEGIN;
// outside it the channel is not even asked.
func TestAClosedMaintenanceWindowStartsNothing(t *testing.T) {
	t.Parallel()

	closed := func(time.Time) bool { return false }

	s, resolve, starter := starting(t, StartPolicy{Enabled: true, OpenAt: closed}, "epyc-1")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if resolve.calls != 0 || openRollout(t, s) != nil {
		t.Errorf("a closed window resolved %d time(s) and started a rollout: %v",
			resolve.calls, openRollout(t, s) != nil)
	}
}

// AN OPEN ROLLOUT IS LEFT ALONE. Two decisions at once is what ErrOpen exists to
// refuse, and the starter must not even resolve the channel over one.
func TestAnOpenRolloutIsNotJoinedByAnother(t *testing.T) {
	t.Parallel()

	s, resolve, starter := starting(t, StartPolicy{Enabled: true}, "epyc-1")

	existing := start(t, s, "epyc-1")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if resolve.calls != 0 {
		t.Errorf("the starter resolved the channel %d time(s) over an open rollout", resolve.calls)
	}

	if r := openRollout(t, s); r == nil || r.ID != existing.ID {
		t.Error("the open rollout was replaced")
	}
}

// A FLEET ALREADY ON THE TARGET NEEDS NO ROLLOUT, and a host that reports
// nothing is not on it.
func TestAFleetOnTheTargetStartsNothing(t *testing.T) {
	t.Parallel()

	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", newerVersion, 19, true)

	resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}

	starter := NewStarter(s, fleet, resolve, StartPolicy{Enabled: true, Channel: "stable"},
		newerVersion, WithStarterLogger(slog.New(slog.DiscardHandler)))

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("a rollout was started for a fleet already on the target")
	}

	// A HOST THAT SAYS NOTHING IS NOT CONVERGED, so a rollout starts for it.
	fleet.set("silent", "", 13, true)

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) == nil {
		t.Fatal("a host reporting no release was read as already on the target")
	}
}

// AN AUTOMATIC START NEVER DOWNGRADES. A pin older than the running release is
// an operator's decision to make by name, not one the channel makes for them.
func TestAnOlderTargetIsNeverStartedAutomatically(t *testing.T) {
	t.Parallel()

	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", newerVersion, 19, true)

	resolve := &fakeResolver{target: Target{Version: targetVersion, Digest: otherDigest}}

	starter := NewStarter(s, fleet, resolve,
		StartPolicy{Enabled: true, Pin: targetVersion, Rollout: Policy{AllowDowngrade: true}},
		newerVersion, WithStarterLogger(slog.New(slog.DiscardHandler)))

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("an automatic rollout was started to an older release")
	}
}

// AN OPERATOR'S ABORT BEATS THE CHANNEL. A rollout to these exact bytes was
// abandoned with a reason, and restarting it an hour later would overrule that
// with nothing new to go on. A different digest ends the refusal.
func TestAnAbortedTargetIsNotRestartedAutomatically(t *testing.T) {
	t.Parallel()

	s, resolve, starter := starting(t, StartPolicy{Enabled: true}, "epyc-1")

	aborted, err := s.Start(t.Context(), StartRequest{
		Channel: "stable", TargetVersion: newerVersion, TargetDigest: otherDigest,
		PriorVersion: targetVersion, Policy: DefaultPolicy(), CreatedBy: "ops",
		Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := s.Finish(t.Context(), aborted.ID, StateAborted, "a cache regression"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("the aborted target was restarted automatically")
	}

	// THE CHANNEL MOVING ON IS WHAT ENDS THE REFUSAL.
	resolve.target = Target{Version: "v0.5.1", Digest: targetDigest}

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if r := openRollout(t, s); r == nil || r.TargetVersion != "v0.5.1" {
		t.Fatal("a newer target after the abort was not started")
	}
}

// A TARGET THAT CANNOT BE RESOLVED RECORDS NOTHING AND IS NOT AN ERROR. An
// unreachable or expired channel is a condition the next tick may find changed,
// and reporting it as a failed pass would have the control plane's log call the
// mechanism broken while it is waiting exactly as designed.
func TestAResolutionFailureRecordsNothing(t *testing.T) {
	t.Parallel()

	s, resolve, starter := starting(t, StartPolicy{Enabled: true}, "epyc-1")

	resolve.err = errors.New("the stable channel expired")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick reported a resolution failure as an error: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("a rollout was started with no resolved target")
	}

	// AND IT IS SAID ONCE PER WINDOW, NOT ONCE PER TICK. The record of when a
	// condition was last said is the observable: it moves when the message is
	// logged and stays when it is suppressed.
	clock := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	when := clock

	starter.now = func() time.Time { return when }
	starter.said = map[string]time.Time{}

	starter.say("resolve", "x")

	if got := starter.said["resolve"]; !got.Equal(clock) {
		t.Fatalf("the first report was recorded at %v, want %v", got, clock)
	}

	when = clock.Add(time.Hour)
	starter.say("resolve", "x")

	if got := starter.said["resolve"]; !got.Equal(clock) {
		t.Errorf("a report one hour after the first was said again (recorded %v)", got)
	}

	when = clock.Add(sayAgainAfter + time.Minute)
	starter.say("resolve", "x")

	if got := starter.said["resolve"]; !got.Equal(when) {
		t.Errorf("a report after the window was suppressed (recorded %v, want %v)", got, when)
	}

	// A DIFFERENT CONDITION HAS ITS OWN WINDOW.
	when = clock.Add(sayAgainAfter + 2*time.Minute)
	starter.say("aborted", "y")

	if got := starter.said["aborted"]; !got.Equal(when) {
		t.Errorf("a different condition shared the first one's window (recorded %v)", got)
	}
}

// AN EMPTY FLEET IS AN ORDINARY STATE, NOT A FAILURE. A control plane whose first
// node has not registered would otherwise reach Start's refusal every hour.
func TestAnEmptyFleetStartsNothingQuietly(t *testing.T) {
	t.Parallel()

	s, _, starter := starting(t, StartPolicy{Enabled: true})

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("a rollout was started over no hosts")
	}
}
