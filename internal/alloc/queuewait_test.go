package alloc

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// NEAREST RANK IS THE SMALLEST RANK WHOSE SHARE REACHES THE PERCENTILE, and
// p99.99 is where a float rounding of 0.9999*n picks the wrong job.
func TestNearestRankIsTheCeilingOfTheShare(t *testing.T) {
	for _, c := range []struct{ p, n, want int }{
		{5000, 1, 1}, {9999, 1, 1},
		{5000, 2, 1}, {7500, 2, 2},
		{5000, 100, 50}, {9900, 100, 99}, {9999, 100, 100},
		{9999, 10000, 9999}, {9999, 10001, 10000}, {9999, 20000, 19998},
		{9000, 7, 7}, {7500, 7, 6},
	} {
		if got := nearestRank(c.p, c.n); got != c.want {
			t.Errorf("nearestRank(%d, %d) = %d, want %d", c.p, c.n, got, c.want)
		}
	}
}

// A JOB MISSING AN END IS LEFT OUT, NEVER COUNTED AS NO WAIT, and a negative
// wait stays negative in the distribution and is counted.
func TestSummarizeLeavesUnknownsOutAndKeepsNegatives(t *testing.T) {
	base := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)
	job := func(lease string, wait time.Duration) JobWait {
		return JobWait{LeaseID: lease, QueuedAt: base, StartedAt: base.Add(wait)}
	}

	jobs := []JobWait{
		job("e", 40*time.Second), job("a", -2*time.Second), job("c", 20*time.Second),
		{LeaseID: "unknown", StartedAt: base},
		job("d", 30*time.Second), job("b", 10*time.Second), job("f", 52*time.Minute),
	}

	s := Summarize(jobs, JobWait.Wait)

	if s.Count != 6 {
		t.Fatalf("count = %d, want the six jobs with both ends", s.Count)
	}
	if s.P50 != 20*time.Second || s.P75 != 40*time.Second || s.P90 != 52*time.Minute {
		t.Errorf("p50, p75, p90 = %s, %s, %s; want 20s, 40s, 52m", s.P50, s.P75, s.P90)
	}
	if s.Longest != 52*time.Minute || s.LongestLease != "f" {
		t.Errorf("longest = %s on %q, want 52m on f", s.Longest, s.LongestLease)
	}
	if s.Negative != 1 || s.Shortest != -2*time.Second {
		t.Errorf("negative = %d, shortest %s; want 1, -2s", s.Negative, s.Shortest)
	}
	if want := (52*time.Minute + 98*time.Second) / 6; s.Mean != want {
		t.Errorf("mean = %s, want %s with the negative wait counted as negative", s.Mean, want)
	}

	// A WINDOW AT THE REPORT'S BOUND, of waits a day long, would overflow a sum
	// of nanoseconds into a negative mean.
	long := make([]JobWait, 120_000)
	for i := range long {
		long[i] = job("l", 24*time.Hour)
	}
	if m := Summarize(long, JobWait.Wait).Mean; m != 24*time.Hour {
		t.Errorf("the mean of 120,000 one-day waits is %s, want 24h", m)
	}

	if none := Summarize([]JobWait{{LeaseID: "x", StartedAt: base}}, JobWait.Wait); none != (WaitSummary{}) {
		t.Errorf("a set with no measurable wait summarized as %+v, want nothing", none)
	}
}

// waitsHeard is a QueueWaitObserver that keeps what it heard.
type waitsHeard []time.Duration

func (w *waitsHeard) JobWaited(_ string, wait time.Duration) { *w = append(*w, wait) }

// WHICHEVER END LANDS SECOND TELLS THE WAIT, once: a queue time recorded before
// the start is told by the start, and a repeated start tells nothing.
func TestAQueueTimeRecordedBeforeTheStartIsToldByTheStart(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("linux", 2, 8*config.GiB)}, WithClock(func() time.Time { return now }))

	heard := &waitsHeard{}
	a.ObserveQueueWaits(heard)

	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	name := "billet-" + lease.ID
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 1, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: "j",
		QueuedAt: now.Add(-7 * time.Minute)}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}
	if len(*heard) != 0 {
		t.Fatalf("a wait was told before the job started: %v", *heard)
	}

	now = now.Add(time.Minute)
	for range 2 {
		if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 1, name, 1, 9, "j", JobIdentity{}); err != nil {
			t.Fatalf("StartPoolRunner: %v", err)
		}
	}

	if len(*heard) != 1 || (*heard)[0] != 8*time.Minute {
		t.Errorf("heard %v, want one 8m wait told by the start", *heard)
	}
}

// A NODE'S BUSY REPORT TELLS THE WAIT TOO when it is the start that lands
// second, and a repeated report tells nothing more.
func TestABusyReportAfterTheQueueTimeTellsTheWaitOnce(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	heard := &waitsHeard{}
	a.ObserveQueueWaits(heard)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	fresh, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	advanceTo(t, a, fresh, PhaseOnline)

	if err := a.RecordJobIdentity(t.Context(), fresh.ID, HistoryJob{JobID: "j",
		QueuedAt: now.Add(-7 * time.Minute)}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}

	now = now.Add(time.Minute)
	for range 2 {
		if err := a.Advance(t.Context(), fresh.ID, fresh.Epoch, PhaseBusy); err != nil {
			t.Fatalf("Advance to busy: %v", err)
		}
	}

	if len(*heard) != 1 || (*heard)[0] != 8*time.Minute {
		t.Errorf("heard %v, want one 8m wait told by the busy report", *heard)
	}
}

