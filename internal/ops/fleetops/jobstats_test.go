package fleetops

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/usage"
)

// reportNow is when the reports over a fixed ledger are read.
var reportNow = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)

// fixedUsage is a job's usage row. A zero window is a day long, so a job's duration
// is what its busy fraction is taken over.
type fixedUsage struct {
	unmeasured           string
	windowMS             int64
	cpuUser, cpuSystem   int64
	guestCPU             int64
	memoryPeak, oomKills int64
	cpuSome              int64
	activeUJ, idleUJ     int64
	energySource         string
}

// fixedJob is one row of a fixed ledger, placed relative to reportNow.
type fixedJob struct {
	lease, tier, node, repo, workflow, job string
	run, attempt                           int64
	result, conclusion, disruption         string
	// startedAgo is how long before reportNow the job started, and ran how long it
	// took to GitHub's result; a zero ran is no result time recorded.
	startedAgo, ran time.Duration
	// queued is how long before the start GitHub queued it; zero is not recorded.
	queued      time.Duration
	vcpu        int64
	memory      int64
	priceMicros int64
	// instanceType is the shape the lease bought; empty is a host-backed lease.
	instanceType string
	usage        *fixedUsage
	// series is the host's per-sample series, as a run's busy fraction of its
	// charged vCPUs from a second before its start to a second after its result;
	// nil writes none. cover is how much of the run it covers, from the start, 1
	// when zero. outside, when set, runs the series for an hour before the job and
	// an hour after it at that fraction instead.
	series  *float64
	cover   float64
	outside *float64
	// guest is the most of its memory the job's own VM says it used, written as a
	// report the node kept whole unless guestPartial; nil writes none.
	guest        *float64
	guestPartial bool
}

func fraction(v float64) *float64 { return &v }

// fixedSeries is a run's series: a sample every ten seconds whose cgroup CPU grows at
// busy of vcpu.
func fixedSeries(t *testing.T, started time.Time, ran time.Duration, cover, busy float64, outside *float64,
	vcpu int64,
) []byte {
	t.Helper()

	if cover == 0 {
		cover = 1
	}
	first := started.Add(-time.Second)
	end := started.Add(time.Duration(cover*float64(ran)) + time.Second)
	if cover < 1 {
		end = started.Add(time.Duration(cover * float64(ran)))
	}
	finish := started.Add(ran)
	rate := func(at time.Time) float64 { return busy }
	if outside != nil {
		first, end = started.Add(-time.Hour), finish.Add(time.Hour)
		rate = func(at time.Time) float64 {
			if at.After(started) && !at.After(finish) {
				return busy
			}
			return *outside
		}
	}
	var points []usage.Point
	var cpu float64
	last := first
	for at := first; ; at = at.Add(10 * time.Second) {
		if at.After(end) {
			at = end
		}
		cpu += rate(at) * float64(vcpu) * float64(at.Sub(last).Microseconds())
		last = at
		points = append(points, usage.Point{OffsetMillis: at.Sub(first).Milliseconds(), CPUUsage: int64(cpu)})
		if !at.Before(end) {
			break
		}
	}
	data, _, err := usage.EncodeSeriesGaps(points, first, alloc.MaxUsageSeriesBytes)
	if err != nil {
		t.Fatalf("encode the series: %v", err)
	}

	return data
}

// fixedGuest is a guest report whose VM, with 4 GB, used peak of it at most.
func fixedGuest(t *testing.T, peak float64) []byte {
	t.Helper()

	const total = 4_000_000_000
	batch := guestreport.Batch{Seq: 1, AgentVersion: "0.15.1", TicksPerSecond: 100}
	for i, used := range []float64{peak / 4, peak, peak / 2} {
		u := int64(used * total)
		batch.Samples = append(batch.Samples, guestreport.Sample{AtMillis: int64(i+1) * 1000, CPUBusyTicks: int64(i),
			CPUTotalTicks: int64(i) * 2, MemUsedBytes: u, MemAvailableBytes: total - u})
	}

	return guestRow(t, []guestreport.Batch{batch}, alloc.GuestReport{Accepted: 1, Hello: true, FinalSeen: true}).Data
}

func fixedStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }

// writeFixedJobs writes rows the way the ledger holds them, so a report reads them
// through the real query and nothing about them was decided by a test's fake.
func writeFixedJobs(t *testing.T, db *state.DB, jobs []fixedJob) {
	t.Helper()

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range jobs {
			j := &jobs[i]
			started := reportNow.Add(-j.startedAgo)
			resultAt, queuedAt := "", ""
			if j.ran > 0 {
				resultAt = fixedStamp(started.Add(j.ran))
			}
			if j.queued > 0 {
				queuedAt = fixedStamp(started.Add(-j.queued))
			}
			conclusion := sql.NullString{String: j.conclusion, Valid: j.conclusion != ""}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_history
				(lease_id, tier, node, repo, workflow_ref, job_name, github_run_id, github_run_attempt,
				 result, result_at, conclusion, disruption, queued_at, started_at, github_queued_at, vcpu,
				 memory, price_micros_per_hour, instance_type)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				j.lease, j.tier, j.node, j.repo, j.workflow, j.job, j.run, j.attempt, j.result, resultAt,
				conclusion, j.disruption, fixedStamp(started), fixedStamp(started), queuedAt, j.vcpu,
				j.memory, j.priceMicros, j.instanceType); err != nil {
				return fmt.Errorf("job %s: %w", j.lease, err)
			}
			if j.series != nil {
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_series (lease_id, codec, series) VALUES (?, ?, ?)`,
					j.lease, usage.SeriesCodecGaps, base64.StdEncoding.EncodeToString(
						fixedSeries(t, started, j.ran, j.cover, *j.series, j.outside, j.vcpu))); err != nil {
					return fmt.Errorf("series of %s: %w", j.lease, err)
				}
			}
			if j.guest != nil {
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_guest_reports
					(lease_id, node, recorded_at, agent_version, agent_schema, codec, data, accepted, refused,
					 dropped_bytes, hello, final_seen, node_restarted)
					VALUES (?, 'n', 't', '0.15.1', 1, ?, ?, 1, 0, 0, 1, ?, 0)`, j.lease, alloc.GuestReportCodec,
					base64.StdEncoding.EncodeToString(fixedGuest(t, *j.guest)), !j.guestPartial); err != nil {
					return fmt.Errorf("guest report of %s: %w", j.lease, err)
				}
			}
			if j.usage == nil {
				continue
			}
			u := j.usage
			window := u.windowMS
			if window == 0 {
				window = 24 * 3_600_000
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO job_usage
				(lease_id, node, recorded_at, source, unmeasured, samples, interval_ms, window_ms, cpu_user_us,
				 cpu_system_us, guest_cpu_us, vmm_cpu_us, memory_peak_bytes, oom_kills, disk_read_bytes,
				 disk_write_bytes, net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets, cpu_some_us,
				 cpu_full_us, memory_some_us, memory_full_us, io_some_us, io_full_us, energy_active_uj,
				 energy_idle_uj, energy_source)
				VALUES (?, ?, ?, 'host', ?, 10, 1000, ?, ?, ?, ?, 0, ?, ?, 0, 0, 0, 0, 0, 0, ?, 0, 0, 0, 0, 0, ?, ?, ?)`,
				j.lease, j.node, fixedStamp(started), u.unmeasured, window, u.cpuUser, u.cpuSystem, u.guestCPU,
				u.memoryPeak, u.oomKills, u.cpuSome, u.activeUJ, u.idleUJ, u.energySource); err != nil {
				return fmt.Errorf("usage of %s: %w", j.lease, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("write the fixed ledger: %v", err)
	}
}

// fixedAllocator is an allocator over a ledger holding jobs.
func fixedAllocator(t *testing.T, jobs []fixedJob) *alloc.Allocator {
	t.Helper()

	stateDir := t.TempDir()
	seedLedger(t, stateDir)

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	writeFixedJobs(t, db, jobs)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, []config.Tier{waitTier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	return a
}

// statsOver builds the report over jobs, a week against the week before.
func statsOver(t *testing.T, by, repo string, jobs []fixedJob) jobStats {
	t.Helper()

	report, err := buildJobStats(t.Context(), fixedAllocator(t, jobs),
		statsQuery{By: by, Repo: repo, Window: 7 * 24 * time.Hour, Now: reportNow})
	if err != nil {
		t.Fatalf("buildJobStats: %v", err)
	}

	return report
}

const fixedWorkflow = "acme/api/.github/workflows/ci.yml@refs/heads/main"

// EVERY JOB IS IN EXACTLY ONE OUTCOME, rates are over those with one, a failure
// billet's lifecycle has something to say about is counted apart from the job's
// own, and each percentile names the job that holds it. A job whose start lies in
// the window before is compared, never counted.
func TestJobStatsCountsOutcomesAndNamesTheJobAtEachPercentile(t *testing.T) {
	t.Parallel()

	job := func(lease, result string, ran time.Duration) fixedJob {
		return fixedJob{lease: lease, tier: "billet-2vcpu", node: "epyc-1", repo: "acme/api", workflow: fixedWorkflow,
			job: "test", run: 100, result: result, startedAgo: 24 * time.Hour, ran: ran, vcpu: 2,
			memory: int64(8 * config.GiB)}
	}
	disrupted := job("attributed-disruption", "failed", 2*time.Minute)
	disrupted.disruption = "spot_interruption"
	leaseFailed := job("attributed-lease", "", 0)
	leaseFailed.conclusion = "failed"
	oldest := job("previous", "succeeded", 5*time.Minute)
	oldest.startedAgo = 8 * 24 * time.Hour
	beyond := job("before-both", "failed", time.Minute)
	beyond.startedAgo = 15 * 24 * time.Hour

	report := statsOver(t, statsByRepository, "", []fixedJob{
		job("a", "succeeded", 10*time.Minute), job("b", "succeeded", 30*time.Minute), job("c", "failed", 20*time.Minute),
		job("d", "canceled", time.Minute), job("e", "cancelled-newly", 3*time.Minute), job("f", "", 0),
		disrupted, leaseFailed, oldest, beyond,
	})

	if len(report.Groups) != 1 || report.Groups[0].Key != "acme/api" {
		t.Fatalf("groups = %+v, want acme/api alone", report.Groups)
	}
	c, p := report.Groups[0].Current, report.Groups[0].Previous
	if c.Jobs != 8 || c.Succeeded != 2 || c.Failed != 1 || c.Canceled != 1 || c.Attributed != 2 || c.NoResult != 1 ||
		c.Other["cancelled-newly"] != 1 {
		t.Errorf("current outcomes = %+v; want 8 jobs: 2 succeeded, 1 failed, 1 canceled, 2 billet's, 1 other, 1 none", c)
	}
	if p.Jobs != 1 || p.Succeeded != 1 {
		t.Errorf("previous = %+v; want the one job of the week before and not the one before that", p)
	}
	if got := c.Succeeded + c.Failed + c.Canceled + c.Attributed + c.NoResult + c.Other["cancelled-newly"]; got != c.Jobs {
		t.Errorf("the outcomes add up to %d of %d jobs", got, c.Jobs)
	}

	d := c.Duration
	if d.Count != 6 || d.P50 != (alloc.Exemplar{Value: 3 * time.Minute, Lease: "e"}) ||
		d.P90 != (alloc.Exemplar{Value: 30 * time.Minute, Lease: "b"}) ||
		d.P99 != (alloc.Exemplar{Value: 30 * time.Minute, Lease: "b"}) || c.NoDuration != 2 {
		t.Errorf("duration = %+v and %d not recorded; want p50 3m on e, p90 and p99 30m on b, of 6 with 2 left out",
			d, c.NoDuration)
	}

	var out bytes.Buffer
	renderJobStats(&out, report)
	for _, want := range []string{
		"jobs that started from 2026-10-09T08:00:00Z to 2026-10-16T08:00:00Z, by repository, against " +
			"2026-10-02T08:00:00Z to 2026-10-09T08:00:00Z",
		`"acme/api"`, "succeeded  28.6% (2)", "previous: 100.0% (1) (-71.4 points)", "failed     14.3% (1)",
		"billet's   28.6% (2)", `other      "cancelled-newly" (1)`, "no result  1",
		"p50 3m0s on e, p90 30m0s on b, p99 30m0s on b (6 jobs; 2 more not recorded)",
		"previous: 1 (+700.0%)", statsCovers,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
}

// FLAKY IS A JOB THAT FAILED AND THEN PASSED ON A RE-RUN OF ONE RUN, and what
// cannot be told is left out of what was judged rather than counted either way.
func TestJobStatsJudgesAFlakeOnlyFromAttemptsItCanTell(t *testing.T) {
	t.Parallel()

	attempt := func(lease string, run, n int64, result string, startedAgo time.Duration) fixedJob {
		return fixedJob{lease: lease, tier: "billet-2vcpu", repo: "acme/api", workflow: fixedWorkflow, job: "test",
			run: run, attempt: n, result: result, startedAgo: startedAgo, ran: time.Minute, vcpu: 2,
			memory: int64(8 * config.GiB)}
	}
	const day = 24 * time.Hour
	lostHost := attempt("lost-1", 104, 1, "failed", day)
	lostHost.disruption = "spot_interruption"
	otherJob := attempt("other-2", 108, 2, "succeeded", day)
	otherJob.job = "lint"

	report := statsOver(t, statsByRepository, "", []fixedJob{
		// Failed, then passed.
		attempt("flaky-1", 100, 1, "failed", day), attempt("flaky-2", 100, 2, "succeeded", day),
		// Passed the first time.
		attempt("clean", 101, 1, "succeeded", day),
		// An attempt not recorded: cannot be told.
		attempt("unknown-1", 102, 0, "failed", day), attempt("unknown-2", 102, 2, "succeeded", day),
		// Two jobs of one attempt share a name: cannot be told.
		attempt("twice-a", 103, 1, "failed", day), attempt("twice-b", 103, 1, "succeeded", day),
		// billet lost the host, and the re-run passed: not the job's flake.
		lostHost, attempt("lost-2", 104, 2, "succeeded", day),
		// Canceled, then passed: not a failure.
		attempt("canceled-1", 105, 1, "canceled", day), attempt("canceled-2", 105, 2, "succeeded", day),
		// Failed in the week before, passed in this one: one flake, of this week.
		attempt("across-1", 106, 1, "failed", 8*day), attempt("across-2", 106, 2, "succeeded", day),
		// Passed, then failed: the order is what makes a flake.
		attempt("regressed-1", 107, 1, "succeeded", day), attempt("regressed-2", 107, 2, "failed", day),
		// Different jobs of one run are not attempts of one job.
		attempt("other-1", 108, 1, "failed", day), otherJob,
		// A re-run still going decides nothing, and neither does a first attempt
		// still going; a flake already there before one is still a flake.
		attempt("going-1", 109, 1, "failed", day), attempt("going-2", 109, 2, "", day),
		attempt("only-going", 110, 1, "", day),
		attempt("done-1", 111, 1, "failed", day), attempt("done-2", 111, 2, "succeeded", day),
		attempt("done-3", 111, 3, "", day),
	})

	c, p := report.Groups[0].Current, report.Groups[0].Previous
	// 100, 101, 104, 105, 106, 107, 108's first job and 111; not 102, 103, 109 or 110,
	// nor 108's second job, whose first attempt ran elsewhere.
	if c.Judged != 8 || c.Flaky != 3 {
		t.Errorf("current judged %d and found %d flaky; want 8 and 3", c.Judged, c.Flaky)
	}
	if p.Judged != 0 || p.Flaky != 0 {
		t.Errorf("previous judged %d and found %d flaky; a run ending in this week belongs to it", p.Judged, p.Flaky)
	}
	if c.WithAttempt != c.Jobs-1 {
		t.Errorf("%d of %d jobs have an attempt; want all but one", c.WithAttempt, c.Jobs)
	}

	var out bytes.Buffer
	renderJobStats(&out, report)
	if want := fmt.Sprintf("37.5%% (3 of 8 job runs; attempt recorded for %d of %d jobs)", c.WithAttempt, c.Jobs); !strings.Contains(out.String(), want) {
		t.Errorf("the report does not say %q:\n%s", want, out.String())
	}

	jobs := statsOver(t, statsByJob, "", []fixedJob{attempt("one", 200, 0, "succeeded", day)})
	if jobs.Groups[0].Current.Judged != 0 {
		t.Errorf("a job with no attempt recorded was judged: %+v", jobs.Groups[0].Current)
	}
	out.Reset()
	renderJobStats(&out, jobs)
	if want := "not recorded (attempt recorded for 0 of 1 jobs)"; !strings.Contains(out.String(), want) {
		t.Errorf("a group nobody could judge does not say %q:\n%s", want, out.String())
	}
}

// A GROUP WITH NOTHING MEASURED SAYS SO, NEVER ZERO: energy is per source and never
// summed across them, and a shape nobody bought has no cost rather than $0. The
// JSON says the same with null.
func TestJobStatsNeverPrintsAFigureItDidNotMeasureAsZero(t *testing.T) {
	t.Parallel()

	base := fixedJob{tier: "billet-2vcpu", repo: "acme/measured", workflow: fixedWorkflow, job: "test", run: 1,
		result: "succeeded", startedAgo: 24 * time.Hour, ran: time.Hour, queued: 30 * time.Second, vcpu: 4,
		memory: int64(16 * config.GiB)}
	rapl, rapl2, meter, unmeasuredEnergy, bought := base, base, base, base, base
	bought.instanceType = "m7i.xlarge"
	rapl.lease, rapl.usage = "rapl", &fixedUsage{activeUJ: 2_000_000, idleUJ: 500_000, energySource: alloc.EnergyRAPL}
	rapl2.lease, rapl2.usage = "rapl-2", &fixedUsage{activeUJ: 1_000_000, idleUJ: 500_000, energySource: alloc.EnergyRAPL}
	meter.lease, meter.usage = "meter", &fixedUsage{activeUJ: 5_000_000, idleUJ: 2_000_000, energySource: alloc.EnergyMeter}
	unmeasuredEnergy.lease, unmeasuredEnergy.usage = "no-energy", &fixedUsage{unmeasured: alloc.UsageEnergy}
	bought.lease, bought.priceMicros = "bought", 2_000_000
	nothing := fixedJob{lease: "nothing", tier: "billet-2vcpu", repo: "acme/silent", workflow: fixedWorkflow, job: "x",
		result: "succeeded", startedAgo: 24 * time.Hour}

	report := statsOver(t, statsByRepository, "", []fixedJob{rapl, rapl2, meter, unmeasuredEnergy, bought, nothing})
	if len(report.Groups) != 2 {
		t.Fatalf("groups = %+v, want two", report.Groups)
	}
	if report.Groups[0].Key != "acme/measured" {
		t.Errorf("the first group is %q; the one with the vCPU-hours comes first", report.Groups[0].Key)
	}

	m := report.Groups[0].Current
	if want := []statsEnergy{
		{Source: alloc.EnergyMeter, ActiveMicrojoules: 5_000_000, IdleMicrojoules: 2_000_000, Jobs: 1},
		{Source: alloc.EnergyRAPL, ActiveMicrojoules: 3_000_000, IdleMicrojoules: 1_000_000, Jobs: 2},
	}; len(m.Energy) != 2 || m.Energy[0] != want[0] || m.Energy[1] != want[1] {
		t.Errorf("energy = %+v, want %+v", m.Energy, want)
	}
	if m.EnergyUnmeasured != 2 {
		t.Errorf("%d jobs have no energy measured, want the one naming it and the one with no report", m.EnergyUnmeasured)
	}
	if m.CostJobs != 1 || m.CostUSD != 2 {
		t.Errorf("cost = $%v over %d jobs, want $2 over the one bought at $2 an hour for an hour", m.CostUSD, m.CostJobs)
	}
	if m.VCPUJobs != 5 || m.VCPUHours != 20 {
		t.Errorf("vCPU-hours = %v over %d jobs, want 20 over 5", m.VCPUHours, m.VCPUJobs)
	}
	if m.Wait.Count != 5 || m.Wait.P99 != 30*time.Second {
		t.Errorf("queue wait = %+v, want p99 30s over 5", m.Wait)
	}

	var out bytes.Buffer
	renderJobStats(&out, report)
	for _, want := range []string{
		"energy     meter: 5.0 J active, 2.0 J idle (the whole host at the wall) over 1 jobs; rapl: 3.0 J active, " +
			"1.0 J idle (package RAPL, a model rather than a meter) over 2 jobs; " +
			"not measured for 2 jobs",
		"cloud cost $2.00 over 1 of 5 jobs", "vCPU-hours 20.0 over 5 of 5 jobs", "queue wait p99 30s (5 jobs)",
		"energy     not measured", "cloud cost no bought shape", "duration   not recorded for any of 1 jobs",
		"queue wait not recorded for any of 1 jobs", "vCPU-hours not recorded for any of 1 jobs",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := renderJobStatsJSON(&out, report); err != nil {
		t.Fatalf("renderJobStatsJSON: %v", err)
	}
	var decoded struct {
		Groups []struct {
			Key     string `json:"key"`
			Current struct {
				Duration     *statsDurations   `json:"duration"`
				QueueWaitP99 *float64          `json:"queue_wait_p99_seconds"`
				VCPUHours    *float64          `json:"vcpu_hours"`
				CostUSD      *float64          `json:"cloud_cost_usd"`
				FlakyRate    *float64          `json:"flaky_rate"`
				Energy       []statsEnergyJSON `json:"energy"`
			} `json:"current"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	silent := decoded.Groups[1].Current
	if decoded.Groups[1].Key != "acme/silent" || silent.Duration != nil || silent.QueueWaitP99 != nil ||
		silent.VCPUHours != nil || silent.CostUSD != nil || silent.FlakyRate != nil || len(silent.Energy) != 0 {
		t.Errorf("the group with nothing measured = %+v; want every figure null and no energy", decoded.Groups[1])
	}
	first := decoded.Groups[0].Current
	if first.CostUSD == nil || *first.CostUSD != 2 || first.VCPUHours == nil || *first.VCPUHours != 20 ||
		len(first.Energy) != 2 || first.Energy[0].ActiveJoules != 5 {
		t.Errorf("the measured group = %+v", decoded.Groups[0])
	}
}

