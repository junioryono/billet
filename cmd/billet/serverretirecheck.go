package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/retirement"
)

// The operations member is retained verbatim: whitespace, ordering and every
// proposed operand participate in the verdict's operation digest.
type retireNodeConfigInput struct {
	Schema          int             `json:"schema"`
	Run             string          `json:"run"`
	Retiring        string          `json:"retiring"`
	TransitionID    string          `json:"transition_id"`
	Rendering       string          `json:"rendering"`
	RenderingSHA256 string          `json:"rendering_sha256"`
	Operations      json.RawMessage `json:"operations"`
}

type retireNodeOperations struct {
	Filesystem []retireFilesystemOperation `json:"filesystem"`
	Services   []retireServiceOperation    `json:"services"`
	Units      []retireProposedUnit        `json:"units"`
}

type retireFilesystemOperation struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type retireServiceOperation struct {
	Verb string `json:"verb"`
	Unit string `json:"unit"`
}

// Unit replacement initially admits the installed supported definition, byte
// for byte. New execution semantics need their own effects model before they
// can be authorized by a read-only check of the currently loaded manager.
type retireProposedUnit struct {
	Unit     string `json:"unit"`
	Path     string `json:"path"`
	Contents string `json:"contents"`
	SHA256   string `json:"sha256"`
}

func checkRetireNodeConfigCombination(m retireMode) *retireRefusal {
	refuse := func(why string) *retireRefusal { return retireRefuse(retireReasonCombination, why, "") }
	if m.input != "-" || m.configPath == "" || m.run == "" || m.retiringHost == "" || !guardHex32.MatchString(m.transition) ||
		m.expectedHolder != m.run || !guardHex32.MatchString(m.expectedGuard) {
		return refuse("--check-node-config requires --input -, --config, --run, --retiring-host, --transition, --expected-holder equal to --run and --expected-guard")
	}
	if m.reserve || m.abandon || m.completeRow || m.acknowledge || m.dryRun || m.requested || m.serverOnly ||
		m.survivorFlagged || m.failoverVerified || m.reservationFresh || m.reportAgeFlagged || len(m.sharedAddresses) != 0 ||
		m.survivorHost != "" || m.installedSHA != "" || m.asHost != "" || m.completion != "" || m.answer != "" || m.environmentFile != "" {
		return refuse("--check-node-config accepts no mutation, classification or fresh-request operands")
	}
	for _, holder := range []string{m.run, m.retiringHost} {
		if err := checkHolder(holder); err != nil {
			return refuse(err.Error())
		}
	}
	return nil
}

func decodeRetireNodeConfig(raw []byte, m retireMode) (retireNodeConfigInput, retireNodeOperations, *retireRefusal) {
	var in retireNodeConfigInput
	var operations retireNodeOperations
	refuse := func(why string) (retireNodeConfigInput, retireNodeOperations, *retireRefusal) {
		return in, operations, retireRefuse(retireReasonInput, why, "")
	}
	if err := retirement.DecodeDocument(raw, &in); err != nil {
		return refuse("node-config document: " + err.Error())
	}
	if in.Schema != 1 || in.Run != m.run || in.Retiring != m.retiringHost || in.TransitionID != m.transition ||
		len(in.Rendering) == 0 || len(in.Rendering) > retirement.MaxStageBytes || retirement.Digest([]byte(in.Rendering)) != in.RenderingSHA256 {
		return refuse("node-config document schema, run, host, transition or exact rendering digest does not match")
	}
	if err := retirement.DecodeDocument(in.Operations, &operations); err != nil {
		return refuse("node-config operations: " + err.Error())
	}
	if operations.Filesystem == nil || operations.Services == nil || operations.Units == nil {
		return refuse("node-config operations require filesystem, services and units arrays, including when empty")
	}
	return in, operations, nil
}

