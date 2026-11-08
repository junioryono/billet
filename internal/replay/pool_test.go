package replay

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// poolOnlyFleet is a fleet whose every host the pool orders: none fixed, one x64 tier,
// and a pool from floor to max of 16-vcpu hosts arriving a lead time after each order.
func poolOnlyFleet(floor, maxHosts int, lead, idleAfter time.Duration) Fleet {
	return Fleet{
		Tiers:     []TierShape{{Label: "billet-4vcpu", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchX64}},
		MaxVCPU:   1024,
		MaxMemory: 4096 * config.GiB,
		Pool: &PoolFleet{
			Archs:     map[config.Arch]PoolArch{config.ArchX64: {Floor: floor, Max: maxHosts}},
			Host:      Host{VCPU: 16, Memory: 64 * config.GiB},
			LeadTime:  lead,
			Interval:  time.Minute,
			IdleAfter: idleAfter,
		},
	}
}

// speaks fails the test when the report cannot speak for its replay: an overcommit, a
// lost job, a start or completion delivered and never recorded, or a job that never
// started; and when the pool's observation stopped with hosts still above the floor.
func speaks(t *testing.T, report *Report) {
	t.Helper()
	if why := report.unspeakable(); why != "" {
		t.Fatalf("the replay cannot speak for itself: %s", why)
	}
	if report.Pool == nil {
		t.Fatal("the report says nothing of the pool")
	}
	if report.Pool.Stalled || len(report.Pool.StoppedAbove) > 0 {
		t.Fatalf("the replay's pool stopped stepping early (stalled %v) with hosts above the floor not yet asked "+
			"to leave: %v", report.Pool.Stalled, report.Pool.StoppedAbove)
	}
}

// joinsAtItsLeadTime fails the test when an order's host did not join exactly its lead
// time after it was ordered: a join the harness held back would be measured as the
// pool's.
func joinsAtItsLeadTime(t *testing.T, pr *PoolReport, lead time.Duration) {
	t.Helper()
	for i := range pr.Ordered {
		o := &pr.Ordered[i]
		if got := o.Joined.Sub(o.Ordered); got < lead || got > lead+time.Millisecond {
			t.Errorf("order %s joined %s after it was ordered; want its lead time, %s", o.Order, got, lead)
		}
	}
}

// asksAfterIdle fails the test unless every release the pool asked for came at least
// idle_after after the later of its host's join and the end of the last job it ran, and
// releases on one architecture came a step apart or more: a pool asking at once, or
// several hosts in one step, would pass a check of which hosts were asked alone.
func asksAfterIdle(t *testing.T, report *Report, idleAfter, step time.Duration) {
	t.Helper()
	pr := report.Pool
	lastJob := map[string]time.Time{}
	for i := range report.Records {
		rec := &report.Records[i]
		if rec.FinishedAt.After(lastJob[rec.Node]) {
			lastJob[rec.Node] = rec.FinishedAt
		}
	}
	var asks []PoolOrderRecord
	for i := range pr.Ordered {
		o := pr.Ordered[i]
		if o.ReleaseAsked.IsZero() {
			continue
		}
		asks = append(asks, o)
		idleSince := o.Joined
		if last := lastJob["pool-"+o.Order]; last.After(idleSince) {
			idleSince = last
		}
		if o.ReleaseAsked.Before(idleSince.Add(idleAfter)) {
			t.Errorf("order %s's release was asked at %s, %s after its host was last busy at %s; want idle_after, %s",
				o.Order, o.ReleaseAsked, o.ReleaseAsked.Sub(idleSince), idleSince, idleAfter)
		}
	}
	slices.SortFunc(asks, func(a, b PoolOrderRecord) int { return a.ReleaseAsked.Compare(b.ReleaseAsked) })
	for i := 1; i < len(asks); i++ {
		if a, b := asks[i-1], asks[i]; a.Arch == b.Arch && b.ReleaseAsked.Sub(a.ReleaseAsked) < step {
			t.Errorf("orders %s and %s were asked to leave %s apart; the pool asks one a step", a.Order, b.Order,
				b.ReleaseAsked.Sub(a.ReleaseAsked))
		}
	}
}

