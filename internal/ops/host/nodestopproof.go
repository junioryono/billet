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
// because a clean stop clears systemd's. A node that was not running is judged
// by what its last process left, so a retry does not pass a stop an earlier
// attempt refused.
func stopNodeProved(
	ctx context.Context, conv *lifeops.Converger, insp *lifeops.Inspector, cfg *config.Config, drain bool,
) (string, error) {
	invocation, err := invocationBeforeStop(ctx, insp)
	if err != nil {
		return "", err
	}

	if invocation != "" {
		if _, err := conv.StopAndProve(ctx, nodeUnit); err != nil {
			return "", fmt.Errorf("stopping %s: %w", nodeUnit, err)
		}
	}

	j := judgeNodeStop(ctx, insp, cfg, invocation, drain)
	if j.Verdict != nodeending.Proved {
		return "", fmt.Errorf("%s is stopped, but its stop is %s: %s%s", nodeUnit, j.Answer, j.Why,
			acknowledgeRemedy(j))
	}

	if invocation == "" {
		return fmt.Sprintf("%s was not running; what it left is proved by %s: %s", nodeUnit, j.Proof, j.Why), nil
	}

	return fmt.Sprintf("%s stopped, proved by %s: %s", nodeUnit, j.Proof, j.Why), nil
}

// acknowledgeRemedy names the acknowledgement a refusal's judgement says
// would admit a start, and what it does not do.
func acknowledgeRemedy(j nodeending.Judgement) string {
	if j.Acknowledge == "" {
		return ""
	}

	return fmt.Sprintf(". Once what it left is known, `billet node ending acknowledge %s` permits a start, "+
		"and nothing that needs a drain: start the node so it adopts what was left, then stop it again to "+
		"drain", j.Acknowledge)
}

// nodeUnitProperties are what a judgement reads of the node unit.
var nodeUnitProperties = []string{"ActiveState", "SubState", "Result", "MainPID", "Job", "InvocationID"}

// nodeUnitFrom is the judgement's view of what systemd answered. Job's answer
// is empty when no job is pending, so whether systemd answered it at all is
// read from its presence.
func nodeUnitFrom(props map[string][]string) nodeending.Unit {
	_, jobAnswered := props["Job"]

	return nodeending.Unit{ActiveState: firstProp(props, "ActiveState"), SubState: firstProp(props, "SubState"),
		Result: firstProp(props, "Result"), MainPID: firstProp(props, "MainPID"), Job: firstProp(props, "Job"),
		JobAnswered: jobAnswered}
}

// invocationBeforeStop is the running node's invocation, read before its stop
// because a clean stop clears systemd's; empty when systemd positively reports
// no process and no pending job, and an error when it could not say, or names
// no invocation for a process that runs.
func invocationBeforeStop(ctx context.Context, insp *lifeops.Inspector) (string, error) {
	before, err := insp.UnitProperties(ctx, nodeUnit, nodeUnitProperties...)
	if err != nil {
		return "", fmt.Errorf("ask systemd about %s before stopping it: %w", nodeUnit, err)
	}

	switch v, why := nodeUnitFrom(before).NoProcess(); v {
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
	// JudgeStop judges the stop of invocation, or, with none, what the last
	// node process left a unit that had no process to stop.
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
// systemd says of the unit, and records a stop only systemd spoke for, all under
// the record's lock (nodeending.JudgeStop). With no invocation, nothing was
// running to stop, and what the newest node process left is judged instead.
func judgeNodeStop(
	ctx context.Context, insp *lifeops.Inspector, cfg *config.Config, invocation string, drain bool,
) nodeending.Judgement {
	if invocation == "" {
		return nodeending.JudgeStanding(cfg.Node.StateDir, drain,
			func() nodeending.Unit { return observeNodeUnit(ctx, insp) })
	}

	j, _ := nodeending.JudgeStop(cfg.Node.StateDir, invocation, drain, cfg.Node.Provider.RunsOnHost(),
		func() nodeending.Unit { return observeNodeUnit(ctx, insp) }, nil)

	return j
}

// admitNodeStart is the gate before every node start billet performs: what the
// node processes left (a drain, a handoff, an acknowledgement, or a reboot
// since a host-run node) admits it; anything else refuses, naming the
// acknowledgement. A node already running is not started, so there is nothing
// to admit: a resumed upgrade proves it as it proves any running service.
func admitNodeStart(ctx context.Context, insp *lifeops.Inspector, cfg *config.Config) error {
	// A NODE ALREADY RUNNING IS ASKED ABOUT FIRST, without the record's lock,
	// since nothing is started and a state directory it cannot write must not
	// refuse it; anything else is judged afresh under the lock.
	if running(observeNodeUnit(ctx, insp)) {
		return nil
	}

	var now nodeending.Unit

	j := nodeending.JudgeStanding(cfg.Node.StateDir, false, func() nodeending.Unit {
		now = observeNodeUnit(ctx, insp)

		return now
	})

	if running(now) {
		return nil
	}

	if j.Verdict != nodeending.Proved {
		return fmt.Errorf("%s is not started: what its last process left is %s: %s%s", nodeUnit, j.Answer,
			j.Why, acknowledgeRemedy(j))
	}

	return nil
}

// running says systemd reports the unit up or coming up, so a start of it
// starts nothing.
func running(u nodeending.Unit) bool {
	switch u.ActiveState {
	case "active", "activating", "reloading":
		return true
	}

	return false
}

// observeNodeUnit is what a judgement reads of the node unit; a failed read
// leaves every field unanswered, which every judgement reads as could-not-tell.
func observeNodeUnit(ctx context.Context, insp *lifeops.Inspector) nodeending.Unit {
	props, err := insp.UnitProperties(ctx, nodeUnit, nodeUnitProperties...)
	if err != nil {
		return nodeending.Unit{}
	}

	return nodeUnitFrom(props)
}
