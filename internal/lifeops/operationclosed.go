package lifeops

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// The edge vocabulary comes from deploy/ and roles/host/templates, and systemd
// v255 (source read 2026-09-15). CI's disposable-host witness is the measurement:
// service_add_default_dependencies: service.c#L639-L672;
// timer_add_default_dependencies/trigger_dependencies: timer.c#L77-L120;
// mount_add_{mount,device,default}_dependencies: mount.c#L262-L551;
// unit_add_exec_dependencies (journald stdio and directories): unit.c#L1177-L1287;
// unit_add_default_target_dependency, slices and mounts: unit.c#L1374-L1464;
// unit_add_node_dependency/block-device ordering: unit.c#L3470-L3528.
// v255 has no unit_add_default_dependencies function: the common work is in
// unit_add_default_target_dependency and unit_add_{exec,slice,mount}_dependencies.
// https://github.com/systemd/systemd/blob/v255/src/core/unit.c
// https://github.com/systemd/systemd/blob/v255/src/core/service.c
// https://github.com/systemd/systemd/blob/v255/src/core/timer.c
// https://github.com/systemd/systemd/blob/v255/src/core/mount.c
// https://github.com/systemd/systemd/blob/v255/src/core/slice.c
// Ancestor fstab mounts remain leaves, including passno fsck dependencies:
// https://github.com/systemd/systemd/blob/v255/src/fstab-generator/fstab-generator.c
// Each row is (property, source class, destination class). Inverse properties
// are checked against the reversed row, never against an endpoint name alone.
// Template citations below are under
// ansible_collections/junioryono/billet/roles/host/templates/.
// images-refresh and ceph-health templates have no relationships to these
// protected roles and are not in retirement's protected unit set.
var operationEdges = []struct{ property, source, destination string }{
	{"Requires", "service|timer", "sysinit.target"},
	{"After", "service|timer", "sysinit.target"},
	{"After", "service", "basic.target"},
	{"Before", "service|timer", "shutdown.target"},
	{"Conflicts", "service|timer", "shutdown.target"},
	// billet-server.service.j2:4, billet-node.service.j2:8,11,
	// billet-backup.service.j2:17, billet-upgrade.service.j2:14,
	// billet-dnsmasq@.service.j2:3; deploy/billet-{server,node,backup,upgrade}
	// .service:12,14,20,31 respectively.
	{"After", "server|node|backup|upgrade|dns", "network-online.target"},
	// The same templates:5,13,18,15,4; deploy units:13,15,21,32.
	{"Wants", "server|node|backup|upgrade|dns", "network-online.target"},
	// deploy/billet-node.service:14.
	{"After", "node", "docker.service"},
	// billet-network.service.j2:3.
	{"After", "network", "network-pre.target"},
	// billet-network.service.j2:3; billet-dnsmasq@.service.j2:3.
	{"After", "network|dns", "systemd-networkd.service"},
	// billet-upgrade.service.j2:14; deploy/billet-upgrade.service:31.
	{"After", "upgrade", "server"},
	// billet-node.service.j2:8; billet-network.service.j2:4 (inverse).
	{"After", "node", "network"},
	// billet-node.service.j2:9.
	{"Requires", "node", "network"},
	{"Before", "timer", "timers.target"},
	// billet-{backup,upgrade}.timer.j2:28,16; deploy timers:32,30.
	{"WantedBy", "timer", "timers.target"},
	{"After", "timer", "time-set.target"},
	{"After", "timer", "time-sync.target"},
	{"Before", "timer", "paired-service"},
	// Unit= in billet-{backup,upgrade}.timer.j2:25,13; deploy timers:29,27.
	// timer_add_trigger_dependencies also adds the preceding Before row.
	{"Triggers", "timer", "paired-service"},
	// billet-{server,node,network}.service.j2:110,75,13,
	// billet-dnsmasq@.service.j2:19, billet-ledger.mount.j2:22;
	// deploy/billet-{server,node}.service:104,84. The next Before row is
	// unit_add_default_target_dependency's ordering for these install links.
	{"WantedBy", "server|node|network|dns|ledger", "multi-user.target"},
	{"Before", "server|node|network|dns|ledger", "multi-user.target"},
	{"Requires", "service|ledger", "own-slice"},
	{"After", "service|ledger", "own-slice"},
	{"After", "service|ledger", "systemd-journald.socket"},
	{"After", "server|node|backup|dns", "systemd-remount-fs.service"},
	{"After", "server|node|backup|dns", "systemd-tmpfiles-setup.service"},
	{"Wants", "server|node|backup|dns", "tmp.mount"},
	{"After", "server|node|backup|dns", "tmp.mount"},
	{"Requires", "service|timer|ledger", "path-mount"},
	{"After", "service|timer|ledger", "path-mount"},
	// billet-server.service.j2:19; billet-backup.service.j2:27.
	{"Requires", "server|backup", "ledger"},
	// billet-server.service.j2:20; billet-backup.service.j2:28.
	// RequiresMountsFor= also derives these and ancestor mount pairs:
	// server template:21, backup template:33; deploy/billet-backup.service:31.
	{"After", "server|backup", "ledger"},
	{"After", "ledger", "local-fs-pre.target"},
	{"Before", "ledger", "local-fs.target"},
	{"Before", "ledger", "umount.target"},
	{"Conflicts", "ledger", "umount.target"},
	{"After", "tmpfs-ledger", "swap.target"},
	{"Requires", "ledger", "backing-device"},
	{"BindsTo", "ledger", "backing-device"},
	// Optional: Ubuntu 24.04/systemd 255.4 CI (2026-09-15) first reported
	// no such edge, then the exact pair at 55cdd81. Admit it when present;
	// retirement never stops a device (v255 mount.c#L367).
	{"StopPropagatedFrom", "ledger", "backing-device"},
	{"After", "ledger", "backing-device"},
	{"After", "ledger", "backing-blockdev"},
}

