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

// AdmitUnitReplacement admits an exact re-render of the loaded definition.
// Changed definitions are outside the currently loaded operation-effects graph.
func (i *Inspector) AdmitUnitReplacement(ctx context.Context, unit, path, body, environmentLine string) error {
	props, err := i.properties(ctx, unit, "LoadState", "FragmentPath", "DropInPaths", "NeedDaemonReload")
	if err != nil {
		return err
	}
	if err := requireOperationProperties(unit, props, []string{"LoadState", "FragmentPath", "DropInPaths", "NeedDaemonReload"}); err != nil {
		return err
	}
	if first(props, "LoadState") != "loaded" || first(props, "FragmentPath") != path || first(props, "DropInPaths") != "" || first(props, "NeedDaemonReload") != "no" {
		return fmt.Errorf("operation-proposed-unit-unsupported: installed and loaded unit sources differ")
	}
	sources, err := readOperationSources(props)
	if err != nil {
		return err
	}
	if len(sources) != 1 || sources[0].Body != body {
		return fmt.Errorf("operation-proposed-unit-changed: only the installed definition can be re-rendered")
	}
	var environment strings.Builder
	for _, line := range strings.Split(body, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "EnvironmentFile" {
			environment.WriteString(line + "\n")
		}
	}
	if environment.String() != environmentLine {
		return fmt.Errorf("operation-proposed-environment-mismatch: preserve the loaded filename and optionality")
	}
	return nil
}
