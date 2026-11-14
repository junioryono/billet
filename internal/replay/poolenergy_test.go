package replay

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// idleWallWatts is the reference EPYC 7763's draw at the wall with nothing running,
// read from its Shelly plug over five quiet minutes on 2026-10-23
// (docs/reference/records/cpu-placement.md, session 2): the energy a host draws for
// every hour it is powered and carries nothing.
const idleWallWatts = 149.9

// energyTiers are a week's shapes, at 4 GiB a vCPU, the smallest the most common.
var energyTiers = []TierShape{
	{Label: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB, Arch: config.ArchX64},
	{Label: "billet-4vcpu", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchX64},
	{Label: "billet-8vcpu", VCPU: 8, Memory: 32 * config.GiB, Arch: config.ArchX64},
	{Label: "billet-16vcpu", VCPU: 16, Memory: 64 * config.GiB, Arch: config.ArchX64},
}

// WorkWeek is five working days of MorningBurst, Monday to Friday from DefaultStart,
// each its own seed, and nothing over the weekend: the shape whose nights and
// weekend a pool that keeps its hosts pays for.
func WorkWeek(seed uint64, jobsPerDay int) Trace {
	var week Trace
	tiers := make([]string, len(energyTiers))
	for i, s := range energyTiers {
		tiers[i] = s.Label
	}
	for day := range 5 {
		d := MorningBurst(seed+uint64(day), Params{Jobs: jobsPerDay, Tiers: tiers,
			Start: DefaultStart.Add(time.Duration(day) * 24 * time.Hour)})
		for i := range d.Arrivals {
			// A RUN IS ONE DAY'S, never two days' that drew the same number.
			d.Arrivals[i].RunID += int64(day) * 1_000_000
		}
		week.Arrivals = append(week.Arrivals, d.Arrivals...)
	}
	week.Normalize()

	return week
}

// energyFleet is a pool of the reference host's shape and nothing else.
func energyFleet(lead, idleAfter time.Duration, maxHosts int) Fleet {
	return Fleet{
		Tiers:     energyTiers,
		MaxVCPU:   4096,
		MaxMemory: 16384 * config.GiB,
		Pool: &PoolFleet{
			Archs:     map[config.Arch]PoolArch{config.ArchX64: {Floor: 0, Max: maxHosts}},
			Host:      Host{VCPU: 128, Memory: 512 * config.GiB},
			LeadTime:  lead,
			Interval:  time.Minute,
			IdleAfter: idleAfter,
		},
	}
}

