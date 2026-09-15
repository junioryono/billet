package lifeops

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// AdmitExecution applies the lifecycle's existing executable, argument and
// execution-flag inspection without planning a start or requiring a live PID.
func (i *Inspector) AdmitExecution(ctx context.Context, unit, role, binary, configPath string) error {
	info, statErr := os.Stat(binary)
	facts, err := i.service(ctx, unit, "", info, statErr)
	if err != nil {
		return err
	}
	refusals := execRefusals(facts, unitSpec{role: role, configPath: configPath})
	var reasons []string
	for _, refusal := range refusals {
		reasons = append(reasons, refusal.What)
	}
	if len(reasons) != 0 {
		return fmt.Errorf("operation-execution-unsupported: %s", strings.Join(reasons, "; "))
	}
	return nil
}
