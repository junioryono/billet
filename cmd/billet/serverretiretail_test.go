package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// useEndpointReceipt points this process's receipt at a directory the test
// owns and stands root in for its owner, the way the inspector's own tests do:
// the path and the ownership a receipt requires are the machine's, and neither
// is a thing a test suite can have.
func useEndpointReceipt(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "registration")
	mustOK(t, os.Mkdir(dir, 0o700))

	savedPath, savedOwner := receiptPath, receiptOwnerOf
	receiptPath = filepath.Join(dir, "endpoint-migration.json")
	receiptOwnerOf = func(os.FileInfo) (uint32, bool) { return 0, true }

	t.Cleanup(func() { receiptPath, receiptOwnerOf = savedPath, savedOwner })

	return receiptPath
}

// tailWrites records the retirement's own durable publishes in order, with the
// guard's marker and the journal's acknowledgement AS THEY STAND WHEN EACH
// WRITE IS MADE. It is the witness for the tail's ordering: no schedule can
// observe the moment between two sequential writes, so the order is read from
// what each write found.
type tailWrite struct {
	file    string
	rowDone bool
	marker  bool
}

func recordTailWrites(t *testing.T, f *requestFixture) *[]tailWrite {
	t.Helper()

	seen := &[]tailWrite{}
	saved := retirement.Publishing

	retirement.Publishing = func(path string) error {
		w := tailWrite{file: filepath.Base(path)}

		j, presence, err := retirement.ReadJournal()
		mustOK(t, err)

		if presence == retirement.JournalPresent {
			w.rowDone = j.RowDone
		}

		w.marker = f.guard.record(t).Transition != nil
		*seen = append(*seen, w)

		return nil
	}

	t.Cleanup(func() { retirement.Publishing = saved })

	return seen
}

// THE TAIL'S ORDER IS WHAT MAKES ITS CRASH WINDOWS RECOVERABLE. The marker is
// what keeps the guard from being released under a retirement that still owes
// the ledger a row, so it is cleared only once the journal acknowledges the
// row; and `settled` is written after the marker, so the one state a crash can
// leave — an unsettled journal with no marker — is the one the guard's
// takeover rule reads as an unfinished tail.
func TestTheTailClearsTheMarkerAfterTheRowAndSettlesAfterTheMarker(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	writes := recordTailWrites(t, f)

	out, code := f.request(t, f.input(t, nil))
	m := retiredAnswer(t, out, code)

	if m["marker"] != retireMarkerCleared || m["completed_by"] != requestRetiring {
		t.Fatalf("the tail's answer: %s", out)
	}

	compareFixture(t, "server-retire", "retired-settled", out)

	// THE LAST TWO JOURNAL WRITES ARE THE TAIL'S: the acknowledgement, made
	// with the marker still in place and the row not yet acknowledged on disk,
	// and `settled`, made with the marker already gone.
	journals := make([]tailWrite, 0, len(*writes))

	for _, w := range *writes {
		if w.file == "journal.json" {
			journals = append(journals, w)
		}
	}

	if len(journals) < 2 {
		t.Fatalf("the tail published no journal: %+v", *writes)
	}

	acknowledgement, settled := journals[len(journals)-2], journals[len(journals)-1]

	switch {
	case acknowledgement.rowDone:
		t.Fatalf("the acknowledgement was written over a journal that already had one: %+v", journals)
	case !acknowledgement.marker:
		t.Fatalf("the marker was cleared before the row was acknowledged: %+v", journals)
	case !settled.rowDone:
		t.Fatalf("`settled` was written before the row was acknowledged: %+v", journals)
	case settled.marker:
		t.Fatalf("`settled` was written while the marker was still there: %+v", journals)
	}

	// AND THE STATUS SAID `done` BEFORE EITHER, so a reader that sees the
	// journal settled never sees a status below it.
	status := -1

	for i, w := range *writes {
		if w.file == filepath.Base(retirement.StatusPath()) {
			status = i
		}
	}

	if status == -1 {
		t.Fatalf("the tail published no status: %+v", *writes)
	}
}

// A CRASH BETWEEN THE LEDGER'S ROW AND THE JOURNAL'S ACKNOWLEDGEMENT leaves
// the row done and the journal not saying so, which is recoverable: the next
// converge's tail asks the ledger again and is told `already`. What must NOT
// happen is the marker being cleared over a journal that has not recorded the
// row, because the guard would then be releasable while the journal still
// reads unsettled.
func TestTheTailKeepsTheMarkerWhenTheAcknowledgementCannotBeWritten(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The journal's acknowledgement is the FIRST publish made while the row is
	// already done, which is exactly the write this fails.
	saved := retirement.Publishing
	failed := false

	retirement.Publishing = func(path string) error {
		if failed || filepath.Base(path) != "journal.json" {
			return nil
		}

		j, presence, err := retirement.ReadJournal()
		mustOK(t, err)

		if presence != retirement.JournalPresent || j.Phase != retirement.PhaseDone || j.RowDone {
			return nil
		}

		failed = true

		return os.ErrPermission
	}

	t.Cleanup(func() { retirement.Publishing = saved })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonJournal || code != exitUnknown || m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("a journal that could not record the completed row: %s", out)
	}

	if !failed {
		t.Fatal("the acknowledgement was never attempted, so this case proved nothing")
	}

	// THE ROW IS DONE and the journal does not say so.
	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementDone {
			t.Fatalf("the row: %+v (present %v)", r, present)
		}
	})

	j, presence, err := retirement.ReadJournal()
	if err != nil || presence != retirement.JournalPresent || j.RowDone || j.Settled {
		t.Fatalf("the journal: %+v %d %v", j, presence, err)
	}

	// AND THE MARKER IS STILL THERE, so nothing releases this guard.
	if f.guard.record(t).Transition == nil {
		t.Fatal("the marker was cleared over a journal that had not acknowledged the row")
	}

	if err := guardRun(t, "release", "--holder", requestRun); err == nil {
		t.Fatal("the guard of an unsettled retirement was released")
	}
}

