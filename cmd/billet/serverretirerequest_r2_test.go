package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// A DRY RUN TAKES NOTHING AND WRITES NOTHING, whatever it is asked: it never
// releases a reservation, not even a fresh one it is told to, and it never
// creates the retirement directory a real request would.
func TestServerRetireRequestDryRunMutatesNothing(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	mustOK(t, os.RemoveAll(retirement.RetiredDir()))

	// A REFUSED dry run with the flag that would release the row.
	out, code := f.request(t, f.input(t, map[string]any{"survivor": nil}), "--dry-run", "--reservation-fresh")

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonSurvivor || code != exitRefused || m["reservation"] != "kept" ||
		strings.Contains(whyOf(m), "releasing the reservation") {
		t.Fatalf("a refused dry run: %s", out)
	}

	// AND A SUCCESSFUL one.
	out, code = f.request(t, f.input(t, nil), "--dry-run", "--reservation-fresh")
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeReported || code != 0 {
		t.Fatalf("a reported dry run: %s", out)
	}

	if _, err := os.Lstat(retirement.RetiredDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dry run created the retirement directory: %v", err)
	}

	// AND IT JUDGES THE GUARD AS THE REAL RUN DOES: a guard still in its
	// acquirer's cleanup window is refused by the preview too, so a report
	// never says a request would proceed where it would not.
	settled := f.guard.record(t)
	preparing := settled
	preparing.Preparing = true
	preparing.Token = strings.Repeat("a", 32)
	writeGuardRecordForTest(t, f.guard, preparing)

	out, code = f.request(t, f.input(t, nil), "--dry-run")
	if m := retireAnswer(t, out); m["reason"] != retireReasonGuard || code != exitRefused ||
		!strings.Contains(whyOf(m), "still preparing") {
		t.Fatalf("a preview over a preparing guard: %s", out)
	}

	writeGuardRecordForTest(t, f.guard, settled)

	// AND THE GUARD DIRECTORY IS HELD TO ITS TRUST: the mutating run's own
	// open requires exactly 0700, and the preview makes the same judgement.
	mustOK(t, os.Chmod(f.guard.active(), 0o755))

	out, code = f.request(t, f.input(t, nil), "--dry-run")
	if m := retireAnswer(t, out); m["reason"] != retireReasonGuard || code != exitUnknown ||
		!strings.Contains(whyOf(m), "0700") {
		t.Fatalf("a preview over a guard directory whose mode is not 0700: %s", out)
	}

	mustOK(t, os.Chmod(f.guard.active(), 0o700))

	// AND THE REPORT SAYS WHAT IT COULD NOT JUDGE.
	out, _ = f.request(t, f.input(t, nil), "--dry-run")
	if m := retireAnswer(t, out); !strings.Contains(fmt.Sprint(m["unlocked"]), "without the transaction lock") {
		t.Fatalf("the report does not say it is unlocked: %s", out)
	}

	// AND IT TAKES NOTHING: with the transaction lock held by another process
	// the dry run still answers, because a preview holds this host for nobody.
	held, err := takeTxLock()
	mustOK(t, err)

	out, code = f.request(t, f.input(t, nil), "--dry-run")
	held.release()

	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeReported || code != 0 {
		t.Fatalf("a dry run under a held transaction lock: %s", out)
	}

	f.pgLedger(t, func(db *state.DB) {
		r, present, err := db.ReadRetirement(t.Context(), f.identity)
		mustOK(t, err)

		if !present || r.State != state.RetirementReserved {
			t.Fatalf("the dry run moved the row: %+v (present %v)", r, present)
		}
	})
}

