package guestagent

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// parsedCases is what one result file's text keeps of its cases.
func parsedCases(t *testing.T, text string) []guestreport.SuiteCases {
	t.Helper()

	w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}
	if _, ok := w.parse(strings.NewReader(text), "r.xml"); !ok {
		t.Fatalf("the file did not parse: %s", text)
	}

	return w.parsed.suites
}

// A CASE IS KEPT WITH WHAT IT CAME TO, how long it took and why it failed, and a
// message is the attribute or, where a tool leaves it out, the start of the text. A
// time nobody can read is no duration, which a report says apart from a duration of
// zero.
func TestACaseIsKeptWithItsStatusDurationAndMessage(t *testing.T) {
	t.Parallel()

	got := parsedCases(t, `<testsuite name="pkg">
		<testcase classname="pkg" name="Passes" time="1.5"/>
		<testcase classname="pkg" name="Fast" time="0"/>
		<testcase classname="pkg" name="NoTime"/>
		<testcase classname="pkg" name="BadTime" time="1,234.5"/>
		<testcase classname="pkg" name="Fails" time="0.25"><failure message="wanted 1">got 2</failure></testcase>
		<testcase classname="pkg" name="TextOnly"><failure>
			the first line of the text
		</failure></testcase>
		<testcase classname="pkg" name="Errors"><error message="boom"/></testcase>
		<testcase classname="pkg" name="Skips"><skipped/></testcase>
	</testsuite>`)
	if len(got) != 1 {
		t.Fatalf("kept %d suites, want 1", len(got))
	}

	s := got[0]
	if s.Name != "pkg" || s.Tests != 8 || s.Failures != 2 || s.Errors != 1 || s.Skipped != 1 || !s.Complete {
		t.Errorf("the suite is %+v, want its counts and Complete", s)
	}

	for i, want := range []struct {
		name, status string
		millis       int64
		message      string
	}{
		{"pkg.Passes", guestreport.CasePassed, 1500, ""},
		{"pkg.Fast", guestreport.CasePassed, 0, ""},
		{"pkg.NoTime", guestreport.CasePassed, -1, ""},
		{"pkg.BadTime", guestreport.CasePassed, -1, ""},
		{"pkg.Fails", guestreport.CaseFailed, 250, "wanted 1"},
		{"pkg.TextOnly", guestreport.CaseFailed, -1, "the first line of the text"},
		{"pkg.Errors", guestreport.CaseErrored, -1, "boom"},
		{"pkg.Skips", guestreport.CaseSkipped, -1, ""},
	} {
		c := s.Cases[i]
		millis := int64(-1)
		if c.DurationMillis != nil {
			millis = *c.DurationMillis
		}
		if c.Name != want.name || c.Status != want.status || millis != want.millis || c.Message != want.message {
			t.Errorf("case %d = %+v (%d ms), want %+v", i, c, millis, want)
		}
	}
}

// THE RESULT FILE IS THE JOB'S, so a message is cut and cleaned of what a terminal
// would act on, and a batch holding it still validates.
func TestACaseMessageIsCutAndCleaned(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", guestreport.MaxCaseMessageBytes+50)
	// U+009B is a terminal's control sequence introducer, which XML admits.
	got := parsedCases(t, `<testsuite name="pkg"><testcase name="T"><failure message="a&#10;b&#155;[31m`+long+
		`"/></testcase></testsuite>`)

	message := got[0].Cases[0].Message
	if len(message) > guestreport.MaxCaseMessageBytes || !strings.HasPrefix(message, "a?b?[31m") {
		t.Errorf("the message is %q (%d bytes)", message, len(message))
	}
	if _, err := guestreport.EncodeBatch(guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100,
		Cases: got}); err != nil {
		t.Errorf("a batch holding it is refused: %v", err)
	}
}

func TestACaseTimeIsADurationOnlyWhenItSaysOne(t *testing.T) {
	t.Parallel()

	for text, want := range map[string]int64{
		"0": 0, "0.0004": 0, "0.0005": 1, "1.5": 1500, " 2 ": 2000, "1e3": 1_000_000,
		"": -1, "soon": -1, "-1": -1, "NaN": -1, "Inf": -1, "1,234.5": -1, "1e300": -1,
	} {
		got := int64(-1)
		if m := caseMillis(text); m != nil {
			got = *m
		}
		if got != want {
			t.Errorf("%q reads as %d ms, want %d", text, got, want)
		}
	}
}

