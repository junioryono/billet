package guestreport

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func millis(n int64) *int64 { return &n }

// aSuite is a whole suite of one failure, one skip and a passing case per entry of
// passing, each taking that many milliseconds.
func aSuite(name string, passing ...int64) SuiteCases {
	s := SuiteCases{Name: name, Failures: 1, Skipped: 1, Complete: true, Cases: []CaseResult{
		{Name: "fails", Status: CaseFailed, DurationMillis: millis(7), Message: "wanted 1"},
		{Name: "skips", Status: CaseSkipped},
	}}
	for i, ms := range passing {
		s.Cases = append(s.Cases, CaseResult{Name: fmt.Sprintf("passes-%d", i), Status: CasePassed,
			DurationMillis: millis(ms)})
	}
	s.Tests = int64(len(s.Cases))

	return s
}

// A JOB'S RESULTS COME BACK AS ENCODED: a duration of zero stays zero and an absent
// one stays absent, which the report says apart.
func TestTestResultsRoundTrip(t *testing.T) {
	t.Parallel()

	r := TestResults{Suites: []SuiteCases{aSuite("a", 0, 12)}, Dropped: DroppedCases{Suites: 2, Cases: 3}}

	_, data, err := EncodeTestResults(r, MaxTestResultsBytes)
	if err != nil {
		t.Fatalf("EncodeTestResults: %v", err)
	}

	got, err := DecodeTestResults(data)
	if err != nil {
		t.Fatalf("DecodeTestResults: %v", err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("read back %+v, want %+v", got, r)
	}
	if got.Suites[0].Cases[1].DurationMillis != nil || *got.Suites[0].Cases[2].DurationMillis != 0 {
		t.Error("an absent duration and a duration of zero did not stay apart")
	}
}

// THE GUEST WROTE ALL OF IT, so each thing a reader relies on is refused, by the
// sentinel for it and without quoting a byte of what was sent.
func TestTestResultsAreHeldToTheirSchema(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"

	for name, tc := range map[string]struct {
		change func(*SuiteCases)
		want   error
	}{
		"a status outside the set": {func(s *SuiteCases) { s.Cases[0].Status = secret }, ErrOutOfRange},
		"a negative duration":      {func(s *SuiteCases) { s.Cases[0].DurationMillis = millis(-1) }, ErrOutOfRange},
		"a duration past the bound": {
			func(s *SuiteCases) { s.Cases[0].DurationMillis = millis(MaxValue + 1) },
			ErrOutOfRange,
		},
		"a passing case's message": {func(s *SuiteCases) { s.Cases[2].Message = secret }, ErrInconsistent},
		"a control character":      {func(s *SuiteCases) { s.Cases[0].Message = "a\nforged line " + secret }, ErrControlCharacter},
		"a message past its bound": {
			func(s *SuiteCases) { s.Cases[0].Message = strings.Repeat("x", MaxCaseMessageBytes+1) },
			ErrNameTooLong,
		},
		"a name past its bound": {
			func(s *SuiteCases) { s.Cases[0].Name = strings.Repeat("x", MaxFailedNameBytes+1) },
			ErrNameTooLong,
		},
		"a case with no name":          {func(s *SuiteCases) { s.Cases[0].Name = "" }, ErrEmpty},
		"a suite with no name":         {func(s *SuiteCases) { s.Name = "" }, ErrEmpty},
		"a negative count":             {func(s *SuiteCases) { s.Failures = -1 }, ErrOutOfRange},
		"a list that is not complete":  {func(s *SuiteCases) { s.Tests++ }, ErrInconsistent},
		"a list longer than its count": {func(s *SuiteCases) { s.Tests-- }, ErrInconsistent},
	} {
		s := aSuite("a", 1)
		tc.change(&s)

		err := validateCases([]SuiteCases{s}, "suites")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: refused with %v, want %v", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal quotes what the guest wrote: %v", name, err)
		}
	}

	// And the one that passes is held to none of them.
	if err := validateCases([]SuiteCases{aSuite("a", 1)}, "suites"); err != nil {
		t.Errorf("the suite every case above breaks is refused: %v", err)
	}
}

