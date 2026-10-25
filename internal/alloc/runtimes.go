package alloc

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// A RUNTIME PREDICTION IS AN OPTIMISATION, NEVER A DEPENDENCY (#524). It may
// change which host a policy prefers; it never decides whether capacity is
// bought, held or released, and nothing that does reads it. A missing or wrong
// prediction picks a worse host, never a wrong outcome.

// Policy constants for learning runtimes. Each is a choice, not a measurement:
// the record of how well the choice predicts is
// docs/reference/records/runtime-predictions.md.
const (
	// RuntimeWindow is how far back a prediction looks.
	RuntimeWindow = 14 * 24 * time.Hour
	// RuntimeRefresh is how long one snapshot of what was learned is used
	// before it is rebuilt, on the allocator's clock.
	RuntimeRefresh = time.Hour
	// MinRuntimeSamples is the fewest jobs a level of the key must have seen to
	// predict from it; with fewer, the next, broader level answers.
	MinRuntimeSamples = 3
	// DefaultRuntime is the prediction when nothing has been learned, from no
	// samples at all.
	DefaultRuntime = 10 * time.Minute
)

// runtimeRetry is how long a rebuild that failed is not tried again: the
// last snapshot is served meanwhile, and a ledger that could not be read is
// not asked for a fortnight of history by every caller that follows.
const runtimeRetry = 5 * time.Minute

// runtimeReadLimit bounds one read of the history. The reference
// deployment's busiest recorded day started under 3,000 jobs, so a fortnight
// is well inside it; a window past it keeps its newest jobs and says partial.
const runtimeReadLimit = 200_000

// RuntimeLevel is how specific the key a prediction was learned under is.
type RuntimeLevel string

const (
	// RuntimeByJob is the job itself: its tier, repository, workflow file and
	// whole display name.
	RuntimeByJob RuntimeLevel = "job"
	// RuntimeByName is the job without its matrix values: a display name's
	// parenthesised suffix, `build (ubuntu, 1.22)` for `build`, removed.
	RuntimeByName RuntimeLevel = "name"
	// RuntimeByTier is every job of the tier.
	RuntimeByTier RuntimeLevel = "tier"
	// RuntimeByDefault is DefaultRuntime, from nothing learned.
	RuntimeByDefault RuntimeLevel = "default"
)

// JobKey names what a runtime is learned under.
//
// NO TARGET, because the tier names it: a tier belongs to one target, and a
// repository's name carries its owner. Keyed by tier as well as repository, a
// job that moved to a larger tier is learned afresh there, which is right,
// since its runtime is likely to change with its shape.
type JobKey struct {
	Tier string
	// Repository is owner/name, as the history row keeps it.
	Repository string
	// Workflow is the workflow file, its `@ref` cut, so one workflow run from
	// every branch is one key.
	Workflow string
	// Name is the job's display name as GitHub sent it.
	Name string
}

// JobKeyOf is the key a history row's columns name.
func JobKeyOf(tier, repo, workflowRef, name string) JobKey {
	workflow, _, _ := strings.Cut(workflowRef, "@")

	return JobKey{Tier: tier, Repository: repo, Workflow: workflow, Name: name}
}

// matrixBase is a display name without its trailing parenthesised matrix
// values, and false when it carries none. The parentheses are matched from the
// end, so a value holding parentheses of its own is removed whole.
func matrixBase(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(name, ")") {
		return "", false
	}

	depth := 0

	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case ')':
			depth++
		case '(':
			depth--
		}

		if depth != 0 {
			continue
		}

		base := strings.TrimSpace(name[:i])
		if base == "" {
			return "", false
		}

		return base, true
	}

	return "", false
}

// runtimeKey is one level of a JobKey.
type runtimeKey struct {
	level                            RuntimeLevel
	tier, repository, workflow, name string
}

