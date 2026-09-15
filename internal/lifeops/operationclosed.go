package lifeops

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Derived from systemd v255 src/core/{unit,service,timer,mount}.c: the type
// add_default_dependencies functions, unit_add_{exec,slice,mount}_dependencies
// (v255 has no function named unit_add_default_dependencies), and journald stdio;
// plus deploy/ and the host role's rendered units. Source read 2026-09-15:
// https://github.com/systemd/systemd/tree/v255/src/core
// Never expand this from the host graph. Unknown names must fail the CI witness.
var operationStandardUnits = []string{
	"sysinit.target", "basic.target", "shutdown.target", "network-online.target",
	"network.target", "network-pre.target", "timers.target", "multi-user.target",
	"local-fs.target", "local-fs-pre.target", "remote-fs.target", "remote-fs-pre.target",
	"time-set.target", "time-sync.target", "umount.target", "system.slice", "-.slice", "-.mount", "tmp.mount",
	"systemd-journald.socket", "systemd-journald-dev-log.socket", "docker.service",
	"systemd-networkd.service", "systemd-remount-fs.service", "systemd-tmpfiles-setup.service",
}

func (w *operationWalk) admitClosedSet(ctx context.Context) error {
	own := slices.Clone(w.protection.Units)
	own = append(own, w.protection.QuietUnits...)
	for unit := range w.targets {
		own = append(own, unit)
	}
	slices.Sort(own)
	own = slices.Compact(own)
	for _, unit := range operationStandardUnits {
		w.standard[unit] = true
	}
	// Only ancestors of protected paths and the shipped PrivateTmp paths can
	// supply implicit mount dependencies. A random *.mount is never admitted.
	paths := append(slices.Clone(w.protection.Paths), "/tmp", "/var/tmp")
	for _, owned := range w.protection.UnitPaths {
		paths = append(paths, owned...)
	}
	for _, path := range paths {
		resolved, err := w.bindPath(path)
		if err != nil {
			return err
		}
		for _, spelling := range []string{path, resolved} {
			for parent := spelling; parent != "/"; parent = filepath.Dir(parent) {
				w.standard[operationPathUnit(parent)] = true
			}
		}
	}
	for n := 0; n < len(own); n++ {
		unit := own[n]
		delete(w.standard, unit)
		ev, err := w.get(ctx, unit)
		if err != nil {
			return err
		}
		// Protect the deepest named mount serving the controller's ledger path.
		// Other ancestor mounts remain standard no-ops. PrivateTmp's separate
		// requirements do not identify a ledger mount.
		if strings.HasSuffix(unit, "-server.service") || strings.HasSuffix(unit, "-backup.service") {
			for _, path := range strings.Fields(first(ev.props, "RequiresMountsFor")) {
				if !filepath.IsAbs(path) || filepath.Clean(path) != path {
					return fmt.Errorf("operation-mount-requirement-unknown: %s", unit)
				}
				if path == "/tmp" || path == "/var/tmp" {
					continue
				}
				for parent := path; parent != "/"; parent = filepath.Dir(parent) {
					mount := operationPathUnit(parent)
					if !slices.Contains(strings.Fields(first(ev.props, "Requires")), mount) {
						continue
					}
					if w.standard[mount] && !slices.Contains(own, mount) {
						own = append(own, mount)
						w.protection.Units = append(w.protection.Units, mount)
						w.protection.RequiredActive = append(w.protection.RequiredActive, mount)
					}
					break
				}
			}
		}
		// Instantiated services acquire a per-template system slice.
		if prefix, _, ok := strings.Cut(unit, "@"); ok && strings.HasSuffix(unit, ".service") {
			w.standard["system-"+strings.TrimSuffix(operationPathUnit("/"+prefix), ".mount")+".slice"] = true
		}
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
	for _, unit := range own {
		ev := w.units[unit]
		entries, err := operationInstallEntries(ev.sources)
		if err != nil {
			return err
		}
		for _, relation := range operationRelations {
			for _, target := range strings.Fields(first(ev.props, relation)) {
				if err := w.closedRelation(unit, relation, target); err != nil {
					return err
				}
			}
		}
		for _, key := range []string{"Also", "Alias", "WantedBy", "RequiredBy", "UpheldBy"} {
			for _, target := range entries[key] {
				if err := w.closedRelation(unit, key, target); err != nil {
					return err
				}
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

func (w *operationWalk) closedRelation(unit, relation, target string) error {
	if _, own := w.units[target]; own {
		return nil
	}
	if !w.standard[target] {
		return fmt.Errorf("operation-unit-outside-set: %s %s=%s", unit, relation, target)
	}
	// These service names are named only for ordering by the shipped units.
	if strings.HasSuffix(target, ".service") && relation != "Before" && relation != "After" {
		return fmt.Errorf("operation-standard-effect: %s %s=%s is not ordering-only", unit, relation, target)
	}
	return nil
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
