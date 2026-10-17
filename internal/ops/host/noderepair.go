package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/regularfile"
)

// `billet node repair-unit` gives a node unit rendered before the registration
// record the running binary publishes the one runtime directory that record
// needs (#168). A rollout moves the binary and never the units, so a host whose
// unit declares only billet/locks runs a node that cannot write
// /run/billet/registration/current, and the endpoint decision, which waits for
// that record, refuses a converge that is the only thing that would install the
// fixed unit.
//
// IT ADMITS ONE SHAPE AND MAKES ONE CHANGE. The unit the role rendered at
// /etc/systemd/system/billet-node.service, loaded, with no drop-in and no
// pending reload, declaring exactly RuntimeDirectory=billet/locks in exactly one
// [Service] line, under a running node positively of a release that publishes
// its record and that has published none. That line becomes
// RuntimeDirectory=billet/locks billet/registration and nothing else in the file
// changes; the converge that follows installs the current unit. The node is
// stopped under the migration's own proof (inactive, dead, success), the unit
// installed and reloaded, the node started, and the answer waits for a record
// the new invocation wrote naming the expected identity and the installed
// endpoint. A restart here is maintenance the operator's converge authorises:
// it runs under this converge's guard (`--run`), claims no endpoint continuity
// it did not prove, and the endpoint decision is then made from scratch.
//
// `--dry-run` says whether the repair is required and changes nothing.

const (
	// outcomeRepaired is the action's answer for a repair performed.
	outcomeRepaired = "repaired"

	// stateUnitInstalled is a node stopped with the repaired unit installed.
	stateUnitInstalled = "unit-installed"

	// The one line admitted, and what it becomes.
	legacyRuntimeDirectory   = "RuntimeDirectory=billet/locks"
	repairedRuntimeDirectory = legacyRuntimeDirectory + " " + deploy.NodeRegistrationDirectory

	// maxUnitBytes bounds the unit file's read.
	maxUnitBytes = 64 << 10
)

// repairUnitPath is the unit the host role renders, the only one a repair
// edits; a variable so a test can point it into its own directory.
var repairUnitPath = "/etc/systemd/system/" + deploy.NodeUnitName

// repairUnitOwned refuses a unit file root does not own, judged on the file
// the descriptor that is read was opened on; a variable so a test, which is
// not root, can admit its own fixture.
var repairUnitOwned = rootOwnedUnit

func rootOwnedUnit(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return fmt.Errorf("%s is not owned by root", repairUnitPath)
	}

	return nil
}

type repairMode struct {
	configPath, run   string
	stopTimeout, wait time.Duration
	dryRun            bool
}

// repairShape is the loaded unit as an answer carries it.
type repairShape struct {
	LoadState        string `json:"load_state"`
	FragmentPath     string `json:"fragment_path"`
	RuntimeDirectory string `json:"runtime_directory"`
}

// repairReport is the dry run's answer.
type repairReport struct {
	Schema   int         `json:"schema"`
	Outcome  string      `json:"outcome"`
	Required bool        `json:"required"`
	Why      string      `json:"why"`
	Unit     repairShape `json:"unit"`
}

// repairUnchanged is the action's answer when nothing is to be repaired.
type repairUnchanged struct {
	Schema  int         `json:"schema"`
	Outcome string      `json:"outcome"`
	Why     string      `json:"why"`
	Unit    repairShape `json:"unit"`
}

// repairEvidence is the answer of a repair performed.
type repairEvidence struct {
	Schema         int            `json:"schema"`
	Outcome        string         `json:"outcome"`
	Node           string         `json:"node"`
	Deployment     string         `json:"deployment"`
	Endpoint       string         `json:"endpoint"`
	UnitPath       string         `json:"unit_path"`
	BeforeSHA256   string         `json:"before_sha256"`
	AfterSHA256    string         `json:"after_sha256"`
	FromInvocation string         `json:"from_invocation"`
	InvocationID   string         `json:"invocation_id"`
	Incarnation    string         `json:"incarnation"`
	RegisteredAt   string         `json:"registered_at"`
	Stopped        migrateStopped `json:"stopped"`
}

