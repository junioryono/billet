package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/lifeops"
)

// `billet node migrate-endpoint` moves a running node from the endpoint it
// dials to the one the installed configuration names, as ONE COMMAND that
// judges afresh when it acts. The role renders the configuration first (with
// the render's owner and mode), then the units and the environment, and
// runs this in place of the ordinary node restart; the command installs
// nothing. It observes the unit (a unit still stopping refuses whatever the
// endpoints say), establishes the running node's EFFECTIVE endpoint from its
// registration record read inside a bracket of unit observations, requires
// the rendering (on stdin) to be what is installed, and, when the effective
// endpoint differs from the installed one, requires a KillMode under which a
// stop proves something, stops the node under the migration's own deadline
// with only its own systemctl subprocess ever signalled, proves the stop by
// observation (inactive, dead, success), starts the node, and waits for the
// node's own record under the new invocation naming the installed endpoint
// as effective. The answer is evidence the role carries to the controller's
// confirmation and to the receipt.
//
// `--dry-run` is THE DECISION, run before the transaction and the render: it
// reports whether a migration is planned (the effective endpoint, or the
// installed one, or both, differing from the rendering's) and stops nothing.
// Node presence is the unit's and its process's, never a configuration's:
// an installed configuration without a node section, or an absent one, is
// observed like any other, and a first start is what the unit's positive
// absence of a process establishes.

type migrateMode struct {
	configPath, desired string
	stopTimeout, wait   time.Duration
	dryRun              bool
}

// migrateUnit is the unit's state as an answer carries it.
type migrateUnit struct {
	LoadState   string `json:"load_state"`
	ActiveState string `json:"active_state"`
	KillMode    string `json:"kill_mode"`
}

// migrateStopped is the post-stop triple the proof rests on.
type migrateStopped struct {
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
	Result      string `json:"result"`
}

// migrateReport is the dry run's answer.
type migrateReport struct {
	Schema      int         `json:"schema"`
	Outcome     string      `json:"outcome"`
	Planned     bool        `json:"planned"`
	FirstStart  bool        `json:"first_start"`
	NodeRemoved bool        `json:"node_removed"`
	From        *string     `json:"from"`
	To          *string     `json:"to"`
	Effective   *string     `json:"effective"`
	Record      string      `json:"record"`
	Config      string      `json:"config"`
	Unit        migrateUnit `json:"unit"`
}

// migrateUnchanged is the action's answer when nothing is to be migrated.
type migrateUnchanged struct {
	Schema      int         `json:"schema"`
	Outcome     string      `json:"outcome"`
	Node        *string     `json:"node"`
	Deployment  *string     `json:"deployment"`
	Endpoint    *string     `json:"endpoint"`
	Effective   *string     `json:"effective"`
	Record      string      `json:"record"`
	NodeRemoved bool        `json:"node_removed"`
	Unit        migrateUnit `json:"unit"`
}

// migrateEvidence is the answer of a migration performed: what the receipt
// and the controller's confirmation are built from.
type migrateEvidence struct {
	Schema          int            `json:"schema"`
	Outcome         string         `json:"outcome"`
	Node            string         `json:"node"`
	Deployment      string         `json:"deployment"`
	From            string         `json:"from"`
	To              string         `json:"to"`
	ConfigPath      string         `json:"config_path"`
	InstalledSHA256 string         `json:"installed_sha256"`
	InvocationID    string         `json:"invocation_id"`
	Incarnation     string         `json:"incarnation"`
	RegisteredAt    string         `json:"registered_at"`
	Stopped         migrateStopped `json:"stopped"`
}

