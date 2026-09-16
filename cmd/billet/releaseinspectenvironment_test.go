package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
)

func TestReleaseInspectPublishesTypedEnvironmentOptionality(t *testing.T) {
	for _, optional := range []bool{false, true} {
		for _, present := range []bool{false, true} {
			t.Run(map[bool]string{true: "optional", false: "required"}[optional]+"/"+map[bool]string{true: "present", false: "absent"}[present], func(t *testing.T) {
				f := newInspectFixture(t)
				path := filepath.Join(f.dir, "node.env")
				if present {
					writeFile(t, path, "", 0o600)
				}
				f.unitEnvironment(t, serverUnit, []any{[]any{path, optional}})
				svc := f.report(t).Services["server"]
				want := []lifeops.EnvironmentFile{{Path: path, IgnoreErrors: optional}}
				if !reflect.DeepEqual(mustKnown(t, "specs", svc.EnvironmentFileSpecs), want) {
					t.Fatalf("typed observation lost optionality: %+v", svc.EnvironmentFileSpecs)
				}
				if !reflect.DeepEqual(mustKnown(t, "paths", svc.EnvironmentFiles), []string{path}) {
					t.Fatal("path-only compatibility field changed")
				}
				body, err := json.Marshal(svc.EnvironmentFileSpecs)
				mustOK(t, err)
				if !strings.Contains(string(body), `"ignore_errors":`) || strings.Contains(string(body), `"IgnoreErrors"`) {
					t.Fatalf("specs use the wrong wire members: %s", body)
				}
			})
		}
	}
}

func TestReleaseInspectDistinguishesEmptyAbsentAndUnknownEnvironment(t *testing.T) {
	f := newInspectFixture(t)
	svc := f.report(t).Services["server"]
	if specs, ok := mustKnown(t, "empty specs", svc.EnvironmentFileSpecs).([]lifeops.EnvironmentFile); !ok || specs == nil || len(specs) != 0 {
		t.Fatal("empty loaded array is not []")
	}
	f.unitAbsent(t, serverUnit)
	if got := mustKnown(t, "absent specs", f.report(t).Services["server"].EnvironmentFileSpecs); got != nil {
		t.Fatalf("absent unit became an empty loaded array: %v", got)
	}
	f.unitRunning(t, serverUnit, "server", f.configPath, nil)
	for _, row := range []any{[]any{"/etc/node.env"}, []any{"/etc/node.env", "false"}, []any{"/etc/node.env", nil}} {
		f.unitEnvironment(t, serverUnit, []any{row})
		svc := f.report(t).Services["server"]
		mustUnknown(t, "specs", svc.EnvironmentFileSpecs, "operation-array-unknown")
		mustUnknown(t, "paths", svc.EnvironmentFiles, "operation-array-unknown")
	}
}

func TestReleaseInspectRechecksEnvironmentOptionalityForQuietUnits(t *testing.T) {
	f := newInspectFixture(t)
	saved := managerCommandRunner
	t.Cleanup(func() { managerCommandRunner = saved })
	reads := 0
	managerCommandRunner = func(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
		if args[0] == "show" {
			props := map[string]string{"LoadState": "loaded", "UnitFileState": "enabled", "ActiveState": "inactive",
				"MainPID": "0", "NeedDaemonReload": "no", "ExecStart": "{ path=" + f.binPath + "; argv[]=" + f.binPath + " server --config " + f.configPath + " ; }"}
			for _, arg := range args {
				if name, ok := strings.CutPrefix(arg, "--property="); ok {
					if _, err := fmt.Fprintf(stdout, "%s=%s\n", name, props[name]); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if len(args) != 6 || args[0] != "--json=short" || args[1] != "get-property" {
			return fmt.Errorf("unexpected manager request: %v", args)
		}
		switch args[5] {
		case "EnvironmentFiles":
			reads++
			return json.NewEncoder(stdout).Encode(map[string]any{"type": "a(sb)", "data": []any{[]any{"/etc/node.env", reads > 1}}})
		case "ExecStart":
			return json.NewEncoder(stdout).Encode(map[string]any{"type": "a(sasbttttuii)", "data": []any{[]any{f.binPath, []string{f.binPath, "server", "--config", f.configPath}}}})
		default:
			return fmt.Errorf("unexpected property: %v", args)
		}
	}
	svc, binding := inspectServiceSection(t.Context(), "server", serverUnit, nil, f.configPath, nil, "", "", nil)
	if reads != 2 {
		t.Fatalf("quiet unit received %d environment observations, want opening and closing", reads)
	}
	mustUnknown(t, "specs", svc.EnvironmentFileSpecs, "closing observation")
	mustUnknown(t, "paths", svc.EnvironmentFiles, "closing observation")
	mustUnknown(t, "binding", binding, "closing observation")
}
