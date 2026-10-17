package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeending"
)

// stagedNodeEndings is a nodeEndingChecks a test decides: the invocation a
// stop reads first, the verdict its judgement returns, and the start gate's
// answer; it records what it was asked.
type stagedNodeEndings struct {
	invocation string
	beforeErr  error
	verdict    nodeending.Verdict
	admitErr   error
	judged     []string
	admitted   int
	// trace, when set, is the converger's, so a judgement's place among the
	// stops is asserted.
	trace *[]string
}

func (s *stagedNodeEndings) InvocationBeforeStop(context.Context) (string, error) {
	return s.invocation, s.beforeErr
}

func (s *stagedNodeEndings) JudgeStop(_ context.Context, _ *config.Config, invocation string, drain bool) nodeending.Judgement {
	s.judged = append(s.judged, invocation)
	if !drain {
		s.judged = append(s.judged, "no drain required")
	}

	if s.trace != nil {
		*s.trace = append(*s.trace, "judge "+invocation)
	}

	j := nodeending.Judgement{Verdict: s.verdict, Answer: s.verdict.String(), Why: "staged"}
	if s.verdict != nodeending.Proved {
		j.Acknowledge = "--invocation staged-newest"
	}

	return j
}

func (s *stagedNodeEndings) AdmitStart(context.Context, *config.Config) error {
	s.admitted++

	return s.admitErr
}

// onLinuxWith pins the platform and installs staged checks for one test.
func onLinuxWith(t *testing.T, s *stagedNodeEndings) {
	t.Helper()

	prevOS, prev := cli.HostOS, nodeEndings
	cli.HostOS = "linux"
	nodeEndings = func() nodeEndingChecks { return s }

	t.Cleanup(func() { cli.HostOS, nodeEndings = prevOS, prev })
}

// A `down` WHOSE NODE DID NOT DRAIN STOPS THERE: the node's stop is judged by
// the invocation read before it, with a drain required, and the server, whose
// stop tears down every lease its listener holds, is not stopped.
func TestDownStopsAtANodeThatDidNotDrain(t *testing.T) {
	asLinux(t)

	s := &stagedNodeEndings{invocation: "inv-9", verdict: nodeending.Unproved}
	onLinuxWith(t, s)

	cfgPath, stateDir := downConfig(t, true)
	_ = openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	capture(t, func() { err = runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath}) })

	if err == nil || !strings.Contains(err.Error(), "did not prove it drained") ||
		!strings.Contains(err.Error(), "acknowledge --invocation staged-newest") {
		t.Fatalf("down over a node that did not drain = %v, want the refusal naming the acknowledgement", err)
	}

	if !slices.Equal(s.judged, []string{"inv-9"}) {
		t.Errorf("the stop was judged as %q, want invocation inv-9 with a drain required", s.judged)
	}

	if slices.Contains(f.trace, "stop "+deploy.ServerUnitName) {
		t.Errorf("down went on to stop the server after a node that did not drain: %q", f.trace)
	}
}

// A `down` THAT CANNOT READ THE NODE'S INVOCATION STOPS NOTHING.
func TestDownThatCannotBindTheNodeStopStopsNothing(t *testing.T) {
	asLinux(t)

	onLinuxWith(t, &stagedNodeEndings{beforeErr: errors.New("systemd did not answer")})

	cfgPath, stateDir := downConfig(t, true)
	_ = openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	capture(t, func() { err = runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath}) })

	if err == nil || !strings.Contains(err.Error(), "systemd did not answer") {
		t.Fatalf("down = %v, want the invocation's refusal", err)
	}

	for _, verb := range f.trace {
		if strings.HasPrefix(verb, "stop ") {
			t.Errorf("down stopped %q without the node's invocation", verb)
		}
	}
}

// A `down` OVER A NODE THAT DRAINED GOES ON TO THE SERVER.
func TestDownOverADrainedNodeStopsTheServer(t *testing.T) {
	asLinux(t)

	s := &stagedNodeEndings{invocation: "inv-9", verdict: nodeending.Proved}
	onLinuxWith(t, s)

	cfgPath, stateDir := downConfig(t, true)
	_ = openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)
	s.trace = &f.trace

	var err error

	capture(t, func() { err = runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath}) })

	if err != nil {
		t.Fatalf("down over a drained node = %v", err)
	}

	node := slices.Index(f.trace, "stop "+deploy.NodeUnitName)
	judge := slices.Index(f.trace, "judge inv-9")
	server := slices.Index(f.trace, "stop "+deploy.ServerUnitName)

	if node < 0 || judge <= node || server <= judge || len(s.judged) != 1 {
		t.Errorf("down's trace %q and judgements %q, want the node stopped, then judged once, then the server "+
			"stopped", f.trace, s.judged)
	}
}

// A SECOND `down` OVER A NODE THE FIRST LEFT STOPPED UNPROVED IS REFUSED AGAIN:
// a node with no process is judged by what its last process left, and the
// server is not stopped.
func TestDownOverAStoppedNodeJudgesWhatItLeft(t *testing.T) {
	asLinux(t)

	s := &stagedNodeEndings{verdict: nodeending.Unproved}
	onLinuxWith(t, s)

	cfgPath, stateDir := downConfig(t, true)
	_ = openLedger(t, stateDir)

	f := stageDown(t, &fakeConverger{}, deploy.ServerUnitName, deploy.NodeUnitName)

	var err error

	capture(t, func() { err = runLocalDown(t.Context(), processEnv(), downOptions{configPath: cfgPath}) })

	if err == nil || !strings.Contains(err.Error(), "did not prove it drained") {
		t.Fatalf("a second down over a node stopped unproved = %v, want the refusal", err)
	}

	if !slices.Equal(s.judged, []string{""}) {
		t.Errorf("the stop was judged as %q, want what the last process left, with a drain required", s.judged)
	}

	if slices.Contains(f.trace, "stop "+deploy.ServerUnitName) {
		t.Errorf("down went on to stop the server: %q", f.trace)
	}
}

// `up` STARTS NO NODE OVER AN UNPROVED ENDING: the gate is asked before the
// node's start, and a refusal leaves the node unstarted.
func TestUpStartsNoNodeOverAnUnprovedEnding(t *testing.T) {
	s := &stagedNodeEndings{admitErr: errors.New("its last stop is unproved")}
	onLinuxWith(t, s)

	cfg := serviceConfig(t)
	f := stageUp(t, &fakeConverger{plan: bothUnits()}, githubVerified)

	err := runLocalUp(t.Context(), processEnv(), upOptions{configPath: cfg, servicePath: cfg})
	if err == nil || !strings.Contains(err.Error(), "its last stop is unproved") {
		t.Fatalf("up over an unproved ending = %v, want the gate's refusal", err)
	}

	if s.admitted != 1 {
		t.Errorf("the gate was asked %d times, want once", s.admitted)
	}

	if slices.Contains(f.trace, "start "+deploy.NodeUnitName) {
		t.Errorf("up started the node over an unproved ending: %q", f.trace)
	}
}
