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
	// Read is how many assigned jobs the window held.
	Read int
	// NeverStarted, NeverCompleted and NotARuntime were left out: a job with no
	// start has no runtime, one GitHub never reported complete (cancelled, its
	// runner lost, or still running) has no runtime GitHub ran it for, and one
	// whose completion is not after its start has none that can be replayed.
	NeverStarted, NeverCompleted, NotARuntime int
	// UndeclaredTier were left out because their tier is not in this
	// allocator's catalogue, so the fleet cannot give their shape.
	UndeclaredTier int
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
// was assigned it) and running from its start to GitHub's completion of it.
// names keeps owners, repositories, workflow files, job names and GitHub's run
// ids as recorded; without it each becomes a stand-in numbered in the order the
// trace carries the jobs, so a trace can be shared without naming anyone's code or
// pointing at their runs, and a job's matrix values get a stand-in of their
// own so the runtime fallbacks still find a job's name. Only jobs of tiers in
// this allocator's catalogue are carried, since a replay needs their shape.
//
// SAME-INSTANT ARRIVALS ARE ORDERED AS BILLET WAS FIRST ASSIGNED THEM
// (queued_at, which the row keeps from its first assignment, where assigned_at
// moves on a reassignment), then by request id in the order it was allocated,
// GitHub's job id and lease, so a trace exported twice, or a replayed trace
// exported back, numbers its jobs the same way under either protocol: GitHub's
// request ids rise, and billet's own, which a direct assignment and a pool slot
// carry, are negative and allocated downwards (requestOrder).
//
// A TIME THAT DOES NOT READ IS AN ERROR naming the lease and the column, never
// a time not recorded: read as absent, an unreadable start would drop the job
// as never started and an unreadable completion would move its end.
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
		arrival       jobtrace.Arrival
		firstAssigned time.Time
		request       int64
		job           string
		lease         string
		// What the stand-ins replace, numbered once the trace is in order.
		repo, workflow, name string
		run                  int64
	}

	stand := newStandIns(names)
	out := make([]keyed, 0, len(rows))

	for i := range rows {
		r := &rows[i]

		var times [4]time.Time
		for j, c := range []struct{ column, value string }{
			{"started_at", r.StartedAt}, {"result_at", r.ResultAt}, {"github_queued_at", r.GithubQueuedAt},
			{"queued_at", r.QueuedAt},
		} {
			t, err := strictStamp(c.value)
			if err != nil {
				return jobtrace.Trace{}, ExportGaps{}, fmt.Errorf("alloc: export lease %s: %s: %w", r.LeaseID,
					c.column, err)
			}

			times[j] = t
		}

		started, completed, queued, firstAssigned := times[0], times[1], times[2], times[3]

		switch {
		case started.IsZero():
			gaps.NeverStarted++

			continue
		case completed.IsZero():
			gaps.NeverCompleted++

			continue
		case !completed.After(started):
			gaps.NotARuntime++

			continue
		}

		if _, declared := a.tiers[r.Tier]; !declared {
			gaps.UndeclaredTier++

			continue
		}

		at := queued
		if at.IsZero() {
			at = firstAssigned
			gaps.AssignedAsArrival++
		}

		repo, workflow := r.Repo, r.WorkflowRef
		if repo == "" || workflow == "" {
			gaps.Unidentified++
		}

		out = append(out, keyed{
			arrival: jobtrace.Arrival{
				At: at, Tier: r.Tier, Duration: jobtrace.Duration(completed.Sub(started)), Result: r.Result,
			},
			firstAssigned: firstAssigned, request: r.RequestID, job: r.GithubJobID, lease: r.LeaseID,
			repo: repo, workflow: workflow, name: r.JobName, run: r.RunID,
		})
	}

	slices.SortStableFunc(out, func(x, y keyed) int {
		return cmp.Or(x.arrival.At.Compare(y.arrival.At), x.firstAssigned.Compare(y.firstAssigned),
			requestOrder(x.request, y.request), cmp.Compare(x.job, y.job), cmp.Compare(x.lease, y.lease))
	})

	// THE STAND-INS ARE NUMBERED IN THE TRACE'S ORDER, not the read's, whose ties
	// fall to random lease ids: two exports of one history number them alike.
	trace := jobtrace.Trace{Arrivals: make([]jobtrace.Arrival, 0, len(out))}
	for i := range out {
		k := &out[i]
		k.arrival.Owner, k.arrival.Repository, k.arrival.WorkflowRef, k.arrival.Job = stand.of(k.repo, k.workflow,
			k.name)
		k.arrival.RunID = stand.run(k.run)
		trace.Arrivals = append(trace.Arrivals, k.arrival)
	}

	trace.Normalize()

	return trace, gaps, nil
}

// requestOrder compares two request ids in the order they were allocated:
// GitHub's, positive, rise; billet's own, negative, fall from -1; and
// GitHub's come before billet's.
func requestOrder(x, y int64) int {
	switch {
	case x < 0 && y < 0:
		return cmp.Compare(y, x)
	case x < 0:
		return 1
	case y < 0:
		return -1
	default:
		return cmp.Compare(x, y)
	}
}

// strictStamp reads a time the ledger wrote: the empty string is a time not
// recorded, and anything else that does not read is an error, never an absence.
func strictStamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a time this binary reads: %w", s, err)
	}

	return t, nil
}

// standIns replaces names by numbered stand-ins, each kind numbered in order
// of first appearance; with keep set it replaces nothing it has.
type standIns struct {
	keep                                    bool
	owners, repos, workflows, jobs, matrix  map[string]string
	nOwners, nRepos, nWorkflows, nJobs, nMx int
	runs                                    map[int64]int64
	nRuns                                   int64
}

func newStandIns(keep bool) *standIns {
	return &standIns{keep: keep, owners: map[string]string{}, repos: map[string]string{},
		workflows: map[string]string{}, jobs: map[string]string{}, matrix: map[string]string{},
		runs: map[int64]int64{}}
}

// run is a GitHub run id as the trace carries it: as recorded with names kept,
// otherwise numbered from 1 in order of first appearance, so the jobs of one
// run still share one.
//
// A RUN NOT RECORDED IS THE JOB'S OWN. With stand-ins it takes the next number
// from the same count, so it can never share one with a recorded run. With
// names kept it stays zero, which the trace reads as the job being its own run
// numbered by its sequence: GitHub's run ids are in the billions and a trace
// holds at most a read's 200,000 jobs, so the two cannot meet.
func (s *standIns) run(id int64) int64 {
	if s.keep {
		return max(id, 0)
	}

	if id <= 0 {
		s.nRuns++

		return s.nRuns
	}

	if n, ok := s.runs[id]; ok {
		return n
	}

	s.nRuns++
	s.runs[id] = s.nRuns

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

	// AN OWNER NOT RECORDED IS A STAND-IN TOO, never the trace's default owner,
	// which a reader could not tell from a name the ledger held.
	o := numbered(s.owners, &s.nOwners, "owner", owner)

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

	return o, r, o + "/" + r + "/.github/workflows/" + w + ".yml", job
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