// EACH ENERGY SOURCE IS WORDED AS IT MEASURES: only a source that splits out the
// idle draw says what was idle, an unsplit total says idle is inside it, and the
// JSON gives no idle figure where there is none.
func TestJobStatsWordsEachEnergySourceAsItMeasures(t *testing.T) {
	t.Parallel()

	job := func(lease, source string) fixedJob {
		return fixedJob{lease: lease, tier: "t", repo: "acme/api", workflow: fixedWorkflow, job: "test", run: 1,
			result: "succeeded", startedAgo: time.Hour, ran: time.Minute,
			usage: &fixedUsage{activeUJ: 4_000_000, idleUJ: 9_000_000, energySource: source}}
	}
	report := statsOver(t, statsByRepository, "", []fixedJob{job("a", alloc.EnergyProcess),
		job("b", alloc.EnergyRAPLUnsplit), job("c", "watts-from-a-later-billet")})

	var out bytes.Buffer
	renderJobStats(&out, report)
	for _, want := range []string{
		"process: 4.0 J (macOS's own estimate for the VM's process; the machine's idle draw is not in it) over 1 jobs",
		"rapl-unsplit: 4.0 J (package RAPL with no idle baseline, so idle is inside it) over 1 jobs",
		`"watts-from-a-later-billet": 4.0 J, split by a source this binary does not know over 1 jobs`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "9.0 J") {
		t.Errorf("an idle figure no source split out was printed:\n%s", out.String())
	}

	figures := statsFiguresJSONOf(&report.Groups[0].Current)
	for _, e := range figures.Energy {
		if e.IdleJoules != nil {
			t.Errorf("%s has an idle figure %v, want null", e.Source, *e.IdleJoules)
		}
		switch {
		case e.Source == "watts-from-a-later-billet" && e.IdleIncluded != nil:
			t.Errorf("a source this binary does not know says idle included %t, want null", *e.IdleIncluded)
		case e.Source != "watts-from-a-later-billet" && (e.IdleIncluded == nil || *e.IdleIncluded != (e.Source == alloc.EnergyRAPLUnsplit)):
			t.Errorf("%s says idle included %v", e.Source, e.IdleIncluded)
		}
	}
}

