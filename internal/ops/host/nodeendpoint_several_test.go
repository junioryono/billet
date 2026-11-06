package host

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/rollout"
)

// A NODE THAT NAMES SEVERAL CONTROLLERS, through the endpoint commands (#684).
// What is compared is the list the running process dials, from its record, never
// only the controller it is on; an order is not a difference; and everything
// about a node with one controller stays what it always was.

// several is the node-only configuration naming addrs.
func (f *endpointFixture) several(addrs ...string) string {
	quoted := make([]string, len(addrs))
	for i, addr := range addrs {
		quoted[i] = `"` + addr + `"`
	}

	return strings.Replace(f.nodeOnlyConfig(), "server_addr: "+endpointA, "server_addrs: ["+strings.Join(quoted, ", ")+"]", 1)
}

// dialling is a record of this node on one controller, whose process dials the
// list; no list is the one-controller record, which a process knowing no other
// writes.
func (f *endpointFixture) dialling(on string, invocation, incarnation string, list ...string) map[string]any {
	over := map[string]any{"deployment": f.deployment, "endpoint": on, "invocation_id": invocation, "incarnation": incarnation}
	if len(list) > 0 {
		over["schema"], over["endpoints"] = 2, list
	}

	return f.record(over)
}

func (f *endpointFixture) recordAfterStartDialling(t *testing.T, on string, list ...string) {
	t.Helper()

	body, err := json.Marshal(f.dialling(on, newInvocation, newIncarnation, list...))
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.unitFile("record.after-start"), string(body)+"\n", 0o600)
}

func asStrings(t *testing.T, v any) []string {
	t.Helper()

	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a list", v)
	}

	out := make([]string, len(list))
	for i, e := range list {
		out[i] = asString(e)
	}

	return out
}

// THE DECISION, over each thing a process can dial against each thing a rendering
// can name.
func TestTheDecisionComparesWhatTheProcessDials(t *testing.T) {
	both := []string{canonicalA, canonicalB}

	for name, tc := range map[string]struct {
		installed string
		on        string
		dials     []string
		rendered  func(f *endpointFixture) string
		planned   bool
		from, to  []string
	}{
		"both, and both wanted": {
			installed: "both", on: canonicalA, dials: both,
			rendered: func(f *endpointFixture) string { return f.several(endpointA, endpointB) }, from: both, to: both,
		},
		"both, on the second, and both wanted": {
			installed: "both", on: canonicalB, dials: both,
			rendered: func(f *endpointFixture) string { return f.several(endpointA, endpointB) }, from: both, to: both,
		},
		"both, wanted in another order": {
			installed: "both", on: canonicalA, dials: both,
			rendered: func(f *endpointFixture) string { return f.several(endpointB, endpointA) }, from: both,
			to: []string{canonicalB, canonicalA},
		},
		// THE CASE THE LIST IN THE RECORD EXISTS FOR: the node is on a controller
		// the rendering still names, and its process knows no other.
		"one, on a controller the rendering still names": {
			installed: "one", on: canonicalA,
			rendered: func(f *endpointFixture) string { return f.several(endpointA, endpointB) }, planned: true,
			from: []string{canonicalA}, to: both,
		},
		// AND WITH NOTHING ELSE TO GIVE IT AWAY: the render has already happened, so
		// the two configurations agree, and only the record says the process is
		// behind them.
		"one, with both installed and both wanted": {
			installed: "both", on: canonicalA,
			rendered: func(f *endpointFixture) string { return f.several(endpointA, endpointB) }, planned: true,
			from: []string{canonicalA}, to: both,
		},
		"both, and the first alone wanted": {
			installed: "both", on: canonicalA, dials: both,
			rendered: func(f *endpointFixture) string { return f.rendering(endpointA) }, planned: true,
			from: both, to: []string{canonicalA},
		},
		"both, and a list of one wanted": {
			installed: "both", on: canonicalA, dials: both,
			rendered: func(f *endpointFixture) string { return f.several(endpointA) }, planned: true,
			from: both, to: []string{canonicalA},
		},
		"one, and a list of that one wanted": {
			installed: "one", on: canonicalA,
			rendered: func(f *endpointFixture) string { return f.several(endpointA) },
		},
		"one, and the same one wanted": {
			installed: "one", on: canonicalA,
			rendered: func(f *endpointFixture) string { return f.rendering(endpointA) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEndpointFixture(t)

			if tc.installed == "both" {
				f.writeConfig(t, f.several(endpointA, endpointB))
			}

			f.writeRecord(t, f.dialling(tc.on, nodeInvocation, testIncarnation, tc.dials...))

			o := f.migrate(t, tc.rendered(f), "--dry-run")
			mustEndpointOutcome(t, o, outcomeReported)

			if o.boolean("planned") != tc.planned {
				t.Fatalf("planned = %v, want %v: %s", o.boolean("planned"), tc.planned, o.raw)
			}

			if o.str("from") != tc.on {
				t.Fatalf("from = %q, want the controller the node is on, %q", o.str("from"), tc.on)
			}

			// ONLY A NODE NAMING SEVERAL CARRIES A LIST: one with one controller
			// answers what it always did, which the role's parser holds to the
			// member.
			for member, want := range map[string][]string{"from_endpoints": tc.from, "to_endpoints": tc.to} {
				value, present := o.doc[member]
				if present != (want != nil) {
					t.Fatalf("%s present: %v, want %v: %s", member, present, want != nil, o.raw)
				}

				if present && !slices.Equal(asStrings(t, value), want) {
					t.Fatalf("%s = %v, want %v", member, value, want)
				}
			}
		})
	}
}

