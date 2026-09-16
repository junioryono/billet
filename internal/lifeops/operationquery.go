package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CI on systemd 255.4 (2026-09-15) reports escaped mount and slice names as
// double-quoted array entries with doubled backslashes. Decode that display
// layer only; the remaining unit-name escapes are part of the unit's identity.
// v255 src/shared/bus-print-properties.c:225-237 uses shell_maybe_quote for
// string arrays. Scalar Id and FragmentPath are not rendered this way.
func operationUnitList(value string) (string, error) {
	entries := strings.Fields(value)
	for n, entry := range entries {
		if !strings.HasPrefix(entry, "\"") {
			continue
		}
		decoded, err := strconv.Unquote(entry)
		if err != nil || !operationUnitName(decoded) {
			return "", fmt.Errorf("unsupported quoted unit name %q", entry)
		}
		entries[n] = decoded
	}
	return strings.Join(entries, " "), nil
}

// Empty arrays of structs are omitted by systemctl show, even with --all:
// https://github.com/systemd/systemd/blob/v255/src/systemctl/systemctl-show.c.
// A typed busctl response proves an empty array; missing show output cannot.
// Signatures are the type-specific/ExecContext vtables in v255 dbus-*.c and
// dbus-execute.c. ConfigurationDirectory has no Symlink property in v255.
var operationArrayProperties = []struct{ name, signature string }{
	{"StateDirectorySymlink", "a(sst)"}, {"RuntimeDirectorySymlink", "a(sst)"},
	{"CacheDirectorySymlink", "a(sst)"}, {"LogsDirectorySymlink", "a(sst)"},
	{"BindPaths", "a(ssbt)"}, {"BindReadOnlyPaths", "a(ssbt)"},
	{"MountImages", "a(ssba(ss))"}, {"ExtensionImages", "a(sba(ss))"},
	{"TemporaryFileSystem", "a(ss)"},
	{"ExecCondition", "a(sasbttttuii)"}, {"ExecStartPre", "a(sasbttttuii)"},
	{"ExecStartPost", "a(sasbttttuii)"}, {"ExecStop", "a(sasbttttuii)"}, {"ExecStopPost", "a(sasbttttuii)"},
}

// WithOperationBusctl selects the typed system-bus reader for operation admission.
func WithOperationBusctl(path string) Option {
	return func(i *Inspector) { i.operationBusctl = path }
}

func (i *Inspector) operationExecution(ctx context.Context, unit string, commands bool) (map[string][]string, error) {
	if i.operationPass != nil {
		if props, ok := i.operationPass.execution[unit]; ok {
			return cloneOperationProperties(props), nil
		}
	}
	names := make([]string, 0, len(operationExecutionProperties)+1)
	for _, property := range operationExecutionProperties {
		if !slices.ContainsFunc(operationArrayProperties, func(p struct{ name, signature string }) bool { return p.name == property }) {
			names = append(names, property)
		}
	}
	if strings.HasSuffix(unit, ".service") {
		names = append(names, "PIDFile")
	}
	props, err := i.properties(ctx, unit, names...)
	if err != nil {
		return nil, err
	}
	if err := requireOperationProperties(unit, props, names); err != nil {
		return nil, err
	}
	arrays := slices.DeleteFunc(slices.Clone(operationArrayProperties), func(p struct{ name, signature string }) bool {
		return !commands && strings.HasPrefix(p.name, "Exec")
	})
	if i.operationPass != nil && slices.Contains(i.operationPass.retained, unit) {
		values, err := i.operationPass.typedProperties(ctx, i, unit)
		if err != nil {
			return nil, err
		}
		for _, property := range arrays {
			value, ok := values[property.name]
			var entries []json.RawMessage
			if !ok || value.Type != property.signature || json.Unmarshal(value.Data, &entries) != nil || string(value.Data) == "null" {
				return nil, fmt.Errorf("operation-array-unknown: %s %s has no typed array", unit, property.name)
			}
			props[property.name] = []string{""}
			if len(entries) != 0 {
				props[property.name] = []string{string(value.Data)}
			}
		}
		i.operationPass.execution[unit] = cloneOperationProperties(props)
		return props, nil
	}
	values, err := i.operationArrays(ctx, unit, arrays)
	if err != nil {
		return nil, err
	}
	for property, value := range values {
		props[property] = value
	}
	if i.operationPass != nil {
		i.operationPass.execution[unit] = cloneOperationProperties(props)
	}
	return props, nil
}

