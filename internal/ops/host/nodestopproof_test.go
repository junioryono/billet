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
