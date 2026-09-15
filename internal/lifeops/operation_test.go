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
	if err := os.WriteFile(path, []byte("[Unit]\nDescription=operation fixture\n[Install]\nWantedBy=multi-user.target\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := map[string]string{
		"Id": name, "Names": name, "LoadState": "loaded", "ActiveState": "inactive", "UnitFileState": "disabled",
		"FragmentPath": path, "SourcePath": "", "DropInPaths": "", "NeedDaemonReload": "no",
		"OnSuccessJobMode": "fail", "OnFailureJobMode": "replace", "FailureAction": "none", "SuccessAction": "none",
		"StartLimitAction": "none", "JobTimeoutAction": "none", "RequiresMountsFor": "", "Where": "/ledger",
		"KillMode": "control-group", "DynamicUser": "no", "RuntimeDirectoryPreserve": "no", "StopWhenUnneeded": "no",
	}
	// Independent of the production query lists, so deleting a queried property
	// cannot delete that evidence from the fixture at the same time.
	for _, property := range strings.Fields(
		"Requires Requisite Wants BindsTo Upholds PartOf RequiredBy RequisiteOf WantedBy BoundBy UpheldBy ConsistsOf " +
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
			if err == nil || !strings.Contains(err.Error(), "operation-protected-effect") || !strings.Contains(err.Error(), "billet-node.service") {
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
				if err == nil || !strings.Contains(err.Error(), "operation-install-") {
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
		{"mount", "ledger.mount", "ActiveState", "active"},
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
			if err == nil || !strings.Contains(err.Error(), "operation-protected-") {
				t.Fatalf("protected %s admitted: %v", c.name, err)
			}
		})
	}
}

func TestOperationAdmissionRefusesUnreadableDriftingAndBoundedEvidence(t *testing.T) {
	for _, problem := range []string{"missing property", "empty names", "unreadable source", "source drift", "reload", "job mode", "bound"} {
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
				want = "operation-job-mode"
			case "bound":
				for n := range operationUnitLimit {
					name := fmt.Sprintf("helper-%d.service", n)
					p["PropagatesStopTo"] = name
					p = f.unit(t, name)
				}
				want = "operation-traversal-bound"
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
	absent := f.unit(t, "optional.service")
	masked := f.unit(t, "container-only.service")
	absent["LoadState"], absent["FragmentPath"] = "not-found", ""
	masked["LoadState"], masked["UnitFileState"], masked["FragmentPath"] = "masked", "masked", "/dev/null"
	node["Requires"], node["Wants"], node["Conflicts"] = "-.mount", "network-online.target optional.service container-only.service", "shutdown.target"
	node["RuntimeDirectory"] = "billet/locks billet/registration"
	network["ActiveState"], root["ActiveState"], shutdown["ActiveState"] = "active", "active", "inactive"
	root["FragmentPath"], root["Where"] = "", "/"
	protection := OperationProtection{Units: []string{"billet-node.service", "billet-server.service"},
		UnitPaths: map[string][]string{"billet-node.service": {"/run/billet/locks", "/run/billet/registration"}}}
	for _, verb := range []string{"enable", "stop", "start", "disable"} {
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: verb, Unit: "billet-node.service"}}, protection); err != nil {
			t.Fatalf("supported infrastructure %s: %v", verb, err)
		}
	}
	root["Wants"] = "billet-server.service"
	err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "start", Unit: "billet-node.service"}}, protection)
	if err == nil || !strings.Contains(err.Error(), "operation-protected-effect") {
		t.Fatalf("an active mount hid a transitive activation: %v", err)
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
			want := "operation-reactivation"
			switch relation {
			case "StopWhenUnneeded":
				server["Requires"] = "billet-node.service"
				node["StopWhenUnneeded"] = "yes"
				want = "operation-protected-effect"
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

// R34: unrelated infrastructure is a positive control, including commands and
// job policies ordinary distribution units carry. The same helper must refuse
// as soon as its runtime graph can act on the protected controller.
func TestOperationAdmissionScopesCommandsToProtectedEffects(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		t.Run(verb, func(t *testing.T) {
			f := newOperationFixture(t)
			node := f.unit(t, "billet-node.service")
			f.unit(t, "billet-server.service")
			helper := f.unit(t, "billet-network.service")
			node["Requires"] = "billet-network.service"
			if verb == "stop" {
				helper["StopWhenUnneeded"] = "yes"
			}
			helper["ExecStartPre"], helper["ExecStop"] = `"/usr/bin/true" 1 "/usr/bin/true" false 0 0 0 0 0 0 0`, `"/usr/bin/true" 1 "/usr/bin/true" false 0 0 0 0 0 0 0`
			helper["OnFailureJobMode"] = "isolate"
			protection := OperationProtection{Units: []string{"billet-node.service", "billet-server.service"}}
			sequence := []Operation{{Verb: verb, Unit: "billet-node.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("unrelated ordinary dependency: %v", err)
			}
			helper["FailureAction"] = "reboot"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-manager-action") {
				t.Fatalf("helper reboot admitted: %v", err)
			}
			helper["FailureAction"] = "none"
			delete(helper, "ExecStop")
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("irrelevant command evidence was required: %v", err)
			}
			helper["OnSuccess"] = "billet-server.service"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-protected-effect") {
				t.Fatalf("helper reaching the controller was admitted: %v", err)
			}
			delete(helper, "OnSuccess")
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-property-unknown") {
				t.Fatalf("unknown helper reachability was admitted: %v", err)
			}
		})
	}
}

