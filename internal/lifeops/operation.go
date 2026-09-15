package lifeops

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// Operation grants one target effect. It never grants the effects of a helper.
type Operation struct {
	Verb string
	Unit string
}

// OperationProtection describes resources which must survive the operation.
// Paths owned by the target remain protected; only UnitPaths of the target
// may be managed by its own directory directives. RequiredActive permits an
// idempotent dependency start while active, never a stop.
type OperationProtection struct {
	Units          []string
	RequiredActive []string
	Paths          []string
	UnitPaths      map[string][]string
}

// WithOperationUnitDirectories selects the system manager's installation roots.
// The default is systemctl's persistent and runtime system-unit directories.
func WithOperationUnitDirectories(dirs ...string) Option {
	return func(i *Inspector) { i.operationUnitDirs = slices.Clone(dirs) }
}

const operationUnitLimit = 512

// These are Unit interface properties, including the manager-computed inverse
// edges (not unit-file directives): systemd v255 src/core/dbus-unit.c,
// https://github.com/systemd/systemd/blob/v255/src/core/dbus-unit.c#L784-L818.
// In particular Also is NOT a runtime property; installation reads its sources.
var operationRelations = []string{
	"Requires", "Requisite", "Wants", "BindsTo", "Upholds", "PartOf",
	"RequiredBy", "RequisiteOf", "WantedBy", "BoundBy", "UpheldBy", "ConsistsOf",
	"Conflicts", "ConflictedBy", "OnSuccess", "OnFailure", "OnSuccessOf", "OnFailureOf",
	"Triggers", "TriggeredBy", "PropagatesStopTo", "StopPropagatedFrom", "JoinsNamespaceOf",
}

var operationUnitProperties = []string{
	"Id", "Names", "LoadState", "FragmentPath", "SourcePath", "DropInPaths", "NeedDaemonReload",
	"OnSuccessJobMode", "OnFailureJobMode", "FailureAction", "SuccessAction", "StartLimitAction",
	"JobTimeoutAction", "RequiresMountsFor", "ActiveState", "UnitFileState", "StopWhenUnneeded",
}

var operationDirectoryRoots = map[string]string{
	"StateDirectory": "/var/lib", "RuntimeDirectory": "/run", "CacheDirectory": "/var/cache",
	"LogsDirectory": "/var/log", "ConfigurationDirectory": "/etc",
}

// ConfigurationDirectory has no Symlink property in v255. The other four do:
// https://github.com/systemd/systemd/blob/v255/src/core/dbus-execute.c#L1026-L1040.
var operationExecutionProperties = []string{
	"StateDirectory", "RuntimeDirectory", "CacheDirectory", "LogsDirectory", "ConfigurationDirectory",
	"StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink",
	"RuntimeDirectoryPreserve", "RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths",
	"TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "DynamicUser",
	"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost",
}

type operationEvidence struct {
	props   map[string][]string
	sources []operationSource
}

type operationWalk struct {
	inspector  *Inspector
	protection OperationProtection
	units      map[string]operationEvidence
	targets    map[string]bool
	paths      map[string]operationPathBinding
}