// takeRetireInspectionLock opens only existing trusted objects. It must not use
// takeTxLock, whose preparation creates and flushes records even on a refusal.
func takeRetireInspectionLock() (*txLock, error) {
	root, err := openTrustedDir(upgradeRoot)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat()
	if err == nil {
		err = requireTrustedDir(upgradeRoot, info, 0)
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	f, info, err := regularfile.OpenAt(root, txLockName)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	tx := &txLock{f: f, dir: root}
	if err := requireTrustedFile(filepath.Join(upgradeRoot, txLockName), info); err != nil {
		tx.release()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		tx.release()
		return nil, err
	}
	if err := requireRootInPlace(tx); err != nil {
		tx.release()
		return nil, err
	}
	return tx, nil
}

func retireCheckNodeConfig(ctx context.Context, m retireMode) (any, *retireRefusal) {
	raw, r := readRetireDocument(4 * retirement.MaxStageBytes)
	if r != nil {
		return nil, r
	}
	in, operations, r := decodeRetireNodeConfig(raw, m)
	if r != nil {
		return nil, r
	}
	root, err := takeRetireInspectionLock()
	if err != nil {
		return nil, retireUnknown(retireReasonLock, err.Error(), "")
	}
	defer root.release()
	// Acquire with no creation privilege; there is no ownership hand-back.
	hold, err := retirement.Acquire(ctx, retirement.AcquireOptions{})
	if err != nil {
		return nil, retireUnknown(retireReasonLock, err.Error(), "")
	}
	defer func() { _ = hold.Release() }()
	j, activity, r := observeRetireOrdinaryEntry(ctx, m, root)
	if r != nil {
		return nil, r
	}
	installed, er := observeInstalledConfig(m.configPath, true)
	if er != nil {
		return nil, retireFromEndpoint(er)
	}
	future, err := config.Parse("future node configuration", []byte(in.Rendering))
	if err != nil {
		return nil, retireRefuse(retireReasonConfig, "future rendering is not a valid configuration", "")
	}
	var literal config.Config
	if err := yaml.Unmarshal([]byte(in.Rendering), &literal); err != nil {
		return nil, retireRefuse(retireReasonConfig, "future rendering cannot be read literally", "")
	}
	if literal.Node != nil {
		for _, path := range nodePathsOf(&literal) {
			if path.name == "node.deployment_id" && literal.Node.StateDir == "" {
				continue
			}
			if !supportedRetirePath(path.path) {
				return nil, retireRefuse(retireReasonNodePath, "unsupported literal node path spelling", "")
			}
		}
	}
	if !installed.present || installed.cfg.Node == nil || installed.cfg.Server != nil || future.Node == nil || future.Server != nil ||
		future.GitHub != nil || len(future.Targets) != 0 || future.Backup != nil {
		return nil, retireRefuse(retireReasonConfig, "installed and future configurations must be serverless retained-node configurations", "")
	}
	if future.Node.StateDir != installed.cfg.Node.StateDir {
		return nil, retireRefuse(retireReasonConfig, "future rendering changes the retained node state directory", "")
	}
	identity := expectedRegistrationIdentity(installed.cfg)
	if identity.why != "" || identity.contradiction != "" || identity.absent || identity.deployment != j.Deployment {
		return nil, retireUnknown(retireReasonIdentity, "installed retained node identity is not proved against the archived deployment", "")
	}
	if future.Node.Name == "" || future.Node.Name != identity.node {
		return nil, retireRefuse(retireReasonIdentity, "future rendering changes the retained node identity", "")
	}
	openingEnvironment, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil {
		return nil, retireUnknown(retireReasonEffects, err.Error(), "")
	}
	if r := admitRetireNodeOperations(ctx, m, j, future, operations); r != nil {
		return nil, r
	}
	// These readers may block. Rebind every record and installed operand at
	// the end; a successful answer is an observation, never a reusable token.
	after, afterActivity, r := observeRetireOrdinaryEntry(ctx, m, root)
	if r != nil {
		return nil, r
	}
	current, er := observeInstalledConfig(m.configPath, true)
	if er != nil {
		return nil, retireFromEndpoint(er)
	}
	if !reflect.DeepEqual(j, after) || activity != afterActivity || !current.present || !reflect.DeepEqual(installed.body, current.body) {
		return nil, retireUnknown(retireReasonMismatch, "retained records, node activity or installed configuration changed during admission", "")
	}
	if r := admitRetireNodeOperations(ctx, m, j, future, operations); r != nil {
		return nil, r
	}
	closingEnvironment, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil || !slices.Equal(openingEnvironment, closingEnvironment) {
		return nil, retireUnknown(retireReasonEffects, "EnvironmentFiles changed across ordinary preflight", "")
	}
	latest, er := observeInstalledConfig(m.configPath, true)
	if er != nil || !latest.present || !reflect.DeepEqual(installed.body, latest.body) {
		return nil, retireUnknown(retireReasonMismatch, "installed configuration changed after operation admission", "")
	}
	if err := requireRootInPlace(root); err != nil {
		return nil, retireUnknown(retireReasonGuard, err.Error(), "")
	}
	return &retirement.NodeConfigVerdict{Schema: 1, Purpose: "node-config", Outcome: "admitted", Run: m.run,
		Guard: m.expectedGuard, Retiring: m.retiringHost, Deployment: j.Deployment, TransitionID: m.transition,
		Variant: j.Variant, RenderingSHA256: in.RenderingSHA256, OperationsSHA256: retirement.Digest(in.Operations),
		NodeActivity: activity, State: stateNothingRetire}, nil
}

// observeRetireOrdinaryEntry has no publication path and grants no drain proof.
// A separate command may reuse this observation without changing strict done.
func observeRetireOrdinaryEntry(ctx context.Context, m retireMode, root *txLock) (retirement.Journal, string, *retireRefusal) {
	var j retirement.Journal
	dir, shape, err := openGuardForMutation(root)
	if dir != nil {
		defer func() { _ = dir.Close() }()
	}
	if err != nil {
		return j, "", retireUnknown(retireReasonGuard, err.Error(), "")
	}
	if r := judgeGuardShape(shape, m.run); r != nil {
		return j, "", r
	}
	if shape.Guard.ID != m.expectedGuard || shape.Guard.Transition != nil {
		return j, "", retireRefuse(retireReasonGuard, "ordinary preflight requires the expected settled guard and no transition marker", "")
	}
	j, fact, r := readRetireJournal()
	if r != nil {
		return j, "", r
	}
	if fact != retirement.JournalFactDone || !j.Settled || !j.RowDone || j.CompletedBy == "" || j.Variant != retirement.VariantRetainedNode || j.Provenance.TransitionID != m.transition {
		return j, "", retireRefuse(retireReasonJournal, "ordinary preflight requires the matching retained, done, row-done and settled journal", "")
	}
	if j.IdentityDir != j.Locator.IdentityDir || j.Archive != j.Locator.Archive {
		return j, "", retireRefuse(retireReasonJournal, "journal archive and identity locators disagree", "")
	}
	identity, r := retireArchivedIdentity(j)
	if r != nil {
		return j, "", r
	}
	if err := j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Identity: identity}); err != nil {
		return j, "", retireRefuse(retireReasonJournal, err.Error(), "")
	}
	present, err := retireNamePresent(j.IdentityDir)
	if err != nil {
		return j, "", retireUnknown(retireReasonIdentity, err.Error(), "")
	}
	if present {
		return j, "", retireRefuse(retireReasonIdentity, "the original identity location has been recreated", "")
	}
	status, presence, err := retirement.ReadStatus()
	if err != nil || presence != retirement.StatusPresent {
		return j, "", retireUnknown(retireReasonStatus, "ordinary preflight requires readable published closure status; it performs no repair", "")
	}
	if status.Phase != retirement.PhaseDone || status.Variant != j.Variant {
		return j, "", retireRefuse(retireReasonStatus, "published closure status disagrees with the settled journal", "")
	}
	if _, r := observeRetireControllerPostconditions(ctx, m, j); r != nil {
		return j, "", r
	}
	if r := proveRetireMaskedAccount(ctx); r != nil {
		return j, "", r
	}
	activity, r := observeRetireEntryNode(ctx, m, j)
	if r != nil {
		return j, "", r
	}
	if r := admitRetireDoneProtection(ctx, m, j); r != nil {
		return j, "", r
	}
	return j, activity, nil
}

