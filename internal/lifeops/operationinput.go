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
// Required inputs may traverse neither volatile roots nor directories the caller
// will archive, including an intermediate symlink that leads back outside.
func (i *Inspector) AdmitRetainedInputs(paths []string, archivedRoots ...string) error {
	w := operationWalk{inspector: i, paths: make(map[string]operationPathBinding),
		protection: OperationProtection{RequiredInputs: map[string][]string{"": paths}, ArchivedInputRoots: archivedRoots}}
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
			if err := w.admitRetainedInput(path, roots, "retained-input-archived"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *operationWalk) admitRetainedInput(path string, volatileRoots []string, archiveReason string) error {
	type boundary struct {
		root   string
		reason string
	}
	boundaries := make([]boundary, 0, len(volatileRoots)+len(w.protection.ArchivedInputRoots))
	for _, root := range volatileRoots {
		boundaries = append(boundaries, boundary{root, "retained-input-volatile"})
	}
	for _, root := range w.protection.ArchivedInputRoots {
		boundaries = append(boundaries, boundary{root, archiveReason})
	}
	// A known lexical violation needs no successful traversal of the input.
	for _, bound := range boundaries {
		if Contained(bound.root, path) {
			return fmt.Errorf("%s: %s traverses %s", bound.reason, path, bound.root)
		}
	}
	forms := slices.Clone(boundaries)
	for _, bound := range boundaries {
		resolved, err := w.bindPath(bound.root)
		if err != nil {
			return err
		}
		forms = append(forms, boundary{resolved, bound.reason})
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
	// An incomplete walk can still prove either forbidden dependency. Check
	// both classes before reporting the unresolved suffix as unknown.
	for _, entry := range entries {
		for _, form := range forms {
			if Contained(form.root, entry) {
				return fmt.Errorf("%s: %s traverses %s", form.reason, path, form.root)
			}
		}
	}
	if pathErr != nil {
		return pathErr
	}
	w.paths[path] = binding
	return nil
}
