package lifeops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRetirementTimerHelpersRequirePositiveAbsence(t *testing.T) {
	for _, verb := range []string{"stop", "disable"} {
		for _, problem := range []string{"absent", "loaded", "masked", "masked-runtime", "masked dev-null", "masked active", "masked job", "masked fragment", "masked missing", "masked target", "masked enablement", "read error", "missing state", "active", "job", "fragment", "enablement", "command error"} {
			t.Run(verb+"/"+problem, func(t *testing.T) {
				props := map[string]string{"LoadState": "not-found", "ActiveState": "inactive", "UnitFileState": "", "FragmentPath": "", "Job": ""}
				switch problem {
				case "loaded", "command error":
					props["LoadState"], props["FragmentPath"], props["UnitFileState"] = "loaded", "/run/systemd/system/retirement.timer", "disabled"
				case "masked", "masked-runtime", "masked dev-null", "masked active", "masked job", "masked fragment", "masked missing", "masked target", "masked enablement":
					props["LoadState"], props["FragmentPath"], props["UnitFileState"] = "masked", "/dev/null", "masked"
					if problem != "masked dev-null" {
						path := filepath.Join(t.TempDir(), "retirement.timer")
						props["FragmentPath"] = path
						if problem != "masked missing" {
							target := "/dev/null"
							if problem == "masked target" {
								target = "/dev/zero"
							}
							if err := os.Symlink(target, path); err != nil {
								t.Fatal(err)
							}
						}
					}
				case "missing state":
					delete(props, "ActiveState")
				case "active":
					props["ActiveState"] = "active"
				case "job":
					props["Job"] = "42"
				case "fragment":
					props["FragmentPath"] = "/run/systemd/system/retirement.timer"
				case "enablement":
					props["UnitFileState"] = "enabled"
				}
				if problem == "masked-runtime" {
					props["UnitFileState"] = "masked-runtime"
				}
				switch problem {
				case "masked active":
					props["ActiveState"] = "active"
				case "masked job":
					props["Job"] = "42"
				case "masked fragment":
					if err := os.Remove(props["FragmentPath"]); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(props["FragmentPath"], nil, 0o644); err != nil {
						t.Fatal(err)
					}
				case "masked enablement":
					props["UnitFileState"] = "enabled"
				}
				var commands []string
				failure := errors.New("manager failed")
				c := NewConverger(NewInspector(withRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
					if args[0] != "show" {
						commands = append(commands, strings.Join(args, " "))
						if problem == "command error" {
							// Later absence must not erase this failed command.
							props["LoadState"], props["FragmentPath"], props["UnitFileState"] = "not-found", "", ""
							return nil, failure
						}
						return nil, nil
					}
					if problem == "read error" {
						return nil, failure
					}
					var out strings.Builder
					for _, arg := range args {
						if name, ok := strings.CutPrefix(arg, "--property="); ok {
							if value, present := props[name]; present {
								fmt.Fprintf(&out, "%s=%s\n", name, value)
							}
						}
					}
					return []byte(out.String()), nil
				})))
				var err error
				var result StopResult
				if verb == "stop" {
					result, err = c.StopAndProve(t.Context(), "retirement.timer")
				} else {
					err = c.Disable(t.Context(), "retirement.timer")
				}
				if slices.Contains([]string{"absent", "loaded", "masked", "masked-runtime", "masked dev-null"}, problem) {
					if err != nil || (verb == "stop" && result.Gone != Yes) {
						t.Fatalf("supported timer: result=%+v error=%v", result, err)
					}
					want := []string{verb + " -- retirement.timer"}
					if problem != "loaded" {
						want = nil
						how := problem
						if problem == "absent" {
							how = "not-found"
						} else if problem == "masked dev-null" {
							how = "masked"
						}
						if verb == "stop" && result.How != how {
							t.Fatalf("absence was not reported: %+v", result)
						}
					}
					if !slices.Equal(commands, want) {
						t.Fatalf("timer commands=%v want=%v", commands, want)
					}
					return
				}
				if err == nil || (verb == "stop" && result.Gone == Yes) {
					t.Fatalf("uncertainty became absence: result=%+v error=%v", result, err)
				}
				if problem == "masked missing" && !strings.Contains(err.Error(), "operation-mask-unreadable: lstat ") {
					t.Fatalf("mask read failure was not could-not-tell: %v", err)
				}
				if problem == "command error" {
					if !errors.Is(err, failure) || len(commands) != 1 {
						t.Fatalf("command failure suppressed: commands=%v error=%v", commands, err)
					}
				} else if len(commands) != 0 {
					t.Fatalf("unknown timer submitted command: %v", commands)
				}
			})
		}
	}
}
