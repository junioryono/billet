package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

const retireReasonEffects = "service-operation-effects"
const retireReasonStopped = "stopped-obligations"

// The inspector seam supplies observations, never an admission verdict.
var retireOperationInspector = endpointInspector

// Preparation has no journal yet. Derive the same protected paths and roles
// from the installed request configuration without creating invocation evidence.
func admitRetirePreparation(ctx context.Context, cfg *config.Config, configPath, archive string) *retireRefusal {
	if archive == "" {
		archive = retirement.RetiredDir()
	}
	j := retirement.Journal{Phase: retirement.PhaseIntent, Variant: retirement.VariantServerOnly,
		IdentityDir: cfg.Server.IdentityDir, Archive: archive}
	return admitRetireConfiguredProtection(ctx, cfg, configPath, j)
}

func admitRetireConfiguredProtection(ctx context.Context, cfg *config.Config, configPath string, j retirement.Journal) *retireRefusal {
	p := retireOperationProtection(j)
	if j.Phase == retirement.PhaseDone {
		p.ArchivedInputRoots = nil
	}
	services, paths, err := retireRequiredServices(cfg)
	if err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "")
	}
	p.Units = append(p.Units, services...)
	p.RequiredActive = append(p.RequiredActive, services...)
	p.RequiredInputs[""] = append(p.RequiredInputs[""], paths...)
	if cfg.Node != nil {
		if r := proveRetireConfigLeaf(configPath); r != nil {
			return r
		}
		p.RetainedPathUnits = []string{nodeUnit}
		environment, err := requiredRetireEnvironmentFiles(ctx)
		if err != nil {
			return retireUnknown(retireReasonEffects, err.Error(), "")
		}
		p.RequiredInputs[nodeUnit] = append(p.RequiredInputs[nodeUnit], environment...)
		p.RequiredInputs[nodeUnit] = append(p.RequiredInputs[nodeUnit], configPath)
		p.UnitPaths[nodeUnit] = []string{filepath.Dir(registrationRecordPath)}
		for _, path := range nodePathsOf(cfg) {
			if path.name == "node.lock_dir" {
				p.UnitPaths[nodeUnit] = append(p.UnitPaths[nodeUnit], path.path)
			} else {
				p.RequiredInputs[nodeUnit] = append(p.RequiredInputs[nodeUnit], path.path)
			}
		}
	}
	if cfg.Node != nil {
		environment, err := requiredRetireEnvironmentFiles(ctx)
		if err != nil {
			return retireUnknown(retireReasonEffects, err.Error(), "")
		}
		for _, path := range environment {
			if !slices.Contains(p.RequiredInputs[nodeUnit], path) {
				return retireUnknown(retireReasonEffects, "retained-input-environment-unknown: EnvironmentFiles changed during preparation", "")
			}
			if _, _, err := hashRegular(path, maxEnvironmentBytes); err != nil {
				return retireUnknown(retireReasonEffects, "retained-input-unreadable: "+err.Error(), "")
			}
		}
	}
	if err := retireOperationInspector().AdmitOperations(ctx, nil, p); err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "")
	}
	noteRetireMutation("admission", "")
	return nil
}

// Taking the request's locks may create files. A journal past the archive must
// supply protection before that acquisition, without requiring the old config.
func admitRetireRequestPreparation(ctx context.Context, m retireMode) *retireRefusal {
	j, fact, r := readRetireJournal()
	if r != nil {
		return r
	}
	if fact == retirement.JournalFactDone {
		if _, r := observeRetirePostconditions(ctx, m, j); r != nil {
			return r
		}
		return admitRetireDoneProtection(ctx, m, j)
	}
	if fact != retirement.JournalFactAbsent {
		if r := proveRetireConfigPath(ctx, m.configPath, j); r != nil {
			return r
		}
		if r := proveRetireRequiredResources(ctx, j); r != nil {
			return retireUnknown(retireReasonEffects, r.Why, "")
		}
		if err := retireOperationInspector().AdmitOperations(ctx, nil, retireOperationProtection(j)); err != nil {
			return retireUnknown(retireReasonEffects, err.Error(), "")
		}
		if r := proveRetireRequiredResources(ctx, j); r != nil {
			return retireUnknown(retireReasonEffects, r.Why, "")
		}
		noteRetireMutation("admission", "")
		return nil
	}
	obs, r := observeRetireConfig(m.configPath)
	if r != nil {
		return r
	}
	return admitRetirePreparation(ctx, obs.cfg, m.configPath, "")
}

