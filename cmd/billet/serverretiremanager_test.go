package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("BILLET_RETIRE_MANAGER_FAKE") == "1" {
		name := filepath.Base(os.Args[0])
		if name == "systemctl" || name == "busctl" || name == "busctl-execution" {
			if err := runRetireManagerFake(name, os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func retireManagerExecutable(t *testing.T, name string) string {
	t.Helper()
	binary, err := os.Executable()
	mustOK(t, err)
	path := filepath.Join(t.TempDir(), name)
	mustOK(t, os.Symlink(binary, path))
	t.Setenv("BILLET_RETIRE_MANAGER_FAKE", "1")
	// Race-instrumented helper exits must not each sleep for the default second.
	// The parent test still runs with the race detector and its original options.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return path
}

// A wildcard-aware fake would conceal the retained inventory regression.
func TestRetireManagerFakeShowFiltersLiteralPropertyNames(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BILLET_FAKE_UNITS", root)
	binary := retireManagerExecutable(t, "systemctl")
	writeFile(t, filepath.Join(root, nodeUnit), "Id="+nodeUnit+"\n", 0o600)
	writeFile(t, filepath.Join(root, nodeUnit+".effects"), "Conditions=[unprintable]\n", 0o600)
	for _, c := range []struct {
		name string
		args []string
		full bool
	}{
		{"unfiltered", nil, true},
		{"all", []string{"--all"}, true},
		{"literal star", []string{"--property=Id,*"}, false},
		{"all with literal star", []string{"--all", "--property=Id,*"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"show"}, c.args...)
			args = append(args, "--", nodeUnit)
			out, err := exec.CommandContext(t.Context(), binary, args...).Output()
			mustOK(t, err)
			want := "Id=" + nodeUnit + "\n"
			if c.full {
				want = "Conditions=[unprintable]\n" + want
			}
			if string(out) != want {
				t.Fatalf("fake invented property expansion: got %q want %q", out, want)
			}
		})
	}
}

func appendRetireManagerRecord(root, name, value string) error {
	file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func readRetireManagerProperties(root, unit string) (map[string][]string, error) {
	props := make(map[string][]string)
	for _, name := range []string{unit, unit + ".effects"} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				props[key] = append(props[key], value)
			}
		}
	}
	if slices.Contains(props["LoadState"], "not-found") {
		for _, name := range []string{"FragmentPath", "SourcePath", "DropInPaths", "UnitFileState"} {
			props[name] = []string{""}
		}
	}
	return props, nil
}

func runRetireManagerFake(name string, args []string) error {
	root := os.Getenv("BILLET_FAKE_UNITS")
	if err := appendRetireManagerRecord(root, ".manager-calls", name+" "+strings.Join(args, " ")); err != nil {
		return err
	}
	if name != "systemctl" {
		return runRetireBusFake(root, name, args)
	}
	if len(args) < 3 {
		return fmt.Errorf("incomplete systemctl request: %v", args)
	}
	unit := args[len(args)-1]
	if args[0] != "show" {
		if args[0] != "stop" && args[0] != "disable" {
			return fmt.Errorf("unsupported systemctl command: %v", args)
		}
		if err := appendRetireManagerRecord(root, ".submitted", args[0]+" "+unit); err != nil {
			return err
		}
		body, err := os.ReadFile(filepath.Join(root, unit))
		if err != nil {
			return err
		}
		changes := map[string]string{"UnitFileState": "disabled"}
		if args[0] == "stop" {
			changes = map[string]string{"ActiveState": "inactive", "SubState": "dead", "Result": "success", "MainPID": "0"}
		}
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		for key, value := range changes {
			lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, key+"=") })
			lines = append(lines, key+"="+value)
		}
		return os.WriteFile(filepath.Join(root, unit), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	props, err := readRetireManagerProperties(root, unit)
	if err != nil {
		return err
	}
	var requested []string
	for _, arg := range args[1 : len(args)-1] {
		if value, ok := strings.CutPrefix(arg, "--property="); ok {
			requested = append(requested, strings.Split(value, ",")...)
		}
	}
	var names []string
	for property := range props {
		if len(requested) == 0 || slices.Contains(requested, property) {
			names = append(names, property)
		}
	}
	slices.Sort(names)
	var out strings.Builder
	for _, property := range names {
		for _, value := range props[property] {
			if value == "" && retireFakeOmitsEmptyArray(property) {
				continue
			}
			fmt.Fprintf(&out, "%s=%s\n", property, value)
		}
	}
	if _, err := fmt.Fprint(os.Stdout, out.String()); err != nil {
		return err
	}
	state := ""
	if slices.Contains(names, "ActiveState") {
		state = "ActiveState=" + strings.Join(props["ActiveState"], "\nActiveState=")
	}
	if err := appendRetireManagerRecord(root, ".asked", unit+" "+state); err != nil {
		return err
	}
	if slices.Contains(requested, "ExecMainStatus") {
		return appendRetireManagerRecord(root, ".backup-observed", unit)
	}
	return nil
}