// A PROCESS DIALLING THE INSTALLED CONTROLLERS IS LEFT ALONE, whichever it is on
// and in whatever order they are written.
func TestAProcessDiallingTheInstalledControllersIsNotStopped(t *testing.T) {
	f := newEndpointFixture(t)
	f.writeConfig(t, f.several(endpointA, endpointB))
	f.writeRecord(t, f.dialling(canonicalB, nodeInvocation, testIncarnation, canonicalB, canonicalA))

	o := f.migrate(t, f.several(endpointA, endpointB))
	mustEndpointOutcome(t, o, outcomeUnchanged)

	if f.calls(t, "stop") != 0 || f.calls(t, "start") != 0 {
		t.Fatal("a process dialling the installed controllers was stopped or started")
	}

	if o.str("endpoint") != canonicalB {
		t.Fatalf("endpoint = %q, want the controller the node is on", o.str("endpoint"))
	}

	if got := asStrings(t, o.doc["endpoints"]); !slices.Equal(got, []string{canonicalA, canonicalB}) {
		t.Fatalf("endpoints = %v, want the installed list in its order", got)
	}
}

// A MOVE TO THE LIST IS PROVED BY THE NEW PROCESS'S OWN LIST: stopped, started, and
// its record naming the controllers installed.
func TestAMoveToAListIsProvedByTheNewProcesssList(t *testing.T) {
	both := []string{canonicalA, canonicalB}

	t.Run("the new process dials them", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, f.several(endpointA, endpointB))
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStartDialling(t, canonicalB, both...)

		o := f.migrate(t, f.several(endpointA, endpointB))
		mustEndpointOutcome(t, o, outcomeMigrated)

		if f.calls(t, "stop") != 1 || f.calls(t, "start") != 1 {
			t.Fatal("the move did not stop and start the node once each")
		}

		if o.str("from") != canonicalA || o.str("to") != canonicalB {
			t.Fatalf("from %q to %q, want the controllers the node was and is on", o.str("from"), o.str("to"))
		}

		if got := asStrings(t, o.doc["from_endpoints"]); !slices.Equal(got, []string{canonicalA}) {
			t.Fatalf("from_endpoints = %v", got)
		}

		if got := asStrings(t, o.doc["to_endpoints"]); !slices.Equal(got, both) {
			t.Fatalf("to_endpoints = %v", got)
		}
	})

	// ON A LISTED CONTROLLER AND NOT KNOWING THE LIST is not the move.
	t.Run("the new process dials one of them", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, f.several(endpointA, endpointB))
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStartDialling(t, canonicalA)

		o := f.migrate(t, f.several(endpointA, endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

		if !strings.Contains(o.str("why"), canonicalA+", "+canonicalB) {
			t.Fatalf("the refusal does not name what was installed: %s", o.raw)
		}
	})

	// AND A RENDERING IN ANOTHER ORDER WAS NOT INSTALLED, whatever a process needs.
	t.Run("the rendering in another order", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, f.several(endpointA, endpointB))
		f.writeRecord(t, f.dialling(canonicalA, nodeInvocation, testIncarnation, both...))

		o := f.migrate(t, f.several(endpointB, endpointA))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)
	})

	t.Run("the one controller a list of two moves to", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, f.rendering(endpointB))
		f.writeRecord(t, f.dialling(canonicalA, nodeInvocation, testIncarnation, both...))
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStartDialling(t, canonicalB)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeMigrated)

		if got := asStrings(t, o.doc["from_endpoints"]); !slices.Equal(got, both) {
			t.Fatalf("from_endpoints = %v, want the list the old process dialled", got)
		}
	})
}