// A FILE THAT DOES NOT PARSE TO ITS END ADDS NO CASE, as it adds no suite: a count
// from half a file would be a claim, and so would half its cases.
func TestAFileCutShortAddsNoCase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for name, text := range map[string]string{
		"whole.xml": `<testsuite name="whole"><testcase name="T"/></testsuite>`,
		"cut.xml":   `<testsuite name="cut"><testcase name="U"/><testcase name="V">`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	log, _ := discardLog()
	suites, cases := collectJUnit([]string{root}, unhurried, log)
	if len(suites) != 1 || len(cases.suites) != 1 || cases.suites[0].Name != "whole" {
		t.Errorf("kept suites %+v and cases %+v, want only the whole file's", suites, cases)
	}
}

// ONE FILE OF MORE CASES THAN A BATCH HOLDS COSTS NO MORE THAN A BATCH, keeps its
// failures and its slowest, and is no longer Complete.
func TestASuitePastTheBoundKeepsItsFailuresAndItsSlowest(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(`<testsuite name="big">`)
	fmt.Fprintf(&b, `<testcase name="fails"><failure message="m"/></testcase>`)
	for i := range guestreport.MaxBatchCases + 100 {
		fmt.Fprintf(&b, `<testcase name="t%d" time="%d"/>`, i, i)
	}
	b.WriteString(`</testsuite>`)

	got := parsedCases(t, b.String())
	if len(got) != 1 {
		t.Fatalf("kept %d suites", len(got))
	}

	s := got[0]
	if s.Complete || s.Tests != guestreport.MaxBatchCases+101 || s.Failures != 1 {
		t.Errorf("the suite says complete %t, %d tests, %d failures", s.Complete, s.Tests, s.Failures)
	}
	if len(s.Cases) > guestreport.MaxBatchCases {
		t.Errorf("it holds %d cases, past the bound", len(s.Cases))
	}
	if s.Cases[0].Name != "fails" {
		t.Errorf("its first case is %q, want the failure kept", s.Cases[0].Name)
	}
	if last := s.Cases[len(s.Cases)-1]; last.Name != fmt.Sprintf("t%d", guestreport.MaxBatchCases+99) {
		t.Errorf("its last case is %q, want the slowest kept", last.Name)
	}
}

// PAST THE BOUND ON SUITES THE ONES WITH FAILURES ARE KEPT, as the suite counts rank
// them, and the cases of those kept are whole.
func TestSuitesPastTheBoundKeepTheFailingOnes(t *testing.T) {
	t.Parallel()

	var kept junitCases

	kept.keep(guestreport.SuiteCases{Name: "failing", Tests: 1, Failures: 1, Complete: true,
		Cases: []guestreport.CaseResult{{Name: "f", Status: guestreport.CaseFailed}}})
	for i := range guestreport.MaxBatchCaseSuites + 10 {
		kept.keep(guestreport.SuiteCases{Name: fmt.Sprintf("s%d", i), Tests: 1, Complete: true,
			Cases: []guestreport.CaseResult{{Name: "p", Status: guestreport.CasePassed}}})
	}

	if len(kept.suites) != guestreport.MaxBatchCaseSuites || kept.suites[0].Name != "failing" || !kept.suites[0].Complete {
		t.Errorf("kept %d suites, the first %+v", len(kept.suites), kept.suites[0])
	}
	// A SUITE LEFT OUT IS COUNTED, or a job that lost whole suites reads as complete.
	if kept.dropped.Suites != 11 || kept.dropped.Cases != 11 {
		t.Errorf("counted %+v left out, want 11 suites and 11 cases", kept.dropped)
	}
}

