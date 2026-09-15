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
			path, err := w.bindPath(filepath.Join("/run", entry))
			if err != nil {
				return err
			}
			disposable = append(disposable, filepath.Join("/run", entry), path)
		}
		for property, values := range props {
			switch property {
			case "FragmentPath", "SourcePath", "DropInPaths", "ControlGroup", "ControlGroupId":
				continue
			}
			for _, value := range values {
				inputs := []string{value}
				// v255 prints unknown types and newline-bearing strings this way:
				// https://github.com/systemd/systemd/blob/v255/src/shared/bus-print-properties.c
				// Read their typed values, including Conditions/Asserts, rather than
				// treating an unrendered array as empty or enumerating path carriers.
				typed := value == "[unprintable]" || strings.Contains(value, "\\") || len(operationAbsolutePaths(value)) != 0
				if typed {
					inputs, err = w.inspector.operationPropertyStrings(ctx, unit, property)
					if err != nil {
						return err
					}
				}
				for _, input := range inputs {
					paths := operationAbsolutePaths(input)
					// Typed strings retain spaces and punctuation in a single path;
					// show's joined arrays and command arguments do not.
					exact := strings.TrimLeft(input, "-+@|!")
					if typed && filepath.IsAbs(exact) {
						paths = append(paths, "/"+strings.Trim(exact, "/"))
					}
					for _, path := range paths {
						if err := w.admitRetainedNodePath(path, disposable); err != nil {
							return fmt.Errorf("%s %s: %w", unit, property, err)
						}
					}
				}
			}
		}
	}
	return nil
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
	bin := i.operationBusctl
	if bin == "" {
		bin = "busctl"
	}
	ctx, cancel := i.bounded(ctx)
	defer cancel()
	var readErr error
	for _, kind := range []string{"Service", "Unit"} {
		args := []string{"--json=short", "get-property", "org.freedesktop.systemd1", operationObjectPath(unit), "org.freedesktop.systemd1." + kind, property}
		if i.observe != nil {
			i.observe(ctx, args)
		}
		out, err := i.run(ctx, bin, args)
		if err != nil {
			readErr = err
			continue
		}
		var reply struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(out, &reply); err != nil || reply.Type == "" || len(reply.Data) == 0 || string(reply.Data) == "null" {
			return nil, fmt.Errorf("retained-node-path-unknown: %s %s has no typed value", unit, property)
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
