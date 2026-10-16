package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// nodeStateDirExpression matches the role template's expression for the node's
// state directory whatever variable it reads, so a rename cannot quietly stop
// this control from projecting it.
//
// NOT A [^{}] CHARACTER CLASS: the expression contains `{}` of its own, in
// `.get('node', {})`, so a class excluding braces matches nothing at all. The
// lazy quantifiers stop at the first `}}`, which is the expression's own close.
var nodeStateDirExpression = regexp.MustCompile(`\{\{.*?state_dir.*?\}\}`)

func TestRetainedNodePathsCoverEveryPropertyAtEveryAdmission(t *testing.T) {
	for _, verb := range []string{"", "enable", "disable", "stop", "start"} {
		for _, property := range []string{"ReadWritePaths", "ReadOnlyPaths", "InaccessiblePaths", "ConditionPathExists", "AssertPathExists", "WorkingDirectory", "RootDirectory", "EnvironmentFiles", "PIDFile", "LoadCredential", "ExecStart", "FutureSystemdPath"} {
			t.Run(verb+"/"+property, func(t *testing.T) {
				f := newOperationFixture(t)
				node := f.unit(t, "billet-node.service")
				p := OperationProtection{Units: []string{"billet-node.service"}, RetainedPathUnits: []string{"billet-node.service"}, ArchivedInputRoots: []string{"/var/lib/billet/server"}}
				var sequence []Operation
				if verb != "" {
					sequence = []Operation{{Verb: verb, Unit: "billet-node.service"}}
				}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
					t.Fatalf("clean admission: %v", err)
				}
				for _, prefix := range []string{"", "-", "!", "|!"} {
					node[property] = prefix + "/var/lib/billet/server"
					if property == "EnvironmentFiles" {
						// systemd reports an EnvironmentFile= entry with its optionality, not a prefix.
						ignore := "no"
						if prefix == "-" {
							ignore = "yes"
						}
						node[property] = "/var/lib/billet/server (ignore_errors=" + ignore + ")"
					}
					if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
						t.Fatalf("property omitted or optional semantics exempted archive dependence: %v", err)
					}
				}
			})
		}
	}
}

func TestRetainedNodePathsAdmitPackagedAndRoleDirectoryValues(t *testing.T) {
	for _, role := range []bool{false, true} {
		t.Run(fmt.Sprint(role), func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			path := filepath.Join("..", "..", "deploy", "billet-node.service")
			if role {
				path = filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host", "templates", "billet-node.service.j2")
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Project the actual directory declarations into fake loaded values.
			// The real-systemd sequence renders the entire Jinja template.
			for _, line := range strings.Split(string(body), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch key {
				case "ReadOnlyPaths", "ReadWritePaths", "RuntimeDirectory", "StateDirectory", "LogsDirectory":
					// PROJECT WHATEVER EXPRESSION NAMES THE STATE DIRECTORY, by shape
					// rather than by the variable's name, and refuse a leftover. Matched
					// on the exact old text this silently stopped replacing when 5d
					// renamed the variable, after which ReadWritePaths held the literal
					// `{{ ... }}` — non-empty, so the guard below still passed, and every
					// path comparison ran against a string that is not a path.
					value = nodeStateDirExpression.ReplaceAllString(value, "/var/lib/billet/node")
					if strings.Contains(value, "{{") {
						t.Fatalf("%s carries an expression this control cannot project: %s", key, value)
					}
					node[key] = strings.ReplaceAll(value, "\"", "")
				}
			}
			if !slices.Contains(strings.Fields(node["ReadWritePaths"]), "/run/billet/locks") ||
				node["RuntimeDirectory"] != "billet/registration" || (!role && node["StateDirectory"] != "billet/node") {
				t.Fatal("control lost the source's real directory values")
			}
			node["PrivateTmp"] = "yes"
			node["RequiresMountsFor"] = "/var/tmp"
			node["ExecStart"] = "{ path=/usr/bin/billet ; argv[]=/usr/bin/billet node --config /etc/billet/billet.yaml ; }"
			node["DeviceAllow"] = "/dev/null rw"
			p := OperationProtection{Units: []string{"billet-node.service"}, RetainedPathUnits: []string{"billet-node.service"}, ArchivedInputRoots: []string{"/var/lib/billet/server"}}
			for _, path := range []string{"/run/billet/locks", "/run/billet/locks/record", "/run/billet/registration/current"} {
				node["ReadWritePaths"] += " " + path
			}
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "enable", Unit: "billet-node.service"}, {Verb: "stop", Unit: "billet-node.service"}, {Verb: "start", Unit: "billet-node.service"}}, p); err != nil {
				t.Fatalf("shipped/role directory control refused: %v", err)
			}
			node["ReadWritePaths"] += " /run/billet/somewhere-else"
			if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
				t.Fatalf("runtime exemption escaped the node's own prefix: %v", err)
			}
		})
	}
}

