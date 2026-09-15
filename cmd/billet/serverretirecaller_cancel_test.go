package main

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// A later converge must adopt the old run's row before it can cancel it.
func TestTheRetireCallerCancelsAPreviousRunsReservation(t *testing.T) {
	f := newRequestFixture(t)
	mustOK(t, guardRun(t, "release", "--holder", requestRun))
	mustHold(t, "ci-0")

	out, code := f.run(t, "", "--reserve", "--run", "ci-0", "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor)
	if m := retireAnswer(t, out); code != 0 || m["outcome"] != retireOutcomeReserved || m["run"] != "ci-0" {
		t.Fatalf("the earlier run did not reserve: %s", out)
	}

	mustOK(t, guardRun(t, "release", "--holder", "ci-0"))
	mustHold(t, requestRun)
	before := mustRead(t, f.cfg)

	out, code = f.run(t, "", "--dry-run", "--retiring-host", requestRetiring,
		"--expected-holder", requestRun, "--expected-guard", f.guard.record(t).ID)
	m := assertRetireRoute(t, out, code, "cancel", "inventory no longer requests")
	row := asMap(m["row"])
	if row["run"] != "ci-0" || row["retiring"] != requestRetiring || row["survivor"] != requestSurvivor ||
		row["transition_id"] != retireTestID || m["journal"] != nil || m["marker"] != nil {
		t.Fatalf("the classifier did not observe the older unstarted reservation: %s", out)
	}
	expectRetire(t, out, code, "dry-run-cancel-previous-run", retireOutcomeReported, "")

	out, code = f.run(t, "", "--abandon-reservation", "--run", requestRun, "--retiring-host", requestRetiring)
	expectRetire(t, out, code, "refused-abandon-run", retireOutcomeRefused, retireReasonReserved)
	f.pgLedger(t, func(db *state.DB) {
		stood, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)
		if !present || stood.Run != "ci-0" || stood.TransitionID != retireTestID {
			t.Fatalf("unadopted abandonment changed the reservation: %+v", stood)
		}
	})

	out, code = f.run(t, "", "--reserve", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--installed-sha256", f.installedSHA(t))
	m = expectRetire(t, out, code, "adopted", retireOutcomeAdopted, "")
	if m["run"] != requestRun || m["retiring"] != row["retiring"] || m["survivor"] != row["survivor"] ||
		m["transition_id"] != row["transition_id"] || m["reserved_at"] != row["reserved_at"] {
		t.Fatalf("adoption changed the recorded binding: %s", out)
	}
	f.pgLedger(t, func(db *state.DB) {
		stood, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)
		if !present || stood.Run != requestRun || stood.State != state.RetirementReserved ||
			stood.TransitionID != retireTestID || stood.ReservedAt != row["reserved_at"] {
			t.Fatalf("adoption did not bind the existing row to this run: %+v", stood)
		}
	})

	out, code = f.run(t, "", "--abandon-reservation", "--run", requestRun, "--retiring-host", requestRetiring)
	expectRetire(t, out, code, "abandoned", retireOutcomeAbandoned, "")
	f.pgLedger(t, func(db *state.DB) {
		_, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)
		if present {
			t.Fatal("confirmed cancellation left its reservation")
		}
	})
	out, code = f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
	m = assertRetireRoute(t, out, code, "ordinary", "")
	if m["journal"] != nil || m["marker"] != nil || m["status_presence"] != "absent" || m["stage"] != "absent" ||
		mustRead(t, f.cfg) != before || len(f.svc.trace) != 0 {
		t.Fatalf("cancellation started retirement or changed the installed host: %s, %v", out, f.svc.trace)
	}
}

