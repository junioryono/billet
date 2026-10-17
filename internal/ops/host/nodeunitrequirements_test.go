package host

import (
	"bytes"
	"go/ast"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
)

// requirementsSystemctl is a systemctl that answers the node unit's properties
// from a file, each only when asked for, and nothing about any other unit.
func requirementsSystemctl(t *testing.T, unit string) *lifeops.Inspector {
	t.Helper()

	dir := t.TempDir()
	properties := filepath.Join(dir, "node.properties")
	writeFile(t, properties, unit, 0o644)

	systemctl := filepath.Join(dir, "systemctl")
	writeFile(t, systemctl, `#!/bin/sh
case " $* " in *" billet-node.service "*) ;; *) exit 0 ;; esac
for a in "$@"; do case "$a" in --property=*) grep "^${a#--property=}=" "`+properties+`";; esac; done
exit 0
`, 0o755)

	return lifeops.NewInspector(lifeops.WithSystemctl(systemctl))
}

const (
	legacyNodeUnit  = "LoadState=loaded\nRuntimeDirectory=billet/locks\n"
	currentNodeUnit = "LoadState=loaded\nRuntimeDirectory=billet/locks billet/registration\n"
)

// nodeConfig is a configuration with a node section, which is all the check
// reads of one.
var nodeConfig = &config.Config{Node: &config.NodeConfig{}}

// A RELEASE NEEDS WHAT THE TABLE SAYS IT DOES: the record's directory from the
// release that first published the record, in either spelling of a release;
// nothing before; and everything for a release whose order cannot be told.
func TestAReleaseNeedsWhatTheTableSays(t *testing.T) {
	registration := []string{deploy.NodeRegistrationDirectory}

	for release, want := range map[string][]string{
		"v0.10.9": nil,
		"v0.11.0": registration,
		"0.11.0":  registration,
		"v0.17.2": registration,
		"(devel)": registration,
	} {
		if got := nodeUnitRequirementsOf(release); !slices.Equal(got, want) {
			t.Errorf("%s needs %q, want %q", release, got, want)
		}
	}
}

// A CANDIDATE IS HELD TO WHAT IT NEEDS OF THE NODE UNIT (#168): a loaded unit
// that lacks a required runtime directory refuses naming the preparation; one
// that has it passes; a release before the record needs nothing; a host with
// no node, or whose node unit is absent or masked, holds nothing; and anything
// systemd did not answer is an error, never a pass.
func TestTheCandidateIsHeldToWhatItNeedsOfTheNodeUnit(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, unit string
		cfg                   *config.Config
		refused               string // a clause of the refusal, or "" for a pass
		found                 string // a clause of the pass's report
	}{
		{"the unit from before", "v0.17.0", legacyNodeUnit, nodeConfig,
			"v0.17.0 needs billet-node.service to provide RuntimeDirectory=billet/registration", ""},
		{"the current unit", "v0.17.0", currentNodeUnit, nodeConfig, "", "provides everything v0.17.0 needs"},
		{"a release before the record", "v0.10.0", legacyNodeUnit, nodeConfig, "", "needs nothing"},
		{"a release whose order cannot be told", "(devel)", legacyNodeUnit, nodeConfig,
			"(devel) needs billet-node.service", ""},
		{"a host that runs no node", "v0.17.0", legacyNodeUnit, &config.Config{}, "", "runs no node"},
		{"no node unit", "v0.17.0", "LoadState=not-found\nRuntimeDirectory=\n", nodeConfig, "", "is not-found"},
		{"a masked node unit", "v0.17.0", "LoadState=masked\nRuntimeDirectory=\n", nodeConfig, "", "is masked"},
		{"RuntimeDirectory unanswered", "v0.17.0", "LoadState=loaded\n", nodeConfig, "answered no RuntimeDirectory", ""},
		{"a unit in another state", "v0.17.0", "LoadState=error\nRuntimeDirectory=\n", nodeConfig, "LoadState=error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			insp := requirementsSystemctl(t, tc.unit)

			found, err := checkUnitRequirements(t.Context(), tc.cfg, tc.candidate, insp)

			switch {
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("checked to %q, %v; want a refusal saying %q", found, err, tc.refused)
			case tc.refused == "" && (err != nil || !strings.Contains(found, tc.found)):
				t.Fatalf("checked to %q, %v; want a pass saying %q", found, err, tc.found)
			}

			if tc.name == "the unit from before" && !strings.Contains(err.Error(), "converge the host role, or install the package") {
				t.Errorf("the refusal %q does not name the preparation", err)
			}
		})
	}
}

// THE PREFLIGHT ASKS SYSTEMD ONLY WHERE IT RUNS: a Mac's node is a launch agent
// with no registration record, and is never refused for a unit it does not
// have.
func TestThePreflightAsksOnlyASystemdHost(t *testing.T) {
	insp := requirementsSystemctl(t, legacyNodeUnit)

	prevInspector, prevOS := unitRequirementsInspector, cli.HostOS
	unitRequirementsInspector = func() *lifeops.Inspector { return insp }

	t.Cleanup(func() { unitRequirementsInspector, cli.HostOS = prevInspector, prevOS })

	var out bytes.Buffer

	env := processEnv()
	env.Stdout = &out

	cli.HostOS = "darwin"

	if err := refuseUnmetUnitRequirements(t.Context(), env, nodeConfig, "v0.17.0"); err != nil {
		t.Fatalf("a Mac was refused for a systemd unit: %v", err)
	}

	cli.HostOS = "linux"

	if err := refuseUnmetUnitRequirements(t.Context(), env, nodeConfig, "v0.17.0"); err == nil ||
		!strings.Contains(err.Error(), "the loaded unit does not") {
		t.Fatalf("a systemd host whose unit lacks the directory was admitted: %v", err)
	}
}

// BEFORE THE CLAIM AND BEFORE THE JOB IS TAKEN: a refusal after the node is told
// the job was accepted leaves the rollout waiting on a host that never moves,
// and one after the claim leaves a claim to give back. Structural, because the
// path needs a signed release to reach.
func TestTheUnitRequirementsAreJudgedBeforeTheClaim(t *testing.T) {
	var order []string

	ast.Inspect(findFunc(t, "actOnResolved").Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch name := calleeName(call); name {
		case "checkDowngrade", "refuseUnmetUnitRequirements", "stageClaim", "accept":
			order = append(order, name)
		}

		return true
	})

	at := func(name string) int { return slices.Index(order, name) }

	if at("refuseUnmetUnitRequirements") < 0 || at("refuseUnmetUnitRequirements") < at("checkDowngrade") ||
		at("refuseUnmetUnitRequirements") > at("stageClaim") || at("refuseUnmetUnitRequirements") > at("accept") {
		t.Errorf("actOnResolved calls %q; the unit requirements belong after the downgrade check and before every "+
			"claim and acceptance", order)
	}
}
