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
		if got := NearestRank(c.p, c.n); got != c.want {
			t.Errorf("NearestRank(%d, %d) = %d, want %d", c.p, c.n, got, c.want)
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

// waitsSeenCommitted is a QueueWaitObserver that, as it is told a wait, reads
// the lease's row through the reader pool: told inside the write, the read
// cannot yet see the start that write recorded.
type waitsSeenCommitted struct {
	t     *testing.T
	a     *Allocator
	lease string
	waitsHeard
}

func (w *waitsSeenCommitted) JobWaited(tier string, wait time.Duration) {
	if got, err := w.a.Job(w.t.Context(), w.lease); err != nil || got.StartedAt == "" {
		w.t.Errorf("told a wait while the row's start reads %q (%v): told before the commit", got.StartedAt, err)
	}

	w.waitsHeard.JobWaited(tier, wait)
}

// A WRITE THAT ROLLS BACK TELLS NOTHING, and leaves the wait to the write that
// commits, which tells it only once its start can be read.
func TestARolledBackStartTellsNoWait(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB},
		[]config.Tier{tier("linux", 2, 8*config.GiB)}, WithClock(func() time.Time { return now }))

	lease := reserve(t, a, "linux")
	probe := &waitsSeenCommitted{t: t, a: a, lease: lease.ID}
	heard := &probe.waitsHeard
	a.ObserveQueueWaits(probe)

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

