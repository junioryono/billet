package state

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/joblog"
)

var logDay = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func aLogIndex(t *testing.T) (*LogIndex, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "logindex")

	x, err := OpenLogIndex(dir)
	if err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	t.Cleanup(func() {
		if err := x.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return x, dir
}

func addLines(t *testing.T, x *LogIndex, lease string, first int64, at time.Time, step int64, name string, lines ...string) {
	t.Helper()

	c := &joblog.Chunk{Seq: 1, Step: step, StepName: name}
	for _, text := range lines {
		c.Lines = append(c.Lines, joblog.Line{Text: text})
	}

	if err := x.Add(t.Context(), LogIndexChunk{Lease: lease, FirstLine: first, Chunk: c, ReceivedAt: at}); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// search is every hit of a person's query, newest first.
func search(t *testing.T, dir string, must, mustNot []string, since time.Time) []LogHit {
	t.Helper()

	match, err := BuildLogMatch(must, mustNot)
	if err != nil {
		t.Fatalf("BuildLogMatch: %v", err)
	}

	var out []LogHit

	partial, err := SearchLogIndex(t.Context(), dir, LogSearch{Match: match, Since: since, MaxScan: 1000,
		Visit: func(h LogHit) bool {
			out = append(out, h)

			return true
		}})
	if err != nil {
		t.Fatalf("SearchLogIndex: %v", err)
	}
	if partial {
		t.Fatal("a search inside its bound says it is partial")
	}

	return out
}

// A LINE IS FOUND BY A TERM, A PHRASE OR A PART OF A WORD, in any case, and said to be whose:
// its lease, its number, its step and its level. The text is not there to read.
func TestALineIsFoundAndSaidToBeWhose(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 2, "Run tests",
		"go test ./...", "Error: connection reset by peer", "##[warning]deprecated")

	for _, must := range [][]string{{"connection reset"}, {"CONNECTION"}, {"onnect"}, {"reset", "peer"}} {
		got := search(t, dir, must, nil, logDay.Add(-time.Hour))
		if len(got) != 1 {
			t.Errorf("%v found %+v, want the one line", must, got)

			continue
		}
		h := got[0]
		if h.Lease != "lease-a" || h.Line != 1 || h.Level != joblog.LevelError || h.Step != 2 ||
			h.StepName != "Run tests" || !h.ReceivedAt.Equal(logDay) {
			t.Errorf("%v found %+v", must, h)
		}
	}

	if got := search(t, dir, []string{"deprecated"}, nil, logDay.Add(-time.Hour)); len(got) != 1 ||
		got[0].Line != 2 || got[0].Level != joblog.LevelWarning {
		t.Errorf("the warning is %+v", got)
	}
	if got := search(t, dir, []string{"zzzzz"}, nil, logDay.Add(-time.Hour)); len(got) != 0 {
		t.Errorf("a term in no line found %+v", got)
	}
}

// AN EXCLUSION TAKES A LINE OUT, AND NOTHING IN A TERM IS AN OPERATOR: a job's words can be
// searched for whatever they hold.
func TestAnExclusionAndATermAreWhatTheySay(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 0, "", "error: timeout", "error: reset", `say "NOT quoted" OR this`)

	if got := search(t, dir, []string{"error"}, []string{"timeout"}, logDay.Add(-time.Hour)); len(got) != 1 || got[0].Line != 1 {
		t.Errorf("an exclusion found %+v", got)
	}
	if got := search(t, dir, []string{`"NOT quoted" OR`}, nil, logDay.Add(-time.Hour)); len(got) != 1 || got[0].Line != 2 {
		t.Errorf("a term holding quotes and operators found %+v", got)
	}
}

// WHAT A TRIGRAM CANNOT FIND IS REFUSED, BY THE SENTINEL, measured to answer nothing rather
// than an error, which would read as no such line; so is a query of only exclusions, which
// is a syntax error.
func TestWhatCannotBeSearchedIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ must, mustNot []string }{
		"nothing to find":    {nil, []string{"error"}},
		"too short":          {[]string{"ab"}, nil},
		"too short to leave": {[]string{"error"}, []string{"ab"}},
		// SIX BYTES, TWO CHARACTERS, and the tokenizer walks characters.
		"too few characters": {[]string{"日本"}, nil},
	} {
		if _, err := BuildLogMatch(tc.must, tc.mustNot); !errors.Is(err, ErrLogQuery) {
			t.Errorf("%s: %v, want ErrLogQuery", name, err)
		}
	}

	if got, err := BuildLogMatch([]string{"abc", `d"ef`}, []string{"ghi"}); err != nil || got != `"abc" "d""ef" NOT "ghi"` {
		t.Errorf("the match is %q, %v", got, err)
	}
}