func retireBusUnit(object string) (string, error) {
	encoded, ok := strings.CutPrefix(object, "/org/freedesktop/systemd1/unit/")
	if !ok {
		return "", fmt.Errorf("unknown bus object %s", object)
	}
	var unit strings.Builder
	for n := 0; n < len(encoded); n++ {
		if encoded[n] != '_' {
			unit.WriteByte(encoded[n])
			continue
		}
		if n+2 >= len(encoded) {
			return "", fmt.Errorf("invalid bus object %s", object)
		}
		decoded, err := hex.DecodeString(encoded[n+1 : n+3])
		if err != nil {
			return "", err
		}
		unit.Write(decoded)
		n += 2
	}
	return unit.String(), nil
}

func retireFakeArraySignature(property string) string {
	switch property {
	case "StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink":
		return "a(sst)"
	case "BindPaths", "BindReadOnlyPaths":
		return "a(ssbt)"
	case "MountImages":
		return "a(ssba(ss))"
	case "ExtensionImages":
		return "a(sba(ss))"
	case "TemporaryFileSystem":
		return "a(ss)"
	case "EnvironmentFiles":
		return "a(sb)"
	case "ExecStartEx":
		return "a(sasasttttuii)"
	case "ExecCondition", "ExecStart", "ExecStartPre", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost":
		return "a(sasbttttuii)"
	default:
		return ""
	}
}

// v255 systemctl-show.c prints these arrays only from inside the entry loop.
func retireFakeOmitsEmptyArray(property string) bool {
	return property == "EnvironmentFiles" || strings.HasSuffix(property, "DirectorySymlink") || strings.HasPrefix(property, "Exec") && retireFakeArraySignature(property) != "" ||
		slices.Contains([]string{"Paths", "Listen", "TimersMonotonic", "TimersCalendar", "LogFilterPatterns", "BPFProgram", "SocketBindAllow", "SocketBindDeny", "OpenFile"}, property)
}

