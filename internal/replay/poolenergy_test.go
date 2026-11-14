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

// WorkWeek is five working days of MorningBurst, Monday to Friday, each its own
// seed, then a weekend with no work: the shape whose nights and weekend a pool
// that keeps its hosts pays for.
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
	// The weekend's one job, so the span runs to Monday and every host the week
	// left powered is counted over the weekend it would have idled through.
	week.Arrivals = append(week.Arrivals, Arrival{
		At: DefaultStart.Add(7 * 24 * time.Hour), Tier: energyTiers[0].Label, Owner: DefaultOwner,
		Repository: "web", WorkflowRef: DefaultOwner + "/web/.github/workflows/ci.yml@refs/heads/main",
		RunID: 9_000_000, Duration: Duration(5 * time.Minute), Result: ResultSucceeded,
	})
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

// A POOL HOST IS POWERED FROM ITS JOIN TO ITS RELEASE, the ask plus the clearance
// when the replay recorded no release, and busy while it carried a lease: a burst
// that a pool of none carries, then asks to release, is powered for its jobs, their
// last idle_after and the clearance, and idle for all of that but its jobs.
func TestAPoolHostIsPoweredFromItsJoinToItsRelease(t *testing.T) {
	t.Parallel()

	trace := Burst(7, Params{Jobs: 8, Tiers: []string{"billet-4vcpu"}})
	report := Run(t, poolOnlyFleet(0, 2, 10*time.Minute), trace, Options{DirectAssignment: true})
	speaks(t, report)

	p := report.PoolPowered()
	if p.Hosts == 0 || p.ReleasesAsk != p.Hosts {
		t.Fatalf("pool power %+v; want every joined host asked to leave", p)
	}
	var want float64
	for _, o := range report.Pool.Ordered {
		if o.Joined.IsZero() {
			continue
		}
		off := o.Released
		if off.IsZero() {
			off = o.ReleaseAsked.Add(PoolClearance)
		}
		want += off.Sub(o.Joined).Hours()
	}
	if math.Abs(p.Powered-want) > 1e-9 {
		t.Errorf("powered %.6f host-hours, want %.6f, each host's join to its release", p.Powered, want)
	}
	// Each host idled at least the idle_after before its release was asked, and
	// the clearance after it.
	if p.Busy <= 0 || p.Idle < float64(p.Hosts)*(10*time.Minute+PoolClearance).Hours()-1e-9 ||
		math.Abs(p.Busy+p.Idle-p.Powered) > 1e-9 {
		t.Errorf("busy %.4f and idle %.4f of %.4f powered host-hours; want idle at least each host's "+
			"idle_after and clearance", p.Busy, p.Idle, p.Powered)
	}
	if none := (&Report{}).PoolPowered(); none != (PoolPower{}) {
		t.Errorf("a report with no pool measured %+v", none)
	}
}

// THE POOL'S RELEASE, PRICED: a week of work through a pool that keeps its hosts and
// through pools that release a host idle past idle_after, beside the fleet a deployment
// without a pool keeps on all week, with each idle host-hour priced at the reference
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

	type row struct {
		name  string
		power PoolPower
		p50   time.Duration
		p99   time.Duration
		jobs  int
	}
	var rows []row
	for _, idle := range []time.Duration{0, 5 * time.Minute, 20 * time.Minute, time.Hour} {
		name := "never released (no idle_after)"
		if idle > 0 {
			name = "idle_after " + idle.String()
		}
		report := Run(t, energyFleet(lead, idle, maxHosts), trace,
			Options{DirectAssignment: true, Log: discardLogger()})
		speaks(t, report)
		all := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
		rows = append(rows, row{name: name, power: report.PoolPowered(), p50: all.P50, p99: all.P99,
			jobs: len(report.Records)})
		t.Logf("%s: %+v, queue wait p50 %s p99 %s", name, rows[len(rows)-1].power, all.P50, all.P99)
	}

	// THE FLEET WITHOUT A POOL keeps the most hosts the week needed at once on for
	// all of it, every hour of which no lease ran on one being idle.
	kept := rows[0].power
	alwaysOn := float64(kept.MostAtOnce) * kept.SpanHours
	alwaysIdle := alwaysOn - kept.Busy
	kwh := func(hours float64) float64 { return hours * idleWallWatts / 1000 }

	var b strings.Builder
	fmt.Fprintf(&b, "Work week: %d jobs a weekday (MorningBurst, seed %d), a weekend with one job; pool of "+
		"128-vCPU, 512-GiB hosts, floor 0, max %d, lead time %s, scaler every minute; an idle host-hour "+
		"priced at %.1f W.\n\n", jobs, seed, maxHosts, lead, idleWallWatts)
	fmt.Fprintln(&b, "| Configuration | Hosts joined | Most at once | Powered host-hours | Busy | Idle | "+
		"Idle energy (kWh) | Saved against always on | Queue wait p50 | p99 |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|---|---|---|")
	fmt.Fprintf(&b, "| %d hosts always on (no pool) | %d | %d | %.1f | %.1f | %.1f | %.1f | | | |\n",
		kept.MostAtOnce, kept.MostAtOnce, kept.MostAtOnce, alwaysOn, kept.Busy, alwaysIdle, kwh(alwaysIdle))
	for _, r := range rows {
		saved := 100 * (alwaysIdle - r.power.Idle) / alwaysIdle
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f kWh (%.0f%%) | %s | %s |\n",
			r.name, r.power.Hosts, r.power.MostAtOnce, r.power.Powered, r.power.Busy, r.power.Idle,
			kwh(r.power.Idle), kwh(alwaysIdle-r.power.Idle), saved, r.p50, r.p99)
	}
	fmt.Fprintf(&b, "\nSpan %.1f h; every configuration ran all %d jobs.\n", kept.SpanHours, rows[0].jobs)
	t.Log("\n" + b.String())
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