// A REFUSAL NEVER LEAVES A MARKER NAMING NO ROW: a request whose guard already
// carries this transition's marker keeps its reservation, because deleting the
// row would leave the marker with nothing to name, and the abandonment is the
// one way out of that pair.
func TestServerRetireRequestKeepsAMarkedReservationOnARefusal(t *testing.T) {
	f := newRequestFixture(t)
	row := f.reserve(t)

	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: row.TransitionID}, nil)

	out, code := f.request(t, f.input(t, map[string]any{"survivor": nil}), "--reservation-fresh")

	m := retireAnswer(t, out)
	if m["reason"] != retireReasonSurvivor || code != exitRefused || m["reservation"] != "kept" ||
		!strings.Contains(whyOf(m), "--abandon-reservation") {
		t.Fatalf("a refusal over a marked guard: %s", out)
	}

	f.pgLedger(t, func(db *state.DB) {
		if _, present, err := db.ReadRetirement(t.Context(), f.identity); err != nil || !present {
			t.Fatalf("the row must be kept: %v %v", present, err)
		}
	})

	// Without a marker the fresh row IS released.
	markGuard(t, f.guard, nil, nil)

	out, _ = f.request(t, f.input(t, map[string]any{"survivor": nil}), "--reservation-fresh")
	if m := retireAnswer(t, out); m["reservation"] != "released" {
		t.Fatalf("an unmarked fresh reservation is released: %s", out)
	}
}

// THE SURVIVOR IS THE RESERVATION'S: a request naming another survivor than
// the row the two controllers contended for is refused before anything is
// judged.
func TestServerRetireRequestRequiresTheReservationsSurvivor(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	self := f.selfReport(t)
	survivor := f.survivorReport(t)
	survivor["host"] = "control-c"

	out, code := f.run(t, f.input(t, map[string]any{"self": self, "survivor": survivor}),
		"--input", "-", "--run", requestRun, "--retiring-host", requestRetiring, "--survivor-host", "control-c",
		"--server-only", "--installed-sha256", f.installedSHA(t))

	if m := retireAnswer(t, out); m["reason"] != retireReasonReserved || code != exitRefused ||
		!strings.Contains(whyOf(m), requestSurvivor) {
		t.Fatalf("another survivor than the reservation's: %s", out)
	}
}

// A RETIREMENT IS REQUESTED ONLY ON A HOST AN INSTALLER HAS PREPARED: without
// the service-account record there is no global exclusion for the transition
// to close, and the request refuses naming the installers.
func TestServerRetireRequestNeedsAPreparedHost(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	mustOK(t, os.Remove(retirement.ServiceAccountPath()))
	mustOK(t, os.Remove(retirement.GlobalLockPath()))

	out, code := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonIdentity || code != exitRefused ||
		!strings.Contains(whyOf(m), "legacy") {
		t.Fatalf("an unprepared host: %s", out)
	}

	// And a lock beside no record is damage, not a legacy host.
	writeFile(t, retirement.GlobalLockPath(), "", 0o660)

	out, code = f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonIdentity || code != exitUnknown {
		t.Fatalf("a lock beside no record: %s", out)
	}
}