// R34: empty struct arrays require typed evidence. A missing show property is
// neither a configured command nor a proof that the array is empty.
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

func TestOperationAdmissionProtectsMountPathsWithoutProtectingEveryMount(t *testing.T) {
	f := newOperationFixture(t)
	server := f.unit(t, "billet-server.service")
	mount := f.unit(t, "scratch.mount")
	mount["Where"], mount["ActiveState"] = "/scratch", "active"
	server["PropagatesStopTo"] = "scratch.mount"
	sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
	protection := OperationProtection{Paths: []string{"/ledger/identity"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
		t.Fatalf("unprotected mount stop: %v", err)
	}
	mount["Where"] = "/ledger"
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-protected-mount") {
		t.Fatalf("ledger mount stop admitted: %v", err)
	}
	delete(mount, "Where")
	if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-property-unknown") {
		t.Fatalf("unknown mount point admitted: %v", err)
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
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-install-collateral") {
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

func TestOperationAdmissionJobModeOnlyAppliesToNonemptyHandlers(t *testing.T) {
	for _, relation := range []string{"OnSuccess", "OnFailure"} {
		for _, mode := range []string{"fail", "replace", "isolate", "flush", "ignore-dependencies"} {
			t.Run(relation+"/"+mode, func(t *testing.T) {
				f := newOperationFixture(t)
				server := f.unit(t, "billet-server.service")
				f.unit(t, "helper.service")
				f.unit(t, "billet-node.service")
				server[relation+"JobMode"] = mode
				sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
				protection := OperationProtection{Units: []string{"billet-node.service"}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("empty handler has no job-mode effect: %v", err)
				}
				server[relation] = "helper.service"
				err := f.inspector.AdmitOperations(t.Context(), sequence, protection)
				if mode != "fail" && mode != "replace" {
					if err == nil || !strings.Contains(err.Error(), "operation-job-mode") {
						t.Fatalf("unsupported transaction mode admitted: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("supported handler transaction refused: %v", err)
				}
				server[relation] = "billet-node.service"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-protected-effect") {
					t.Fatalf("supported job mode bypassed handler traversal: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionHelperManagerActions(t *testing.T) {
	for _, property := range []string{"FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction"} {
		for _, verb := range []string{"start", "stop"} {
			t.Run(verb+"/"+property, func(t *testing.T) {
				f := newOperationFixture(t)
				server := f.unit(t, "billet-server.service")
				helper := f.unit(t, "helper.service")
				relation := "OnSuccess"
				if verb == "stop" {
					relation = "PropagatesStopTo"
				}
				server[relation] = "helper.service"
				sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
					t.Fatalf("clean helper: %v", err)
				}
				helper[property] = "reboot"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-manager-action") {
					t.Fatalf("helper action admitted: %v", err)
				}
				delete(helper, property)
				if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-property-unknown") {
					t.Fatalf("unreadable helper action admitted: %v", err)
				}
			})
		}
	}
}

func TestOperationAdmissionExecutionContextUnitTypes(t *testing.T) {
	for _, kind := range []string{"service", "socket", "mount", "swap"} {
		for _, directive := range []string{"StateDirectory", "RuntimeDirectory", "CacheDirectory", "LogsDirectory", "ConfigurationDirectory"} {
			t.Run(kind+"/"+directive, func(t *testing.T) {
				f := newOperationFixture(t)
				server := f.unit(t, "billet-server.service")
				name := "helper." + kind
				helper := f.unit(t, name)
				helper["Where"] = "/unprotected-mount"
				server["PropagatesStopTo"] = name
				sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
				protection := OperationProtection{Paths: []string{filepath.Join(operationDirectoryRoots[directive], "billet/registration")}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("clean execution context: %v", err)
				}
				helper[directive] = "billet/registration"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") {
					t.Fatalf("directory collateral admitted: %v", err)
				}
				delete(helper, directive)
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-property-unknown") {
					t.Fatalf("unobserved directories treated as empty: %v", err)
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
					body += "Also=unrelated.service\n"
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
		if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, protection); err == nil || !strings.Contains(err.Error(), "operation-protected-effect") {
			t.Fatalf("guest network stop admitted: %s %v", unit, err)
		}
	}
}