// NOTHING BOUGHT IS SAID ONLY WHEN NO LEASE BOUGHT A SHAPE: a bought shape with no
// recorded price, or still running, has a cost nobody can say.
func TestJobStatsTellsAnUnrecordedCostFromNone(t *testing.T) {
	t.Parallel()

	job := func(lease, shape string, price int64, ran time.Duration) fixedJob {
		return fixedJob{lease: lease, tier: "t", repo: "acme/api", workflow: fixedWorkflow, job: "test", run: 1,
			result: "succeeded", startedAgo: 2 * time.Hour, ran: ran, vcpu: 2, instanceType: shape, priceMicros: price}
	}
	for _, tc := range []struct {
		name string
		jobs []fixedJob
		want string
	}{
		{"host-backed alone", []fixedJob{job("a", "", 0, time.Hour)}, "cloud cost no bought shape"},
		{"bought at no recorded price", []fixedJob{job("a", "m7i.xlarge", 0, time.Hour)},
			"cloud cost not recorded for the 1 jobs on a bought shape"},
		{"bought and still running", []fixedJob{job("a", "m7i.xlarge", 1_000_000, 0)},
			"cloud cost not recorded for the 1 jobs on a bought shape"},
		{"one priced, one not", []fixedJob{job("a", "m7i.xlarge", 1_000_000, time.Hour), job("b", "m7i.xlarge", 0, time.Hour)},
			"cloud cost $1.00 over 1 of 2 jobs; not recorded for 1 more on a bought shape"},
	} {
		var out bytes.Buffer
		renderJobStats(&out, statsOver(t, statsByRepository, "", tc.jobs))
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: the report does not say %q:\n%s", tc.name, tc.want, out.String())
		}
	}
}