// THE INTENT'S WRITES ARE ORDERED, and each one's failure leaves the records
// before it and nothing after: the marker, the stage, the journal, the row,
// the status.
func TestServerRetireRequestWritesItsIntentInOrder(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	rowState := func() string {
		var row state.Retirement

		f.pgLedger(t, func(db *state.DB) {
			r, present, err := db.ReadRetirement(t.Context(), f.identity)
			mustOK(t, err)

			if !present {
				t.Fatal("the row is gone")
			}

			row = r
		})

		return row.State
	}

	// THE JOURNAL PRECEDES THE ROW: failing the journal leaves the row where
	// the reservation put it.
	injected := errors.New("staged failure")

	failPublish := func(t *testing.T, at string) {
		t.Helper()

		retirement.Publishing = func(path string) error {
			if path == at {
				return injected
			}

			return nil
		}

		// CLEARED WHATEVER HAPPENS NEXT: a failed assertion between the set
		// and the clear would leak the hook into every later test.
		t.Cleanup(func() { retirement.Publishing = nil })
	}

	failPublish(t, retirement.JournalPath())

	out, code := f.request(t, f.input(t, nil))
	retirement.Publishing = nil

	if m := retireAnswer(t, out); m["reason"] != retireReasonJournal || code != exitUnknown {
		t.Fatalf("a failed journal: %s", out)
	}

	if st := rowState(); st != state.RetirementReserved {
		t.Fatalf("the row moved before the journal: %s", st)
	}

	if _, presence, err := retirement.ReadStatus(); presence != retirement.StatusAbsent || err != nil {
		t.Fatalf("the status was published before the journal (%d %v)", presence, err)
	}

	// But the MARKER precedes the journal, and a retry finds it.
	if rec := f.guard.record(t); rec.Transition == nil {
		t.Fatal("the marker was not written before the journal")
	}

	// THE ROW PRECEDES THE STATUS: failing the status leaves the row at
	// intent with the journal written.
	failPublish(t, retirement.StatusPath())

	out, code = f.request(t, f.input(t, nil))
	retirement.Publishing = nil

	if m := retireAnswer(t, out); m["reason"] != retireReasonStatus || code != exitUnknown {
		t.Fatalf("a failed status: %s", out)
	}

	if st := rowState(); st != state.RetirementIntent {
		t.Fatalf("the row did not precede the status: %s", st)
	}

	if _, presence, err := retirement.ReadJournal(); presence != retirement.JournalPresent || err != nil {
		t.Fatalf("the journal did not precede the row: %d %v", presence, err)
	}
}

// AN EXISTING MARKER'S DURABILITY IS THIS INVOCATION'S TO FINISH: a retry that
// finds the marker already there flushes the guard and the root before it
// writes anything else, so a marker an earlier invocation renamed but never
// flushed cannot be the only thing holding the retirement together.
func TestServerRetireRequestFlushesAMarkerItDidNotWrite(t *testing.T) {
	f := newRequestFixture(t)
	row := f.reserve(t)

	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: row.TransitionID}, nil)

	for _, stop := range []string{"fsync upgrades/active", "fsync upgrades"} {
		injected := errors.New("staged failure at " + stop)

		guardHook = func(op guardOp) error {
			if op.Kind+" "+strings.TrimPrefix(op.Path, f.guard.parent+"/") == stop {
				return injected
			}

			return nil
		}

		out, code := f.request(t, f.input(t, nil))
		guardHook = nil

		if m := retireAnswer(t, out); m["reason"] != retireReasonMarker || code != exitUnknown ||
			!strings.Contains(whyOf(m), "staged failure") {
			t.Fatalf("%s: %s", stop, out)
		}

		if _, presence, err := retirement.ReadJournal(); presence != retirement.JournalAbsent || err != nil {
			t.Fatalf("%s: the journal was written over an unflushed marker (%d %v)", stop, presence, err)
		}
	}
}

