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

type operationFixture struct {
	inspector *Inspector
	units     map[string]map[string]string
	root      string
	calls     []string
	before    func(string)
	busReply  func(string, string, string) string
}

func newOperationFixture(t *testing.T) *operationFixture {
	t.Helper()
	f := &operationFixture{units: make(map[string]map[string]string), root: t.TempDir()}
	f.inspector = NewInspector(WithOperationUnitDirectories(f.root), withRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if args[0] == "get-property" {
			for unit, props := range f.units {
				if operationObjectPath(unit) != args[2] {
					continue
				}
				kind := map[string]string{".service": "Service", ".socket": "Socket", ".mount": "Mount", ".swap": "Swap"}[filepath.Ext(unit)]
				if args[3] != "org.freedesktop.systemd1."+kind {
					return nil, fmt.Errorf("wrong execution interface for %s: %s", unit, args[3])
				}
				var out strings.Builder
				for _, name := range args[4:] {
					signature := fixtureOperationSignature(name)
					value, ok := props[name]
					if signature == "" || !ok {
						return nil, fmt.Errorf("fixture has no typed evidence for %s %s", unit, name)
					}
					line := signature + " 0"
					if value != "" {
						line = signature + " 1 " + value
					}
					if f.busReply != nil {
						line = f.busReply(unit, name, line)
					}
					fmt.Fprintln(&out, line)
				}
				return []byte(out.String()), nil
			}
			return nil, fmt.Errorf("fixture has no bus object %s", args[2])
		}
		if args[0] != "show" {
			t.Fatalf("admission executed an operation as a probe: %v", args)
		}
		unit := args[len(args)-1]
		if f.before != nil {
			f.before(unit)
		}
		props, ok := f.units[unit]
		if !ok {
			return nil, fmt.Errorf("fixture has no evidence for %s", unit)
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
	}))
	return f
}

// Independent of the production vtable: omitting or mistyping a query must
// change the fixture's answer, not its definition of the expected API.
func fixtureOperationSignature(name string) string {
	switch name {
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
	case "ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost":
		return "a(sasbttttuii)"
	default:
		return ""
	}
}

func (f *operationFixture) unit(t *testing.T, name string) map[string]string {
	t.Helper()
	path := filepath.Join(f.root, name)
	body := "[Unit]\nDescription=operation fixture\n"
	if strings.HasSuffix(name, ".timer") {
		body += "[Install]\nWantedBy=timers.target\n"
	} else if !strings.HasSuffix(name, "-backup.service") && !strings.HasSuffix(name, "-upgrade.service") {
		body += "[Install]\nWantedBy=multi-user.target\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := map[string]string{
		"Id": name, "Names": name, "LoadState": "loaded", "ActiveState": "inactive", "UnitFileState": "disabled",
		"FragmentPath": path, "SourcePath": "", "DropInPaths": "", "NeedDaemonReload": "no",
		"OnSuccessJobMode": "fail", "OnFailureJobMode": "replace", "FailureAction": "none", "SuccessAction": "none",
		"StartLimitAction": "none", "JobTimeoutAction": "none", "RequiresMountsFor": "", "Where": "/ledger", "What": "/dev/vdb1", "Type": "ext4",
		"StandardInput": "null", "StandardOutput": "journal", "StandardError": "inherit",
		"Transient": "no", "Job": "", "KillMode": "control-group", "DynamicUser": "no", "RuntimeDirectoryPreserve": "no", "StopWhenUnneeded": "no",
	}
	// Independent of the production query lists, so deleting a queried property
	// cannot delete that evidence from the fixture at the same time.
	for _, property := range strings.Fields(
		"Before After PropagatesReloadTo ReloadPropagatedFrom SliceOf Following PIDFile PAMName LogNamespace NetworkNamespacePath IPCNamespacePath UtmpIdentifier Requires Requisite Wants BindsTo Upholds PartOf RequiredBy RequisiteOf WantedBy BoundBy UpheldBy ConsistsOf " +
			"Conflicts ConflictedBy OnSuccess OnFailure OnSuccessOf OnFailureOf Triggers TriggeredBy PropagatesStopTo " +
			"StopPropagatedFrom JoinsNamespaceOf StateDirectory RuntimeDirectory CacheDirectory LogsDirectory " +
			"ConfigurationDirectory StateDirectorySymlink RuntimeDirectorySymlink CacheDirectorySymlink LogsDirectorySymlink " +
			"RootDirectory RootImage BindPaths BindReadOnlyPaths TemporaryFileSystem MountImages ExtensionImages ExtensionDirectories " +
			"ExecCondition ExecStartPre ExecStartPost ExecStop ExecStopPost") {
		p[property] = ""
	}
	f.units[name] = p
	return p
}