// The survivor's acknowledgement leaves the marker until a local continuation.
func TestTheRetireCallerFixtureSettlesAfterSurvivorAcknowledgement(t *testing.T) {
	f := newRequestFixture(t)
	survivorDir := t.TempDir()
	writeFile(t, state.DeploymentIDPath(survivorDir), mustRead(t, state.DeploymentIDPath(f.stateDir)), 0o600)
	survivorConfig := writeRetirePostgresConfig(t, survivorDir)
	f.reserve(t)

	saved := retireBeforeRename
	retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }
	t.Cleanup(func() { retireBeforeRename = saved })
	out, code := f.request(t, f.input(t, nil))
	m := expectRetire(t, out, code, "retired-pending", retireOutcomeRetired, "")
	if m["row"] != retireRowPending || m["settled"] != false || f.guard.record(t).Transition == nil {
		t.Fatalf("the request did not leave a pending row and marker: %s", out)
	}
	completion := strings.ReplaceAll(string(mustMarshal(t, m["completion"])), retireTestIdentity, f.identity)
	retiringRoot := retirement.Root
	closedStatus := mustRead(t, retirement.StatusPath())

	var answer string
	if !t.Run("survivor", func(t *testing.T) {
		useRetirementRoot(t)
		newGuardFixture(t)
		preparedHost(t)
		mustHold(t, requestRun)
		t.Setenv("BILLET_STATE_DSN", f.dsn)
		var exit int
		answer, exit = f.run(t, completion, "--complete-row", "--run", requestRun,
			"--as-host", requestSurvivor, "--completion", "-", "--config", survivorConfig)
		if a := retireAnswer(t, answer); exit != 0 || a["row"] != "done" || a["completed_by"] != requestSurvivor {
			t.Fatalf("the survivor did not complete the row: %s", answer)
		}
	}) {
		t.Fatal("the survivor could not produce its completion answer")
	}
	if retirement.Root != retiringRoot || mustRead(t, retirement.StatusPath()) != closedStatus {
		t.Fatal("the survivor did not preserve the retiring host's closed authority")
	}

	answer = strings.ReplaceAll(answer, retireTestIdentity, f.identity)
	out, code = f.run(t, answer, "--acknowledge-row", "--run", requestRun,
		"--retiring-host", requestRetiring, "--answer", "-")
	expectRetire(t, out, code, "acknowledged", retireOutcomeAcknowledged, "")
	j, presence, err := retirement.ReadJournal()
	mustOK(t, err)
	if presence != retirement.JournalPresent || !j.RowDone || j.Settled || j.CompletedBy != requestSurvivor ||
		f.guard.record(t).Transition == nil {
		t.Fatalf("acknowledgement settled or failed to record the survivor: %+v", j)
	}

	retiredUnits(t, f)
	traceBefore := len(f.svc.trace)
	document := string(mustMarshal(t, map[string]any{
		"schema": retireInputSchema,
		"round":  map[string]any{"id": requestRun + "-" + requestStarted, "started_at": requestStarted},
		"self":   f.selfReport(t), "survivor": nil, "nodes": map[string]any{}, "desired": nil,
	}))
	out, code = f.run(t, document, "--input", "-", "--run", requestRun,
		"--retiring-host", requestRetiring, "--survivor-host", requestSurvivor)
	m = expectRetire(t, out, code, "retired-after-survivor-ack", retireOutcomeRetired, "")
	if m["settled"] != true || m["completed_by"] != requestSurvivor || m["marker"] != retireMarkerCleared ||
		f.guard.record(t).Transition != nil || len(f.svc.trace) != traceBefore {
		t.Fatalf("the final continuation did not settle only the pending tail: %s", out)
	}
	j, presence, err = retirement.ReadJournal()
	mustOK(t, err)
	if presence != retirement.JournalPresent || !j.Settled || !j.RowDone || j.CompletedBy != requestSurvivor {
		t.Fatalf("the final continuation did not durably settle: %+v", j)
	}
	if _, err := os.Lstat(f.cfg); !os.IsNotExist(err) {
		t.Fatalf("the final continuation recreated the retired configuration: %v", err)
	}
}

