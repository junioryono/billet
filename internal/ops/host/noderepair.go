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
// [Service] line, under a running node that has published no record. That line
// becomes RuntimeDirectory=billet/locks billet/registration and nothing else in
// the file changes; the converge that follows installs the current unit. The
// node is stopped under the migration's own proof (inactive, dead, success),
// the unit installed and reloaded, the node started, and the answer waits for a
// record the new invocation wrote naming the expected identity and the
// installed endpoint. A restart here is maintenance the operator's converge
// authorises: it claims no endpoint continuity it did not prove, and the
// endpoint decision is then made from scratch.
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

// repairUnitOwned refuses a unit file root does not own; a variable so a test,
// which is not root, can admit its own fixture.
var repairUnitOwned = func(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return fmt.Errorf("%s is not owned by root", repairUnitPath)
	}

	return nil
}

type repairMode struct {
	configPath        string
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

// NodeRepairUnit is `billet node repair-unit`.
func NodeRepairUnit(ctx context.Context, env cli.Env, args []string) error {
	flags := cli.NewFlagSet("billet node repair-unit", env.Stdout)
	configPath := flags.String("config", "", "the installed configuration (required)")
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

	m := repairMode{configPath: *configPath, stopTimeout: *stopTimeout, wait: *wait, dryRun: *dryRun}

	switch {
	case m.configPath == "":
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonCombination,
			"--config names the installed configuration, and it is empty",
			"repair-unit --config PATH [--stop-timeout D] [--wait D] [--dry-run] --json", stateNothing))
	case m.stopTimeout <= 0 || m.wait <= 0:
		return answerEndpointRefusal(env, endpointRefuse(endpointReasonCombination,
			"--stop-timeout and --wait must be positive",
			"repair-unit --config PATH [--stop-timeout D] [--wait D] [--dry-run] --json", stateNothing))
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

	// (2) THE LOADED UNIT'S SHAPE.
	shape, required, why, r := observeRepairShape(ctx, insp)
	if r != nil {
		return nil, r
	}

	var unitBody []byte

	if required {
		// (3) THE FILE, read once by descriptor: exactly the role's, root's,
		// with exactly the one admitted line.
		unitBody, r = readRepairUnit()
		if r != nil {
			return nil, r
		}
	}

	// (4) THE RUNNING NODE, and its record read inside a bracket: a repair is
	// for a node that runs and publishes nothing.
	var br bracketedRecord

	if required {
		var problem string

		br, problem = readRecordUnderBracket(ctx, insp, nodeUnit, identity, time.Time{})
		if problem != "" {
			return nil, endpointUnknown(endpointReasonUnit, problem, "", stateNothing)
		}

		switch {
		case !br.running:
			required, why = false, "the node is not running, so the converge installs the current unit before it starts"
		case br.class == recordUsable:
			return nil, endpointRefuse(endpointReasonRecord, "the running node published a record although its unit "+
				"declares no directory for one; inspect the host", "", stateNothing)
		case br.class != recordAbsent:
			return nil, endpointUnknown(endpointReasonRecord, "the running node's record cannot be judged: "+br.why, "",
				stateNothing)
		default:
			why = "runtime-unit repair required; effective endpoint unknown: the running node cannot publish its " +
				"registration record, because its unit declares no " + deploy.NodeRegistrationDirectory + " directory"
		}
	}

	if m.dryRun {
		if problem := closeInstalledConfig(installed); problem != "" {
			return nil, endpointUnknown(endpointReasonConfig, problem, "", stateNothing)
		}

		return repairReport{Schema: endpointSchema, Outcome: outcomeReported, Required: required, Why: why,
			Unit: shape}, nil
	}

	if !required {
		if problem := closeInstalledConfig(installed); problem != "" {
			return nil, endpointUnknown(endpointReasonConfig, problem, "", stateNothing)
		}

		return repairUnchanged{Schema: endpointSchema, Outcome: outcomeUnchanged, Why: why, Unit: shape}, nil
	}

	// (5) THE LIFECYCLE LOCK, so a `local up` or `local down` cannot act on
	// the node between this stop and this start.
	lock, err := lifecycleLock()
	if err != nil {
		return nil, endpointUnknown(endpointReasonUnit, "take the lifecycle lock: "+err.Error(), "", stateNothing)
	}

	//nolint:errcheck // the answer is JSON alone and has no place for a warning, and the process's exit releases the flock whatever this returns.
	defer func() { _ = lock.Release() }()

	return repairUnderLock(ctx, m, insp, installed, installedEP, identity, br, unitBody)
}