// A LEDGER THIS HOST CANNOT REACH IS NOT A FAILED RETIREMENT. The transition
// is complete on the host — the server is stopped, the identity archived, the
// configuration rewritten — and what is left is a row the survivor can write
// instead, from the completion document this answer carries.
func TestTheTailLeavesThePendingRowToTheSurvivor(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// THE LEDGER GOES OUT OF REACH AFTER THE INTENT, which is the frozen
	// controller's case: the request read the row through the configuration's
	// DSN, and the tail reads it through the journal's locator, by which time
	// the server's environment is gone.
	saved := retireBeforeRename
	retireBeforeRename = func() {
		t.Setenv("BILLET_STATE_DSN", "postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable")
	}

	t.Cleanup(func() { retireBeforeRename = saved })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowPending {
		t.Fatalf("a tail whose ledger is out of reach: %s", out)
	}

	why, ok := m["row_why"].(string)
	if !ok || !strings.Contains(why, "did not answer") {
		t.Fatalf("the pending row does not say why: %s", out)
	}

	if m["marker"] != retireMarkerKept || m["settled"] != false {
		t.Fatalf("a pending row settled the retirement: %s", out)
	}

	// THE COMPLETION THE SURVIVOR TAKES is this retirement's, field for field.
	completion, ok := m["completion"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no completion: %s", out)
	}

	// The deployment reads as the fixture's canned identity because the harness
	// normalises the minted one, which is what makes these answers comparable
	// between machines.
	if completion["retiring"] != requestRetiring || completion["survivor"] != requestSurvivor ||
		completion["transition_id"] != retireTestID || completion["deployment"] != retireTestIdentity {
		t.Fatalf("the completion: %v", completion)
	}

	// AND THE GUARD IS NOT RELEASABLE while the tail owes the ledger a row.
	if err := guardRun(t, "release", "--holder", requestRun); err == nil {
		t.Fatal("the guard of a retirement with a pending row was released")
	}
}

// THE SAME ANSWER, staged so its diagnostic is the same on every machine: the
// variable the locator names is not set at all, which is what the rewrite
// leaves behind on a host whose server environment went with the server. This
// is the fixture the role's parser is written against.
func TestTheTailsPendingAnswerIsTheRolesFixture(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	saved := retireBeforeRename
	retireBeforeRename = func() { t.Setenv("BILLET_STATE_DSN", "") }

	t.Cleanup(func() { retireBeforeRename = saved })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowPending {
		t.Fatalf("a tail whose locator names an unset variable: %s", out)
	}

	compareFixture(t, "server-retire", "retired-pending", out)
}

// A RETAINED NODE'S RECEIPT IS REWRITTEN BY THE TAIL. The node was restarted
// by the phase before it, so its invocation is new and the configuration it
// loaded is the serverless one: the receipt from before the retirement names
// an invocation that is gone and a digest that has changed, and the fleet's
// next check refuses on a receipt like that. The converge that retires this
// host ends at the retirement and never reaches the task that would otherwise
// do it.
func TestTheTailRewritesTheRetainedNodesReceipt(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	record := useRegistrationRecord(t)

	f.svc.onStart = func(unit string) {
		if unit == nodeUnit {
			writeRegistrationRecord(t, record, f.identity, retainedEndpoint)
		}
	}

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	m := retiredAnswer(t, out, code)
	if m["receipt"] != retireReceiptWritten {
		t.Fatalf("the tail did not rewrite the node's receipt: %s", out)
	}

	// AND IT NAMES WHAT THE HOST NOW RUNS: the configuration the rewrite
	// installed, and the invocation the restart produced.
	var receipt endpointReceipt
	mustOK(t, json.Unmarshal([]byte(mustRead(t, receiptPath)), &receipt))

	if receipt.InstalledSHA256 != retirement.Digest([]byte(f.rendering(t))) {
		t.Fatalf("the receipt names another configuration: %+v", receipt)
	}

	if receipt.Run != requestRun || receipt.EffectiveEndpoint != retainedEndpoint {
		t.Fatalf("the receipt: %+v", receipt)
	}
}

// THE STATUS IS THE PUBLICATION OF THE JOURNAL, and the tail repairs it: it is
// the file that closes the authority to every ordinary writer, so a retired
// host whose status went missing is a host the next `ca rotate` would be
// admitted on. The journal is the record; this run knows what the status
// should say.
func TestTheTailRepublishesAStatusThatWentMissing(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The status goes at the flush that follows ITS OWN `done` publication,
	// which is the last thing the transition writes before the tail reads it.
	removed := false

	retirement.SyncingDir = func(string) error {
		if removed {
			return nil
		}

		st, presence, err := retirement.ReadStatus()
		mustOK(t, err)

		if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
			return nil
		}

		removed = true

		return os.Remove(retirement.StatusPath())
	}

	t.Cleanup(func() { retirement.SyncingDir = nil })

	out, code := f.request(t, f.input(t, nil))
	retiredAnswer(t, out, code)

	if !removed {
		t.Fatal("the status was never removed, so this case proved nothing")
	}

	st, presence, err := retirement.ReadStatus()
	if err != nil || presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
		t.Fatalf("the tail left the status: %+v %d %v", st, presence, err)
	}
}