// THE TABLE OF A JOB'S FACTS READS WHAT THE LEDGER HOLDS, the usage report only where
// one exists: no report is nil, which a reader cannot take for a report of zeros.
// A window holding more than one read takes says so and loses its oldest.
func TestJobFactsReadsAWindowAndSaysWhenItIsPartial(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier("linux", 2, 8*config.GiB)})
	base := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)

	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for lease, row := range map[string]struct {
			started time.Time
			attempt int64
		}{
			"older":   {base.Add(-time.Hour), 1},
			"newer":   {base, 2},
			"outside": {base.Add(-3 * time.Hour), 3},
			"after":   {base.Add(2 * time.Hour), 4},
		} {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, repo, workflow_ref, job_name, run_id, github_run_id, run_attempt, result,
				 result_at, disruption, queued_at, started_at, github_queued_at, vcpu, memory,
				 price_micros_per_hour)
				VALUES (?, 'small', 'acme/api', 'acme/api/.github/workflows/ci.yml@refs/heads/main', 'test', 5, 6,
				        ?, 'failed', ?, 'spot', 't', ?, ?, 2, 8, 9)`,
				lease, row.attempt, ts(row.started.Add(time.Minute)), ts(row.started),
				ts(row.started.Add(-time.Second))); err != nil {
				return err
			}
		}

		_, err := tx.ExecContext(t.Context(), `INSERT INTO job_usage
			(lease_id, node, recorded_at, source, unmeasured, samples, interval_ms, window_ms, cpu_user_us,
			 cpu_system_us, guest_cpu_us, vmm_cpu_us, memory_peak_bytes, oom_kills, disk_read_bytes,
			 disk_write_bytes, net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets, cpu_some_us,
			 cpu_full_us, memory_some_us, memory_full_us, io_some_us, io_full_us, energy_active_uj,
			 energy_idle_uj, energy_source)
			VALUES ('newer', 'n', 't', 'host', 'energy,io', 1, 1000, 11, 12, 13, 14, 0, 15, 16, 0, 0, 0, 0, 0, 0, 17, 0, 0,
			        0, 0, 0, 18, 19, 'rapl')`)

		return err
	}); err != nil {
		t.Fatalf("write the rows: %v", err)
	}

	from, until := base.Add(-2*time.Hour), base.Add(2*time.Hour)
	facts, partial, err := a.JobFacts(t.Context(), from, until)
	if err != nil {
		t.Fatalf("JobFacts: %v", err)
	}
	if partial || len(facts) != 2 || facts[0].LeaseID != "newer" || facts[1].LeaseID != "older" {
		t.Fatalf("facts = %+v (partial %t); want newer then older, the window closed at its start and open at its end",
			facts, partial)
	}

	newer, older := facts[0], facts[1]
	if newer.Repo != "acme/api" || newer.Workflow() != "acme/api/.github/workflows/ci.yml" || newer.JobName != "test" ||
		newer.RunID != 6 || newer.RunAttempt != 2 || newer.VCPU != 2 || newer.Memory != 8 || newer.PriceMicrosPerHour != 9 {
		t.Errorf("newer = %+v", newer)
	}
	if d, ok := newer.Duration(); !ok || d != time.Minute {
		t.Errorf("duration = %s, %t; want a minute", d, ok)
	}
	if w, ok := newer.Wait().Wait(); !ok || w != time.Second {
		t.Errorf("wait = %s, %t; want a second", w, ok)
	}
	if !newer.Attributed() || newer.Succeeded() {
		t.Errorf("a failure with a disruption: attributed %t, succeeded %t", newer.Attributed(), newer.Succeeded())
	}

	u := newer.Usage
	if u == nil || u.Measured(UsageEnergy) || u.Measured(UsageIO) || !u.Measured(UsageCPU) || u.WindowMillis != 11 ||
		u.CPUUserMicros != 12 || u.CPUSystemMicros != 13 || u.GuestCPUMicros != 14 || u.MemoryPeakBytes != 15 ||
		u.OOMKills != 16 || u.CPUSomeMicros != 17 || u.EnergyActiveMicrojoules != 18 || u.EnergyIdleMicrojoules != 19 ||
		u.EnergySource != "rapl" {
		t.Errorf("usage = %+v", u)
	}
	if older.Usage != nil {
		t.Errorf("a job with no report has usage %+v, want nil", older.Usage)
	}

	facts, partial, err = a.jobFacts(t.Context(), from, until, 1)
	if err != nil {
		t.Fatalf("jobFacts: %v", err)
	}
	if !partial || len(facts) != 1 || facts[0].LeaseID != "newer" {
		t.Errorf("facts = %+v (partial %t); want the newest alone, marked partial", facts, partial)
	}
	if _, partial, err := a.jobFacts(t.Context(), from, until, 2); err != nil || partial {
		t.Errorf("a window of exactly the bound: partial %t, %v", partial, err)
	}
}

// A RESULT STAMPED BEFORE THE START IS NO DURATION, since the two are billet's clock
// and a message handled out of order says nothing about how long a job ran.
func TestAResultBeforeItsStartIsNoDuration(t *testing.T) {
	at := time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)
	if d, ok := (&JobFact{StartedAt: at, ResultAt: at.Add(-time.Second)}).Duration(); ok {
		t.Errorf("duration = %s, want none", d)
	}
	if d, ok := (&JobFact{StartedAt: at}).Duration(); ok {
		t.Errorf("no result time gave duration %s, want none", d)
	}
	if d, ok := (&JobFact{StartedAt: at, ResultAt: at}).Duration(); !ok || d != 0 {
		t.Errorf("a job of no time = %s, %t; want zero, recorded", d, ok)
	}
}

// THE PERCENTILES NAME THE JOB THAT HOLDS THEM, the same one on every run, and an
// empty distribution has none rather than percentiles of zero.
func TestSummarizeDurationsNamesTheJobAtEachRank(t *testing.T) {
	if got := SummarizeDurations(nil); got != (DurationSummary{}) {
		t.Errorf("no samples = %+v, want nothing", got)
	}

	got := SummarizeDurations([]Exemplar{{3 * time.Second, "c"}, {time.Second, "z"}, {time.Second, "a"}, {2 * time.Second, "b"}})
	if got.Count != 4 || got.P50 != (Exemplar{time.Second, "z"}) || got.P90 != (Exemplar{3 * time.Second, "c"}) ||
		got.P99 != (Exemplar{3 * time.Second, "c"}) {
		t.Errorf("summary = %+v; want p50 1s on z (a before it on the tie), p90 and p99 3s on c", got)
	}
}

// A RUN ATTEMPT IS WRITTEN ONCE, on the row of the job the write names: a later read
// cannot rewrite it and a message about another job cannot splice its attempt on.
func TestARunAttemptIsWrittenOnceOnTheJobItNames(t *testing.T) {
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{tier("linux", 2, 8*config.GiB)})
	lease, err := a.Reserve(t.Context(), "linux")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 2); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	attempt := func() int64 {
		t.Helper()
		rec, err := a.Job(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("Job: %v", err)
		}

		return rec.Job.RunAttempt
	}
	record := func(job HistoryJob) {
		t.Helper()
		if err := a.RecordJobIdentity(t.Context(), lease.ID, job); err != nil {
			t.Fatalf("RecordJobIdentity: %v", err)
		}
	}

	record(HistoryJob{JobID: "j1", Name: "test"})
	if got := attempt(); got != 0 {
		t.Fatalf("a write naming no attempt recorded %d", got)
	}
	record(HistoryJob{JobID: "other", RunAttempt: 9})
	if got := attempt(); got != 0 {
		t.Errorf("a message about another job wrote attempt %d", got)
	}
	record(HistoryJob{JobID: "j1", RunAttempt: -3})
	if got := attempt(); got != 0 {
		t.Errorf("a negative attempt wrote %d", got)
	}
	record(HistoryJob{JobID: "j1", RunAttempt: 2})
	if got := attempt(); got != 2 {
		t.Errorf("attempt = %d, want 2", got)
	}
	record(HistoryJob{JobID: "j1", RunAttempt: 3})
	if got := attempt(); got != 2 {
		t.Errorf("a later read rewrote the attempt to %d", got)
	}
	record(HistoryJob{JobID: "j1"})
	if got := attempt(); got != 2 {
		t.Errorf("a read that did not say blanked the attempt to %d", got)
	}
}
