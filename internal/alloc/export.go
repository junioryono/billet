package alloc

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/jobtrace"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// exportReadLimit bounds one export's read of the history, as the reports'
// reads are bounded; a window past it keeps its oldest jobs and says partial.
const exportReadLimit = 200_000

// ExportGaps counts what an export read and could not carry into the trace,
// so a replay of it says what it is missing rather than looking whole.
type ExportGaps struct {
	// Read is how many jobs the window held.
	Read int
	// NeverStarted and NeverFinished were left out: a job with no start has no
	// runtime, and one with no end has none yet.
	NeverStarted, NeverFinished int
	// NotARuntime were left out because their end is not after their start.
	NotARuntime int
	// AssignedAsArrival had no GitHub queue time on record, and arrive at
	// billet's assignment instead: their wait at GitHub is not in the trace.
	AssignedAsArrival int
	// Unidentified had no repository or workflow on record (rows from before
	// they were kept), and carry a stand-in.
	Unidentified int
	// Partial is true when the window held more jobs than one read takes, and
	// its newest were left out.
	Partial bool
}

// ExportTrace writes the jobs assigned in [since, until) as a trace: each job
// arriving when GitHub queued it (or, with no queue time on record, when billet
// was assigned it) and running from its start to its completion. names keeps
// owners, repositories, workflow files, job names and GitHub's run ids as
// recorded; without it each becomes a stand-in numbered in order of first
// appearance, so a trace can be shared without naming anyone's code or
// pointing at their runs, and a job's matrix values get a stand-in of their
// own so the runtime fallbacks still find a job's name.
//
// SAME-INSTANT ARRIVALS ARE ORDERED BY REQUEST ID, THEN GITHUB'S JOB ID, THEN
// LEASE, so a trace exported twice, or a replayed trace exported back, numbers
// its jobs the same way.
func (a *Allocator) ExportTrace(ctx context.Context, since, until time.Time, names bool,
) (jobtrace.Trace, ExportGaps, error) {
	var (
		rows []ledgerdb.ListJobsForExportRow
		gaps ExportGaps
	)

	err := a.db.View(ctx, func(q querier) error {
		var err error

		// ONE PAST THE BOUND, so a window of exactly the bound is not called partial.
		rows, err = state.ReadQueries(q).ListJobsForExport(ctx, ledgerdb.ListJobsForExportParams{
			Since: ts(since), Until: ts(until), MaxRows: exportReadLimit + 1,
		})
		if err != nil {
			return fmt.Errorf("alloc: list jobs for export: %w", err)
		}

		return nil
	})
	if err != nil {
		return jobtrace.Trace{}, ExportGaps{}, err
	}

	if len(rows) > exportReadLimit {
		rows, gaps.Partial = rows[:exportReadLimit], true
	}

	gaps.Read = len(rows)

	type keyed struct {
		arrival jobtrace.Arrival
		request int64
		job     string
		lease   string
	}

	stand := newStandIns(names)
	out := make([]keyed, 0, len(rows))

	for i := range rows {
		r := &rows[i]

		started := parseStamp(r.StartedAt)
		end := parseStamp(r.ResultAt)
		if end.IsZero() {
			end = parseStamp(r.FinishedAt)
		}

		switch {
		case started.IsZero():
			gaps.NeverStarted++

			continue
		case end.IsZero():
			gaps.NeverFinished++

			continue
		case !end.After(started):
			gaps.NotARuntime++

			continue
		}

		at := parseStamp(r.GithubQueuedAt)
		if at.IsZero() {
			at = parseStamp(r.QueuedAt)
			gaps.AssignedAsArrival++
		}

		repo, workflow := r.Repo, r.WorkflowRef
		if repo == "" || workflow == "" {
			gaps.Unidentified++
		}

		owner, repository, workflowRef, job := stand.of(repo, workflow, r.JobName)

		out = append(out, keyed{
			arrival: jobtrace.Arrival{
				At: at, Tier: r.Tier, Owner: owner, Repository: repository, WorkflowRef: workflowRef,
				Job: job, RunID: stand.run(r.RunID), Duration: jobtrace.Duration(end.Sub(started)),
				Result: r.Result,
			},
			request: r.RequestID, job: r.GithubJobID, lease: r.LeaseID,
		})
	}

	slices.SortStableFunc(out, func(x, y keyed) int {
		return cmp.Or(x.arrival.At.Compare(y.arrival.At), cmp.Compare(x.request, y.request),
			cmp.Compare(x.job, y.job), cmp.Compare(x.lease, y.lease))
	})

	trace := jobtrace.Trace{Arrivals: make([]jobtrace.Arrival, 0, len(out))}
	for i := range out {
		trace.Arrivals = append(trace.Arrivals, out[i].arrival)
	}

	trace.Normalize()

	return trace, gaps, nil
}