// A NODE RE-SHIPPING A CHUNK ADDS NOTHING: the same line is not found twice from one file.
func TestARepeatedChunkAddsNothing(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 0, "", "error: once")
	addLines(t, x, "lease-a", 0, logDay, 0, "", "error: once")

	if got := search(t, dir, []string{"once"}, nil, logDay.Add(-time.Hour)); len(got) != 1 {
		t.Errorf("a repeat found %d hits, want 1", len(got))
	}
}

// A LINE'S NUMBER IS WHAT THE NODE GAVE IT, however the chunks arrive, and each stays with its
// own step.
func TestChunksKeepTheirNumbersAndSteps(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 5, logDay, 1, "second", "marker in second")
	addLines(t, x, "lease-a", 0, logDay, 0, "first", "padding", "marker in first")
	addLines(t, x, "lease-b", 0, logDay, 0, "other", "marker in other")

	got := search(t, dir, []string{"marker"}, nil, logDay.Add(-time.Hour))
	want := map[string]struct {
		line int64
		step string
	}{"lease-a/5": {5, "second"}, "lease-a/1": {1, "first"}, "lease-b/0": {0, "other"}}

	if len(got) != len(want) {
		t.Fatalf("found %+v", got)
	}
	for _, h := range got {
		key := h.Lease + "/" + string(rune('0'+h.Line))
		w, ok := want[key]
		if !ok || w.line != h.Line || w.step != h.StepName {
			t.Errorf("found %+v, not one of %v", h, want)
		}
	}
}

// ONE FILE A DAY, SEARCHED ONLY INSIDE ITS WINDOW AND NEWEST FIRST; removing what is past the
// retention or the bound takes whole days, and never the one being written.
func TestDaysAreSearchedAndRemovedWhole(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	for i, age := range []int{3, 2, 1, 0} {
		addLines(t, x, "lease-"+string(rune('a'+i)), 0, logDay.AddDate(0, 0, -age), 0, "", "marker line")
	}

	days, err := logIndexDays(dir)
	if err != nil || len(days) != 4 {
		t.Fatalf("days %v, %v", days, err)
	}

	got := search(t, dir, []string{"marker"}, nil, logDay.AddDate(0, 0, -1))
	if len(got) != 2 || got[0].Lease != "lease-d" || got[1].Lease != "lease-c" {
		t.Errorf("the last two days found %+v, want newest first", got)
	}

	removed, err := x.Retain(logDay.AddDate(0, 0, -2), 1<<40)
	if err != nil || removed != 1 {
		t.Errorf("past the retention removed %d, %v; want 1", removed, err)
	}
	if got := search(t, dir, []string{"marker"}, nil, logDay.AddDate(0, 0, -10)); len(got) != 3 {
		t.Errorf("after the retention found %+v, want three days", got)
	}

	removed, err = x.Retain(logDay.AddDate(0, 0, -10), 1)
	if err != nil || removed != 2 {
		t.Errorf("past the bound removed %d, %v; want every day but the one being written", removed, err)
	}
	got = search(t, dir, []string{"marker"}, nil, logDay.AddDate(0, 0, -10))
	if len(got) != 1 || got[0].Lease != "lease-d" {
		t.Errorf("the day being written went: %+v", got)
	}
	// The day is still being written: an Add after a removal lands.
	addLines(t, x, "lease-e", 0, logDay, 0, "", "marker again")
	if got := search(t, dir, []string{"again"}, nil, logDay.Add(-time.Hour)); len(got) != 1 {
		t.Errorf("the file being written stopped taking lines: %+v", got)
	}
}

// A DAY WITH NOTHING IN IT DOES NOT END A SEARCH OF THE OTHERS: one removed since its name was read
// is not created again, and one whose schema is still being written is empty. A day that cannot be
// read for any other reason does end it, and says so.
func TestAnEmptyDayIsNotAFailedSearch(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 0, "", "marker line")

	gone := filepath.Join(dir, "2000-01-01.db")

	visit := func(LogHit) bool { return true }

	more, stopped, err := searchLogDay(t.Context(), gone, LogSearch{Match: `"marker"`, Visit: visit}, new(int))
	if err != nil || more || stopped {
		t.Errorf("a day removed under the search: %t, %t, %v", more, stopped, err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("searching a removed day brought it back: %v", err)
	}

	bare := filepath.Join(dir, "2000-01-02.db")
	db, err := sql.Open("sqlite", dsn(bare))
	if err != nil {
		t.Fatal(err)
	}
	if err := logIndexExec(t.Context(), db, "CREATE TABLE other (a TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := searchLogDay(t.Context(), bare, LogSearch{Match: `"marker"`, Visit: visit}, new(int)); err != nil {
		t.Errorf("a day with no schema yet: %v", err)
	}

	if got := search(t, dir, []string{"marker"}, nil, logDay.AddDate(-40, 0, 0)); len(got) != 1 {
		t.Errorf("the search of the other days found %+v", got)
	}

	garbage := filepath.Join(dir, "2000-01-03.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := searchLogDay(t.Context(), garbage, LogSearch{Match: `"marker"`, Visit: visit}, new(int)); err == nil {
		t.Error("a day that is no database read as empty")
	}
}