// A POOL HOST IS POWERED FROM ITS JOIN TO ITS RELEASE, counted against figures
// worked out by hand from literal times: a recorded release over the fallback, the
// ask plus exactly five minutes without one, the window's end for a host never
// asked, a host that never joined not at all; busy once per host however many
// leases overlap on it; and the most at once by the same intervals, a host leaving
// at the instant another joins not counted twice.
func TestAPoolHostIsPoweredFromItsJoinToItsRelease(t *testing.T) {
	t.Parallel()

	t0 := DefaultStart
	h := func(hours float64) time.Time { return t0.Add(time.Duration(hours * float64(time.Hour))) }
	r := &Report{
		Pool: &PoolReport{
			Ordered: []PoolOrderRecord{
				// a: asked at 4h, no recorded release, so off at 4h05.
				{Order: "a", Joined: h(1), ReleaseAsked: h(4)},
				// b: joins at 4h05, the instant a leaves; its recorded release at 5h
				// wins over its ask at 4h50 plus the clearance.
				{Order: "b", Joined: h(4).Add(5 * time.Minute), ReleaseAsked: h(4).Add(50 * time.Minute),
					Released: h(5)},
				// c: joins at 6h and is never asked: on to the window's end.
				{Order: "c", Joined: h(6)},
				// d: ordered, never joined.
				{Order: "d"},
			},
			ReleasesAsked: []string{"a", "b"},
		},
		Records: []Record{
			// Two overlapping leases on a: busy from 1h to 2h30 once, 1.5 h.
			{ChargedFrom: h(1), FinishedAt: h(2), VCPU: 4, Memory: 16 * config.GiB, Node: alloc.PoolNodeName("a")},
			{ChargedFrom: h(1.5), FinishedAt: h(2.5), VCPU: 4, Memory: 16 * config.GiB, Node: alloc.PoolNodeName("a")},
			// b, from 4h10 to 4h40, 0.5 h.
			{ChargedFrom: h(4).Add(10 * time.Minute), FinishedAt: h(4).Add(40 * time.Minute), VCPU: 4,
				Memory: 16 * config.GiB, Node: alloc.PoolNodeName("b")},
			// c, 1 h.
			{ChargedFrom: h(6), FinishedAt: h(7), VCPU: 4, Memory: 16 * config.GiB, Node: alloc.PoolNodeName("c")},
		},
	}

	// One host at a time throughout: b joins as a leaves, and c after b.
	got := r.PoolPowered(t0, h(10))
	want := PoolPower{Hosts: 3, ReleasesAsk: 2, MostAtOnce: 1,
		// a 3h05m, b 55m, c 4h.
		Powered: 3 + 5.0/60 + 55.0/60 + 4,
		Busy:    1.5 + 0.5 + 1,
	}
	want.Idle = want.Powered - want.Busy
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if got.Hosts != want.Hosts || got.ReleasesAsk != want.ReleasesAsk || got.MostAtOnce != want.MostAtOnce ||
		!near(got.Powered, want.Powered) || !near(got.Busy, want.Busy) || !near(got.Idle, want.Idle) {
		t.Errorf("PoolPowered = %+v,\nwant %+v", got, want)
	}

	// A WINDOW CUTS A HOST'S POWERED TIME at both ends, and its busy time with it:
	// a from 1h30, b whole, c to 6h30.
	cut := r.PoolPowered(h(1.5), h(6.5))
	if wantPowered := (4 + 5.0/60 - 1.5) + 55.0/60 + 0.5; !near(cut.Powered, wantPowered) ||
		!near(cut.Busy, 1+0.5+0.5) {
		t.Errorf("PoolPowered over 1h30 to 6h30 = %+v, want %.4f powered and 2 busy", cut, wantPowered)
	}

	if none := (&Report{}).PoolPowered(t0, h(10)); none != (PoolPower{}) {
		t.Errorf("a report with no pool measured %+v", none)
	}
}

// THROUGH THE HARNESS, a burst that a pool of none carries, then asks to release, is
// powered for its jobs, their last idle_after and the clearance.
func TestAPooledBurstIsPoweredThroughItsIdleAfterAndClearance(t *testing.T) {
	t.Parallel()

	trace := Burst(7, Params{Jobs: 8, Tiers: []string{"billet-4vcpu"}})
	report := Run(t, poolOnlyFleet(0, 2, 10*time.Minute), trace, Options{DirectAssignment: true})
	speaks(t, report)

	p := report.PoolPowered(trace.Arrivals[0].At.Add(-time.Hour), trace.Arrivals[0].At.Add(24*time.Hour))
	if p.Hosts == 0 || p.ReleasesAsk != p.Hosts || p.Busy <= 0 ||
		p.Idle < float64(p.Hosts)*(10*time.Minute+PoolClearance).Hours()-1e-9 {
		t.Errorf("pool power %+v; want every host asked to leave, idle at least its idle_after and clearance", p)
	}
}