// THE RECEIPT OF A NODE NAMING SEVERAL is schema 2, and a failover is what makes it
// stale: the same process on the other controller, which the next refresh writes.
func TestTheReceiptNamesTheListAndFollowsAFailover(t *testing.T) {
	both := []string{canonicalA, canonicalB}

	f := newReceiptCmdFixture(t)
	f.writeConfig(t, f.several(endpointA, endpointB))
	f.touchBeforeStart(t, f.configPath)
	f.setNode(t, "active", "running", inspectPID, newInvocation, "mixed")
	f.writeRecord(t, f.dialling(canonicalA, newInvocation, newIncarnation, both...))

	mustWritten(t, f.refresh(t, f.several(endpointA, endpointB)))

	first := f.receiptOnDisk(t)
	if first.Schema != receiptSeveralSchema || !slices.Equal(first.InstalledEndpoints, both) ||
		first.InstalledEndpoint != "" || first.EffectiveEndpoint != canonicalA {
		t.Fatalf("the receipt is %+v", first)
	}

	if o := f.refresh(t, f.several(endpointA, endpointB)); o.str("outcome") != outcomeCurrent {
		t.Fatalf("a second refresh answered %s", o.raw)
	}

	f.writeRecord(t, f.dialling(canonicalB, newInvocation, newIncarnation, both...))
	mustWritten(t, f.refresh(t, f.several(endpointA, endpointB)))

	if got := f.receiptOnDisk(t); got.EffectiveEndpoint != canonicalB {
		t.Fatalf("after the failover the receipt says the node is on %s", got.EffectiveEndpoint)
	}

	// A RENDERING IN ANOTHER ORDER WAS NOT INSTALLED, though the process need not move.
	mustEndpointRefusal(t, f.refresh(t, f.several(endpointB, endpointA)), outcomeRefused, endpointReasonDesired)

	// A PROCESS THAT DOES NOT KNOW THE LIST owes a migration, on a listed controller.
	f.writeRecord(t, f.dialling(canonicalA, newInvocation, newIncarnation))
	mustEndpointRefusal(t, f.refresh(t, f.several(endpointA, endpointB)), outcomeRefused, endpointReasonMigration)
}

// A RECEIPT WRITTEN FOR A NODE NAMING ONE CONTROLLER STAYS SCHEMA 1, byte for byte,
// a list of one included: one controller is one endpoint however it is written.
func TestAReceiptForOneControllerStaysSchemaOne(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.writeConfig(t, f.several(endpointA))
	f.touchBeforeStart(t, f.configPath)
	f.setNode(t, "active", "running", inspectPID, newInvocation, "mixed")
	f.writeRecord(t, f.dialling(canonicalA, newInvocation, newIncarnation))

	mustWritten(t, f.refresh(t, f.several(endpointA)))

	got := f.receiptOnDisk(t)
	if got.Schema != receiptSchema || got.InstalledEndpoint != canonicalA || got.InstalledEndpoints != nil {
		t.Fatalf("the receipt is %+v", got)
	}
}

