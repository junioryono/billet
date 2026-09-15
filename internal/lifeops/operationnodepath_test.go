package lifeops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
					value = strings.ReplaceAll(value, "{{ billet_config.get('node', {}).get('state_dir', '/var/lib/billet/node') }}", "/var/lib/billet/node")
					node[key] = strings.ReplaceAll(value, "\"", "")
				}
			}
			if node["ReadWritePaths"] == "" || node["RuntimeDirectory"] != "billet/locks billet/registration" || (!role && node["StateDirectory"] != "billet/node") {
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
		if unit == "billet-node.service" && operationCallRequests(f.calls[len(f.calls)-1], "*") {
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