func observeRetireEntryNode(ctx context.Context, m retireMode, j retirement.Journal) (string, *retireRefusal) {
	insp := retireOperationInspector()
	props, err := insp.UnitProperties(ctx, nodeUnit, "LoadState", "ActiveState", "MainPID", "UnitFileState", "NeedDaemonReload", "User", "Group")
	if err != nil {
		return "", retireUnknown("settled-entry-node-observation-unreadable", err.Error(), "")
	}
	for _, name := range []string{"LoadState", "ActiveState", "MainPID", "UnitFileState", "NeedDaemonReload", "User", "Group"} {
		if len(props[name]) != 1 {
			return "", retireUnknown("settled-entry-node-observation-unreadable", "missing or repeated "+name, "")
		}
	}
	if !slices.Contains([]string{"loaded", "not-found", "masked", "error", "bad-setting"}, firstProp(props, "LoadState")) ||
		!knownUnitFileState(firstProp(props, "UnitFileState")) || !slices.Contains([]string{"yes", "no"}, firstProp(props, "NeedDaemonReload")) {
		return "", retireUnknown("settled-entry-node-observation-unreadable", "unfamiliar load, enablement or reload property", "")
	}
	if firstProp(props, "LoadState") != "loaded" || firstProp(props, "NeedDaemonReload") != "no" {
		return "", retireRefuse("settled-entry-node-unit-mismatch", "node must be loaded with no pending definition reload", "")
	}
	if firstProp(props, "UnitFileState") != "enabled" {
		return "", retireRefuse("settled-entry-node-enablement", "node must be persistently enabled", "")
	}
	if !slices.Contains([]string{"", "root", "0"}, firstProp(props, "User")) || !slices.Contains([]string{"", "root", "0"}, firstProp(props, "Group")) {
		return "", retireRefuse("settled-entry-node-account", "node must retain root user and group", "")
	}
	if r := retireQuietJob(ctx, insp, nodeUnit, true); r != nil {
		return "", retireUnknown("settled-entry-node-job-observation", r.Why, "")
	}
	if r := proveRetireNodeExecution(ctx, m.configPath); r != nil {
		return "", r
	}
	active := firstProp(props, "ActiveState")
	switch active {
	case "active":
		if r := proveRetireDoneRegistration(ctx, insp, m.configPath, j); r != nil {
			return "", r
		}
		return "active", nil
	case "inactive", "failed":
		pid, err := strconv.ParseUint(firstProp(props, "MainPID"), 10, 32)
		if err != nil {
			return "", retireUnknown("settled-entry-node-observation-unreadable", "malformed MainPID", "")
		}
		if pid != 0 {
			return "", retireRefuse("settled-entry-node-process-present", "quiet node still has a main process", "")
		}
		if err := insp.ProveUnitProcessesGone(ctx, nodeUnit); err != nil {
			return "", retireUnknown("settled-entry-node-process-present", err.Error(), "")
		}
		return "quiet-" + active, nil
	default:
		if !knownActiveState(active) {
			return "", retireUnknown("settled-entry-node-observation-unreadable", "unfamiliar node activity", "")
		}
		return "", retireRefuse("settled-entry-node-"+active, "node is not quiet or active", "")
	}
}