// keys are the levels a job is learned under and predicted from, most
// specific first. A job with no repository, workflow or name on record (a row
// from before they were kept) is learned under its tier alone.
func (k JobKey) keys() []runtimeKey {
	out := make([]runtimeKey, 0, 3)

	if k.Repository != "" && k.Workflow != "" && strings.TrimSpace(k.Name) != "" {
		out = append(out, runtimeKey{level: RuntimeByJob, tier: k.Tier, repository: k.Repository,
			workflow: k.Workflow, name: strings.TrimSpace(k.Name)})

		if base, ok := matrixBase(k.Name); ok {
			out = append(out, runtimeKey{level: RuntimeByName, tier: k.Tier, repository: k.Repository,
				workflow: k.Workflow, name: base})
		}
	}

	return append(out, runtimeKey{level: RuntimeByTier, tier: k.Tier})
}

// RuntimeSample is one finished job: what it is and how long it held its
// lease, from its start to the lease's archive.
type RuntimeSample struct {
	LeaseID  string
	Key      JobKey
	Started  time.Time
	Finished time.Time
}

// Runtime is how long the job held its lease.
func (s RuntimeSample) Runtime() time.Duration { return s.Finished.Sub(s.Started) }

// RuntimeDistribution is what one key's jobs took.
type RuntimeDistribution struct {
	Count       int
	Median, P90 time.Duration
}

// RuntimeEstimate is a prediction of one job's runtime and what it rests on.
type RuntimeEstimate struct {
	Median, P90 time.Duration
	// Samples is how many jobs the level saw; zero for the default.
	Samples int
	Level   RuntimeLevel
}

// RuntimePriors is what one window of the history taught, fixed once built.
type RuntimePriors struct {
	// AsOf is the instant the snapshot speaks for: it learned from the jobs
	// that started in the window before it and had finished by it.
	AsOf time.Time
	// Partial is true when the window held more jobs than one read takes, and
	// its oldest were left out.
	Partial bool
	dist    map[runtimeKey]RuntimeDistribution
}

// LearnRuntimes builds a snapshot as of asOf from samples. Only the samples
// that started inside the window before asOf and finished before it are
// learned from; a runtime that is not positive (two writes out of order on
// one clock) is not a runtime and is left out.
func LearnRuntimes(samples []RuntimeSample, asOf time.Time) *RuntimePriors {
	from := asOf.Add(-RuntimeWindow)
	grouped := map[runtimeKey][]time.Duration{}

	for i := range samples {
		s := &samples[i]
		if s.Started.Before(from) || !s.Started.Before(asOf) || !s.Finished.Before(asOf) ||
			s.Runtime() <= 0 {
			continue
		}

		for _, k := range s.Key.keys() {
			grouped[k] = append(grouped[k], s.Runtime())
		}
	}

	out := &RuntimePriors{AsOf: asOf, dist: make(map[runtimeKey]RuntimeDistribution, len(grouped))}

	for k, runtimes := range grouped {
		slices.Sort(runtimes)
		n := len(runtimes)
		out.dist[k] = RuntimeDistribution{
			Count:  n,
			Median: runtimes[nearestRank(5000, n)-1],
			P90:    runtimes[nearestRank(9000, n)-1],
		}
	}

	return out
}

// Estimate predicts a job's runtime from the most specific level of its key
// that has seen at least MinRuntimeSamples jobs, and DefaultRuntime when none
// has. A nil snapshot knows nothing.
func (p *RuntimePriors) Estimate(key JobKey) RuntimeEstimate {
	if p != nil {
		for _, k := range key.keys() {
			if d, ok := p.dist[k]; ok && d.Count >= MinRuntimeSamples {
				return RuntimeEstimate{Median: d.Median, P90: d.P90, Samples: d.Count, Level: k.level}
			}
		}
	}

	return RuntimeEstimate{Median: DefaultRuntime, P90: DefaultRuntime, Level: RuntimeByDefault}
}