// Acknowledgement takes its lock under effects admission without requiring the
// current node to be healthy; its document and journal still prove the history.
func admitRetireHistoricalPreparation(ctx context.Context, m retireMode) *retireRefusal {
	j, fact, r := readRetireJournal()
	if r != nil {
		return r
	}
	if fact == retirement.JournalFactAbsent {
		return retireRefuse(retireReasonJournal, "no retirement journal exists on this host; an acknowledgement "+
			"follows a journal at done", "")
	}
	if j.Phase != retirement.PhaseDone {
		return retireRefuse(retireReasonJournal, fmt.Sprintf("the journal is at %s; the row is acknowledged at done", j.Phase), "")
	}
	return admitRetireHistoricalProtection(ctx, m, j)
}

// Historical row work still admits local effects, without a server-only
// configuration postcondition that belongs to settlement.
func admitRetireHistoricalProtection(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
	if j.Variant == retirement.VariantServerOnly {
		j.RetainedInvocation = nil
		return admitRetireConfiguredProtection(ctx, &config.Config{}, m.configPath, j)
	}
	return admitRetireDoneProtection(ctx, m, j)
}

// Completed work uses today's serverless inputs, never handoff identities.
// Row acknowledgement still records history independently of node health.
func admitRetireDoneProtection(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
	if _, r := retireConfigPostcondition(m.configPath, j); r != nil {
		return r
	}
	cfg := &config.Config{}
	if j.Variant == retirement.VariantRetainedNode {
		obs, r := observeInstalledConfig(m.configPath, true)
		if r != nil {
			return retireUnknown(retireReasonEffects, r.Why, "")
		}
		if !obs.present || obs.cfg.Server != nil || obs.cfg.Node == nil {
			return retireUnknown(retireReasonPostcondition, "the current configuration is not serverless with a retained node", "")
		}
		cfg = obs.cfg
	}
	j.RetainedInvocation = nil
	return admitRetireConfiguredProtection(ctx, cfg, m.configPath, j)
}

func retireServiceSequence(j retirement.Journal, d retirement.Decision) []lifeops.Operation {
	var sequence []lifeops.Operation
	for _, action := range retirement.RemainingServiceActions(j.Variant, j.Phase, d) {
		switch action {
		case retirement.ActionStop:
			for _, unit := range []string{upgradeTimerUnit, backupTimerUnit, serverUnit} {
				sequence = append(sequence, lifeops.Operation{Verb: "stop", Unit: unit},
					lifeops.Operation{Verb: "disable", Unit: unit})
			}
		case retirement.ActionRestart:
			for _, verb := range []string{"enable", "stop", "start"} {
				sequence = append(sequence, lifeops.Operation{Verb: verb, Unit: nodeUnit})
			}
		}
	}
	return sequence
}

