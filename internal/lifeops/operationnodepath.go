package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// AdmitRetainedUnitPaths rereads the complete loaded property set at stopped
// and archive boundaries, using the same traversal proof as operation admission.
func (i *Inspector) AdmitRetainedUnitPaths(ctx context.Context, unit string, archivedRoots ...string) error {
	pass := *i
	pass.operationPass = newOperationPass([]string{unit})
	i = &pass
	w := operationWalk{inspector: i, paths: make(map[string]operationPathBinding),
		protection: OperationProtection{RetainedPathUnits: []string{unit}, ArchivedInputRoots: archivedRoots}}
	if err := w.admitRetainedUnitPaths(ctx); err != nil {
		return err
	}
	return w.revalidatePaths()
}

func (w *operationWalk) admitRetainedUnitPaths(ctx context.Context) error {
	for _, unit := range w.protection.RetainedPathUnits {
		props, err := w.inspector.properties(ctx, unit)
		if err != nil {
			return fmt.Errorf("retained-node-path-unknown: %s: %w", unit, err)
		}
		if err := requireOperationProperties(unit, props, []string{"Id", "LoadState", "RuntimeDirectory"}); err != nil {
			return err
		}
		if first(props, "LoadState") != "loaded" {
			return fmt.Errorf("retained-node-path-unknown: %s is not loaded", unit)
		}
		var disposable []string
		for _, entry := range strings.Fields(first(props, "RuntimeDirectory")) {
			if entry == "." || strings.HasPrefix(entry, "../") || filepath.IsAbs(entry) ||
				filepath.Clean(entry) != entry || strings.ContainsAny(entry, ":\\%\"'") {
				return fmt.Errorf("retained-node-path-unknown: %s RuntimeDirectory", unit)
			}
			path, err := w.bindPath(filepath.Join(runtimeRoot, entry))
			if err != nil {
				return err
			}
			disposable = append(disposable, filepath.Join(runtimeRoot, entry), path)
		}
		paths, err := w.inspector.retainedUnitPaths(ctx, unit, props)
		if err != nil {
			return err
		}
		for _, property := range sortedRetainedPathProperties(paths) {
			for _, path := range paths[property] {
				// PrivateTmp's exact mount prerequisite is manager setup, not
				// a retained input. Never exempt descendants or other carriers.
				// v255 unit_add_exec_dependencies: unit.c#L1227-L1241.
				if property == "RequiresMountsFor" && path == "/var/tmp" && first(props, "PrivateTmp") == "yes" {
					if err := w.admitRetainedInput(path, nil, "retained-node-path-archived"); err != nil {
						return fmt.Errorf("%s %s: %w", unit, property, err)
					}
					continue
				}
				if err := w.admitRetainedNodePath(path, disposable); err != nil {
					return fmt.Errorf("%s %s: %w", unit, property, err)
				}
			}
		}
	}
	return nil
}

// Keep extraction shared with the real-host diagnostics: CI logs the exact
// path operands the admission judges, including directory-derived paths.
func (i *Inspector) retainedUnitPaths(ctx context.Context, unit string, props map[string][]string) (map[string][]string, error) {
	if i.operationPass == nil {
		reader := *i
		reader.operationPass = newOperationPass([]string{unit})
		i = &reader
	}
	typedValues, err := i.operationPass.typedProperties(ctx, i, unit)
	if err != nil {
		return nil, err
	}
	// systemctl omits some properties entirely. Its keys cannot bound the
	// typed inventory; both reads contribute properties to the path proof.
	inventory := cloneOperationProperties(props)
	for property := range typedValues {
		if _, shown := inventory[property]; !shown {
			inventory[property] = nil
		}
	}
	found := make(map[string][]string)
	for _, property := range sortedRetainedPathProperties(inventory) {
		values := inventory[property]
		switch property {
		case "FragmentPath", "SourcePath", "DropInPaths", "ControlGroup", "ControlGroupId":
			continue
		}
		_, typed := typedValues[property]
		if !typed {
			typed = slices.ContainsFunc(values, func(value string) bool {
				return value == "[unprintable]" || strings.Contains(value, "\\") || len(operationAbsolutePaths(value)) != 0
			})
		}
		if typed {
			// Typed strings preserve escaped pathnames and array boundaries.
			values, err = i.operationPropertyStrings(ctx, unit, property)
			if err != nil {
				return found, err
			}
		}
		for _, value := range values {
			paths := operationAbsolutePaths(value)
			exact := strings.TrimLeft(value, "-+@|!")
			if typed && filepath.IsAbs(exact) {
				paths = append(paths, "/"+strings.Trim(exact, "/"))
			}
			found[property] = append(found[property], paths...)
			if root, directory := operationDirectoryRoots[property]; directory {
				for _, entry := range strings.Fields(value) {
					if entry == "." || strings.HasPrefix(entry, "../") || filepath.IsAbs(entry) ||
						filepath.Clean(entry) != entry || strings.ContainsAny(entry, ":\\%\"'") {
						return nil, fmt.Errorf("retained-node-path-unknown: %s %s=%q", unit, property, entry)
					}
					found[property] = append(found[property], filepath.Join(root, entry))
				}
			}
		}
		slices.Sort(found[property])
		found[property] = slices.Compact(found[property])
		if len(found[property]) == 0 {
			delete(found, property)
		}
	}
	return found, nil
}

