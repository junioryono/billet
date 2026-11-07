package alloc

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// JobFact is one started job as the reports over many jobs read it. Every empty
// string and zero time is "not recorded".
type JobFact struct {
	LeaseID string
	Tier    string
	Node    string
	// Repo is owner/name. WorkflowRef is GitHub's owner/name/path@ref, and JobName
	// the name GitHub shows, with its matrix values.
	Repo        string
	WorkflowRef string
	JobName     string
	// RunID is the run that started on the runner, and RunAttempt which attempt
	// of it the job was; zero is unknown, never a first attempt.
	RunID      int64
	RunAttempt int64
	// Result is GitHub's word, Conclusion the lease's terminal phase and
	// Disruption what billet's own infrastructure did to the job.
	Result     string
	Conclusion string
	Disruption string
	// StartedAt and ResultAt are billet's clock, GitHubQueuedAt GitHub's.
	StartedAt      time.Time
	ResultAt       time.Time
	GitHubQueuedAt time.Time
	// VCPU, Memory and PriceMicrosPerHour are what the lease was charged; a zero
	// is not recorded, and a zero price is also what a host-backed lease has.
	// InstanceType is the shape it bought, empty for a host-backed lease, which
	// buys nothing.
	VCPU               int64
	Memory             int64
	PriceMicrosPerHour int64
	InstanceType       string
	// Usage is the host's report, nil when none was recorded. It carries the
	// groups, the window, CPU, peak memory, OOM kills, CPU pressure and energy;
	// disk, network and the counters are not read.
	Usage *JobUsage
}

// Duration is how long the job ran on billet's clock, from GitHub starting it to
// GitHub's result reaching billet, false when either end is missing or the two
// are out of order. The lease's own finish is later, by its teardown.
func (f *JobFact) Duration() (time.Duration, bool) {
	d, ok := between(f.StartedAt, f.ResultAt)

	return d, ok && d >= 0
}

// Succeeded says GitHub's result is the one word billet recognises as success.
func (f *JobFact) Succeeded() bool { return f.Result == ResultSucceeded }

// Attributed says the job did not succeed and billet's own lifecycle has
// something to say about it: a disruption was recorded, or its lease failed. Two
// facts, as `billet leases failures` reads them, and not a verdict that one
// caused the other.
func (f *JobFact) Attributed() bool {
	return !f.Succeeded() && (f.Disruption != "" || f.Conclusion == "failed")
}

// Workflow is the workflow file, owner/name/path, with its ref cut off, so one
// workflow on two branches is one workflow.
func (f *JobFact) Workflow() string {
	workflow, _, _ := strings.Cut(f.WorkflowRef, "@")

	return workflow
}

// Wait is the job's times as a queue wait.
func (f *JobFact) Wait() JobWait {
	return JobWait{LeaseID: f.LeaseID, Tier: f.Tier, VCPU: int(f.VCPU), QueuedAt: f.GitHubQueuedAt,
		StartedAt: f.StartedAt}
}

// JobFacts reads the jobs that started in [from, until), newest start first. The
// bool is true when the window held more than one read takes, in which case the
// oldest are the ones left out.
func (a *Allocator) JobFacts(ctx context.Context, from, until time.Time) ([]JobFact, bool, error) {
	return a.jobFacts(ctx, from, until, jobWaitsLimit)
}

func (a *Allocator) jobFacts(ctx context.Context, from, until time.Time, limit int) ([]JobFact, bool, error) {
	var out []JobFact

	partial := false

	err := a.db.View(ctx, func(q querier) error {
		// ONE PAST THE BOUND, so a window of exactly the bound is not called partial.
		rows, err := state.ReadQueries(q).ListJobFacts(ctx, ledgerdb.ListJobFactsParams{
			Since: ts(from), Until: ts(until), MaxRows: int64(limit) + 1,
		})
		if err != nil {
			return fmt.Errorf("alloc: list the jobs of a window: %w", err)
		}

		if len(rows) > limit {
			rows, partial = rows[:limit], true
		}

		out = make([]JobFact, 0, len(rows))
		for i := range rows {
			out = append(out, jobFactOf(&rows[i]))
		}

		return nil
	})

	return out, partial, err
}

func jobFactOf(r *ledgerdb.ListJobFactsRow) JobFact {
	fact := JobFact{
		LeaseID: r.LeaseID, Tier: r.Tier, Node: r.Node, Repo: r.Repo, WorkflowRef: r.WorkflowRef,
		JobName: r.JobName, RunID: r.RunID, RunAttempt: r.RunAttempt, Result: r.Result,
		Conclusion: r.Conclusion, Disruption: r.Disruption, StartedAt: parseStamp(r.StartedAt),
		ResultAt: parseStamp(r.ResultAt), GitHubQueuedAt: parseStamp(r.GithubQueuedAt),
		VCPU: r.Vcpu, Memory: r.Memory, PriceMicrosPerHour: r.PriceMicrosPerHour, InstanceType: r.InstanceType,
	}
	if r.HasUsage == 0 {
		return fact
	}

	var unmeasured []string
	if r.Unmeasured != "" {
		unmeasured = strings.Split(r.Unmeasured, ",")
	}
	fact.Usage = &JobUsage{
		Unmeasured: unmeasured, WindowMillis: r.WindowMs, CPUUserMicros: r.CpuUserUs,
		CPUSystemMicros: r.CpuSystemUs, GuestCPUMicros: r.GuestCpuUs, MemoryPeakBytes: r.MemoryPeakBytes,
		OOMKills: r.OomKills, CPUSomeMicros: r.CpuSomeUs, EnergyActiveMicrojoules: r.EnergyActiveUj,
		EnergyIdleMicrojoules: r.EnergyIdleUj, EnergySource: r.EnergySource,
	}

	return fact
}

// Exemplar is one job's value in a distribution, so a number leads to a job.
type Exemplar struct {
	Value time.Duration
	Lease string
}

// DurationSummary is one distribution's percentiles, each nearest rank and each
// the job that holds it. Count is zero for an empty distribution, which has no
// percentiles rather than percentiles of zero.
type DurationSummary struct {
	Count int
	P50   Exemplar
	P90   Exemplar
	P99   Exemplar
}

// SummarizeDurations is the distribution of samples, which it sorts. TIES BY
// LEASE, so the job named at a percentile is the same on every run over the same
// rows.
func SummarizeDurations(samples []Exemplar) DurationSummary {
	if len(samples) == 0 {
		return DurationSummary{}
	}

	slices.SortFunc(samples, func(x, y Exemplar) int {
		return cmp.Or(cmp.Compare(x.Value, y.Value), cmp.Compare(x.Lease, y.Lease))
	})

	n := len(samples)
	at := func(perTenThousand int) Exemplar { return samples[NearestRank(perTenThousand, n)-1] }

	return DurationSummary{Count: n, P50: at(5000), P90: at(9000), P99: at(9900)}
}

// Percentile is the nearest-rank value of sorted, which is not empty, at a
// percentile given in parts per ten thousand.
func Percentile[T any](sorted []T, perTenThousand int) T {
	return sorted[NearestRank(perTenThousand, len(sorted))-1]
}