func cmdNodeMigrate(ctx context.Context, args []string) error {
	flags := newFlagSet("billet node migrate-endpoint")
	configPath := flags.String("config", "", "the installed configuration (required)")
	desired := flags.String("desired", "", "the rendering the role installed, or means to: a path, or - for stdin")
	stopTimeout := flags.Duration("stop-timeout", time.Hour, "the migration's own deadline for the node's stop; "+
		"only this command's systemctl is signalled when it expires, never the node or its compute")
	wait := flags.Duration("wait", 5*time.Minute, "how long to wait for the node's own record under the new "+
		"invocation, and for a running node's record before the decision")
	dryRun := flags.Bool("dry-run", false, "decide and report; stop nothing")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("migrate-endpoint answers as JSON; pass --json")
	}

	m := migrateMode{configPath: *configPath, desired: *desired, stopTimeout: *stopTimeout, wait: *wait, dryRun: *dryRun}

	if r := checkMigrateCombination(m); r != nil {
		return answerEndpointRefusal(r)
	}

	if hostOS == "darwin" {
		return answerEndpointRefusal(endpointRefuse(endpointReasonPlatform,
			"an endpoint migration needs systemd and the node's runtime record, and this platform has neither",
			"", stateNothing))
	}

	answer, r := migrateEndpoint(ctx, m)
	if r != nil {
		return answerEndpointRefusal(r)
	}

	return answerObject(answer)
}

func checkMigrateCombination(m migrateMode) *endpointRefusal {
	bad := func(why string) *endpointRefusal {
		return endpointRefuse(endpointReasonCombination, why,
			"migrate-endpoint --config PATH --desired -|PATH [--stop-timeout D] [--wait D] [--dry-run] --json", stateNothing)
	}

	switch {
	case m.configPath == "":
		return bad("--config names the installed configuration, and it is empty")
	case m.desired == "" && !m.dryRun:
		return bad("--desired names the rendering, and it is empty (a dry run may omit it)")
	case m.stopTimeout <= 0:
		return bad("--stop-timeout must be positive")
	case m.wait <= 0:
		return bad("--wait must be positive")
	}

	return nil
}