// THE HOST LOCK DIRECTORY IS DISPOSABLE ONLY AS THE PACKAGE NAMES IT (#175).
// The current node unit names /run/billet/locks in ReadWritePaths and declares
// only billet/registration; a unit from before still declares the lock
// directory as a RuntimeDirectory, and both are admitted. Named as optional
// ('-') it is the same directory. Anything else under /run/billet, or the lock
// directory named only by another property, is still a volatile input.
func TestRetainedNodePathsAdmitTheLockDirectoryOnlyWhereTheUnitNamesIt(t *testing.T) {
	p := OperationProtection{Units: []string{"billet-node.service"}, RetainedPathUnits: []string{"billet-node.service"}, ArchivedInputRoots: []string{"/var/lib/billet/server"}}

	cases := []struct {
		name, runtime, readWrite, condition string
		admitted                            bool
	}{
		{name: "named in ReadWritePaths", runtime: "billet/registration", readWrite: "/run/billet/locks", admitted: true},
		{name: "named as optional", runtime: "billet/registration", readWrite: "-/run/billet/locks", admitted: true},
		{name: "a lock record beneath it", runtime: "billet/registration", readWrite: "/run/billet/locks /run/billet/locks/deployment-x.lock", admitted: true},
		{name: "a unit from before declares it", runtime: "billet/locks billet/registration", readWrite: "/run/billet/locks/deployment-x.lock", admitted: true},
		{name: "a sibling beside it", runtime: "billet/registration", readWrite: "/run/billet/locks /run/billet/locks-else", admitted: false},
		{name: "a lock record named without its directory", runtime: "billet/registration", readWrite: "/run/billet/locks/deployment-x.lock", admitted: false},
		{name: "its parent", runtime: "billet/registration", readWrite: "/run/billet/locks /run/billet", admitted: false},
		{name: "named only by a condition", runtime: "billet/registration", readWrite: "-/etc/billet/tls", condition: "/run/billet/locks/x", admitted: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			node["RuntimeDirectory"] = tc.runtime
			node["ReadWritePaths"] = tc.readWrite
			if tc.condition != "" {
				node["ConditionPathExists"] = tc.condition
			}

			err := f.inspector.AdmitOperations(t.Context(), nil, p)
			switch {
			case tc.admitted && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.admitted && (err == nil || !strings.Contains(err.Error(), "retained-input-volatile")):
				t.Fatalf("admitted, or refused for another reason: %v", err)
			}
		})
	}
}