// A WRITE THAT ROLLS BACK TELLS NOTHING, and leaves the wait to the write that
// commits: the start it recorded is undone with it.
func TestARolledBackStartTellsNoWait(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("linux", 2, 8*config.GiB)}, WithClock(func() time.Time { return now }))

	heard := &waitsHeard{}
	a.ObserveQueueWaits(heard)

	lease := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, HistoryJob{JobID: "j",
		QueuedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("RecordJobIdentity: %v", err)
	}

	refused := errors.New("refused after the start")
	err := a.txTelling(t.Context(), func(tx *sql.Tx, told *waitsToTell) error {
		if err := a.recordJobStartTx(t.Context(), tx, lease.ID, now, told); err != nil {
			return err
		}
		if len(told.waits) != 1 {
			t.Errorf("the start inside the write collected %d waits, want 1", len(told.waits))
		}

		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("txTelling = %v, want the refusal", err)
	}
	if len(*heard) != 0 {
		t.Fatalf("a rolled-back write told %v", *heard)
	}
	if got, err := a.Job(t.Context(), lease.ID); err != nil || got.StartedAt != "" {
		t.Fatalf("after the rollback the start reads %q (%v), want none", got.StartedAt, err)
	}

	name := "billet-" + lease.ID
	if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
		LaunchRequestID: 1, RunnerName: name}); err != nil {
		t.Fatalf("RegisterPoolRunner: %v", err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", 1, name, 1, 9, "j", JobIdentity{}); err != nil {
		t.Fatalf("StartPoolRunner: %v", err)
	}
	if len(*heard) != 1 || (*heard)[0] != time.Minute {
		t.Errorf("heard %v after the committed start, want one 1m wait", *heard)
	}
}

// THE REPORT READS WHAT THE LEDGER RECORDED: GitHub's times from the job's
// message, the purchase from the lease, the start from the runner's binding,
// windowed on the start. Its parts add up to the wait.
func TestJobWaitsReadsTheWindowAndItsPartsAddUp(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("linux", 2, 8*config.GiB)}, WithClock(func() time.Time { return now }))

	start := func(request int64, job HistoryJob) *Lease {
		t.Helper()

		lease := reserve(t, a, "linux")
		now = now.Add(3 * time.Second)
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, request); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		name := "billet-" + lease.ID
		if err := a.RegisterPoolRunner(t.Context(), PoolRunner{LeaseID: lease.ID, Tier: "linux",
			LaunchRequestID: request, RunnerName: name}); err != nil {
			t.Fatalf("RegisterPoolRunner: %v", err)
		}
		now = now.Add(40 * time.Second)
		if _, err := a.StartPoolRunner(t.Context(), lease.ID, "linux", request, name, request, 9,
			job.JobID, JobIdentity{}); err != nil {
			t.Fatalf("StartPoolRunner: %v", err)
		}
		if err := a.RecordJobIdentity(t.Context(), lease.ID, job); err != nil {
			t.Fatalf("RecordJobIdentity: %v", err)
		}

		return lease
	}

	before := start(1, HistoryJob{JobID: "early", QueuedAt: now.Add(-time.Minute)})
	now = now.Add(time.Hour)
	since := now

	queued := now.Add(-52 * time.Minute)
	bought := now
	waited := start(2, HistoryJob{JobID: "late", QueuedAt: queued,
		ScaleSetAssignedAt: queued.Add(10 * time.Second), RunnerAssignedAt: now.Add(42 * time.Second)})
	unstarted := reserve(t, a, "linux")
	if err := a.Assign(t.Context(), unstarted.ID, unstarted.Epoch, 0, 3); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	jobs, partial, err := a.JobWaits(t.Context(), since)
	if err != nil {
		t.Fatalf("JobWaits: %v", err)
	}
	if partial || len(jobs) != 1 || jobs[0].LeaseID != waited.ID {
		t.Fatalf("JobWaits = %+v (partial %v), want only %s: %s started before the window and %s never",
			jobs, partial, waited.ID, before.ID, unstarted.ID)
	}

	// THE BOUND KEEPS THE NEWEST, and a window of exactly the bound is whole.
	all, partial, err := a.jobWaits(t.Context(), since.Add(-2*time.Hour), 1)
	if err != nil || !partial || len(all) != 1 || all[0].LeaseID != waited.ID {
		t.Errorf("a bound of one over two started jobs read %+v (partial %v, %v); want only the newest, %s, partial",
			all, partial, err, waited.ID)
	}
	if _, partial, err := a.jobWaits(t.Context(), since.Add(-2*time.Hour), 2); err != nil || partial {
		t.Errorf("a bound of two over two started jobs was called partial (%v)", err)
	}

	w := jobs[0]
	if w.Tier != "linux" || w.VCPU != 2 || !w.BoughtAt.Equal(bought) ||
		!w.StartedAt.Equal(bought.Add(43*time.Second)) || !w.QueuedAt.Equal(queued) {
		t.Errorf("the job's times are %+v; want shape 2, bought at %s, started 43s later, queued at %s",
			w, bought, queued)
	}

	wait, _ := w.Wait()
	github, _ := w.AtGitHub()
	room, _ := w.ForRoom()
	toStart, ok := w.ToStart()
	if !ok || wait != 52*time.Minute+43*time.Second || github != 10*time.Second ||
		room != 52*time.Minute-10*time.Second || toStart != 43*time.Second || github+room+toStart != wait {
		t.Errorf("wait %s = at github %s + for room %s + to start %s; want 52m43s = 10s + 51m50s + 43s",
			wait, github, room, toStart)
	}
}