// A LIST WHOSE COUNT THE GUEST CUT IS NOT COMPLETE, and is kept for what it holds.
func TestAnIncompleteSuiteMayNameFewerCasesThanItCounts(t *testing.T) {
	t.Parallel()

	s := aSuite("a", 1)
	s.Tests += 10
	s.Complete = false

	if err := validateCases([]SuiteCases{s}, "suites"); err != nil {
		t.Errorf("refused: %v", err)
	}
}

func TestSuitesAndCasesAreBounded(t *testing.T) {
	t.Parallel()

	suites := make([]SuiteCases, MaxBatchCaseSuites+1)
	for i := range suites {
		suites[i] = SuiteCases{Name: "s"}
	}
	if err := validateCases(suites, "suites"); !errors.Is(err, ErrTooMany) {
		t.Errorf("%d suites refused with %v, want ErrTooMany", len(suites), err)
	}

	whole := SuiteCases{Name: "s", Tests: MaxBatchCases + 1, Complete: true}
	whole.Cases = make([]CaseResult, MaxBatchCases+1)
	for i := range whole.Cases {
		whole.Cases[i] = CaseResult{Name: "c", Status: CasePassed}
	}
	if err := validateCases([]SuiteCases{whole}, "suites"); !errors.Is(err, ErrTooMany) {
		t.Errorf("%d cases refused with %v, want ErrTooMany", len(whole.Cases), err)
	}
}

// names is every case's name in suites.
func names(suites []SuiteCases) []string {
	var out []string

	for i := range suites {
		for j := range suites[i].Cases {
			out = append(out, suites[i].Cases[j].Name)
		}
	}

	return out
}

// WHAT IS GIVEN UP IS WHAT IS LEAST WORTH KEEPING, in the order the doc comment says:
// the fastest cases that did not fail, a case that says no duration first; then the
// messages; then the later failures; then the suites.
func TestCasesAreShedInOrderOfWhatTheySay(t *testing.T) {
	t.Parallel()

	suites := []SuiteCases{aSuite("a", 50, 1, 900)}
	suites[0].Cases = append(suites[0].Cases, CaseResult{Name: "second failure", Status: CaseErrored, Message: "boom"})
	suites[0].Tests = int64(len(suites[0].Cases))

	if !ShedCases(&suites) {
		t.Fatal("nothing was shed")
	}
	// Four cases did not fail: the skip with no duration and the two fastest go.
	if got, want := names(suites), []string{"fails", "passes-0", "passes-2", "second failure"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after one shed the cases are %v, want %v", got, want)
	}
	if suites[0].Complete {
		t.Error("a suite that lost cases is still Complete")
	}

	ShedCases(&suites)
	if got, want := names(suites), []string{"fails", "passes-2", "second failure"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after two sheds the cases are %v, want %v", got, want)
	}
	ShedCases(&suites)
	if got, want := names(suites), []string{"fails", "second failure"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after three sheds the cases are %v, want %v", got, want)
	}
	if suites[0].Cases[0].Message == "" {
		t.Fatal("a message went before every passing case had")
	}

	ShedCases(&suites)
	if suites[0].Cases[0].Message != "" || suites[0].Cases[1].Message != "" {
		t.Fatal("the messages stayed once only failures were left")
	}

	ShedCases(&suites)
	if got, want := names(suites), []string{"fails"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the later failure stayed: %v", got)
	}

	ShedCases(&suites)
	if len(suites) != 1 || suites[0].Cases != nil {
		t.Fatalf("after the last failure went, %+v", suites)
	}

	ShedCases(&suites)
	if suites != nil {
		t.Fatalf("the suites stayed: %+v", suites)
	}
	if ShedCases(&suites) {
		t.Error("shedding nothing says something was given up")
	}
}