func retireOperationProtection(j retirement.Journal) lifeops.OperationProtection {
	p := lifeops.OperationProtection{
		Units:      []string{serverUnit, nodeUnit, backupServiceUnit, "billet-upgrade.service", upgradeTimerUnit, backupTimerUnit},
		QuietUnits: retireQuietServices,
		Paths: []string{j.Archive, retirement.RetiredDir(), retirement.GlobalLockPath(), retirement.StatusPath(),
			retirement.ServiceAccountPath(), retirement.InitLockPath(j.IdentityDir), filepath.Join(hostLockDir, "billet-lifecycle.lock"), upgradeRoot},
		UnitPaths:          map[string][]string{serverUnit: {j.IdentityDir}},
		RequiredInputs:     make(map[string][]string),
		ArchivedInputRoots: []string{j.IdentityDir},
	}
	if j.Phase == retirement.PhaseIntent {
		p.QuietExceptions = []string{upgradeTimerUnit, backupTimerUnit}
	}
	if j.Phase == retirement.PhaseIntent || j.Phase == retirement.PhaseStopped {
		p.WaitingUnits = []string{backupServiceUnit}
	}
	if j.RetainedInvocation != nil {
		p.RetainedPathUnits = []string{nodeUnit}
		for _, service := range j.RetainedInvocation.Services {
			p.Units = append(p.Units, service.Unit)
			p.RequiredActive = append(p.RequiredActive, service.Unit)
		}
		p.RequiredInputs[nodeUnit] = []string{j.RetainedInvocation.ConfigPath}
		for _, resource := range j.RetainedInvocation.Resources {
			switch {
			case resource.GuestNetwork:
				p.RequiredInputs[""] = append(p.RequiredInputs[""], resource.Path, resource.ResolvedPath)
			case resource.Runtime:
				p.UnitPaths[nodeUnit] = append(p.UnitPaths[nodeUnit], resource.Path, resource.ResolvedPath)
			default:
				p.RequiredInputs[nodeUnit] = append(p.RequiredInputs[nodeUnit], resource.Path, resource.ResolvedPath)
			}
		}
	}
	return p
}

// Runtime is recorded by the capture's exact registration/lock classification;
// every other resource is required, including guest-network configuration.
func retainedRequiredInputs(want *retirement.RetainedInvocation) []string {
	paths := []string{want.ConfigPath}
	for _, resource := range want.Resources {
		if resource.GuestNetwork || !resource.Runtime {
			paths = append(paths, resource.Path, resource.ResolvedPath)
		}
	}
	return paths
}

func admitRetireOperations(ctx context.Context, j retirement.Journal, operations []lifeops.Operation) *retireRefusal {
	if j.Variant == retirement.VariantRetainedNode && (j.RetainedInvocation == nil || j.RetainedInvocation.Provider == "" || j.RetainedInvocation.ConfigPath == "") {
		return retireUnknown(retireReasonStopped, "the journal has no original retained-node configuration path, provider and invocation evidence", "")
	}
	if j.RetainedInvocation != nil {
		if r := proveRetireConfigPath(ctx, j.RetainedInvocation.ConfigPath, j); r != nil {
			return r
		}
		if r := proveRetireRequiredResources(ctx, j); r != nil {
			return retireUnknown(retireReasonEffects, r.Why, "")
		}
	}
	if j.RetainedInvocation != nil {
		for _, resource := range j.RetainedInvocation.Resources {
			resolved, err := lifeops.ResolveOperationPath(resource.Path)
			if err != nil || resource.ResolvedPath == "" || resolved != resource.ResolvedPath {
				return retireUnknown(retireReasonEffects, "retained path resolution changed or could not be read: "+resource.Path, "")
			}
		}
	}
	protection := retireOperationProtection(j)
	// Individual operations use the same fixed shutdown order as the driver.
	// A timer's exception expires before its disable, even while phase=intent.
	first := lifeops.Operation{}
	if len(operations) != 0 {
		first = operations[0]
	}
	switch {
	case first.Unit == backupTimerUnit:
		protection.QuietExceptions = slices.DeleteFunc(protection.QuietExceptions, func(unit string) bool {
			return unit == upgradeTimerUnit || first.Verb != "stop"
		})
	case first.Unit == upgradeTimerUnit && first.Verb != "stop":
		protection.QuietExceptions = slices.DeleteFunc(protection.QuietExceptions, func(unit string) bool { return unit == upgradeTimerUnit })
	case first.Unit != upgradeTimerUnit:
		protection.QuietExceptions = nil
	}
	if err := retireOperationInspector().AdmitOperations(ctx, operations, protection); err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "inspect the named unit and its effective sources; retry with current evidence")
	}
	if r := proveRetireRequiredResources(ctx, j); r != nil {
		return retireUnknown(retireReasonEffects, r.Why, "")
	}
	noteRetireMutation("admission", "")
	return nil
}