var operationInverseRelations = map[string]string{
	"RequiredBy": "Requires", "WantedBy": "Wants", "BoundBy": "BindsTo",
	"Before": "After", "After": "Before", "ConflictedBy": "Conflicts",
	"TriggeredBy": "Triggers", "PropagatesStopTo": "StopPropagatedFrom",
}

func (w *operationWalk) admitClosedSet(ctx context.Context) error {
	// Each pass starts from declarations; discovered aliases are not roles.
	w.protection = w.declared
	w.protection.Units = slices.Clone(w.declared.Units)
	w.protection.RequiredActive = slices.Clone(w.declared.RequiredActive)
	own := slices.Clone(w.protection.Units)
	own = append(own, w.protection.QuietUnits...)
	own = append(own, w.protection.WaitingUnits...)
	own = append(own, w.protection.RequiredActive...)
	roles := slices.Clone(own)
	for unit := range w.targets {
		own = append(own, unit)
	}
	slices.Sort(own)
	own = slices.Compact(own)
	for n := 0; n < len(own); n++ {
		unit := own[n]
		delete(w.standard, unit)
		ev, err := w.get(ctx, unit)
		if err != nil {
			return err
		}
		// Only the mount at the exact ledger path is protected. A separate
		// /var supplying that path is an active standard leaf, not our mount.
		if strings.HasSuffix(unit, "-server.service") || strings.HasSuffix(unit, "-backup.service") {
			for _, path := range strings.Fields(first(ev.props, "RequiresMountsFor")) {
				if !filepath.IsAbs(path) || filepath.Clean(path) != path {
					return fmt.Errorf("operation-mount-requirement-unknown: %s", unit)
				}
				mount := operationPathUnit(path)
				if (path == "/tmp" || path == "/var/tmp") || !slices.Contains(strings.Fields(first(ev.props, "Requires")), mount) {
					continue
				}
				ledgerPath := false
				for owner, paths := range w.protection.ownedPaths() {
					if strings.HasSuffix(owner, "-server.service") && slices.Contains(paths, path) {
						ledgerPath = true
					}
				}
				if ledgerPath && !slices.Contains(own, mount) {
					own = append(own, mount)
					w.protection.Units = append(w.protection.Units, mount)
					w.protection.RequiredActive = append(w.protection.RequiredActive, mount)
				}
			}
		}
	}
	// Resolve every role before populating aliases: a Names entry must never
	// replace another role's evidence before its identity has been compared.
	if err := w.admitRoleIdentities(roles); err != nil {
		return err
	}
	for _, unit := range own {
		ev := w.units[unit]
		for _, alias := range strings.Fields(first(ev.props, "Names")) {
			if !operationUnitName(alias) {
				return fmt.Errorf("operation-names-unknown: %s", unit)
			}
			// Aliases use the same object, policies and source binding.
			if len(w.units) >= operationUnitLimit {
				return fmt.Errorf("operation-traversal-bound: more than %d unit names", operationUnitLimit)
			}
			if previous, exists := w.units[alias]; exists && first(previous.props, "Id") != first(ev.props, "Id") {
				return fmt.Errorf("operation-names-unknown: %s collides with %s", unit, alias)
			}
			w.units[alias] = ev
			w.protection.Units = append(w.protection.Units, alias)
		}
	}
	for _, proof := range w.protection.RetiredConditions {
		if proof.unit == "" || !slices.Contains(w.declared.Units, proof.unit) {
			return fmt.Errorf("operation-inert-evidence: no protected unit binding")
		}
		ev := w.units[proof.unit]
		if err := proof.proveProperties(ev.props); err != nil {
			return fmt.Errorf("operation-inert-evidence: %s: %w", proof.unit, err)
		}
		fresh, err := w.inspector.ProveRetiredConditionEvidence(ctx, proof.unit, proof.marker)
		if err != nil {
			return fmt.Errorf("operation-inert-evidence: %s: %w", proof.unit, err)
		}
		if fresh != proof {
			return fmt.Errorf("operation-inert-evidence: %s changed its inertness proof", proof.unit)
		}
	}
	for _, unit := range own {
		ev := w.units[unit]
		entries, err := operationInstallEntries(ev.sources)
		if err != nil {
			return err
		}
		for _, relation := range operationRelations {
			for _, target := range strings.Fields(first(ev.props, relation)) {
				if err := w.closedRelation(ctx, unit, relation, target); err != nil {
					return err
				}
			}
		}
		for _, key := range []string{"Also", "Alias", "WantedBy", "RequiredBy", "UpheldBy"} {
			for _, target := range entries[key] {
				if err := w.closedRelation(ctx, unit, "Install."+key, target); err != nil {
					return err
				}
			}
		}
		if strings.HasSuffix(unit, ".mount") {
			if first(ev.props, "LoadState") != "loaded" || first(ev.props, "ActiveState") != "active" || first(ev.props, "Job") != "" ||
				operationPathUnit(first(ev.props, "Where")) != unit {
				return fmt.Errorf("operation-standard-effect: dedicated ledger %s is not an active mount at its canonical path", unit)
			}
		}
		if first(ev.props, "LoadState") != "loaded" {
			continue
		}
		if err := admitOperationManagerEffects(unit, ev); err != nil {
			return err
		}
		if operationExecutionInterface(unit) != "" {
			if err := admitOperationSetupValues(unit, ev); err != nil {
				return err
			}
			// A running backup may complete while retirement waits. Its directory
			// teardown must be safe even though retirement never stops that backup.
			if !slices.Contains(w.protection.RequiredActive, unit) {
				if err := w.admitDirectories(Operation{Verb: "stop", Unit: unit}, ev); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (w *operationWalk) admitRoleIdentities(roles []string) error {
	slices.Sort(roles)
	roles = slices.Compact(roles)
	owners := make(map[string]string)
	for _, unit := range roles {
		ev := w.units[unit]
		canonical := first(ev.props, "Id")
		if !operationUnitName(canonical) {
			return fmt.Errorf("operation-names-unknown: %s has no canonical identity", unit)
		}
		if owner, exists := owners[canonical]; exists && owner != unit {
			return fmt.Errorf("operation-role-collision: %s and %s resolve to %s", owner, unit, canonical)
		}
		owners[canonical] = unit
		for _, alias := range strings.Fields(first(ev.props, "Names")) {
			if alias != unit && slices.Contains(roles, alias) {
				return fmt.Errorf("operation-role-collision: %s Names includes protected role %s", unit, alias)
			}
		}
	}
	return nil
}

func (w *operationWalk) closedRelation(ctx context.Context, unit, relation, target string) error {
	if w.inertIncomingEdge(unit, relation, target) {
		return nil
	}
	if !w.edgeAllowed(unit, relation, target) {
		return fmt.Errorf("operation-edge-outside-set: %s %s=%s", unit, strings.TrimPrefix(relation, "Install."), target)
	}
	if _, own := w.units[target]; own && !w.standard[target] {
		return nil
	}
	w.standard[target] = true
	// Ancestor mounts are always active no-ops. Read no relationships from
	// them: device/blockdev, fsck (fstab passno), swap, local-fs and root-mount
	// dependencies belong to their graph, never to a protected service's graph.
	// The exact default slice is likewise an active no-op, never permission
	// to create a new slice for a protected template instance.
	if (strings.HasSuffix(target, ".mount") && target != "tmp.mount") || strings.HasSuffix(target, ".slice") {
		ev, err := w.get(ctx, target)
		if err != nil {
			return err
		}
		if first(ev.props, "LoadState") != "loaded" || first(ev.props, "ActiveState") != "active" {
			return fmt.Errorf("operation-standard-effect: dependency %s is not an active no-op", target)
		}
		if err := admitStandardEffect(Operation{Verb: "start", Unit: target}, ev); err != nil {
			return err
		}
	}
	return nil
}

func (w *operationWalk) inertUnit(unit string) bool {
	unit = w.canonicalUnit(unit)
	for _, proof := range w.protection.RetiredConditions {
		if proof.unit != "" && w.canonicalUnit(proof.unit) == unit {
			return true
		}
	}
	return false
}

// Installation, ordering and outgoing effects retain their normal checks.
func (w *operationWalk) inertIncomingEdge(unit, relation, target string) bool {
	switch relation {
	case "Wants", "Requires", "Requisite", "BindsTo", "Upholds", "Triggers", "OnSuccess", "OnFailure", "PropagatesStopTo", "PropagatesReloadTo":
		return w.inertUnit(target)
	case "WantedBy", "RequiredBy", "RequisiteOf", "BoundBy", "UpheldBy", "TriggeredBy", "OnSuccessOf", "OnFailureOf", "StopPropagatedFrom", "ReloadPropagatedFrom":
		return w.inertUnit(unit)
	default:
		return false
	}
}

func (w *operationWalk) edgeAllowed(unit, relation, target string) bool {
	unit, target = w.canonicalUnit(unit), w.canonicalUnit(target)
	if relation == "Install.Also" && w.inertUnit(target) {
		return true
	}
	if key, install := strings.CutPrefix(relation, "Install."); install {
		if key == "Alias" {
			return unit == target
		}
		// None of billet's shipped units declares Also, RequiredBy or UpheldBy.
		if key != "WantedBy" {
			return false
		}
		relation = key
	}
	for _, edge := range operationEdges {
		if edge.property == relation && w.edgeClass(unit, target, edge.source) && w.edgeClass(target, unit, edge.destination) {
			return true
		}
		if edge.property == operationInverseRelations[relation] && w.edgeClass(target, unit, edge.source) && w.edgeClass(unit, target, edge.destination) {
			return true
		}
	}
	return false
}

func (w *operationWalk) canonicalUnit(unit string) string {
	if ev, ok := w.units[unit]; ok && !w.standard[unit] && first(ev.props, "Id") != "" {
		return first(ev.props, "Id")
	}
	return unit
}

func (w *operationWalk) edgeClass(unit, other, classes string) bool {
	for _, class := range strings.Split(classes, "|") {
		if w.matchesEdgeClass(unit, other, class) {
			return true
		}
	}
	return false
}

func (w *operationWalk) matchesEdgeClass(unit, other, class string) bool {
	ev, own := w.units[unit]
	own = own && !w.standard[unit]
	kind := ""
	if own {
		for _, name := range []string{"server", "node", "backup", "upgrade", "network"} {
			if strings.HasSuffix(unit, "-"+name+".service") {
				kind = name
			}
		}
		if strings.Contains(unit, "-dnsmasq@") && strings.HasSuffix(unit, ".service") {
			kind = "dns"
		}
	}
	switch class {
	case "service":
		return kind != ""
	case "server", "node", "backup", "upgrade", "network", "dns":
		return kind == class
	case "timer":
		return own && (strings.HasSuffix(unit, "-backup.timer") || strings.HasSuffix(unit, "-upgrade.timer"))
	case "paired-service":
		return own && operationTimerPair(other, unit)
	case "ledger":
		return own && strings.HasSuffix(unit, ".mount") && slices.Contains(w.protection.RequiredActive, unit)
	case "tmpfs-ledger":
		return w.matchesEdgeClass(unit, other, "ledger") && first(ev.props, "Type") == "tmpfs"
	case "own-slice":
		// unit_set_default_slice, systemd v255 src/core/unit.c:3254-3299:
		// escape the already escaped TEMPLATE PREFIX again, including its
		// hyphens; the instance is not part of the slice name. Measured for
		// dnsmasq@br0 on Ubuntu 24.04/systemd 255.4, 2026-09-15.
		if prefix, _, ok := strings.Cut(other, "@"); ok && strings.HasSuffix(other, ".service") {
			return unit == "system-"+strings.TrimSuffix(operationPathUnit("/"+prefix), ".mount")+".slice"
		}
		return unit == "system.slice"
	case "path-mount":
		return !own && w.pathMount(other, unit)
	case "backing-device", "backing-blockdev":
		mount, ok := w.units[other]
		what := first(mount.props, "What")
		if !ok || !strings.HasPrefix(what, "/dev/") || filepath.Clean(what) != what || what == "/dev/root" || what == "/dev/nfs" {
			return false
		}
		device := strings.TrimSuffix(operationPathUnit(what), ".mount")
		if class == "backing-device" {
			return unit == device+".device"
		}
		return unit == "blockdev@"+device+".target"
	default:
		return unit == class && !own
	}
}

func operationTimerPair(timer, service string) bool {
	return (strings.HasSuffix(timer, "-backup.timer") || strings.HasSuffix(timer, "-upgrade.timer")) &&
		service == strings.TrimSuffix(timer, ".timer")+".service"
}

func (w *operationWalk) pathMount(unit, mount string) bool {
	if !strings.HasSuffix(mount, ".mount") {
		return false
	}
	ev := w.units[unit]
	paths := append(slices.Clone(w.protection.Paths), "/var/tmp", "/var/lib/systemd/timers")
	for _, owned := range w.protection.ownedPaths() {
		paths = append(paths, owned...)
	}
	for directive, root := range operationDirectoryRoots {
		for _, entry := range strings.Fields(first(ev.props, directive)) {
			paths = append(paths, filepath.Join(root, entry))
		}
	}
	if strings.HasSuffix(unit, ".mount") {
		paths = append(paths, filepath.Dir(first(ev.props, "Where")))
		what := first(ev.props, "What")
		if w.matchesEdgeClass(unit, "", "ledger") && strings.HasPrefix(what, "/dev/") && filepath.Clean(what) == what && what != "/dev/root" && what != "/dev/nfs" {
			// mount_add_mount_dependencies also requires the source path;
			// unit_add_mount_dependencies orders it after that path's mounts.
			paths = append(paths, what)
		}
	}
	for _, required := range strings.Fields(first(ev.props, "RequiresMountsFor")) {
		if !slices.Contains(paths, required) || !filepath.IsAbs(required) || filepath.Clean(required) != required {
			continue
		}
		for path := required; ; path = filepath.Dir(path) {
			if mount == operationPathUnit(path) {
				return true
			}
			if path == "/" {
				break
			}
		}
	}
	return false
}

func admitStandardEffect(effect Operation, ev operationEvidence) error {
	state, load := first(ev.props, "ActiveState"), first(ev.props, "LoadState")
	if first(ev.props, "Id") != effect.Unit || !slices.Contains(strings.Fields(first(ev.props, "Names")), effect.Unit) {
		return fmt.Errorf("operation-standard-unknown: %s canonical identity", effect.Unit)
	}
	if first(ev.props, "Job") != "" {
		return fmt.Errorf("operation-standard-effect: %s has a pending job", effect.Unit)
	}
	if state == "inactive" && (load == "not-found" || load == "masked") {
		return nil // Positive absence/masking cannot execute even a wanted start.
	}
	if load == "loaded" && ((effect.Verb == "start" && state == "active") || (effect.Verb == "stop" && state == "inactive")) {
		return nil
	}
	return fmt.Errorf("operation-standard-effect: %s %s would change or has unknown activity (%s/%s)", effect.Verb, effect.Unit, load, state)
}

func operationPathUnit(path string) string {
	var name strings.Builder
	for n, b := range []byte(strings.TrimPrefix(path, "/")) {
		switch {
		case b == '/':
			name.WriteByte('-')
		case (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == ':' || (b == '.' && n > 0):
			name.WriteByte(b)
		default:
			fmt.Fprintf(&name, `\x%02x`, b)
		}
	}
	if name.Len() == 0 {
		name.WriteByte('-')
	}
	return name.String() + ".mount"
}