// migrateEndpoint is the command's order of operations; every step's refusal
// names the state the host is left in.
func migrateEndpoint(ctx context.Context, m migrateMode) (any, *endpointRefusal) {
	// (3) THE INSTALLED CONFIGURATION, one observation.
	installed, r := observeInstalledConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	// (4) THE RENDERING.
	var (
		rendering *configObservationLite
	)

	if m.desired != "" {
		body, cfg, r := readRendering(m.desired)
		if r != nil {
			return nil, r
		}

		rendering = &configObservationLite{body: body, cfg: cfg}
	}

	if !installed.present && rendering == nil {
		return nil, endpointRefuse(endpointReasonConfig, "no configuration is installed at "+installed.path+
			" and no rendering was given, so there is no endpoint to judge", "", stateNothing)
	}

	// (5) THE ENDPOINTS AND THE IDENTITY, from whichever configuration has a
	// node section.
	installedEP, installedHasNode, err := nodeEndpointOf(installed.cfg)
	if err != nil {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration's node endpoint: "+err.Error(), "",
			stateNothing)
	}

	var (
		renderedEP      endpoint.Endpoint
		renderedHasNode bool
	)

	if rendering != nil {
		renderedEP, renderedHasNode, err = nodeEndpointOf(rendering.cfg)
		if err != nil {
			return nil, endpointRefuse(endpointReasonDesired, "the rendering's node endpoint: "+err.Error(), "",
				stateNothing)
		}
	}

	var renderingCfg = (*configObservationLite)(nil)
	if rendering != nil {
		renderingCfg = rendering
	}

	identity := identityFor(installed.cfg, cfgOf(renderingCfg))

	if installedHasNode && renderedHasNode {
		if problem := sameNodeIdentity(installed, rendering); problem != "" {
			return nil, endpointRefuse(endpointReasonDesired, problem, "", stateNothing)
		}
	}

	insp := endpointInspector()

	in := migrateInputs{installed: installed, rendering: rendering, installedEP: installedEP,
		installedHasNode: installedHasNode, renderedEP: renderedEP, renderedHasNode: renderedHasNode, identity: identity}

	// (6) TO (10) THE JUDGEMENT, retried from the unit's first observation
	// when the closing observation finds the process moved under it (three
	// attempts), never when the unit is found stopping or the configuration
	// moved, and never once the stop has begun.
	var j judgedMigration

	for attempt := 1; ; attempt++ {
		var (
			r     *endpointRefusal
			moved bool
		)

		j, r, moved = judgeMigration(ctx, m, insp, in)
		if r == nil {
			break
		}

		if moved && attempt < migrateJudgementAttempts {
			continue
		}

		return nil, r
	}

	if j.answer != nil {
		return j.answer, nil
	}

	br, effective := j.br, j.effective

	// (11) THE POLICY AND THE UNIT, ON A FRESH OBSERVATION IMMEDIATELY BEFORE
	// THE STOP: the judgement's observation is from before the record wait,
	// and what authorises the stop is the unit as it is now: the same process
	// the record was read from, not stopping, loaded, under a KillMode whose
	// stop proves something; and the configuration still the one judged.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem, "", stateNothing)
	}

	pre, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnit, "before the stop: "+problem, "", stateNothing)
	}

	if pre.ActiveState == "deactivating" {
		return nil, endpointRefuse(endpointReasonStopping, fmt.Sprintf("%s began stopping under the judgement (since %s); "+
			"wait or inspect the drain; nothing to do here", nodeUnit, orUnknownWord(pre.StateChangeTimestamp)),
			"wait for the stop to complete, then check and approve afresh", stateNothing)
	}

	if _, running, problem := runningPID(pre); problem != "" || !running || processMoved(br.obs, pre) {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("%s moved between the judgement and the stop (pid %s, "+
			"invocation %s, %s now; pid %s, invocation %s when judged)", nodeUnit, pre.MainPID, pre.InvocationID,
			pre.ActiveState, br.obs.MainPID, br.obs.InvocationID), "check and approve afresh", stateNothing)
	}

	switch pre.KillMode {
	case "mixed", "control-group":
	default:
		return nil, endpointRefuse(endpointReasonPolicy, fmt.Sprintf("%s has KillMode=%s, under which a stop proves nothing "+
			"about the processes that remain; only mixed or control-group is migrated", nodeUnit, orUnknownWord(pre.KillMode)),
			"", stateNothing)
	}

	if pre.LoadState != "loaded" {
		return nil, endpointRefuse(endpointReasonUnit, fmt.Sprintf("%s has LoadState=%s, so it cannot be started after the "+
			"stop", nodeUnit, orUnknownWord(pre.LoadState)), "", stateNothing)
	}

	// (12) THE STOP under the migration's own deadline: the context bounds
	// the command's systemctl and nothing else; a stop job systemd has begun
	// continues under the unit's own TimeoutStopSec.
	stopCtx, cancelStop := context.WithTimeout(ctx, m.stopTimeout)
	_, stopErr := lifeops.NewConverger(insp).StopAndProve(stopCtx, nodeUnit)
	cancelStop()

	if stopErr != nil && errors.Is(stopCtx.Err(), context.DeadlineExceeded) {
		after, _ := observeUnit(ctx, insp, nodeUnit)

		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the stop of %s did not complete within the "+
			"migration's deadline of %s (the unit is %s); the stop continues under systemd's own bound, and nothing "+
			"else was signalled", nodeUnit, m.stopTimeout, orUnknownWord(after.ActiveState)),
			"wait for the stop to complete, then check and approve afresh", stateNothing)
	}

	// (13) THE POST-STOP OBSERVATION, the command's own: the proof.
	post, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnproved, "after the stop: "+problem, "", stateStopped)
	}

	stopped := migrateStopped{ActiveState: post.ActiveState, SubState: post.SubState, Result: post.Result}

	if _, ok := post.raw["Result"]; !ok || stopped.ActiveState != "inactive" || stopped.SubState != "dead" ||
		stopped.Result != "success" {
		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the stop of %s is not proved: systemd reports "+
			"ActiveState=%s SubState=%s Result=%s, and only inactive/dead/success proves the process gone", nodeUnit,
			orUnknownWord(post.ActiveState), orUnknownWord(post.SubState), orUnknownWord(post.Result)),
			"inspect the unit; check and approve afresh", stateStopped)
	}

	// THE CONFIGURATION AGAIN, before the start: a file replaced or modified
	// under the stop is not the one judged, and the node stays stopped rather
	// than starting on it.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem+"; the node is stopped and was not started on it",
			"check and approve afresh", stateStopped)
	}

	// (14) THE START under the unit's own start bound.
	startCtx, cancelStart := context.WithTimeout(ctx, deploy.UnitStartTimeout+lifecycleDeadlineMargin)
	_, startErr := lifeops.NewConverger(insp).StartAndProve(startCtx, nodeUnit)
	cancelStart()

	if startErr != nil {
		return nil, endpointUnknown(endpointReasonUnproved, "the node was stopped and the configuration is installed, "+
			"but the start did not succeed: "+startErr.Error(), "systemctl start "+nodeUnit+", then converge again",
			stateStopped)
	}

	// (15) THE NEW INVOCATION.
	started, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnproved, "after the start: "+problem, "", stateStartedNoRecord)
	}

	if started.InvocationID == "" {
		return nil, endpointUnknown(endpointReasonUnproved, "systemd answered no InvocationID for the started "+nodeUnit,
			"", stateStartedNoRecord)
	}

	// (16) THE RECORD WAIT under the new invocation, naming the installed
	// endpoint as effective.
	newBr, elapsed, problem := waitForRecord(ctx, insp, nodeUnit, identity, m.wait)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnproved, "after the start: "+problem, "", stateStartedNoRecord)
	}

	switch {
	case !newBr.running:
		return nil, endpointUnknown(endpointReasonUnproved, "the node was started and is not running now ("+
			newBr.obs.ActiveState+"); its journal says why", "", stateStartedNoRecord)
	case newBr.class == recordUnreadable:
		return nil, endpointUnknown(endpointReasonUnproved, "the started node's record could not be read: "+newBr.why, "",
			stateStartedNoRecord)
	case newBr.class == recordInvalid:
		return nil, endpointUnknown(endpointReasonUnproved, "the started node's record is not one billet wrote: "+newBr.why, "",
			stateStartedNoRecord)
	case newBr.class == recordForeign:
		return nil, endpointUnknown(endpointReasonUnproved, "the started node's record cannot be judged: "+newBr.why, "",
			stateStartedNoRecord)
	case elapsed || newBr.class != recordUsable:
		return nil, endpointUnknown(endpointReasonUnproved, "the node was started and published no usable record within "+
			m.wait.String()+" ("+newBr.why+")", "wait for it to register, then converge again", stateStartedNoRecord)
	}

	// THE RECORD IS THE STARTED INVOCATION'S: a restart under the wait would
	// publish a record under a later invocation, which is not the process
	// this migration started.
	if newBr.obs.InvocationID != started.InvocationID || newBr.obs.MainPID != started.MainPID {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the node started as pid %s invocation %s and its "+
			"record was read from pid %s invocation %s; it moved under the wait", started.MainPID, started.InvocationID,
			newBr.obs.MainPID, newBr.obs.InvocationID), "", stateStarted)
	}

	newEP, err := endpoint.ParseCanonical(newBr.record.Endpoint)
	if err != nil || !newEP.Equal(installedEP) {
		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the started node dials %s, not the installed "+
			"endpoint %s", newBr.record.Endpoint, installedEP.String()), "", stateStartedNoRecord)
	}

	// (17) THE CLOSING CHECK: the configuration still the one judged, the
	// process still the one the record named.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem, "", stateStarted)
	}

	closing, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonProcess, "at the close: "+problem, "", stateStarted)
	}

	if processMoved(newBr.obs, closing) || closing.ActiveState == "deactivating" {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the started node moved before the answer: "+
			"pid %s invocation %s (%s) at the close, %s %s at the record", closing.MainPID, closing.InvocationID,
			closing.ActiveState, newBr.obs.MainPID, newBr.obs.InvocationID), "", stateStarted)
	}

	return migrateEvidence{
		Schema: endpointSchema, Outcome: outcomeMigrated, Node: newBr.record.Node, Deployment: newBr.record.Deployment,
		From: effective.String(), To: installedEP.String(), ConfigPath: installed.path,
		InstalledSHA256: installed.sha256, InvocationID: newBr.record.InvocationID,
		Incarnation: newBr.record.Incarnation, RegisteredAt: newBr.record.RegisteredAt, Stopped: stopped,
	}, nil
}

