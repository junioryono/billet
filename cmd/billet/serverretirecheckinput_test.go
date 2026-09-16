package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

func TestNodeConfigInputStrictlyBindsRenderingAndOperationDocument(t *testing.T) {
	m := retireMode{run: "ci-1", retiringHost: "control-a", transition: retireTestID}
	operations := `{ "filesystem": [], "services": [], "units": [] }`
	rendering := "node:\n  name: node-a\n"
	in := retireNodeConfigInput{Schema: 1, Run: m.run, Retiring: m.retiringHost, TransitionID: m.transition,
		Rendering: rendering, RenderingSHA256: retirement.Digest([]byte(rendering)), Operations: json.RawMessage(operations)}
	body, err := json.Marshal(in)
	mustOK(t, err)
	// Marshal compacts a RawMessage. Restore exact transport whitespace to prove
	// the decoder preserves the operation document rather than remarshal it.
	body = []byte(strings.Replace(string(body), `{"filesystem":[],"services":[],"units":[]}`, operations, 1))
	got, _, r := decodeRetireNodeConfig(body, m)
	if r != nil || string(got.Operations) != operations {
		t.Fatalf("input lost the exact operation bytes: %q %+v", got.Operations, r)
	}
	for _, changed := range []string{
		strings.Replace(string(body), `"schema":1`, `"schema":2`, 1),
		strings.Replace(string(body), `"schema":1`, `"Schema":1`, 1),
		strings.Replace(string(body), `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(string(body), `"schema":1`, `"unknown":true,"schema":1`, 1),
		strings.Replace(string(body), `"run":"ci-1"`, `"run":"ci-2"`, 1),
		strings.Replace(string(body), `"retiring":"control-a"`, `"retiring":"control-b"`, 1),
		strings.Replace(string(body), retireTestID, strings.Repeat("f", 32), 1),
		strings.Replace(string(body), in.RenderingSHA256, strings.Repeat("0", 64), 1),
		strings.Replace(string(body), `"units": []`, `"units": null`, 1),
		strings.Replace(string(body), `"units": []`, `"Units": []`, 1),
		strings.Replace(string(body), `"units": []`, `"units": [], "units": []`, 1),
		string(body) + "}", string(body[:len(body)-1]),
	} {
		if changed == string(body) {
			t.Fatal("invalid-input witness did not change bytes")
		}
		if _, _, r := decodeRetireNodeConfig([]byte(changed), m); r == nil || r.Reason != retireReasonInput {
			t.Fatalf("invalid or mismatched input admitted: %+v", r)
		}
	}
}

func TestNodeConfigModeRejectsMutationAndFreshRequestOperands(t *testing.T) {
	base := retireMode{checkNodeConfig: true, input: "-", configPath: "/etc/billet/billet.yaml", run: "ci-1", retiringHost: "control-a",
		transition: retireTestID, expectedHolder: "ci-1", expectedGuard: strings.Repeat("a", 32)}
	if r := checkRetireCombination(base); r != nil {
		t.Fatal(r)
	}
	for _, mutate := range []func(*retireMode){
		func(m *retireMode) { m.reserve = true },
		func(m *retireMode) { m.reportAgeFlagged = true },
		func(m *retireMode) { m.abandon = true },
		func(m *retireMode) { m.completeRow = true },
		func(m *retireMode) { m.acknowledge = true },
		func(m *retireMode) { m.dryRun = true },
		func(m *retireMode) { m.requested = true },
		func(m *retireMode) { m.serverOnly = true },
		func(m *retireMode) { m.survivorFlagged = true },
		func(m *retireMode) { m.failoverVerified = true },
		func(m *retireMode) { m.reservationFresh = true },
		func(m *retireMode) { m.sharedAddresses = []string{"10.0.0.1"} },
		func(m *retireMode) { m.survivorHost = "control-b" },
		func(m *retireMode) { m.installedSHA = strings.Repeat("a", 64) },
		func(m *retireMode) { m.environmentFile = "/etc/server.env" },
		func(m *retireMode) { m.input = "" },
		func(m *retireMode) { m.transition = "" },
		func(m *retireMode) { m.expectedGuard = "" },
		func(m *retireMode) { m.expectedHolder = "another" },
	} {
		m := base
		mutate(&m)
		if r := checkRetireCombination(m); r == nil || r.Reason != retireReasonCombination {
			t.Fatalf("incompatible mode admitted: %+v", m)
		}
	}
}