// THE RENDERING'S NODE MUST BE ABLE TO START: the certificate and the key are
// read and must be a pair, and the trust store must hold an authority, because
// the archive moves the identity directory and the node is restarted after it.
func TestServerRetireRequestRefusesARenderingTheNodeCannotStart(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)

	keyPath := nodeTLSPathOf(t, f, "key")

	// The key removed: could-not-tell, since the file is the node's own.
	mustOK(t, os.Remove(keyPath))

	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitUnknown {
		t.Fatalf("a removed key: %s", out)
	}

	// Another authority's bundle: every file parses and the pair matches, and
	// the node still cannot start, because the trust store does not verify
	// the leaf. THE NODE'S OWN LOADER AND ITS OWN TLS CONSTRUCTION decide.
	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), strings.Repeat("f", 32))
	mustOK(t, err)

	other, err := ca.IssueNode("node-a")
	mustOK(t, err)

	writeFile(t, keyPath, string(other.KeyPEM), 0o600)

	out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitRefused {
		t.Fatalf("another authority's key: %s", out)
	}

	// The whole foreign pair, beside THIS deployment's trust store: the pair
	// holds and the leaf does not verify.
	writeFile(t, nodeTLSPathOf(t, f, "cert"), string(other.CertPEM), 0o644)

	out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitRefused ||
		!strings.Contains(whyOf(m), "not ones it can start with") {
		t.Fatalf("a foreign pair against this trust store: %s", out)
	}

	// A KEY THE LOADER REFUSES, over a bundle that otherwise HOLDS: the node
	// reads a private key only from the path it was given and only when
	// nobody else can read it, so the mode is the one thing left to refuse.
	writeFile(t, nodeTLSPathOf(t, f, "cert"), mustRead(t, nodeTLSPathOf(t, f, "cert")+".orig"), 0o644)
	writeFile(t, keyPath, mustRead(t, keyPath+".orig"), 0o600)
	mustOK(t, os.Chmod(keyPath, 0o644))

	out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitRefused ||
		!strings.Contains(whyOf(m), "not ones it reads") || !strings.Contains(whyOf(m), "chmod 600") {
		t.Fatalf("a group-readable key is a rule refused, not a read that failed: %s", out)
	}

	mustOK(t, os.Chmod(keyPath, 0o600))

	// AND A KEY THAT IS GONE is a read that failed: could-not-tell.
	mustOK(t, os.Remove(keyPath))
	mustOK(t, os.Remove(keyPath+".orig"))

	out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitUnknown {
		t.Fatalf("a removed key is could-not-tell: %s", out)
	}
}

// nodeTLSPathOf is the path the fixture's node configuration names for one of
// its credentials.
func nodeTLSPathOf(t *testing.T, f *requestFixture, member string) string {
	t.Helper()

	for _, line := range strings.Split(mustRead(t, f.cfg), "\n") {
		field := strings.TrimSpace(line)
		if strings.HasPrefix(field, member+": ") {
			return strings.TrimSpace(strings.TrimPrefix(field, member+":"))
		}
	}

	t.Fatalf("the configuration names no node tls %s", member)

	return ""
}

// THE ARCHIVE'S DESTINATION IS THE MOUNT COMPARED, not its parent: a
// retirement directory on its own mount is refused even though its parent
// shares the identity's.
func TestServerRetireRequestComparesTheArchivesOwnMount(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	mustOK(t, retirement.EnsureRetiredDir())

	writeFile(t, mountinfoPath, fmt.Sprintf("1 0 8:1 / / rw - ext4 /dev/sda1 rw\n2 1 8:2 / %s rw - ext4 /dev/sdb1 rw\n",
		mustEval(t, retirement.RetiredDir())), 0o644)

	out, code := f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonMount || code != exitRefused {
		t.Fatalf("the archive on its own mount: %s", out)
	}

	// And the identity directory itself a mount point.
	writeFile(t, mountinfoPath, fmt.Sprintf("1 0 8:1 / / rw - ext4 /dev/sda1 rw\n2 1 8:2 / %s rw - ext4 /dev/sdb1 rw\n",
		mustEval(t, f.stateDir)), 0o644)

	out, code = f.request(t, f.input(t, nil))
	if m := retireAnswer(t, out); m["reason"] != retireReasonMount || code != exitRefused ||
		!strings.Contains(whyOf(m), "mount point") {
		t.Fatalf("an identity directory that is a mount point: %s", out)
	}
}