// THE RECEIPT FROM A MIGRATION'S EVIDENCE: the lists read strictly, the process's
// own list re-verified, and the controller it is on now recorded, since the same
// process may have failed over after the migration saw it.
func TestAMigrationsEvidenceCarriesTheList(t *testing.T) {
	both := []string{canonicalA, canonicalB}

	setup := func(t *testing.T) *receiptCmdFixture {
		t.Helper()

		f := newReceiptCmdFixture(t)
		f.writeConfig(t, f.several(endpointA, endpointB))
		f.touchBeforeStart(t, f.configPath)
		f.setNode(t, "active", "running", inspectPID, newInvocation, "mixed")
		f.writeRecord(t, f.dialling(canonicalA, newInvocation, newIncarnation, both...))

		return f
	}

	lists := func(from, to []string, on string) map[string]any {
		return map[string]any{"from": canonicalA, "to": on, "from_endpoints": from, "to_endpoints": to}
	}

	t.Run("written", func(t *testing.T) {
		f := setup(t)

		mustWritten(t, f.evidence(t, f.evidenceObject(t, lists([]string{canonicalA}, both, canonicalA)), f.confirmationObject(nil)))

		if got := f.receiptOnDisk(t); got.Schema != receiptSeveralSchema || !slices.Equal(got.InstalledEndpoints, both) {
			t.Fatalf("the receipt is %+v", got)
		}
	})

	t.Run("the node has failed over since", func(t *testing.T) {
		f := setup(t)
		f.writeRecord(t, f.dialling(canonicalB, newInvocation, newIncarnation, both...))

		mustWritten(t, f.evidence(t, f.evidenceObject(t, lists([]string{canonicalA}, both, canonicalA)), f.confirmationObject(nil)))

		if got := f.receiptOnDisk(t); got.EffectiveEndpoint != canonicalB {
			t.Fatalf("the receipt says the node is on %s, and it is on %s", got.EffectiveEndpoint, canonicalB)
		}
	})

	t.Run("the process does not know the list", func(t *testing.T) {
		f := setup(t)
		f.writeRecord(t, f.dialling(canonicalA, newInvocation, newIncarnation))

		o := f.evidence(t, f.evidenceObject(t, lists([]string{canonicalA}, both, canonicalA)), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonRecord)
	})

	t.Run("the migration moved it elsewhere", func(t *testing.T) {
		f := setup(t)

		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})

	for name, over := range map[string]map[string]any{
		"half of the pair":           {"from_endpoints": []string{canonicalA}},
		"the other half":             {"to_endpoints": both},
		"neither naming several":     lists([]string{canonicalA}, []string{canonicalA}, canonicalA),
		"a list without its `to`":    lists([]string{canonicalA}, []string{canonicalB, "http://127.0.0.1:7720"}, canonicalA),
		"a list without its `from`":  lists([]string{canonicalB}, both, canonicalA),
		"one controller twice":       lists([]string{canonicalA}, []string{canonicalA, canonicalA}, canonicalA),
		"a list that is not a list":  {"from_endpoints": canonicalA, "to_endpoints": both},
		"a spelling not canonical":   lists([]string{canonicalA}, []string{canonicalA, "127.0.0.1:7719"}, canonicalA),
		"a member spelled otherwise": {"From_Endpoints": both, "to_endpoints": both},
		"two null lists":             {"from_endpoints": json.RawMessage("null"), "to_endpoints": json.RawMessage("null")},
		"one null list":              {"from_endpoints": json.RawMessage("null"), "to_endpoints": both},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			f := setup(t)

			o := f.evidence(t, f.evidenceObject(t, over), f.confirmationObject(nil))
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
			f.noReceipt(t)
		})
	}
}