// AdmitOperations is read-only and valid only at this boundary. It checks the
// whole sequence, then rereads the evidence so a changed definition refuses.
func (i *Inspector) AdmitOperations(ctx context.Context, sequence []Operation, protection OperationProtection) error {
	w := operationWalk{inspector: i, protection: protection, units: make(map[string]operationEvidence), targets: make(map[string]bool), paths: make(map[string]operationPathBinding)}
	for _, op := range sequence {
		w.targets[op.Unit] = true
	}
	paths := slices.Clone(protection.Paths)
	for _, owned := range protection.UnitPaths {
		paths = append(paths, owned...)
	}
	for _, path := range paths {
		if _, err := w.bindPath(path); err != nil {
			return err
		}
	}
	for _, op := range sequence {
		if op.Unit == "" || !slices.Contains([]string{"stop", "start", "enable", "disable"}, op.Verb) {
			return fmt.Errorf("operation-unsupported: %s %s", op.Verb, op.Unit)
		}
		if err := w.admit(ctx, op); err != nil {
			return fmt.Errorf("operation-effects: %s %s: %w", op.Verb, op.Unit, err)
		}
	}
	for _, unit := range sortedOperationUnits(w.units) {
		before := w.units[unit]
		after, err := w.read(ctx, unit)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("operation-evidence-changed: %s changed during admission", unit)
		}
	}
	for _, op := range sequence {
		if op.Verb == "enable" || op.Verb == "disable" {
			if err := w.admitInstallation(ctx, op); err != nil {
				return err
			}
		}
	}
	if err := w.revalidatePaths(); err != nil {
		return err
	}
	// This read follows all source/path observations, including ones that wait.
	for _, op := range sequence {
		if op.Verb == "stop" && strings.HasSuffix(op.Unit, ".service") && first(w.units[op.Unit].props, "LoadState") == "loaded" {
			if err := i.AdmitUnitTermination(ctx, op.Unit); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedOperationUnits(units map[string]operationEvidence) []string {
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (w *operationWalk) read(ctx context.Context, unit string) (operationEvidence, error) {
	names := append(slices.Clone(operationUnitProperties), operationRelations...)
	props, err := w.inspector.properties(ctx, unit, names...)
	if err != nil {
		return operationEvidence{}, fmt.Errorf("operation-evidence-unreadable: %s: %w", unit, err)
	}
	scoped := w.targets[unit] || protectedOperationUnit(unit, w.protection.Units)
	if err := requireOperationProperties(unit, props, names); err != nil {
		return operationEvidence{}, err
	}
	if first(props, "LoadState") == "not-found" {
		if first(props, "ActiveState") != "inactive" || first(props, "FragmentPath") != "" {
			return operationEvidence{}, fmt.Errorf("operation-evidence-inconsistent: absent %s", unit)
		}
		return operationEvidence{props: props}, nil
	}
	if first(props, "Id") != unit && (scoped || !slices.Contains(strings.Fields(first(props, "Names")), unit)) {
		return operationEvidence{}, fmt.Errorf("operation-source-unsupported: %s is not the canonical unit", unit)
	}
	if !operationUnitName(first(props, "Id")) || !slices.Contains(strings.Fields(first(props, "Names")), first(props, "Id")) {
		return operationEvidence{}, fmt.Errorf("operation-names-unknown: %s has no complete canonical names", unit)
	}
	if first(props, "LoadState") == "masked" && first(props, "ActiveState") == "inactive" &&
		first(props, "UnitFileState") == "masked" && first(props, "FragmentPath") == "/dev/null" {
		return operationEvidence{props: props}, nil
	}
	if first(props, "LoadState") != "loaded" || first(props, "NeedDaemonReload") != "no" {
		return operationEvidence{}, fmt.Errorf("operation-source-unsupported: %s is not a current loaded unit", unit)
	}
	if scoped && !operationPassiveUnit(unit) && (first(props, "SourcePath") != "" || first(props, "FragmentPath") == "") {
		return operationEvidence{}, fmt.Errorf("operation-source-unsupported: %s has no supported effective source", unit)
	}
	if first(props, "StopWhenUnneeded") != "yes" && first(props, "StopWhenUnneeded") != "no" {
		return operationEvidence{}, fmt.Errorf("operation-unneeded-unknown: %s", unit)
	}
	if strings.HasSuffix(unit, ".mount") || strings.HasSuffix(unit, ".automount") {
		mount, err := w.inspector.properties(ctx, unit, "Where")
		if err != nil {
			return operationEvidence{}, fmt.Errorf("operation-mount-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, mount, []string{"Where"}); err != nil {
			return operationEvidence{}, err
		}
		props["Where"] = mount["Where"]
	}
	if operationExecutionInterface(unit) != "" {
		execution, err := w.inspector.operationExecution(ctx, unit, scoped && strings.HasSuffix(unit, ".service"))
		if err != nil {
			return operationEvidence{}, fmt.Errorf("operation-evidence-unreadable: %s directories: %w", unit, err)
		}
		required := operationExecutionProperties
		if !scoped || !strings.HasSuffix(unit, ".service") {
			required = required[:len(required)-5]
		}
		if err := requireOperationProperties(unit, execution, required); err != nil {
			return operationEvidence{}, err
		}
		for key, value := range execution {
			props[key] = value
		}
	}
	if !scoped || operationPassiveUnit(unit) {
		return operationEvidence{props: props}, nil
	}
	sources, err := readOperationSources(props)
	if err != nil {
		return operationEvidence{}, fmt.Errorf("operation-source-unreadable: %s: %w", unit, err)
	}
	return operationEvidence{props: props, sources: sources}, nil
}

func requireOperationProperties(unit string, props map[string][]string, names []string) error {
	for _, name := range names {
		if len(props[name]) != 1 {
			return fmt.Errorf("operation-property-unknown: %s answered %d values for %s", unit, len(props[name]), name)
		}
	}
	return nil
}

func (w *operationWalk) get(ctx context.Context, unit string) (operationEvidence, error) {
	if !operationUnitName(unit) {
		return operationEvidence{}, fmt.Errorf("operation-unit-unsupported: %q", unit)
	}
	if ev, ok := w.units[unit]; ok {
		return ev, nil
	}
	if len(w.units) >= operationUnitLimit {
		return operationEvidence{}, fmt.Errorf("operation-traversal-bound: more than %d units", operationUnitLimit)
	}
	ev, err := w.read(ctx, unit)
	if err == nil {
		w.units[unit] = ev
	}
	return ev, err
}

func operationUnitName(unit string) bool {
	if unit == "" || !strings.Contains(unit, ".") || strings.ContainsAny(unit, "/ \t\n\r%\"'") {
		return false
	}
	for n := 0; n < len(unit); n++ {
		if unit[n] != '\\' {
			continue
		}
		if n+3 >= len(unit) || unit[n+1] != 'x' ||
			!strings.ContainsRune("0123456789abcdefABCDEF", rune(unit[n+2])) ||
			!strings.ContainsRune("0123456789abcdefABCDEF", rune(unit[n+3])) {
			return false
		}
		n += 3
	}
	return true
}

func protectedOperationUnit(unit string, protected []string) bool {
	for _, name := range protected {
		if unit == name {
			return true
		}
		base, suffix, ok := strings.Cut(name, ".")
		if ok && strings.HasPrefix(unit, base+"@") && strings.HasSuffix(unit, "."+suffix) {
			return true
		}
	}
	return false
}

func (w *operationWalk) admit(ctx context.Context, op Operation) error {
	// Observe protected units first: their inverse edges and directory ownership
	// matter even when the requested unit declares no outgoing dependency.
	for _, unit := range w.protection.Units {
		if _, err := w.get(ctx, unit); err != nil {
			return err
		}
	}
	if op.Verb == "enable" || op.Verb == "disable" {
		return w.admitInstallation(ctx, op)
	}
	queue := []Operation{op}
	seen := make(map[Operation]bool)
	for len(queue) != 0 {
		effect := queue[0]
		queue = queue[1:]
		if seen[effect] {
			continue
		}
		seen[effect] = true
		ev, err := w.get(ctx, effect.Unit)
		if err != nil {
			return err
		}
		if effect != op && protectedOperationUnit(effect.Unit, w.protection.Units) &&
			!(effect.Verb == "start" && slices.Contains(w.protection.RequiredActive, effect.Unit) && first(ev.props, "ActiveState") == "active") {
			return fmt.Errorf("operation-protected-effect: %s %s reaches %s %s", op.Verb, op.Unit, effect.Verb, effect.Unit)
		}
		for _, name := range strings.Fields(first(ev.props, "Names")) {
			if name != effect.Unit && protectedOperationUnit(name, w.protection.Units) {
				return fmt.Errorf("operation-protected-alias: %s names %s", effect.Unit, name)
			}
		}
		if first(ev.props, "LoadState") == "not-found" || first(ev.props, "LoadState") == "masked" {
			if effect == op && effect.Verb == "start" {
				return fmt.Errorf("operation-source-unavailable: start %s", effect.Unit)
			}
			// A positively absent or masked dependency cannot execute. Wants
			// may ignore it; Requires may fail the target, whose failure effects
			// are still traversed. This is different from an unreadable unit.
			continue
		}
		// Manager actions bypass the dependency graph and affect the whole host.
		// Helpers may carry commands only when their graph and directories cannot
		// reach protected resources.
		if err := admitOperationManagerEffects(effect.Unit, ev); err != nil {
			return err
		}
		if effect == op {
			if err := admitOperationCommands(effect.Unit, ev); err != nil {
				return err
			}
		}
		if err := w.admitPassiveEffect(effect, ev); err != nil {
			return err
		}
		if effect == op && effect.Verb == "start" {
			if err := w.admitMountRequirements(effect, ev); err != nil {
				return err
			}
		}
		if effect == op || effect.Verb != "start" || first(ev.props, "ActiveState") != "active" {
			if err := w.admitDirectories(effect, ev); err != nil {
				return err
			}
		}
		add := func(verb, property string) {
			for _, target := range strings.Fields(first(ev.props, property)) {
				queue = append(queue, Operation{Verb: verb, Unit: target})
			}
		}
		if effect == op || effect.Verb == "stop" || first(ev.props, "ActiveState") != "active" {
			add("start", "OnSuccess")
			add("start", "OnFailure")
		}
		if effect.Verb == "start" {
			for _, prop := range pullers {
				if prop != "PartOf" && prop != "Requisite" {
					add("start", prop)
				}
			}
			add("start", "Triggers")
			add("start", "JoinsNamespaceOf")
			add("stop", "Conflicts")
			add("stop", "ConflictedBy")
		} else {
			for _, prop := range pullers {
				for _, dependency := range strings.Fields(first(ev.props, prop)) {
					dep, err := w.get(ctx, dependency)
					if err != nil {
						return err
					}
					switch first(dep.props, "StopWhenUnneeded") {
					case "yes":
						queue = append(queue, Operation{Verb: "stop", Unit: dependency})
					case "no":
					default:
						return fmt.Errorf("operation-unneeded-unknown: %s", dependency)
					}
				}
			}
			for _, prop := range []string{"RequiredBy", "RequisiteOf", "BoundBy", "ConsistsOf", "PropagatesStopTo"} {
				add("stop", prop)
			}
			// An active upholder or activation source can restart the target
			// after a successful stop. Stopping it is not an authorized repair.
			for _, prop := range []string{"UpheldBy", "TriggeredBy"} {
				for _, source := range strings.Fields(first(ev.props, prop)) {
					from, err := w.get(ctx, source)
					if err != nil {
						return err
					}
					if first(from.props, "ActiveState") != "inactive" {
						return fmt.Errorf("operation-reactivation: %s %s=%s is not quiet", effect.Unit, prop, source)
					}
				}
			}
		}
		// Match inverse declarations too, rather than assuming fake or partial
		// evidence supplied both halves consistently.
		for unit, other := range w.units {
			for _, prop := range []string{"OnSuccessOf", "OnFailureOf"} {
				if slices.Contains(strings.Fields(first(other.props, prop)), effect.Unit) {
					queue = append(queue, Operation{Verb: "start", Unit: unit})
				}
			}
			for _, prop := range []string{"PartOf", "BindsTo", "Requires", "StopPropagatedFrom", "JoinsNamespaceOf"} {
				if effect.Verb == "stop" && slices.Contains(strings.Fields(first(other.props, prop)), effect.Unit) {
					queue = append(queue, Operation{Verb: "stop", Unit: unit})
				}
			}
		}
	}
	return nil
}

func admitOperationManagerEffects(unit string, ev operationEvidence) error {
	// v255 unit_new defaults success to fail and failure to replace. Both
	// enqueue the same dependency transaction; fail may refuse a conflict.
	// Other modes can discard jobs or isolate the host, outside this closure.
	// https://github.com/systemd/systemd/blob/v255/src/core/unit.c#L102-L103
	for _, relation := range []string{"OnSuccess", "OnFailure"} {
		prop := relation + "JobMode"
		if first(ev.props, relation) != "" && !slices.Contains([]string{"fail", "replace"}, first(ev.props, prop)) {
			return fmt.Errorf("operation-job-mode: %s %s=%s", unit, prop, first(ev.props, prop))
		}
	}
	// These are all four emergency-action properties of the v255 Unit vtable;
	// *ExitStatus and *RebootArgument configure an action but do not initiate it.
	// https://github.com/systemd/systemd/blob/v255/src/core/dbus-unit.c#L857-L875
	for _, prop := range []string{"FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction"} {
		if first(ev.props, prop) != "none" {
			return fmt.Errorf("operation-manager-action: %s %s=%s", unit, prop, first(ev.props, prop))
		}
	}
	return nil
}

func admitOperationCommands(unit string, ev operationEvidence) error {
	for _, prop := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		if first(ev.props, prop) != "" {
			return fmt.Errorf("operation-command-effects-unsupported: %s %s", unit, prop)
		}
	}
	return nil
}

func (w *operationWalk) admitPassiveEffect(effect Operation, ev operationEvidence) error {
	if !operationPassiveUnit(effect.Unit) || (effect.Verb == "start" && first(ev.props, "ActiveState") == "active") {
		return nil
	}
	if strings.HasSuffix(effect.Unit, ".slice") || strings.HasSuffix(effect.Unit, ".device") {
		return fmt.Errorf("operation-passive-effect-unknown: %s %s", effect.Verb, effect.Unit)
	}
	where := first(ev.props, "Where")
	if !filepath.IsAbs(where) || filepath.Clean(where) != where {
		return fmt.Errorf("operation-mount-unknown: %s has no canonical mount point", effect.Unit)
	}
	paths := slices.Clone(w.protection.Paths)
	for _, owned := range w.protection.UnitPaths {
		paths = append(paths, owned...)
	}
	for _, path := range paths {
		overlap, err := w.pathsOverlap(where, path)
		if err != nil {
			return err
		}
		if overlap {
			return fmt.Errorf("operation-protected-mount: %s %s covers %s", effect.Verb, effect.Unit, path)
		}
	}
	return nil
}

func (w *operationWalk) admitDirectories(effect Operation, ev operationEvidence) error {
	hasDirectories := false
	for directive := range operationDirectoryRoots {
		hasDirectories = hasDirectories || first(ev.props, directive) != ""
	}
	if hasDirectories && operationExecutionInterface(effect.Unit) != "" {
		for _, prop := range operationExecutionProperties[5 : len(operationExecutionProperties)-5] {
			want := ""
			switch prop {
			case "DynamicUser":
				want = "no"
			case "RuntimeDirectoryPreserve":
				if slices.Contains([]string{"no", "yes", "restart"}, first(ev.props, prop)) {
					continue
				}
			}
			if first(ev.props, prop) != want {
				return fmt.Errorf("operation-directory-mapping-unsupported: %s %s", effect.Unit, prop)
			}
		}
	}
	for directive, root := range operationDirectoryRoots {
		for _, entry := range strings.Fields(first(ev.props, directive)) {
			if entry == "." || strings.HasPrefix(entry, "../") || filepath.IsAbs(entry) ||
				filepath.Clean(entry) != entry || strings.ContainsAny(entry, ":\\%\"'") {
				return fmt.Errorf("operation-directory-unsupported: %s %s=%s", effect.Unit, directive, entry)
			}
			path := filepath.Join(root, entry)
			// Stop removes runtime directories; the other four survive a stop.
			// Still refuse shared ownership across protected units: a later
			// activation can recursively reown their contents.
			protected := slices.Clone(w.protection.Paths)
			for unit, paths := range w.protection.UnitPaths {
				if unit != effect.Unit {
					protected = append(protected, paths...)
				}
			}
			for unit, other := range w.units {
				if unit == effect.Unit || !protectedOperationUnit(unit, w.protection.Units) {
					continue
				}
				for otherDirective, otherRoot := range operationDirectoryRoots {
					for _, otherEntry := range strings.Fields(first(other.props, otherDirective)) {
						protected = append(protected, filepath.Join(otherRoot, otherEntry))
					}
				}
			}
			for _, keep := range protected {
				overlap, err := w.pathsOverlap(path, keep)
				if err != nil {
					return err
				}
				if overlap {
					return fmt.Errorf("operation-directory-overlap: %s %s=%s affects %s", effect.Unit, directive, entry, keep)
				}
			}
			if _, err := w.bindPath(path); err != nil {
				return fmt.Errorf("operation-directory-evidence: %s %s: %w", effect.Unit, directive, err)
			}
		}
	}
	return nil
}

func (w *operationWalk) admitMountRequirements(effect Operation, ev operationEvidence) error {
	protected := slices.Clone(w.protection.Paths)
	for unit, paths := range w.protection.UnitPaths {
		if unit != effect.Unit {
			protected = append(protected, paths...)
		}
	}
	for _, path := range strings.Fields(first(ev.props, "RequiresMountsFor")) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\\%\"'") {
			return fmt.Errorf("operation-mount-requirement-unknown: %s %s", effect.Unit, path)
		}
		for _, keep := range protected {
			overlap, err := w.pathsOverlap(path, keep)
			if err != nil {
				return err
			}
			if overlap {
				return fmt.Errorf("operation-protected-mount-requirement: %s requires %s for %s", effect.Unit, path, keep)
			}
		}
	}
	return nil
}

func operationPassiveUnit(unit string) bool {
	for _, suffix := range []string{".mount", ".automount", ".slice", ".device"} {
		if strings.HasSuffix(unit, suffix) {
			return true
		}
	}
	return false
}