func TestOperationAdmissionClosesForwardReverseAndTransitiveEffects(t *testing.T) {
	for _, c := range []struct{ verb, property, placement string }{
		{"stop", "OnSuccess", "direct"}, {"stop", "OnFailure", "transitive"},
		{"stop", "PropagatesStopTo", "direct"}, {"stop", "RequiredBy", "transitive"},
		{"stop", "BoundBy", "transitive"}, {"stop", "ConsistsOf", "transitive"},
		{"stop", "PartOf", "reverse"}, {"stop", "BindsTo", "reverse"},
		{"stop", "StopPropagatedFrom", "reverse"},
		{"stop", "OnSuccessOf", "reverse"}, {"stop", "OnFailureOf", "reverse"},
		{"start", "Requires", "transitive"}, {"start", "Wants", "transitive"},
		{"start", "BindsTo", "transitive"}, {"start", "Upholds", "transitive"},
		{"start", "Conflicts", "transitive"}, {"start", "ConflictedBy", "transitive"},
		{"start", "Triggers", "transitive"}, {"start", "JoinsNamespaceOf", "transitive"},
	} {
		t.Run(c.verb+"/"+c.property+"/"+c.placement, func(t *testing.T) {
			f := newOperationFixture(t)
			target := f.unit(t, "billet-server.service")
			node := f.unit(t, "billet-node.service")
			helper := f.unit(t, "helper.service")
			protection := OperationProtection{Units: []string{"billet-server.service", "billet-node.service"}}
			op := Operation{Verb: c.verb, Unit: "billet-server.service"}
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection); err != nil {
				t.Fatalf("clean supported control: %v", err)
			}
			switch c.placement {
			case "direct":
				target[c.property] = "billet-node.service"
			case "reverse":
				node[c.property] = "billet-server.service"
			case "transitive":
				entry := "Requires"
				if c.verb == "stop" {
					entry = "PropagatesStopTo"
				}
				target[entry] = "helper.service"
				helper[c.property] = "billet-node.service"
			}
			err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection)
			want, name := "operation-edge-outside-set", "billet-node.service"
			if c.placement == "transitive" {
				want, name = "operation-edge-outside-set", "helper.service"
			}
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), name) {
				t.Fatalf("unsafe %s path admitted: %v", c.property, err)
			}
		})
	}
}

func TestOperationAdmissionQueriesEveryDirectoryAndRejectsSharedOwnership(t *testing.T) {
	for _, c := range []struct{ directive, root string }{
		{"StateDirectory", "/var/lib"}, {"RuntimeDirectory", "/run"}, {"CacheDirectory", "/var/cache"},
		{"LogsDirectory", "/var/log"}, {"ConfigurationDirectory", "/etc"},
	} {
		t.Run(c.directive, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			f.unit(t, "billet-node.service")
			protection := OperationProtection{Units: []string{"billet-server.service", "billet-node.service"},
				UnitPaths: map[string][]string{"billet-node.service": {filepath.Join(c.root, "billet/shared/registration")}}}
			op := Operation{Verb: "stop", Unit: "billet-server.service"}
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection); err != nil {
				t.Fatalf("clean control: %v", err)
			}
			p[c.directive] = "billet/shared"
			err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection)
			if err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") || !strings.Contains(err.Error(), c.directive) {
				t.Fatalf("directory overlap admitted: %v", err)
			}
			delete(p, c.directive)
			err = f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection)
			if err == nil || !strings.Contains(err.Error(), "operation-property-unknown") {
				t.Fatalf("missing directory evidence admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionSeparatesInstallationFromRuntimeProperties(t *testing.T) {
	for _, verb := range []string{"enable", "disable"} {
		for _, clause := range []string{"Also=helper.service", "Alias=billet-server.service", "WantedBy=billet-server.service", "UnknownSetting=yes"} {
			t.Run(verb+"/"+clause, func(t *testing.T) {
				f := newOperationFixture(t)
				p := f.unit(t, "billet-node.service")
				protection := OperationProtection{Units: []string{"billet-node.service"}}
				op := Operation{Verb: verb, Unit: "billet-node.service"}
				if err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection); err != nil {
					t.Fatalf("clean installation: %v", err)
				}
				if err := os.WriteFile(p["FragmentPath"], []byte("[Install]\n"+clause+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, protection)
				if err == nil || !(strings.Contains(err.Error(), "operation-install-") || strings.Contains(err.Error(), "operation-edge-outside-set")) {
					t.Fatalf("installation effect admitted: %v", err)
				}
				for _, call := range f.calls {
					if strings.Contains(call, "--property=Also") {
						t.Fatal("asked for a property systemd 255 does not expose")
					}
				}
			})
		}
	}
}

func TestOperationAdmissionRefusesAliasesInstancesAndMountStops(t *testing.T) {
	for _, c := range []struct{ name, helper, property, value string }{
		{"alias", "helper.service", "Names", "helper.service billet-node.service"},
		{"instance", "billet-node@jobs.service", "Names", "billet-node@jobs.service"},
		{"mount", "scratch.mount", "ActiveState", "active"},
		{"automount", "ledger.automount", "ActiveState", "active"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			f.unit(t, "billet-node.service")
			helper := f.unit(t, c.helper)
			helper[c.property] = c.value
			p["PropagatesStopTo"] = c.helper
			err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}},
				OperationProtection{Units: []string{"billet-server.service", "billet-node.service"}, Paths: []string{"/ledger/identity"}})
			if err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
				t.Fatalf("protected %s admitted: %v", c.name, err)
			}
		})
	}
}

