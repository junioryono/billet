package lifeops

import (
	"context"
	"fmt"
	"strings"
)

// quietTimer proves an absent or masked timer before submitting any command.
// A failed command never grants permission; role-managed hosts may lack timers.
func (c *Converger) quietTimer(ctx context.Context, unit string) (string, error) {
	if !strings.HasSuffix(unit, ".timer") {
		return "", nil
	}
	names := []string{"LoadState", "ActiveState", "UnitFileState", "FragmentPath", "Job"}
	props, err := c.inspector.properties(ctx, unit, names...)
	if err != nil {
		return "", err
	}
	if err := requireOperationProperties(unit, props, names); err != nil {
		return "", err
	}
	if first(props, "LoadState") == "not-found" {
		if first(props, "ActiveState") != "inactive" || first(props, "UnitFileState") != "" ||
			first(props, "FragmentPath") != "" || first(props, "Job") != "" {
			return "", fmt.Errorf("operation-timer-absence-inconsistent: %s", unit)
		}
		return "not-found", nil
	}
	masked, err := operationQuietMask(props)
	if err != nil {
		return "", fmt.Errorf("%s: %w", unit, err)
	}
	if masked {
		return first(props, "UnitFileState"), nil
	}
	if first(props, "LoadState") != "loaded" {
		return "", fmt.Errorf("operation-timer-load-unknown: %s LoadState=%q FragmentPath=%q UnitFileState=%q ActiveState=%q Job=%q", unit,
			first(props, "LoadState"), first(props, "FragmentPath"), first(props, "UnitFileState"), first(props, "ActiveState"), first(props, "Job"))
	}
	return "", nil
}