// THE CASES GIVE WAY BEFORE ANYTHING THE REPORT SHOWS: a batch over the limit keeps its
// suite counts and its failed names while passing cases remain to give up.
func TestABatchShedsCasesBeforeTheSuitesItsReportShows(t *testing.T) {
	t.Parallel()

	s := guestreport.SuiteCases{Name: "s", Failures: 1, Complete: true}
	s.Cases = append(s.Cases, guestreport.CaseResult{Name: "f", Status: guestreport.CaseFailed, Message: "m"})
	for i := range 2000 {
		s.Cases = append(s.Cases, guestreport.CaseResult{Name: fmt.Sprintf("case-%d-%x", i, i*2654435761),
			Status: guestreport.CasePassed})
	}
	s.Tests = int64(len(s.Cases))
	b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Cases: []guestreport.SuiteCases{s},
		Tests: []guestreport.SuiteResult{{Name: "s", Tests: s.Tests, Failures: 1, Failed: []string{"f"}}}}

	body, err := fit(&b, 4000, time.Time{})
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if len(body) > 4000 {
		t.Fatalf("encoded to %d bytes", len(body))
	}
	if len(b.Tests) != 1 || len(b.Tests[0].Failed) != 1 {
		t.Errorf("the suite counts gave way: %+v", b.Tests)
	}
	if len(b.Cases) != 1 || b.Cases[0].Complete || len(b.Cases[0].Cases) >= 2001 || b.Cases[0].Cases[0].Name != "f" {
		t.Errorf("the cases are %d, complete %t", len(b.Cases[0].Cases), b.Cases[0].Complete)
	}
}

// AN AGENT SENDS CASES ONLY TO A NODE THAT SAID IT KNOWS THEM, because one that does
// not refuses the whole batch and the job's suite counts with it. Every image and
// every node of either age meet in the fleet.
func TestCasesGoOnlyToANodeThatOffersThem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		schemas string
		carried bool
	}{{"[1]", false}, {"[1,2]", true}} {
		synctest.Test(t, func(t *testing.T) {
			node := newFakeNode(t)
			node.schemas = tc.schemas
			var log syncBuffer
			r := start(t, testConfig(t, node, &log))

			time.Sleep(15 * time.Second)
			synctest.Wait()

			if _, err := r.stop(); err != nil {
				t.Fatal(err)
			}

			_, posts := node.snapshot()
			final := posts[len(posts)-1]
			if !final.final || len(final.batch.Tests) == 0 {
				t.Fatalf("offered %s: the final batch is final %t with %d suites", tc.schemas, final.final,
					len(final.batch.Tests))
			}
			if got := len(final.batch.Cases) > 0; got != tc.carried {
				t.Errorf("offered %s: the final batch carries %d suites of cases, want carried %t", tc.schemas,
					len(final.batch.Cases), tc.carried)
			}
			for _, p := range posts[:len(posts)-1] {
				if len(p.batch.Cases) != 0 {
					t.Errorf("batch %d, before the last, carries cases", p.batch.Seq)
				}
			}
		})
	}
}

// CASES SHED TO FIT ARE COUNTED, and a batch past its deadline gives them all up at
// once and keeps what the report shows: a job of many cases costs its cases, never its
// suite counts. The deadline is checked before the encoding it would save.
func TestABatchPastItsDeadlineGivesUpItsCasesAndKeepsItsSuites(t *testing.T) {
	t.Parallel()

	s := guestreport.SuiteCases{Name: "s", Tests: 1, Failures: 1, Complete: true,
		Cases: []guestreport.CaseResult{{Name: "f", Status: guestreport.CaseFailed}}}
	b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Cases: []guestreport.SuiteCases{s},
		Tests: []guestreport.SuiteResult{{Name: "s", Tests: 1, Failures: 1, Failed: []string{"f"}}}}

	body, err := fit(&b, guestreport.MaxBatchBytes, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	got, err := guestreport.DecodeBatch(body)
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	if len(got.Cases) != 0 || got.CasesDropped != (guestreport.DroppedCases{Suites: 1, Cases: 1}) {
		t.Errorf("the batch holds cases %+v and counts %+v, want none and one of each", got.Cases, got.CasesDropped)
	}
	if len(got.Tests) != 1 || got.Tests[0].Failures != 1 {
		t.Errorf("the suite counts went too: %+v", got.Tests)
	}
}

