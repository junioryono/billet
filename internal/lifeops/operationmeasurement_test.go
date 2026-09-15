package lifeops

import (
	"fmt"
	"strings"
	"testing"
)

func TestOperationAdmissionMeasuredLedgerRelationships(t *testing.T) {
	for _, host := range []string{"retained", "server-only"} {
		t.Run(host, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			backup := f.unit(t, "billet-backup.service")
			const ledger = `ledger\x2dvolume.mount`
			mount := f.unit(t, ledger)
			mount["Where"], mount["ActiveState"] = "/ledger-volume", "active"
			// systemctl 255.4 renders string arrays with an extra quoting layer.
			mount["Names"] = `"ledger\\x2dvolume.mount"`
			mount["RequiredBy"] = "billet-backup.service billet-server.service"
			mount["Before"] = "billet-backup.service billet-server.service"
			for _, props := range []map[string]string{server, backup} {
				props["Requires"] = `"ledger\\x2dvolume.mount"`
				props["After"] = `"ledger\\x2dvolume.mount"`
				props["RequiresMountsFor"] = "/ledger-volume"
			}
			protection := OperationProtection{
				Units:     []string{"billet-server.service", "billet-backup.service"},
				UnitPaths: map[string][]string{"billet-server.service": {"/ledger-volume"}},
			}
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}, {Verb: "disable", Unit: "billet-server.service"}}
			if host == "retained" {
				f.unit(t, "billet-node.service")
				protection.Units = append(protection.Units, "billet-node.service")
				sequence = append(sequence, Operation{Verb: "enable", Unit: "billet-node.service"},
					Operation{Verb: "stop", Unit: "billet-node.service"}, Operation{Verb: "start", Unit: "billet-node.service"})
			}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("measured server/backup ledger edges and inverses refused: %v", err)
			}
			backup["Requires"] = `"other\\x2dvolume.mount"`
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil ||
				!strings.Contains(err.Error(), `operation-edge-outside-set: billet-backup.service Requires=other\x2dvolume.mount`) {
				t.Fatalf("backup mount permission escaped the exact ledger: %v", err)
			}
		})
	}
}

func TestOperationAdmissionMeasuredTemplateSlice(t *testing.T) {
	for _, relation := range []string{"Requires", "After"} {
		for _, problem := range []string{"", "other template", "instance slice", "unescaped prefix", "inactive", "masked", "job"} {
			t.Run(relation+"/"+problem, func(t *testing.T) {
				f := newOperationFixture(t)
				const dns = "billet-effects-dnsmasq@br0.service"
				const slice = `system-billet\x2deffects\x2ddnsmasq.slice`
				d := f.unit(t, dns)
				d[relation] = `"system-billet\\x2deffects\\x2ddnsmasq.slice"`
				s := f.unit(t, slice)
				s["Names"], s["ActiveState"] = fmt.Sprintf("%q", slice), "active"
				p := OperationProtection{Units: []string{dns}, RequiredActive: []string{dns}}
				if err := f.inspector.AdmitOperations(t.Context(), nil, p); err != nil {
					t.Fatalf("exact active per-template slice refused: %v", err)
				}
				want := "operation-edge-outside-set: " + dns + " " + relation + "="
				switch problem {
				case "":
					return
				case "other template":
					d[relation] = `"system-other\\x2ddnsmasq.slice"`
				case "instance slice":
					d[relation] = `"system-billet\\x2deffects\\x2ddnsmasq@br0.slice"`
				case "unescaped prefix":
					d[relation] = "system-billet-effects-dnsmasq.slice"
				case "inactive":
					s["ActiveState"] = "inactive"
					want = "operation-standard-effect:"
				case "masked":
					s["LoadState"], s["ActiveState"] = "masked", "inactive"
					want = "operation-standard-effect:"
				case "job":
					s["Job"] = "42"
					want = "operation-standard-effect:"
				}
				if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("template slice proof admitted or refused for another reason: %v", err)
				}
			})
		}
	}
}

func TestOperationUnitListsPreserveUnitNameEscapes(t *testing.T) {
	for _, c := range []struct{ rendered, want string }{
		{`"run-billet\\x2dledger.mount" sysinit.target`, `run-billet\x2dledger.mount sysinit.target`},
		{`"system-billet\\x2ddnsmasq.slice"`, `system-billet\x2ddnsmasq.slice`},
		{`"system-billet\\x5cx2ddnsmasq.slice"`, `system-billet\x5cx2ddnsmasq.slice`},
		{`run-billet\x2dledger.mount`, `run-billet\x2dledger.mount`},
		{"", ""},
	} {
		got, err := operationUnitList(c.rendered)
		if err != nil || got != c.want {
			t.Fatalf("unit list %q = %q, %v; want %q", c.rendered, got, err, c.want)
		}
	}
	for _, malformed := range []string{`"unterminated.service`, `"two units.service"`, `"bad\\q.service"`, `"line\nunit.service"`} {
		if _, err := operationUnitList(malformed); err == nil {
			t.Fatalf("malformed unit list accepted: %q", malformed)
		}
	}
}
