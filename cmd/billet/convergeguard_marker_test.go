package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
)

const markerID = "fedcba9876543210fedcba9876543210"

// markGuard writes the retirement marker into a held guard's record the way
// the retire command will, through the record's own JSON, so the readers below
// meet exactly what the producer writes.
func markGuard(t *testing.T, f *guardFixture, transition *guardTransition, takenOverFrom []string) {
	t.Helper()

	rec := f.record(t)
	rec.Transition = transition
	rec.TakenOverFrom = takenOverFrom

	body, err := json.Marshal(rec)
	mustOK(t, err)
	mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), body, 0o600))
}

// plantJournal writes a journal at the pinned retirement root, at the phase
// given, owned by the reserving holder, for the marker's transition.
func plantJournal(t *testing.T, phase retirement.Phase, owner string, settled bool) {
	t.Helper()
	plantJournalFor(t, phase, owner, settled, markerID)
}

// plantJournalFor is plantJournal for the transition named.
func plantJournalFor(t *testing.T, phase retirement.Phase, owner string, settled bool, transition string) {
	t.Helper()

	j := retirement.Journal{
		Schema: retirement.JournalSchema, Phase: phase, Variant: retirement.VariantServerOnly,
		Deployment: strings.Repeat("d", 32), Retiring: "control-a",
		Survivor: retirement.JournalSurvivor{Host: "control-b", Deployment: strings.Repeat("d", 32), CASHA256: strings.Repeat("c", 64)},
		Backend:  "postgres", Controllers: "active-passive",
		IdentityDir: "/var/lib/billet/server", Archive: "/var/lib/billet/retired/identity-2026-09-11T08:00:00Z",
		InstalledSHA256: strings.Repeat("a", 64), Config: "absent",
		Provenance: retirement.Provenance{
			ReservingHolder: owner, TransitionID: transition, Reservation: "2026-09-11T08:00:00Z",
			Deployment: strings.Repeat("d", 32), Retiring: "control-a", Survivor: "control-b",
		},
		Ownership: retirement.Ownership{Owner: owner},
		Settled:   settled,
	}

	if phase == retirement.PhaseDone {
		j.DoneAt = "2026-09-11T09:00:00Z"
	}

	// Settlement follows the row's acknowledgement, and the journal's own
	// shape check holds it to that.
	if settled {
		j.RowDone, j.CompletedBy = true, "control-b"
	}

	mustOK(t, j.Write(time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC)))
}

// A GUARD CARRYING THE RETIREMENT MARKER IS RELEASED BY NOBODY: not by its
// holder, not by the acquirer's cleanup token, not by recover; the record is
// left byte for byte.
func TestAGuardWithTheRetirementMarkerIsNeitherReleasedNorRecovered(t *testing.T) {
	useRetirementRoot(t)
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	markGuard(t, f, &guardTransition{Kind: transitionRetirement, ID: markerID}, nil)

	before, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	cleanScan(t)

	for name, args := range map[string][]string{
		"release": {"release", "--holder", "ci-1"},
		"recover": {"recover", "--holder", "ci-1", "--old-driver-stopped"},
	} {
		err := guardRun(t, args...)
		if !errors.Is(err, errGuardTransition) {
			t.Errorf("%s over a marked guard: %v, want errGuardTransition", name, err)
		}
	}

	after, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	if !bytes.Equal(before, after) {
		t.Fatalf("a refusal must leave the record as it was:\n%s\n%s", before, after)
	}

	if _, err := os.Stat(f.active()); err != nil {
		t.Fatal("the guard must still be held")
	}

	// THE MARKER IS REPORTED, so a preparation sees it before anything ordinary.
	out := capture(t, func() { mustOK(t, guardRun(t, "status", "--json")) })

	var report struct {
		Guard *struct {
			Transition *guardTransition `json:"transition"`
		} `json:"guard"`
	}

	mustOK(t, json.Unmarshal([]byte(out), &report))

	if report.Guard == nil || report.Guard.Transition == nil || report.Guard.Transition.ID != markerID {
		t.Fatalf("the status must report the marker, got %s", out)
	}
}

