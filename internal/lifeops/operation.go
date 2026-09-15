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
// Paths remain protected even against the target. Owned paths may be managed
// by their own unit's directory directives. RequiredInputs must first pass the
// volatile-input rule even for their own stop.
// UnitPaths holds recreatable records and other operation-owned directories.
// RequiredActive permits an idempotent dependency start while active, never
// a stop. QuietUnits require
// inactive activation sources; QuietExceptions are sources this sequence stops.
// WaitingUnits defer backup activity to the caller's wait/reconciliation proof.
type OperationProtection struct {
	Units           []string
	QuietUnits      []string
	WaitingUnits    []string
	QuietExceptions []string
	RequiredActive  []string
	Paths           []string
	UnitPaths       map[string][]string
	RequiredInputs  map[string][]string
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
	"Before", "After", "PropagatesReloadTo", "ReloadPropagatedFrom", "SliceOf",
	"Following",
}

var operationUnitProperties = []string{
	"Id", "Names", "LoadState", "FragmentPath", "SourcePath", "DropInPaths", "NeedDaemonReload",
	"FailureAction", "SuccessAction", "StartLimitAction",
	"JobTimeoutAction", "RequiresMountsFor", "ActiveState", "UnitFileState", "StopWhenUnneeded", "Job",
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
	"StandardInput", "StandardOutput", "StandardError", "PAMName", "LogNamespace",
	"NetworkNamespacePath", "IPCNamespacePath", "UtmpIdentifier", "PrivateTmp",
	"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost",
}

type operationEvidence struct {
	props        map[string][]string
	sources      []operationSource
	privateTrees []string
}

type operationWalk struct {
	inspector  *Inspector
	protection OperationProtection
	units      map[string]operationEvidence
	targets    map[string]bool
	paths      map[string]operationPathBinding
	stopped    map[string]bool
	standard   map[string]bool
}