// migrateJudgementAttempts bounds the judgement's retries when the process
// moves under it.
const migrateJudgementAttempts = 3

// migrateInputs is what the judgement reads: the two configurations, their
// endpoints and the identity the record must name.
type migrateInputs struct {
	installed                         *installedConfigObservation
	rendering                         *configObservationLite
	installedEP, renderedEP           endpoint.Endpoint
	installedHasNode, renderedHasNode bool
	identity                          registrationIdentity
}

// judgedMigration is the judgement's result: an answer (a dry run's report
// or `unchanged`), or the evidence a migration proceeds from.
type judgedMigration struct {
	answer    any
	obs       unitObservation
	br        bracketedRecord
	effective *endpoint.Endpoint
	unit      migrateUnit
}

// judgeMigration is steps (6) to (10): the unit's first observation, the
// effective endpoint from the record under the bracket, the dry run's
// report or the `unchanged` answer with their closing checks, or the
// decision to migrate. The third result says the closing observation found
// the process moved, which the caller retries.
func judgeMigration(ctx context.Context, m migrateMode, insp *lifeops.Inspector, in migrateInputs) (judgedMigration, *endpointRefusal, bool) {
	installed, rendering := in.installed, in.rendering
	installedEP, installedHasNode := in.installedEP, in.installedHasNode
	renderedEP, renderedHasNode := in.renderedEP, in.renderedHasNode
	identity := in.identity

	// (6) THE UNIT'S FIRST OBSERVATION.
	obs, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return judgedMigration{}, endpointUnknown(endpointReasonUnit, problem, "", stateNothing), false
	}

	if obs.ActiveState == "deactivating" {
		return judgedMigration{}, endpointRefuse(endpointReasonStopping, fmt.Sprintf("%s is still stopping (since %s); wait or "+
			"inspect the drain; nothing to do here", nodeUnit, orUnknownWord(obs.StateChangeTimestamp)),
			"wait for the stop to complete, then check and approve afresh", stateNothing), false
	}

	// (7) THE EFFECTIVE ENDPOINT, from the running node's record under the
	// bracket, waited for while a node that just started has none. With no
	// node section on either side there is no identity and no endpoint to
	// judge, so the branch is OBSERVATION-ONLY: the unit's state alone, the
	// record not consulted.
	var (
		br      bracketedRecord
		elapsed bool
	)

	if !installedHasNode && !renderedHasNode {
		_, running, problem := runningPID(obs)
		if problem != "" {
			return judgedMigration{}, endpointUnknown(endpointReasonUnit, problem, "", stateNothing), false
		}

		br = bracketedRecord{obs: obs, running: running, class: recordAbsent}
	} else {
		br, elapsed, problem = waitForRecord(ctx, insp, nodeUnit, identity, m.wait)
		if problem != "" {
			return judgedMigration{}, endpointUnknown(endpointReasonUnit, problem, "", stateNothing), false
		}
	}

	var (
		effective  *endpoint.Endpoint
		recordKind = recordNone
	)

	switch {
	case !br.running:
		recordKind = recordNone
	case !installedHasNode && !renderedHasNode:
		recordKind = recordUnread
	case br.class == recordUnreadable:
		return judgedMigration{}, endpointUnknown(endpointReasonRecord, "the running node's record could not be read: "+br.why,
			"", stateNothing), false
	case br.class == recordInvalid:
		return judgedMigration{}, endpointUnknown(endpointReasonRecord, "the running node's record is not one billet wrote: "+
			br.why, "", stateNothing), false
	case br.class == recordForeign:
		return judgedMigration{}, endpointUnknown(endpointReasonRecord, "the running node's record cannot be judged: "+br.why, "",
			stateNothing), false
	case br.class == recordUsable:
		e, err := endpoint.ParseCanonical(br.record.Endpoint)
		if err != nil {
			return judgedMigration{}, endpointUnknown(endpointReasonRecord, "the record's endpoint: "+err.Error(), "", stateNothing), false
		}

		effective, recordKind = &e, recordCurrent
	case elapsed:
		// A RUNNING NODE WITHOUT A USABLE RECORD: positively a release before
		// the guard, or could-not-tell.
		pid, _, _ := runningPID(br.obs)

		preR, why := preRProcess(ctx, pid)
		if !preR {
			return judgedMigration{}, endpointUnknown(endpointReasonRecord, "the running node published no record within the wait ("+
				why+"): an R process that has not registered, or whose record write failed; wait, or restart it",
				"", stateNothing), false
		}

		if !installedHasNode {
			return judgedMigration{}, endpointUnknown(endpointReasonRecord, "a release before the guard runs here and its configured "+
				"endpoint cannot be read (the installed configuration has no node section); stop it, or restore "+
				"its configuration, before this converge", "", stateNothing), false
		}

		if !m.dryRun {
			return judgedMigration{}, endpointRefuse(endpointReasonPreR, "the running node is a release before the converge guard; "+
				"a binary upgrade restarts it into one that carries the guard, and the action then judges that process",
				"upgrade the binary first", stateNothing), false
		}

		recordKind = recordAbsentPreR
	default:
		recordKind = recordUnread
	}

	unit := migrateUnit{LoadState: obs.LoadState, ActiveState: obs.ActiveState, KillMode: obs.KillMode}

	// (8) THE DRY RUN.
	if m.dryRun {
		report := migrateReport{
			Schema: endpointSchema, Outcome: outcomeReported, Record: recordKind, Unit: unit,
			Config: configWord(installed.present), Effective: canonicalPtr(effective),
		}

		if effective != nil {
			report.From = canonicalPtr(effective)
		} else if installedHasNode {
			report.From = canonicalOrNull(installedEP, true)
		}

		if rendering != nil {
			report.To = canonicalOrNull(renderedEP, renderedHasNode)
			report.NodeRemoved = !renderedHasNode
			report.FirstStart = !installedHasNode && renderedHasNode && !br.running

			if renderedHasNode {
				if effective != nil && !effective.Equal(renderedEP) {
					report.Planned = true
				}

				if installedHasNode && !installedEP.Equal(renderedEP) {
					report.Planned = true
				}
			}
		} else if effective != nil && installedHasNode && !effective.Equal(installedEP) {
			report.Planned = true
		}

		if problem := closeInstalledConfig(installed); problem != "" {
			return judgedMigration{}, endpointUnknown(endpointReasonConfig, problem, "", stateNothing), false
		}

		if problem, moved := closeUnitObservation(ctx, insp, br); problem != nil {
			return judgedMigration{}, problem, moved
		}

		return judgedMigration{answer: report}, nil, false
	}

	// (9) THE RENDERING MUST BE INSTALLED.
	if !installed.present {
		return judgedMigration{}, endpointRefuse(endpointReasonConfig, "no configuration is installed at "+installed.path+
			"; the render precedes this command", "", stateNothing), false
	}

	if renderedHasNode != installedHasNode || (renderedHasNode && !renderedEP.Equal(installedEP)) {
		return judgedMigration{}, endpointRefuse(endpointReasonDesired, "the rendering was not installed: the installed configuration's "+
			"node endpoint is not the rendering's", "the render precedes this command", stateNothing), false
	}

	// (10) NOTHING TO MIGRATE.
	unchanged := func() (judgedMigration, *endpointRefusal, bool) {
		if problem := closeInstalledConfig(installed); problem != "" {
			return judgedMigration{}, endpointUnknown(endpointReasonConfig, problem, "", stateNothing), false
		}

		if problem, moved := closeUnitObservation(ctx, insp, br); problem != nil {
			return judgedMigration{}, problem, moved
		}

		out := migrateUnchanged{Schema: endpointSchema, Outcome: outcomeUnchanged, Record: recordKind, Unit: unit,
			Effective: canonicalPtr(effective), NodeRemoved: rendering != nil && !renderedHasNode}

		if installedHasNode {
			out.Endpoint = canonicalOrNull(installedEP, true)
			// THE IDENTITY IS SEPARATE from the record's judgement: the name is
			// the configuration's effective one whenever a node section exists,
			// and the deployment a string only where a certificate or an
			// existing identity file gives it, never minted.
			name := identity.node
			if name == "" {
				name = installed.cfg.Node.Name
			}

			if name == "" {
				return judgedMigration{}, endpointUnknown(endpointReasonConfig, "the installed configuration has a node "+
					"section whose effective name cannot be resolved ("+identity.why+")", "", stateNothing), false
			}

			out.Node = stringOrNull(name)
			out.Deployment = stringOrNull(identity.deployment)
		}

		return judgedMigration{answer: out}, nil, false
	}

	if !br.running || !renderedHasNode || (effective != nil && effective.Equal(installedEP)) {
		return unchanged()
	}

	if effective == nil {
		// A running node with a record that is neither usable nor a positively
		// pre-R absence is already refused above; what remains is a node
		// running beside configurations without endpoints, an observation-only
		// answer.
		return unchanged()
	}

	return judgedMigration{obs: obs, br: br, effective: effective, unit: unit}, nil, false
}