// Tiers is what each tier's jobs took, by label.
func (p *RuntimePriors) Tiers() map[string]RuntimeDistribution {
	out := map[string]RuntimeDistribution{}
	if p == nil {
		return out
	}

	for k, d := range p.dist {
		if k.level == RuntimeByTier {
			out[k.tier] = d
		}
	}

	return out
}

// PredictEnd is when a job that started at start is predicted to end, as seen
// at now.
//
// AN OVERDUE JOB IS NEVER ABOUT TO END. Once now is past start plus the
// median, the job is predicted to need half its median again from now, and
// overdue is true: a job that has already outrun what its kind usually takes
// is the one most likely to run long, and a host running it must not look
// like the one about to free up.
func PredictEnd(est RuntimeEstimate, start, now time.Time) (time.Time, bool) {
	end := start.Add(est.Median)
	if now.After(end) {
		return now.Add(est.Median / 2), true
	}

	return end, false
}

// runtimeBook is the allocator's copy of what the history taught: one
// snapshot, rebuilt when it is RuntimeRefresh old on the allocator's clock.
//
// REBUILT WHEN ASKED, NOT BY A LOOP. A rebuild is one read of a fortnight of
// history through the reader pool, so it costs nothing while nothing asks, and
// on the allocator's clock a replay or a test moves it by moving time. One
// rebuild runs at a time, and once a snapshot exists nobody waits for the next:
// a caller arriving during a rebuild is answered from the one it replaces.
type runtimeBook struct {
	snapshot atomic.Pointer[RuntimePriors]
	building sync.Mutex
	// failedAt is when the last rebuild failed, on the allocator's clock, in
	// Unix nanoseconds; zero before any has. A success needs no reset: the
	// snapshot it stores is fresh for an hour, far past runtimeRetry.
	failedAt atomic.Int64
}

// ErrRuntimesStale means the history could not be read to rebuild what was
// learned, and the answer is the older snapshot it was to replace.
var ErrRuntimesStale = errors.New("alloc: the learned runtimes could not be refreshed")

// RuntimePriors is what this allocator has learned of its jobs' runtimes,
// rebuilt from the ledger when the snapshot is RuntimeRefresh old.
//
// A REBUILD THAT FAILS KEEPS THE LAST SNAPSHOT: it is returned with an error
// wrapping ErrRuntimesStale, which a caller logs and predicts from anyway,
// since a prediction is an optimisation, and the rebuild is not tried again
// for runtimeRetry, the last snapshot answering meanwhile. With no snapshot to
// keep, the error alone is returned, and the next caller tries again.
func (a *Allocator) RuntimePriors(ctx context.Context) (*RuntimePriors, error) {
	now := a.now()

	current := a.runtimes.snapshot.Load()
	if current.freshAt(now) {
		return current, nil
	}

	if failed := a.runtimes.failedAt.Load(); current != nil && failed != 0 &&
		now.Sub(time.Unix(0, failed)) < runtimeRetry {
		return current, nil
	}

	if current != nil {
		if !a.runtimes.building.TryLock() {
			return current, nil
		}
	} else {
		a.runtimes.building.Lock()
	}
	defer a.runtimes.building.Unlock()

	// ANOTHER CALLER MAY HAVE REBUILT IT while this one waited for the lock.
	if latest := a.runtimes.snapshot.Load(); latest.freshAt(now) {
		return latest, nil
	}

	built, err := a.learnRuntimesAt(ctx, now)
	if err != nil {
		if current != nil {
			a.runtimes.failedAt.Store(now.UnixNano())

			return current, fmt.Errorf("%w: %w", ErrRuntimesStale, err)
		}

		return nil, err
	}

	a.runtimes.snapshot.Store(built)

	return built, nil
}

// freshAt reports whether a snapshot may still answer at now. A clock that
// moved back past it is a reason to rebuild, not to trust it.
func (p *RuntimePriors) freshAt(now time.Time) bool {
	return p != nil && !now.Before(p.AsOf) && now.Sub(p.AsOf) < RuntimeRefresh
}