const repairUsage = "repair-unit --config PATH --run ID [--stop-timeout D] [--wait D] --json, or " +
	"repair-unit --config PATH --dry-run --json"

// NodeRepairUnit is `billet node repair-unit`.
func NodeRepairUnit(ctx context.Context, env cli.Env, args []string) error {
	flags := cli.NewFlagSet("billet node repair-unit", env.Stdout)
	configPath := flags.String("config", "", "the installed configuration (required)")
	run := flags.String("run", "", "the holder of this converge's guard, which the repair runs under (required "+
		"unless --dry-run)")
	stopTimeout := flags.Duration("stop-timeout", time.Hour, "the repair's own deadline for the node's stop; "+
		"only this command's systemctl is signalled when it expires, never the node or its compute")
	wait := flags.Duration("wait", 5*time.Minute, "how long to wait for the node's own record under the new invocation")
	dryRun := flags.Bool("dry-run", false, "say whether the repair is required; change nothing")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := cli.Parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("repair-unit answers as JSON; pass --json")
	}

	m := repairMode{configPath: *configPath, run: *run, stopTimeout: *stopTimeout, wait: *wait, dryRun: *dryRun}

	switch {
	case m.configPath == "":
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonCombination,
			"--config names the installed configuration, and it is empty", repairUsage, stateNothing))
	case m.run == "" && !m.dryRun:
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonCombination,
			"--run names the holder of this converge's guard, which a repair runs under, and it is empty",
			repairUsage, stateNothing))
	case m.stopTimeout <= 0 || m.wait <= 0:
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonCombination,
			"--stop-timeout and --wait must be positive", repairUsage, stateNothing))
	}

	if cli.HostOS != "linux" {
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonPlatform,
			"a runtime-unit repair needs systemd, and this platform has none", "", stateNothing))
	}

	answer, r := repairNodeUnit(ctx, m)
	if r != nil {
		return answerEndpointRefusal(env, r)
	}

	return answerObject(env, answer)
}

// repairJudgement is what a repair rests on, judged afresh under the locks: the
// loaded unit's shape, the file's bytes, and the running node's record.
type repairJudgement struct {
	shape    repairShape
	required bool
	why      string
	body     []byte
	br       bracketedRecord
}

// judgeRepair observes the loaded unit, reads its file and the running node's
// record, and says whether the repair is required; a refusal is a unit or a
// node a repair does not admit.
func judgeRepair(ctx context.Context, insp *lifeops.Inspector, identity registrationIdentity) (repairJudgement, *endpointRefusal) {
	var j repairJudgement

	var r *endpointRefusal

	j.shape, j.required, j.why, r = observeRepairShape(ctx, insp)
	if r != nil || !j.required {
		return j, r
	}

	// THE FILE, read once by descriptor: exactly the role's, root's, with
	// exactly the one admitted line.
	if j.body, r = readRepairUnit(); r != nil {
		return j, r
	}

	// THE RUNNING NODE, its record read inside a bracket: a repair is for a node
	// that runs and publishes nothing.
	br, problem := readRecordUnderBracket(ctx, insp, nodeUnit, identity, time.Time{})
	if problem != "" {
		return j, endpointUnknown(endpointReasonUnit, problem, "", stateNothing)
	}

	j.br = br

	switch {
	case !br.running:
		j.required, j.why = false, "the node is not running, so the converge installs the current unit before it starts"

		return j, nil
	case br.class == recordUsable:
		return j, endpointRefuse(endpointReasonRecord, "the running node published a record although its unit "+
			"declares no directory for one; inspect the host", "", stateNothing)
	case br.class != recordAbsent:
		return j, endpointUnknown(endpointReasonRecord, "the running node's record cannot be judged: "+br.why, "",
			stateNothing)
	}

	// A RELEASE THAT PUBLISHES, POSITIVELY: a restart of anything else under
	// the repaired unit would still publish nothing.
	pid, _, _ := runningPID(br.obs)

	switch vintage, why := processVintageOf(ctx, pid); vintage {
	case vintageR:
	case vintagePreR:
		return j, endpointRefuse(endpointReasonPreR, "the running node is a release before the converge guard, which "+
			"publishes no registration record under any unit; a repair would restart it for nothing",
			"upgrade the binary first", stateNothing)
	default:
		return j, endpointUnknown(endpointReasonProcess, "whether the running node publishes a registration record "+
			"cannot be told ("+why+")", "", stateNothing)
	}

	j.why = "runtime-unit repair required; effective endpoint unknown: the running node cannot publish its " +
		"registration record, because its unit declares no " + deploy.NodeRegistrationDirectory + " directory"

	return j, nil
}

