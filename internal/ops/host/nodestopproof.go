package host

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/nodeending"
)

// stopNodeProved stops the node unit and proves the stop by the stopped
// invocation's own ending (#127), not by systemd's exit: the node exits 0
// after a drain, a handoff, a drain a second signal cut short and a
// supersession alike, and reset-failed erases a timed-out stop's account. With
// drain, only a drain proves it. The invocation is read before the stop,
// because a clean stop clears systemd's. A node that was not running is
// reported and not stopped; the start after it is admitNodeStart's to judge.
func stopNodeProved(
	ctx context.Context, conv *lifeops.Converger, insp *lifeops.Inspector, cfg *config.Config, drain bool,
) (string, error) {
	invocation, err := invocationBeforeStop(ctx, insp)
	if err != nil {
		return "", err
	}

	if invocation == "" {
		return fmt.Sprintf("%s was not running", nodeUnit), nil
	}

	if _, err := conv.StopAndProve(ctx, nodeUnit); err != nil {
		return "", fmt.Errorf("stopping %s: %w", nodeUnit, err)
	}

	j := judgeNodeStop(ctx, insp, cfg, invocation, drain)
	if j.Verdict != nodeending.Proved {
		return "", fmt.Errorf("%s stopped, but its stop is %s: %s. The node is left stopped; once what it "+
			"left is known, `billet node ending acknowledge --invocation %s` permits a start", nodeUnit, j.Answer,
			j.Why, invocation)
	}

	return fmt.Sprintf("%s stopped, proved by %s: %s", nodeUnit, j.Proof, j.Why), nil
}

// invocationBeforeStop is the running node's invocation, read before its stop
// because a clean stop clears systemd's; empty when systemd positively reports
// no process, and an error when it could not say, or names no invocation for a
// process that runs.
func invocationBeforeStop(ctx context.Context, insp *lifeops.Inspector) (string, error) {
	before, err := insp.UnitProperties(ctx, nodeUnit, "ActiveState", "MainPID", "InvocationID")
	if err != nil {
		return "", fmt.Errorf("ask systemd about %s before stopping it: %w", nodeUnit, err)
	}

	unit := nodeending.Unit{ActiveState: firstProp(before, "ActiveState"), MainPID: firstProp(before, "MainPID")}
	switch v, why := unit.NoProcess(); v {
	case nodeending.Proved:
		return "", nil
	case nodeending.Unknown:
		return "", fmt.Errorf("before stopping %s: %s; nothing was stopped", nodeUnit, why)
	}

	invocation := firstProp(before, "InvocationID")
	if invocation == "" {
		return "", fmt.Errorf("systemd named no InvocationID for the running %s, so its stop cannot be bound "+
			"to its ending; nothing was stopped", nodeUnit)
	}

	return invocation, nil
}

// nodeEndingChecks is how `local down` and `local up` prove a node stop and
// gate a node start. A seam, because those commands' tests drive a recording
// converger with no systemd behind it; the real one asks the node unit.
type nodeEndingChecks interface {
	InvocationBeforeStop(ctx context.Context) (string, error)
	JudgeStop(ctx context.Context, cfg *config.Config, invocation string, drain bool) nodeending.Judgement
	AdmitStart(ctx context.Context, cfg *config.Config) error
}

type systemdNodeEndings struct{ insp *lifeops.Inspector }

func (s systemdNodeEndings) InvocationBeforeStop(ctx context.Context) (string, error) {
	return invocationBeforeStop(ctx, s.insp)
}

func (s systemdNodeEndings) JudgeStop(
	ctx context.Context, cfg *config.Config, invocation string, drain bool,
) nodeending.Judgement {
	return judgeNodeStop(ctx, s.insp, cfg, invocation, drain)
}

func (s systemdNodeEndings) AdmitStart(ctx context.Context, cfg *config.Config) error {
	return admitNodeStart(ctx, s.insp, cfg)
}

// nodeEndings builds the checks; TestMain points it at a fake that proves
// everything, and a test of the checks themselves installs its own.
var nodeEndings = func() nodeEndingChecks { return systemdNodeEndings{insp: lifeops.NewInspector()} }

// judgeNodeStop judges a stopped invocation against the record and what
// systemd says of the unit now, and records a stop only systemd proved.
func judgeNodeStop(
	ctx context.Context, insp *lifeops.Inspector, cfg *config.Config, invocation string, drain bool,
) nodeending.Judgement {
	after := observeNodeUnit(ctx, insp)

	rec, _, readErr := nodeending.Read(cfg.Node.StateDir)

	return recordSystemdStop(cfg, invocation, nodeending.Judge(rec, readErr, invocation, after, drain))
}

// recordSystemdStop writes down a stop only systemd proved, so the start after
// it, and anyone reading later, see it after reset-failed or the unit's garbage
// collection have erased systemd's own account. A stop that cannot be recorded
// is could-not-tell: the start gate would refuse it.
func recordSystemdStop(cfg *config.Config, invocation string, j nodeending.Judgement) nodeending.Judgement {
	if j.Verdict != nodeending.Proved || j.Proof != nodeending.ProofSystemd {
		return j
	}

	if err := nodeending.Observe(cfg.Node.StateDir, nodeending.Entry{
		InvocationID: invocation, BootID: nodeending.BootID(), Ending: nodeending.Stopped,
		RunsOnHost: cfg.Node.Provider.RunsOnHost(),
	}, nil); err != nil {
		return nodeending.Judgement{Verdict: nodeending.Unknown, Answer: nodeending.Unknown.String(),
			Why: "systemd reports a clean stop, but it could not be recorded for the start that follows: " +
				err.Error()}
	}

	return j
}

// admitNodeStart is the gate before every node start billet performs: the
// newest node ending, an acknowledgement, or a reboot since a host-run node
// admits it; anything else refuses, naming the acknowledgement.
func admitNodeStart(ctx context.Context, insp *lifeops.Inspector, cfg *config.Config) error {
	now := observeNodeUnit(ctx, insp)

	rec, _, readErr := nodeending.Read(cfg.Node.StateDir)

	j := nodeending.AdmitStart(rec, readErr, now, nodeending.BootID())
	if j.Verdict != nodeending.Proved {
		return fmt.Errorf("%s is not started: its last stop is %s: %s", nodeUnit, j.Answer, j.Why)
	}

	return nil
}

// observeNodeUnit is what a judgement reads of the node unit; a failed read
// leaves every field empty, which every judgement reads as could-not-tell.
func observeNodeUnit(ctx context.Context, insp *lifeops.Inspector) nodeending.Unit {
	props, err := insp.UnitProperties(ctx, nodeUnit, "ActiveState", "SubState", "Result", "MainPID", "Job")
	if err != nil {
		return nodeending.Unit{}
	}

	return nodeending.Unit{ActiveState: firstProp(props, "ActiveState"), SubState: firstProp(props, "SubState"),
		Result: firstProp(props, "Result"), MainPID: firstProp(props, "MainPID"), Job: firstProp(props, "Job")}
}
