package lifeops

import (
	"context"
	"fmt"
	"strings"
)

// absentTimer establishes absence before an operation, never by suppressing a
// failed command. Role-managed server-only hosts may lack retirement timers.
func (c *Converger) absentTimer(ctx context.Context, unit string) (bool, error) {
	if !strings.HasSuffix(unit, ".timer") {
		return false, nil
	}
	names := []string{"LoadState", "ActiveState", "UnitFileState", "FragmentPath", "Job"}
	props, err := c.inspector.properties(ctx, unit, names...)
	if err != nil {
		return false, err
	}
	if err := requireOperationProperties(unit, props, names); err != nil {
		return false, err
	}
	if first(props, "LoadState") == "not-found" {
		if first(props, "ActiveState") != "inactive" || first(props, "UnitFileState") != "" ||
			first(props, "FragmentPath") != "" || first(props, "Job") != "" {
			return false, fmt.Errorf("operation-timer-absence-inconsistent: %s", unit)
		}
		return true, nil
	}
	if first(props, "LoadState") != "loaded" && first(props, "LoadState") != "masked" {
		return false, fmt.Errorf("operation-timer-load-unknown: %s", unit)
	}
	return false, nil
}
