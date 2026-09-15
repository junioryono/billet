package lifeops

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Independent loaded values, not generated from the admission policy.
func fixtureOperationSetup() map[string]operationSetupValue {
	values := map[string]operationSetupValue{}
	for name, value := range map[string]string{
		"StandardInput": "null", "StandardOutput": "journal", "StandardError": "inherit",
		"User": "", "KeyringMode": "private", "Type": "oneshot", "Restart": "no",
		"Capabilities": "", "RebootArgument": "", "StartLimitAction": "none", "FailureAction": "none",
	} {
		values[name] = operationSetupValue{Type: "s", Data: json.RawMessage(strconv.Quote(value))}
	}
	values["PrivateTmp"] = operationSetupValue{Type: "b", Data: json.RawMessage("false")}
	values["Delegate"] = operationSetupValue{Type: "b", Data: json.RawMessage("false")}
	values["LoadCredential"] = operationSetupValue{Type: "a(ss)", Data: json.RawMessage("[]")}
	values["RuntimeDirectory"] = operationSetupValue{Type: "as", Data: json.RawMessage("[]")}
	for _, name := range []string{"ReadWriteDirectories", "ReadOnlyDirectories", "InaccessibleDirectories"} {
		values[name] = operationSetupValue{Type: "as", Data: json.RawMessage("[]")}
	}
	values["PermissionsStartOnly"] = operationSetupValue{Type: "b", Data: json.RawMessage("false")}
	values["IOScheduling"] = operationSetupValue{Type: "i", Data: json.RawMessage("0")}
	values["StartLimitInterval"] = operationSetupValue{Type: "t", Data: json.RawMessage("10000000")}
	values["StartLimitBurst"] = operationSetupValue{Type: "u", Data: json.RawMessage("5")}
	return values
}

func (f *operationFixture) setupReply(args []string) ([]byte, error) {
	for unit, values := range f.setups {
		if operationObjectPath(unit) != args[3] {
			continue
		}
		if args[4] != operationExecutionInterface(unit) {
			return nil, fmt.Errorf("wrong setup interface: %v", args)
		}
		if f.setupRead != nil {
			f.setupRead(unit)
		}
		var out strings.Builder
		if args[0] == "--xml-interface" {
			fmt.Fprintf(&out, "<node><interface name=%q>", args[4])
			for name, value := range values {
				if strings.Contains(" Capabilities ReadWriteDirectories ReadOnlyDirectories InaccessibleDirectories IOScheduling PermissionsStartOnly StartLimitInterval StartLimitBurst StartLimitAction FailureAction RebootArgument ", " "+name+" ") {
					continue
				}
				fmt.Fprintf(&out, "<property name=%q type=%q access=\"read\"/>", name, value.Type)
			}
			out.WriteString("</interface></node>")
		} else {
			for _, name := range args[5:] {
				value, ok := values[name]
				if !ok {
					return nil, fmt.Errorf("missing setup property %s", name)
				}
				body, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				fmt.Fprintln(&out, string(body))
			}
		}
		return []byte(out.String()), nil
	}
	return nil, fmt.Errorf("missing setup object %s", args[3])
}

func TestNewHelperSetupIsAClosedAllowlist(t *testing.T) {
	cases := []struct{ name, kind, data string }{
		{"StandardOutput", "s", `"truncate:/run/billet/registration/current"`},
		{"StandardOutput", "s", `"truncate"`},
		{"StandardError", "s", `"append"`},
		{"StandardInput", "s", `"file"`},
		{"StandardOutput", "s", `"socket"`},
		{"StandardError", "s", `"fd"`},
		{"StandardOutput", "s", `"future-mode"`},
		{"StandardOutputFileDescriptorName", "s", `"retained"`},
		{"PrivateTmp", "b", "true"}, {"Delegate", "b", "true"}, {"NonBlocking", "b", "true"},
		{"PAMName", "s", `"login"`}, {"UtmpIdentifier", "s", `"tty1"`},
		{"KeyringMode", "s", `"shared"`}, {"RootDirectory", "s", `"/retained"`},
		{"RootImage", "s", `"/retained.img"`}, {"RootEphemeral", "b", "true"},
		{"DynamicUser", "b", "true"}, {"RemoveIPC", "b", "true"},
		{"IPCNamespacePath", "s", `"/proc/1/ns/ipc"`},
		{"NetworkNamespacePath", "s", `"/proc/1/ns/net"`},
		{"PrivateIPC", "b", "true"}, {"MountAPIVFS", "b", "true"},
		{"ReadWritePaths", "as", `["/retained"]`},
		{"ReadOnlyPaths", "as", `["/retained"]`},
		{"InaccessiblePaths", "as", `["/retained"]`},
		{"TemporaryFileSystem", "a(ss)", `[["/retained",""]]`},
		{"BindPaths", "a(ssbt)", `[["/tmp","/retained",false,0]]`},
		{"BindReadOnlyPaths", "a(ssbt)", `[["/tmp","/retained",false,0]]`},
		{"MountImages", "a(ssba(ss))", `[["/disk","/retained",false,[]]]`},
		{"ExtensionImages", "a(sba(ss))", `[["/disk",false,[]]]`},
		{"ExtensionDirectories", "as", `["/retained"]`},
		{"LoadCredential", "a(ss)", `[["token","/retained"]]`},
		{"LoadCredentialEncrypted", "a(ss)", `[["token","/retained"]]`},
		{"SetCredential", "a(say)", `[["token",[1]]]`},
		{"SetCredentialEncrypted", "a(say)", `[["token",[1]]]`},
		{"ImportCredential", "as", `["*"]`},
		{"ReadWriteDirectories", "as", `["/retained"]`},
		{"PermissionsStartOnly", "b", "true"},
		{"OpenFile", "a(sst)", `[["/retained","file",0]]`},
		{"PIDFile", "s", `"/retained"`}, {"LogNamespace", "s", `"retained"`},
		{"TTYVHangup", "b", "true"}, {"TTYReset", "b", "true"},
		{"Sockets", "as", `["retained.socket"]`},
		{"NFTSet", "a(iiss)", `[[0,0,"filter","retained"]]`},
		{"FutureExecutionProperty", "s", `""`},
	}
	for _, c := range cases {
		t.Run(c.name+"/"+c.data, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			f.unit(t, "helper.service")
			server["OnSuccess"] = "helper.service"
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			protection := OperationProtection{Units: []string{"billet-server.service"}, Paths: []string{"/run/billet/registration"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("clean helper refused: %v", err)
			}
			f.setups["helper.service"][c.name] = operationSetupValue{Type: c.kind, Data: json.RawMessage(c.data)}
			err := f.inspector.AdmitOperations(t.Context(), sequence, protection)
			if err == nil || !strings.Contains(err.Error(), "operation-setup-unsupported: helper.service "+c.name) {
				t.Fatalf("single unadmitted setup value escaped: %v", err)
			}
		})
	}
}