func TestOperationAdmissionRefusesUnreadableDriftingAndOutsideEvidence(t *testing.T) {
	for _, problem := range []string{"missing property", "empty names", "unreadable source", "source drift", "reload", "job mode", "outside set"} {
		t.Run(problem, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			want := "operation-"
			switch problem {
			case "empty names":
				p["Names"] = ""
				want = "operation-names-unknown"
			case "missing property":
				delete(p, "PropagatesStopTo")
				want = "operation-property-unknown"
			case "unreadable source":
				p["FragmentPath"] = filepath.Join(f.root, "missing")
				want = "operation-source-unreadable"
			case "source drift":
				reads := 0
				f.before = func(_ string) {
					reads++
					if reads == 3 {
						if err := os.WriteFile(p["FragmentPath"], []byte("[Unit]\nDescription=changed\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				want = "operation-evidence-changed"
			case "reload":
				p["NeedDaemonReload"] = "yes"
				want = "operation-source-unsupported"
			case "job mode":
				p["OnSuccessJobMode"], p["OnSuccess"] = "isolate", "helper.service"
				want = "operation-edge-outside-set"
			case "outside set":
				p["PropagatesStopTo"] = "helper.service"
				want = "operation-edge-outside-set"
			}
			err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s admitted or wrong diagnostic: %v", problem, err)
			}
		})
	}
}

func TestOperationAdmissionRefusesInstallationLinks(t *testing.T) {
	f := newOperationFixture(t)
	p := f.unit(t, "billet-node.service")
	link := filepath.Join(f.root, "billet-server.service")
	if err := os.Symlink(p["FragmentPath"], link); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"enable", "disable"} {
		err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: verb, Unit: "billet-node.service"}}, OperationProtection{})
		if err == nil || !strings.Contains(err.Error(), "operation-install-alias") {
			t.Fatalf("installation alias admitted: %v", err)
		}
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, verb := range []string{"enable", "disable"} {
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: verb, Unit: "billet-node.service"}}, OperationProtection{}); err != nil {
			t.Fatalf("clean installation rejected: %v", err)
		}
	}
	if !slices.ContainsFunc(f.calls, func(call string) bool { return strings.Contains(call, "--property=FragmentPath") }) {
		t.Fatal("installation never queried the effective source")
	}
}

func TestOperationAdmissionKeepsSupportedInfrastructureAndDirectoryLists(t *testing.T) {
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	f.unit(t, "billet-server.service")
	network := f.unit(t, "network-online.target")
	root := f.unit(t, "-.mount")
	shutdown := f.unit(t, "shutdown.target")
	node["Requires"], node["Wants"], node["Conflicts"] = "-.mount", "network-online.target", "shutdown.target"
	// The distribution chain is present with DefaultDependencies enabled:
	// v255 units/sysinit.target and units/local-fs.target (source, 2026-09-15).
	sysinit := f.unit(t, "sysinit.target")
	localFS := f.unit(t, "local-fs.target")
	f.unit(t, "emergency.target")
	f.unit(t, "emergency.service")
	swap := f.unit(t, "swap.target")
	node["Requires"] += " sysinit.target"
	node["After"] = "sysinit.target"
	sysinit["Wants"], sysinit["Conflicts"] = "local-fs.target swap.target systemd-firstboot.service", "emergency.service emergency.target"
	sysinit["ActiveState"], localFS["ActiveState"], swap["ActiveState"] = "active", "active", "active"
	firstboot := f.unit(t, "systemd-firstboot.service")
	firstboot["StandardInput"], firstboot["StandardOutput"], firstboot["StandardError"] = "tty", "tty", "tty"
	firstboot["ImportCredential"] = "firstboot.*"
	localFS["OnFailure"], localFS["OnFailureJobMode"] = "emergency.target", "replace-irreversibly"
	localFS["Conflicts"] = "shutdown.target"
	node["RuntimeDirectory"] = "billet/locks billet/registration"
	node["RequiresMountsFor"] = "/run/billet/locks /run/billet/registration"
	network["ActiveState"], root["ActiveState"], shutdown["ActiveState"] = "active", "active", "inactive"
	root["FragmentPath"], root["Where"] = "", "/"
	protection := OperationProtection{Units: []string{"billet-node.service", "billet-server.service"},
		UnitPaths: map[string][]string{"billet-node.service": {"/run/billet/locks", "/run/billet/registration"}}}
	for _, verb := range []string{"enable", "stop", "start", "disable"} {
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: verb, Unit: "billet-node.service"}}, protection); err != nil {
			t.Fatalf("supported infrastructure %s: %v", verb, err)
		}
	}
	// A standard no-op ends traversal even with arbitrary distribution children.
	for _, call := range f.calls {
		if strings.HasSuffix(call, " systemd-firstboot.service") {
			t.Fatal("traversed beyond the standard sysinit target")
		}
	}
}

