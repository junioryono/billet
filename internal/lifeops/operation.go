package lifeops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
// may be managed by its own directory directives.
type OperationProtection struct {
	Units     []string
	Paths     []string
	UnitPaths map[string][]string
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
}

// AdmitOperations is read-only and valid only at this boundary. It checks the
// whole sequence, then rereads the evidence so a changed definition refuses.
func (i *Inspector) AdmitOperations(ctx context.Context, sequence []Operation, protection OperationProtection) error {
	w := operationWalk{inspector: i, protection: protection, units: make(map[string]operationEvidence), targets: make(map[string]bool)}
	for _, op := range sequence {
		w.targets[op.Unit] = true
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
	required := slices.Clone(names)
	if !scoped {
		required = slices.DeleteFunc(required, func(name string) bool {
			return slices.Contains([]string{"OnSuccessJobMode", "OnFailureJobMode", "FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction"}, name)
		})
	}
	if err := requireOperationProperties(unit, props, required); err != nil {
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
	if strings.HasSuffix(unit, ".service") {
		execution, err := w.inspector.operationExecution(ctx, unit, scoped)
		if err != nil {
			return operationEvidence{}, fmt.Errorf("operation-evidence-unreadable: %s directories: %w", unit, err)
		}
		required := operationExecutionProperties
		if !scoped {
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
		if effect != op && protectedOperationUnit(effect.Unit, w.protection.Units) {
			return fmt.Errorf("operation-protected-effect: %s %s reaches %s %s", op.Verb, op.Unit, effect.Verb, effect.Unit)
		}
		ev, err := w.get(ctx, effect.Unit)
		if err != nil {
			return err
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
		// Relationship closure is always inspected, including active dependencies.
		// Commands and manager actions are refused on the target. A helper path
		// reaching a protected unit or directory refuses at that resource below,
		// independently of whether the helper has commands of its own.
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

func admitOperationCommands(unit string, ev operationEvidence) error {
	for _, prop := range []string{"OnSuccessJobMode", "OnFailureJobMode"} {
		if first(ev.props, prop) != "replace" {
			return fmt.Errorf("operation-job-mode: %s %s=%s", unit, prop, first(ev.props, prop))
		}
	}
	for _, prop := range []string{"FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction"} {
		if first(ev.props, prop) != "none" {
			return fmt.Errorf("operation-manager-action: %s %s=%s", unit, prop, first(ev.props, prop))
		}
	}
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
		if where == "/" || Contained(where, path) || Contained(path, where) {
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
	if hasDirectories && strings.HasSuffix(effect.Unit, ".service") {
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
				if Contained(path, keep) || Contained(keep, path) {
					return fmt.Errorf("operation-directory-overlap: %s %s=%s affects %s", effect.Unit, directive, entry, keep)
				}
			}
			if err := operationDirectoryPath(path); err != nil {
				return fmt.Errorf("operation-directory-evidence: %s %s: %w", effect.Unit, directive, err)
			}
		}
	}
	return nil
}

// Directory mappings with links require namespace analysis outside the supported
// subset. Inspect every existing prefix without following one or creating any.
func operationDirectoryPath(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a direct directory", current)
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
			if Contained(keep, path) || Contained(path, keep) {
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
