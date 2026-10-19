package host

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeending"
)

// `billet node ending` reads and judges the node's own record of how each of
// its processes ended (#127): `show` prints it, `check` says whether a stop of
// one invocation is proved, `admit-start` whether a node may be started, and
// `acknowledge` accepts an unproved stop so a start may follow. The shell and
// the role read the exit status: 0 proved, 2 unproved or refused, 3
// could-not-tell.

// endingSchema is the answers' format.
const endingSchema = 1

// endingAnswer is every answer but show's.
type endingAnswer struct {
	Schema  int    `json:"schema"`
	Command string `json:"command"`
	Outcome string `json:"outcome"`
	Proof   string `json:"proof,omitempty"`
	Why     string `json:"why"`
	// Acknowledge is the `billet node ending acknowledge` arguments that would
	// admit a start over an unproved or unknown answer the record decided.
	Acknowledge string                `json:"acknowledge,omitempty"`
	Invocation  string                `json:"invocation,omitempty"`
	Drain       bool                  `json:"drain,omitempty"`
	Unit        *endingUnitAnswer     `json:"unit,omitempty"`
	Entry       *nodeending.Entry     `json:"entry,omitempty"`
	Record      *nodeending.Record    `json:"record,omitempty"`
	Recorded    *bool                 `json:"recorded,omitempty"`
	judgement   *nodeending.Judgement `json:"-"`
}

// endingUnitAnswer is what systemd said of the node unit.
type endingUnitAnswer struct {
	ActiveState  string `json:"active_state"`
	SubState     string `json:"sub_state"`
	Result       string `json:"result"`
	MainPID      string `json:"main_pid"`
	Job          string `json:"job"`
	InvocationID string `json:"invocation_id"`
}

// Seams a test replaces: the unit's observer, the root check and the
// deployment claim acknowledge holds.
var (
	endingInspector = endpointInspector
	endingEUID      = os.Geteuid
	endingClaim     = func(cfg *config.Config) (func() error, error) {
		bundle, err := app.NodeBundle(cfg)
		if err != nil {
			return nil, err
		}

		_, lock, err := app.ClaimNodeDeployment(cfg, bundle)
		if err != nil {
			return nil, err
		}

		return lock.Release, nil
	}
)

// NodeEnding is `billet node ending`.
func NodeEnding(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("billet node ending: want show, check, admit-start or acknowledge")
	}

	switch args[0] {
	case "show":
		return endingShow(env, args[1:])
	case "check":
		return endingCheck(ctx, env, args[1:])
	case "admit-start":
		return endingAdmitStart(ctx, env, args[1:])
	case "acknowledge":
		return endingAcknowledge(ctx, env, args[1:])
	default:
		return fmt.Errorf("billet node ending: unknown command %q; want show, check, admit-start or acknowledge",
			args[0])
	}
}

// endingTarget is the record a subcommand reads and the node it belongs to.
type endingTarget struct {
	cfg     *config.Config
	cfgPath string
	// stateDir is --state-dir, the configuration's node.state_dir, or, where
	// the configuration names no node, the packaged one.
	stateDir string
	// runsOnHost is the configured provider's, and only for its own record: a
	// record named any other way says nothing of what compute survives a
	// reboot, so it asserts none.
	runsOnHost bool
}

// endingPackagedStateDir is the packaged configuration's node.state_dir, read
// where a configuration names no node: a node the package ran under it may
// have left a record there however the configuration has been edited since.
var endingPackagedStateDir = "/var/lib/billet/node"

// endingStateDirFlag adds --state-dir to a subcommand that reads a record
// another configuration may have named.
func endingStateDirFlag(fs *flag.FlagSet) *string {
	return fs.String("state-dir", "", "the node state directory whose record to read, in place of the "+
		"configuration's node.state_dir: a converge that has already rendered the node's configuration away "+
		"names the one its node used")
}

// endingFlags adds --config and --json to one subcommand's flag set, parses
// it, and reads the configuration; stateDir is its --state-dir, or nil for a
// subcommand that reads only the configuration's own record.
func endingFlags(fs *flag.FlagSet, args []string, stateDir *string) (endingTarget, error) {
	cfgPath := cli.AddConfigFlag(fs)
	if stateDir == nil {
		stateDir = new(string)
	}

	asJSON := fs.Bool("json", false, "print the answer as JSON (the only form)")

	if err := cli.Parse(fs, args); err != nil {
		return endingTarget{}, err
	}

	if !*asJSON {
		return endingTarget{}, fmt.Errorf("%s answers as JSON; pass --json", fs.Name())
	}

	cfg, err := readEndingConfig(*cfgPath)
	if err != nil {
		return endingTarget{}, err
	}

	t := endingTarget{cfg: cfg, cfgPath: *cfgPath, stateDir: *stateDir}

	switch {
	case cfg.Node != nil && (t.stateDir == "" || t.stateDir == cfg.Node.StateDir):
		t.stateDir = cfg.Node.StateDir
		t.runsOnHost = cfg.Node.Provider.RunsOnHost()
	case cfg.Node == nil && t.stateDir == "":
		t.stateDir = endingPackagedStateDir
	}

	return t, nil
}