func TestOperationAdmissionRejectsActivationAndUnneededStopSources(t *testing.T) {
	for _, relation := range []string{"TriggeredBy", "UpheldBy", "StopWhenUnneeded", "RequiresMountsFor"} {
		t.Run(relation, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			node := f.unit(t, "billet-node.service")
			trigger := f.unit(t, "activation.timer")
			trigger["ActiveState"] = "active"
			op := Operation{Verb: "stop", Unit: "billet-server.service"}
			want := "operation-edge-outside-set"
			switch relation {
			case "StopWhenUnneeded":
				network := f.unit(t, "network-online.target")
				network["ActiveState"], network["StopWhenUnneeded"] = "active", "yes"
				server["Wants"] = "network-online.target"
				want = "operation-standard-effect"
			case "RequiresMountsFor":
				op = Operation{Verb: "start", Unit: "billet-node.service"}
				node[relation] = "/var/lib/billet/server"
				want = "operation-protected-mount-requirement"
			default:
				server[relation] = "activation.timer"
			}
			err := f.inspector.AdmitOperations(t.Context(), []Operation{op}, OperationProtection{
				Units:     []string{"billet-server.service", "billet-node.service"},
				UnitPaths: map[string][]string{"billet-server.service": {"/var/lib/billet/server"}},
			})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s effect admitted: %v", relation, err)
			}
		})
	}
}