// THE LONGEST LIST A CONFIGURATION ADMITS FITS WHAT NAMES IT. A reader refuses a
// record or a receipt over its bound, and a node whose evidence it refuses is one no
// migration can then shorten the list of. The receipt is the indented one, naming a
// holder JSON escapes in full.
func TestTheLongestListAConfigurationAdmitsFitsItsEvidence(t *testing.T) {
	label := strings.Repeat("a", 63) + "."
	texts := make([]string, config.MaxServerAddrs)

	for i := range texts {
		addr := string(rune('a'+i)) + label[1:] + label + label + strings.Repeat("a", 61) + ":65535"
		if len(addr) != config.MaxServerAddrBytes {
			t.Fatalf("the longest address here is %d bytes, and the configuration admits %d", len(addr), config.MaxServerAddrBytes)
		}

		e, err := endpoint.Parse(addr, true)
		if err != nil {
			t.Fatalf("parse %s: %v", addr, err)
		}

		texts[i] = e.String()
	}

	set, err := parseControllerTexts(texts)
	if err != nil {
		t.Fatal(err)
	}

	node, deployment := strings.Repeat("n", 64), strings.Repeat("d", 32)

	record, err := json.Marshal(map[string]any{
		"schema": 2, "node": node, "deployment": deployment, "incarnation": newIncarnation, "invocation_id": newInvocation,
		"endpoint": texts[0], "endpoints": texts, "registered_at": "2026-09-10T12:00:00.123456789Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(record)+1 > maxRegistrationRecordBytes {
		t.Errorf("the record is %d bytes, over the reader's %d", len(record)+1, maxRegistrationRecordBytes)
	}

	if _, err := decodeRegistrationRecord(record); err != nil {
		t.Errorf("the record is refused: %v", err)
	}

	receipt, err := json.MarshalIndent(endpointReceipt{
		Run: strings.Repeat("<", maxHolderBytes), Node: node, Deployment: deployment, InstalledSHA256: strings.Repeat("a", 64),
		EffectiveEndpoint: texts[0], InvocationID: newInvocation, Incarnation: newIncarnation,
		WrittenAt: "2026-09-10T12:00:00.123456789Z",
	}.withInstalled(set), "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if len(receipt)+1 > maxReceiptBytes {
		t.Errorf("the receipt is %d bytes, over the reader's %d", len(receipt)+1, maxReceiptBytes)
	}

	if _, err := decodeEndpointReceipt(receipt); err != nil {
		t.Errorf("the receipt is refused: %v", err)
	}
}

// THE TWO SCHEMAS ARE EXACT, in the record and the receipt: a list only under
// schema 2, schema 2 only with a list holding the controller beside it.
func TestARecordsAndAReceiptsSchemasAreExact(t *testing.T) {
	both := []any{canonicalA, canonicalB}
	f := newEndpointFixture(t)

	for name, tc := range map[string]struct {
		over map[string]any
		want string
	}{
		"a list under schema 1":       {map[string]any{"endpoints": both}, `"endpoints" is not one the node writes`},
		"schema 2 without a list":     {map[string]any{"schema": 2}, `"endpoints" is missing`},
		"a list of one":               {map[string]any{"schema": 2, "endpoints": []any{canonicalA}}, "names 1 controllers"},
		"a list without its endpoint": {map[string]any{"schema": 2, "endpoint": "http://127.0.0.1:7720", "endpoints": both}, "does not name"},
		"one controller twice":        {map[string]any{"schema": 2, "endpoints": []any{canonicalA, canonicalA}}, "named twice"},
		"a spelling not canonical":    {map[string]any{"schema": 2, "endpoints": []any{canonicalA, "127.0.0.1:7719"}}, "endpoints: "},
		"a list of something else":    {map[string]any{"schema": 2, "endpoints": []any{1, 2}}, "not a list of strings"},
		"a list that is null":         {map[string]any{"schema": 2, "endpoints": nil}, "not a list of strings"},
		"a schema from later":         {map[string]any{"schema": 3, "endpoints": both}, "schema is 3"},
	} {
		t.Run("the record: "+name, func(t *testing.T) {
			rec := f.record(map[string]any{"endpoint": canonicalA})
			for k, v := range tc.over {
				rec[k] = v
			}

			body, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := decodeRegistrationRecord(body); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: %v, want %q", body, err, tc.want)
			}
		})
	}

	body, err := json.Marshal(f.record(map[string]any{"schema": 2, "endpoint": canonicalA, "endpoints": both}))
	if err != nil {
		t.Fatal(err)
	}

	rec, err := decodeRegistrationRecord(body)
	if err != nil {
		t.Fatalf("a schema 2 record was refused: %v", err)
	}

	if set, err := rec.controllers(); err != nil || len(set) != 2 {
		t.Fatalf("the record dials %v (%v)", set, err)
	}

	receipt := func(over map[string]any) []byte {
		m := map[string]any{
			"schema": 2, "run": receiptRun, "node": "node-a", "deployment": f.deployment,
			"installed_sha256": strings.Repeat("ab", 32), "installed_endpoints": both, "effective_endpoint": canonicalA,
			"invocation_id": newInvocation, "incarnation": newIncarnation, "written_at": "2026-11-04T00:00:00Z",
		}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}

		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	if got, err := decodeEndpointReceipt(receipt(nil)); err != nil || got.Schema != receiptSeveralSchema {
		t.Fatalf("a schema 2 receipt = %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		over map[string]any
		want string
	}{
		"schema 1 with a list":        {map[string]any{"schema": 1}, `"installed_endpoint" is missing`},
		"schema 1 with both forms":    {map[string]any{"schema": 1, "installed_endpoint": canonicalA}, `"installed_endpoints" is not one billet writes`},
		"schema 2 with both forms":    {map[string]any{"installed_endpoint": canonicalA}, `"installed_endpoint" is not one billet writes`},
		"no list":                     {map[string]any{"installed_endpoints": nil}, `"installed_endpoints" is missing`},
		"a list of one":               {map[string]any{"installed_endpoints": []any{canonicalA}}, "names 1 controllers"},
		"the effective one not in it": {map[string]any{"effective_endpoint": "http://127.0.0.1:7720"}, "does not name"},
		"one controller twice":        {map[string]any{"installed_endpoints": []any{canonicalA, canonicalA}}, "named twice"},
		"a schema from later":         {map[string]any{"schema": 3}, "schema is 3"},
	} {
		t.Run("the receipt: "+name, func(t *testing.T) {
			if got, err := decodeEndpointReceipt(receipt(tc.over)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%+v: %v, want %q", got, err, tc.want)
			}
		})
	}
}

// ONE CONTROLLER TWICE UNDER TWO SPELLINGS, which the configuration's comparison of
// strings cannot see and the node would refuse at its start, is refused here too.
func TestAControllerNamedTwiceUnderTwoSpellingsIsRefused(t *testing.T) {
	cfg := &config.Config{Node: &config.NodeConfig{ServerAddrs: []string{"10.0.0.4:7717", "10.0.0.4:07717"}}}

	if _, _, err := nodeEndpointsOf(cfg); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Fatalf("nodeEndpointsOf = %v, want the controller named twice", err)
	}
}