func admitRetireNodeOperations(ctx context.Context, m retireMode, j retirement.Journal, cfg *config.Config, operations retireNodeOperations) *retireRefusal {
	if cfg.Node.LockDir == "" {
		return retireUnknown(retireReasonNodePath, "node.lock_dir must be explicit: the service's environment-dependent default cannot be inferred from the inspector's environment", "")
	}
	j.RetainedInvocation = nil
	protection := retireOperationProtection(j)
	protected := append(slices.Clone(protection.Paths), j.IdentityDir)
	for _, unit := range []string{serverUnit, backupServiceUnit, backupTimerUnit, upgradeTimerUnit, "billet-upgrade.service"} {
		for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
			protected = append(protected, filepath.Join(dir, unit), filepath.Join(dir, unit+".d"))
		}
		props, err := retireOperationInspector().UnitProperties(ctx, unit, "LoadState", "FragmentPath", "DropInPaths")
		if err != nil || len(props["LoadState"]) != 1 || len(props["FragmentPath"]) != 1 || len(props["DropInPaths"]) != 1 {
			return retireUnknown(retireReasonEffects, "controller unit source paths could not be observed", "")
		}
		if path := firstProp(props, "FragmentPath"); path != "" && path != "/dev/null" {
			protected = append(protected, path)
		}
		protected = append(protected, strings.Fields(firstProp(props, "DropInPaths"))...)
	}
	protection.Paths = protected
	protection.ArchivedInputRoots = []string{j.IdentityDir}
	protection.RetainedPathUnits = []string{nodeUnit}
	protection.RequiredInputs[nodeUnit] = []string{m.configPath}
	protection.UnitPaths[nodeUnit] = []string{filepath.Dir(registrationRecordPath)}
	for _, path := range nodePathsOf(cfg) {
		if r := admitRetireFuturePath(path.path, false, protected); r != nil {
			return r
		}
		if path.name == "node.lock_dir" {
			protection.UnitPaths[nodeUnit] = append(protection.UnitPaths[nodeUnit], path.path)
		} else {
			protection.RequiredInputs[nodeUnit] = append(protection.RequiredInputs[nodeUnit], path.path)
		}
	}
	if r := admitRetireFuturePath(m.configPath, false, protected); r != nil {
		return r
	}
	environment, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "")
	}
	line, err := lifeops.RenderEnvironmentFiles(environment)
	if err != nil {
		return retireRefuse(retireReasonEffects, err.Error(), "")
	}
	for _, spec := range environment {
		if r := admitRetireFuturePath(spec.Path, false, protected); r != nil {
			return r
		}
		if _, err := readRetireEnvironment(spec); err != nil {
			return retireUnknown(retireReasonEffects, "environment file is not positively readable or positively optional and absent", "")
		}
		if !spec.IgnoreErrors {
			protection.RequiredInputs[nodeUnit] = append(protection.RequiredInputs[nodeUnit], spec.Path)
		}
	}
	nodeSource, err := retireOperationInspector().UnitProperties(ctx, nodeUnit, "FragmentPath")
	if err != nil || len(nodeSource["FragmentPath"]) != 1 || firstProp(nodeSource, "FragmentPath") == "" {
		return retireUnknown(retireReasonEffects, "node unit source could not be observed", "")
	}
	for _, op := range operations.Filesystem {
		if !slices.Contains([]string{"read", "write", "mkdir", "chown", "delete", "recursive-chown", "recursive-delete", "recursive-walk"}, op.Kind) {
			return retireRefuse(retireReasonInput, "unsupported filesystem operation kind", "")
		}
		recursive := strings.HasPrefix(op.Kind, "recursive-") || op.Kind == "delete" || op.Kind == "write" || op.Kind == "chown"
		if r := admitRetireFuturePath(op.Path, recursive, protected); r != nil {
			return r
		}
		if op.Kind != "read" && op.Kind != "mkdir" {
			target, err := lifeops.ResolveOperationPath(op.Path)
			if err != nil {
				return retireUnknown(retireReasonNodePath, err.Error(), "")
			}
			source, err := lifeops.ResolveOperationPath(firstProp(nodeSource, "FragmentPath"))
			if err != nil {
				return retireUnknown(retireReasonNodePath, err.Error(), "")
			}
			unitPath := target == source || recursive && underOrEqual(target, source)
			for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
				unitPath = unitPath || underOrEqual(dir, target) || recursive && underOrEqual(target, dir)
			}
			if unitPath && (op.Kind != "write" || !slices.ContainsFunc(operations.Units, func(unit retireProposedUnit) bool { return unit.Path == op.Path })) {
				return retireRefuse(retireReasonEffects, "a unit destination requires its exact proposed unit document", "")
			}
		}
		// Environment retention never grants a credential write or removal.
		for _, spec := range environment {
			if op.Kind != "read" && op.Kind != "mkdir" {
				resolved, err := lifeops.ResolveOperationPath(op.Path)
				env, envErr := lifeops.ResolveOperationPath(spec.Path)
				if err != nil || envErr != nil {
					return retireUnknown(retireReasonNodePath, "environment operation path could not be resolved", "")
				}
				if resolved == env || recursive && underOrEqual(resolved, env) {
					return retireRefuse(retireReasonNodePath, "retaining an environment file grants no mutation of it", "")
				}
			}
		}
	}
	for _, unit := range operations.Units {
		if unit.Unit != nodeUnit || retirement.Digest([]byte(unit.Contents)) != unit.SHA256 {
			return retireRefuse(retireReasonInput, "proposed unit must be the node and match its exact digest", "")
		}
		if r := admitRetireFuturePath(unit.Path, false, protected); r != nil {
			return r
		}
		if err := retireOperationInspector().AdmitUnitReplacement(ctx, unit.Unit, unit.Path, unit.Contents, line); err != nil {
			return retireUnknown(retireReasonEffects, err.Error(), "")
		}
	}
	services, paths, err := retireRequiredServices(cfg)
	if err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "")
	}
	protection.Units = append(protection.Units, services...)
	protection.RequiredActive = append(protection.RequiredActive, services...)
	protection.RequiredInputs[""] = paths
	var sequence []lifeops.Operation
	for _, op := range operations.Services {
		if op.Unit != nodeUnit && !slices.Contains(services, op.Unit) || !slices.Contains([]string{"start", "stop", "enable"}, op.Verb) {
			return retireRefuse(retireReasonEffects, "ordinary preflight grants only retained node and required provider service operations", "")
		}
		sequence = append(sequence, lifeops.Operation{Verb: op.Verb, Unit: op.Unit})
	}
	if err := retireOperationInspector().AdmitOperations(ctx, sequence, protection); err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "")
	}
	after, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil || !slices.Equal(environment, after) {
		return retireUnknown(retireReasonEffects, "EnvironmentFiles changed during ordinary admission", "")
	}
	return nil
}