// standIns replaces names by numbered stand-ins, each kind numbered in order
// of first appearance; with keep set it replaces nothing it has.
type standIns struct {
	keep                                    bool
	owners, repos, workflows, jobs, matrix  map[string]string
	nOwners, nRepos, nWorkflows, nJobs, nMx int
	runs                                    map[int64]int64
}

func newStandIns(keep bool) *standIns {
	return &standIns{keep: keep, owners: map[string]string{}, repos: map[string]string{},
		workflows: map[string]string{}, jobs: map[string]string{}, matrix: map[string]string{},
		runs: map[int64]int64{}}
}

// run is a GitHub run id as the trace carries it: as recorded with names kept,
// otherwise numbered from 1 in order of first appearance, so the jobs of one
// run still share one. An id not recorded (zero) stays zero, which the trace
// reads as the job being its own run.
func (s *standIns) run(id int64) int64 {
	if id <= 0 {
		return 0
	}

	if s.keep {
		return id
	}

	if n, ok := s.runs[id]; ok {
		return n
	}

	s.runs[id] = int64(len(s.runs) + 1)

	return s.runs[id]
}

// unrecorded stands in for a repository or workflow a row does not record,
// with names kept or not: Validate needs one, and none was recorded.
const unrecorded = "unrecorded"

// of is a job's owner, repository, workflow ref and display name as the trace
// carries them.
func (s *standIns) of(repo, workflowRef, name string) (string, string, string, string) {
	owner, repository, ok := strings.Cut(repo, "/")
	if !ok {
		owner, repository = "", repo
	}

	if repository == "" {
		repository = unrecorded
	}

	workflow, _, _ := strings.Cut(workflowRef, "@")
	if workflow == "" {
		workflowRef = repo + "/.github/workflows/" + unrecorded + ".yml"
		workflow = workflowRef
	}

	if s.keep {
		return owner, repository, workflowRef, name
	}

	numbered := func(seen map[string]string, n *int, prefix, key string) string {
		if v, ok := seen[key]; ok {
			return v
		}

		*n++
		seen[key] = fmt.Sprintf("%s-%d", prefix, *n)

		return seen[key]
	}

	o := ""
	if owner != "" {
		o = numbered(s.owners, &s.nOwners, "owner", owner)
	}

	r := numbered(s.repos, &s.nRepos, "repo", owner+"/"+repository)
	w := numbered(s.workflows, &s.nWorkflows, "workflow", workflow)

	job := ""
	if strings.TrimSpace(name) != "" {
		base, suffix := strings.TrimSpace(name), ""
		if b, ok := matrixBase(name); ok {
			base, suffix = b, strings.TrimSpace(strings.TrimSpace(name)[len(b):])
		}

		job = numbered(s.jobs, &s.nJobs, "job", r+"/"+w+"/"+base)
		if suffix != "" {
			job += " (" + numbered(s.matrix, &s.nMx, "matrix", suffix) + ")"
		}
	}

	return o, r, cmp.Or(o, jobtrace.DefaultOwner) + "/" + r + "/.github/workflows/" + w + ".yml", job
}

// ExportHosts is every host the fleet still expects to answer, as a trace's
// fleet names them: its contribution, its backend and its site.
func (a *Allocator) ExportHosts(ctx context.Context) ([]jobtrace.Host, error) {
	contributions, err := a.RegisteredContribution(ctx)
	if err != nil {
		return nil, err
	}

	nodes, err := a.RegisteredNodes(ctx)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]RegisteredNode, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}

	out := make([]jobtrace.Host, 0, len(contributions))

	for _, c := range contributions {
		n := byName[c.Node]
		out = append(out, jobtrace.Host{
			Name: c.Node, VCPU: c.VCPU, Memory: c.Memory, Site: n.Site, Provider: n.Provider,
		})
	}

	slices.SortFunc(out, func(x, y jobtrace.Host) int { return cmp.Compare(x.Name, y.Name) })

	return out, nil
}