// THE LOCK DIRECTORY IS DISPOSABLE ONLY AS ITSELF. A symlink at the directory or
// at its parent would otherwise make wherever it leads disposable, a sibling
// such as /run/billet/somewhere-else among them, so a name that resolves
// elsewhere refuses; the same directory, real, beneath the same root, is the
// control. The temporary root stands in for /run, through the two seams the
// admission reads its roots and the lock directory from.
func TestRetainedNodePathsRefuseALockDirectoryThatLeadsElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plant    func(t *testing.T, run string)
		admitted bool
	}{
		{"a real directory", func(t *testing.T, run string) {
			t.Helper()
			mustMkdirAll(t, filepath.Join(run, "billet", "locks"))
		}, true},
		{"a symlink at the directory", func(t *testing.T, run string) {
			t.Helper()
			mustMkdirAll(t, filepath.Join(run, "billet", "somewhere-else"))
			mustSymlink(t, filepath.Join(run, "billet", "somewhere-else"), filepath.Join(run, "billet", "locks"))
		}, false},
		{"a symlink at its parent", func(t *testing.T, run string) {
			t.Helper()
			mustMkdirAll(t, filepath.Join(run, "elsewhere", "locks"))
			mustSymlink(t, filepath.Join(run, "elsewhere"), filepath.Join(run, "billet"))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			run := filepath.Join(root, "run")
			tc.plant(t, run)

			f := newOperationFixture(t)
			f.inspector.retainedInputRoots = []string{run}
			f.inspector.retainedLockDir = filepath.Join(run, "billet", "locks")

			node := f.unit(t, "billet-node.service")
			node["RuntimeDirectory"] = "billet/registration"
			node["ReadWritePaths"] = f.inspector.retainedLockDir

			p := OperationProtection{Units: []string{"billet-node.service"}, RetainedPathUnits: []string{"billet-node.service"}, ArchivedInputRoots: []string{"/var/lib/billet/server"}}

			err = f.inspector.AdmitOperations(t.Context(), nil, p)
			switch {
			case tc.admitted && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.admitted && (err == nil || !strings.Contains(err.Error(), "retained-input-volatile")):
				t.Fatalf("admitted, or refused for another reason: %v", err)
			}
		})
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedNodePathsReadUnprintedPropertiesAndRereadTheSet(t *testing.T) {
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	node["Conditions"] = "[unprintable]"
	value := "/etc/billet"
	f.pathReply = func(object, property string) ([]byte, error) {
		if object != operationObjectPath("billet-node.service") || property != "Conditions" {
			return nil, fmt.Errorf("unexpected property: %s %s", object, property)
		}
		return json.Marshal(map[string]any{"type": "a(sbbsi)", "data": []any{[]any{"ConditionPathExists", true, true, value, 0}}})
	}
	p := OperationProtection{Units: []string{"billet-node.service"}, RetainedPathUnits: []string{"billet-node.service"}, ArchivedInputRoots: []string{"/var/lib/billet/server"}}
	if err := f.inspector.AdmitOperations(t.Context(), nil, p); err != nil {
		t.Fatalf("typed persistent control refused: %v", err)
	}
	value = "/var/lib/billet/server"
	if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
		t.Fatalf("unprinted condition skipped: %v", err)
	}
	f.pathReply = func(string, string) ([]byte, error) { return nil, fmt.Errorf("fixture cannot read property") }
	if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), "retained-node-path-unknown") {
		t.Fatalf("unreadable typed property treated as empty: %v", err)
	}
	delete(node, "Conditions")
	reads := 0
	f.before = func(unit string) {
		if unit == "billet-node.service" && f.calls[len(f.calls)-1] == "show --all -- billet-node.service" {
			reads++
			if reads == 2 {
				node["FutureSystemdPath"] = "/var/lib/billet/server"
			}
		}
	}
	if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") || reads != 2 {
		t.Fatalf("property set not reread after admission: reads=%d err=%v", reads, err)
	}
}

// Omitting typed-only keys from extraction would admit the archived condition.
func TestRetainedNodePathsIncludePropertiesMissingFromShow(t *testing.T) {
	for _, boundary := range []string{"operation", "stopped"} {
		t.Run(boundary, func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			node["Conditions"] = "[unprintable]"
			value := "/etc/billet"
			f.pathReply = func(string, string) ([]byte, error) {
				return json.Marshal(map[string]any{"type": "a(sbbsi)", "data": []any{[]any{"ConditionPathExists", false, false, value, 0}}})
			}
			run := f.inspector.run
			f.inspector.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
				out, err := run(ctx, bin, args)
				if args[0] == "show" {
					out = []byte(strings.ReplaceAll(string(out), "Conditions=[unprintable]\n", ""))
				}
				return out, err
			}
			admit := func() error {
				if boundary == "stopped" {
					return f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", "/var/lib/billet/server")
				}
				return f.inspector.AdmitOperations(t.Context(), nil, OperationProtection{
					Units:              []string{"billet-node.service"},
					RetainedPathUnits:  []string{"billet-node.service"},
					ArchivedInputRoots: []string{"/var/lib/billet/server"},
				})
			}
			if err := admit(); err != nil {
				t.Fatalf("typed-only persistent condition refused: %v", err)
			}
			value = "/var/lib/billet/server"
			if err := admit(); err == nil || !strings.Contains(err.Error(), "billet-node.service Conditions: retained-node-path-archived") {
				t.Fatalf("typed-only archived condition admitted: %v", err)
			}
		})
	}
}

