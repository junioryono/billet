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

// WithOperationCgroupRoot selects the mounted unified cgroup hierarchy.
func WithOperationCgroupRoot(root string) Option {
	return func(i *Inspector) { i.operationCgroupRoot = root }
}

// AdmitUnitTermination must be called with current evidence at the stop boundary.
func (i *Inspector) AdmitUnitTermination(ctx context.Context, unit string) error {
	props, err := i.properties(ctx, unit, "KillMode")
	if err != nil {
		return fmt.Errorf("operation-termination-unknown: %s: %w", unit, err)
	}
	if len(props["KillMode"]) != 1 || !slices.Contains([]string{"control-group", "mixed"}, first(props, "KillMode")) {
		return fmt.Errorf("operation-termination-unsupported: %s KillMode=%s", unit, first(props, "KillMode"))
	}
	return nil
}

// ProveUnitProcessesGone checks the cgroup even when systemd cleared MainPID.
// Empty ControlGroup is accepted only with the standard system.slice placement,
// whose path remains identifiable after systemd releases the cgroup reference.
func (i *Inspector) ProveUnitProcessesGone(ctx context.Context, unit string) error {
	props, err := i.properties(ctx, unit, "ControlGroup", "Slice")
	if err != nil {
		return fmt.Errorf("operation-processes-unknown: %s: %w", unit, err)
	}
	if err := requireOperationProperties(unit, props, []string{"ControlGroup", "Slice"}); err != nil {
		return err
	}
	group := first(props, "ControlGroup")
	if group == "" {
		if first(props, "Slice") != "system.slice" || !operationUnitName(unit) || strings.ContainsAny(unit, "@\\") {
			return fmt.Errorf("operation-processes-unknown: %s has no identifiable cgroup", unit)
		}
		group = "/system.slice/" + unit
	}
	if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
		return fmt.Errorf("operation-processes-unknown: %s has invalid ControlGroup=%q", unit, group)
	}
	root := i.operationCgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	if _, err := os.ReadDir(root); err != nil {
		return fmt.Errorf("operation-processes-unknown: cgroup root: %w", err)
	}
	path := filepath.Join(root, strings.TrimPrefix(group, "/"))
	if err := proveOperationCgroupEmpty(path); err != nil {
		return fmt.Errorf("operation-processes-unknown: %s: %w", unit, err)
	}
	after, err := i.properties(ctx, unit, "ControlGroup", "Slice")
	if err != nil || !reflect.DeepEqual(props, after) {
		return fmt.Errorf("operation-processes-unknown: %s cgroup changed during observation", unit)
	}
	return nil
}

func proveOperationCgroupEmpty(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a direct cgroup directory", path)
	}
	// Descendants can hold processes after the parent's cgroup.procs is empty.
	return filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(filepath.Join(current, "cgroup.procs"))
		if err != nil {
			return err
		}
		if len(body) != 0 {
			return fmt.Errorf("controller processes remain in %s", current)
		}
		return nil
	})
}
