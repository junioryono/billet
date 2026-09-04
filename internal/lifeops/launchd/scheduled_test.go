package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

const upgradeTarget = "gui/501/sh.billet.upgrade"

// upgradePrint renders a `launchctl print` reply for the upgrade agent.
func upgradePrint(fields ...string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s = {\n", upgradeTarget)

	for _, f := range fields {
		fmt.Fprintf(&b, "\t%s\n", f)
	}

	b.WriteString("}\n")

	return b.String()
}

// ENABLING A SCHEDULED AGENT INSTALLS IT, CLEARS ITS OVERRIDE AND LOADS IT NOW.
//
// Enable alone leaves the schedule for the next login; a Mac converged on
// Tuesday that acted on nothing until somebody logged out would read as a
// working schedule for a week. The bootstrap is what makes it live, and launchd
// holding the job afterwards is what proves it.
func TestEnableScheduledInstallsAndLoadsAnUnloadedAgent(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
		"enable":         {{}},
		"print": {
			// Not loaded before the bootstrap, loaded and idle after it.
			{code: notLoaded},
			{out: upgradePrint("state = waiting")},
		},
		"bootstrap": {{}},
	}}

	c := f.converger(t)
	label := deploy.UpgradeAgentLabel

	if err := c.EnableScheduled(t.Context(), label); err != nil {
		t.Fatalf("EnableScheduled: %v", err)
	}

	if _, err := os.Stat(filepath.Join(c.agentsDir, label+".plist")); err != nil {
		t.Errorf("the agent was not installed: %v", err)
	}

	want := "bootstrap gui/501 " + filepath.Join(c.agentsDir, label+".plist")
	if !slices.Contains(f.calls, want) {
		t.Errorf("the agent was not bootstrapped; launchctl was asked %v", f.calls)
	}

	for _, call := range f.calls {
		if strings.HasPrefix(call, "bootout") {
			t.Errorf("an unloaded agent was booted out first: %v", f.calls)
		}
	}
}

// A LOADED AGENT WITH A PROCESS IS LEFT ALONE. That process may be the upgrade
// transaction itself, midway through draining the node; booting it out to load
// a fresh copy of its plist would kill the updater.
func TestEnableScheduledLeavesARunningAgentAlone(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
		"enable":         {{}},
		"print":          {{out: upgradePrint("state = running", "pid = 7001")}},
	}}

	c := f.converger(t)

	if err := c.EnableScheduled(t.Context(), deploy.UpgradeAgentLabel); err != nil {
		t.Fatalf("EnableScheduled: %v", err)
	}

	for _, call := range f.calls {
		if strings.HasPrefix(call, "bootout") || strings.HasPrefix(call, "bootstrap") {
			t.Errorf("a running scheduled agent was disturbed: %v", f.calls)
		}
	}
}

// A LOADED, IDLE AGENT IS RELOADED, so a plist this build changed takes effect.
// launchd reads a plist once, at bootstrap, and nothing else refreshes what it
// holds.
func TestEnableScheduledReloadsAnIdleAgent(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
		"enable":         {{}},
		"print": {
			{out: upgradePrint("state = waiting")},
			{out: upgradePrint("state = waiting")},
		},
		"bootout":   {{}},
		"bootstrap": {{}},
	}}

	c := f.converger(t)
	label := deploy.UpgradeAgentLabel

	if err := c.EnableScheduled(t.Context(), label); err != nil {
		t.Fatalf("EnableScheduled: %v", err)
	}

	bootout := slices.Index(f.calls, "bootout "+upgradeTarget)
	bootstrap := slices.Index(f.calls, "bootstrap gui/501 "+filepath.Join(c.agentsDir, label+".plist"))

	if bootout < 0 || bootstrap < 0 || bootout > bootstrap {
		t.Errorf("an idle agent was not booted out and back in, in that order: %v", f.calls)
	}
}
