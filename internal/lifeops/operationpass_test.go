package lifeops

import (
	"slices"
	"strings"
	"testing"
)

func TestOperationAdmissionBatchesRetainedShippedSequence(t *testing.T) {
	f := newOperationFixture(t)
	units := []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service", "billet-upgrade.timer", "billet-backup.timer"}
	for _, unit := range units {
		p := f.unit(t, unit)
		p["Requires"] = "sysinit.target"
		if strings.HasSuffix(unit, ".service") {
			p["Requires"] += " system.slice"
			p["Wants"] = "network-online.target"
		}
	}
	// The shipped services' directory and mount prerequisites on a root-only
	// filesystem. Additional host mounts are distinct no-op leaves.
	for unit, path := range map[string]string{
		"billet-server.service": "/var/lib/billet/server",
		"billet-node.service":   "/var/lib/billet/node",
		"billet-backup.service": "/var/lib/billet/server",
	} {
		f.units[unit]["Requires"] += " -.mount"
		f.units[unit]["RequiresMountsFor"] = path
		f.units[unit]["PrivateTmp"] = "yes"
		f.units[unit]["RequiresMountsFor"] += " /var/tmp"
		f.units[unit]["Wants"] += " tmp.mount"
	}
	for _, unit := range []string{"sysinit.target", "network-online.target", "system.slice", "-.mount"} {
		f.unit(t, unit)["ActiveState"] = "active"
	}
	tmp := f.unit(t, "tmp.mount")
	tmp["LoadState"], tmp["ActiveState"] = "not-found", "inactive"
	f.units["billet-server.service"]["StateDirectory"] = "billet/server"
	f.units["billet-node.service"]["StateDirectory"] = "billet/node"
	f.units["billet-node.service"]["RuntimeDirectory"] = "billet/locks billet/registration"
	f.units["billet-backup.service"]["StateDirectory"] = "billet/backups"
	f.units["billet-node.service"]["ActiveState"] = "active"
	f.units["billet-node.service"]["ReadOnlyPaths"] = "/etc/billet"
	f.units["billet-node.service"]["ReadWritePaths"] = "-/etc/billet/tls -/srv/jailer"
	var sequence []Operation
	for _, unit := range []string{"billet-upgrade.timer", "billet-backup.timer", "billet-server.service"} {
		sequence = append(sequence, Operation{Verb: "stop", Unit: unit}, Operation{Verb: "disable", Unit: unit})
	}
	for _, verb := range []string{"enable", "stop", "start"} {
		sequence = append(sequence, Operation{Verb: verb, Unit: "billet-node.service"})
	}
	protection := OperationProtection{Units: units, RetainedPathUnits: []string{"billet-node.service"},
		QuietUnits: []string{"billet-server.service", "billet-backup.service", "billet-upgrade.service"},
		UnitPaths: map[string][]string{"billet-server.service": {"/var/lib/billet/server"},
			"billet-node.service": {"/var/lib/billet/node", "/run/billet/locks", "/run/billet/registration"}}}
	for range 2 {
		f.calls = nil
		if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
			t.Fatalf("full retained sequence: %v", err)
		}
		shows, typed := make(map[string]int), make(map[string]int)
		for _, call := range f.calls {
			args := strings.Fields(call)
			switch {
			case args[0] == "show":
				unit := args[len(args)-1]
				shows[unit]++
				switch {
				case unit == "billet-node.service":
					if !slices.Equal(args, []string{"show", "--all", "--", unit}) {
						t.Fatalf("retained inventory was filtered: %s", call)
					}
				case len(args) != 4 || !strings.HasPrefix(args[1], "--property=") || strings.Contains(args[1], "*"):
					t.Fatalf("properties were split across requests: %s", call)
				}
			case args[0] == "get-property":
				typed[args[2]]++
			case slices.Equal(args[:2], []string{"--json=short", "call"}):
				typed[args[3]]++
			default:
				t.Fatalf("unexpected admission invocation: %s", call)
			}
		}
		if len(shows) != 11 || len(typed) != 4 || len(f.calls) != 30 {
			t.Fatalf("retained admission calls=%d shows=%v typed=%v", len(f.calls), shows, typed)
		}
		for unit, count := range shows {
			if count != 2 {
				t.Fatalf("%s show count=%d want one observation plus one reread", unit, count)
			}
		}
		for unit, count := range typed {
			if count != 2 {
				t.Fatalf("%s typed count=%d want one observation plus one reread", unit, count)
			}
		}
	}
}