// A RETIREMENT LEAVES ONE CONTROLLER, so a node naming several is refused at the
// request, naming what to do, before the chain that follows one endpoint is read.
func TestARetirementRefusesANodeNamingSeveralControllers(t *testing.T) {
	inspected := nodeTLSFixture(t, true)

	config1, err := os.ReadFile(inspected.configPath)
	if err != nil {
		t.Fatal(err)
	}

	inspected.writeConfig(t, strings.Replace(string(config1), "  server_addr: 10.0.0.5:7717\n",
		"  server_addrs: [10.0.0.5:7717, 10.0.0.6:7717]\n", 1))

	body, err := json.Marshal(inspected.report(t))
	if err != nil {
		t.Fatal(err)
	}

	doc, err := decodeReport(body)
	if err != nil {
		t.Fatal(err)
	}

	// THE BINDING IS THE RELEASE'S TO SAY AND NOT WHAT THIS IS ABOUT.
	doc.m["config_binding"] = true

	_, r := judgeNodeChain(retireMode{}, &retireInput{}, &retireEnvelope{Host: "node-1", inspect: doc}, "node-1", "",
		rollout.Registration{}, nil, nil, nil)
	if r == nil || r.Reason != retireReasonEndpoint || !strings.Contains(r.Next, "survivor alone") {
		t.Fatalf("judgeNodeChain = %+v, want the endpoint refusal naming the survivor", r)
	}

	// AND THE ONE-ENDPOINT QUESTION SAYS WHY THERE IS NO ANSWER.
	cfg := &config.Config{Node: &config.NodeConfig{ServerAddrs: []string{"10.0.0.4:7717", "10.0.0.5:7717"}}}
	if _, hasNode, err := nodeEndpointOf(cfg); !errors.Is(err, errSeveralControllers) || !hasNode {
		t.Fatalf("nodeEndpointOf = (%v, %v), want errSeveralControllers", hasNode, err)
	}

	if got := installedEndpoints(cfg, true); len(got) != 2 {
		t.Fatalf("installedEndpoints = %v, want the two", got)
	}

	one := &config.Config{Node: &config.NodeConfig{ServerAddr: "10.0.0.4:7717"}}
	if got := installedEndpoints(one, true); got != nil {
		t.Fatalf("installedEndpoints of one controller = %v, want none, so the report is what it always was", got)
	}
}
