package replay

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// THE TAIL IS NEAREST RANK, in integers: p99.99 of ten thousand and one waits
// is the ten-thousandth, where a float rounding of 0.9999*n picks another.
func TestTheTailPercentilesAreNearestRank(t *testing.T) {
	t.Parallel()

	sorted := make([]time.Duration, 10001)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Second
	}

	for _, c := range []struct {
		p    int
		want time.Duration
	}{
		{5000, 5001 * time.Second}, {9000, 9001 * time.Second}, {9900, 9901 * time.Second},
		{9999, 10000 * time.Second},
	} {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("percentile(%d) of 10001 = %s, want %s", c.p, got, c.want)
		}
	}

	if got := percentile([]time.Duration{time.Minute}, 5000); got != time.Minute {
		t.Errorf("the median of one wait is %s, want it", got)
	}
}

// A REPORT GROUPS BY THE CHARGED SHAPE, and the tail is over started jobs only.
func TestQueueWaitIsGroupedByShape(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	rec := func(vcpu int, wait time.Duration, started bool) Record {
		r := Record{VCPU: vcpu, Arrival: start, QueueWait: Duration(wait)}
		if started {
			r.StartedAt = start.Add(wait)
		}

		return r
	}

	// A hundred 2 vCPU waits of 1s to 100s, given out of order.
	r := &Report{Records: []Record{rec(8, time.Minute, true), rec(8, time.Hour, false)}}
	for i := range 100 {
		r.Records = append(r.Records, rec(2, time.Duration((i*37)%100+1)*time.Second, true))
	}

	byShape := r.QueueWaitByShape()

	if two := byShape[2]; two.Count != 100 || two.P50 != 50*time.Second || two.P90 != 90*time.Second ||
		two.P95 != 95*time.Second || two.P99 != 99*time.Second || two.P9999 != 100*time.Second ||
		two.Max != 100*time.Second {
		t.Errorf("the 2 vCPU shape is %+v; want a hundred waits, p50 50s, p90 90s, p95 95s, p99 99s, "+
			"p99.99 and max 100s", two)
	}

	if eight := byShape[8]; eight.Count != 1 || eight.P9999 != time.Minute {
		t.Errorf("the 8 vCPU shape is %+v; want its one started job, the unstarted one left out", eight)
	}
}

// UTILIZATION AND EMPTY HOSTS ARE INTEGRATED OVER THE SPAN from the ledger's
// own charges: one host, two 4 vCPU charges, the second of two hours idle.
func TestUtilizationAndEmptyHostTimeAreIntegratedFromTheCharges(t *testing.T) {
	t.Parallel()

	// THE ROOM IS THE CEILING when it is below what the hosts hold: 12 vCPU and
	// 48 GiB of the hosts' 16 and 64.
	start := DefaultStart
	fleet := Fleet{
		Hosts: []Host{{Name: "only", VCPU: 8, Memory: 32 * config.GiB},
			{Name: "spare", VCPU: 8, Memory: 32 * config.GiB}},
		MaxVCPU: 12, MaxMemory: 48 * config.GiB,
	}

	// 0h-1h: 8 vCPU on "only"; 1h-2h: nothing; 2h-3h: 4 vCPU on "only". The
	// span is the three hours from the first charge to the last end.
	r := &Report{Fleet: fleet, Records: []Record{
		chargeAt(start, start.Add(time.Hour)),
		chargeAt(start, start.Add(time.Hour)),
		chargeAt(start.Add(2*time.Hour), start.Add(3*time.Hour)),
	}}

	vcpu, memory := r.MeanUtilization()
	if math.Abs(vcpu-12.0/36) > 1e-9 || math.Abs(memory-24.0/144) > 1e-9 {
		t.Errorf("mean utilization %.4f vCPU, %.4f memory; want 12 of 36 vCPU-hours, 24 of 144 GiB-hours",
			vcpu, memory)
	}

	// "only" was empty from 1h to 2h, "spare" for all three.
	if got := r.EmptyHostHours(); math.Abs(got-4) > 1e-9 {
		t.Errorf("%.3f empty host-hours; want 4", got)
	}

	points := r.Utilization(time.Hour)
	if len(points) != 3 || math.Abs(points[0].VCPU-8.0/12) > 1e-9 || points[1].VCPU != 0 ||
		math.Abs(points[2].VCPU-4.0/12) > 1e-9 || !points[1].From.Equal(start.Add(time.Hour)) {
		t.Errorf("hourly utilization %+v; want 8, 0 and 4 of 12 vCPU", points)
	}

	// A HORIZON ENDS THE SPAN: an hour and a half in, the first hour full and
	// the half after it empty.
	r.Horizon = start.Add(90 * time.Minute)
	if vcpu, _ := r.MeanUtilization(); math.Abs(vcpu-8.0/18) > 1e-9 {
		t.Errorf("to the horizon, mean utilization %.4f; want 8 of 18 vCPU-hours", vcpu)
	}
}

// A COMPLETION DELIVERED AND NEVER ARCHIVED IS LOST, whatever the horizon and
// however the ledger dated the start: judged from what the scripted GitHub
// sent, never from a deadline rebuilt from the ledger's times.
func TestADeliveredCompletionTheLedgerNeverSawIsLost(t *testing.T) {
	t.Parallel()

	start := DefaultStart
	r := &Report{Horizon: start.Add(time.Minute), Records: []Record{
		// Delivered, never archived: lost, though its start was dated a
		// nanosecond late and a rebuilt deadline would fall past the horizon.
		{Seq: 1, StartedAt: start.Add(time.Nanosecond)},
		// Running at the horizon, its completion not yet delivered.
		{Seq: 2, StartedAt: start},
		// Delivered and archived.
		{Seq: 3, StartedAt: start, FinishedAt: start.Add(time.Minute)},
	}}

	r.noteLostCompletions(map[int64]bool{1: true, 3: true})

	if len(r.LostCompletions) != 1 || r.LostCompletions[0] != 1 {
		t.Errorf("lost completions %v; want job 1 alone", r.LostCompletions)
	}
}

// A COMPARISON REFUSES A REPORT THAT CANNOT SPEAK FOR ITS REPLAY, each reason
// on its own, and a horizon excuses only the jobs it cut short.
func TestAComparisonRefusesAnUnspeakableReport(t *testing.T) {
	t.Parallel()

	horizon := DefaultStart.Add(time.Hour)

	for _, c := range []struct {
		name   string
		report Report
		want   string
	}{
		{"sound", Report{}, ""},
		{"overcommit", Report{Violations: []string{"host-1 over by 2 vCPU"}}, "overcommit"},
		{"missing", Report{Missing: []int64{4}}, "no row for jobs [4]"},
		{"unstarted without a horizon", Report{Unstarted: []int64{5}}, "with no horizon"},
		{"unfinished without a horizon", Report{Unfinished: []int64{6}}, "with no horizon"},
		{"cut short by the horizon", Report{Horizon: horizon, Unstarted: []int64{5}, Unfinished: []int64{6}}, ""},
		{"lost at the horizon", Report{Horizon: horizon, Unfinished: []int64{6}, LostCompletions: []int64{6}},
			"jobs [6] were completed"},
	} {
		got := c.report.unspeakable()
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: unspeakable says %q; want %q", c.name, got, c.want)
		}
	}
}