func admitRetireOperation(ctx context.Context, j retirement.Journal, verb, unit string) *retireRefusal {
	return admitRetireOperations(ctx, j, []lifeops.Operation{{Verb: verb, Unit: unit}})
}

func admitRetireRemaining(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
	if r := proveRetireConfigPath(ctx, m.configPath, j); r != nil {
		return r
	}
	facts, r := observeRetireFacts(ctx, m, j)
	if r != nil {
		return r
	}
	d := retirement.Decide(j.Variant, j.Phase, facts)
	if d.Action == retirement.ActionRefuse {
		return retireUnknown(retireReasonPhase, "remaining service operations could not be determined: "+d.Reason, "")
	}
	if r := admitRetireOperations(ctx, j, retireServiceSequence(j, d)); r != nil {
		return r
	}
	if j.Variant == retirement.VariantRetainedNode &&
		(j.Phase == retirement.PhaseIntent || j.Phase == retirement.PhaseStopped || j.Phase == retirement.PhaseArchived) {
		want, r := retireInvocationForConfig(j)
		if r != nil {
			return r
		}
		if r := proveRetireInvocation(ctx, want); r != nil {
			return r
		}
	}
	if j.Variant == retirement.VariantRetainedNode {
		if r := proveRetireNodeExecution(ctx, m.configPath); r != nil {
			return r
		}
	}
	if r := proveRetireActivation(ctx, j.Phase == retirement.PhaseIntent, j.Phase == retirement.PhaseStopped); r != nil {
		return r
	}
	// Invocation and execution reads may block after the first admission.
	return admitRetireOperations(ctx, j, retireServiceSequence(j, d))
}

