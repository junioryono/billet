package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func proveRetireConfigLeaf(path string) *retireRefusal {
	info, err := os.Lstat(path)
	if err != nil {
		return retireUnknown("retained-config-path-unknown", "examine the configuration pathname: "+err.Error(), "")
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return retireUnknown("retained-config-path-unknown", "read the configuration symlink: "+err.Error(), "")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return retireUnknown("retained-config-symlink", "configuration is a leaf symlink; configure its target directly: "+target, "")
}

// A matching digest cannot authorize moving the configuration into a directory
// retirement renames. Both request operands remain bound to the recorded name.
func proveRetireConfigPath(ctx context.Context, path string, j retirement.Journal) *retireRefusal {
	if j.Phase == retirement.PhaseDone || j.Variant != retirement.VariantRetainedNode {
		return nil
	}
	refuse := func() *retireRefusal {
		return retireUnknown("retained-config-path-changed", "the CLI, loaded node ExecStart and recorded configuration must retain the same lexical and resolved pathname", "")
	}
	want := j.RetainedInvocation
	if want == nil || want.ConfigPath == "" {
		return retireUnknown(retireReasonStopped, "the journal has no original retained-node configuration path and invocation evidence", "")
	}
	if path != want.ConfigPath {
		return refuse()
	}
	records, err := unitExecStart(ctx, nodeUnit)
	if err != nil || len(records) != 1 || len(records[0].Argv) != 4 ||
		records[0].Argv[2] != "--config" || records[0].Argv[3] != want.ConfigPath {
		return refuse()
	}
	resolved, err := lifeops.ResolveOperationPath(path)
	if err != nil {
		return refuse()
	}
	for _, resource := range want.Resources {
		if resource.Path == want.ConfigPath && resource.ResolvedPath != "" && resource.ResolvedPath == resolved {
			return proveRetireConfigLeaf(path)
		}
	}
	return refuse()
}
