package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			if err := runRetireManagerFake(context.Background(), name, os.Args[1:], os.Stdout); err != nil {
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
	binary := "systemctl"
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
			out, err := retireManagerCommand(t.Context(), binary, args)
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

// Manager callers refuse any error; none inspect *exec.ExitError. Keep the
// numeric status and stderr available for the subprocess equivalence witness.
type retireManagerExitError struct {
	stderr string
}

func (e *retireManagerExitError) Error() string {
	return "exit status 2: " + strings.TrimSpace(e.stderr)
}

func (e *retireManagerExitError) ExitCode() int { return 2 }

func retireManagerCommand(ctx context.Context, bin string, args []string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := runRetireManagerFake(ctx, filepath.Base(bin), args, &stdout); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return stdout.Bytes(), &retireManagerExitError{stderr: fmt.Sprintln(err)}
	}
	return stdout.Bytes(), nil
}

func runRetireManagerFake(ctx context.Context, name string, args []string, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name != "systemctl" && name != "busctl" && name != "busctl-execution" {
		return fmt.Errorf("unknown fake manager %s", name)
	}
	root := os.Getenv("BILLET_FAKE_UNITS")
	if err := appendRetireManagerRecord(root, ".manager-calls", name+" "+strings.Join(args, " ")); err != nil {
		return err
	}
	if name != "systemctl" {
		return runRetireBusFake(ctx, root, name, args, stdout)
	}
	if len(args) < 3 {
		return fmt.Errorf("incomplete systemctl request: %v", args)
	}
	unit := args[len(args)-1]
	if args[0] != "show" {
		if args[0] != "stop" && args[0] != "disable" && args[0] != "reset-failed" {
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
		} else if args[0] == "reset-failed" {
			changes = map[string]string{"ActiveState": "inactive", "SubState": "dead", "Result": "success", "ExecMainStatus": "0"}
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
	if _, err := fmt.Fprint(stdout, out.String()); err != nil {
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

func runRetireBusFake(ctx context.Context, root, name string, args []string, stdout io.Writer) error {
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
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "/usr/bin/busctl", forwarded...).Output()
			if err != nil {
				return err
			}
			_, err = stdout.Write(out)
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
		_, err = stdout.Write(body)
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
		return json.NewEncoder(stdout).Encode(map[string]any{"type": "a{sv}", "data": []any{values}})
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
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}
	return nil
}

func TestRetireManagerFakeOmitsOnlySystemd255EmptyStructuredArrays(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BILLET_FAKE_UNITS", root)
	binary := "systemctl"
	properties := []string{"EnvironmentFiles", "ExecStart", "ExecStartEx", "ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink", "BindPaths", "BindReadOnlyPaths", "MountImages", "ExtensionImages", "TemporaryFileSystem"}
	writeFile(t, filepath.Join(root, nodeUnit), "LoadState=loaded\n", 0o600)
	writeFile(t, filepath.Join(root, nodeUnit+".effects"), strings.Join(properties, "=\n")+"=\n", 0o600)
	for _, filtered := range []bool{false, true} {
		args := []string{"show", "--all"}
		if filtered {
			args = append(args, "--property="+strings.Join(properties, ","))
		}
		args = append(args, "--", nodeUnit)
		out, err := retireManagerCommand(t.Context(), binary, args)
		mustOK(t, err)
		for _, property := range properties {
			printsEmpty := slices.Contains([]string{"BindPaths", "BindReadOnlyPaths", "MountImages", "ExtensionImages", "TemporaryFileSystem"}, property)
			if strings.Contains(string(out), property+"=\n") != printsEmpty {
				t.Fatalf("wrong empty printer for %s filtered=%v: %s", property, filtered, out)
			}
		}
	}
	bus := "busctl"
	out, err := retireManagerCommand(t.Context(), bus, []string{"get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.systemd1.Service", "EnvironmentFiles", "ExecStopPost", "StateDirectorySymlink"})
	mustOK(t, err)
	if string(out) != "a(sb) 0\na(sasbttttuii) 0\na(sst) 0\n" {
		t.Fatalf("empty typed arrays: %q", out)
	}
}

// Keep one subprocess witness for the protocol; admissions use no child process.
func TestRetireManagerInProcessMatchesSubprocess(t *testing.T) {
	for _, c := range []struct {
		name string
		bin  string
		args []string
		code int
	}{
		{"show", "systemctl", []string{"show", "--all", "--", nodeUnit}, 0},
		{"typed", "busctl", []string{"get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.systemd1.Service", "EnvironmentFiles"}, 0},
		{"typed inventory", "busctl", []string{"--json=short", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.DBus.Properties", "GetAll", "s", ""}, 0},
		{"execution", "busctl-execution", []string{"--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.systemd1.Service", "ExecStart"}, 0},
		{"reset", "systemctl", []string{"reset-failed", "--", nodeUnit}, 0},
		{"failure", "systemctl", []string{"unsupported", "--", nodeUnit}, 2},
		{"output then failure", "systemctl", []string{"show", "--all", "--", nodeUnit}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("BILLET_FAKE_UNITS", root)
			writeFile(t, filepath.Join(root, nodeUnit), "Id="+nodeUnit+"\nActiveState=failed\nResult=exit-code\n", 0o600)
			writeFile(t, filepath.Join(root, nodeUnit+".effects"), "EnvironmentFiles=\n", 0o600)
			writeFile(t, filepath.Join(root, "node-exec.json"), `{"type":"a(sasbttttuii)","data":[["/usr/bin/billet",["/usr/bin/billet","node","--config","/etc/billet.yaml"],false,0,0,0,0,0,0,0]]}`, 0o600)
			if c.name == "output then failure" {
				mustOK(t, os.Mkdir(filepath.Join(root, ".asked"), 0o700))
			}
			binary := retireManagerExecutable(t, c.bin)
			wantOut, wantErr := retireManagerCommand(t.Context(), binary, c.args)
			wantCode, wantStderr := 0, ""
			if wantErr != nil {
				var failure *retireManagerExitError
				if !errors.As(wantErr, &failure) {
					t.Fatal(wantErr)
				}
				wantCode, wantStderr = failure.ExitCode(), failure.stderr
			}
			var stdout, stderr bytes.Buffer
			cmd := exec.CommandContext(t.Context(), binary, c.args...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				var failure *exec.ExitError
				if !errors.As(err, &failure) {
					t.Fatal(err)
				}
				code = failure.ExitCode()
			}
			if !bytes.Equal(stdout.Bytes(), wantOut) || stderr.String() != wantStderr || code != wantCode || code != c.code {
				t.Fatalf("subprocess=(%q, %q, %d), in-process=(%q, %q, %d)", stdout.Bytes(), stderr.String(), code, wantOut, wantStderr, wantCode)
			}
		})
	}
}