// THE DAY LAST WRITTEN EXPIRES TOO: it stays the day being written for as long as nothing else
// arrives, so an idle controller would otherwise keep it past any retention. A later line opens
// its day afresh.
func TestTheLastDayWrittenExpires(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 0, "", "marker line")

	removed, err := x.Retain(logDay.AddDate(0, 0, 1), 1<<40)
	if err != nil || removed != 1 {
		t.Fatalf("past the retention removed %d, %v; want the one day", removed, err)
	}

	if days, err := logIndexDays(dir); err != nil || len(days) != 0 {
		t.Errorf("days left %v, %v", days, err)
	}

	addLines(t, x, "lease-b", 0, logDay.AddDate(0, 0, 1), 0, "", "marker again")
	if got := search(t, dir, []string{"again"}, nil, logDay); len(got) != 1 {
		t.Errorf("a line after the removal found %+v", got)
	}
}

// A SEARCH STOPS WHEN ITS CALLER SAYS, AND ITS OWN BOUND SAYS SO, which a reader must not take
// for every match.
func TestASearchStopsAndSaysWhy(t *testing.T) {
	t.Parallel()

	x, dir := aLogIndex(t)
	addLines(t, x, "lease-a", 0, logDay, 0, "", "marker one", "marker two", "marker three")

	match, err := BuildLogMatch([]string{"marker"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	count := 0

	partial, err := SearchLogIndex(t.Context(), dir, LogSearch{Match: match, Since: logDay.Add(-time.Hour), MaxScan: 1000,
		Visit: func(LogHit) bool {
			count++

			return false
		}})
	if err != nil || partial || count != 1 {
		t.Errorf("a caller's stop: %d hits, partial %t, %v", count, partial, err)
	}

	count = 0

	partial, err = SearchLogIndex(t.Context(), dir, LogSearch{Match: match, Since: logDay.Add(-time.Hour), MaxScan: 2,
		Visit: func(LogHit) bool {
			count++

			return true
		}})
	if err != nil || !partial || count != 2 {
		t.Errorf("past the bound: %d hits, partial %t, %v", count, partial, err)
	}
}

// NO INDEX HERE IS NOT NO MATCHES, and an id survives a reopen, since a new one has every node
// send everything again.
func TestAnAbsentIndexAndAnIDAreWhatTheySay(t *testing.T) {
	t.Parallel()

	if _, err := SearchLogIndex(t.Context(), filepath.Join(t.TempDir(), "absent"), LogSearch{Match: `"abc"`}); !errors.Is(err, ErrLogIndexAbsent) {
		t.Errorf("no index here: %v", err)
	}

	x, dir := aLogIndex(t)
	if x.ID() == "" {
		t.Fatal("an index with no id")
	}

	again, err := OpenLogIndex(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.ID() != x.ID() {
		t.Errorf("the id moved from %s to %s", x.ID(), again.ID())
	}

	if err := os.WriteFile(filepath.Join(dir, logIndexIDFile), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLogIndex(dir); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an id file with nothing in it: %v, want refused, never a new id", err)
	}
}

// A CHUNK PAST WHAT A ROWID CAN NUMBER IS REFUSED, or its lines would be read as another job's.
func TestAChunkPastTheRowidIsRefused(t *testing.T) {
	t.Parallel()

	x, _ := aLogIndex(t)
	c := &joblog.Chunk{Seq: 1, Lines: []joblog.Line{{Text: "line"}}}

	for _, first := range []int64{-1, joblog.MaxLines} {
		if err := x.Add(t.Context(), LogIndexChunk{Lease: "l", FirstLine: first, Chunk: c, ReceivedAt: logDay}); !errors.Is(err, ErrLogLineRange) {
			t.Errorf("first line %d: %v", first, err)
		}
	}
}