// THE ROUND'S BOUNDARIES: a round exactly as old as the bound is admitted and
// one older is not; a report collected exactly when the round started is
// admitted and one before it is not.
func TestServerRetireRequestHoldsTheRoundsBoundaries(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	// The fixture's clock is 10:00:30 and the round starts at 10:00:05, so
	// twenty-five seconds is the exact age.
	out, code := f.request(t, f.input(t, nil), "--report-max-age", "25s")
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeRetired || code != 0 {
		t.Fatalf("a round exactly as old as the bound: %s", out)
	}

	f2 := newRequestFixture(t)
	f2.reserve(t)

	out, code = f2.request(t, f2.input(t, nil), "--report-max-age", "24s")
	if m := retireAnswer(t, out); m["reason"] != retireReasonReportStale || code != exitRefused {
		t.Fatalf("a round older than the bound: %s", out)
	}

	// A report collected exactly when the round started.
	f3 := newRequestFixture(t)
	f3.reserve(t)

	self := f3.selfReport(t)
	self["collected_at"] = requestStarted

	out, code = f3.request(t, f3.input(t, map[string]any{"self": self}))
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeRetired || code != 0 {
		t.Fatalf("a report collected at the round's start: %s", out)
	}

	// And one collected a second before it.
	f4 := newRequestFixture(t)
	f4.reserve(t)

	self = f4.selfReport(t)
	self["collected_at"] = "2026-09-11T10:00:04Z"

	out, code = f4.request(t, f4.input(t, map[string]any{"self": self}))
	if m := retireAnswer(t, out); m["reason"] != retireReasonReportStale || code != exitRefused {
		t.Fatalf("a report collected before the round: %s", out)
	}
}

// writeGuardRecordForTest writes a guard record as the command writes it, for
// a case that needs a shape the command does not produce.
func writeGuardRecordForTest(t *testing.T, f *guardFixture, rec guardRecord) {
	t.Helper()

	body, err := json.Marshal(rec)
	mustOK(t, err)
	mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), body, 0o600))
}

// THE MUTATING REQUEST OBSERVES THE CONFIGURATION UNDER THE TRANSACTION LOCK,
// and this is a STRUCTURAL witness because no schedule can show it: the lock
// is taken non-blocking, so a contender never waits inside the window an
// observation made before it would open. What the window costs is a request
// that judges bytes another converge has already replaced, and the only proof
// available is the order of the calls themselves.
func TestServerRetireRequestObservesTheConfigurationUnderTheLock(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "serverretirerequest.go", nil, 0)
	mustOK(t, err)

	var body *ast.BlockStmt

	// THE FUNCTION THAT DOES THE WORK, not the exit that annotates its answer:
	// `retireRequest` is a thin wrapper whose body takes nothing at all.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "retireRequestUnder" {
			body = fn.Body
		}
	}

	if body == nil {
		t.Fatal("retireRequestUnder is gone")
	}

	guard, observe := -1, -1

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		name, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}

		// The LAST occurrence of each in the function body: the preview's own
		// observation comes first and is the one that takes nothing.
		switch name.Name {
		case "retireGuard":
			guard = int(call.Pos())
		case "observeRetireConfig":
			if int(call.Pos()) > guard {
				observe = int(call.Pos())
			}
		}

		return true
	})

	switch {
	case guard < 0:
		t.Fatal("the request takes no guard")
	case observe < 0:
		t.Fatal("the request never observes the configuration after taking the guard")
	case observe < guard:
		t.Fatal("the request observes the configuration before it takes the transaction lock")
	}
}

// AND THE DIGEST THE ROLE READ IS COMPARED WITH THE BYTES THE REQUEST JUDGES:
// a configuration replaced between the role's read and the request refuses
// rather than recording a decision about bytes that are gone.
func TestServerRetireRequestRefusesAConfigurationThatMoved(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	installed := mustRead(t, f.cfg)
	digest := f.installedSHA(t)

	writeFile(t, f.cfg, strings.Replace(installed, "max_vcpu: 8", "max_vcpu: 16", 1), 0o600)

	out, code := f.run(t, f.input(t, nil), "--input", "-", "--run", requestRun, "--retiring-host", requestRetiring,
		"--survivor-host", requestSurvivor, "--server-only", "--installed-sha256", digest)

	if m := retireAnswer(t, out); m["reason"] != retireReasonConfig || code != exitRefused ||
		!strings.Contains(whyOf(m), "moved since the role read it") {
		t.Fatalf("a configuration that moved: %s", out)
	}
}
