package deploy_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// THE UPGRADE UNIT IS THE ROOT EXECUTOR, AND ITS TIMER IS WHAT IS ENABLED.
//
// The control plane runs unprivileged and cannot replace its own binary, so the
// one thing that turns a recorded rollout into an upgraded controller host is
// this pair. A unit that ran as the service account, or whose ExecStart acted on
// the channel rather than the ledger, would be a different design wearing the
// same name.
func TestTheUpgradeUnitActsOnTheRecordedRolloutAsRoot(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"User=root",
		"Type=oneshot",
		"ExecStart=/usr/bin/billet host-upgrade --from-rollout --config /etc/billet/billet.yaml",
		"TimeoutStartSec=88200",
		// THE LEDGER'S ENVIRONMENT, or a PostgreSQL controller's updater cannot
		// read the rollout it is meant to act on.
		"EnvironmentFile=-/etc/billet/server.env",
	} {
		if !strings.Contains(deploy.UpgradeUnit, want) {
			t.Errorf("%s does not carry %q", deploy.UpgradeUnitName, want)
		}
	}

	requireScheduledPair(t, deploy.UpgradeUnitName, deploy.UpgradeUnit,
		deploy.UpgradeTimerName, deploy.UpgradeTimer)

	if !strings.Contains(deploy.UpgradeTimer, "OnUnitActiveSec=5min") {
		t.Errorf("%s does not look every five minutes", deploy.UpgradeTimerName)
	}
}

// THE IMAGES UNIT REFRESHES AS ROOT, DAILY, AND RUNS A MISSED DAY.
func TestTheImagesUnitRefreshesDailyAsRoot(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"User=root",
		"Type=oneshot",
		"ExecStart=/usr/bin/billet images refresh --config /etc/billet/billet.yaml",
	} {
		if !strings.Contains(deploy.ImagesRefreshUnit, want) {
			t.Errorf("%s does not carry %q", deploy.ImagesRefreshUnitName, want)
		}
	}

	requireScheduledPair(t, deploy.ImagesRefreshUnitName, deploy.ImagesRefreshUnit,
		deploy.ImagesRefreshTimerName, deploy.ImagesRefreshTimer)

	for _, want := range []string{"OnCalendar=daily", "Persistent=true"} {
		if !strings.Contains(deploy.ImagesRefreshTimer, want) {
			t.Errorf("%s does not carry %q", deploy.ImagesRefreshTimerName, want)
		}
	}
}

// THE MAC'S SCHEDULED AGENTS ARE ONESHOTS ON A SCHEDULE, NOT SERVICES. A
// KeepAlive on either would have launchd restart a oneshot the instant it
// exited, forever; a missing schedule would run it once at login and never
// again, which reads exactly like a working schedule.
func TestTheMacScheduledAgentsAreOneshotsOnASchedule(t *testing.T) {
	t.Parallel()

	for name, agent := range map[string]struct{ body, schedule, program string }{
		deploy.UpgradeAgentName: {deploy.UpgradeAgent, "StartInterval",
			"<string>host-upgrade</string>"},
		deploy.ImagesAgentName: {deploy.ImagesAgent, "StartCalendarInterval",
			"<string>refresh</string>"},
	} {
		if strings.Contains(agent.body, "KeepAlive") {
			t.Errorf("%s carries KeepAlive, which restarts a oneshot the instant it exits", name)
		}

		for _, want := range []string{
			agent.schedule, agent.program, "<key>RunAtLoad</key>",
			"/usr/local/bin/billet", "/usr/local/etc/billet/billet.yaml",
			"/opt/homebrew/bin:/usr/local/bin",
		} {
			if !strings.Contains(agent.body, want) {
				t.Errorf("%s does not carry %q", name, want)
			}
		}
	}

	// THE UPGRADE AGENT CARRIES THE SAME DRAIN GRACE AS THE SERVICES, because a
	// transaction that has booted the node out is waiting on that node's jobs.
	if !strings.Contains(deploy.UpgradeAgent, "<integer>88200</integer>") {
		t.Errorf("%s does not carry the 88200 second ExitTimeOut the drain needs",
			deploy.UpgradeAgentName)
	}
}

// requireScheduledPair holds what every oneshot-and-timer pair billet ships has
// to satisfy: the service is not itself enabled (a oneshot enabled at boot runs
// once per boot and reads like a schedule), the timer is what `systemctl enable`
// reaches, and the timer names the service.
func requireScheduledPair(t *testing.T, unitName, unit, timerName, timer string) {
	t.Helper()

	if strings.Contains(unit, "[Install]") {
		t.Errorf("%s has an [Install] section; the timer is what is enabled", unitName)
	}

	if !strings.Contains(timer, "[Install]") {
		t.Errorf("%s has no [Install] section, so `systemctl enable` cannot reach it", timerName)
	}

	if !strings.Contains(timer, "Unit="+unitName) {
		t.Errorf("%s does not name %s", timerName, unitName)
	}
}
