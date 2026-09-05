package rollout

import (
	"log/slog"
	"strings"
	"testing"
)

// A RUNNING ROLLOUT WHOSE TARGET IS BEHIND THE CONTROL PLANE IS FINISHED AS
// SUPERSEDED, AND THE CHANNEL'S TARGET STARTS ON THE NEXT TICK.
//
// A rollout to v0.4.0 was recorded while the channel said v0.4.0; the channel
// moved to v0.5.0 and a person moved the control plane there by hand. The
// rollout can never end forwards (the controller's timer refuses a downgrade)
// and, left running, it blocks every later rollout — or, on a release binary
// whose downgrade guard could not tell, it ends backwards (2026-09-05). Here the
// starter names it superseded with the reason on the record, and the next tick
// starts v0.5.0. The aborted digest is not restarted by itself, which is the
// ordinary abort rule and exactly right for a target the fleet has passed.
func TestARunningRolloutBehindTheControlPlaneIsSuperseded(t *testing.T) {
	t.Parallel()

	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", targetVersion, 19, true)

	// THE CONTROL PLANE IS ON THE CHANNEL'S TARGET, moved there by hand, and the
	// rollout on record still names the release before it.
	resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}
	starter := NewStarter(s, fleet, resolve, StartPolicy{Enabled: true, Channel: "stable"},
		newerVersion, WithStarterLogger(slog.New(slog.DiscardHandler)))

	stale := start(t, s, "epyc-1")

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick over the stale rollout: %v", err)
	}

	if r := openRollout(t, s); r != nil {
		t.Fatalf("the stale rollout %s is still open after the tick", r.ID)
	}

	finished, found, err := s.NewestForTarget(t.Context(), stale.TargetDigest)
	if err != nil || !found {
		t.Fatalf("NewestForTarget: found=%v err=%v", found, err)
	}

	if finished.State != StateAborted || !strings.Contains(finished.TerminalReason, "superseded") ||
		!strings.Contains(finished.TerminalReason, newerVersion) {
		t.Fatalf("the stale rollout ended as %q with reason %q; want aborted as superseded by %s",
			finished.State, finished.TerminalReason, newerVersion)
	}

	if resolve.calls != 0 {
		t.Errorf("the superseding tick resolved the channel %d time(s); it should only end the "+
			"stale rollout", resolve.calls)
	}

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick after the supersede: %v", err)
	}

	next := openRollout(t, s)
	if next == nil || next.TargetVersion != newerVersion {
		t.Fatalf("the channel's target did not start after the supersede: %+v", next)
	}
}

// A RUNNING ROLLOUT AT OR ABOVE THE CONTROL PLANE IS LEFT ALONE, and so is one
// the guard cannot order: only a proved downgrade is dead.
func TestARunningRolloutAtOrAboveTheControlPlaneIsNotSuperseded(t *testing.T) {
	t.Parallel()

	for _, ours := range []string{targetVersion, "v0.3.0", "(devel)"} {
		_, s := open(t)

		fleet := &fakeFleet{}
		fleet.set("epyc-1", "v0.3.26", 19, true)

		resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}
		starter := NewStarter(s, fleet, resolve, StartPolicy{Enabled: true, Channel: "stable"},
			ours, WithStarterLogger(slog.New(slog.DiscardHandler)))

		existing := start(t, s, "epyc-1")

		if err := starter.Tick(t.Context()); err != nil {
			t.Fatalf("Tick with the control plane on %s: %v", ours, err)
		}

		if r := openRollout(t, s); r == nil || r.ID != existing.ID {
			t.Errorf("with the control plane on %s the running rollout to %s was ended", ours,
				targetVersion)
		}
	}
}

// AN OPERATOR'S DOWNGRADE IS BEHIND THE CONTROL PLANE BY DEFINITION, AND IS NOT
// SUPERSEDED: `rollout start --version <older> --allow-downgrade` names the
// downgrade on purpose, and the timer acts on it under the permission it carries.
func TestAnOperatorsDowngradeRolloutIsNotSuperseded(t *testing.T) {
	t.Parallel()

	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", newerVersion, 19, true)

	resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}
	starter := NewStarter(s, fleet, resolve, StartPolicy{Enabled: true, Channel: "stable"},
		newerVersion, WithStarterLogger(slog.New(slog.DiscardHandler)))

	policy := DefaultPolicy()
	policy.AllowDowngrade = true

	downgrade, err := s.Start(t.Context(), StartRequest{
		Channel: "", TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: newerVersion, Policy: policy, CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start the downgrade: %v", err)
	}

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if r := openRollout(t, s); r == nil || r.ID != downgrade.ID {
		t.Fatal("the operator's downgrade rollout was superseded by the starter")
	}
}

// A HOST THAT REGISTERED THE BARE FORM IS ON THE TARGET. Releases through v0.9.1
// register "0.9.1" while the channel says "v0.9.1", and a string comparison read
// every such host as never converged, so the starter kept trying to start what
// the fleet already ran.
func TestAHostReportingTheBareFormIsOnTheTarget(t *testing.T) {
	t.Parallel()

	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", strings.TrimPrefix(newerVersion, "v"), 19, true)

	resolve := &fakeResolver{target: Target{Version: newerVersion, Digest: otherDigest}}
	starter := NewStarter(s, fleet, resolve, StartPolicy{Enabled: true, Channel: "stable"},
		strings.TrimPrefix(newerVersion, "v"), WithStarterLogger(slog.New(slog.DiscardHandler)))

	if err := starter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if openRollout(t, s) != nil {
		t.Fatal("a rollout was started for a fleet whose hosts report the target in the bare form")
	}
}
