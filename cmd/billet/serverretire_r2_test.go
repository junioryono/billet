package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// A DOCUMENT PAST THE BOUND IS DRAINED, THEN REFUSED: the collector writes
// stdin before it reads stdout, so a refusal that left the pipe unread would
// meet a writer still writing and hide the typed answer behind a broken pipe.
// The writer here is a goroutine on a real pipe with more than the bound, and
// it must finish.
func TestServerRetireDrainsAnOversizedDocumentBeforeRefusing(t *testing.T) {
	f := newRetireFixture(t)

	r, w, err := os.Pipe()
	mustOK(t, err)

	saved := retireStdin
	retireStdin = r

	t.Cleanup(func() { retireStdin = saved })

	written := make(chan error, 1)

	go func() {
		defer func() { _ = w.Close() }()

		// Two mebibytes, far past any pipe's buffer, under a write deadline: a
		// reader that stopped at the bound leaves this writer blocked, and the
		// deadline turns that block into the failure it is.
		mustOK(t, w.SetWriteDeadline(time.Now().Add(10*time.Second)))

		chunk := []byte(strings.Repeat("{", 64<<10))
		for range 32 {
			if _, err := w.Write(chunk); err != nil {
				written <- err

				return
			}
		}

		written <- nil
	}()

	var runErr error

	out := capture(t, func() {
		runErr = cmdServer(t.Context(), nil, []string{"retire", "--json", "--config", f.cfg, "--complete-row", "--run", "ci-b",
			"--as-host", "control-b", "--completion", "-"})
	})

	if err := <-written; err != nil {
		t.Fatalf("the writer met a closed pipe: %v", err)
	}

	m := retireAnswer(t, out)
	if m["outcome"] != retireOutcomeRefused || m["reason"] != retireReasonInput || runErr == nil {
		t.Fatalf("an oversized document must refuse on input after draining: %s (%v)", out, runErr)
	}
}

// EVERY HOST NAME IS A HOLDER BY THE GUARD'S GRAMMAR, --survivor-host
// included: a reservation naming a survivor the completion helper's --as-host
// could never spell is refused before anything is opened.
func TestServerRetireHoldsEveryHostNameToTheHolderGrammar(t *testing.T) {
	f := newRetireFixture(t)

	for name, survivor := range map[string]string{
		"a slash":    "control/b",
		"whitespace": "control b",
		"too long":   strings.Repeat("b", 201),
	} {
		out, code := f.run(t, "", "--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", survivor)

		if m := retireAnswer(t, out); m["reason"] != retireReasonCombination || code != exitRefused ||
			!strings.Contains(whyOf(m), "--survivor-host") {
			t.Errorf("%s: %s", name, out)
		}
	}
}

