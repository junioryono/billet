package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/nodeending"
)

// provedNodeEndings proves every stop and admits every start: TestMain's
// default, for the tests that are not about the node's ending.
type provedNodeEndings struct{}

func (provedNodeEndings) InvocationBeforeStop(context.Context) (string, error) {
	return "inv-test", nil
}

func (provedNodeEndings) JudgeStop(context.Context, *config.Config, string, bool) nodeending.Judgement {
	return nodeending.Judgement{Verdict: nodeending.Proved, Answer: "proved", Proof: nodeending.ProofRecord}
}

func (provedNodeEndings) AdmitStart(context.Context, *config.Config) error { return nil }

// stopProofHost is a systemd host whose node unit is a state file a fake
// systemctl answers from: `stop` replaces it with the state the test names
// for after the stop, as systemd would after the process ended its own way.
type stopProofHost struct {
	host     *systemdHost
	state    string
	after    string
	stateDir string
}

const runningUnit = "ActiveState=active\nSubState=running\nResult=success\nMainPID=5\nJob=\nInvocationID=inv-1\n"

func newStopProofHost(t *testing.T, after string) *stopProofHost {
	t.Helper()

	dir := t.TempDir()
	h := &stopProofHost{state: filepath.Join(dir, "state"), after: filepath.Join(dir, "after"),
		stateDir: filepath.Join(dir, "node")}

	if err := os.MkdirAll(h.stateDir, 0o750); err != nil {
		t.Fatal(err)
	}

	systemctl := filepath.Join(dir, "systemctl")
	writeFile(t, systemctl, `#!/bin/sh
case "$1" in
show) for a in "$@"; do case "$a" in --property=*) grep "^${a#--property=}=" "`+h.state+`";; esac; done ;;
stop) echo stop >>"`+h.state+`.log"; cp "`+h.after+`" "`+h.state+`" ;;
start) echo start >>"`+h.state+`.log"; exit 1 ;;
*) exit 0 ;;
esac
`, 0o755)

	writeFile(t, h.state, runningUnit, 0o644)
	writeFile(t, h.after, after, 0o644)

	insp := lifeops.NewInspector(lifeops.WithSystemctl(systemctl))
	cfg := &config.Config{Node: &config.NodeConfig{StateDir: h.stateDir, Provider: config.ProviderDocker}}
	h.host = &systemdHost{ledgerHost: ledgerHost{env: processEnv(), cfg: cfg}, inspect: insp,
		converge: lifeops.NewConverger(insp)}

	return h
}

func (h *stopProofHost) record(t *testing.T, ending nodeending.Ending) {
	t.Helper()

	e, err := nodeending.Begin(h.stateDir, nodeending.Entry{InvocationID: "inv-1", PID: 5, Role: nodeending.RoleNode},
		nil)
	if err != nil {
		t.Fatal(err)
	}

	if ending != nodeending.Running {
		if err := e.End(ending, false); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *stopProofHost) verbs(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(h.state + ".log")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(body))
}

const (
	cleanAfter    = "ActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\nJob=\nInvocationID=\n"
	timedOutAfter = "ActiveState=failed\nSubState=failed\nResult=timeout\nMainPID=0\nJob=\nInvocationID=inv-1\n"
)

// THE UPGRADE'S NODE STOP IS PROVED BY THE STOPPED INVOCATION'S ENDING: a
// drain passes; a node killed at TimeoutStopSec fails the step even though
// `systemctl stop` exited 0, and so does one reset-failed made look clean,
// each naming the acknowledgement; a handoff passes unless an operation asked
// for a drain.
func TestTheUpgradesNodeStopIsProvedByItsEnding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ending  nodeending.Ending
		after   string
		drain   bool
		wantErr string
	}{
		{"a drain", nodeending.Drained, cleanAfter, false, ""},
		{"killed at the timeout", nodeending.Running, timedOutAfter, false, "acknowledge --invocation inv-1"},
		{"killed, then reset-failed", nodeending.Running, cleanAfter, false, "killed or crashed"},
		{"a handoff", nodeending.HandedOff, cleanAfter, false, ""},
		{"a handoff where a drain was asked", nodeending.HandedOff, cleanAfter, true, "required a drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStopProofHost(t, tc.after)
			h.record(t, tc.ending)

			prev := nodeDrainRequestFile
			nodeDrainRequestFile = filepath.Join(t.TempDir(), "drain")

			t.Cleanup(func() { nodeDrainRequestFile = prev })

			if tc.drain {
				writeFile(t, nodeDrainRequestFile, "drain\n", 0o644)
			}

			err := h.host.StopNode(t.Context())

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("StopNode = %v, want a proved stop", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("StopNode = %v, want a refusal saying %q", err, tc.wantErr)
			}

			if h.verbs(t) != "stop" {
				t.Errorf("systemctl was asked %q, want the one stop", h.verbs(t))
			}
		})
	}
}

