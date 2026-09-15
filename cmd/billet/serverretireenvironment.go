package main

import (
	"context"
	"fmt"
	"strings"
)

// environmentFileSpec keeps systemd's optionality beside the shared inspector's
// parsed path. Path-only and typed observations use the same path parser.
type environmentFileSpec struct {
	Path         string
	IgnoreErrors bool
}

func environmentFileSpecsOfAll(lines []string) ([]environmentFileSpec, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("retained-input-environment-unknown: missing EnvironmentFiles evidence")
	}
	var specs []environmentFileSpec
	for _, line := range lines {
		paths, err := environmentFilesOfAll([]string{line})
		if err != nil {
			return nil, fmt.Errorf("retained-input-environment-unknown: %w", err)
		}
		if len(paths) == 0 {
			if len(lines) != 1 || strings.TrimRight(line, "\r") != "" {
				return nil, fmt.Errorf("retained-input-environment-unknown: inconsistent empty EnvironmentFiles evidence")
			}
			continue
		}
		if strings.ContainsAny(paths[0], " \t\n\r\x00%*?[]\\") {
			return nil, fmt.Errorf("retained-input-environment-unknown: unsupported EnvironmentFiles path")
		}
		specs = append(specs, environmentFileSpec{Path: paths[0],
			IgnoreErrors: strings.HasSuffix(strings.TrimRight(line, "\r"), " (ignore_errors=yes)")})
	}
	return specs, nil
}

func requiredRetireEnvironmentFiles(ctx context.Context) ([]string, error) {
	props, err := retireOperationInspector().UnitProperties(ctx, nodeUnit, "EnvironmentFiles")
	if err != nil {
		return nil, fmt.Errorf("retained-input-environment-unknown: read EnvironmentFiles: %w", err)
	}
	specs, err := environmentFileSpecsOfAll(props["EnvironmentFiles"])
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(specs))
	for _, spec := range specs {
		if !spec.IgnoreErrors {
			paths = append(paths, spec.Path)
		}
	}
	return paths, nil
}