// closeUnitObservation is the closing check of a reported or unchanged
// answer: the unit observed once more and compared with the bracket the
// answer rests on; a process that moved is could-not-tell here and true in
// the second result, which the caller's judgement retries; a unit found
// stopping refuses.
func closeUnitObservation(ctx context.Context, insp *lifeops.Inspector, br bracketedRecord) (*endpointRefusal, bool) {
	closing, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return endpointUnknown(endpointReasonProcess, "at the close: "+problem, "", stateNothing), false
	}

	if closing.ActiveState == "deactivating" {
		return endpointRefuse(endpointReasonStopping, fmt.Sprintf("%s began stopping under the judgement (since %s); "+
			"wait or inspect the drain; nothing to do here", nodeUnit, orUnknownWord(closing.StateChangeTimestamp)),
			"wait for the stop to complete, then check and approve afresh", stateNothing), false
	}

	_, running, problem := runningPID(closing)
	if problem != "" {
		return endpointUnknown(endpointReasonProcess, "at the close: "+problem, "", stateNothing), false
	}

	if running != br.running || (running && processMoved(br.obs, closing)) {
		return endpointUnknown(endpointReasonProcess, fmt.Sprintf("%s moved under the judgement (pid %s, invocation "+
			"%s, %s at the close; pid %s, invocation %s, %s when judged)", nodeUnit, closing.MainPID, closing.InvocationID,
			closing.ActiveState, br.obs.MainPID, br.obs.InvocationID, br.obs.ActiveState),
			"check and approve afresh", stateNothing), true
	}

	return nil, false
}

