package alloc

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// QueueWaitObserver is told each job's queue wait once, when the ledger first
// holds both GitHub's queue time and billet's start of the job. internal/metrics
// satisfies it by its method alone, so the ledger imports no metrics library.
//
// CALLED AFTER THE WRITE COMMITS, on the writer's goroutine: an implementation
// returns at once and touches nothing that could reach the ledger. The wait is
// the start (billet's clock) less the queue time (GitHub's), and can be
// negative when the two clocks disagree.
type QueueWaitObserver interface {
	JobWaited(tier string, wait time.Duration)
}

// ObserveQueueWaits sets what this allocator tells about queue waits; nil, the
// default, tells nothing. It may be called while writes run.
func (a *Allocator) ObserveQueueWaits(o QueueWaitObserver) {
	if o == nil {
		a.waits.Store(nil)

		return
	}

	a.waits.Store(&o)
}

// waitsToTell holds the queue waits one write transaction completed.
type waitsToTell struct {
	waits []JobWait
}

// ifCompleted adds the lease's wait when this transaction's write is the one
// that made both of its ends known: the queue time and the start are each
// written once, so exactly one write ever completes the pair, whichever of the
// two lands second. That holds because writes are serialized (SQLite's
// IMMEDIATE begin, PostgreSQL's advisory lock), so this before is read after
// any other write's commit; it must stay inside the write that changes the row.
func (w *waitsToTell) ifCompleted(ctx context.Context, tx *sql.Tx, leaseID string,
	before ledgerdb.ReadJobWaitRow,
) error {
	if before.GithubQueuedAt != "" && before.StartedAt != "" {
		return nil
	}

	after, found, err := readJobWait(ctx, tx, leaseID)
	if err != nil || !found || after.GithubQueuedAt == "" || after.StartedAt == "" {
		return err
	}

	w.waits = append(w.waits, JobWait{LeaseID: leaseID, Tier: after.Tier,
		QueuedAt: parseStamp(after.GithubQueuedAt), StartedAt: parseStamp(after.StartedAt)})

	return nil
}

// txTelling runs one write and, only once it commits, tells the observer every
// queue wait it completed.
func (a *Allocator) txTelling(ctx context.Context, fn func(*sql.Tx, *waitsToTell) error) error {
	var told waitsToTell

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		told = waitsToTell{}

		return fn(tx, &told)
	})
	if err != nil {
		return err
	}

	o := a.waits.Load()
	if o == nil {
		return nil
	}

	for i := range told.waits {
		if wait, ok := told.waits[i].Wait(); ok {
			(*o).JobWaited(told.waits[i].Tier, wait)
		}
	}

	return nil
}

// JobWait is one started job's times, each zero where the ledger has none.
//
// TWO CLOCKS. QueuedAt, ScaleSetAssignedAt and RunnerAssignedAt are GitHub's;
// BoughtAt, AssignedAt and StartedAt are billet's. A difference between the two
// is only as good as their agreement, and a negative one says they disagreed.
type JobWait struct {
	LeaseID            string
	Tier               string
	VCPU               int
	QueuedAt           time.Time
	ScaleSetAssignedAt time.Time
	RunnerAssignedAt   time.Time
	BoughtAt           time.Time
	AssignedAt         time.Time
	StartedAt          time.Time
}

// Wait is the whole wait, from GitHub queueing the job to billet starting it.
func (w JobWait) Wait() (time.Duration, bool) { return between(w.QueuedAt, w.StartedAt) }

// AtGitHub is the part GitHub held the job before handing it to this scale
// set, on GitHub's clock alone.
func (w JobWait) AtGitHub() (time.Duration, bool) {
	return between(w.QueuedAt, w.ScaleSetAssignedAt)
}

// ForRoom is from the scale set's assignment to billet buying the lease that
// ran the job. Negative when the lease was bought first: the room was there
// before the job, and the launch part then holds that head start too.
func (w JobWait) ForRoom() (time.Duration, bool) {
	return between(w.ScaleSetAssignedAt, w.BoughtAt)
}