// repairNodeUnit is the command's order of operations; every refusal names the
// state the host is left in.
func repairNodeUnit(ctx context.Context, m repairMode) (any, *endpointRefusal) {
	// (1) THE INSTALLED CONFIGURATION, one observation: the endpoint and the
	// identity the new invocation's record must name.
	installed, r := observeInstalledConfig(m.configPath, !m.dryRun)
	if r != nil {
		return nil, r
	}

	if !installed.present {
		return nil, endpointRefuse(endpointReasonConfig, "no configuration is installed at "+installed.path, "", stateNothing)
	}

	installedEP, hasNode, err := nodeEndpointOf(installed.cfg)
	if err != nil {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration's node endpoint: "+err.Error(), "",
			stateNothing)
	}

	if !hasNode {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration has no node section, so there is "+
			"no node whose unit to repair", "", stateNothing)
	}

	identity := expectedRegistrationIdentity(installed.cfg)
	if identity.contradiction != "" {
		return nil, endpointRefuse(endpointReasonConfig, identity.contradiction, "", stateNothing)
	}

	if identity.why != "" {
		return nil, endpointUnknown(endpointReasonConfig, "the installed configuration's node identity: "+identity.why, "",
			stateNothing)
	}

	insp := endpointInspector()

	// (2) THE JUDGEMENT, for the dry run's answer and the action's premise.
	judged, r := judgeRepair(ctx, insp, identity)
	if r != nil {
		return nil, r
	}

	if m.dryRun {
		if problem := closeInstalledConfig(installed); problem != "" {
			return nil, endpointUnknown(endpointReasonConfig, problem, "", stateNothing)
		}

		return repairReport{Schema: endpointSchema, Outcome: outcomeReported, Required: judged.required, Why: judged.why,
			Unit: judged.shape}, nil
	}

	if !judged.required {
		if problem := closeInstalledConfig(installed); problem != "" {
			return nil, endpointUnknown(endpointReasonConfig, problem, "", stateNothing)
		}

		return repairUnchanged{Schema: endpointSchema, Outcome: outcomeUnchanged, Why: judged.why,
			Unit: judged.shape}, nil
	}

	// (3) THIS CONVERGE'S GUARD, under the transaction lock, so no upgrade and
	// no other converge moves the host while the node is stopped; and the
	// lifecycle lock, so no `local up` or `local down` does.
	tx, guard, r := repairGuard(m.run)
	if r != nil {
		return nil, r
	}

	defer func() {
		_ = guard.Close()

		tx.release()
	}()

	lock, err := lifecycleLock()
	if err != nil {
		return nil, endpointUnknown(endpointReasonLock, "take the lifecycle lock: "+err.Error(), "", stateNothing)
	}

	//nolint:errcheck // the answer is JSON alone and has no place for a warning, and the process's exit releases the flock whatever this returns.
	defer func() { _ = lock.Release() }()

	// (4) THE JUDGEMENT AGAIN, UNDER THE LOCKS: a record published, a unit
	// replaced or reloaded, a process moved since the first is not what was
	// judged, and nothing is stopped over it.
	again, r := judgeRepair(ctx, insp, identity)
	if r != nil {
		return nil, r
	}

	if !again.required || !bytes.Equal(again.body, judged.body) || processMoved(judged.br.obs, again.br.obs) {
		return nil, endpointUnknown(endpointReasonProcess, "the node unit, its file or its process changed between the "+
			"judgement and the locks; nothing was stopped", "converge again", stateNothing)
	}

	return repairUnderLock(ctx, m, insp, installed, installedEP, identity, again)
}