func TestHelperSetupRequiresCompleteStableTypedEvidence(t *testing.T) {
	for _, defect := range []string{"missing", "type", "changed", "new property", "transient"} {
		t.Run(defect, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			f.unit(t, "helper.service")
			server["OnSuccess"] = "helper.service"
			values := f.setups["helper.service"]
			if defect == "transient" {
				f.units["helper.service"]["Transient"] = "yes"
			}
			reads := 0
			f.setupRead = func(unit string) {
				if unit != "helper.service" {
					return
				}
				reads++
				switch defect {
				case "missing":
					delete(values, "PrivateTmp")
				case "type":
					values["StandardOutput"] = operationSetupValue{Type: "b", Data: json.RawMessage("false")}
				case "changed":
					if reads == 3 {
						values["StandardOutput"] = operationSetupValue{Type: "s", Data: json.RawMessage(`"truncate"`)}
					}
				case "new property":
					if reads == 3 {
						values["FutureSetup"] = operationSetupValue{Type: "b", Data: json.RawMessage("false")}
					}
				}
			}
			err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{})
			if err == nil || !strings.Contains(err.Error(), "operation-setup-") {
				t.Fatalf("incomplete/changed setup admitted: %v", err)
			}
		})
	}
}

func TestHelperSetupCoversEveryNewStartAndKeepsActiveNoops(t *testing.T) {
	for _, route := range []string{"target", "Requires", "Triggers", "OnSuccess", "transitive stop"} {
		t.Run(route, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			helper := f.unit(t, "helper.service")
			sequence := []Operation{{Verb: "start", Unit: "billet-server.service"}}
			switch route {
			case "target":
				sequence[0].Unit = "helper.service"
			case "OnSuccess":
				sequence[0].Verb = "stop"
				server[route] = "helper.service"
			case "transitive stop":
				server["PropagatesStopTo"], server["Requires"] = "helper.service", "helper.service"
				sequence = []Operation{{Verb: "stop", Unit: "billet-server.service"}, {Verb: "start", Unit: "billet-server.service"}}
			default:
				server[route] = "helper.service"
			}
			protection := OperationProtection{Units: []string{"billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("clean %s start refused: %v", route, err)
			}
			f.setups["helper.service"]["StandardOutput"] = operationSetupValue{Type: "s", Data: json.RawMessage(`"truncate"`)}
			if route == "transitive stop" {
				helper["ActiveState"] = "active"
			}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-setup-unsupported") {
				t.Fatalf("new start via %s escaped setup admission: %v", route, err)
			}
			if route == "target" || route == "Requires" || route == "Triggers" {
				helper["ActiveState"] = "active"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
					t.Fatalf("active no-op unnecessarily admitted execution setup: %v", err)
				}
			}
		})
	}
}

func TestHelperSetupDirectoriesUseProtectedPathBindings(t *testing.T) {
	for _, c := range []struct{ name, root string }{
		{"StateDirectory", "/var/lib"}, {"RuntimeDirectory", "/run"}, {"CacheDirectory", "/var/cache"},
		{"LogsDirectory", "/var/log"}, {"ConfigurationDirectory", "/etc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			helper := f.unit(t, "helper.service")
			server["OnSuccess"] = "helper.service"
			helper[c.name] = "billet-helper"
			f.setups["helper.service"][c.name] = operationSetupValue{Type: "as", Data: json.RawMessage(`["billet-helper"]`)}
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			protection := OperationProtection{Units: []string{"billet-server.service"}, Paths: []string{c.root + "/billet/registration"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("separate helper directory refused: %v", err)
			}
			helper[c.name] = "billet/registration"
			f.setups["helper.service"][c.name] = operationSetupValue{Type: "as", Data: json.RawMessage(`["billet/registration"]`)}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") {
				t.Fatalf("new helper can reown protected %s: %v", c.name, err)
			}
		})
	}
}

func TestHelperSetupRefusesSocketAndNamespaceJoins(t *testing.T) {
	for _, relation := range []string{"TriggeredBy", "JoinsNamespaceOf"} {
		t.Run(relation, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			helper := f.unit(t, "helper.service")
			f.unit(t, "external.socket")
			server["OnSuccess"] = "helper.service"
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err != nil {
				t.Fatalf("isolated helper refused: %v", err)
			}
			helper[relation] = "external.socket"
			if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-setup-unsupported") {
				t.Fatalf("helper joined another unit's execution resources: %v", err)
			}
		})
	}
}