// ToStart is from billet buying the lease to the job starting on it, on
// billet's clock alone: the launch, the runner's registration, and any time
// the runner stood idle before GitHub gave it this job.
func (w JobWait) ToStart() (time.Duration, bool) { return between(w.BoughtAt, w.StartedAt) }

// Waited is one job record's times as a wait.
func (r JobRecord) Waited() JobWait {
	return JobWait{
		LeaseID: r.LeaseID, Tier: r.Tier, VCPU: int(r.VCPU),
		QueuedAt:           parseStamp(r.GitHubQueuedAt),
		ScaleSetAssignedAt: parseStamp(r.GitHubScaleSetAssignedAt),
		RunnerAssignedAt:   parseStamp(r.GitHubRunnerAssignedAt),
		BoughtAt:           parseStamp(r.BoughtAt),
		AssignedAt:         parseStamp(r.AssignedAt),
		StartedAt:          parseStamp(r.StartedAt),
	}
}

func between(from, to time.Time) (time.Duration, bool) {
	if from.IsZero() || to.IsZero() {
		return 0, false
	}

	return to.Sub(from), true
}

// jobWaitsLimit bounds one report's read. The reference deployment's busiest
// recorded day, 2026-10-16, started under 3,000 jobs, so this is months of
// history; a window past it says the report is partial rather than reading
// without end.
const jobWaitsLimit = 200_000

// JobWaits reads the jobs that started at or after since, newest start first.
// The bool is true when the window held more than one read takes, in which case
// the oldest are the ones left out.
func (a *Allocator) JobWaits(ctx context.Context, since time.Time) ([]JobWait, bool, error) {
	return a.jobWaits(ctx, since, jobWaitsLimit)
}

func (a *Allocator) jobWaits(ctx context.Context, since time.Time, limit int) ([]JobWait, bool, error) {
	var out []JobWait

	partial := false

	err := a.db.View(ctx, func(q querier) error {
		// ONE PAST THE BOUND, so a window of exactly the bound is not called partial.
		rows, err := state.ReadQueries(q).ListJobWaits(ctx, ledgerdb.ListJobWaitsParams{
			Since: sql.NullString{String: ts(since), Valid: true}, MaxRows: int64(limit) + 1,
		})
		if err != nil {
			return fmt.Errorf("alloc: list job waits: %w", err)
		}

		if len(rows) > limit {
			rows, partial = rows[:limit], true
		}

		out = make([]JobWait, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, JobWait{
				LeaseID: r.LeaseID, Tier: r.Tier, VCPU: int(r.Vcpu),
				QueuedAt:           parseStamp(r.GithubQueuedAt),
				ScaleSetAssignedAt: parseStamp(r.GithubScaleSetAssignedAt),
				RunnerAssignedAt:   parseStamp(r.GithubRunnerAssignedAt),
				BoughtAt:           parseStamp(r.BoughtAt),
				AssignedAt:         parseStamp(r.AssignedAt),
				StartedAt:          parseStamp(r.StartedAt),
			})
		}

		return nil
	})

	return out, partial, err
}

// AwaitingQueueTime is a started job with no GitHub queue time on record, and
// what finds it in GitHub's REST record of its run.
type AwaitingQueueTime struct {
	LeaseID string
	Tier    string
	// Repository is owner/name.
	Repository string
	RunID      int64
	// JobID is GitHub's workflow-job id, which the write presents so it can
	// only complete the row of the job it names.
	JobID string
	// Cursor is where the page after this row begins.
	Cursor QueueTimeCursor
}

// QueueTimeCursor is a position in the newest-first order of jobs awaiting a
// queue time; the zero cursor is the newest.
type QueueTimeCursor struct {
	startedAt string
	leaseID   string
}