// Typed empty arrays must remain distinct from missing command evidence.
func TestOperationAdmissionRequiresTypedCommandAndMappingEvidence(t *testing.T) {
	for _, property := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		t.Run(property, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
				t.Fatalf("typed empty control: %v", err)
			}
			p[property] = `"/usr/bin/true" 1 "/usr/bin/true" false 0 0 0 0 0 0 0`
			if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-command-effects-unsupported") {
				t.Fatalf("configured %s admitted: %v", property, err)
			}
			delete(p, property)
			if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-array-unreadable") {
				t.Fatalf("missing %s admitted: %v", property, err)
			}
		})
	}
	for _, reply := range []string{"", "as 0", "a(sst) invalid", "a(sst) 0 extra", "a(sst) 1"} {
		t.Run("malformed/"+reply, func(t *testing.T) {
			f := newOperationFixture(t)
			f.unit(t, "billet-node.service")
			f.busReply = func(_, property, line string) string {
				if property == "RuntimeDirectorySymlink" {
					return reply
				}
				return line
			}
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "start", Unit: "billet-node.service"}}, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-array-unknown") {
				t.Fatalf("unreadable typed mapping admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionBindsInstallationToCurrentSources(t *testing.T) {
	f := newOperationFixture(t)
	p := f.unit(t, "billet-server.service")
	// A role ledger mount uses systemd's escaped unit-name spelling. Its
	// backslash is outside Install and must not reject the plain install data.
	body := "[Unit]\nRequires=run-ledger\\x2dvolume.mount\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(p["FragmentPath"], []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sequence := []Operation{{Verb: "disable", Unit: "billet-server.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
		t.Fatalf("role mount spelling: %v", err)
	}
	dropin := filepath.Join(f.root, "retirement.conf")
	if err := os.WriteFile(dropin, []byte("[Install]\nAlso=billet-node.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p["DropInPaths"] = dropin
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
		t.Fatalf("effective installation drop-in was ignored: %v", err)
	}
	p["DropInPaths"] = ""
	vendor := filepath.Join(t.TempDir(), "billet-server.service")
	if err := os.WriteFile(vendor, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p["FragmentPath"] = vendor
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-install-source-inconsistent") {
		t.Fatalf("manager and installation source disagreement was admitted: %v", err)
	}
}

// The same destination is safe as a boot dependency and unsafe as a new
// completion transaction, regardless of the handler's job mode.
func TestOperationAdmissionRejectsCompletionAnchorsInEveryJobMode(t *testing.T) {
	for _, relation := range []string{"OnSuccess", "OnFailure"} {
		for _, mode := range []string{"fail", "replace", "isolate", "flush", "ignore-dependencies"} {
			t.Run(relation+"/"+mode, func(t *testing.T) {
				f := newOperationFixture(t)
				server := f.unit(t, "billet-server.service")
				f.unit(t, "multi-user.target")["ActiveState"] = "active"
				server["UnitFileState"], server["WantedBy"] = "enabled", "multi-user.target"
				server[relation+"JobMode"] = mode
				sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
					t.Fatalf("enabled clean controller: %v", err)
				}
				server[relation] = "multi-user.target"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: billet-server.service "+relation+"=multi-user.target") {
					t.Fatalf("completion anchor admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionResolvesProtectedAliasesAndRevalidates(t *testing.T) {
	f := newOperationFixture(t)
	server := f.unit(t, "billet-server.service")
	alias := filepath.Join(t.TempDir(), "node-state")
	if err := os.Symlink("/run/shared", alias); err != nil {
		t.Fatal(err)
	}
	server["RuntimeDirectory"] = "shared"
	protection := OperationProtection{UnitPaths: map[string][]string{"billet-node.service": {alias}}}
	sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") {
		t.Fatalf("protected symlink bypassed overlap: %v", err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run/unrelated", alias); err != nil {
		t.Fatal(err)
	}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
		t.Fatalf("unrelated resolved path: %v", err)
	}
	reads := 0
	f.before = func(_ string) {
		reads++
		if reads == 3 {
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/run/shared", alias); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-path-changed") {
		t.Fatalf("path binding was not revalidated: %v", err)
	}
}

func TestOperationAdmissionAlsoAccumulatesAcrossEmptyAssignmentsAndDropins(t *testing.T) {
	for _, direction := range []struct{ verb, target, collateral string }{
		{"enable", "billet-node.service", "billet-server.service"},
		{"disable", "billet-server.service", "billet-node.service"},
	} {
		for _, placement := range []string{"repeated", "empty", "drop-in", "empty drop-in"} {
			t.Run(direction.verb+"/"+placement, func(t *testing.T) {
				f := newOperationFixture(t)
				p := f.unit(t, direction.target)
				f.unit(t, direction.collateral)
				sequence := []Operation{{Verb: direction.verb, Unit: direction.target}}
				protection := OperationProtection{Units: []string{direction.target, direction.collateral}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("clean installation: %v", err)
				}
				body := "[Install]\nAlso=" + direction.collateral + "\n"
				switch placement {
				case "repeated":
					body += "Also=" + direction.target + "\n"
				case "empty":
					body += "Also=\n"
				case "drop-in", "empty drop-in":
					dropin := filepath.Join(f.root, "extra.conf")
					extra := "[Install]\nAlso=\n"
					if placement == "drop-in" {
						extra = body + "Also=\n"
						body = "[Install]\nWantedBy=multi-user.target\n"
					}
					if err := os.WriteFile(dropin, []byte(extra), 0o644); err != nil {
						t.Fatal(err)
					}
					p["DropInPaths"] = dropin
				}
				if err := os.WriteFile(p["FragmentPath"], []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), direction.collateral) {
					t.Fatalf("Also installation effect was erased: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionKeepsRequiredNetworkActive(t *testing.T) {
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	server := f.unit(t, "billet-server.service")
	network := f.unit(t, "billet-network.service")
	f.unit(t, "billet-dnsmasq@br0.service")
	network["ActiveState"] = "active"
	node["Requires"] = "billet-network.service"
	protection := OperationProtection{
		Units:          []string{"billet-node.service", "billet-server.service", "billet-network.service", "billet-dnsmasq@br0.service"},
		RequiredActive: []string{"billet-network.service"},
	}
	start := []Operation{{Verb: "start", Unit: "billet-node.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), start, protection); err != nil {
		t.Fatalf("idempotent network dependency start: %v", err)
	}
	network["ActiveState"] = "inactive"
	if err := f.inspector.AdmitOperations(t.Context(), start, protection); err == nil || !strings.Contains(err.Error(), "operation-protected-effect") {
		t.Fatalf("inactive network could be started under retained authority: %v", err)
	}
	network["ActiveState"] = "active"
	for _, unit := range []string{"billet-network.service", "billet-dnsmasq@br0.service"} {
		server["PropagatesStopTo"] = unit
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
			t.Fatalf("guest network stop admitted: %s %v", unit, err)
		}
	}
}

// Removing any relationship from admission must admit that matrix row.
func TestOperationAdmissionClosesEveryRelationship(t *testing.T) {
	for _, relation := range strings.Fields("Requires Requisite Wants BindsTo Upholds PartOf RequiredBy RequisiteOf WantedBy BoundBy UpheldBy ConsistsOf Conflicts ConflictedBy OnSuccess OnFailure OnSuccessOf OnFailureOf Triggers TriggeredBy PropagatesStopTo StopPropagatedFrom JoinsNamespaceOf Before After PropagatesReloadTo ReloadPropagatedFrom SliceOf Following") {
		for _, verb := range []string{"stop", "start", "enable", "disable"} {
			t.Run(verb+"/"+relation, func(t *testing.T) {
				f := newOperationFixture(t)
				server := f.unit(t, "billet-server.service")
				sequence := []Operation{{Verb: verb, Unit: "billet-server.service"}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
					t.Fatalf("clean control: %v", err)
				}
				server[relation] = "external.service"
				// No evidence exists for the helper: refusal must precede a read.
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: billet-server.service "+relation+"=external.service") {
					t.Fatalf("outside unit admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionStandardUnitsAreOnlyNoops(t *testing.T) {
	for _, c := range []struct {
		relation, target, state, verb, refusal string
	}{
		{"Requires", "sysinit.target", "active", "start", ""},
		{"Requires", "sysinit.target", "inactive", "start", "operation-standard-effect"},
		{"After", "sysinit.target", "inactive", "start", ""},
		{"Before", "shutdown.target", "failed", "stop", ""},
		{"Conflicts", "shutdown.target", "inactive", "stop", ""},
		{"Conflicts", "shutdown.target", "inactive", "start", ""},
		{"Conflicts", "shutdown.target", "active", "start", "operation-standard-effect"},
		{"Requires", "shutdown.target", "active", "start", "operation-edge-outside-set"},
		{"PropagatesStopTo", "shutdown.target", "inactive", "stop", "operation-edge-outside-set"},
		{"PropagatesStopTo", "shutdown.target", "active", "stop", "operation-edge-outside-set"},
	} {
		t.Run(c.relation+"/"+c.target+"/"+c.state+"/"+c.verb, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			standard := f.unit(t, c.target)
			standard["ActiveState"] = c.state
			server[c.relation] = c.target
			err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: c.verb, Unit: "billet-server.service"}}, OperationProtection{})
			if c.refusal == "" && err != nil {
				t.Fatalf("no-op refused: %v", err)
			}
			if c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)) {
				t.Fatalf("standard transition admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionOwnUnitsKeepSetupChecksAtStop(t *testing.T) {
	for _, unit := range []string{"billet-server.service", "billet-backup.service", "billet-upgrade.service"} {
		for _, property := range []string{"StandardInput", "StandardOutput", "StandardError", "PIDFile", "PAMName", "RootDirectory", "IPCNamespacePath"} {
			t.Run(unit+"/"+property, func(t *testing.T) {
				f := newOperationFixture(t)
				p := f.unit(t, unit)
				sequence := []Operation{{Verb: "stop", Unit: unit}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
					t.Fatalf("clean stop: %v", err)
				}
				p[property] = "truncate"
				p["ExecStop"] = `"/usr/bin/true" 1 "/usr/bin/true" false 0 0 0 0 0 0 0`
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-setup-unsupported: "+unit+" "+property) {
					t.Fatalf("stop setup admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionOwnManagerActions(t *testing.T) {
	for _, property := range []string{"FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction"} {
		f := newOperationFixture(t)
		server := f.unit(t, "billet-server.service")
		server[property] = "reboot"
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-manager-action") {
			t.Fatalf("%s action admitted: %v", property, err)
		}
	}
}

func TestOperationAdmissionJudgesLoadedAliasesAsOneUnit(t *testing.T) {
	f := newOperationFixture(t)
	server := f.unit(t, "billet-server.service")
	server["Names"] += " controller.service"
	f.units["controller.service"] = server
	if err := os.WriteFile(server["FragmentPath"], []byte("[Install]\nAlias=controller.service\nWantedBy=multi-user.target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"stop", "start", "enable", "disable"} {
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: verb, Unit: "billet-server.service"}}, OperationProtection{}); err != nil {
			t.Fatalf("loaded alias refused: %v", err)
		}
	}
	server["After"] = "outside.service"
	if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "controller.service"}}, OperationProtection{Units: []string{"billet-server.service"}}); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
		t.Fatalf("alias bypassed relationship judgment: %v", err)
	}
}

func TestOperationAdmissionProtectsDedicatedLedgerMount(t *testing.T) {
	f := newOperationFixture(t)
	server := f.unit(t, "billet-server.service")
	mount := f.unit(t, "ledger.mount")
	mount["Where"], mount["What"], mount["ActiveState"] = "/ledger", "/dev/vdb1", "active"
	mount["Requires"] = "dev-vdb1.device -.mount"
	mount["StopPropagatedFrom"] = "dev-vdb1.device"
	mount["After"] = "dev-vdb1.device blockdev@dev-vdb1.target local-fs-pre.target -.mount dev.mount system.slice systemd-journald.socket"
	mount["Before"], mount["Conflicts"] = "local-fs.target umount.target", "umount.target"
	mount["RequiresMountsFor"] = "/ /dev/vdb1"
	for _, name := range []string{"dev-vdb1.device", "-.mount", "dev.mount"} {
		f.unit(t, name)["ActiveState"] = "active"
	}
	server["RequiresMountsFor"], server["Requires"] = "/ledger", "ledger.mount"
	protection := OperationProtection{Units: []string{"billet-server.service"}, UnitPaths: map[string][]string{"billet-server.service": {"/ledger"}}}
	sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
		t.Fatalf("clean dedicated mount: %v", err)
	}
	mount["After"] = "outside.service"
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: ledger.mount After=outside.service") {
		t.Fatalf("ledger mount was treated as a standard leaf: %v", err)
	}
}

func TestOperationAdmissionRechecksOwnUnitsWithoutServiceOperations(t *testing.T) {
	f := newOperationFixture(t)
	backup := f.unit(t, "billet-backup.service")
	protection := OperationProtection{Units: []string{"billet-backup.service"}, Paths: []string{"/run/billet/registration/current"}}
	if err := f.inspector.AdmitOperations(t.Context(), nil, protection); err != nil {
		t.Fatalf("clean filesystem boundary: %v", err)
	}
	backup["RuntimeDirectory"] = "billet/registration"
	if err := f.inspector.AdmitOperations(t.Context(), nil, protection); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") {
		t.Fatalf("backup completion could remove registration: %v", err)
	}
	backup["RuntimeDirectory"], backup["OnSuccessOf"] = "", "outside.service"
	if err := f.inspector.AdmitOperations(t.Context(), nil, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
		t.Fatalf("empty operation sequence skipped closed set: %v", err)
	}
}

func TestOperationAdmissionRefusesExcessiveOwnUnitEvidence(t *testing.T) {
	f := newOperationFixture(t)
	protection := OperationProtection{}
	for n := range operationUnitLimit + 1 {
		unit := fmt.Sprintf("billet-dnsmasq@br%d.service", n)
		f.unit(t, unit)
		protection.Units = append(protection.Units, unit)
	}
	if err := f.inspector.AdmitOperations(t.Context(), nil, protection); err == nil || !strings.Contains(err.Error(), "operation-traversal-bound") {
		t.Fatalf("unit evidence bound ignored: %v", err)
	}
}

// Each row must fail on its edge, even when both endpoint names are protected.
func TestOperationAdmissionClosesEdgesBetweenOwnUnits(t *testing.T) {
	for _, relation := range strings.Fields("OnSuccess OnFailure Upholds PropagatesStopTo StopPropagatedFrom JoinsNamespaceOf Triggers OnSuccessOf OnFailureOf UpheldBy PartOf ConsistsOf Requisite RequisiteOf PropagatesReloadTo ReloadPropagatedFrom SliceOf Following") {
		t.Run(relation, func(t *testing.T) {
			f := newOperationFixture(t)
			backup := f.unit(t, "billet-backup.service")
			f.unit(t, "billet-server.service")
			protection := OperationProtection{Units: []string{"billet-server.service", "billet-backup.service"}, WaitingUnits: []string{"billet-backup.service"}}
			backup["ActiveState"], backup["Job"] = "activating", "42"
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("in-flight backup control: %v", err)
			}
			backup[relation] = "billet-server.service"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: billet-backup.service "+relation+"=billet-server.service") {
				t.Fatalf("in-flight backup edge admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionPairsEachTimerWithItsOwnService(t *testing.T) {
	for _, timerName := range []string{"billet-backup.timer", "billet-upgrade.timer"} {
		for _, inverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inverse=%v", timerName, inverse), func(t *testing.T) {
				f := newOperationFixture(t)
				timer := f.unit(t, timerName)
				serviceName := strings.TrimSuffix(timerName, ".timer") + ".service"
				service := f.unit(t, serviceName)
				otherName := "billet-server.service"
				other := f.unit(t, otherName)
				protection := OperationProtection{Units: []string{timerName, serviceName, otherName}}
				sequence := []Operation{{Verb: "stop", Unit: timerName}}
				if inverse {
					service["TriggeredBy"] = timerName
				} else {
					timer["Triggers"] = serviceName
				}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("own timer control: %v", err)
				}
				if inverse {
					other["TriggeredBy"] = timerName
				} else {
					timer["Triggers"] = otherName
				}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
					t.Fatalf("crossed timer pair admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionProtectsImplicitCredentialTeardown(t *testing.T) {
	for _, canonical := range []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service"} {
		for _, alias := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alias=%v", canonical, alias), func(t *testing.T) {
				f := newOperationFixture(t)
				unit := f.unit(t, canonical)
				target := canonical
				if alias {
					target = "alias.service"
					unit["Names"] += " " + target
					f.units[target] = unit
				}
				sequence := []Operation{{Verb: "stop", Unit: target}}
				protection := OperationProtection{Units: []string{canonical}, Paths: []string{"/run/credentials/unrelated.service/node.crt"}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("unrelated credential path: %v", err)
				}
				protection.Paths = []string{filepath.Join("/run/credentials", canonical, "node.crt")}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") || !strings.Contains(err.Error(), "CredentialDirectory=/run/credentials/"+canonical) {
					t.Fatalf("implicit canonical credential teardown admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionKeepsSeparateVarAnActiveLeaf(t *testing.T) {
	for _, verb := range []string{"stop", "start", "disable"} {
		t.Run(verb, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			server["RequiresMountsFor"] = "/var/lib/billet/server"
			server["Requires"], server["After"] = "var.mount -.mount", "var.mount -.mount"
			root := f.unit(t, "-.mount")
			root["ActiveState"] = "active"
			mount := f.unit(t, "var.mount")
			mount["Where"], mount["What"], mount["ActiveState"] = "/var", "/dev/vdb1", "active"
			mount["Requires"] = "dev-vdb1.device systemd-fsck@dev-vdb1.service -.mount"
			mount["After"] = "dev-vdb1.device blockdev@dev-vdb1.target systemd-fsck@dev-vdb1.service local-fs-pre.target systemd-remount-fs.service -.mount"
			mount["Before"], mount["Conflicts"] = "local-fs.target umount.target", "umount.target"
			mount["StopPropagatedFrom"] = "dev-vdb1.device"
			protection := OperationProtection{UnitPaths: map[string][]string{"billet-server.service": {"/var/lib/billet/server"}}}
			sequence := []Operation{{Verb: verb, Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("separate /var on /dev/vdb1: %v", err)
			}
			for _, call := range f.calls {
				if strings.HasSuffix(call, " var.mount") && strings.Contains(call, "--property=Requires") {
					t.Fatalf("traversed ancestor mount graph: %s", call)
				}
			}
			mount["ActiveState"] = "inactive"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-standard-effect: ancestor var.mount") {
				t.Fatalf("inactive ancestor admitted: %v", err)
			}
			mount["ActiveState"] = "active"
			server["Requires"] += " dev-vdc1.device"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: billet-server.service Requires=dev-vdc1.device") {
				t.Fatalf("unrelated device admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionBindsMountEdgesToTheirSource(t *testing.T) {
	for _, c := range []struct{ property, destination string }{
		{"Requires", "dev-vdc1.device"},
		{"After", "blockdev@dev-vdc1.target"},
		{"After", "unrelated.mount"},
		{"OnSuccess", "multi-user.target"},
		{"Triggers", "billet-server.service"},
	} {
		t.Run(c.property+"/"+c.destination, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			server["Requires"], server["RequiresMountsFor"] = "ledger.mount", "/ledger"
			mount := f.unit(t, "ledger.mount")
			mount["ActiveState"], mount["What"] = "active", "/dev/vdb1"
			mount["Requires"], mount["After"] = "dev-vdb1.device", "dev-vdb1.device blockdev@dev-vdb1.target"
			mount["StopPropagatedFrom"] = "dev-vdb1.device"
			protection := OperationProtection{UnitPaths: map[string][]string{"billet-server.service": {"/ledger"}}}
			sequence := []Operation{{Verb: "disable", Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("block-backed mount control: %v", err)
			}
			mount[c.property] = c.destination
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: ledger.mount "+c.property+"="+c.destination) {
				t.Fatalf("unrelated mount edge admitted: %v", err)
			}
		})
	}
	f := newOperationFixture(t)
	server := f.unit(t, "billet-server.service")
	server["Requires"], server["RequiresMountsFor"] = "ledger.mount", "/ledger"
	mount := f.unit(t, "ledger.mount")
	mount["ActiveState"], mount["Type"], mount["What"], mount["After"] = "active", "tmpfs", "tmpfs", "swap.target"
	protection := OperationProtection{UnitPaths: map[string][]string{"billet-server.service": {"/ledger"}}}
	if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "disable", Unit: "billet-server.service"}}, protection); err != nil {
		t.Fatalf("tmpfs swap ordering: %v", err)
	}
	mount["Type"], mount["What"] = "ext4", "/dev/vdb1"
	if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "disable", Unit: "billet-server.service"}}, protection); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: ledger.mount After=swap.target") {
		t.Fatalf("tmpfs rule applied to block mount: %v", err)
	}
}