// A TAKEOVER ADMITS A MARKED GUARD EXACTLY WHEN A JOURNAL EXISTS, keeps the
// marker and appends the old holder to the chain; a marker with no journal is
// an abandoned reservation, refused naming the abandon command.
func TestATakeoverOfAMarkedGuardNeedsAJournalAndKeepsTheMarker(t *testing.T) {
	useRetirementRoot(t)
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	markGuard(t, f, &guardTransition{Kind: transitionRetirement, ID: markerID}, nil)
	cleanScan(t)

	err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
	if err == nil || !strings.Contains(err.Error(), "--abandon-reservation") {
		t.Fatalf("a marker with no journal must refuse the takeover naming the abandonment, got %v", err)
	}

	plantJournal(t, retirement.PhaseStopped, "ci-1", false)

	mustOK(t, guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"))

	rec := f.record(t)
	if rec.Holder != "ci-2" || rec.Transition == nil || rec.Transition.ID != markerID ||
		!reflect.DeepEqual(rec.TakenOverFrom, []string{"ci-1"}) || rec.Token != "" || rec.Preparing {
		t.Fatalf("the takeover must re-label, keep the marker and append the chain, got %+v", rec)
	}

	// A SECOND TAKEOVER APPENDS, never overwrites.
	mustOK(t, guardRun(t, "hold", "--holder", "ci-3", "--recover-from", "ci-2", "--old-driver-stopped"))

	if rec := f.record(t); !reflect.DeepEqual(rec.TakenOverFrom, []string{"ci-1", "ci-2"}) {
		t.Fatalf("the chain must hold every previous holder in order, got %v", rec.TakenOverFrom)
	}

	// The done tail is a takeover's too: a done journal not yet settled.
	plantJournal(t, retirement.PhaseDone, "ci-3", false)

	mustOK(t, guardRun(t, "hold", "--holder", "ci-4", "--recover-from", "ci-3", "--old-driver-stopped"))

	if err := guardRun(t, "release", "--holder", "ci-4"); !errors.Is(err, errGuardTransition) {
		t.Fatalf("an unsettled done journal must keep the guard, got %v", err)
	}
}

// A MARKER IS HELD TO ITS JOURNAL: a guard marked for one transition beside
// another transition's journal, or beside a journal another holder owns, is
// refused naming both and the record is left byte for byte; and a journal the
// chain does not reach is refused.
func TestATakeoverRefusesAMarkerThatIsNotTheJournals(t *testing.T) {
	useRetirementRoot(t)
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	markGuard(t, f, &guardTransition{Kind: transitionRetirement, ID: markerID}, nil)
	cleanScan(t)

	before, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	other := "0123456789abcdef0123456789abcdef"
	plantJournalFor(t, retirement.PhaseStopped, "ci-1", false, other)

	err = guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
	if err == nil || !strings.Contains(err.Error(), markerID) || !strings.Contains(err.Error(), other) {
		t.Fatalf("a marker beside another transition's journal must refuse naming both ids, got %v", err)
	}

	// The journal is the marker's transition but ANOTHER HOLDER'S, one the
	// guard's chain never reached.
	plantJournal(t, retirement.PhaseStopped, "ci-elsewhere", false)

	err = guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("a journal outside the guard's chain must refuse the takeover, got %v", err)
	}

	after, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
	mustOK(t, err)

	if !bytes.Equal(before, after) {
		t.Fatalf("the refused takeovers changed the record:\n%s\n%s", before, after)
	}
}