// RESULTS LARGER THAN THE BOUND ARE CUT TO FIT, THE CALLER'S UNTOUCHED, and what was
// cut is counted, so a report can say its counts may fall short.
func TestResultsPastTheBoundAreCutToFit(t *testing.T) {
	t.Parallel()

	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	rng := rand.New(rand.NewPCG(1, 2))
	text := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(letters[rng.IntN(len(letters))])
		}

		return b.String()
	}

	s := SuiteCases{Name: "big", Failures: 1, Complete: true}
	s.Cases = append(s.Cases, CaseResult{Name: "the failure", Status: CaseFailed, Message: "kept"})
	for i := range 4000 {
		s.Cases = append(s.Cases, CaseResult{Name: text(150), Status: CasePassed, DurationMillis: millis(int64(i))})
	}
	s.Tests = int64(len(s.Cases))
	in := TestResults{Suites: []SuiteCases{s}}

	kept, data, err := EncodeTestResults(in, MaxTestResultsBytes)
	if err != nil {
		t.Fatalf("EncodeTestResults: %v", err)
	}
	if len(data) > MaxTestResultsBytes {
		t.Fatalf("encoded to %d bytes, over the bound", len(data))
	}
	if len(in.Suites[0].Cases) != 4001 || !in.Suites[0].Complete {
		t.Fatal("the caller's results were changed")
	}

	got, err := DecodeTestResults(data)
	if err != nil {
		t.Fatalf("DecodeTestResults: %v", err)
	}
	if !reflect.DeepEqual(got, kept) {
		t.Fatal("what it answers is not what it encoded")
	}

	suite := got.Suites[0]
	if suite.Complete || suite.Cases[0].Name != "the failure" || suite.Cases[0].Message != "kept" {
		t.Errorf("the suite is complete %t and starts %+v, want cut with its failure first", suite.Complete, suite.Cases[0])
	}
	if want := int64(4001 - len(suite.Cases)); got.Dropped.Cases != want || want == 0 {
		t.Errorf("Dropped counts %d cases, want %d", got.Dropped.Cases, want)
	}
	// THE SLOW ONES ARE THE ONES KEPT, so the slowest ranking loses nothing it shows.
	if last := suite.Cases[len(suite.Cases)-1]; *last.DurationMillis != 3999 {
		t.Errorf("the slowest case kept took %d ms, want the slowest of all", *last.DurationMillis)
	}
}

// A NUMBER RECEIVED TWICE COUNTS ONCE, AS THE MERGE COUNTS IT, the first received
// kept; and what is past the bounds is counted, the list it was cut from no longer
// Complete.
func TestTestResultsOfGathersBatches(t *testing.T) {
	t.Parallel()

	first := Batch{Seq: 2, Cases: []SuiteCases{aSuite("a", 1)}}
	again := Batch{Seq: 2, Cases: []SuiteCases{aSuite("other", 1)}}
	earlier := Batch{Seq: 1}

	got, ok := TestResultsOf([]Batch{earlier, first, again})
	if !ok || len(got.Suites) != 1 || got.Suites[0].Name != "a" {
		t.Fatalf("got %+v, %t, want the first received's one suite", got, ok)
	}

	if _, ok := TestResultsOf([]Batch{earlier}); ok {
		t.Error("batches that carried no cases are results")
	}

	big := SuiteCases{Name: "big", Tests: MaxBatchCases, Complete: true, Cases: make([]CaseResult, MaxBatchCases)}
	for i := range big.Cases {
		big.Cases[i] = CaseResult{Name: "c", Status: CasePassed}
	}
	got, ok = TestResultsOf([]Batch{{Seq: 1, Cases: []SuiteCases{big}}, first})
	if !ok || len(got.Suites) != 2 || got.Suites[1].Cases != nil || got.Suites[1].Complete {
		t.Fatalf("a suite past the case bound kept %+v", got.Suites[len(got.Suites)-1])
	}
	if got.Dropped.Cases != int64(len(first.Cases[0].Cases)) {
		t.Errorf("Dropped counts %d cases, want %d", got.Dropped.Cases, len(first.Cases[0].Cases))
	}
}

