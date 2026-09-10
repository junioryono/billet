package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE NOTE: one line the holder writes at acquisition for whoever finds the
// guard (Puppet's disable message, balena's lock reason), kept for the
// guard's life and never rewritten by a later validation, a re-binding, a
// settlement or a takeover; typed as the command writes it by every reader;
// absent on a record whose holder wrote none, so every record from before
// this change reads exactly as it did.

func recordBytes(t *testing.T, f *guardFixture) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	return body
}

// E1: an acquisition with a note writes it, answers it, and status and the
// dry run report it.
func TestANoteIsWrittenAtAcquisitionAndReported(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.0")

	o := runPrepare(t, "--holder", "ci-1", "--validate", "--note", "run 12 of owner/repo")
	mustOutcome(t, o, prepareAcquired)

	if o.str("note") != "run 12 of owner/repo" || f.record(t).Note != "run 12 of owner/repo" {
		t.Fatalf("the note: answer %q, record %q", o.str("note"), f.record(t).Note)
	}

	status := capture(t, func() { mustOK(t, guardRun(t, "status", "--json")) })
	if !strings.Contains(status, `"note": "run 12 of owner/repo"`) {
		t.Errorf("status does not report the note:\n%s", status)
	}

	report := runPrepare(t, "--dry-run")
	if asMap(report.doc["guard"])["note"] != "run 12 of owner/repo" {
		t.Errorf("the dry run does not report the note: %v", report.doc["guard"])
	}
}

// E1b: a hold with a note writes it; a hold with an invalid note refuses
// before the lock, publishing nothing.
func TestAHoldWritesANoteAndRefusesOneThatIsNotALine(t *testing.T) {
	f := newGuardFixture(t)

	mustOK(t, guardRun(t, "hold", "--holder", "op-1", "--note", "kernel patch, back by 15:00"))

	if got := f.record(t).Note; got != "kernel patch, back by 15:00" {
		t.Fatalf("the hold's note: %q", got)
	}

	f = newGuardFixture(t)

	err := guardRun(t, "hold", "--holder", "op-1", "--note", "two\nlines")
	if err == nil || !strings.Contains(err.Error(), "one line of printable text") {
		t.Fatalf("a two-line note on a hold: %v", err)
	}

	if _, statErr := os.Lstat(f.active()); statErr == nil {
		t.Fatal("a refused note still published a guard")
	}
}

// E2, E2b, E3: a later validation with another note validates with the note
// as recorded; the re-binding, the settlement and a takeover keep it byte
// for byte in the record.
func TestANoteIsNeverRewrittenAfterAcquisition(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.0")

	first := runPrepare(t, "--holder", "ci-1", "--validate", "--note", "run 12 of owner/repo")
	mustOutcome(t, first, prepareAcquired)

	o := runPrepare(t, "--holder", "ci-1", "--validate", "--note", "another note")
	mustOutcome(t, o, prepareValidated)

	if o.str("note") != "run 12 of owner/repo" || f.record(t).Note != "run 12 of owner/repo" {
		t.Fatalf("a later validation rewrote the note: answer %q, record %q", o.str("note"), f.record(t).Note)
	}

	// The re-binding to a candidate keeps it.
	cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")
	o = runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustOutcome(t, o, prepareRebound)

	if o.str("note") != "run 12 of owner/repo" || f.record(t).Note != "run 12 of owner/repo" {
		t.Fatalf("the re-binding lost the note: answer %q, record %q", o.str("note"), f.record(t).Note)
	}

	// The settlement keeps it.
	mustOK(t, guardRun(t, "settle", "--holder", "ci-1", "--token", first.str("token")))

	if f.record(t).Note != "run 12 of owner/repo" {
		t.Fatalf("the settlement lost the note: %q", f.record(t).Note)
	}

	// A takeover keeps it (and refuses a note of its own). The recovery
	// directory is the candidate's, given a journal so the pointer names an
	// interrupted transaction.
	recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
	writeJournalFixture(t, recovery, "installed")
	mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
	cleanScan(t)

	err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped", "--note", "mine")
	if err == nil || !strings.Contains(err.Error(), "keeps the note") {
		t.Fatalf("a takeover with a note of its own: %v", err)
	}

	mustOK(t, guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"))

	rec := f.record(t)
	if rec.Holder != "ci-2" || rec.Note != "run 12 of owner/repo" {
		t.Fatalf("the takeover: holder %q note %q", rec.Holder, rec.Note)
	}
}

// E4: a note that is not one short line refuses before the lock, as a
// combination, and nothing is published or read.
func TestANoteThatIsNotOneLineIsRefusedBeforeTheLock(t *testing.T) {
	for _, c := range []struct{ name, note string }{
		{"201 bytes", strings.Repeat("n", 201)},
		{"a newline", "run 12\nof owner/repo"},
		{"a control character", "run\x0112"},
		{"a replacement rune", "run � 12"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			managedScript(t, f, "v0.10.0")

			o := runPrepare(t, "--holder", "ci-1", "--validate", "--note", c.note)
			mustRefusal(t, o, reasonCombination)

			if _, err := os.Lstat(f.root); err == nil {
				t.Fatal("a refused note still created the upgrade root")
			}
		})
	}

	// And --note outside --validate is a combination refusal too.
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.0")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	o := runPrepare(t, "--holder", "ci-1", "--no-change", "--note", "late")
	mustRefusal(t, o, reasonCombination)
}

// E5, E6: a record without a note answers without the member and reads as
// before; a record whose note is not one the command writes is malformed,
// never adopted or validated, and its bytes are left alone.
func TestARecordWithoutANoteReadsAsBeforeAndABadNoteIsMalformed(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.0")

	o := runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, o, prepareAcquired)

	if _, present := o.doc["note"]; present {
		t.Fatalf("an acquisition without a note answered one: %v", o.doc["note"])
	}

	if bytes.Contains(recordBytes(t, f), []byte(`"note"`)) {
		t.Fatal("a record without a note carries the member")
	}

	for _, c := range []struct {
		name string
		note any
	}{
		{"a number", 7},
		{"empty", ""},
		{"a newline", "two\nlines"},
		{"over the bound", strings.Repeat("n", 201)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			path := filepath.Join(f.active(), guardRecordName)

			var doc map[string]any
			mustOK(t, json.Unmarshal(recordBytes(t, f), &doc))

			doc["note"] = c.note
			damaged, err := json.MarshalIndent(doc, "", "  ")
			mustOK(t, err)
			mustOK(t, os.WriteFile(path, damaged, 0o600))

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonRecord)

			if !strings.Contains(o.str("why"), "note") {
				t.Errorf("why %q does not name the note", o.str("why"))
			}

			after, err := os.ReadFile(path)
			mustOK(t, err)

			if !bytes.Equal(after, damaged) {
				t.Error("the damaged record was rewritten")
			}
		})
	}
}
