// Package logs is the `billet logs` family: search what jobs wrote to their logs, across
// every job a control plane's index holds.
//
// A LOG IS WHAT THE JOB WROTE. Every line is quoted as it is printed, and labelled as the
// job's. The index says which lines match; their text is read back from the node that ran
// the job, through the control plane on this host, and a line that cannot be read is said
// to be, since it is never no such line.
package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/state"
)

// banner is said wherever a line a job wrote is shown.
const banner = "What jobs wrote to their logs. A job can write anything there; billet does not act on it."

// The bounds of one search: the rows of the index it reads, and the hits it prints the text
// of, which each lease's node is asked for.
const (
	searchMaxScan = 5_000_000
	defaultHits   = 50
	searchMaxHits = 1_000
)

// Run is `billet logs`.
func Run(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet logs search '<query>' [--since 7d] [filters] [--histogram hour|day] [--json]")
	}
	if args[0] == "search" {
		return cmdSearch(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown logs command %q; try search", args[0])
}

// filters narrow a search to jobs and lines. An empty field filters nothing.
type filters struct {
	repo, workflow, job, branch, step, tier, node string
	run                                           int64
	level                                         string
}

func cmdSearch(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet logs search", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := fs.String("since", "7d", "search the jobs this far back, such as 7d or 36h")
	limit := fs.Int("limit", defaultHits, "how many matching lines to print")
	histogram := fs.String("histogram", "", "count the matches by hour or day, rather than print them")
	asJSON := fs.Bool("json", false, "print the result as one JSON object")

	var f filters

	fs.StringVar(&f.repo, "repo", "", "only this repository, as owner/name")
	fs.StringVar(&f.workflow, "workflow", "", "only workflows whose ref holds this, such as ci.yml")
	fs.StringVar(&f.job, "job", "", "only jobs of this name")
	fs.Int64Var(&f.run, "run", 0, "only this workflow run")
	fs.StringVar(&f.branch, "branch", "", "only this branch")
	fs.StringVar(&f.step, "step", "", "only steps of this name")
	fs.StringVar(&f.tier, "tier", "", "only this tier")
	fs.StringVar(&f.node, "node", "", "only this node")
	fs.StringVar(&f.level, "level", "", "only lines inferred to be this: error, warning or other")

	query, rest := splitQuery(args)
	if err := cli.Parse(fs, rest); err != nil {
		return err
	}

	if query == "" {
		return errors.New("usage: billet logs search '<query>' [--since 7d] [filters] [--histogram hour|day] [--json]")
	}

	span, err := cli.ParseSpan(*since)
	if err != nil {
		return fmt.Errorf("billet logs search: --since: %w", err)
	}

	if *histogram != "" && *histogram != "hour" && *histogram != "day" {
		return fmt.Errorf("billet logs search: --histogram takes hour or day, not %q", *histogram)
	}

	if *limit < 1 || *limit > searchMaxHits {
		return fmt.Errorf("billet logs search: --limit takes 1 to %d, not %d", searchMaxHits, *limit)
	}

	var level *joblog.Level

	if f.level != "" {
		l, ok := joblog.ParseLevel(f.level)
		if !ok {
			return fmt.Errorf("billet logs search: --level takes error, warning or other, not %q", f.level)
		}

		level = &l
	}

	must, mustNot, err := ParseQuery(query)
	if err != nil {
		return fmt.Errorf("billet logs search: %w", err)
	}

	match, err := state.BuildLogMatch(must, mustNot)
	if err != nil {
		return fmt.Errorf("billet logs search: %w", err)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("billet logs search runs on the control plane, and this config has no server section")
	}

	a, _, closeDB, err := app.OpenOperator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	s := search{cfg: cfg, jobs: newJobCache(a), filter: f, level: level, since: time.Now().Add(-span), limit: *limit,
		histogram: *histogram}

	result, err := s.run(ctx, match)
	if err != nil {
		return err
	}

	if *asJSON {
		body, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("encode the search's result: %w", err)
		}

		if _, err := fmt.Fprintf(env.Stdout, "%s\n", body); err != nil {
			return fmt.Errorf("write the search's result: %w", err)
		}

		return nil
	}

	render(env.Stdout, &result)

	return nil
}

