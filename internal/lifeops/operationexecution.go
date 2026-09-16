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
	reasons := make([]string, 0, len(refusals))
	for _, refusal := range refusals {
		reasons = append(reasons, refusal.What)
	}
	if len(reasons) != 0 {
		return fmt.Errorf("operation-execution-unsupported: %s", strings.Join(reasons, "; "))
	}
	return nil
}

// Stdio modes are the manager's loaded values ("truncate", not its pathname).
// Refuse external descriptors/files and setup redirection for billet's units at
// every admission, including stops that can spawn ExecStop or ExecStopPost.
func admitOperationSetupValues(unit string, ev operationEvidence) error {
	if first(ev.props, "LoadState") != "loaded" {
		return nil
	}
	for _, property := range []string{"StandardInput", "StandardOutput", "StandardError"} {
		value := first(ev.props, property)
		allowed := value == "null" || (property != "StandardInput" && (value == "journal" || value == "inherit"))
		if !allowed {
			return fmt.Errorf("operation-setup-unsupported: %s %s", unit, property)
		}
	}
	for _, property := range []string{"PIDFile", "PAMName", "LogNamespace", "NetworkNamespacePath", "IPCNamespacePath", "UtmpIdentifier",
		"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories"} {
		if first(ev.props, property) != "" {
			return fmt.Errorf("operation-setup-unsupported: %s %s", unit, property)
		}
	}
	return nil
}