// AdmitOperations checks a closed edge list, not merely a set of unit names.
// Every relationship property in either direction must match its fixed source
// and destination classes. Completion handlers and arbitrary billet-to-billet
// edges refuse too, including effects of a backup allowed to remain in flight.
// Stop propagation admits only the ledger's What-derived device stopping that
// mount (and its inverse); retirement never stops a device.
// Standard dependencies end traversal and admit only no-op effects. Billet
// units retain directory, termination, manager-action, stdio and setup checks;
// callers check node/server execution.
// An unrelated filesystem watcher is outside the manager-effects boundary,
// like cron or inotify. Standard units are trusted as the operating system
// ships them: their own relationships, drop-ins and .wants/ links are its init
// graph, outside admission. Root additions there (including cycles beneath
// sysinit.target) have the same status as an unrelated watcher. Every edge
// between a protected role and a standard unit must still be listed, and a
// standard unit which an operation would start must already be active.
// Protected roles have distinct canonical Ids; no role's Names may include
// another role. Benign extra aliases of one role remain supported.
// Required retained inputs (configuration, identity, credentials and other startup
// files) never lie under /run, /tmp or /var/tmp: lexical and resolved paths and
// every traversed prefix and symlink are checked against both forms of those
// roots, independently of loaded settings or reload-preserved teardown state.
// A violation refuses with retained-input-volatile, including the node's own
// handoff. Only recreatable registration and lock records are disposable;
// cross-unit runtime, credential and private-tmp cleanup still protects them.
// Direct triggers of protected services are checked.
// Evidence is reread, and admission grants no future authority.
func (i *Inspector) AdmitOperations(ctx context.Context, sequence []Operation, protection OperationProtection) error {
	w := operationWalk{inspector: i, protection: protection, units: make(map[string]operationEvidence), targets: make(map[string]bool), paths: make(map[string]operationPathBinding), stopped: make(map[string]bool), standard: make(map[string]bool)}
	if err := w.admitRetainedInputs(); err != nil {
		return err
	}
	for _, op := range sequence {
		w.targets[op.Unit] = true
	}
	if err := w.admitClosedSet(ctx); err != nil {
		return err
	}
	paths := slices.Clone(protection.Paths)
	for _, owned := range protection.ownedPaths() {
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
		if op.Verb == "stop" || op.Verb == "start" {
			w.stopped[op.Unit] = op.Verb == "stop"
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
	if err := i.AdmitQuietActivation(ctx, protection.QuietUnits, protection.QuietExceptions, protection.WaitingUnits...); err != nil {
		return err
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
	if w.standard[unit] {
		props, err := w.inspector.properties(ctx, unit, "Id", "Names", "LoadState", "ActiveState", "Job", "StopWhenUnneeded")
		if err != nil {
			return operationEvidence{}, fmt.Errorf("operation-standard-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, props, []string{"Id", "Names", "LoadState", "ActiveState", "Job", "StopWhenUnneeded"}); err != nil {
			return operationEvidence{}, err
		}
		return operationEvidence{props: props}, nil
	}
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
	if first(props, "Id") != unit && !slices.Contains(strings.Fields(first(props, "Names")), unit) {
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
		mount, err := w.inspector.properties(ctx, unit, "Where", "What", "Type")
		if err != nil {
			return operationEvidence{}, fmt.Errorf("operation-mount-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, mount, []string{"Where", "What", "Type"}); err != nil {
			return operationEvidence{}, err
		}
		for key, value := range mount {
			props[key] = value
		}
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
	var privateTrees []string
	otherRecords := false
	for owner, paths := range w.protection.UnitPaths {
		otherRecords = otherRecords || (len(paths) != 0 && !slices.Contains(strings.Fields(first(props, "Names")), owner))
	}
	if strings.HasSuffix(unit, ".service") && otherRecords {
		privateTrees, err = w.inspector.operationPrivateTmp(first(props, "Id"), first(props, "PrivateTmp"))
		if err != nil {
			return operationEvidence{}, err
		}
	}
	if !scoped || first(props, "FragmentPath") == "" {
		return operationEvidence{props: props, privateTrees: privateTrees}, nil
	}
	sources, err := readOperationSources(props)
	if err != nil {
		return operationEvidence{}, fmt.Errorf("operation-source-unreadable: %s: %w", unit, err)
	}
	return operationEvidence{props: props, sources: sources, privateTrees: privateTrees}, nil
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
		if w.standard[effect.Unit] {
			if err := admitStandardEffect(effect, ev); err != nil {
				return err
			}
			continue
		}
		// Relationships may use a loaded alias; it is the same target effect.
		if canonical := first(ev.props, "Id"); canonical != "" {
			effect.Unit = canonical
		}
		if canonical := first(w.units[op.Unit].props, "Id"); canonical != "" {
			op.Unit = canonical
		}
		if effect != op && protectedOperationUnit(effect.Unit, w.protection.Units) &&
			!(effect.Verb == "start" && slices.Contains(w.protection.RequiredActive, effect.Unit) && first(ev.props, "ActiveState") == "active") {
			return fmt.Errorf("operation-protected-effect: %s %s reaches %s %s", op.Verb, op.Unit, effect.Verb, effect.Unit)
		}
		if first(ev.props, "LoadState") == "not-found" || first(ev.props, "LoadState") == "masked" {
			if effect == op && effect.Verb == "start" {
				return fmt.Errorf("operation-source-unavailable: start %s", effect.Unit)
			}
			// A positively absent or masked dependency cannot execute. Requires
			// may fail the target, whose completion handlers already refused.
			continue
		}
		if effect.Verb == "stop" && !w.stopped[effect.Unit] {
			w.stopped[effect.Unit] = true
		}
		// An active start is idempotent, but its dependencies still join the
		// transaction. Earlier stops in this sequence invalidate that premise.
		activeNoop := effect.Verb == "start" && first(ev.props, "ActiveState") == "active" && !w.stopped[effect.Unit]
		if !activeNoop {
			if err := admitOperationManagerEffects(effect.Unit, ev); err != nil {
				return err
			}
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
		if !activeNoop {
			if err := w.admitDirectories(effect, ev); err != nil {
				return err
			}
		}
		add := func(verb, property string) {
			for _, target := range strings.Fields(first(ev.props, property)) {
				queue = append(queue, Operation{Verb: verb, Unit: target})
			}
		}
		if effect.Verb == "start" {
			for _, prop := range []string{"Requires", "Wants", "BindsTo"} {
				add("start", prop)
			}
			add("start", "Triggers")
			add("stop", "Conflicts")
			add("stop", "ConflictedBy")
		} else {
			for _, prop := range []string{"Requires", "Wants", "BindsTo"} {
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
			for _, prop := range []string{"RequiredBy", "BoundBy"} {
				add("stop", prop)
			}
			// An active timer can restart the target
			// after a successful stop. Stopping it is not an authorized repair.
			for _, prop := range []string{"TriggeredBy"} {
				for _, source := range strings.Fields(first(ev.props, prop)) {
					from, err := w.get(ctx, source)
					if err != nil {
						return err
					}
					if first(from.props, "ActiveState") != "inactive" && !w.stopped[source] && !slices.Contains(w.protection.QuietExceptions, source) {
						return fmt.Errorf("operation-reactivation: %s %s=%s is not quiet", effect.Unit, prop, source)
					}
				}
			}
		}
		// Match inverse declarations too, rather than assuming fake or partial
		// evidence supplied both halves consistently.
		for unit, other := range w.units {
			if w.standard[unit] {
				continue
			}
			for _, prop := range []string{"BindsTo", "Requires"} {
				if effect.Verb == "stop" && slices.Contains(strings.Fields(first(other.props, prop)), effect.Unit) {
					queue = append(queue, Operation{Verb: "stop", Unit: unit})
				}
			}
		}
	}
	return nil
}

func admitOperationManagerEffects(unit string, ev operationEvidence) error {
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
	for _, owned := range w.protection.ownedPaths() {
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
		for _, prop := range operationExecutionProperties[5:19] {
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
	protected := slices.Clone(w.protection.Paths)
	for unit, paths := range w.protection.ownedPaths() {
		if w.canonicalUnit(unit) != w.canonicalUnit(effect.Unit) {
			protected = append(protected, paths...)
		}
	}
	for unit, other := range w.units {
		if w.standard[unit] || w.canonicalUnit(unit) == w.canonicalUnit(effect.Unit) {
			continue
		}
		for directive, root := range operationDirectoryRoots {
			for _, entry := range strings.Fields(first(other.props, directive)) {
				protected = append(protected, filepath.Join(root, entry))
			}
		}
	}
	check := func(path, directive string) error {
		keepPaths := protected
		if directive == "PrivateTmp" || directive == "CredentialDirectory" {
			// Required inputs already passed the closed volatile rule. These
			// teardown checks retain only other units' disposable records.
			keepPaths = w.otherUnitPaths(effect.Unit)
		}
		for _, keep := range keepPaths {
			overlap, err := w.pathsOverlap(path, keep)
			if err != nil {
				return err
			}
			// Recursive cleanup removes traversed links even when the leaf
			// resolves outside the directory being removed.
			for _, object := range w.paths[keep].Objects {
				overlap = overlap || Contained(path, object.Path) || Contained(w.paths[path].Resolved, object.Path)
			}
			if overlap {
				return fmt.Errorf("operation-directory-overlap: %s %s=%s affects %s", effect.Unit, directive, path, keep)
			}
		}
		if _, err := w.bindPath(path); err != nil {
			return fmt.Errorf("operation-directory-evidence: %s %s: %w", effect.Unit, directive, err)
		}
		return nil
	}
	// service_enter_dead destroys /run/credentials/<Id> unconditionally, even
	// without LoadCredential. Aliases cannot change this canonical teardown:
	// https://github.com/systemd/systemd/blob/v255/src/core/service.c#L1845
	// https://github.com/systemd/systemd/blob/v255/src/core/exec-credential.c#L132
	if effect.Verb == "stop" && strings.HasSuffix(effect.Unit, ".service") {
		for _, tree := range ev.privateTrees {
			if err := check(tree, "PrivateTmp"); err != nil {
				return err
			}
		}
		if err := check(filepath.Join("/run/credentials", first(ev.props, "Id")), "CredentialDirectory"); err != nil {
			return err
		}
	}
	for directive, root := range operationDirectoryRoots {
		for _, entry := range strings.Fields(first(ev.props, directive)) {
			if entry == "." || strings.HasPrefix(entry, "../") || filepath.IsAbs(entry) ||
				filepath.Clean(entry) != entry || strings.ContainsAny(entry, ":\\%\"'") {
				return fmt.Errorf("operation-directory-unsupported: %s %s=%s", effect.Unit, directive, entry)
			}
			if err := check(filepath.Join(root, entry), directive); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *operationWalk) admitMountRequirements(effect Operation, ev operationEvidence) error {
	protected := slices.Clone(w.protection.Paths)
	for unit, paths := range w.protection.ownedPaths() {
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