// A POOL GROWS FROM ITS FLOOR FOR A BURST, AND ASKS TO SHRINK ONCE IT IS QUIET. The real
// scaler, allocator and listeners, over a played source: a burst on a tier no fixed
// host serves is demand, the pool orders one host at a time while one is on its way, up
// to its maximum, every job runs on a host it ordered, and once nothing has run for
// idle_after it asks for each host's release, down to its floor of none.
func TestAPoolGrowsFromItsFloorForABurstAndAsksToShrink(t *testing.T) {
	t.Parallel()
	lead := 3 * time.Minute
	report := Run(t, poolOnlyFleet(0, 3, lead, 10*time.Minute), Burst(7, Params{Jobs: 24, Tiers: []string{"billet-4vcpu"}}),
		Options{DirectAssignment: true})
	speaks(t, report)
	pr := report.Pool
	if got := pr.MostJoined[config.ArchX64]; got != 3 || len(pr.Ordered) != 3 {
		t.Fatalf("the pool placed %d orders and joined at most %d x64 hosts at once; a burst of 24 four-vcpu "+
			"jobs on 16-vcpu hosts wants its maximum, 3, of each", len(pr.Ordered), got)
	}
	joinsAtItsLeadTime(t, pr, lead)
	// ONE HOST AT A TIME, AND NO LATER THAN THAT: each next order is placed at the step
	// the one before joined at, so nothing of the harness's stands between them.
	for i := 1; i < len(pr.Ordered); i++ {
		o, prev := pr.Ordered[i], pr.Ordered[i-1]
		if gap := o.Ordered.Sub(prev.Joined); gap < 0 || gap > time.Millisecond {
			t.Errorf("order %s was placed at %s and order %s joined at %s: the pool orders its next host at "+
				"the step the one on its way joins at", o.Order, o.Ordered, prev.Order, prev.Joined)
		}
	}
	if got := slices.Sorted(slices.Values(pr.ReleasesAsked)); !slices.Equal(got, []string{"r001", "r002", "r003"}) {
		t.Errorf("the pool asked to release %v once the burst was over and idle_after had passed; want "+
			"each of its three hosts once, down to its floor of none", pr.ReleasesAsked)
	}
	asksAfterIdle(t, report, 10*time.Minute, time.Minute)
	p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
	t.Logf("pool of 0..3, lead %s: orders %+v; queue wait p50 %s p90 %s p99 %s; releases asked %v",
		lead, pr.Ordered, p.P50, p.P90, p.P99, pr.ReleasesAsked)
}

// THE FLOOR IS WHAT A BURST DOES NOT WAIT FOR. The pool grows one host at a time while
// one is on its way, so a burst arriving at a floor of none waits out a lead time for
// each host it needs; a floor of hosts already joined carries the same burst at once.
// One burst, two floors: the tail of the first is the cost of the second's idle hosts.
func TestAPoolsFloorIsWhatABurstDoesNotWait(t *testing.T) {
	t.Parallel()
	lead := 3 * time.Minute
	trace := Burst(7, Params{Jobs: 24, Tiers: []string{"billet-4vcpu"}})
	p99 := map[int]time.Duration{}
	for _, floor := range []int{0, 3} {
		report := Run(t, poolOnlyFleet(floor, 3, lead, 0), trace, Options{DirectAssignment: true})
		speaks(t, report)
		joinsAtItsLeadTime(t, report.Pool, lead)
		p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
		p99[floor] = p.P99
		t.Logf("burst of 24 four-vcpu jobs, pool floor %d of 3 16-vcpu hosts, lead %s: queue wait p50 %s "+
			"p90 %s p99 %s", floor, lead, p.P50, p.P90, p.P99)
	}
	// Three lead times stand between the burst and the floor of none's third host, and
	// the tail job waits on growth a floor of three already holds.
	if p99[0]-p99[3] < lead {
		t.Errorf("queue wait p99 is %s from a floor of none and %s from a floor of three; the growth of "+
			"three hosts one at a time should cost the first at least a lead time more", p99[0], p99[3])
	}
}