// repairGuard takes the transaction lock and requires this converge's guard, as
// a retirement does (judgeGuardShape): the lock and the guard directory are the
// caller's to release.
func repairGuard(run string) (*txLock, *os.File, *endpointRefusal) {
	tx, err := takeTxLock()
	if err != nil {
		return nil, nil, endpointRefuse(endpointReasonLock, "take the upgrade transaction lock: "+err.Error(),
			"wait for the transaction to finish, then converge again", stateNothing)
	}

	dir, shape, err := openGuardForMutation(tx)
	if err != nil {
		tx.release()

		return nil, nil, endpointUnknown(endpointReasonGuard, "open this converge's guard: "+err.Error(), "", stateNothing)
	}

	if rr := judgeGuardShape(shape, run, "a runtime-unit repair"); rr != nil {
		if dir != nil {
			_ = dir.Close()
		}

		tx.release()

		refusal := endpointRefuse
		if rr.Outcome == outcomeUnknown {
			refusal = endpointUnknown
		}

		return nil, nil, refusal(endpointReasonGuard, rr.Why, "",
			stateNothing)
	}

	return tx, dir, nil
}

// repairUnderLock is the stop, the unit, the start and the record, under the
// locks.
func repairUnderLock(ctx context.Context, m repairMode, insp *lifeops.Inspector, installed *installedConfigObservation,
	installedEP endpoint.Endpoint, identity registrationIdentity, judged repairJudgement,
) (any, *endpointRefusal) {
	unitBody, br := judged.body, judged.br

	// (5) THE CONFIGURATION AND THE PROCESS, IMMEDIATELY BEFORE THE STOP: the
	// file still the one judged, the process the one the bracket read, not
	// stopping, under a KillMode whose stop proves something.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem+"; nothing was stopped", "", stateNothing)
	}

	if r := admitNodeStop(ctx, insp, br.obs, "repaired"); r != nil {
		return nil, r
	}

	// AND THE LOADED UNIT, LAST: a drop-in or a reload since the judgement read
	// it changes what the stop is for, without moving the process.
	if now, required, _, r := observeRepairShape(ctx, insp); r != nil {
		return nil, r
	} else if !required || now != judged.shape {
		return nil, endpointUnknown(endpointReasonShape, fmt.Sprintf("%s changed under the judgement (RuntimeDirectory=%s "+
			"from %s); nothing was stopped", nodeUnit, now.RuntimeDirectory, now.FragmentPath), "converge again",
			stateNothing)
	}

	// (6) THE STOP, under the repair's own deadline, and its proof.
	stopCtx, cancelStop := context.WithTimeout(ctx, m.stopTimeout)
	_, stopErr := lifeops.NewConverger(insp).StopAndProve(stopCtx, nodeUnit)
	cancelStop()

	if stopErr != nil && errors.Is(stopCtx.Err(), context.DeadlineExceeded) {
		after, _ := observeUnit(ctx, insp, nodeUnit)

		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the stop of %s did not complete within the "+
			"repair's deadline of %s (the unit is %s); the stop continues under systemd's own bound, and nothing else "+
			"was signalled", nodeUnit, m.stopTimeout, orUnknownWord(after.ActiveState)),
			"wait for the stop to complete, then converge again", stateNothing)
	}

	stopped, r := proveNodeStopped(ctx, insp)
	if r != nil {
		return nil, r
	}

	// (7) THE FILE AGAIN, before it is replaced: bytes changed under the stop
	// are not the ones admitted, and the node stays stopped.
	again, r := readRepairUnit()
	if r != nil {
		r.State = stateStopped

		return nil, r
	}

	if !bytes.Equal(again, unitBody) {
		return nil, endpointUnknown(endpointReasonShape, repairUnitPath+" changed under the node's stop; the node is "+
			"stopped and the unit was not touched", "inspect the unit, then converge again", stateStopped)
	}

	repaired, problem := repairRuntimeDirectory(unitBody)
	if problem != "" {
		return nil, endpointRefuse(endpointReasonShape, problem, "", stateStopped)
	}

	// (8) THE INSTALL, through the durable installer's one ordering, with the
	// file's own mode. An error after the rename (the directory's sync) leaves
	// the repaired file in place, so what is on disk is read back to name the
	// state.
	info, err := os.Lstat(repairUnitPath)
	if err != nil {
		return nil, endpointUnknown(endpointReasonShape, "examine "+repairUnitPath+": "+err.Error(), "", stateStopped)
	}

	if _, err := repairInstaller.Install(filepath.Dir(repairUnitPath), filepath.Base(repairUnitPath),
		info.Mode().Perm(), func(w io.Writer) error {
			_, err := w.Write(repaired)

			return err
		}); err != nil {
		now, readErr := regularfile.ReadFile(repairUnitPath, maxUnitBytes, regularfile.Options{NoFollow: true})

		switch {
		case readErr != nil:
			// COULD-NOT-TELL WHICH UNIT IS ON DISK is not a unit untouched: the
			// rename may have landed, so the worse of the two states is named.
			return nil, endpointUnknown(endpointReasonShape, "install the repaired unit: "+err.Error()+"; and whether "+
				"it was put in place cannot be told: "+readErr.Error(), "inspect "+repairUnitPath+", then converge again",
				stateUnitInstalled)
		case bytes.Equal(now, repaired):
			return nil, endpointUnknown(endpointReasonShape, "the repaired unit was put in place and its durability is "+
				"unproved: "+err.Error(), "inspect "+repairUnitPath+", then converge again", stateUnitInstalled)
		}

		return nil, endpointUnknown(endpointReasonShape, "install the repaired unit: "+err.Error(),
			"inspect "+repairUnitPath+", then converge again", stateStopped)
	}

	// (9) THE RELOAD, and the manager's own word that it loaded the repair and
	// nothing else: the role's fragment, no drop-in, no reload pending.
	if err := insp.DaemonReload(ctx); err != nil {
		return nil, endpointUnknown(endpointReasonShape, "reload systemd: "+err.Error(),
			"systemctl daemon-reload, then converge again", stateUnitInstalled)
	}

	if r := proveRepairLoaded(ctx, insp); r != nil {
		return nil, r
	}

	// (10) THE CONFIGURATION AGAIN, before the start: a file replaced under the
	// stop is not the one judged, and the node stays stopped rather than
	// starting on it.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem+"; the node is stopped with the repaired unit "+
			"installed and was not started on it", "converge again", stateUnitInstalled)
	}

	// (11) THE START, under the unit's own start bound. A start whose proof
	// failed may have left a process running, so what systemd says now names
	// the state.
	startCtx, cancelStart := context.WithTimeout(ctx, deploy.UnitStartTimeout+lifecycleDeadlineMargin)
	_, startErr := lifeops.NewConverger(insp).StartAndProve(startCtx, nodeUnit)
	cancelStart()

	if startErr != nil {
		state := stateStartedNoRecord

		if after, problem := observeUnit(ctx, insp, nodeUnit); problem == "" {
			if _, running, problem := runningPID(after); problem == "" && !running {
				state = stateUnitInstalled
			}
		}

		return nil, endpointUnknown(endpointReasonUnproved, "the repaired unit is installed and the node's start was not "+
			"proved: "+startErr.Error(), "inspect "+nodeUnit+", then converge again", state)
	}

	started, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnproved, "after the start: "+problem, "", stateStartedNoRecord)
	}

	if started.InvocationID == "" || started.InvocationID == br.obs.InvocationID {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the started node answers invocation %q, not a "+
			"new one", started.InvocationID), "", stateStartedNoRecord)
	}

	// (12) THE NEW INVOCATION'S RECORD, naming the identity and the installed
	// endpoint.
	newBr, elapsed, problem := waitForRecord(ctx, insp, identity, m.wait)
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
		return nil, endpointUnknown(endpointReasonUnproved, "the started node's record is not one billet wrote: "+newBr.why,
			"", stateStartedNoRecord)
	case newBr.class == recordForeign:
		return nil, endpointUnknown(endpointReasonUnproved, "the started node's record cannot be judged: "+newBr.why, "",
			stateStartedNoRecord)
	case elapsed || newBr.class != recordUsable:
		return nil, endpointUnknown(endpointReasonUnproved, "the node was started and published no usable record within "+
			m.wait.String()+" ("+newBr.why+")", "wait for it to register, then converge again", stateStartedNoRecord)
	}

	if newBr.obs.InvocationID != started.InvocationID || newBr.obs.MainPID != started.MainPID {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the node started as pid %s invocation %s and its "+
			"record was read from pid %s invocation %s; it moved under the wait", started.MainPID, started.InvocationID,
			newBr.obs.MainPID, newBr.obs.InvocationID), "", stateStarted)
	}

	recordEP, err := endpoint.ParseCanonical(newBr.record.Endpoint)
	if err != nil || !recordEP.Equal(installedEP) {
		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the started node dials %s, not the installed "+
			"endpoint %s", newBr.record.Endpoint, installedEP.String()), "", stateStarted)
	}

	// (13) THE CLOSE: the configuration still the one judged, the process
	// still the one the record named.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem, "", stateStarted)
	}

	closing, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonProcess, "at the close: "+problem, "", stateStarted)
	}

	if _, running, problem := runningPID(closing); problem != "" || !running || processMoved(newBr.obs, closing) ||
		closing.ActiveState == "deactivating" {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the started node moved or stopped before the "+
			"answer: pid %s invocation %s (%s) at the close", closing.MainPID, closing.InvocationID, closing.ActiveState),
			"", stateStarted)
	}

	return repairEvidence{
		Schema: endpointSchema, Outcome: outcomeRepaired, Node: newBr.record.Node, Deployment: newBr.record.Deployment,
		Endpoint: installedEP.String(), UnitPath: repairUnitPath, BeforeSHA256: digestHex(unitBody),
		AfterSHA256: digestHex(repaired), FromInvocation: br.obs.InvocationID, InvocationID: newBr.record.InvocationID,
		Incarnation: newBr.record.Incarnation, RegisteredAt: newBr.record.RegisteredAt, Stopped: stopped,
	}, nil
}