// A BATCH WITH NO CASES NAMES NO FIELD AN OLDER NODE REFUSES, which is every batch but
// the last and every batch to a node that does not offer them; one with cases reads back.
func TestABatchNamesCasesOnlyWhenItCarriesThem(t *testing.T) {
	t.Parallel()

	old := batchAt(1)
	data, err := EncodeBatch(old)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if strings.Contains(mustInflate(t, data), `"cases"`) {
		t.Fatal("a batch with no cases names the field, which an older node refuses")
	}

	withCases := batchAt(2)
	withCases.Cases = []SuiteCases{aSuite("a", 1)}
	data, err = EncodeBatch(withCases)
	if err != nil {
		t.Fatalf("EncodeBatch with cases: %v", err)
	}
	got, err := DecodeBatch(data)
	if err != nil {
		t.Fatalf("DecodeBatch with cases: %v", err)
	}
	if !reflect.DeepEqual(got.Cases, withCases.Cases) {
		t.Errorf("the cases read back %+v", got.Cases)
	}

}

func mustInflate(t *testing.T, data []byte) string {
	t.Helper()

	raw, err := inflate(data, MaxReportBytes, MaxReportInflatedBytes)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}

	return string(raw)
}

// AN UNKNOWN KEY IS REFUSED BEFORE ANYTHING IS ALLOCATED FOR IT, in test results as in
// a batch.
func TestTestResultsDecodeStrictly(t *testing.T) {
	t.Parallel()

	// What the encoder writes decodes, so each refusal below is about what it changes.
	if _, err := DecodeTestResults(deflated(t, []byte(`{"dropped":{"suites":0,"cases":0}}`))); err != nil {
		t.Fatalf("the encoder's own spelling is refused: %v", err)
	}

	for name, tc := range map[string]struct {
		raw  string
		want error
	}{
		"an unknown field":      {`{"dropped":{"suites":0,"cases":0},"later":1}`, ErrUnknownField},
		"a null list":           {`{"suites":null,"dropped":{"suites":0,"cases":0}}`, ErrNotCanonical},
		"an empty list spelled": {`{"suites":[],"dropped":{"suites":0,"cases":0}}`, ErrNotCanonical},
		"no dropped":            {`{}`, ErrNotCanonical},
	} {
		if _, err := DecodeTestResults(deflated(t, []byte(tc.raw))); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", name, err, tc.want)
		}
	}
}

// A COUNT OF WHAT WAS LEFT OUT STOPS AT ITS BOUND, because past it the whole result would
// refuse to encode, the cases it holds with it.
func TestACountOfWhatWasLeftOutSaturates(t *testing.T) {
	t.Parallel()

	got, ok := TestResultsOf([]Batch{
		{Seq: 1, CasesDropped: DroppedCases{Suites: MaxValue, Cases: MaxValue}},
		{Seq: 2, CasesDropped: DroppedCases{Suites: 1, Cases: 1}, Cases: []SuiteCases{aSuite("a", 1)}},
	})
	if !ok || got.Dropped != (DroppedCases{Suites: MaxValue, Cases: MaxValue}) {
		t.Fatalf("got %+v, %t, want both counts at the bound", got.Dropped, ok)
	}
	if _, _, err := EncodeTestResults(got, MaxTestResultsBytes); err != nil {
		t.Errorf("results at the bound do not encode: %v", err)
	}
}

// AN AGENT THAT LEFT EVERYTHING OUT STILL SAYS SO: drops alone are results, so a reader
// can tell them from a job that reported nothing.
func TestDropsAloneAreResults(t *testing.T) {
	t.Parallel()

	got, ok := TestResultsOf([]Batch{{Seq: 1, CasesDropped: DroppedCases{Suites: 2, Cases: 9}}})
	if !ok || got.Dropped.Suites != 2 || got.Dropped.Cases != 9 {
		t.Errorf("got %+v, %t", got, ok)
	}
}

// AN ADD PAST THE BOUND STOPS AT IT, from a count well below it too, which is the one the
// shedding of results makes.
func TestAnAddStopsAtTheBound(t *testing.T) {
	t.Parallel()

	d := DroppedCases{Suites: MaxValue - 1, Cases: MaxValue}
	d.Add(5, 1)
	if d != (DroppedCases{Suites: MaxValue, Cases: MaxValue}) {
		t.Errorf("past the bound: %+v", d)
	}

	d = DroppedCases{}
	d.Add(2, 3)
	if d != (DroppedCases{Suites: 2, Cases: 3}) {
		t.Errorf("below the bound: %+v", d)
	}
}