// --by GROUPS ON THE FIGURE IT NAMES, one workflow on two branches being one
// workflow, and --repo keeps out what it does not name.
func TestJobStatsGroupsByWhatItIsAsked(t *testing.T) {
	t.Parallel()

	job := func(lease, repo, ref, name, tier, node string) fixedJob {
		return fixedJob{lease: lease, repo: repo, workflow: ref, job: name, tier: tier, node: node, run: 1,
			result: "succeeded", startedAgo: 24 * time.Hour, ran: time.Minute, vcpu: 2}
	}
	jobs := []fixedJob{
		job("a", "acme/api", "acme/api/.github/workflows/ci.yml@refs/heads/main", "test", "small", "n1"),
		job("b", "acme/api", "acme/api/.github/workflows/ci.yml@refs/pull/7/merge", "test", "large", "n2"),
		job("c", "acme/api", "acme/api/.github/workflows/ci.yml@refs/heads/main", "lint", "small", "n1"),
		job("d", "acme/web", "acme/web/.github/workflows/ci.yml@refs/heads/main", "test", "small", "n1"),
	}

	keys := func(r jobStats) map[string]int {
		out := map[string]int{}
		for i := range r.Groups {
			out[r.Groups[i].Key] = r.Groups[i].Current.Jobs
		}

		return out
	}
	for by, want := range map[string]map[string]int{
		statsByRepository: {"acme/api": 3, "acme/web": 1},
		statsByWorkflow:   {"acme/api/.github/workflows/ci.yml": 3, "acme/web/.github/workflows/ci.yml": 1},
		statsByJob: {"acme/api/.github/workflows/ci.yml\x00test": 2, "acme/api/.github/workflows/ci.yml\x00lint": 1,
			"acme/web/.github/workflows/ci.yml\x00test": 1},
		statsByTier: {"small": 3, "large": 1},
		statsByNode: {"n1": 3, "n2": 1},
	} {
		got := keys(statsOver(t, by, "", jobs))
		if len(got) != len(want) {
			t.Errorf("--by %s = %v, want %v", by, got, want)
		}
		for key, n := range want {
			if got[key] != n {
				t.Errorf("--by %s: %q has %d jobs, want %d (all: %v)", by, key, got[key], n, got)
			}
		}
	}

	if got := keys(statsOver(t, statsByTier, "ACME/Web", jobs)); len(got) != 1 || got["small"] != 1 {
		t.Errorf("--repo acme/web = %v, want its one job", got)
	}
}