// repairInstaller is the durable install the repair puts the unit in place
// with; a variable so a test can fail a step after the rename.
var repairInstaller = durablefile.Installer{}

// repairShapeProperties are what a repair reads of the loaded unit.
var repairShapeProperties = []string{"LoadState", "FragmentPath", "DropInPaths", "NeedDaemonReload", "RuntimeDirectory"}

// observeRepairShapeProps reads the loaded node unit's shape properties, every
// one answered or could-not-tell.
func observeRepairShapeProps(ctx context.Context, insp *lifeops.Inspector, state string) (map[string][]string, repairShape, *endpointRefusal) {
	props, err := insp.UnitProperties(ctx, nodeUnit, repairShapeProperties...)
	if err != nil {
		return nil, repairShape{}, endpointUnknown(endpointReasonUnit, "ask systemd about "+nodeUnit+": "+err.Error(),
			"", state)
	}

	shape := repairShape{LoadState: firstProp(props, "LoadState"), FragmentPath: firstProp(props, "FragmentPath"),
		RuntimeDirectory: firstProp(props, "RuntimeDirectory")}

	// EVERY PROPERTY ANSWERED: an absent one is could-not-tell, never empty.
	for _, name := range repairShapeProperties {
		if _, ok := props[name]; !ok {
			return nil, shape, endpointUnknown(endpointReasonUnit, "systemd answered no "+name+" for "+nodeUnit, "", state)
		}
	}

	return props, shape, nil
}