// captureRetireInvocation runs before intent. A resumed journal never invents
// this evidence from the invocation which happens to be running at resume.
func captureRetireInvocation(ctx context.Context, cfg *config.Config, configPath string) (*retirement.RetainedInvocation, *retireRefusal) {
	if cfg.Node == nil {
		return nil, nil
	}
	if r := proveRetireConfigLeaf(configPath); r != nil {
		return nil, r
	}
	props, err := retireOperationInspector().UnitProperties(ctx, nodeUnit, retireNodeProperties...)
	if err != nil {
		return nil, retireUnknown(retireReasonStopped, "read the original node invocation: "+err.Error(), "")
	}
	invocation, pid := firstProp(props, "InvocationID"), firstProp(props, "MainPID")
	n, parseErr := strconv.ParseUint(pid, 10, 32)
	if firstProp(props, "ActiveState") != "active" || invocation == "" || parseErr != nil || n == 0 {
		return nil, retireUnknown(retireReasonStopped, "the original node invocation and process are not proved", "")
	}
	registration := readRegistrationRecord(registrationRecordPath)
	if registration.record == nil || registration.record.InvocationID != invocation {
		return nil, retireUnknown(retireReasonStopped, "the original node has no matching trusted runtime registration", "")
	}
	services, servicePaths, err := retireRequiredServices(cfg)
	if err != nil {
		return nil, retireUnknown(retireReasonStopped, err.Error(), "")
	}
	record := registration.record
	evidence := &retirement.RetainedInvocation{InvocationID: invocation, MainPID: pid,
		Deployment: record.Deployment, Node: record.Node, Incarnation: record.Incarnation, Endpoint: record.Endpoint,
		Provider: string(cfg.Node.Provider), ConfigPath: configPath, IdentityDir: cfg.Server.IdentityDir}
	for _, unit := range services {
		service, err := observeRetireService(ctx, unit)
		if err != nil {
			return nil, retireUnknown(retireReasonStopped, err.Error(), "")
		}
		evidence.Services = append(evidence.Services, service)
	}
	environment, err := requiredRetireEnvironmentFiles(ctx)
	if err != nil {
		return nil, retireUnknown(retireReasonStopped, err.Error(), "")
	}
	paths := append(slices.Clone(servicePaths), filepath.Dir(registrationRecordPath), configPath)
	paths = append(paths, environment...)
	required := append(slices.Clone(servicePaths), configPath)
	required = append(required, environment...)
	for _, path := range nodePathsOf(cfg) {
		paths = append(paths, path.path)
		if path.name != "node.lock_dir" {
			required = append(required, path.path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		resource, err := observeRetireResource(path)
		if err != nil {
			return nil, retireUnknown(retireReasonStopped, err.Error(), "")
		}
		resource.GuestNetwork = slices.Contains(servicePaths, path)
		resource.Runtime = !slices.Contains(required, path) && (path == filepath.Dir(registrationRecordPath) || path == cfg.Node.LockDir)
		evidence.Resources = append(evidence.Resources, resource)
	}
	if r := proveRetireInvocation(ctx, evidence); r != nil {
		return nil, r
	}
	return evidence, nil
}

func observeRetireResource(path string) (retirement.RetainedResource, error) {
	resolved, err := lifeops.ResolveOperationPath(path)
	r := retirement.RetainedResource{Path: path, ResolvedPath: resolved}
	if err != nil {
		return r, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, fs.ErrNotExist) {
		r.Absent = true
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("read retained resource %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return r, fmt.Errorf("read retained resource identity %s", path)
	}
	r.Device, r.Inode, r.Mode, r.UID, r.GID = devOf(st), st.Ino, uint32(info.Mode()), st.Uid, st.Gid
	return r, nil
}

func proveRetireInvocation(ctx context.Context, want *retirement.RetainedInvocation) *retireRefusal {
	refuse := func(why string) *retireRefusal { return retireUnknown(retireReasonStopped, why, "") }
	if want == nil || want.Provider == "" || want.ConfigPath == "" || want.InvocationID == "" || want.MainPID == "" || len(want.Resources) == 0 {
		return refuse("the journal has no original retained-node configuration path, invocation and resource evidence")
	}
	if err := retireOperationInspector().AdmitRetainedInputs(retainedRequiredInputs(want), want.IdentityDir); err != nil {
		return refuse(err.Error())
	}
	if r := proveRetireRequiredResources(ctx, retirement.Journal{IdentityDir: want.IdentityDir, RetainedInvocation: want}); r != nil {
		return r
	}
	if r := proveRetireServices(ctx, want); r != nil {
		return r
	}
	insp := retireOperationInspector()
	if r := retireQuietJob(ctx, insp, nodeUnit, true); r != nil {
		return r
	}
	props, err := insp.UnitProperties(ctx, nodeUnit, retireNodeProperties...)
	if err != nil {
		return refuse("read retained node: " + err.Error())
	}
	for _, name := range []string{"ActiveState", "InvocationID", "MainPID"} {
		if len(props[name]) != 1 {
			return refuse("the retained node has unknown " + name + " evidence")
		}
	}
	if firstProp(props, "ActiveState") != "active" || firstProp(props, "InvocationID") != want.InvocationID ||
		firstProp(props, "MainPID") != want.MainPID {
		return refuse("the retained node is not in its unchanged original invocation and process")
	}
	ev := readRegistrationRecord(registrationRecordPath)
	if ev.record == nil || ev.record.InvocationID != want.InvocationID || ev.record.Deployment != want.Deployment ||
		ev.record.Node != want.Node || ev.record.Incarnation != want.Incarnation || ev.record.Endpoint != want.Endpoint {
		return refuse("the retained node's current runtime registration is absent, unreadable or from another invocation")
	}
	for _, expected := range want.Resources {
		now, err := observeRetireResource(expected.Path)
		if err != nil {
			return refuse(err.Error())
		}
		now.GuestNetwork, now.Runtime = expected.GuestNetwork, expected.Runtime
		if !reflect.DeepEqual(expected, now) {
			return refuse("retained node resource changed: " + expected.Path)
		}
	}
	// Resource reads can wait; bracket them with the same process evidence.
	after, err := insp.UnitProperties(ctx, nodeUnit, retireNodeProperties...)
	if err != nil || !reflect.DeepEqual(props, after) {
		return refuse("the retained node changed while its resources were observed")
	}
	if r := proveRetireServices(ctx, want); r != nil {
		return r
	}
	if r := retireQuietJob(ctx, insp, nodeUnit, true); r != nil {
		return r
	}
	// Recheck inputs after the registration, resource and service reads.
	return proveRetireRequiredResources(ctx, retirement.Journal{IdentityDir: want.IdentityDir, RetainedInvocation: want})
}

// proveRetireStopped is shared by stopped status, stopped journal and archive.
// Backup reconciliation precedes fresh timer/controller/node observations. This
// proof never awaits a backup under the archive's authority exclusion.
func proveRetireStopped(ctx context.Context, j retirement.Journal) *retireRefusal {
	insp := retireOperationInspector()
	if retireBackupFact(ctx, insp, j) != retirement.BackupInactive {
		return retireUnknown(retireReasonStopped, "backup completion is not currently proved", "")
	}
	for _, unit := range []string{upgradeTimerUnit, backupTimerUnit, serverUnit} {
		if r := retireQuietJob(ctx, insp, unit, unit == serverUnit); r != nil {
			return r
		}
		if _, r := retireUnitPostcondition(ctx, insp, unit, unit == serverUnit, false, false); r != nil {
			return retireUnknown(retireReasonStopped, unit+": "+r.Why, "")
		}
	}
	if r := retireQuietJob(ctx, insp, backupServiceUnit, true); r != nil {
		return r
	}
	if _, r := retireUnitPostcondition(ctx, insp, backupServiceUnit, true, false, true); r != nil {
		return retireUnknown(retireReasonStopped, backupServiceUnit+": "+r.Why, "")
	}
	if j.Variant == retirement.VariantRetainedNode {
		if r := proveRetireInvocation(ctx, j.RetainedInvocation); r != nil {
			return r
		}
	}
	if err := insp.ProveUnitProcessesGone(ctx, serverUnit); err != nil {
		return retireUnknown(retireReasonStopped, err.Error(), "")
	}
	if r := proveRetireActivation(ctx, false); r != nil {
		return r
	}
	return admitRetireOperations(ctx, j, nil)
}

var retireQuietServices = []string{serverUnit, backupServiceUnit, "billet-upgrade.service"}

func proveRetireActivation(ctx context.Context, stoppingTimers bool, waitingBackup ...bool) *retireRefusal {
	var exceptions, waiting []string
	if stoppingTimers {
		exceptions = []string{upgradeTimerUnit, backupTimerUnit}
	}
	if stoppingTimers || (len(waitingBackup) != 0 && waitingBackup[0]) {
		waiting = []string{backupServiceUnit}
	}
	if err := retireOperationInspector().AdmitQuietActivation(ctx, retireQuietServices, exceptions, waiting...); err != nil {
		return retireUnknown(retireReasonStopped, err.Error(), "")
	}
	return nil
}

// The release inspector owns the loaded shape; lifeops owns command flags and
// executable identity. Reuse both judgments without interpreting program text.
func proveRetireNodeExecution(ctx context.Context, configPath string) *retireRefusal {
	svc, _ := inspectServiceSection(ctx, "node", nodeUnit, &config.Config{}, configPath, nil, "", "", nil)
	if !svc.Shape.known || svc.Shape.value != "supported" {
		why := svc.ShapeReason
		if !svc.Shape.known {
			why = svc.Shape.why
		}
		return retireUnknown(retireReasonUnit, "the current node execution shape is not supported: "+why, "")
	}
	if err := retireOperationInspector().AdmitExecution(ctx, nodeUnit, "node", installedBinary, configPath); err != nil {
		return retireUnknown(retireReasonUnit, err.Error(), "")
	}
	if err := retireOperationInspector().AdmitUnitTermination(ctx, nodeUnit); err != nil {
		return retireUnknown(retireReasonUnit, err.Error(), "")
	}
	return nil
}

func retireQuietJob(ctx context.Context, insp *lifeops.Inspector, unit string, service bool) *retireRefusal {
	names := []string{"Job"}
	if service {
		names = append(names, "ControlPID")
	}
	props, err := insp.UnitProperties(ctx, unit, names...)
	if err != nil {
		return retireUnknown(retireReasonStopped, "read quiet job evidence for "+unit+": "+err.Error(), "")
	}
	// Job is the Unit interface's (uo), rendered empty by systemctl for id 0:
	// https://github.com/systemd/systemd/blob/v255/src/core/dbus-unit.c#L215-L236.
	if len(props["Job"]) != 1 || firstProp(props, "Job") != "" {
		return retireUnknown(retireReasonStopped, unit+" has a queued job or unknown job evidence", "")
	}
	// ControlPID is separate from MainPID in the Service interface:
	// https://github.com/systemd/systemd/blob/v255/src/core/dbus-service.c.
	if service && (len(props["ControlPID"]) != 1 || firstProp(props, "ControlPID") != "0") {
		return retireUnknown(retireReasonStopped, unit+" has a control process or unknown control-process evidence", "")
	}
	return nil
}

// Bridge names come from the installed config, not currently loaded services.
func retireRequiredServices(cfg *config.Config) ([]string, []string, error) {
	if cfg.Node == nil {
		return nil, nil, nil
	}
	switch cfg.Node.Provider {
	case config.ProviderDocker, config.ProviderEC2, config.ProviderCodeBuild, config.ProviderTart:
		return nil, nil, nil
	case config.ProviderFirecracker:
		if cfg.Node.Firecracker == nil || cfg.Node.Firecracker.Bridge == "" {
			return nil, nil, errors.New("could not derive the retained provider's guest bridges")
		}
	default:
		return nil, nil, fmt.Errorf("unknown retained provider %q", cfg.Node.Provider)
	}
	units := []string{"billet-network.service"}
	paths := []string{"/etc/billet/network.nft"}
	bridges := []string{cfg.Node.Firecracker.Bridge, cfg.Node.Firecracker.UntrustedBridge}
	for _, bridge := range bridges {
		if bridge == "" {
			continue
		}
		if len(bridge) > 15 || strings.ContainsAny(bridge, "/\\@% \t\n\r") || bridge == "." || bridge == ".." {
			return nil, nil, fmt.Errorf("could not derive a DNS instance for bridge %q", bridge)
		}
		units = append(units, "billet-dnsmasq@"+bridge+".service")
		paths = append(paths, "/etc/billet/network/"+bridge+".conf", "/var/lib/billet-dnsmasq/"+bridge)
	}
	return units, paths, nil
}

func observeRetireService(ctx context.Context, unit string) (retirement.RetainedService, error) {
	service := retirement.RetainedService{Unit: unit}
	props, err := retireOperationInspector().UnitProperties(ctx, unit,
		"LoadState", "ActiveState", "InvocationID", "MainPID", "ControlPID", "Job")
	if err != nil {
		return service, err
	}
	for _, name := range []string{"LoadState", "ActiveState", "InvocationID", "MainPID", "ControlPID", "Job"} {
		if len(props[name]) != 1 {
			return service, fmt.Errorf("unknown retained service %s %s", unit, name)
		}
	}
	service.InvocationID, service.MainPID = firstProp(props, "InvocationID"), firstProp(props, "MainPID")
	pid, err := strconv.ParseUint(service.MainPID, 10, 32)
	if err != nil || (pid == 0 && unit != "billet-network.service") || service.InvocationID == "" ||
		firstProp(props, "LoadState") != "loaded" || firstProp(props, "ActiveState") != "active" ||
		firstProp(props, "ControlPID") != "0" || firstProp(props, "Job") != "" {
		return service, fmt.Errorf("retained guest-network service %s is not in a quiet active invocation", unit)
	}
	return service, nil
}

func proveRetireServices(ctx context.Context, want *retirement.RetainedInvocation) *retireRefusal {
	if want.Provider == string(config.ProviderFirecracker) && len(want.Services) < 2 {
		return retireUnknown(retireReasonStopped, "the journal has no original guest-network service evidence", "")
	}
	for _, expected := range want.Services {
		now, err := observeRetireService(ctx, expected.Unit)
		if err != nil || now != expected {
			return retireUnknown(retireReasonStopped, fmt.Sprintf("retained guest-network service %s changed or could not be read: %v", expected.Unit, err), "")
		}
	}
	return nil
}