func sortedRetainedPathProperties(paths map[string][]string) []string {
	properties := make([]string, 0, len(paths))
	for property := range paths {
		properties = append(properties, property)
	}
	slices.Sort(properties)
	return properties
}

// Values can contain command records, colon-separated mappings, optional
// prefixes and condition negation. No such syntax grants a path exemption.
func operationAbsolutePaths(value string) []string {
	var paths []string
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("=\"';:{}()[]|!", r)
	}) {
		token = strings.TrimLeft(token, "-+@")
		if !filepath.IsAbs(token) {
			continue
		}
		// Preserve dot components for the traversal proof; trim only slashes.
		token = "/" + strings.Trim(token, "/")
		paths = append(paths, token)
	}
	return paths
}

func (w *operationWalk) admitRetainedNodePath(path string, disposable []string) error {
	// Archive dependence takes priority and has no optional/runtime exception.
	pathErr := w.admitRetainedInput(path, nil, "retained-node-path-archived")
	if pathErr != nil && strings.HasPrefix(pathErr.Error(), "retained-node-path-archived:") {
		return pathErr
	}
	roots := w.inspector.retainedInputRoots
	if len(roots) == 0 {
		roots = []string{"/run", "/tmp", "/var/tmp"}
	}
	binding, complete := w.paths[path]
	if !complete {
		// An unresolved leaf still carries positive traversal evidence.
		var walkErr error
		binding, walkErr = resolveOperationPath(path)
		if pathErr == nil {
			pathErr = walkErr
		}
	}
	entries := []string{path, binding.Resolved}
	for _, object := range binding.Objects {
		entries = append(entries, object.Path)
	}
	for _, root := range roots {
		resolved, err := w.bindPath(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !Contained(root, entry) && !Contained(resolved, entry) {
				continue
			}
			// Traversal necessarily visits /run and its ancestors of the owned
			// directory. They are disposable only on a path ending beneath it.
			aliasEntry := entry
			if Contained(root, entry) {
				rel, err := filepath.Rel(root, entry)
				if err != nil {
					return err
				}
				aliasEntry = filepath.Join(resolved, rel)
			}
			owned := slices.ContainsFunc(disposable, func(dir string) bool { return Contained(dir, binding.Resolved) }) &&
				slices.ContainsFunc(disposable, func(dir string) bool {
					return Contained(dir, aliasEntry) || Contained(aliasEntry, dir)
				})
			if !owned {
				return fmt.Errorf("retained-input-volatile: %s traverses %s", path, root)
			}
		}
	}
	return pathErr
}

func (i *Inspector) operationPropertyStrings(ctx context.Context, unit, property string) ([]string, error) {
	if i.operationPass != nil {
		values, err := i.operationPass.typedProperties(ctx, i, unit)
		if err != nil {
			return nil, err
		}
		reply, ok := values[property]
		var value any
		if !ok || reply.Type == "" || len(reply.Data) == 0 || string(reply.Data) == "null" || json.Unmarshal(reply.Data, &value) != nil {
			return nil, fmt.Errorf("retained-node-path-unknown: %s %s has no typed value", unit, property)
		}
		return operationValueStrings(value), nil
	}
	var readErr error
	for _, kind := range []string{"Service", "Unit"} {
		reply, err := i.operationTypedProperty(ctx, unit, "org.freedesktop.systemd1."+kind, property)
		if err != nil {
			readErr = err
			continue
		}
		var value any
		if err := json.Unmarshal(reply.Data, &value); err != nil {
			return nil, fmt.Errorf("retained-node-path-unknown: %s %s has an unreadable value", unit, property)
		}
		return operationValueStrings(value), nil
	}
	return nil, fmt.Errorf("retained-node-path-unknown: read %s %s: %w", unit, property, readErr)
}

func operationValueStrings(value any) []string {
	var stringsFound []string
	switch v := value.(type) {
	case string:
		stringsFound = append(stringsFound, v)
	case []any:
		for _, entry := range v {
			stringsFound = append(stringsFound, operationValueStrings(entry)...)
		}
	case map[string]any:
		for _, entry := range v {
			stringsFound = append(stringsFound, operationValueStrings(entry)...)
		}
	}
	return stringsFound
}