func runRetireBusFake(root, name string, args []string) error {
	if len(args) < 5 {
		return fmt.Errorf("incomplete bus request: %v", args)
	}
	jsonMode := args[0] == "--json=short"
	if jsonMode {
		args = args[1:]
	}
	if args[1] != "org.freedesktop.systemd1" {
		return fmt.Errorf("unknown bus destination: %v", args)
	}
	unit, err := retireBusUnit(args[2])
	if err != nil {
		return err
	}
	if jsonMode && args[0] == "get-property" && len(args) == 5 {
		if realUnit := os.Getenv("BILLET_RETIRE_REAL_ENVIRONMENT_UNIT"); realUnit != "" && unit == nodeUnit && args[4] == "EnvironmentFiles" {
			forwarded := append([]string{"--json=short"}, args...)
			forwarded[3] = "/org/freedesktop/systemd1/unit/" + busLabel(realUnit)
			if err := appendRetireManagerRecord(root, ".real-environment-calls", strings.Join(forwarded, " ")); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "/usr/bin/busctl", forwarded...).Output()
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(out)
			return err
		}
		file := unit + "." + args[4] + ".json"
		if name == "busctl-execution" && unit == nodeUnit && args[4] == "ExecStart" {
			file = "node-exec.json"
		}
		body, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(body)
		return err
	}
	props, err := readRetireManagerProperties(root, unit)
	if err != nil {
		return err
	}
	if jsonMode && args[0] == "call" && len(args) == 7 && args[3] == "org.freedesktop.DBus.Properties" && args[4] == "GetAll" && args[5] == "s" && args[6] == "" {
		values := make(map[string]any)
		files, err := filepath.Glob(filepath.Join(root, unit+".*.json"))
		if err != nil {
			return err
		}
		for _, file := range files {
			property := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), unit+"."), ".json")
			if _, ok := props[property]; !ok {
				props[property] = nil
			}
		}
		for property, entries := range props {
			body, err := os.ReadFile(filepath.Join(root, unit+"."+property+".json"))
			if err == nil {
				var value any
				if err := json.Unmarshal(body, &value); err != nil {
					return err
				}
				values[property] = value
				continue
			}
			if !os.IsNotExist(err) {
				return err
			}
			if slices.Contains(entries, "[unprintable]") {
				return fmt.Errorf("missing typed fixture value for %s %s", unit, property)
			}
			signature := retireFakeArraySignature(property)
			if signature != "" {
				data := []any{}
				if strings.Join(entries, "") != "" {
					data = append(data, entries)
				}
				values[property] = map[string]any{"type": signature, "data": data}
			} else {
				values[property] = map[string]any{"type": "as", "data": entries}
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "a{sv}", "data": []any{values}})
	}
	if jsonMode || args[0] != "get-property" || args[3] != "org.freedesktop.systemd1.Service" {
		return fmt.Errorf("unsupported typed request: %v", args)
	}
	for _, property := range args[4:] {
		signature := retireFakeArraySignature(property)
		values, ok := props[property]
		if signature == "" || !ok || len(values) != 1 {
			return fmt.Errorf("missing typed property %s %s", unit, property)
		}
		line := signature + " 0"
		if values[0] != "" {
			line = signature + " 1 " + values[0]
		}
		if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
			return err
		}
	}
	return nil
}

func TestRetireManagerFakeOmitsOnlySystemd255EmptyStructuredArrays(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BILLET_FAKE_UNITS", root)
	binary := retireManagerExecutable(t, "systemctl")
	properties := []string{"EnvironmentFiles", "ExecStart", "ExecStartEx", "ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink", "BindPaths", "BindReadOnlyPaths", "MountImages", "ExtensionImages", "TemporaryFileSystem"}
	writeFile(t, filepath.Join(root, nodeUnit), "LoadState=loaded\n", 0o600)
	writeFile(t, filepath.Join(root, nodeUnit+".effects"), strings.Join(properties, "=\n")+"=\n", 0o600)
	for _, filtered := range []bool{false, true} {
		args := []string{"show", "--all"}
		if filtered {
			args = append(args, "--property="+strings.Join(properties, ","))
		}
		args = append(args, "--", nodeUnit)
		out, err := exec.CommandContext(t.Context(), binary, args...).Output()
		mustOK(t, err)
		for _, property := range properties {
			printsEmpty := slices.Contains([]string{"BindPaths", "BindReadOnlyPaths", "MountImages", "ExtensionImages", "TemporaryFileSystem"}, property)
			if strings.Contains(string(out), property+"=\n") != printsEmpty {
				t.Fatalf("wrong empty printer for %s filtered=%v: %s", property, filtered, out)
			}
		}
	}
	bus := retireManagerExecutable(t, "busctl")
	out, err := exec.CommandContext(t.Context(), bus, "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.systemd1.Service", "EnvironmentFiles", "ExecStopPost", "StateDirectorySymlink").Output()
	mustOK(t, err)
	if string(out) != "a(sb) 0\na(sasbttttuii) 0\na(sst) 0\n" {
		t.Fatalf("empty typed arrays: %q", out)
	}
}