// readEndingConfig reads the node section alone: the record is judged on a
// host whose configuration the loader as a whole would refuse (the package's
// template before it is filled in, which a removal still has to judge), and
// a configuration that is positively absent names no node.
func readEndingConfig(path string) (*config.Config, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &config.Config{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	return parseConfiguration(path, body, false)
}

func endingShow(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet node ending show", env.Stdout)

	target, err := endingFlags(fs, args, endingStateDirFlag(fs))
	if err != nil {
		return err
	}

	rec, present, err := nodeending.Read(target.stateDir)
	if err != nil {
		return answerEnding(env, endingAnswer{Command: "show", Outcome: nodeending.Unknown.String(),
			Why: err.Error()})
	}

	why := "the node's ending record"
	if !present {
		why = "no ending is recorded on this host"
	}

	return answerEnding(env, endingAnswer{Command: "show", Outcome: "shown", Why: why, Record: &rec})
}

func endingCheck(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet node ending check", env.Stdout)
	invocation := fs.String("invocation", "", "the node unit's InvocationID, read before the stop; with none, a "+
		"node that had no process to stop is judged by what its last process left")
	drain := fs.Bool("drain", false, "only a drain proves the stop")

	target, err := endingFlags(fs, args, endingStateDirFlag(fs))
	if err != nil {
		return err
	}

	if r := endingPlatform("check"); r != nil {
		return answerEnding(env, *r)
	}

	var observed *endingUnitAnswer

	sample := func() nodeending.Unit {
		var unit nodeending.Unit

		unit, observed = observeEndingUnit(ctx)

		return unit
	}

	// A NODE THAT HAD NO PROCESS TO STOP, asked about by a retry or by an
	// operation that found it already down: what its last process left
	// decides, at the stop's own standard.
	if *invocation == "" {
		j := nodeending.JudgeStanding(target.stateDir, *drain, sample)

		return answerEnding(env, endingAnswer{Command: "check", Drain: *drain, Unit: observed, judgement: &j})
	}

	j, recorded := nodeending.JudgeStop(target.stateDir, *invocation, *drain, target.runsOnHost, sample, nil)

	answer := endingAnswer{Command: "check", Invocation: *invocation, Drain: *drain, Unit: observed, judgement: &j}

	if j.Floor {
		answer.Recorded = &recorded
	}

	return answerEnding(env, answer)
}

func endingAdmitStart(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet node ending admit-start", env.Stdout)

	target, err := endingFlags(fs, args, endingStateDirFlag(fs))
	if err != nil {
		return err
	}

	if r := endingPlatform("admit-start"); r != nil {
		return answerEnding(env, *r)
	}

	var observed *endingUnitAnswer

	j := admitStart(target.stateDir, func() nodeending.Unit {
		var unit nodeending.Unit

		unit, observed = observeEndingUnit(ctx)

		return unit
	})

	// Only the sample the answer rests on: one discarded because the lock was
	// then refused is not evidence of anything.
	if j.Sampled == (nodeending.Unit{}) {
		observed = nil
	}

	return answerEnding(env, endingAnswer{Command: "admit-start", Unit: observed, judgement: &j})
}

func endingAcknowledge(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet node ending acknowledge", env.Stdout)
	invocation := fs.String("invocation", "", "the newest stopped invocation, exactly as the record names it")
	unrecorded := fs.Bool("unrecorded", false, "acknowledge a stop no entry covers")
	resetUnreadable := fs.Bool("reset-unreadable", false,
		"set aside a record that cannot be read and acknowledge the stop it would have judged")

	target, err := endingFlags(fs, args, nil)
	if err != nil {
		return err
	}

	// THE NODE'S OWN LOCK IS TAKEN THROUGH ITS CONFIGURATION, so the record
	// acknowledged is that configuration's, never another directory's.
	cfg := target.cfg
	if cfg.Node == nil {
		return fmt.Errorf("%s has no node section; an acknowledgement takes the node's own lock", target.cfgPath)
	}

	chosen := 0

	for _, set := range []bool{*invocation != "", *unrecorded, *resetUnreadable} {
		if set {
			chosen++
		}
	}

	if chosen != 1 {
		return errors.New("billet node ending acknowledge: pass exactly one of --invocation, --unrecorded " +
			"and --reset-unreadable")
	}

	refuse := func(why string) error {
		return answerEnding(env, endingAnswer{Command: "acknowledge", Invocation: *invocation,
			judgement: &nodeending.Judgement{Verdict: nodeending.Unproved, Answer: nodeending.Unproved.String(),
				Why: why}})
	}

	if r := endingPlatform("acknowledge"); r != nil {
		return answerEnding(env, *r)
	}

	if endingEUID() != 0 {
		return refuse("acknowledging a stop is root's: the record is root's evidence")
	}

	// THE NODE'S OWN LOCK, which no node process holds once this is taken: an
	// acknowledgement is of a stop, and a running node has not stopped.
	release, err := endingClaim(cfg)
	if err != nil {
		return refuse("the node's deployment lock could not be taken, so a node process " +
			"may hold this host's identity: " + err.Error())
	}

	//nolint:errcheck // the answer is JSON alone and has no place for a warning, and the process's exit releases the flock whatever this returns.
	defer func() { _ = release() }()

	unit, observed := observeEndingUnit(ctx)
	if v, why := unit.NoProcess(); v != nodeending.Proved {
		answer := endingAnswer{Command: "acknowledge", Invocation: *invocation, Unit: observed,
			judgement: &nodeending.Judgement{Verdict: v, Answer: v.String(), Why: why}}

		return answerEnding(env, answer)
	}

	why := "acknowledged: a start may follow; a stop that needed a drain is still not proved by this"

	var e nodeending.Entry

	if *resetUnreadable {
		var aside string

		aside, e, err = nodeending.Reset(cfg.Node.StateDir, nil)
		if err != nil {
			return refuse(err.Error())
		}

		why = fmt.Sprintf("the unreadable record was set aside as %s and the stop it would have judged is %s",
			aside, why)
	} else {
		e, err = nodeending.Acknowledge(cfg.Node.StateDir, *invocation, *unrecorded, nil)
		if err != nil {
			return refuse(err.Error())
		}
	}

	j := nodeending.Judgement{Verdict: nodeending.Proved, Answer: nodeending.Proved.String(),
		Proof: nodeending.ProofAcknowledged, Entry: &e, Why: why}

	return answerEnding(env, endingAnswer{Command: "acknowledge", Invocation: *invocation, Unit: observed,
		judgement: &j})
}

// endingPlatform refuses the judgements off systemd.
func endingPlatform(command string) *endingAnswer {
	if cli.HostOS == "linux" {
		return nil
	}

	return &endingAnswer{Command: command, judgement: &nodeending.Judgement{Verdict: nodeending.Unproved,
		Answer: nodeending.Unproved.String(), Why: "the node's stops are judged against systemd, and this " +
			"platform has none; a Mac's launch agent is a follow-up"}}
}

// observeEndingUnit asks systemd about the node unit. A failed read leaves
// every field unanswered, which every judgement reads as could-not-tell.
func observeEndingUnit(ctx context.Context) (nodeending.Unit, *endingUnitAnswer) {
	props, err := endingInspector().UnitProperties(ctx, nodeUnit, nodeUnitProperties...)
	if err != nil {
		return nodeending.Unit{}, nil
	}

	u := nodeUnitFrom(props)

	return u, &endingUnitAnswer{ActiveState: u.ActiveState, SubState: u.SubState, Result: u.Result,
		MainPID: u.MainPID, Job: u.Job, InvocationID: firstProp(props, "InvocationID")}
}

// answerEnding prints an answer and exits by its verdict.
func answerEnding(env cli.Env, a endingAnswer) error {
	a.Schema = endingSchema

	code := 0

	if a.judgement != nil {
		a.Outcome, a.Proof, a.Why = a.judgement.Answer, a.judgement.Proof, a.judgement.Why
		a.Acknowledge = a.judgement.Acknowledge
		if a.judgement.Entry != nil {
			a.Entry = a.judgement.Entry
		}

		switch a.judgement.Verdict {
		case nodeending.Proved:
		case nodeending.Unproved:
			code = exitRefused
		default:
			code = exitUnknown
		}
	} else if a.Outcome == nodeending.Unknown.String() {
		code = exitUnknown
	}

	return answerJSON(env, a, code, a.Why)
}
