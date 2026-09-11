package retirement

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleJournal() Journal {
	return Journal{
		Schema: JournalSchema, Phase: PhaseIntent, Variant: VariantRetainedNode,
		Deployment: "0123456789abcdef0123456789abcdef", Retiring: "control-a",
		Survivor: JournalSurvivor{Host: "control-b", Deployment: "0123456789abcdef0123456789abcdef", CASHA256: strings.Repeat("c", 64)},
		Backend:  "postgres", Controllers: "active-passive",
		IdentityDir: "/var/lib/billet/server", Archive: "/var/lib/billet/retired/identity-2026-09-11T08:00:00Z",
		InstalledSHA256: strings.Repeat("a", 64), StagedSHA256: strings.Repeat("b", 64), Config: "present",
		Nodes:   []JournalNode{{Name: "epyc-1", Incarnation: "inc-1", Endpoint: "https://10.0.0.2:7717"}},
		Locator: JournalLocator{Backend: "postgres", DSNEnv: "BILLET_LEDGER_DSN", EnvironmentFile: "/etc/billet/server.env", IdentityDir: "/var/lib/billet/server", Archive: "/var/lib/billet/retired/identity-2026-09-11T08:00:00Z"},
		Provenance: Provenance{
			ReservingHolder: "ci-42", TransitionID: strings.Repeat("d", 32), Reservation: "2026-09-11T08:00:00Z",
			Deployment: "0123456789abcdef0123456789abcdef", Retiring: "control-a", Survivor: "control-b",
		},
		Ownership: Ownership{Owner: "ci-42"},
		WrittenAt: "2026-09-11T08:00:00Z",
	}
}