// configObservationLite is a rendering: its bytes and its parse.
type configObservationLite struct {
	body []byte
	cfg  *config.Config
}

func cfgOf(r *configObservationLite) *config.Config {
	if r == nil {
		return nil
	}

	return r.cfg
}

// sameNodeIdentity refuses a rendering whose node is not the installed one.
func sameNodeIdentity(installed *installedConfigObservation, rendering *configObservationLite) string {
	// THE CONFIGURED NAMES FIRST: two names both known and unequal disagree
	// whatever the deployment says, and a deployment that cannot be derived
	// (no identity minted on a stopped certless host) hides nothing.
	if ia, ib := installed.cfg.Node.Name, rendering.cfg.Node.Name; ia != "" && ib != "" && ia != ib {
		return fmt.Sprintf("the rendering names the node %q and the installed configuration %q", ib, ia)
	}

	a := expectedRegistrationIdentity(installed.cfg)
	b := expectedRegistrationIdentity(rendering.cfg)

	if a.why != "" || b.why != "" {
		return ""
	}

	if a.node != b.node {
		return fmt.Sprintf("the rendering names the node %q and the installed configuration %q", b.node, a.node)
	}

	if a.deployment != b.deployment {
		return fmt.Sprintf("the rendering names the deployment %s and the installed configuration %s", b.deployment,
			a.deployment)
	}

	return ""
}

func configWord(present bool) string {
	if present {
		return "present"
	}

	return "absent"
}

func canonicalPtr(e *endpoint.Endpoint) *string {
	if e == nil {
		return nil
	}

	s := e.String()

	return &s
}

func orUnknownWord(s string) string {
	if s == "" {
		return "unknown"
	}

	return s
}