// splitQuery takes the query out of a command line wherever it stands, since a flag after
// it would otherwise be read as part of it. The query is the first argument no flag owns.
func splitQuery(args []string) (string, []string) {
	takesValue := map[string]bool{
		"config": true, "since": true, "limit": true, "histogram": true, "repo": true, "workflow": true, "job": true,
		"run": true, "branch": true, "step": true, "tier": true, "node": true, "level": true,
	}

	var rest []string

	query := ""

	for i := 0; i < len(args); i++ {
		arg := args[i]

		name, inline := strings.CutPrefix(arg, "--")
		if !inline {
			name, inline = strings.CutPrefix(arg, "-")
		}

		// A FLAG'S OWN VALUE CAN HOLD A SPACE, which is otherwise what marks a query that
		// begins with a minus.
		flag, _, hasValue := strings.Cut(name, "=")

		switch {
		case inline && hasValue && takesValue[flag]:
			rest = append(rest, arg)
		case !inline || arg == "-" || arg == "--" || strings.Contains(arg, " "):
			if query != "" {
				return query, append(rest, args[i:]...)
			}

			query = arg
		case takesValue[name] && i+1 < len(args):
			rest = append(rest, arg, args[i+1])
			i++
		default:
			rest = append(rest, arg)
		}
	}

	return query, rest
}

// ParseQuery reads a person's query: whitespace separates terms, double quotes make a
// phrase, and a leading minus excludes. An unclosed quote is refused rather than guessed,
// because the answer would look like an answer.
func ParseQuery(query string) ([]string, []string, error) {
	var must, mustNot []string

	for i := 0; i < len(query); {
		switch query[i] {
		case ' ', '\t':
			i++

			continue
		}

		exclude := false
		if query[i] == '-' && i+1 < len(query) && query[i+1] != ' ' {
			exclude = true
			i++
		}

		var term string

		if query[i] == '"' {
			end := strings.IndexByte(query[i+1:], '"')
			if end < 0 {
				return nil, nil, fmt.Errorf("%w: a quote in it is not closed", state.ErrLogQuery)
			}

			term = query[i+1 : i+1+end]
			i += end + 2
		} else {
			end := strings.IndexAny(query[i:], " \t")
			if end < 0 {
				end = len(query) - i
			}

			term = query[i : i+end]
			i += end
		}

		if term == "" {
			continue
		}

		if exclude {
			mustNot = append(mustNot, term)
		} else {
			must = append(must, term)
		}
	}

	return must, mustNot, nil
}

// jobCache reads each job's history once: a search meets a job once per line it matched.
type jobCache struct {
	a     *alloc.Allocator
	known map[string]*alloc.JobRecord
}

func newJobCache(a *alloc.Allocator) *jobCache {
	return &jobCache{a: a, known: map[string]*alloc.JobRecord{}}
}

// of is a lease's job, nil when the ledger holds none: a line whose job has gone from the
// ledger cannot be told to pass a filter.
func (c *jobCache) of(ctx context.Context, lease string) (*alloc.JobRecord, error) {
	if rec, known := c.known[lease]; known {
		return rec, nil
	}

	rec, err := c.a.Job(ctx, lease)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		c.known[lease] = nil

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	c.known[lease] = &rec

	return &rec, nil
}

// matches says a job passes the job-level filters.
func (f *filters) matches(rec *alloc.JobRecord) bool {
	if rec == nil {
		return f.repo == "" && f.workflow == "" && f.job == "" && f.branch == "" && f.tier == "" && f.node == "" && f.run == 0
	}

	run := rec.GitHubRunID
	if run == 0 {
		run = rec.RunID
	}

	return (f.repo == "" || rec.Repo == f.repo) &&
		(f.workflow == "" || strings.Contains(rec.Job.WorkflowRef, f.workflow)) &&
		(f.job == "" || rec.Job.Name == f.job) &&
		(f.branch == "" || rec.Job.HeadBranch == f.branch) &&
		(f.tier == "" || rec.Tier == f.tier) &&
		(f.node == "" || rec.Node == f.node) &&
		(f.run == 0 || run == f.run)
}