// observeRepairShape reads the loaded node unit and says whether it is the one
// shape a repair admits (required), a unit that needs none (it declares the
// registration directory), or one refused.
func observeRepairShape(ctx context.Context, insp *lifeops.Inspector) (repairShape, bool, string, *endpointRefusal) {
	props, shape, r := observeRepairShapeProps(ctx, insp, stateNothing)
	if r != nil {
		return shape, false, "", r
	}

	if shape.LoadState != "loaded" {
		return shape, false, "", endpointRefuse(endpointReasonUnit, fmt.Sprintf("%s is LoadState=%s, so there is no "+
			"unit to repair", nodeUnit, orUnknownWord(shape.LoadState)), "", stateNothing)
	}

	entries := strings.Fields(shape.RuntimeDirectory)

	if slices.Contains(entries, deploy.NodeRegistrationDirectory) {
		return shape, false, "the unit already declares " + deploy.NodeRegistrationDirectory, nil
	}

	switch {
	case !slices.Equal(entries, []string{"billet/locks"}):
		return shape, false, "", endpointRefuse(endpointReasonShape, fmt.Sprintf("%s declares RuntimeDirectory=%s, which is "+
			"not the one shape a repair admits (billet/locks alone)", nodeUnit, shape.RuntimeDirectory),
			"add "+deploy.NodeRegistrationDirectory+" to its RuntimeDirectory by hand as docs/operating/upgrades.md "+
				"describes, or install the package this release ships", stateNothing)
	case shape.FragmentPath != repairUnitPath:
		return shape, false, "", endpointRefuse(endpointReasonShape, fmt.Sprintf("%s is loaded from %s, not from %s, "+
			"which is the only unit a repair edits", nodeUnit, shape.FragmentPath, repairUnitPath),
			"install the package this release ships, which carries the unit", stateNothing)
	case firstProp(props, "DropInPaths") != "":
		return shape, false, "", endpointRefuse(endpointReasonShape, fmt.Sprintf("%s has drop-ins (%s), which a repair "+
			"does not admit", nodeUnit, firstProp(props, "DropInPaths")), "", stateNothing)
	case firstProp(props, "NeedDaemonReload") != "no":
		return shape, false, "", endpointUnknown(endpointReasonShape, fmt.Sprintf("%s has NeedDaemonReload=%s, so the "+
			"file on disk is not the unit systemd runs", nodeUnit, orUnknownWord(firstProp(props, "NeedDaemonReload"))),
			"systemctl daemon-reload, then converge again", stateNothing)
	}

	return shape, true, "", nil
}