// EVERY STRING A WORKFLOW CHOSE IS QUOTED IN THE TEXT AND ESCAPED ON THE PAGE, and
// the page can run nothing: a job name holding a newline cannot forge a line, and
// one holding markup stays text.
func TestJobStatsQuotesAndEscapesWhatAWorkflowChose(t *testing.T) {
	t.Parallel()

	report := statsOver(t, statsByJob, "", []fixedJob{{lease: "a", tier: "t", repo: "acme/api", workflow: fixedWorkflow,
		job: "x\njobs       forged <script>alert(1)</script>", run: 1, result: "succeeded", startedAgo: time.Hour,
		ran: time.Minute}})

	var out bytes.Buffer
	renderJobStats(&out, report)
	if strings.Contains(out.String(), "\njobs       forged") || !strings.Contains(out.String(), `x\njobs       forged`) {
		t.Errorf("the job name was not quoted:\n%s", out.String())
	}

	out.Reset()
	if err := renderJobStatsPage(&out, report); err != nil {
		t.Fatalf("renderJobStatsPage: %v", err)
	}
	page := out.String()
	if strings.Contains(page, "<script") || !strings.Contains(page, "&lt;script&gt;") {
		t.Errorf("the page holds a script or lost the name's text:\n%s", page)
	}
	if !strings.Contains(page, "default-src &#39;none&#39;") && !strings.Contains(page, "default-src 'none'") {
		t.Errorf("the page carries no policy that forbids fetching:\n%s", page)
	}
	if strings.Contains(page, "script-src") {
		t.Errorf("the page's policy admits a script:\n%s", page)
	}
}

