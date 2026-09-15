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
		UnitPaths: map[string][]string{serverUnit: {j.IdentityDir}},
	}
	if j.Phase == retirement.PhaseIntent {
		p.QuietExceptions = []string{upgradeTimerUnit, backupTimerUnit}
	}
	if j.Phase == retirement.PhaseIntent || j.Phase == retirement.PhaseStopped {
		p.WaitingUnits = []string{backupServiceUnit}
	}
	if j.RetainedInvocation != nil {
		for _, service := range j.RetainedInvocation.Services {
			p.Units = append(p.Units, service.Unit)
			p.RequiredActive = append(p.RequiredActive, service.Unit)
		}
		for _, resource := range j.RetainedInvocation.Resources {
			if resource.GuestNetwork {
				p.Paths = append(p.Paths, resource.Path, resource.ResolvedPath)
			} else {
				p.UnitPaths[nodeUnit] = append(p.UnitPaths[nodeUnit], resource.Path, resource.ResolvedPath)
			}
		}
	}
	return p
}

func admitRetireOperations(ctx context.Context, j retirement.Journal, operations []lifeops.Operation) *retireRefusal {
	if j.Variant == retirement.VariantRetainedNode && (j.RetainedInvocation == nil || j.RetainedInvocation.Provider == "") {
		return retireUnknown(retireReasonStopped, "the journal has no original retained-node provider and invocation evidence", "")
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
	if first.Unit == backupTimerUnit {
		protection.QuietExceptions = slices.DeleteFunc(protection.QuietExceptions, func(unit string) bool {
			return unit == upgradeTimerUnit || first.Verb != "stop"
		})
	} else if first.Unit == upgradeTimerUnit && first.Verb != "stop" {
		protection.QuietExceptions = slices.DeleteFunc(protection.QuietExceptions, func(unit string) bool { return unit == upgradeTimerUnit })
	} else if first.Unit != upgradeTimerUnit {
		protection.QuietExceptions = nil
	}
	if err := retireOperationInspector().AdmitOperations(ctx, operations, protection); err != nil {
		return retireUnknown(retireReasonEffects, err.Error(), "inspect the named unit and its effective sources; retry with current evidence")
	}
	return nil
}

func admitRetireOperation(ctx context.Context, j retirement.Journal, verb, unit string) *retireRefusal {
	return admitRetireOperations(ctx, j, []lifeops.Operation{{Verb: verb, Unit: unit}})
}

func admitRetireRemaining(ctx context.Context, m retireMode, j retirement.Journal) *retireRefusal {
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
		if r := proveRetireInvocation(ctx, j.RetainedInvocation); r != nil {
			return r
		}
	}
	if j.Variant == retirement.VariantRetainedNode {
		if r := proveRetireNodeExecution(ctx, m.configPath); r != nil {
			return r
		}
	}
	return proveRetireActivation(ctx, j.Phase == retirement.PhaseIntent, j.Phase == retirement.PhaseStopped)
}

// captureRetireInvocation runs before intent. A resumed journal never invents
// this evidence from the invocation which happens to be running at resume.
func captureRetireInvocation(ctx context.Context, cfg *config.Config) (*retirement.RetainedInvocation, *retireRefusal) {
	if cfg.Node == nil {
		return nil, nil
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
		Provider: string(cfg.Node.Provider)}
	for _, unit := range services {
		service, err := observeRetireService(ctx, unit)
		if err != nil {
			return nil, retireUnknown(retireReasonStopped, err.Error(), "")
		}
		evidence.Services = append(evidence.Services, service)
	}
	paths := append(slices.Clone(servicePaths), filepath.Dir(registrationRecordPath))
	for _, path := range nodePathsOf(cfg) {
		paths = append(paths, path.path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		resource, err := observeRetireResource(path)
		if err != nil {
			return nil, retireUnknown(retireReasonStopped, err.Error(), "")
		}
		resource.GuestNetwork = slices.Contains(servicePaths, path)
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
	r.Device, r.Inode, r.Mode, r.UID, r.GID = uint64(st.Dev), st.Ino, uint32(info.Mode()), st.Uid, st.Gid
	return r, nil
}

func proveRetireInvocation(ctx context.Context, want *retirement.RetainedInvocation) *retireRefusal {
	refuse := func(why string) *retireRefusal { return retireUnknown(retireReasonStopped, why, "") }
	if want == nil || want.Provider == "" || want.InvocationID == "" || want.MainPID == "" || len(want.Resources) == 0 {
		return refuse("the journal has no original retained-node invocation and resource evidence")
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
		now.GuestNetwork = expected.GuestNetwork
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
	return retireQuietJob(ctx, insp, nodeUnit, true)
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
	return proveRetireActivation(ctx, false)
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
