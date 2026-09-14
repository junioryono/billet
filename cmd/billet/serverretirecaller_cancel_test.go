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
