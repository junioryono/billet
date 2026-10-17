package host

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/releasesource"
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
	legacyNodeUnit = "LoadState=loaded\nFragmentPath=/etc/systemd/system/billet-node.service\n" +
		"RuntimeDirectory=billet/locks\n"
	packagedLegacyNodeUnit = "LoadState=loaded\nFragmentPath=/usr/lib/systemd/system/billet-node.service\n" +
		"RuntimeDirectory=billet/locks\n"
	currentNodeUnit = "LoadState=loaded\nFragmentPath=/etc/systemd/system/billet-node.service\n" +
		"RuntimeDirectory=billet/locks billet/registration\n"
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
// no node holds nothing; a configured node whose unit is absent or masked
// refuses, since the transaction would start it; and anything systemd did not
// answer is an error, never a pass.
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
		{"no node unit", "v0.17.0", "LoadState=not-found\nFragmentPath=\nRuntimeDirectory=\n", nodeConfig,
			"billet-node.service is not-found, so the upgrade would start a unit systemd cannot start", ""},
		{"a masked node unit", "v0.17.0", "LoadState=masked\nFragmentPath=\nRuntimeDirectory=\n", nodeConfig,
			"billet-node.service is masked", ""},
		{"RuntimeDirectory unanswered", "v0.17.0", "LoadState=loaded\nFragmentPath=/etc/x\n", nodeConfig,
			"answered no RuntimeDirectory", ""},
		{"FragmentPath unanswered", "v0.17.0", "LoadState=loaded\nRuntimeDirectory=billet/locks\n", nodeConfig,
			"answered no FragmentPath", ""},
		{"a unit in another state", "v0.17.0", "LoadState=error\nFragmentPath=\nRuntimeDirectory=\n", nodeConfig,
			"LoadState=error", ""},
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
		})
	}
}