func admitRetireFuturePath(path string, recursive bool, protected []string) *retireRefusal {
	for _, root := range protected {
		resolved, err := lifeops.ResolveOperationPath(root)
		if err != nil {
			return retireUnknown(retireReasonNodePath, err.Error(), "")
		}
		for _, spelling := range []string{root, resolved} {
			traverses, unknown, err := walkTraverses(path, spelling)
			switch {
			case err != nil:
				return retireRefuse(retireReasonNodePath, err.Error(), "")
			case unknown != "":
				return retireUnknown(retireReasonNodePath, unknown, "")
			case traverses:
				return retireRefuse(retireReasonNodePath, fmt.Sprintf("planned path traverses protected resource %s", root), "")
			}
		}
		if recursive {
			// Operations crossing a missing prefix with '..' were judged above;
			// unsupported consumer spellings cannot be cleaned just here.
			target, err := lifeops.ResolveOperationPath(path)
			if err != nil {
				return retireUnknown(retireReasonNodePath, err.Error(), "")
			}
			if underOrEqual(target, resolved) {
				return retireRefuse(retireReasonNodePath, "recursive operation contains a protected resource", "")
			}
		}
	}
	return nil
}

// Keep errors from a failed read distinct from a proved missing optional input.
func readRetireEnvironment(spec lifeops.EnvironmentFile) ([]byte, error) {
	body, err := regularfile.ReadFile(spec.Path, maxEnvironmentBytes, regularfile.Options{})
	if errors.Is(err, os.ErrNotExist) && !errors.Is(err, regularfile.ErrReopen) && spec.IgnoreErrors {
		present, observed := retireNamePresent(spec.Path)
		if observed == nil && !present {
			return nil, nil
		}
	}
	return body, err
}