// THE JOURNAL ROUND-TRIPS THROUGH ITS DURABLE WRITE, at 0600 under the private
// directory the write creates, and a stranger's mode or owner is refused as
// untrusted rather than read.
func TestTheJournalRoundTripsAndIsTrustedOnlyAsBilletWritesIt(t *testing.T) {
	root := useRoot(t)

	if _, presence, err := ReadJournal(); presence != JournalAbsent || err != nil {
		t.Fatalf("no journal is positive absence, got %d %v", presence, err)
	}

	j := sampleJournal()
	now := time.Date(2026, 9, 11, 8, 1, 0, 0, time.UTC)

	if err := j.Write(now); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "retired"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the retirement directory must be 0700, got %v %v", info, err)
	}

	info, err = os.Stat(JournalPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the journal must be 0600, got %v %v", info, err)
	}

	read, presence, err := ReadJournal()
	if err != nil || presence != JournalPresent {
		t.Fatalf("ReadJournal: %d %v", presence, err)
	}

	j.WrittenAt = now.Format(time.RFC3339Nano)

	if a, b := mustJSON(t, read), mustJSON(t, j); a != b {
		t.Fatalf("the journal did not round-trip:\n%s\n%s", a, b)
	}

	if err := os.Chmod(JournalPath(), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, presence, err := ReadJournal(); presence != JournalUnreadable || err == nil {
		t.Fatalf("a world-readable journal is not one billet wrote, got %d %v", presence, err)
	}

	if err := os.Chmod(JournalPath(), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(JournalPath(), []byte(`{"schema":1,"phase":"intent"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, presence, err := ReadJournal(); presence != JournalMalformed || err == nil {
		t.Fatalf("a journal missing its fields is malformed, got %d %v", presence, err)
	}

	if err := os.WriteFile(JournalPath(), []byte(`{"schema":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, presence, err := ReadJournal(); presence != JournalMalformed || !strings.Contains(err.Error(), "schema 2") {
		t.Fatalf("another schema is refused whole, got %d %v", presence, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// A JOURNAL THAT DOES NOT HOLD IS NOT WRITTEN: a server-only journal with a
// stage, a retained-node one without, a phase or a variant this binary does
// not know.
func TestAJournalThatDoesNotHoldIsNeitherWrittenNorRead(t *testing.T) {
	useRoot(t)

	cases := map[string]func(*Journal){
		"server-only with a stage": func(j *Journal) {
			j.Variant = VariantServerOnly
		},
		"retained-node without a stage": func(j *Journal) {
			j.StagedSHA256 = ""
		},
		"unknown phase":   func(j *Journal) { j.Phase = "resting" },
		"unknown variant": func(j *Journal) { j.Variant = "hybrid" },
		"no owner":        func(j *Journal) { j.Ownership.Owner = "" },
		"no provenance":   func(j *Journal) { j.Provenance.TransitionID = "" },
	}

	for name, mutate := range cases {
		j := sampleJournal()
		mutate(&j)

		if err := j.Write(time.Now()); err == nil {
			t.Errorf("%s: written", name)
		}
	}

	if _, presence, err := ReadJournal(); presence != JournalAbsent || err != nil {
		t.Fatalf("a refused write left a journal behind (presence %d, err %v)", presence, err)
	}
}

// VALIDATION NAMES THE FIELD THAT DISAGREES, one at a time, and a settled done
// journal is validated by provenance alone with no owner check.
func TestValidationNamesEachMismatchedFieldAndSettledDoneNeedsNoOwner(t *testing.T) {
	base := sampleJournal()
	row := &RowFacts{State: "intent", Retiring: "control-a", Survivor: "control-b",
		ReservedAt: "2026-09-11T08:00:00Z", TransitionID: strings.Repeat("d", 32)}
	exp := JournalExpectation{Retiring: "control-a", Holder: "ci-42", Row: row, Identity: base.Deployment}

	if err := base.Validate(exp); err != nil {
		t.Fatalf("the sample must validate against its own facts, got %v", err)
	}

	cases := map[string]struct {
		mutate func(*Journal, *JournalExpectation)
		field  string
	}{
		"retiring":            {func(_ *Journal, e *JournalExpectation) { e.Retiring = "control-z" }, "retiring"},
		"deployment":          {func(_ *Journal, e *JournalExpectation) { e.Identity = "other" }, "deployment"},
		"provenance retiring": {func(j *Journal, _ *JournalExpectation) { j.Provenance.Retiring = "x" }, "provenance.retiring"},
		"provenance survivor": {func(j *Journal, _ *JournalExpectation) { j.Provenance.Survivor = "x" }, "provenance.survivor"},
		"row survivor":        {func(_ *Journal, e *JournalExpectation) { r := *row; r.Survivor = "x"; e.Row = &r }, "row.survivor"},
		"row reservation":     {func(_ *Journal, e *JournalExpectation) { r := *row; r.ReservedAt = "2026-09-11T08:00:01Z"; e.Row = &r }, "row.reserved_at"},
		"row transition": {func(_ *Journal, e *JournalExpectation) {
			r := *row
			r.TransitionID = strings.Repeat("e", 32)
			e.Row = &r
		}, "row.transition_id"},
		"holder": {func(_ *Journal, e *JournalExpectation) { e.Holder = "ci-99" }, "owned by"},
	}

	for name, c := range cases {
		j := base
		e := exp
		c.mutate(&j, &e)

		err := j.Validate(e)
		if !errors.Is(err, ErrJournalMismatch) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: %v, want ErrJournalMismatch naming %s", name, err, c.field)
		}
	}

	// A takeover chain: the journal's owner is A, the guard's holder C took
	// over from [A, B] with B never rebinding.
	chained := exp
	chained.Holder = "ci-C"
	chained.TakenOverFrom = []string{"ci-42", "ci-B"}

	if err := base.Validate(chained); err != nil {
		t.Fatalf("a holder whose record lists the owner among its takeovers must be admitted, got %v", err)
	}

	settled := base
	settled.Phase, settled.Settled = PhaseDone, true
	settled.RowDone, settled.CompletedBy = true, "control-b"
	settled.DoneAt = "2026-09-11T09:00:00Z"

	unrelated := exp
	unrelated.Holder = "ci-later"
	unrelated.Row = nil

	if err := settled.Validate(unrelated); err != nil {
		t.Fatalf("a settled done journal is validated by provenance alone, got %v", err)
	}

	unsettled := settled
	unsettled.Settled = false

	if err := unsettled.Validate(unrelated); !errors.Is(err, ErrJournalMismatch) {
		t.Fatalf("an UNSETTLED done journal still needs its owner, got %v", err)
	}

	rebound := base
	rebound.Rebind("ci-42")

	if len(rebound.Ownership.Owners) != 0 {
		t.Fatal("rebinding to the current owner appends nothing")
	}

	rebound.Rebind("ci-B")
	rebound.Rebind("ci-C")

	if rebound.Ownership.Owner != "ci-C" || strings.Join(rebound.Ownership.Owners, ",") != "ci-42,ci-B" {
		t.Fatalf("the chain must append every previous owner in order, got %+v", rebound.Ownership)
	}
}

// THE TWO DOCUMENTS ARE DECODED STRICTLY AND HELD TO THE JOURNAL, field by
// field.
func TestTheCompletionAndTheAnswerAreStrictAndBoundToTheJournal(t *testing.T) {
	j := sampleJournal()
	j.Phase, j.DoneAt = PhaseDone, "2026-09-11T09:00:00Z"

	c := CompletionOf(j)

	raw := mustJSON(t, c)

	back, err := DecodeCompletion([]byte(raw))
	if err != nil || back != c {
		t.Fatalf("the completion did not round-trip: %v %+v", err, back)
	}

	for name, doc := range map[string]string{
		"another schema":   strings.Replace(raw, `"schema":1`, `"schema":2`, 1),
		"missing field":    strings.Replace(raw, `"survivor":"control-b",`, ``, 1),
		"repeated member":  strings.Replace(raw, `"retiring":"control-a",`, `"retiring":"control-a","retiring":"control-a",`, 1),
		"same host twice":  strings.Replace(raw, `"survivor":"control-b"`, `"survivor":"control-a"`, 1),
		"unknown member":   strings.Replace(raw, `"schema":1,`, `"schema":1,"extra":true,`, 1),
		"trailing content": raw + "}",
	} {
		if _, err := DecodeCompletion([]byte(doc)); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: %v, want ErrDocument", name, err)
		}
	}

	a := Answer{
		Schema: AnswerSchema, Deployment: c.Deployment, Retiring: c.Retiring, Survivor: c.Survivor,
		TransitionID: c.TransitionID, Reservation: c.Reservation, Row: RowDone,
		CompletedBy: "control-b", CompletedAt: "2026-09-11T09:00:05Z",
	}

	decoded, err := DecodeAnswer([]byte(mustJSON(t, a)))
	if err != nil || decoded != a {
		t.Fatalf("the answer did not round-trip: %v %+v", err, decoded)
	}

	if err := a.MatchesJournal(j); err != nil {
		t.Fatalf("the answer must match the journal it was made for, got %v", err)
	}

	for name, mutate := range map[string]func(*Answer){
		"deployment":  func(x *Answer) { x.Deployment = "other" },
		"retiring":    func(x *Answer) { x.Retiring = "control-z" },
		"survivor":    func(x *Answer) { x.Survivor = "control-z" },
		"transition":  func(x *Answer) { x.TransitionID = strings.Repeat("e", 32) },
		"reservation": func(x *Answer) { x.Reservation = "2026-09-11T08:00:01Z" },
	} {
		x := a
		mutate(&x)

		if err := x.MatchesJournal(j); !errors.Is(err, ErrDocument) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s mismatched: %v, want ErrDocument naming it", name, err)
		}
	}

	bad := a
	bad.Row = "maybe"

	if _, err := DecodeAnswer([]byte(mustJSON(t, bad))); !errors.Is(err, ErrDocument) {
		t.Errorf("an answer with an unknown row word: %v, want ErrDocument", err)
	}

	bad = a
	bad.CompletedBy = ""

	if _, err := DecodeAnswer([]byte(mustJSON(t, bad))); !errors.Is(err, ErrDocument) {
		t.Errorf("an answer naming nobody: %v, want ErrDocument", err)
	}
}