// THE REMEDY IS THE ONE THAT CHANGES THE UNIT IN EFFECT: a unit under /etc
// overrides the packaged one, so a package install leaves it as it was, and the
// refusal names the converge's repair and the manual procedure instead; a
// packaged unit is replaced by this release's package.
func TestTheRefusalNamesTheRemedyForTheUnitInEffect(t *testing.T) {
	for _, tc := range []struct {
		name, unit   string
		want, absent []string
	}{
		{"a unit the role rendered", legacyNodeUnit,
			[]string{"(/etc/systemd/system/billet-node.service)", "converge the host role, which repairs that unit",
				"the manual repair in docs/operating/upgrades.md", "installing the package does not help"},
			[]string{"install this release's package"}},
		{"a packaged unit", packagedLegacyNodeUnit,
			[]string{"(/usr/lib/systemd/system/billet-node.service)", "install this release's package"},
			[]string{"installing the package does not help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkUnitRequirements(t.Context(), nodeConfig, "v0.17.0", requirementsSystemctl(t, tc.unit))
			if err == nil {
				t.Fatal("a unit that lacks the directory was admitted")
			}

			for _, clause := range tc.want {
				if !strings.Contains(err.Error(), clause) {
					t.Errorf("the refusal %q does not say %q", err, clause)
				}
			}

			for _, clause := range tc.absent {
				if strings.Contains(err.Error(), clause) {
					t.Errorf("the refusal %q says %q, which is not this unit's remedy", err, clause)
				}
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
		!strings.Contains(err.Error(), "the loaded unit (/etc/systemd/system/billet-node.service) does not") {
		t.Fatalf("a systemd host whose unit lacks the directory was admitted: %v", err)
	}
}

// onALegacyLinuxNode makes this a systemd host whose loaded node unit lacks the
// record's directory, running release, with no provenance record unless the
// test writes one.
func onALegacyLinuxNode(t *testing.T, release string) {
	t.Helper()

	withUpgradeRoot(t)

	insp := requirementsSystemctl(t, legacyNodeUnit)

	prevInspector, prevOS, prevRelease, prevPath := unitRequirementsInspector, cli.HostOS, runningRelease,
		provenance.Path
	unitRequirementsInspector = func() *lifeops.Inspector { return insp }
	cli.HostOS = "linux"
	runningRelease = func() string { return release }
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")

	t.Cleanup(func() {
		unitRequirementsInspector, cli.HostOS, runningRelease, provenance.Path = prevInspector, prevOS, prevRelease,
			prevPath
	})
}

// candidateManifest is this build's manifest named as v0.17.0, a release that
// needs the record's directory, publishing for the machine the test runs on
// whatever cli.HostOS was set to.
func candidateManifest(t *testing.T) *releasesource.Manifest {
	t.Helper()

	m := installedManifest(t)
	m.Version = "v0.17.0"
	m.Artifacts[0].OS = runtime.GOOS
	m.Artifacts[0].Name = "billet_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"

	return m
}

// noClaim fails when anything under the upgrade root is a transaction's.
func noClaim(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(upgradeRoot)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upgrade-") || e.Name() == activePointer {
			t.Errorf("the refused upgrade left %s under the upgrade root", e.Name())
		}
	}
}

// AN UPGRADE THE NODE UNIT CANNOT RUN IS REFUSED BEFORE THE CLAIM AND BEFORE THE
// JOB IS TAKEN (#168): the node reads the refusal through the caller's own
// bookkeeping, so the rollout backs off; nothing is claimed; and the decision
// mark does not move, so the same decision is carried out once a converge has
// installed the unit.
func TestAnUpgradeTheNodeUnitCannotRunIsRefusedBeforeTheClaim(t *testing.T) {
	onALegacyLinuxNode(t, "v0.16.0")

	ack, answer := ackReader(t)

	err := actOnResolved(t.Context(), processEnv(), nodeConfig, "", hostUpgradeTarget{generation: 10},
		ack, nil, candidateManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err == nil || !strings.Contains(err.Error(), "the loaded unit (/etc/systemd/system/billet-node.service) does not") {
		t.Fatalf("an upgrade the node unit cannot run returned %v, want the unit requirement's refusal", err)
	}

	ack.refuse(err)

	if got := answer(); !strings.HasPrefix(got, node.AckRefused) ||
		!strings.Contains(got, "RuntimeDirectory=billet/registration") {
		t.Errorf("the node was answered %q, want a refusal naming the requirement", got)
	}

	noClaim(t)

	if mark, err := readDecision(); err != nil || mark != 0 {
		t.Errorf("the refused upgrade moved the decision mark to %d (err %v), want it unmoved", mark, err)
	}
}

// THE CLAIM IS NEVER ATTEMPTED: with another transaction's claim published, the
// answer is still the unit's refusal rather than the claim's, so the judgement
// ran first; a claim taken and given back would have met the other one.
func TestTheUnitRequirementIsJudgedBeforeAClaimIsAttempted(t *testing.T) {
	onALegacyLinuxNode(t, "v0.16.0")

	tx := heldTxLock(t)

	other := filepath.Join(upgradeRoot, "upgrade-other")
	if err := os.Symlink(other, filepath.Join(upgradeRoot, activePointer)); err != nil {
		t.Fatal(err)
	}

	err := actOnResolved(t.Context(), processEnv(), nodeConfig, "", hostUpgradeTarget{generation: 10},
		newUpgradeAck(""), nil, candidateManifest(t), strings.Repeat("b", 64), tx)
	if err == nil || !strings.Contains(err.Error(), "RuntimeDirectory=billet/registration") ||
		strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("the upgrade returned %v, want the unit requirement's refusal and nothing of the claim", err)
	}

	if got, err := os.Readlink(filepath.Join(upgradeRoot, activePointer)); err != nil || got != other {
		t.Errorf("the other transaction's claim is %q (err %v), want it untouched", got, err)
	}
}

// A HOST ALREADY ON THE TARGET INSTALLS NOTHING, so what its unit lacks does not
// stop it settling: it accepts and raises the mark as any no-op does, or the
// coordinator would be told a host already where it was sent refused.
func TestAHostAlreadyOnTheTargetSettlesWhateverItsUnitLacks(t *testing.T) {
	onALegacyLinuxNode(t, "v0.17.0")

	ack, answer := ackReader(t)

	err := actOnResolved(t.Context(), processEnv(), nodeConfig, "", hostUpgradeTarget{generation: 10},
		ack, nil, candidateManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err != nil {
		t.Fatalf("a host already on the target was refused: %v", err)
	}

	if got := answer(); got != node.AckAccepted {
		t.Errorf("the host answered %q, want an acceptance", got)
	}

	if mark, err := readDecision(); err != nil || mark != 10 {
		t.Errorf("the decision mark is %d (err %v) after the no-op, want 10", mark, err)
	}
}

// A TIMER ANOTHER TRANSACTION OVERTOOK settles on what that transaction
// installed, whatever the unit lacks: it installs nothing either.
func TestAnOvertakenTimerSettlesWhateverItsUnitLacks(t *testing.T) {
	onALegacyLinuxNode(t, "v0.16.0")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	sum, err := provenance.HashFile(exe)
	if err != nil {
		t.Fatal(err)
	}

	digest := strings.Repeat("5", 64)
	if err := provenance.Write(provenance.Record{
		Version: "v0.17.0", ManifestDigest: digest, BinarySHA256: sum,
	}); err != nil {
		t.Fatal(err)
	}

	ack, answer := ackReader(t)

	err = actOnResolved(t.Context(), processEnv(), nodeConfig, "",
		hostUpgradeTarget{pin: "v0.17.0", digest: digest, generation: 12, fromRollout: true},
		ack, nil, candidateManifest(t), digest, heldTxLock(t))
	if err != nil {
		t.Fatalf("an overtaken timer was refused: %v", err)
	}

	if got := answer(); got != node.AckAccepted {
		t.Errorf("the overtaken timer answered %q, want an acceptance", got)
	}

	if got, err := readSettled(); err != nil || got != 12 {
		t.Errorf("the settled mark is %d (err %v), want 12", got, err)
	}
}
