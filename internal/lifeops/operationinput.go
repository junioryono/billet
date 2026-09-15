package lifeops

import (
	"fmt"
	"slices"
)

// WithRetainedInputRoots substitutes a fixture host's volatile roots. Production
// uses /run, /tmp and /var/tmp, including their resolved filesystem aliases.
func WithRetainedInputRoots(roots ...string) Option {
	return func(i *Inspector) { i.retainedInputRoots = slices.Clone(roots) }
}

// ownedPaths preserves ownership for directory setup checks. Required inputs
// separately pass the volatile rule before any ownership exemption is applied.
func (p OperationProtection) ownedPaths() map[string][]string {
	paths := make(map[string][]string)
	for unit, entries := range p.UnitPaths {
		paths[unit] = slices.Clone(entries)
	}
	for unit, entries := range p.RequiredInputs {
		paths[unit] = append(paths[unit], entries...)
	}
	return paths
}

func (w *operationWalk) otherUnitPaths(effect string) []string {
	var paths []string
	for unit, entries := range w.protection.UnitPaths {
		if w.canonicalUnit(unit) != w.canonicalUnit(effect) {
			paths = append(paths, entries...)
		}
	}
	return paths
}

// AdmitRetainedInputs supplies the same current path proof at stopped and
// archive boundaries, without querying units or assuming their teardown state.
func (i *Inspector) AdmitRetainedInputs(paths []string) error {
	w := operationWalk{inspector: i, paths: make(map[string]operationPathBinding),
		protection: OperationProtection{RequiredInputs: map[string][]string{"": paths}}}
	if err := w.admitRetainedInputs(); err != nil {
		return err
	}
	return w.revalidatePaths()
}

func (w *operationWalk) admitRetainedInputs() error {
	roots := w.inspector.retainedInputRoots
	if len(roots) == 0 {
		roots = []string{"/run", "/tmp", "/var/tmp"}
	}
	for _, paths := range w.protection.RequiredInputs {
		for _, path := range paths {
			if err := w.admitRetainedInput(path, roots); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *operationWalk) admitRetainedInput(path string, roots []string) error {
	// A known lexical violation needs no successful traversal of the input.
	for _, root := range roots {
		if Contained(root, path) {
			return fmt.Errorf("retained-input-volatile: %s traverses %s", path, root)
		}
	}
	forms := slices.Clone(roots)
	for _, root := range roots {
		resolved, err := w.bindPath(root)
		if err != nil {
			return err
		}
		forms = append(forms, resolved)
	}
	binding, bound := w.paths[path]
	var pathErr error
	if !bound {
		binding, pathErr = resolveOperationPath(path)
	}
	entries := []string{path, binding.Resolved}
	for _, object := range binding.Objects {
		entries = append(entries, object.Path)
	}
	// Even an incomplete walk may prove it traversed volatile storage.
	for _, entry := range entries {
		for _, root := range forms {
			if Contained(root, entry) {
				return fmt.Errorf("retained-input-volatile: %s traverses %s", path, root)
			}
		}
	}
	if pathErr != nil {
		return pathErr
	}
	w.paths[path] = binding
	return nil
}