// Completion is historical evidence bound to the document, not a promise of
// remote health. Adding a fresh node-health gate to completion or acknowledgement
// breaks this handoff; dropping local entry proof instead breaks settlement.
func TestRetainedRetirementAcknowledgesHistoricalCompletionAfterNodeFailure(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	survivorDir := t.TempDir()
	writeFile(t, state.DeploymentIDPath(survivorDir), mustRead(t, state.DeploymentIDPath(f.stateDir)), 0o600)
	survivorConfig := writeRetirePostgresConfig(t, survivorDir)
	f.reserve(t)

	saved := retireBeforeRename
	retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }
	t.Cleanup(func() { retireBeforeRename = saved })
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowPending ||
		m["receipt"] != retireReceiptWritten || m["settled"] != false || m["marker"] != retireMarkerKept {
		t.Fatalf("local proof and receipt did not reach a pending completion: %s", out)
	}
	j := requireRetireJournal(t)
	completion := strings.ReplaceAll(string(mustMarshal(t, m["completion"])), retireTestIdentity, f.identity)
	document, err := retirement.DecodeCompletion([]byte(completion))
	mustOK(t, err)
	if document != retirement.CompletionOf(j) || j.DoneAt == "" || j.RowDone || j.Settled {
		t.Fatalf("pending document did not bind the historically proved retirement: %+v %+v", document, j)
	}
	receiptBefore := mustRead(t, receiptPath)
	retiringRoot := retirement.Root
	closedStatus := mustRead(t, retirement.StatusPath())

	// The node fails only after done was proved and the pending answer issued,
	// during the work that carries the same document to the survivor.
	f.manager.set(nodeUnit, "ActiveState", "failed")
	f.manager.set(nodeUnit, "MainPID", "0")
	var answer string
	if !t.Run("survivor records historical completion", func(t *testing.T) {
		useRetirementRoot(t)
		newGuardFixture(t)
		preparedHost(t)
		mustHold(t, requestRun)
		t.Setenv("BILLET_STATE_DSN", f.dsn)
		var exit int
		answer, exit = f.run(t, completion, "--complete-row", "--run", requestRun,
			"--as-host", requestSurvivor, "--completion", "-", "--config", survivorConfig)
		if a := retireAnswer(t, answer); exit != 0 || a["row"] != "done" || a["completed_by"] != requestSurvivor {
			t.Fatalf("survivor refused valid historical completion after node failure: %s", answer)
		}
	}) {
		t.Fatal("survivor did not complete the historical row")
	}
	if retirement.Root != retiringRoot || mustRead(t, retirement.StatusPath()) != closedStatus {
		t.Fatal("survivor changed the retiring host's closed authority")
	}

	answer = strings.ReplaceAll(answer, retireTestIdentity, f.identity)
	out, code = f.run(t, answer, "--acknowledge-row", "--run", requestRun,
		"--retiring-host", requestRetiring, "--answer", "-")
	m = retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeAcknowledged || m["completed_by"] != requestSurvivor {
		t.Fatalf("local acknowledgement refused valid historical completion: %s", out)
	}
	afterAck := requireRetireJournal(t)
	if !afterAck.RowDone || afterAck.CompletedBy != requestSurvivor || afterAck.Settled ||
		retirement.CompletionOf(afterAck) != document || f.guard.record(t).Transition == nil {
		t.Fatalf("acknowledgement changed the historical binding or settled: %+v", afterAck)
	}

	input := string(mustMarshal(t, map[string]any{
		"schema": retireInputSchema,
		"round":  map[string]any{"id": requestRun + "-" + requestStarted, "started_at": requestStarted},
		"self":   f.selfReport(t), "survivor": nil, "nodes": map[string]any{}, "desired": nil,
	}))
	out, code = f.run(t, input, "--input", "-", "--run", requestRun,
		"--retiring-host", requestRetiring, "--survivor-host", requestSurvivor)
	m = retireAnswer(t, out)
	if code != exitRefused || m["reason"] != retireReasonPostcondition ||
		!strings.Contains(whyOf(m), nodeUnit+" is failed") || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("settling continuation did not name the failed node: %s", out)
	}
	after := requireRetireJournal(t)
	if !after.RowDone || after.CompletedBy != requestSurvivor || after.Settled ||
		retirement.CompletionOf(after) != document || f.guard.record(t).Transition == nil ||
		mustRead(t, receiptPath) != receiptBefore {
		t.Fatalf("refused settlement changed historical completion or the receipt: %+v", after)
	}
	t.Setenv("BILLET_STATE_DSN", f.dsn)
	f.stateDir = j.Archive
	f.pgLedger(t, func(db *state.DB) {
		row, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)
		if !present || row.State != state.RetirementDone || row.CompletedBy != requestSurvivor ||
			row.TransitionID != document.TransitionID || row.ReservedAt != document.Reservation {
			t.Fatalf("settlement refusal lost historical ledger completion: %+v", row)
		}
	})
	if err := guardRun(t, "release", "--holder", requestRun); err == nil {
		t.Fatal("failed local settlement released the guard for ordinary work")
	}
}
