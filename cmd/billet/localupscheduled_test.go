package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// scheduledFake is a converger that, like launchd, can install the scheduled
// agents. It records what it was asked beside the rest of the trace.
type scheduledFake struct {
	*fakeConverger

	scheduleErr map[string]error
}

func (f *scheduledFake) Scheduled() (string, string) {
	return deploy.UpgradeAgentLabel, deploy.ImagesAgentLabel
}

func (f *scheduledFake) EnableScheduled(_ context.Context, label string) error {
	f.record("schedule " + label)

	return f.scheduleErr[label]
}

// ON A MANAGER THAT HAS THEM, THE SCHEDULED AGENTS ARE INSTALLED AFTER THE
// SERVICES ARE UP, and only then. A Mac's `up` is its converge, so this is the
// one place the upgrade and image schedules get installed; installing them
// before the services were proved would schedule an upgrade of a host nothing
// had established could run.
func TestUpInstallsTheScheduledAgentsAfterTheServicesOnAMac(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	inner := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)
	f := &scheduledFake{fakeConverger: inner}

	converge = func(...lifeops.ConvergeOption) converger { return f }

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a prepared host was refused: %v", err)
	}

	trace := strings.Join(f.trace, " → ")

	want := "enable " + deploy.NodeUnitName + " → schedule " + deploy.UpgradeAgentLabel +
		" → schedule " + deploy.ImagesAgentLabel
	if !strings.HasSuffix(trace, want) {
		t.Errorf("up did:\n  %s\nwant it to end with:\n  %s", trace, want)
	}
}

// A SCHEDULE THAT COULD NOT BE INSTALLED IS REPORTED AS EXACTLY THAT: the
// services are up, and this Mac will not act on rollouts. The same exit status a
// sealed deployment gets, because a script that brings a host up and moves on
// would otherwise move on.
func TestUpReportsAScheduleItCouldNotInstall(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	inner := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)
	f := &scheduledFake{
		fakeConverger: inner,
		scheduleErr:   map[string]error{deploy.UpgradeAgentLabel: errors.New("no domain")},
	}

	converge = func(...lifeops.ConvergeOption) converger { return f }

	err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil {
		t.Fatal("a schedule that could not be installed was reported as up")
	}

	if exitStatus(err) != 2 || !strings.Contains(err.Error(), deploy.UpgradeAgentLabel) {
		t.Errorf("the failure is %v (exit %d), want exit 2 naming the agent", err,
			exitStatus(err))
	}

	if !strings.Contains(strings.Join(f.trace, " "), "start "+deploy.NodeUnitName) {
		t.Errorf("the services were not started before the schedule was attempted: %v", f.trace)
	}
}

// ON A MANAGER WITHOUT SCHEDULED AGENTS NOTHING IS ATTEMPTED. The package and the
// role own systemd's timers; `up` there asks for nothing it cannot install.
func TestUpInstallsNoScheduleWhereTheManagerHasNone(t *testing.T) {
	asLinux(t)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	if err := runLocalUp(t.Context(), upOptions{configPath: cfg, servicePath: cfg}); err != nil {
		t.Fatalf("a prepared host was refused: %v", err)
	}

	for _, step := range f.trace {
		if strings.HasPrefix(step, "schedule") {
			t.Errorf("a manager with no scheduled agents was asked to install one: %v", f.trace)
		}
	}
}
