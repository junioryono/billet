package lifeops

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestEnvironmentFilesRequiresTypedArrayAndPreservesOptionality(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		want        []EnvironmentFile
	}{
		{"empty", `{"type":"a(sb)","data":[]}`, []EnvironmentFile{}},
		{"mixed", `{"type":"a(sb)","data":[["/etc/one",false],["/run/two",true],["/etc/three",false]]}`, []EnvironmentFile{{Path: "/etc/one"}, {Path: "/run/two", IgnoreErrors: true}, {Path: "/etc/three"}}},
		{"escaped path", `{"type":"a(sb)","data":[["/etc/a b\\c",true]]}`, []EnvironmentFile{{Path: "/etc/a b\\c", IgnoreErrors: true}}},
		{"missing", ``, nil},
		{"missing data", `{"type":"a(sb)"}`, nil},
		{"null", `{"type":"a(sb)","data":null}`, nil},
		{"wrong signature", `{"type":"as","data":[]}`, nil},
		{"object", `{"type":"a(sb)","data":{}}`, nil},
		{"missing flag", `{"type":"a(sb)","data":[["/etc/one"]]}`, nil},
		{"string flag", `{"type":"a(sb)","data":[["/etc/one","false"]]}`, nil},
		{"null flag", `{"type":"a(sb)","data":[["/etc/one",null]]}`, nil},
		{"null path", `{"type":"a(sb)","data":[[null,false]]}`, nil},
		{"relative", `{"type":"a(sb)","data":[["relative",false]]}`, nil},
		{"extra field", `{"type":"a(sb)","data":[["/etc/one",false,0]]}`, nil},
		{"trailing output", `{"type":"a(sb)","data":[]} extra`, nil},
		{"query failure", `{"type":"a(sb)","data":[]}`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			i := NewInspector(WithOperationBusctl("typed-manager"), withRunner(func(ctx context.Context, bin string, args []string) ([]byte, error) {
				calls++
				want := []string{"--json=short", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1/unit/billet_2dnode_2eservice", "org.freedesktop.systemd1.Service", "EnvironmentFiles"}
				if bin != "typed-manager" || !slices.Equal(args, want) {
					t.Fatalf("wrong typed read: %s %v", bin, args)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded typed read")
				}
				if c.name == "query failure" {
					return []byte(c.reply), errors.New("bus unavailable")
				}
				return []byte(c.reply), nil
			}))
			got, err := i.EnvironmentFiles(t.Context(), "billet-node.service")
			if calls != 1 || (err != nil) != (c.want == nil) || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("files=%+v error=%v calls=%d; want %+v", got, err, calls, c.want)
			}
		})
	}
}

// Every empty hook/remapping decision needs positive typed evidence. A text
// omission must neither hide a configured entry nor turn a bus failure into no.
func TestServiceInspectionProvesEmptyStructuredArrays(t *testing.T) {
	for _, property := range []string{"ExecStartPre", "ExecStartPost", "ExecCondition", "ExecStopPost", "ExecReload", "BindPaths", "BindReadOnlyPaths", "MountImages"} {
		t.Run(property, func(t *testing.T) {
			a := &answers{reply: map[string]string{"billet-node.service": measured(nil)}}
			mode := "empty"
			queried := false
			i := NewInspector(withRunner(func(ctx context.Context, bin string, args []string) ([]byte, error) {
				out, err := a.run(ctx, bin, args)
				if args[0] != "get-property" {
					return out, err
				}
				index := slices.Index(args[4:], property)
				if index < 0 {
					return out, err
				}
				queried = true
				if mode == "failure" {
					return nil, errors.New("typed bus unavailable")
				}
				if mode == "configured" {
					lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
					lines[index] = fixtureOperationSignature(property) + " 1 configured"
					return []byte(strings.Join(lines, "\n") + "\n"), nil
				}
				return out, err
			}))
			for _, scenario := range []string{"empty", "configured", "failure"} {
				mode, queried = scenario, false
				facts, err := i.service(t.Context(), "billet-node.service", "", nil, errors.New("unused executable"))
				if !queried || (err != nil) != (mode == "failure") {
					t.Fatalf("%s %s: queried=%v err=%v", property, mode, queried, err)
				}
				if mode == "failure" {
					continue
				}
				_, hook := facts.ExecExtra[property]
				_, namespace := facts.Namespace[property]
				if (hook || namespace) != (mode == "configured") {
					t.Fatalf("%s %s: hooks=%v namespace=%v", property, mode, facts.ExecExtra, facts.Namespace)
				}
			}
		})
	}
}

func TestEnvironmentFileRenderingPreservesTheLiteralGrammar(t *testing.T) {
	for _, c := range []struct {
		name   string
		files  []EnvironmentFile
		want   string
		refuse bool
	}{
		{"empty", []EnvironmentFile{}, "", false},
		{"required", []EnvironmentFile{{Path: "/etc/node.env"}}, "EnvironmentFile=/etc/node.env\n", false},
		{"optional", []EnvironmentFile{{Path: "/etc/node.env", IgnoreErrors: true}}, "EnvironmentFile=-/etc/node.env\n", false},
		{"unknown", nil, "", true},
		{"multiple", []EnvironmentFile{{Path: "/etc/a"}, {Path: "/etc/b"}}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := RenderEnvironmentFiles(c.files)
			if (err != nil) != c.refuse || got != c.want {
				t.Fatalf("rendered %q, %v; want %q refusal=%v", got, err, c.want, c.refuse)
			}
		})
	}
	for _, path := range []string{"relative", "/etc/a b", "/etc/a\t", "/etc/a\x7f", "/etc/%S", `/etc/a\x20b`, `/etc/"a"`, "/etc/a*", "/etc/a\u00a0b"} {
		if _, err := RenderEnvironmentFiles([]EnvironmentFile{{Path: path}}); err == nil || !strings.Contains(err.Error(), "literal unit grammar") {
			t.Fatalf("unsupported path was rendered: %q %v", path, err)
		}
	}
}