// TWO TESTS NEVER READ AS ONE: a name past the bound keeps its identity in a digest of
// the whole, so a failure in one and a pass in the other is not a flaky test. Each stays
// within the bound, and a name that fits is unchanged.
func TestANameCutToFitStaysItsOwn(t *testing.T) {
	t.Parallel()

	class := strings.Repeat("c", guestreport.MaxFailedNameBytes)
	one, other := caseIdentity(class+"A", "T"), caseIdentity(class+"B", "T")
	if one == other {
		t.Errorf("two classes cut to one identity: %q", one)
	}
	if len(one) > guestreport.MaxFailedNameBytes {
		t.Errorf("an identity of %d bytes is past the bound", len(one))
	}
	if got := caseIdentity("pkg", "T"); got != "pkg.T" {
		t.Errorf("a name that fits reads %q", got)
	}
	if got := caseIdentity("", "T"); got != "T" {
		t.Errorf("a name with no class reads %q", got)
	}

	path := strings.Repeat("p", guestreport.MaxNameBytes)
	first, second := suiteIdentity(path+"/one", "f.xml"), suiteIdentity(path+"/two", "f.xml")
	if first == second || len(first) > guestreport.MaxNameBytes {
		t.Errorf("two suites read %q and %q", first, second)
	}
	if got := suiteIdentity("", "named-by-file.xml"); got != "named-by-file.xml" {
		t.Errorf("a suite with no name reads %q", got)
	}
}

// A NODE THAT REFUSES THE LAST BATCH GETS IT ONCE MORE WITHOUT ITS CASES, under one
// number, so what a report shows is not lost with them.
func TestARefusedFinalBatchIsResentWithoutItsCases(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		synctest.Test(t, func(t *testing.T) {
			node := newFakeNode(t)
			node.schemas = "[1,2]"
			node.postScript = []reply{{status: http.StatusNoContent}, {status: status}}

			var log syncBuffer

			r := start(t, testConfig(t, node, &log))

			time.Sleep(15 * time.Second)
			synctest.Wait()

			if _, err := r.stop(); err != nil {
				t.Fatal(err)
			}

			_, posts := node.snapshot()
			if len(posts) != 3 {
				t.Fatalf("answered %d: the node was sent %d batches, want a first, a refused and a resent", status, len(posts))
			}

			refused, resent := posts[1], posts[2]
			if !refused.final || !resent.final || refused.batch.Seq != resent.batch.Seq {
				t.Errorf("answered %d: the two final batches are %v #%d and %v #%d", status, refused.final,
					refused.batch.Seq, resent.final, resent.batch.Seq)
			}
			if len(refused.batch.Cases) == 0 || len(resent.batch.Cases) != 0 {
				t.Errorf("answered %d: the batches carry %d and %d suites of cases, want some and none", status,
					len(refused.batch.Cases), len(resent.batch.Cases))
			}
			// NEITHER FIELD OF SCHEMA 2, which is what lets a node rolled back since the
			// hello take it: the encoder leaves out only what is zero.
			if len(resent.batch.Tests) == 0 || resent.batch.CasesDropped != (guestreport.DroppedCases{}) {
				t.Errorf("answered %d: the resent batch holds %d suites and counts %+v dropped", status,
					len(resent.batch.Tests), resent.batch.CasesDropped)
			}
		})
	}
}

// A REFUSED BATCH WITH NO CASES TO DROP IS NOT SENT AGAIN: it would be refused the same.
func TestAFinalBatchIsResentOnlyWithCasesToDrop(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.postScript = []reply{{status: http.StatusNoContent}, {status: http.StatusBadRequest}}

		var log syncBuffer

		r := start(t, testConfig(t, node, &log))

		time.Sleep(15 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		if _, posts := node.snapshot(); len(posts) != 2 {
			t.Errorf("a node offering no cases was sent %d batches, want the refused one once", len(posts))
		}
	})
}

// WHAT A SUITE GIVES UP WHILE IT IS READ IS COUNTED, and a name the cleaning changed is
// marked apart, so "a\nb" and "a?b" stay two tests.
func TestWhatASuiteShedsWhileReadIsCounted(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(`<testsuite name="big">`)
	for i := range guestreport.MaxBatchCases + 1 {
		fmt.Fprintf(&b, `<testcase name="t%d" time="%d"/>`, i, i)
	}
	b.WriteString(`</testsuite>`)

	w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}
	if _, ok := w.parse(strings.NewReader(b.String()), "r.xml"); !ok {
		t.Fatal("the file did not parse")
	}
	kept := len(w.parsed.suites[0].Cases)
	if want := int64(guestreport.MaxBatchCases + 1 - kept); w.parsed.dropped.Cases != want || want == 0 {
		t.Errorf("counted %d cases shed, want %d", w.parsed.dropped.Cases, want)
	}

	if caseIdentity("", "a\nb") == caseIdentity("", "a?b") {
		t.Error("two names read as one once cleaned")
	}
}