// A POOL CARRIES A WIDE-SHAPE MIX FROM ITS FLOOR. Jobs from 2 to 32 vcpu, a few of the
// widest, offered at the load four 64-vcpu hosts would carry, arrive at a pool that
// keeps one such host and may order three more: every job runs, the pool reaches its
// maximum, and once quiet it asks to release the three beyond its floor and no more.
func TestAPoolCarriesAWideShapeMixFromItsFloor(t *testing.T) {
	t.Parallel()
	fleet := Fleet{MaxVCPU: 1024, MaxMemory: 4096 * config.GiB}
	shaping := Fleet{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}
	for _, vcpu := range []int{2, 4, 8, 16, 32} {
		tier := TierShape{Label: fmt.Sprintf("billet-%dvcpu", vcpu), VCPU: vcpu,
			Memory: config.ByteSize(vcpu) * 4 * config.GiB, Arch: config.ArchX64}
		fleet.Tiers = append(fleet.Tiers, tier)
		shaping.Tiers = append(shaping.Tiers, tier)
	}
	for i := range 4 {
		shaping.Hosts = append(shaping.Hosts, Host{Name: fmt.Sprintf("shape-%d", i), VCPU: 64, Memory: 256 * config.GiB})
	}
	lead := 3 * time.Minute
	fleet.Pool = &PoolFleet{
		Archs:     map[config.Arch]PoolArch{config.ArchX64: {Floor: 1, Max: 4}},
		Host:      Host{VCPU: 64, Memory: 256 * config.GiB},
		LeadTime:  lead,
		Interval:  time.Minute,
		IdleAfter: 10 * time.Minute,
	}
	report := Run(t, fleet, WideShapeMix(4, WideParams{Params: Params{Jobs: 40}, Fleet: shaping}),
		Options{DirectAssignment: true})
	speaks(t, report)
	pr := report.Pool
	if got := pr.MostJoined[config.ArchX64]; got != 4 {
		t.Errorf("the pool joined at most %d hosts at once; the mix offers the load of its maximum, 4", got)
	}
	joinsAtItsLeadTime(t, pr, lead)
	if len(pr.ReleasesAsked) != 3 {
		t.Errorf("the pool asked to release %v once quiet; want the three beyond its floor of one, and no "+
			"more", pr.ReleasesAsked)
	}
	asksAfterIdle(t, report, 10*time.Minute, time.Minute)
	p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
	t.Logf("wide-shape mix on a pool of 1..4 64-vcpu hosts, lead %s: %d orders; queue wait p50 %s p90 %s "+
		"p99 %s; releases asked %v", lead, len(pr.Ordered), p.P50, p.P90, p.P99, pr.ReleasesAsked)
}

// FIVE HOSTS GOING QUIET TOGETHER ARE EACH ASKED TO LEAVE. Twenty hour-long jobs fill a
// pool of five 16-vcpu hosts and end within minutes of each other; the pool asks for one
// release a step, and the replay steps until the fifth is asked, not until a deadline
// that shortened with every release.
func TestFiveHostsGoingQuietTogetherAreEachAskedToLeave(t *testing.T) {
	t.Parallel()
	var trace Trace
	for i := range 20 {
		trace.Arrivals = append(trace.Arrivals, Arrival{
			At: DefaultStart.Add(time.Duration(i) * time.Second), Tier: "billet-4vcpu", Owner: DefaultOwner,
			Repository: "web", WorkflowRef: DefaultOwner + "/web/.github/workflows/ci.yml@refs/heads/main",
			RunID: int64(30000 + i), Duration: Duration(time.Hour), Result: ResultSucceeded,
		})
	}
	trace.Normalize()
	report := Run(t, poolOnlyFleet(0, 5, 3*time.Minute, 10*time.Minute), trace, Options{DirectAssignment: true})
	speaks(t, report)
	pr := report.Pool
	if got := pr.MostJoined[config.ArchX64]; got != 5 {
		t.Fatalf("the pool joined at most %d hosts; twenty four-vcpu jobs fill five", got)
	}
	if got := slices.Sorted(slices.Values(pr.ReleasesAsked)); !slices.Equal(got, []string{"r001", "r002", "r003", "r004", "r005"}) {
		t.Errorf("the pool asked to release %v; want each of its five hosts once", pr.ReleasesAsked)
	}
	asksAfterIdle(t, report, 10*time.Minute, time.Minute)
}

