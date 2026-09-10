package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
)

// EVERY STOP AND START RUNS UNDER THE UNIT'S OWN BOUND: lifeops gives a stop
// or a start no deadline of its own, so each caller derives one from what
// systemd would allow (TimeoutStartSec, TimeoutStopSec) plus a margin, and a
// caller that passed none would let a manager that never answered hold a
// command forever, while one that passed a shorter bound would report a
// draining node as down. The transaction's node stop is the one deliberate
// exception, unbounded by billet.

// deadlineWithin says a deadline lies in [now+bound, now+bound+margin], with
// a little slack for the time the test took to get here.
func deadlineWithin(t *testing.T, what string, d time.Time, bound time.Duration) {
	t.Helper()

	if d.IsZero() {
		t.Fatalf("%s ran under no deadline; want the unit's own bound of %s plus the margin", what, bound)
	}

	remaining := time.Until(d)
	if remaining < bound || remaining > bound+lifecycleDeadlineMargin+time.Second {
		t.Fatalf("%s ran under a deadline %s away; want the unit's own bound %s plus the margin %s", what,
			remaining.Round(time.Second), bound, lifecycleDeadlineMargin)
	}
}

func TestUpStartsEachUnitUnderItsOwnStartBound(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a prepared host was refused: %v", err)
	}

	for _, unit := range []string{deploy.ServerUnitName, deploy.NodeUnitName} {
		deadlineWithin(t, "the start of "+unit, f.deadlines["start "+unit], deploy.UnitStartTimeout)
	}
}

func TestDownStopsEachUnitUnderItsOwnStopBound(t *testing.T) {
	asLinux(t)

	cfgPath, stateDir := downConfig(t, true)
	_ = openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	capture(t, func() {
		if err := runLocalDown(t.Context(), downOptions{configPath: cfgPath}); err != nil {
			t.Errorf("an idle host was refused: %v", err)
		}
	})

	for _, unit := range []string{deploy.ServerUnitName, deploy.NodeUnitName} {
		deadlineWithin(t, "the stop of "+unit, f.deadlines["stop "+unit], deploy.UnitStopTimeout)
	}
}

// THE TRANSACTION: its starts under the start bound, observed through the
// inspector's own seam; its node stop under no deadline at all.
func TestTheTransactionStartsUnderTheStartBoundAndStopsTheNodeUnbounded(t *testing.T) {
	seen := map[string]time.Time{}
	unbounded := map[string]bool{}

	prev := newHostInspector
	newHostInspector = func() *lifeops.Inspector {
		return lifeops.NewInspector(
			lifeops.WithSystemctl("/usr/bin/false"),
			lifeops.WithObserver(func(ctx context.Context, args []string) {
				if d, ok := ctx.Deadline(); ok {
					seen[args[0]+" "+args[len(args)-1]] = d
				} else {
					unbounded[args[0]+" "+args[len(args)-1]] = true
				}
			}))
	}

	t.Cleanup(func() { newHostInspector = prev })

	cfg := &config.Config{Node: &config.NodeConfig{}}
	h := newSystemdHost(cfg, "/etc/billet/billet.yaml", "/tmp/staged", nil)

	// The starts: /usr/bin/false fails them, which is fine; the deadline was
	// observed before the failure.
	if err := h.StartServices(t.Context()); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("a start through /usr/bin/false: %v", err)
	}

	deadlineWithin(t, "the transaction's node start", seen["start "+nodeUnit], deploy.UnitStartTimeout)

	if err := h.StopNode(t.Context()); err != nil {
		t.Logf("the node's stop: %v", err)
	}

	if !unbounded["stop "+nodeUnit] {
		t.Fatalf("the transaction's node stop ran under %s; it is unbounded by billet, bounded by systemd's "+
			"TimeoutStopSec alone", seen["stop "+nodeUnit])
	}
}
