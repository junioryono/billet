package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	status  retirement.Phase
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

		st, statusPresence, err := retirement.ReadStatus()
		mustOK(t, err)

		if statusPresence == retirement.StatusPresent {
			w.status = st.Phase
		}

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

	// AND THE PUBLISHED STATUS ALREADY SAID `done` AT BOTH, so no reader sees a
	// journal past a status that is behind it.
	if acknowledgement.status != retirement.PhaseDone || settled.status != retirement.PhaseDone {
		t.Fatalf("the tail's journal writes ran under a status that was not done: %+v", journals)
	}

	// The status is PUBLISHED before either of them, and the phases before it
	// were published under the status of their own time: `intent` opens the
	// sequence and `stopped` closes the authority.
	statuses := make([]retirement.Phase, 0, len(*writes))

	for _, w := range *writes {
		if w.file == filepath.Base(retirement.StatusPath()) {
			statuses = append(statuses, w.status)
		}
	}

	if len(statuses) != 3 || statuses[0] != "" || statuses[1] != retirement.PhaseIntent ||
		statuses[2] != retirement.PhaseStopped {
		t.Fatalf("the statuses this retirement published were written over: %v", statuses)
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
	if !ok || !strings.Contains(why, "could not be reached") {
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
			restartedNode(t, f, record, retainedEndpoint)
		}
	}

	// THE HOST ALREADY HAS A RECEIPT, written before the retirement by the
	// ordinary converge: it names the invocation the node ran under then and
	// the configuration it loaded, and both are about to change. A test that
	// started from nothing would prove the tail can CREATE a receipt and say
	// nothing about the one it must replace.
	// THE RECEIPT OF THE TIME, coherent with every report the request carries:
	// the configuration this host had before the rewrite, and the invocation
	// it was running under before the restart.
	before := endpointReceipt{
		Schema: 1, Run: "ci-0", Node: "node-a", Deployment: f.identity,
		InstalledSHA256: f.installedSHA(t), InstalledEndpoint: retainedEndpoint,
		EffectiveEndpoint: retainedEndpoint, InvocationID: retainedInvocation,
		Incarnation: retainedIncarnation, WrittenAt: "2026-09-10T10:00:00Z",
	}

	body, err := json.Marshal(before)
	mustOK(t, err)
	writeFile(t, receiptPath, string(body)+"\n", 0o600)

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))

	m := retiredAnswer(t, out, code)
	if m["receipt"] != retireReceiptWritten {
		t.Fatalf("the tail did not rewrite the node's receipt: %s", out)
	}

	// AND IT NAMES WHAT THE HOST NOW RUNS: the configuration the rewrite
	// installed and the invocation the restart produced, neither of them the
	// ones the receipt carried before.
	var receipt endpointReceipt
	mustOK(t, json.Unmarshal([]byte(mustRead(t, receiptPath)), &receipt))

	if receipt.InstalledSHA256 != retirement.Digest([]byte(f.rendering(t))) ||
		receipt.InstalledSHA256 == before.InstalledSHA256 {
		t.Fatalf("the receipt names another configuration: %+v", receipt)
	}

	// THE INVOCATION THE RESTART PRODUCED, which is not the one the receipt
	// carried and not the one every report names.
	if receipt.InvocationID != retainedRestartInvocation {
		t.Fatalf("the receipt names another invocation: %+v", receipt)
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

// THE TAIL OPENS THE LEDGER THE ONE WAY A RETIRED HOST MAY. No schedule can
// witness the difference from the outside — an admin open behaves exactly like
// a completion whenever the survivor happens to be holding the controller
// exclusion, which is every ordinary run — so the witness is structural: this
// file names `OpenPostgresCompletion` and no other of the package's opens.
// What the completion itself does (claims nothing, migrates nothing, refuses a
// schema it would have to move) is proved in internal/state.
func TestTheTailOpensTheLedgerAsACompletionAndNothingElse(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "serverretiretail.go", nil, 0)
	mustOK(t, err)

	var opens []string

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "state" || !strings.HasPrefix(sel.Sel.Name, "Open") {
			return true
		}

		opens = append(opens, sel.Sel.Name)

		return true
	})

	if len(opens) != 1 || opens[0] != "OpenPostgresCompletion" {
		t.Fatalf("the tail's opens of the ledger: %v", opens)
	}
}

// THE LEDGER ATTEMPT HAS ITS OWN DEADLINE, and its expiry is a pending row
// rather than a failed converge. The open bounds itself and that bound ends
// when it returns, so a connection that goes unresponsive afterwards would
// leave the command waiting on the transport's own timeouts while it holds the
// converge's guard.
func TestTheTailBoundsTheWholeLedgerAttempt(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// A bound already spent when the attempt begins: what it proves is that
	// the attempt is under one at all, and what its expiry answers.
	saved := retireLedgerBound
	retireLedgerBound = time.Nanosecond

	t.Cleanup(func() { retireLedgerBound = saved })

	out, code := f.request(t, f.input(t, nil))

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeRetired || m["row"] != retireRowPending {
		t.Fatalf("a ledger attempt past its bound: %s", out)
	}

	why, ok := m["row_why"].(string)
	if !ok || !strings.Contains(why, "did not answer within") {
		t.Fatalf("the pending row does not name the bound: %s", out)
	}

	// AND THE RETIREMENT STILL OWES THE ROW: the marker is kept and nothing is
	// settled, which is what sends the role to the survivor.
	if m["marker"] != retireMarkerKept || m["settled"] != false {
		t.Fatalf("a bounded attempt settled the retirement: %s", out)
	}

	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementIntent {
			t.Fatalf("the row moved under an attempt that never reached the ledger: %+v (present %v)", r, present)
		}
	})
}

// WHOSE DEADLINE IT WAS decides what the expiry means, and the error decides
// whether a deadline ended the attempt at all. The bound's own case is driven
// end to end above; the other three are reached only by a connection that
// hangs for longer than the ledger open's own startup budget, or by an
// operator stopping the converge, so they are taken here.
func TestTheTailTellsTheThreeDeadlinesApart(t *testing.T) {
	live := t.Context()

	expired, cancelExpired := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancelExpired()

	stopped, cancelStopped := context.WithCancel(live)
	cancelStopped()

	cases := map[string]struct {
		outer, bounded context.Context
		err            error
		want           string
	}{
		"this command's own bound": {
			outer: live, bounded: expired, err: context.DeadlineExceeded,
			want: "the ledger did not answer within " + retireLedgerBound.String(),
		},
		"a deadline neither of them set": {
			outer: live, bounded: live, err: context.DeadlineExceeded,
			want: "the ledger did not answer within the open's own startup budget",
		},
		"the operator stopping the converge": {
			outer: stopped, bounded: stopped, err: context.DeadlineExceeded,
			want: "",
		},
		// THE EVIDENCE SURVIVES THE EXPIRY: an error that establishes another
		// deployment's ledger is that, whatever the clock did afterwards.
		"an error that is not the deadline's": {
			outer: live, bounded: expired, err: state.ErrForeignLedger,
			want: "",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := retireDeadlinePending(c.outer, c.bounded, c.err); got != c.want {
				t.Fatalf("the deadline reads %q, want %q", got, c.want)
			}
		})
	}
}
