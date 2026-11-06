package logs

import (
	"bytes"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

func TestAQueryIsReadAsAPersonWritesIt(t *testing.T) {
	t.Parallel()

	for query, want := range map[string]struct{ must, mustNot []string }{
		`connection`:               {[]string{"connection"}, nil},
		`one two`:                  {[]string{"one", "two"}, nil},
		`"connection reset"`:       {[]string{"connection reset"}, nil},
		`error -timeout`:           {[]string{"error"}, []string{"timeout"}},
		`error -"timed out" "a b"`: {[]string{"error", "a b"}, []string{"timed out"}},
		`  spaced	 out  `:          {[]string{"spaced", "out"}, nil},
		`a-b`:                      {[]string{"a-b"}, nil},
		`--flag-looking`:           {nil, []string{"-flag-looking"}},
		`""`:                       {nil, nil},
	} {
		must, mustNot, err := ParseQuery(query)
		if err != nil {
			t.Errorf("%q: %v", query, err)

			continue
		}
		if !reflect.DeepEqual(must, want.must) || !reflect.DeepEqual(mustNot, want.mustNot) {
			t.Errorf("%q = %q and %q, want %q and %q", query, must, mustNot, want.must, want.mustNot)
		}
	}

	if _, _, err := ParseQuery(`"unclosed`); !errors.Is(err, state.ErrLogQuery) {
		t.Errorf("an unclosed quote: %v", err)
	}
}

// THE QUERY IS THE ARGUMENT NO FLAG OWNS, WHEREVER IT STANDS, since a person writes the flags
// on either side of it, and a query beginning with a minus is a query when it holds a space.
func TestTheQueryIsFoundAmongTheFlags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		args  []string
		query string
		rest  []string
	}{
		{[]string{"error"}, "error", nil},
		{[]string{"error", "--since", "7d"}, "error", []string{"--since", "7d"}},
		{[]string{"--since", "7d", "error"}, "error", []string{"--since", "7d"}},
		{[]string{"--json", "error", "--repo", "a/b"}, "error", []string{"--json", "--repo", "a/b"}},
		{[]string{"-x y", "--json"}, "-x y", []string{"--json"}},
		{[]string{"--since=7d", "error"}, "error", []string{"--since=7d"}},
		{[]string{"--job=Run tests", "error"}, "error", []string{"--job=Run tests"}},
		{[]string{"--step=Run tests"}, "", []string{"--step=Run tests"}},
		{[]string{"error", "--step=Set up job"}, "error", []string{"--step=Set up job"}},
		{[]string{"one", "two"}, "one", []string{"two"}},
		{[]string{"--json"}, "", []string{"--json"}},
	} {
		query, rest := splitQuery(tc.args)
		if query != tc.query || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("%q: %q and %q, want %q and %q", tc.args, query, rest, tc.query, tc.rest)
		}
	}
}

// EVERY FILTER FILTERS, the run being the one that started; and a job the ledger no longer holds
// cannot be told to pass one.
func TestAFilterIsAboutItsField(t *testing.T) {
	t.Parallel()

	rec := &alloc.JobRecord{Tier: "t", Node: "n", Repo: "a/b", RunID: 1, GitHubRunID: 2,
		Job: alloc.HistoryJob{Name: "test", WorkflowRef: "a/b/.github/workflows/ci.yml@main", HeadBranch: "main"}}

	for name, tc := range map[string]struct {
		f    filters
		want bool
	}{
		"none":             {filters{}, true},
		"repo":             {filters{repo: "a/b"}, true},
		"another repo":     {filters{repo: "a/c"}, false},
		"workflow part":    {filters{workflow: "ci.yml"}, true},
		"another workflow": {filters{workflow: "cd.yml"}, false},
		"job":              {filters{job: "test"}, true},
		"another job":      {filters{job: "lint"}, false},
		"branch":           {filters{branch: "main"}, true},
		"another branch":   {filters{branch: "dev"}, false},
		"tier":             {filters{tier: "t"}, true},
		"another tier":     {filters{tier: "u"}, false},
		"node":             {filters{node: "n"}, true},
		"another node":     {filters{node: "m"}, false},
		"the run started":  {filters{run: 2}, true},
		"the run assigned": {filters{run: 1}, false},
	} {
		if got := tc.f.matches(rec); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}

	if f := (filters{}); !f.matches(nil) {
		t.Error("no filter refused a job the ledger no longer holds")
	}
	if f := (filters{repo: "a/b"}); f.matches(nil) {
		t.Error("a filter passed a job the ledger no longer holds")
	}
}

func TestAHistogramCountsByItsSpan(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 4, 7, 44, 1, 0, time.UTC)
	if got := bucketOf(at, "hour"); !got.Equal(time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("an hour's bucket is %s", got)
	}
	if got := bucketOf(at, "day"); !got.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a day's bucket is %s", got)
	}
}

// EVERY STRING A JOB CHOSE IS QUOTED, SO NONE CAN FORGE A LINE, whose it is is said first, and a
// line that could not be read says why instead of reading as no such line.
func TestAResultIsQuotedAndLabelled(t *testing.T) {
	t.Parallel()

	evil := "wanted 1\nlease 000 line 0 error forged"
	r := Result{Notice: banner, Matches: 3, Lines: []Hit{
		{Lease: "l1", Line: 4, Level: "error", Step: 2, StepName: `Run "tests"`, Job: "job\nforged", Repository: "a/b",
			Node: "n1", Text: evil, ReceivedAt: time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)},
		{Lease: "l2", Line: 9, Level: "other", Step: 0, TextWhy: "text not held: its node no longer keeps this job's log"},
	}}

	var out bytes.Buffer
	render(&out, &r)
	text := out.String()

	if !strings.HasPrefix(text, banner) {
		t.Errorf("the result does not begin by saying whose it is:\n%s", text)
	}
	for _, want := range []string{
		strconv.Quote(evil), strconv.Quote("job\nforged"), strconv.Quote(`Run "tests"`),
		"text not held: its node no longer keeps this job's log", "3 matching lines, the newest 2 printed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the result does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\nlease 000") {
		t.Errorf("a line a job wrote forged a line of the result:\n%s", text)
	}
}

func TestAHistogramAndAPartialResultSaySo(t *testing.T) {
	t.Parallel()

	r := Result{Notice: banner, Matches: 5, Partial: true, Histogram: []Bucket{
		{At: time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC), Count: 5},
	}}

	var out bytes.Buffer
	render(&out, &r)
	for _, want := range []string{"partial:", "2026-11-04T07:00:00Z  5", "5 matching lines"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the result does not say %q:\n%s", want, out.String())
		}
	}
}