// THE POOL'S RELEASE, PRICED: a work week through a pool that keeps its hosts and
// through pools that release a host idle past idle_after, beside a counterfactual
// fleet that keeps the never-released pool's peak on all week, each weighed over the
// same seven days from Monday 09:00 and each idle host-hour priced at the reference
// host's measured idle draw at the wall. Written by hand to
// BILLET_REPLAY_POOL_ENERGY_OUT, for docs/reference/records/pool-release-energy.md.
//
// Settings: BILLET_REPLAY_JOBS_PER_DAY (default 300), BILLET_REPLAY_SEED (default 1),
// BILLET_REPLAY_LEAD (default 3m), BILLET_REPLAY_POOL_MAX (default 4).
func TestPoolReleaseEnergy(t *testing.T) {
	out := os.Getenv("BILLET_REPLAY_POOL_ENERGY_OUT")
	if out == "" {
		t.Skip("set BILLET_REPLAY_POOL_ENERGY_OUT to a results file to price the pool's release")
	}
	intEnv := func(name string, def int) int {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("%s=%q is not a positive count", name, v)
		}
		return n
	}
	jobs, seed, maxHosts := intEnv("BILLET_REPLAY_JOBS_PER_DAY", 300), intEnv("BILLET_REPLAY_SEED", 1),
		intEnv("BILLET_REPLAY_POOL_MAX", 4)
	lead := 3 * time.Minute
	if v := os.Getenv("BILLET_REPLAY_LEAD"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("BILLET_REPLAY_LEAD=%q is not a duration", v)
		}
		lead = d
	}
	trace := WorkWeek(uint64(seed), jobs)
	// ONE WINDOW FOR EVERY ROW: Monday 09:00 to the next, which holds every join,
	// every job and every release tail of a week whose last job is on Friday.
	from, to := DefaultStart, DefaultStart.Add(7*24*time.Hour)

	type row struct {
		name     string
		power    PoolPower
		p50, p99 time.Duration
		jobs     int
	}
	var rows []row
	for _, idle := range []time.Duration{0, 5 * time.Minute, 20 * time.Minute, time.Hour} {
		name := "never released (no idle_after)"
		if idle > 0 {
			name = "released at idle_after " + idle.String()
		}
		report := Run(t, energyFleet(lead, idle, maxHosts), trace,
			Options{DirectAssignment: true, Log: discardLogger()})
		speaks(t, report)
		if last := lastFinish(report); !last.Before(to) {
			t.Fatalf("%s: the last job finished at %s, past the window's end", name, last)
		}
		all := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
		rows = append(rows, row{name: name, power: report.PoolPowered(from, to), p50: all.P50, p99: all.P99,
			jobs: len(report.Records)})
		t.Logf("%s: %+v, queue wait p50 %s p99 %s", name, rows[len(rows)-1].power, all.P50, all.P99)
	}

	// THE COUNTERFACTUAL FLEET WITHOUT A POOL keeps the never-released pool's peak
	// on for the whole window, and carries the same work as that pool placed it, so
	// its busy hours are that pool's: a fleet replayed on its own could place the
	// same jobs differently.
	kept := rows[0].power
	window := to.Sub(from).Hours()
	alwaysOn := float64(kept.MostAtOnce) * window
	alwaysIdle := alwaysOn - kept.Busy
	kwh := func(hours float64) float64 { return hours * idleWallWatts / 1000 }

	var b strings.Builder
	fmt.Fprintf(&b, "Work week: %d jobs a weekday (MorningBurst, seed %d) and none at the weekend, over the %.0f "+
		"hours from Monday 09:00; pool of 128-vCPU, 512-GiB hosts, floor 0, max %d, lead time %s, scaler every "+
		"minute; a released host counted powered to its release ask plus %s; an idle host-hour priced at %.1f W.\n\n",
		jobs, seed, window, maxHosts, lead, PoolClearance, idleWallWatts)
	fmt.Fprintln(&b, "| Configuration | Hosts joined | Most at once | Powered host-hours | Busy | Idle | "+
		"Idle energy (kWh) | Saved against always on | Queue wait p50 | p99 |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|---|---|---|")
	fmt.Fprintf(&b, "| %d hosts always on (counterfactual: the never-released pool's peak, its placements) "+
		"| %d | %d | %.1f | %.1f | %.1f | %.1f | | | |\n",
		kept.MostAtOnce, kept.MostAtOnce, kept.MostAtOnce, alwaysOn, kept.Busy, alwaysIdle, kwh(alwaysIdle))
	for _, r := range rows {
		saved := 100 * (alwaysIdle - r.power.Idle) / alwaysIdle
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f kWh (%.0f%%) | %s | %s |\n",
			r.name, r.power.Hosts, r.power.MostAtOnce, r.power.Powered, r.power.Busy, r.power.Idle,
			kwh(r.power.Idle), kwh(alwaysIdle-r.power.Idle), saved, r.p50, r.p99)
	}
	fmt.Fprintf(&b, "\nEvery configuration ran all %d jobs.\n", rows[0].jobs)
	t.Log("\n" + b.String())
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lastFinish is when a replay's last job finished.
func lastFinish(r *Report) time.Time {
	var last time.Time
	for i := range r.Records {
		last = later(last, r.Records[i].FinishedAt)
	}

	return last
}