// THE TAIL'S ONE CRASH WINDOW IS TAKEN OVER: the tail clears the marker and
// then writes settled, so a done journal not yet settled beside a guard with
// no marker is that window, and the takeover restores the marker from the
// journal. A guard with no marker beside a journal at any other phase, or a
// settled one, is refused.
func TestATakeoverRestoresTheMarkerAcrossTheTailsCrashWindow(t *testing.T) {
	useRetirementRoot(t)
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	cleanScan(t)

	plantJournal(t, retirement.PhaseStopped, "ci-1", false)

	err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
	if err == nil || !strings.Contains(err.Error(), "no step of the protocol produces") {
		t.Fatalf("no marker beside an incomplete journal must refuse, got %v", err)
	}

	plantJournal(t, retirement.PhaseDone, "ci-1", true)

	err = guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
	if err == nil || !strings.Contains(err.Error(), "settled") {
		t.Fatalf("no marker beside a settled journal leaves nothing to take over, got %v", err)
	}

	plantJournal(t, retirement.PhaseDone, "ci-1", false)

	mustOK(t, guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"))

	rec := f.record(t)
	if rec.Holder != "ci-2" || rec.Transition == nil || rec.Transition.ID != markerID ||
		rec.Transition.Kind != transitionRetirement || !reflect.DeepEqual(rec.TakenOverFrom, []string{"ci-1"}) {
		t.Fatalf("the takeover must restore the marker from the journal, got %+v", rec)
	}

	// And the restored marker keeps the guard until the tail settles.
	if err := guardRun(t, "release", "--holder", "ci-2"); !errors.Is(err, errGuardTransition) {
		t.Fatalf("the restored marker must keep the guard, got %v", err)
	}
}

// AN UNSETTLED DONE JOURNAL KEEPS THE GUARD EVEN WITHOUT THE MARKER (the tail
// clears the marker and then writes settled), and a settled one releases.
func TestAnUnsettledDoneJournalRefusesAReleaseWithoutTheMarker(t *testing.T) {
	useRetirementRoot(t)
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	plantJournal(t, retirement.PhaseDone, "ci-1", false)

	if err := guardRun(t, "release", "--holder", "ci-1"); !errors.Is(err, errGuardTransition) ||
		!strings.Contains(err.Error(), "not settled") {
		t.Fatalf("an unsettled done journal must refuse the release, got %v", err)
	}

	plantJournal(t, retirement.PhaseDone, "ci-1", true)

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))

	if _, err := os.Stat(f.active()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a settled done journal releases the guard")
	}
}

// A MARKER WITHOUT AN ID, OR OF ANOTHER KIND, MAKES THE RECORD UNREADABLE, so
// no reader takes it for an absent marker or a matching one; an empty takeover
// chain likewise.
func TestAnUnidentifiedMarkerMakesTheRecordUnreadable(t *testing.T) {
	for name, raw := range map[string]string{
		"no id":         `"transition":{"kind":"retirement"}`,
		"short id":      `"transition":{"kind":"retirement","id":"abc"}`,
		"another kind":  `"transition":{"kind":"upgrade","id":"` + markerID + `"}`,
		"extra member":  `"transition":{"kind":"retirement","id":"` + markerID + `","phase":"done"}`,
		"not an object": `"transition":"retirement"`,
		"empty chain":   `"taken_over_from":[]`,
		"blank holder":  `"taken_over_from":["ci-1",""]`,
		"chain string":  `"taken_over_from":"ci-1"`,
	} {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		body, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
		mustOK(t, err)

		marked := strings.Replace(string(body), `"holder":`, raw+`,"holder":`, 1)
		mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte(marked), 0o600))

		shape, err := classifyClaim()
		mustOK(t, err)

		if shape.Kind != claimGuard || shape.RecordErr == "" {
			t.Errorf("%s: a record carrying %s must be unreadable, got %+v", name, raw, shape)
		}
	}

	// THE WELL-FORMED SHAPES READ, and the marker and the chain come back.
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	markGuard(t, f, &guardTransition{Kind: transitionRetirement, ID: markerID}, []string{"ci-0"})

	shape, err := classifyClaim()
	mustOK(t, err)

	if shape.RecordErr != "" || shape.Guard.Transition == nil || shape.Guard.Transition.ID != markerID ||
		!reflect.DeepEqual(shape.Guard.TakenOverFrom, []string{"ci-0"}) {
		t.Fatalf("a well-formed marker must read, got %+v", shape)
	}
}