// RuntimePriorsAsOf learns from the ledger as of asOf: the snapshot a
// controller refreshing hourly would have held then. Read afresh every time,
// never the allocator's own copy.
func (a *Allocator) RuntimePriorsAsOf(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
	return a.learnRuntimesAt(ctx, asOf)
}

// PredictJob fills rec.Predicted with what was predicted for its job when it
// started, from priors, which is asked for the snapshot as of the hour the job
// started in, as a controller refreshing hourly would have held it. A job that
// never started has no prediction, and priors is not asked.
func PredictJob(rec *JobRecord, priors func(asOf time.Time) (*RuntimePriors, error)) error {
	started := parseStamp(rec.StartedAt)
	if started.IsZero() {
		rec.Predicted = nil

		return nil
	}

	snapshot, err := priors(started.Truncate(RuntimeRefresh))
	if err != nil {
		return err
	}

	est := snapshot.Estimate(JobKeyOf(rec.Tier, rec.Repo, rec.Job.WorkflowRef, rec.Job.Name))
	rec.Predicted = &est

	return nil
}

// learnRuntimesAt reads the window before asOf and learns from it.
func (a *Allocator) learnRuntimesAt(ctx context.Context, asOf time.Time) (*RuntimePriors, error) {
	samples, partial, err := a.RuntimeSamples(ctx, asOf.Add(-RuntimeWindow), asOf)
	if err != nil {
		return nil, err
	}

	out := LearnRuntimes(samples, asOf)
	out.Partial = partial

	return out, nil
}

// RuntimeSamples reads the jobs that started in [since, until) and finished
// before until, newest start first. The bool is true when the window held
// more than one read takes, in which case the oldest are the ones left out.
func (a *Allocator) RuntimeSamples(ctx context.Context, since, until time.Time) ([]RuntimeSample, bool, error) {
	return a.runtimeSamples(ctx, since, until, runtimeReadLimit)
}

func (a *Allocator) runtimeSamples(ctx context.Context, since, until time.Time, limit int,
) ([]RuntimeSample, bool, error) {
	var (
		out     []RuntimeSample
		partial bool
	)

	err := a.db.View(ctx, func(q querier) error {
		// ONE PAST THE BOUND, so a window of exactly the bound is not called partial.
		rows, err := state.ReadQueries(q).ListJobRuntimes(ctx, ledgerdb.ListJobRuntimesParams{
			Since:   sql.NullString{String: ts(since), Valid: true},
			Until:   sql.NullString{String: ts(until), Valid: true},
			MaxRows: int64(limit) + 1,
		})
		if err != nil {
			return fmt.Errorf("alloc: list job runtimes: %w", err)
		}

		if len(rows) > limit {
			rows, partial = rows[:limit], true
		}

		out = make([]RuntimeSample, 0, len(rows))
		for i := range rows {
			r := &rows[i]

			started, finished := parseStamp(r.StartedAt), parseStamp(r.FinishedAt)
			if started.IsZero() || finished.IsZero() {
				continue
			}

			out = append(out, RuntimeSample{
				LeaseID: r.LeaseID, Key: JobKeyOf(r.Tier, r.Repo, r.WorkflowRef, r.JobName),
				Started: started, Finished: finished,
			})
		}

		return nil
	})

	return out, partial, err
}

// LeasePrediction is when a lease's job is predicted to end.
type LeasePrediction struct {
	End time.Time
	// Overdue is true when the job has outrun its median, and End is then half
	// a median from now (PredictEnd).
	Overdue bool
	// Started is false for a lease whose job has not started, which is
	// predicted as if it started now.
	Started bool
	RuntimeEstimate
}