// repairUnderLock is steps (6) to (13): the stop, the unit, the start and the
// record, under the lifecycle lock.
func repairUnderLock(ctx context.Context, m repairMode, insp *lifeops.Inspector, installed *installedConfigObservation,
	installedEP endpoint.Endpoint, identity registrationIdentity, br bracketedRecord, unitBody []byte,
) (any, *endpointRefusal) {
	// (6) THE PROCESS AGAIN, IMMEDIATELY BEFORE THE STOP: the same one the
	// bracket read, not stopping, under a KillMode whose stop proves something.
	if r := admitNodeStop(ctx, insp, br.obs, "repaired"); r != nil {
		return nil, r
	}

	// (7) THE STOP, under the repair's own deadline, and its proof.
	stopCtx, cancelStop := context.WithTimeout(ctx, m.stopTimeout)
	_, stopErr := lifeops.NewConverger(insp).StopAndProve(stopCtx, nodeUnit)
	cancelStop()

	if stopErr != nil && errors.Is(stopCtx.Err(), context.DeadlineExceeded) {
		return nil, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the stop of %s did not complete within the "+
			"repair's deadline of %s; the stop continues under systemd's own bound, and nothing else was signalled",
			nodeUnit, m.stopTimeout), "wait for the stop to complete, then converge again", stateNothing)
	}

	stopped, r := proveNodeStopped(ctx, insp)
	if r != nil {
		return nil, r
	}

	// (8) THE FILE AGAIN, before it is replaced: bytes changed under the stop
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

	// (9) THE INSTALL, through the durable installer's one ordering, with the
	// file's own mode.
	info, err := os.Lstat(repairUnitPath)
	if err != nil {
		return nil, endpointUnknown(endpointReasonShape, "examine "+repairUnitPath+": "+err.Error(), "", stateStopped)
	}

	if _, err := (durablefile.Installer{}).Install(filepath.Dir(repairUnitPath), filepath.Base(repairUnitPath),
		info.Mode().Perm(), func(w io.Writer) error {
			_, err := w.Write(repaired)

			return err
		}); err != nil {
		return nil, endpointUnknown(endpointReasonShape, "install the repaired unit: "+err.Error(),
			"inspect "+repairUnitPath+", then converge again", stateStopped)
	}

	// (10) THE RELOAD, and the manager's own word that it loaded the repair.
	if err := insp.DaemonReload(ctx); err != nil {
		return nil, endpointUnknown(endpointReasonShape, "reload systemd: "+err.Error(),
			"systemctl daemon-reload, then converge again", stateUnitInstalled)
	}

	loaded, _, _, r := observeRepairShape(ctx, insp)
	if r != nil {
		r.State = stateUnitInstalled

		return nil, r
	}

	if loaded.FragmentPath != repairUnitPath || !slices.Equal(strings.Fields(loaded.RuntimeDirectory),
		strings.Fields(strings.TrimPrefix(repairedRuntimeDirectory, "RuntimeDirectory="))) {
		return nil, endpointUnknown(endpointReasonShape, fmt.Sprintf("after the reload systemd loads %s from %s "+
			"declaring RuntimeDirectory=%s, not the repair", nodeUnit, loaded.FragmentPath, loaded.RuntimeDirectory),
			"inspect the unit, then converge again", stateUnitInstalled)
	}

	// (11) THE START, under the unit's own start bound.
	startCtx, cancelStart := context.WithTimeout(ctx, deploy.UnitStartTimeout+lifecycleDeadlineMargin)
	_, startErr := lifeops.NewConverger(insp).StartAndProve(startCtx, nodeUnit)
	cancelStart()

	if startErr != nil {
		return nil, endpointUnknown(endpointReasonUnproved, "the repaired unit is installed and the node did not start: "+
			startErr.Error(), "systemctl start "+nodeUnit+", then converge again", stateUnitInstalled)
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

// observeRepairShape reads the loaded node unit and says whether it is the one
// shape a repair admits (required), a unit that needs none (it declares the
// registration directory), or one refused.
func observeRepairShape(ctx context.Context, insp *lifeops.Inspector) (repairShape, bool, string, *endpointRefusal) {
	props, err := insp.UnitProperties(ctx, nodeUnit, "LoadState", "FragmentPath", "DropInPaths", "NeedDaemonReload",
		"RuntimeDirectory")
	if err != nil {
		return repairShape{}, false, "", endpointUnknown(endpointReasonUnit, "ask systemd about "+nodeUnit+": "+err.Error(),
			"", stateNothing)
	}

	shape := repairShape{LoadState: firstProp(props, "LoadState"), FragmentPath: firstProp(props, "FragmentPath"),
		RuntimeDirectory: firstProp(props, "RuntimeDirectory")}

	// EVERY PROPERTY ANSWERED: an absent one is could-not-tell, never empty.
	for _, name := range []string{"LoadState", "FragmentPath", "DropInPaths", "NeedDaemonReload", "RuntimeDirectory"} {
		if _, ok := props[name]; !ok {
			return shape, false, "", endpointUnknown(endpointReasonUnit, "systemd answered no "+name+" for "+nodeUnit, "",
				stateNothing)
		}
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
			"install the units this release ships (converge with a release that carries them, or install the package)",
			stateNothing)
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

// readRepairUnit reads the role's unit once, by descriptor and without
// following a symlink, root's, bounded, and holding exactly the admitted line.
func readRepairUnit() ([]byte, *endpointRefusal) {
	info, err := os.Lstat(repairUnitPath)
	if err != nil {
		return nil, endpointUnknown(endpointReasonShape, "examine "+repairUnitPath+": "+err.Error(), "", stateNothing)
	}

	if err := repairUnitOwned(info); err != nil {
		return nil, endpointRefuse(endpointReasonShape, err.Error(), "", stateNothing)
	}

	body, err := regularfile.ReadFile(repairUnitPath, maxUnitBytes, regularfile.Options{NoFollow: true})
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