// ProveServiceArrayEmptiness replaces missing/empty text with typed evidence.
// Nonempty text remains useful for diagnostics and already prevents permission.
func (i *Inspector) ProveServiceArrayEmptiness(ctx context.Context, unit string, props map[string][]string, names ...string) error {
	var arrays []struct{ name, signature string }
	for _, name := range names {
		if first(props, name) != "" {
			continue
		}
		if name == "ExecReload" {
			arrays = append(arrays, struct{ name, signature string }{name, "a(sasbttttuii)"})
			continue
		}
		index := slices.IndexFunc(operationArrayProperties, func(p struct{ name, signature string }) bool { return p.name == name })
		if index < 0 {
			return fmt.Errorf("operation-array-unknown: unsupported property %s", name)
		}
		arrays = append(arrays, operationArrayProperties[index])
	}
	if len(arrays) == 0 {
		return nil
	}
	values, err := i.operationArrays(ctx, unit, arrays)
	if err != nil {
		return err
	}
	for property, value := range values {
		props[property] = value
	}
	return nil
}

func (i *Inspector) operationArrays(ctx context.Context, unit string, arrays []struct{ name, signature string }) (map[string][]string, error) {
	props := make(map[string][]string, len(arrays))
	args := make([]string, 0, 4+len(arrays))
	args = append(args, "get-property", "org.freedesktop.systemd1", operationObjectPath(unit), operationExecutionInterface(unit))
	for _, property := range arrays {
		args = append(args, property.name)
	}
	bin := i.operationBusctl
	if bin == "" {
		bin = "busctl"
	}
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	if i.observe != nil {
		i.observe(ctx, args)
	}
	out, err := i.run(ctx, bin, args)
	if err != nil {
		return nil, fmt.Errorf("operation-array-unreadable: %s: %w", unit, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(arrays) {
		return nil, fmt.Errorf("operation-array-unknown: %s returned %d arrays, expected %d", unit, len(lines), len(arrays))
	}
	for n, property := range arrays {
		fields := strings.Fields(lines[n])
		if len(fields) < 2 || fields[0] != property.signature {
			return nil, fmt.Errorf("operation-array-unknown: %s %s has no typed answer", unit, property.name)
		}
		count, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil || (count == 0 && len(fields) != 2) || (count != 0 && len(fields) == 2) {
			return nil, fmt.Errorf("operation-array-unknown: %s %s has an invalid array", unit, property.name)
		}
		if count != 0 {
			// Retain configured evidence for billet's setup and command checks.
			props[property.name] = []string{lines[n]}
			continue
		}
		props[property.name] = []string{""}
	}
	return props, nil
}

func operationObjectPath(unit string) string {
	var escaped strings.Builder
	escaped.WriteString("/org/freedesktop/systemd1/unit/")
	for n, b := range []byte(unit) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (n > 0 && b >= '0' && b <= '9') {
			escaped.WriteByte(b)
		} else {
			fmt.Fprintf(&escaped, "_%02x", b)
		}
	}
	return escaped.String()
}

// These four unit vtables embed bus_exec_vtable in systemd 255:
// https://github.com/systemd/systemd/tree/v255/src/core (dbus-*.c).
func operationExecutionInterface(unit string) string {
	for suffix, kind := range map[string]string{
		".service": "Service", ".socket": "Socket", ".mount": "Mount", ".swap": "Swap",
	} {
		if strings.HasSuffix(unit, suffix) {
			return "org.freedesktop.systemd1." + kind
		}
	}
	return ""
}