// PredictLease predicts when one lease's job ends, from what this allocator
// has learned. A lease whose job has not started is predicted from now, and a
// lease with no job recorded yet (bought, not yet assigned) from its tier.
// A refresh that failed is ErrRuntimesStale beside a prediction from the older
// snapshot.
func (a *Allocator) PredictLease(ctx context.Context, leaseID string) (LeasePrediction, error) {
	priors, refreshErr := a.RuntimePriors(ctx)
	if priors == nil {
		return LeasePrediction{}, refreshErr
	}

	var (
		key     JobKey
		started time.Time
	)

	switch rec, err := a.Job(ctx, leaseID); {
	case err == nil:
		key = JobKeyOf(rec.Tier, rec.Repo, rec.Job.WorkflowRef, rec.Job.Name)
		started = parseStamp(rec.StartedAt)
	case errors.Is(err, ErrLeaseNotFound):
		lease, err := a.Lease(ctx, leaseID)
		if err != nil {
			return LeasePrediction{}, err
		}

		key = JobKey{Tier: lease.Tier}
	default:
		return LeasePrediction{}, err
	}

	now := a.now()
	est := priors.Estimate(key)
	out := LeasePrediction{Started: !started.IsZero(), RuntimeEstimate: est}

	if started.IsZero() {
		started = now
	}

	out.End, out.Overdue = PredictEnd(est, started, now)

	return out, refreshErr
}

// RuntimeAccuracy is how well predictions from the history matched the jobs
// that followed, for one tier.
type RuntimeAccuracy struct {
	Tier  string
	Count int
	// MedianAbsError is the median of |predicted median - runtime|.
	MedianAbsError time.Duration
	// Within2x counts the jobs whose runtime was between half and twice the
	// prediction.
	Within2x int
	// Levels counts the predictions by the level they came from.
	Levels map[RuntimeLevel]int
}

// Within2xShare is Within2x over Count.
func (r RuntimeAccuracy) Within2xShare() float64 {
	if r.Count == 0 {
		return 0
	}

	return float64(r.Within2x) / float64(r.Count)
}

// BacktestRuntimes grades the predictions a controller refreshing hourly would
// have made for the jobs that started in [from, to): each job is predicted
// from a snapshot as of the hour it started in, learned from the window before
// that hour, so it never learns from itself or from anything that had not yet
// finished. samples must cover RuntimeWindow before from as well.
func BacktestRuntimes(samples []RuntimeSample, from, to time.Time) []RuntimeAccuracy {
	type graded struct {
		err    time.Duration
		within bool
		level  RuntimeLevel
	}

	byTier := map[string][]graded{}
	snapshots := map[time.Time]*RuntimePriors{}

	for i := range samples {
		s := &samples[i]
		if s.Started.Before(from) || !s.Started.Before(to) || s.Runtime() <= 0 {
			continue
		}

		hour := s.Started.Truncate(RuntimeRefresh)

		priors, ok := snapshots[hour]
		if !ok {
			priors = LearnRuntimes(samples, hour)
			snapshots[hour] = priors
		}

		est := priors.Estimate(s.Key)
		runtime := s.Runtime()
		byTier[s.Key.Tier] = append(byTier[s.Key.Tier], graded{
			err:    absDuration(est.Median - runtime),
			within: runtime*2 >= est.Median && runtime <= est.Median*2,
			level:  est.Level,
		})
	}

	out := make([]RuntimeAccuracy, 0, len(byTier))

	for _, tier := range slices.Sorted(maps.Keys(byTier)) {
		g := byTier[tier]
		errs := make([]time.Duration, 0, len(g))
		acc := RuntimeAccuracy{Tier: tier, Count: len(g), Levels: map[RuntimeLevel]int{}}

		for _, x := range g {
			errs = append(errs, x.err)
			acc.Levels[x.level]++

			if x.within {
				acc.Within2x++
			}
		}

		slices.SortFunc(errs, cmp.Compare)
		acc.MedianAbsError = errs[nearestRank(5000, len(errs))-1]
		out = append(out, acc)
	}

	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}

	return d
}