func TestRetainedNodePathsProtectTraversedEntries(t *testing.T) {
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	root := t.TempDir()
	archive, persistent := filepath.Join(root, "server"), filepath.Join(root, "etc")
	for _, dir := range []string{archive, persistent} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bridge, entry := filepath.Join(archive, "bridge"), filepath.Join(root, "entry")
	for link, target := range map[string]string{bridge: persistent, entry: bridge} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	WithRetainedInputRoots(filepath.Join(root, "volatile"))(f.inspector)
	node["ReadWritePaths"] = entry
	if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", archive); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
		t.Fatalf("persistent leaf concealed archive traversal: %v", err)
	}
	WithRetainedInputRoots(archive)(f.inspector)
	if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service"); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
		t.Fatalf("persistent leaf concealed volatile traversal: %v", err)
	}
	WithRetainedInputRoots(filepath.Join(root, "volatile"))(f.inspector)
	quoted := filepath.Join(root, "quoted route")
	if err := os.Symlink(bridge, quoted); err != nil {
		t.Fatal(err)
	}
	node["ReadWritePaths"] = persistent
	node["Conditions"] = "[unprintable]"
	f.pathReply = func(string, string) ([]byte, error) {
		return json.Marshal(map[string]any{"type": "a(sbbsi)", "data": []any{[]any{"ConditionPathExists", false, false, quoted, 0}}})
	}
	if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", archive); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
		t.Fatalf("splitting a typed pathname hid its traversed symlink: %v", err)
	}
}

func TestRetainedNodePathsKeepPrivateTmpExceptionExact(t *testing.T) {
	for _, c := range []struct{ property, path, private string }{
		{"RequiresMountsFor", "/var/tmp/retained", "yes"},
		{"RequiresMountsFor", "/var/tmp", "no"},
		{"ReadWritePaths", "/var/tmp", "yes"},
		{"ExecStart", "/var/tmp/runner", "yes"},
		{"ReadWritePaths", "/run/systemd/journal/stdout", "yes"},
	} {
		t.Run(c.property+"/"+c.path+"/"+c.private, func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			node["PrivateTmp"], node["RequiresMountsFor"] = "yes", "/var/tmp"
			if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service"); err != nil {
				t.Fatalf("private-tmp mount prerequisite refused: %v", err)
			}
			node["PrivateTmp"], node[c.property] = c.private, c.path
			if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service"); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
				t.Fatalf("private-tmp exception escaped its property/path pair: %v", err)
			}
		})
	}
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	node["PrivateTmp"], node["RequiresMountsFor"] = "yes", "/var/tmp"
	if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", "/var/tmp"); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
		t.Fatalf("private-tmp exception bypassed the archive boundary: %v", err)
	}
}

func TestRetainedNodePathsExpandPersistentDirectoryProperties(t *testing.T) {
	for _, property := range []string{"StateDirectory", "LogsDirectory"} {
		t.Run(property, func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			root := "/var/lib"
			if property == "LogsDirectory" {
				root = "/var/log"
			}
			node[property] = "billet/node"
			if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", root+"/billet/server"); err != nil {
				t.Fatalf("persistent directory refused: %v", err)
			}
			node[property] = "billet/server"
			if err := f.inspector.AdmitRetainedUnitPaths(t.Context(), "billet-node.service", root+"/billet/server"); err == nil || !strings.Contains(err.Error(), "retained-node-path-archived") {
				t.Fatalf("relative directory hid archive dependence: %v", err)
			}
		})
	}
}