// THE UPGRADE STARTS THE NODE ONLY OVER A PROVED ENDING: a node whose last
// process was killed is not started, and the start is not even asked for; a
// drained one is.
func TestTheUpgradeStartsTheNodeOnlyOverAProvedEnding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ending    nodeending.Ending
		wantStart bool
	}{
		{"killed", nodeending.Running, false},
		{"drained", nodeending.Drained, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStopProofHost(t, cleanAfter)
			writeFile(t, h.state, cleanAfter, 0o644)
			h.record(t, tc.ending)

			err := h.host.StartServices(t.Context())

			started := h.verbs(t) == "start"
			if started != tc.wantStart {
				t.Fatalf("StartServices = %v and systemctl was asked %q; want a start asked %v", err, h.verbs(t),
					tc.wantStart)
			}

			if !tc.wantStart && (err == nil || !strings.Contains(err.Error(), "is not started")) {
				t.Errorf("StartServices over a killed node = %v, want the gate's refusal", err)
			}
		})
	}
}

// A NODE ALREADY STOPPED IS JUDGED BY WHAT ITS LAST PROCESS LEFT: a retried
// upgrade over a node an earlier attempt stopped unproved is refused again,
// with nothing stopped; one that drained passes.
func TestTheUpgradeJudgesANodeThatWasAlreadyStopped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ending  nodeending.Ending
		wantErr string
	}{
		{"killed", nodeending.Running, "acknowledge --invocation inv-1"},
		{"drained", nodeending.Drained, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStopProofHost(t, cleanAfter)
			writeFile(t, h.state, timedOutAfter, 0o644)
			h.record(t, tc.ending)

			err := h.host.StopNode(t.Context())

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("StopNode = %v, want what the drained node left proved", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("StopNode = %v, want a refusal saying %q", err, tc.wantErr)
			}

			if h.verbs(t) != "" {
				t.Errorf("systemctl was asked %q of a node with no process, want nothing", h.verbs(t))
			}
		})
	}
}

// A JOB SYSTEMD DID NOT ANSWER IS NOT NO JOB: an inactive node whose pending
// job is unknown may have a start queued, so nothing is stopped or judged.
func TestAnUnansweredJobIsNotNoJob(t *testing.T) {
	h := newStopProofHost(t, cleanAfter)
	writeFile(t, h.state, "ActiveState=inactive\nSubState=dead\nResult=success\nMainPID=0\nInvocationID=\n", 0o644)
	h.record(t, nodeending.Drained)

	if err := h.host.StopNode(t.Context()); err == nil || !strings.Contains(err.Error(), "did not answer Job") {
		t.Fatalf("StopNode with Job unanswered = %v, want could-not-tell", err)
	}
}

// A NODE ALREADY RUNNING IS NOT STARTED, SO THERE IS NOTHING TO ADMIT: a
// resumed upgrade whose services came up before it was interrupted is not
// refused by the gate.
func TestARunningNodeIsNotAdmittedAgain(t *testing.T) {
	h := newStopProofHost(t, cleanAfter)
	h.record(t, nodeending.Running)

	err := h.host.StartServices(t.Context())
	if err != nil && strings.Contains(err.Error(), "is not started") {
		t.Fatalf("StartServices over a running node = %v, want no gate", err)
	}

	if h.verbs(t) != "start" {
		t.Errorf("systemctl was asked %q, want the start that proves the running node", h.verbs(t))
	}
}

// THE CHECKS `local up` AND `local down` USE ASK THE UNIT AND THE RECORD: the
// adapter behind the seam TestMain replaces, driven through a systemctl.
func TestTheSystemdChecksAskTheUnitAndTheRecord(t *testing.T) {
	h := newStopProofHost(t, cleanAfter)
	writeFile(t, h.state, timedOutAfter, 0o644)
	h.record(t, nodeending.Running)

	checks := systemdNodeEndings{insp: h.host.inspect}
	cfg := h.host.cfg

	if inv, err := checks.InvocationBeforeStop(t.Context()); err != nil || inv != "" {
		t.Fatalf("InvocationBeforeStop of a stopped node = %q, %v; want none", inv, err)
	}

	if err := checks.AdmitStart(t.Context(), cfg); err == nil ||
		!strings.Contains(err.Error(), "acknowledge --invocation inv-1") {
		t.Fatalf("AdmitStart over a killed node = %v, want the refusal naming the acknowledgement", err)
	}

	if j := checks.JudgeStop(t.Context(), cfg, "", true); j.Verdict != nodeending.Unproved {
		t.Fatalf("JudgeStop of a node already stopped killed = %s (%s), want unproved", j.Answer, j.Why)
	}

	writeFile(t, h.state, runningUnit, 0o644)

	if inv, err := checks.InvocationBeforeStop(t.Context()); err != nil || inv != "inv-1" {
		t.Fatalf("InvocationBeforeStop of a running node = %q, %v; want inv-1", inv, err)
	}
}