// A WINDOW IS DAYS OR A DURATION, AND ONLY A POSITIVE ONE.
func TestAReportWindowIsDaysOrADuration(t *testing.T) {
	t.Parallel()

	for s, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour} {
		if got, err := parseReportWindow(s); err != nil || got != want {
			t.Errorf("parseReportWindow(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "0d", "-1d", "d", "1.5d", "36501d", "99999999999999d", "0h", "-1h", "soon"} {
		if got, err := parseReportWindow(s); err == nil {
			t.Errorf("parseReportWindow(%q) = %v, want refused", s, got)
		}
	}
}

// THROUGH THE COMMAND: a flag that would change the question is refused by name,
// and the report reads the ledger the configuration names.
func TestJobsStatsThroughTheCommand(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	seedLedger(t, stateDir)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--by", "owner"}, `--by takes one of repository, workflow, job, tier, node, not "owner"`},
		{[]string{"--repo", "api"}, `--repo takes owner/name, not "api"`},
		{[]string{"--json", "--html"}, "--json and --html each print the whole report"},
		{[]string{"--since", "0d"}, `"0d" is not between 1d and`},
	} {
		err := Jobs(t.Context(), processEnv(), append([]string{"stats", "--config", cfg}, tc.args...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("billet jobs stats %v = %v, want it refused saying %q", tc.args, err, tc.want)
		}
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	started := time.Since(reportNow)
	writeFixedJobs(t, db, []fixedJob{{lease: "recent", tier: "billet-2vcpu", repo: "acme/api", workflow: fixedWorkflow,
		job: "test", run: 1, result: "succeeded", startedAgo: time.Hour - started, ran: time.Minute, vcpu: 2}})
	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"stats", "--config", cfg, "--since", "1d", "--json"}); err != nil {
			t.Errorf("billet jobs stats --json: %v", err)
		}
	})
	var report jobStatsJSON
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out)
	}
	if len(report.Groups) != 1 || report.Groups[0].Key != "acme/api" || report.Groups[0].Current.Jobs != 1 ||
		report.Groups[0].Current.Duration == nil || report.Groups[0].Current.Duration.P50Lease != "recent" {
		t.Errorf("groups = %+v, want the one recent job named at its p50", report.Groups)
	}
}