// THE ACKNOWLEDGEMENT PROVES THE JOURNAL IS THIS GUARD'S: owned by the holder
// or one its chain reached, its marker naming its transition, and the
// completer a participant; and a second answer cannot rename the completer.
func TestServerRetireAcknowledgesOnlyThisGuardsJournal(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-a")

	acknowledge := []string{"--acknowledge-row", "--run", "ci-a", "--retiring-host", "control-a", "--answer", "-"}

	answer := func(completedBy string) string {
		return string(mustMarshal(t, retirement.Answer{
			Schema: retirement.AnswerSchema, Deployment: f.identity, Retiring: "control-a", Survivor: "control-b",
			TransitionID: retireTestID, Reservation: "2026-09-11T08:00:00Z", Row: retirement.RowDone,
			CompletedBy: completedBy, CompletedAt: "2026-09-11T09:00:05Z",
		}))
	}

	// A journal owned by a holder this guard's chain never reached.
	f.journalAt(t, retirement.PhaseDone, "ci-elsewhere")

	out, code := f.run(t, answer("control-b"), acknowledge...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonJournal || code != exitRefused ||
		!strings.Contains(whyOf(m), "owned by") {
		t.Fatalf("a journal outside the chain must refuse: %s", out)
	}

	// The chain reaches the owner: admitted.
	markGuard(t, f.guard, nil, []string{"ci-elsewhere"})

	// But a marker naming another transition refuses, kept.
	other := "fedcba9876543210fedcba9876543210"
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: other}, []string{"ci-elsewhere"})

	out, code = f.run(t, answer("control-b"), acknowledge...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonMarker || code != exitUnknown {
		t.Fatalf("a marker of another transition must be could-not-tell: %s", out)
	}

	if rec := f.guard.record(t); rec.Transition == nil || rec.Transition.ID != other {
		t.Fatal("the mismatched marker was not kept")
	}

	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, []string{"ci-elsewhere"})

	// A completer that is neither the survivor nor the retiring host.
	out, code = f.run(t, answer("control-x"), acknowledge...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonMismatch || code != exitRefused {
		t.Fatalf("a stranger as completer must refuse: %s", out)
	}

	if j, _, err := retirement.ReadJournal(); err != nil || j.RowDone {
		t.Fatalf("a refused acknowledgement wrote the journal: %+v %v", j, err)
	}

	// The retiring host itself may have completed its own row.
	out, code = f.run(t, answer("control-a"), acknowledge...)
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeAcknowledged || code != 0 {
		t.Fatalf("the retiring host as completer is a participant: %s", out)
	}

	// And a later answer naming another completer cannot be `already`.
	out, code = f.run(t, answer("control-b"), acknowledge...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonMismatch || code != exitRefused ||
		!strings.Contains(whyOf(m), "acknowledged as completed by control-a") {
		t.Fatalf("a contradicting completer on an acknowledged row must refuse: %s", out)
	}

	out, code = f.run(t, answer("control-a"), acknowledge...)
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeAlready || code != 0 {
		t.Fatalf("the same answer again is already: %s", out)
	}

	// A journal whose archive holds no identity is could-not-tell.
	mustOK(t, os.Remove(state.DeploymentIDPath(retirementArchiveOf(t))))

	out, code = f.run(t, answer("control-a"), acknowledge...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonIdentity || code != exitUnknown {
		t.Fatalf("an archive without the identity must be could-not-tell: %s", out)
	}
}

// retirementArchiveOf is the archive the planted journal names.
func retirementArchiveOf(t *testing.T) string {
	t.Helper()

	j, _, err := retirement.ReadJournal()
	mustOK(t, err)

	return j.Archive
}

// THE DRY RUN NEVER READS A FAILED ASSOCIATION AS ABSENCE: an unminted
// identity cannot be associated with any row and is unreadable, and a SQLite
// ledger another account owns is read AS THAT ACCOUNT through the status
// report, never opened as root (which would leave root-owned sidecars).
func TestServerRetireDryRunReadsTheRowAsTheLedgersOwner(t *testing.T) {
	f := newRetireFixture(t)

	savedEUID, savedOwner, savedReexec := statusEUID, statusOwnerOf, retireReexecCapture

	t.Cleanup(func() { statusEUID, statusOwnerOf, retireReexecCapture = savedEUID, savedOwner, savedReexec })

	// Root over a directory the service account owns: the row comes from the
	// owner's report, and the in-process open is never made.
	statusEUID = func() int { return 0 }
	statusOwnerOf = func(string) (uint32, uint32, error) { return 990, 991, nil }

	var seen []string

	retireReexecCapture = func(_ context.Context, uid, gid uint32, args []string) ([]byte, int, error) {
		if uid != 990 || gid != 991 {
			t.Errorf("re-executed as %d:%d, want 990:991", uid, gid)
		}

		seen = args

		report := rolloutStatusReport{Schema: rolloutStatusSchema, Deployment: rolloutStatusDeployment{Bound: true, ID: f.identity},
			Nodes: []rolloutStatusNode{}, Registrations: []rolloutStatusRegistration{},
			Retirement: &rolloutStatusRetirement{Retiring: "control-a", Survivor: "control-b", Run: "ci-1",
				State: state.RetirementReserved, TransitionID: retireTestID, ReservedAt: "2026-09-11T08:00:00Z"}}

		return mustMarshal(t, report), 0, nil
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

	m := retireAnswer(t, out)
	if code != 0 || m["row_fact"] != string(retirement.RowReservedMine) || m["dispatch"] != string(retirement.DispatchAdopt) {
		t.Fatalf("the owner's row: %s", out)
	}

	if strings.Join(seen, " ") != "rollout status --json --config "+f.cfg {
		t.Fatalf("the owner's command: %v", seen)
	}

	// The owner's report bound to another deployment is unreadable, never a
	// row of this host.
	retireReexecCapture = func(context.Context, uint32, uint32, []string) ([]byte, int, error) {
		report := rolloutStatusReport{Schema: rolloutStatusSchema, Deployment: rolloutStatusDeployment{Bound: true,
			ID: strings.Repeat("e", 32)}, Nodes: []rolloutStatusNode{}, Registrations: []rolloutStatusRegistration{}}

		return mustMarshal(t, report), 0, nil
	}

	out, _ = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	if m := retireAnswer(t, out); m["row_fact"] != string(retirement.RowUnreadable) ||
		!strings.Contains(whyOf(m), "bound to deployment") {
		t.Fatalf("a foreign binding: %s", out)
	}

	// An unprivileged reader over another account's directory cannot read
	// the row at all.
	statusEUID = func() int { return 1000 }

	out, _ = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	if m := retireAnswer(t, out); m["row_fact"] != string(retirement.RowUnreadable) ||
		m["dispatch"] != string(retirement.DispatchUnknownLedger) {
		t.Fatalf("an unprivileged reader over another owner's ledger: %s", out)
	}

	// The owner itself reads in process.
	statusEUID = func() int { return 990 }

	out, _ = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	if m := retireAnswer(t, out); m["row_fact"] != string(retirement.RowAbsent) {
		t.Fatalf("the owner reads in process: %s", out)
	}

	// An unminted identity is unreadable, never absence.
	statusEUID, statusOwnerOf = savedEUID, savedOwner
	mustOK(t, os.Remove(state.DeploymentIDPath(f.stateDir)))

	out, _ = f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	if m := retireAnswer(t, out); m["row_fact"] != string(retirement.RowUnreadable) ||
		m["dispatch"] != string(retirement.DispatchUnknownLedger) || !strings.Contains(whyOf(m), "no deployment identity") {
		t.Fatalf("an unminted identity: %s", out)
	}
}

// AN ABANDONMENT'S RETRY OWES THE CLEARING'S FLUSHES: with the marker already
// absent, the guard directory and the root are flushed before the row is
// deleted, so a power loss cannot restore a marked record beside no row.
func TestServerRetireAbandonFlushesTheGuardBeforeDeletingTheRow(t *testing.T) {
	f := newRetireFixture(t)
	mustHold(t, "ci-1")
	f.reserveRow(t, "ci-1")

	var ops []string

	guardHook = func(op guardOp) error {
		ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.guard.parent+"/"))

		return nil
	}

	out, code := f.run(t, "", "--abandon-reservation", "--run", "ci-1", "--retiring-host", "control-a")
	guardHook = nil

	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeAbandoned || code != 0 {
		t.Fatalf("the abandonment: %s", out)
	}

	joined := strings.Join(ops, "\n")
	if !strings.Contains(joined, "fsync upgrades/active") || !strings.Contains(joined, "fsync upgrades") {
		t.Fatalf("the retry must flush the guard directory and the root before the row goes, got:\n%s", joined)
	}

	f.ledger(t, func(db *state.DB) {
		if _, present, err := db.ReadRetirement(t.Context(), f.identity); err != nil || present {
			t.Fatalf("the row must be gone: %v %v", present, err)
		}
	})
}

// THE RESERVATION IS HELD TO AN EXISTING MARKER: a guard marked beside no row,
// or marked for another transition than the row's, is could-not-tell and the
// ledger is not touched.
func TestServerRetireReserveIsHeldToAnExistingMarker(t *testing.T) {
	dsn := requireBackupPostgres(t)
	f := newRetireFixture(t)

	identityDir := t.TempDir()
	f.cfg = writeRetirePostgresConfig(t, identityDir)
	f.stateDir = identityDir
	t.Setenv("BILLET_STATE_DSN", dsn)

	id, err := state.DeploymentID(identityDir)
	mustOK(t, err)

	f.identity = id

	db, err := state.OpenPostgres(t.Context(), identityDir, dsn)
	mustOK(t, err)
	mustOK(t, db.Close())

	mustHold(t, "ci-1")
	reserve := []string{"--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b"}

	// A marker beside no row: nothing is inserted.
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)

	out, code := f.run(t, "", reserve...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonMarker || code != exitUnknown {
		t.Fatalf("a marker beside no row must refuse the reservation: %s", out)
	}

	markGuard(t, f.guard, nil, nil)

	out, code = f.run(t, "", reserve...)
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeReserved || code != 0 {
		t.Fatalf("the reservation: %s", out)
	}

	// A marker for another transition beside this host's row: the row is not
	// adopted and stays as it was.
	other := "fedcba9876543210fedcba9876543210"
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: other}, nil)

	out, code = f.run(t, "", reserve...)
	if m := retireAnswer(t, out); m["reason"] != retireReasonMarker || code != exitUnknown {
		t.Fatalf("a mismatched marker must refuse the adoption: %s", out)
	}

	// The marker for the row's transition: adopted.
	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)

	out, code = f.run(t, "", reserve...)
	if m := retireAnswer(t, out); m["outcome"] != retireOutcomeAdopted || code != 0 {
		t.Fatalf("the marker's own row is adopted: %s", out)
	}

	var report map[string]any
	mustOK(t, json.Unmarshal([]byte(out), &report))

	if report["state"] != stateNothingRetire {
		t.Fatalf("every answer carries state nothing: %s", out)
	}
}

// whyOf is an answer's why, or nothing.
func whyOf(m map[string]any) string {
	s, ok := m["why"].(string)
	if !ok {
		return ""
	}

	return s
}
