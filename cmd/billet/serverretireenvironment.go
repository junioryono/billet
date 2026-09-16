package main

import (
	"context"
	"fmt"
	"strings"
)

func requiredRetireEnvironmentFiles(ctx context.Context) ([]string, error) {
	specs, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil {
		return nil, fmt.Errorf("retained-input-environment-unknown: read EnvironmentFiles: %w", err)
	}
	paths := make([]string, 0, len(specs))
	for _, spec := range specs {
		if strings.ContainsAny(spec.Path, " \t\n\r\x00%*?[]\\") {
			return nil, fmt.Errorf("retained-input-environment-unknown: unsupported EnvironmentFiles path")
		}
		if !spec.IgnoreErrors {
			paths = append(paths, spec.Path)
		}
	}
	return paths, nil
}