// JobsAwaitingQueueTime reads at most limit jobs that started at or after
// since and have no GitHub queue time, newest start first, from after.
func (a *Allocator) JobsAwaitingQueueTime(ctx context.Context, since time.Time, after QueueTimeCursor,
	limit int,
) ([]AwaitingQueueTime, error) {
	var out []AwaitingQueueTime

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListJobsAwaitingQueueTime(ctx,
			ledgerdb.ListJobsAwaitingQueueTimeParams{
				Since:         sql.NullString{String: ts(since), Valid: true},
				BeforeStarted: after.startedAt, BeforeLease: after.leaseID, MaxRows: int64(limit),
			})
		if err != nil {
			return fmt.Errorf("alloc: list jobs awaiting a queue time: %w", err)
		}

		out = make([]AwaitingQueueTime, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, AwaitingQueueTime{LeaseID: r.LeaseID, Tier: r.Tier,
				Repository: r.Repo, RunID: r.RunID, JobID: r.GithubJobID,
				Cursor: QueueTimeCursor{startedAt: r.StartedAt, leaseID: r.LeaseID}})
		}

		return nil
	})

	return out, err
}

// WaitSummary is one distribution of waits. Every percentile is nearest rank
// over the jobs the part could be computed for; Count is how many those were.
type WaitSummary struct {
	Count int
	Mean  time.Duration
	P50   time.Duration
	P75   time.Duration
	P90   time.Duration
	P99   time.Duration
	P9999 time.Duration
	// Longest is the largest, and LongestLease the job that waited it, so a
	// number leads to a job.
	Longest      time.Duration
	LongestLease string
	// Shortest is the smallest, and Negative counts the values below zero, kept
	// in the distribution as they are. What one means is the part's: a room
	// bought before the job, or two clocks or one clock's times out of order.
	Shortest time.Duration
	Negative int
}

// Summarize is the distribution of one part over the jobs it can be computed
// for. A job missing either end is left out, never counted as zero.
func Summarize(jobs []JobWait, part func(JobWait) (time.Duration, bool)) WaitSummary {
	type sample struct {
		d     time.Duration
		lease string
	}

	samples := make([]sample, 0, len(jobs))

	// IN SECONDS, AS A FLOAT: the report's bound of days-long waits would
	// overflow a sum of nanoseconds.
	var sum float64

	for i := range jobs {
		d, ok := part(jobs[i])
		if !ok {
			continue
		}

		samples = append(samples, sample{d, jobs[i].LeaseID})
		sum += d.Seconds()
	}

	if len(samples) == 0 {
		return WaitSummary{}
	}

	// TIES BY LEASE, so the exemplar named for the longest wait is the same on
	// every run over the same rows.
	slices.SortFunc(samples, func(x, y sample) int {
		return cmp.Or(cmp.Compare(x.d, y.d), cmp.Compare(x.lease, y.lease))
	})

	n := len(samples)
	at := func(perTenThousand int) time.Duration {
		return samples[nearestRank(perTenThousand, n)-1].d
	}

	out := WaitSummary{
		Count: n, Mean: time.Duration(sum / float64(n) * float64(time.Second)),
		P50: at(5000), P75: at(7500), P90: at(9000), P99: at(9900), P9999: at(9999),
		Longest: samples[n-1].d, LongestLease: samples[n-1].lease, Shortest: samples[0].d,
	}

	for _, s := range samples {
		if s.d < 0 {
			out.Negative++
		}
	}

	return out
}

// nearestRank is the 1-based rank of a percentile given in parts per ten
// thousand: the smallest rank whose share of n reaches it, ceil(p*n/10000).
// In integers, because a float rounding of 0.9999*n is exactly where p99.99
// would pick the wrong job.
func nearestRank(perTenThousand, n int) int {
	return max((perTenThousand*n+9999)/10000, 1)
}

// stamp is a time as the ledger writes it, or empty for a time not known.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return ts(t)
}

// parseStamp reads a time the ledger wrote. Empty, or a value this binary
// cannot read, is the zero time: not recorded, which every reader leaves out.
func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}