// A QUIET NIGHT IS NOT A STALL. An x64 job in the evening and an arm64 job eight hours
// later, each on a pool of floor none: the pool steps through the night, orders an arm64
// host for the morning's job, and both run.
func TestAPoolStepsThroughAQuietNight(t *testing.T) {
	t.Parallel()
	fleet := Fleet{
		Tiers: []TierShape{
			{Label: "billet-x64", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchX64},
			{Label: "billet-arm64", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchARM64},
		},
		MaxVCPU: 1024, MaxMemory: 4096 * config.GiB,
		Pool: &PoolFleet{
			Archs: map[config.Arch]PoolArch{
				config.ArchX64: {Floor: 0, Max: 1}, config.ArchARM64: {Floor: 0, Max: 1},
			},
			Host: Host{VCPU: 16, Memory: 64 * config.GiB}, LeadTime: 3 * time.Minute, Interval: 10 * time.Minute,
		},
	}
	at := func(tier string, offset time.Duration, run int64) Arrival {
		return Arrival{
			At: DefaultStart.Add(offset), Tier: tier, Owner: DefaultOwner, Repository: "web",
			WorkflowRef: DefaultOwner + "/web/.github/workflows/ci.yml@refs/heads/main", RunID: run,
			Duration: Duration(5 * time.Minute), Result: ResultSucceeded,
		}
	}
	trace := Trace{Arrivals: []Arrival{at("billet-x64", 0, 40000), at("billet-arm64", 8*time.Hour, 40001)}}
	trace.Normalize()
	report := Run(t, fleet, trace, Options{DirectAssignment: true})
	speaks(t, report)
	if got := report.Pool.MostJoined[config.ArchARM64]; got != 1 {
		t.Errorf("the pool joined %d arm64 hosts for the morning's job; want 1", got)
	}
}

// TWO ARCHITECTURES REPLAY THE SAME TWICE. The scaler orders on each architecture in
// one order every step, so the identifiers its orders draw, the node each host joins
// as and where every job ran are the same in two replays of one trace.
func TestAPoolOfTwoArchitecturesReplaysTheSameTwice(t *testing.T) {
	t.Parallel()
	fleet := Fleet{
		Tiers: []TierShape{
			{Label: "billet-x64", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchX64},
			{Label: "billet-arm64", VCPU: 4, Memory: 16 * config.GiB, Arch: config.ArchARM64},
		},
		MaxVCPU: 1024, MaxMemory: 4096 * config.GiB,
		Pool: &PoolFleet{
			Archs: map[config.Arch]PoolArch{
				config.ArchX64: {Floor: 1, Max: 2}, config.ArchARM64: {Floor: 1, Max: 2},
			},
			Host: Host{VCPU: 16, Memory: 64 * config.GiB}, LeadTime: 2 * time.Minute, Interval: time.Minute,
		},
	}
	trace := Burst(9, Params{Jobs: 24, Tiers: []string{"billet-x64", "billet-arm64"}})
	type run struct {
		orders []string
		ran    []string
	}
	replay := func() run {
		report := Run(t, fleet, trace, Options{DirectAssignment: true})
		speaks(t, report)
		var r run
		for _, o := range report.Pool.Ordered {
			r.orders = append(r.orders, o.Order+"="+string(o.Arch))
		}
		for _, rec := range report.Records {
			r.ran = append(r.ran, fmt.Sprintf("%d@%s", rec.Seq, rec.Node))
		}

		return r
	}
	first, second := replay(), replay()
	if !slices.Equal(first.orders, second.orders) || !slices.Equal(first.ran, second.ran) {
		t.Errorf("two replays of one trace differ:\norders %v\n   and %v\nran    %v\n   and %v",
			first.orders, second.orders, first.ran, second.ran)
	}
	if len(first.orders) < 2 || first.orders[0] != "r001=arm64" || first.orders[1] != "r002=x64" {
		t.Errorf("the floors were ordered as %v; want arm64's first, then x64's, in the scaler's order", first.orders)
	}
}