// proveRepairLoaded is the manager's word after the reload, every condition the
// start rests on: the role's fragment, loaded, no drop-in, no reload pending,
// and exactly the repaired runtime directories.
func proveRepairLoaded(ctx context.Context, insp *lifeops.Inspector) *endpointRefusal {
	props, shape, r := observeRepairShapeProps(ctx, insp, stateUnitInstalled)
	if r != nil {
		return r
	}

	want := strings.Fields(strings.TrimPrefix(repairedRuntimeDirectory, "RuntimeDirectory="))

	if shape.LoadState != "loaded" || shape.FragmentPath != repairUnitPath || firstProp(props, "DropInPaths") != "" ||
		firstProp(props, "NeedDaemonReload") != "no" || !slices.Equal(strings.Fields(shape.RuntimeDirectory), want) {
		return endpointUnknown(endpointReasonShape, fmt.Sprintf("after the reload systemd loads %s as LoadState=%s from "+
			"%s with drop-ins %q, NeedDaemonReload=%s and RuntimeDirectory=%s, which is not the repair alone", nodeUnit,
			orUnknownWord(shape.LoadState), orUnknownWord(shape.FragmentPath), firstProp(props, "DropInPaths"),
			orUnknownWord(firstProp(props, "NeedDaemonReload")), shape.RuntimeDirectory),
			"inspect the unit, then converge again", stateUnitInstalled)
	}

	return nil
}

