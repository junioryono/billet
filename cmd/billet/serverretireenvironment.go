package main

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/lifeops"
)

func requiredRetireEnvironmentFiles(ctx context.Context) ([]string, error) {
	specs, err := retireOperationInspector().EnvironmentFiles(ctx, nodeUnit)
	if err != nil {
		return nil, fmt.Errorf("retained-input-environment-unknown: read EnvironmentFiles: %w", err)
	}
	if _, err := lifeops.RenderEnvironmentFiles(specs); err != nil {
		return nil, fmt.Errorf("retained-input-environment-unknown: %w", err)
	}
	paths := make([]string, 0, len(specs))
	for _, spec := range specs {
		if !spec.IgnoreErrors {
			paths = append(paths, spec.Path)
		}
	}
	return paths, nil
}