// search is one `billet logs search`.
type search struct {
	cfg       *config.Config
	jobs      *jobCache
	filter    filters
	level     *joblog.Level
	since     time.Time
	limit     int
	histogram string
}

// Result is what a search found.
type Result struct {
	Notice string `json:"notice"`
	// Matches counts every line that matched, past the ones printed. Partial says the
	// search's own bound stopped it, so there may be more.
	Matches int   `json:"matches"`
	Partial bool  `json:"partial"`
	Lines   []Hit `json:"lines"`
	// Histogram counts the matches by when their chunk reached the control plane, oldest
	// first, set when one was asked for.
	Histogram []Bucket `json:"histogram,omitempty"`
}

// Hit is one matching line.
type Hit struct {
	Lease      string    `json:"lease"`
	Line       int64     `json:"line"`
	Level      string    `json:"level"`
	Step       int64     `json:"step"`
	StepName   string    `json:"step_name,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Repository string    `json:"repository"`
	Job        string    `json:"job"`
	Node       string    `json:"node"`
	// Text is the line, and TextWhy why it is not here when it could not be read.
	Text    string `json:"text,omitempty"`
	TextWhy string `json:"text_why,omitempty"`
}

// Bucket is how many lines matched in one hour or day, by its start.
type Bucket struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// run searches the index, newest first, then reads the text of what it will print.
func (s *search) run(ctx context.Context, match string) (Result, error) {
	result := Result{Notice: banner, Lines: []Hit{}}
	seen := map[string]bool{}
	buckets := map[time.Time]int{}

	var filterErr error

	partial, err := state.SearchLogIndex(ctx, app.LogIndexDir(s.cfg), state.LogSearch{
		Match: match, Since: s.since, MaxScan: searchMaxScan,
		Visit: func(h state.LogHit) bool {
			if h.ReceivedAt.Before(s.since) {
				return true
			}

			// A LINE IS IN TWO FILES WHEN A NODE RE-SHIPPED IT ON A LATER DAY.
			key := h.Lease + "/" + strconv.FormatInt(h.Line, 10)
			if seen[key] {
				return true
			}

			seen[key] = true

			rec, err := s.jobs.of(ctx, h.Lease)
			if err != nil {
				filterErr = err

				return false
			}

			if !s.keeps(rec, &h) {
				return true
			}

			result.Matches++

			if s.histogram != "" {
				buckets[bucketOf(h.ReceivedAt, s.histogram)]++

				return true
			}

			if len(result.Lines) < s.limit {
				result.Lines = append(result.Lines, hitOf(&h, rec))
			}

			return true
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("billet logs search: %w", err)
	}

	if filterErr != nil {
		return Result{}, fmt.Errorf("billet logs search: %w", filterErr)
	}

	result.Partial = partial

	for at, count := range buckets {
		result.Histogram = append(result.Histogram, Bucket{At: at, Count: count})
	}

	slices.SortFunc(result.Histogram, func(a, b Bucket) int { return a.At.Compare(b.At) })

	s.readText(ctx, &result)

	return result, nil
}

// keeps says a hit passes every filter.
func (s *search) keeps(rec *alloc.JobRecord, h *state.LogHit) bool {
	return s.filter.matches(rec) &&
		(s.filter.step == "" || h.StepName == s.filter.step) &&
		(s.level == nil || h.Level == *s.level)
}

func hitOf(h *state.LogHit, rec *alloc.JobRecord) Hit {
	hit := Hit{Lease: h.Lease, Line: h.Line, Level: h.Level.String(), Step: h.Step, StepName: h.StepName,
		ReceivedAt: h.ReceivedAt}
	if rec != nil {
		hit.Repository, hit.Job, hit.Node = rec.Repo, rec.Job.Name, rec.Node
	}

	return hit
}

func bucketOf(at time.Time, histogram string) time.Time {
	if histogram == "day" {
		return at.UTC().Truncate(24 * time.Hour)
	}

	return at.UTC().Truncate(time.Hour)
}

// readTextBudget bounds reading back one search's lines.
const readTextBudget = 2 * time.Minute

// readText reads back the printed lines, a lease at a time. WHAT CANNOT BE READ IS SAID
// on each line: a node that is gone, a log it has evicted or a control plane that is not
// running here says nothing of whether the line exists, since the index says it does.
func (s *search) readText(ctx context.Context, result *Result) {
	// ALL OF IT, since each lease may wait out a node that is registered and not polling: what
	// is past this says it was not read.
	ctx, cancel := context.WithTimeout(ctx, readTextBudget)
	defer cancel()

	byLease := map[string][]int{}
	for i := range result.Lines {
		byLease[result.Lines[i].Lease] = append(byLease[result.Lines[i].Lease], i)
	}

	for lease, indexes := range byLease {
		ranges := make([]nodeapi.LogLineRange, 0, len(indexes))
		for _, i := range indexes {
			ranges = append(ranges, nodeapi.LogLineRange{First: result.Lines[i].Line, Last: result.Lines[i].Line})
		}

		read, err := app.ReadJobLogLines(ctx, s.cfg, app.JobLogRead{Lease: lease, Ranges: ranges})

		why := ""

		switch {
		case err != nil:
			why = "text not read: " + err.Error()
		case !read.Held:
			why = "text not held: its node no longer keeps this job's log"
		}

		text := map[int64]string{}
		for _, l := range read.Lines {
			text[l.Line] = l.Text
		}

		for _, i := range indexes {
			line := &result.Lines[i]

			got, ok := text[line.Line]

			switch {
			case why != "":
				line.TextWhy = why
			case ok:
				line.Text = got
			case read.Cut:
				line.TextWhy = "text not read: past what one read of its node carries"
			default:
				line.TextWhy = "text not held: its node could not read this line back"
			}
		}
	}
}

// render writes a search's result. EVERY STRING A JOB OR A WORKFLOW CHOSE IS QUOTED, so
// none can forge a line.
func render(w io.Writer, r *Result) {
	fmt.Fprintln(w, r.Notice)
	if r.Partial {
		fmt.Fprintln(w, "partial: the search's bound stopped it, so there may be more; shorten --since or name a filter")
	}

	if len(r.Histogram) > 0 {
		for _, b := range r.Histogram {
			fmt.Fprintf(w, "%s  %d\n", b.At.Format(time.RFC3339), b.Count)
		}
		fmt.Fprintf(w, "%d matching lines\n", r.Matches)

		return
	}

	for i := range r.Lines {
		h := &r.Lines[i]
		step := "step " + strconv.FormatInt(h.Step, 10)
		if h.StepName != "" {
			step += " " + strconv.Quote(h.StepName)
		}

		fmt.Fprintf(w, "%s lease %s line %d %s %s, %s of %s on %s, %s\n", h.ReceivedAt.Format(time.RFC3339), h.Lease,
			h.Line, h.Level, step, strconv.Quote(h.Job), strconv.Quote(h.Repository), strconv.Quote(h.Node), lineOf(h))
	}

	fmt.Fprintf(w, "%d matching lines", r.Matches)
	if r.Matches > len(r.Lines) {
		fmt.Fprintf(w, ", the newest %d printed", len(r.Lines))
	}
	fmt.Fprintln(w)
}

// lineOf is a line's text, quoted, or why it is not here.
func lineOf(h *Hit) string {
	if h.TextWhy != "" {
		return h.TextWhy
	}

	return strconv.Quote(h.Text)
}