// readRepairUnit reads the role's unit once, by descriptor and without
// following a symlink, root's (judged on the file the descriptor is open on),
// bounded, and holding exactly the admitted line.
func readRepairUnit() ([]byte, *endpointRefusal) {
	f, info, err := regularfile.Open(repairUnitPath, regularfile.Options{NoFollow: true})
	if err != nil {
		return nil, endpointUnknown(endpointReasonShape, "open "+repairUnitPath+": "+err.Error(), "", stateNothing)
	}

	defer func() { _ = f.Close() }()

	if err := repairUnitOwned(info); err != nil {
		return nil, endpointRefuse(endpointReasonShape, err.Error(), "", stateNothing)
	}

	body, err := regularfile.ReadAllLimited(f, repairUnitPath, maxUnitBytes)
	if err != nil {
		return nil, endpointUnknown(endpointReasonShape, "read "+repairUnitPath+": "+err.Error(), "", stateNothing)
	}

	if _, problem := repairRuntimeDirectory(body); problem != "" {
		return nil, endpointRefuse(endpointReasonShape, problem, "", stateNothing)
	}

	return body, nil
}

// repairRuntimeDirectory is the unit with its one admitted line changed, or a
// problem: the line must stand exactly once, in [Service], and be the only
// RuntimeDirectory assignment in the file.
func repairRuntimeDirectory(body []byte) ([]byte, string) {
	// LINES AS SYSTEMD READS THEM, OR NONE: a carriage return or a NUL ends a
	// line for systemd and not here, and a trailing backslash folds the next line
	// into this one, so a file carrying any of them is refused rather than read
	// differently from the manager that loads it.
	if bytes.ContainsAny(body, "\r\x00") {
		return nil, repairUnitPath + " carries a carriage return or a NUL, which this repair does not read"
	}

	lines := strings.SplitAfter(string(body), "\n")
	section, at, assignments := "", -1, 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasSuffix(trimmed, `\`) {
			return nil, fmt.Sprintf("%s continues line %d onto the next, which this repair does not read", repairUnitPath,
				i+1)
		}

		switch {
		case strings.HasPrefix(trimmed, "["):
			section = trimmed
		case strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
		case strings.HasPrefix(strings.Map(dropBlank, trimmed), "RuntimeDirectory="):
			assignments++

			if section == "[Service]" && strings.TrimSuffix(line, "\n") == legacyRuntimeDirectory {
				at = i
			}
		}
	}

	if assignments != 1 || at < 0 {
		return nil, fmt.Sprintf("%s does not hold exactly one RuntimeDirectory assignment, the line %q in [Service]",
			repairUnitPath, legacyRuntimeDirectory)
	}

	lines[at] = repairedRuntimeDirectory + "\n"

	return []byte(strings.Join(lines, "")), ""
}

// dropBlank removes the spaces and tabs systemd allows around a key.
func dropBlank(r rune) rune {
	if r == ' ' || r == '\t' {
		return -1
	}

	return r
}

func digestHex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}
