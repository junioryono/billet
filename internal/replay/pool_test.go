package replay

import (
	"fmt"
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

// A POOL GROWS FROM ITS FLOOR FOR A BURST, AND ASKS TO SHRINK ONCE IT IS QUIET. The real
// scaler, allocator and listeners, over a played source: a burst on a tier no fixed
// host serves is demand, the pool orders one host at a time while one is on its way, up
// to its maximum, every job runs on a host it ordered, and once nothing has run for
// idle_after it asks for a host's release.
func TestAPoolGrowsFromItsFloorForABurstAndAsksToShrink(t *testing.T) {
	t.Parallel()
	fleet := poolOnlyFleet(0, 3, 3*time.Minute, 10*time.Minute)
	trace := Burst(7, Params{Jobs: 24, Tiers: []string{"billet-4vcpu"}})
	report := Run(t, fleet, trace, Options{DirectAssignment: true})

	if len(report.Missing) > 0 || len(report.Unstarted) > 0 {
		t.Fatalf("jobs missing %v and never started %v", report.Missing, report.Unstarted)
	}
	pr := report.Pool
	if pr == nil {
		t.Fatal("the report says nothing of the pool")
	}
	if got := pr.MostJoined[config.ArchX64]; got != 3 {
		t.Errorf("the pool joined at most %d x64 hosts at once; a burst of 24 four-vcpu jobs on "+
			"16-vcpu hosts wants its maximum, 3", got)
	}
	for i, o := range pr.Ordered {
		if i > 0 && o.Ordered.Before(pr.Ordered[i-1].Joined) {
			t.Errorf("order %s was placed at %s, before order %s joined at %s: the pool grows one host "+
				"at a time while one is on its way", o.Order, o.Ordered, pr.Ordered[i-1].Order, pr.Ordered[i-1].Joined)
		}
	}
	if len(pr.ReleasesAsked) == 0 {
		t.Error("the pool never asked to release a host once the burst was over and idle_after had passed")
	}
	p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
	t.Logf("pool of 0..3, lead 3m: %d orders; queue wait p50 %s p90 %s p99 %s max %s; releases asked %v",
		len(pr.Ordered), p.P50, p.P90, p.P99, p.Max, pr.ReleasesAsked)
}

// A POOL CARRIES A WIDE-SHAPE MIX FROM ITS FLOOR. Jobs from 2 to 32 vcpu, a few of the
// widest, offered at the load four 64-vcpu hosts would carry, arrive at a pool that
// keeps one such host and may order three more: every job runs, on no more hosts than
// the maximum.
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
	fleet.Pool = &PoolFleet{
		Archs:     map[config.Arch]PoolArch{config.ArchX64: {Floor: 1, Max: 4}},
		Host:      Host{VCPU: 64, Memory: 256 * config.GiB},
		LeadTime:  3 * time.Minute,
		Interval:  time.Minute,
		IdleAfter: 10 * time.Minute,
	}
	trace := WideShapeMix(4, WideParams{Params: Params{Jobs: 40}, Fleet: shaping})
	report := Run(t, fleet, trace, Options{DirectAssignment: true})

	if len(report.Missing) > 0 || len(report.Unstarted) > 0 {
		t.Fatalf("jobs missing %v and never started %v", report.Missing, report.Unstarted)
	}
	pr := report.Pool
	if got := pr.MostJoined[config.ArchX64]; got < 1 || got > 4 {
		t.Errorf("the pool joined at most %d hosts at once; want between its floor, 1, and its maximum, 4", got)
	}
	p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
	t.Logf("wide-shape mix on a pool of 1..4 64-vcpu hosts, lead 3m: %d orders, most joined %d; queue wait "+
		"p50 %s p90 %s p99 %s max %s; releases asked %v",
		len(pr.Ordered), pr.MostJoined[config.ArchX64], p.P50, p.P90, p.P99, p.Max, pr.ReleasesAsked)
}

// THE FLOOR IS WHAT A BURST DOES NOT WAIT FOR. The pool grows one host at a time while
// one is on its way, so a burst arriving at a floor of none waits out a lead time for
// each host it needs; a floor of hosts already joined carries the same burst at once.
// One burst, two floors: the tail of the first is the cost of the second's idle hosts.
func TestAPoolsFloorIsWhatABurstDoesNotWait(t *testing.T) {
	t.Parallel()
	trace := Burst(7, Params{Jobs: 24, Tiers: []string{"billet-4vcpu"}})
	p99 := map[int]time.Duration{}
	for _, floor := range []int{0, 3} {
		report := Run(t, poolOnlyFleet(floor, 3, 3*time.Minute, 0), trace, Options{DirectAssignment: true})
		if len(report.Missing) > 0 || len(report.Unstarted) > 0 {
			t.Fatalf("floor %d: jobs missing %v and never started %v", floor, report.Missing, report.Unstarted)
		}
		p := report.QueueWaitBy(func(*Record) string { return "all" })["all"]
		p99[floor] = p.P99
		t.Logf("burst of 24 four-vcpu jobs, pool floor %d of 3 16-vcpu hosts, lead 3m: queue wait p50 %s "+
			"p90 %s p99 %s", floor, p.P50, p.P90, p.P99)
	}
	// Three lead times stand between the burst and the floor of none's third host.
	if p99[0]-p99[3] < 2*3*time.Minute {
		t.Errorf("queue wait p99 is %s from a floor of none and %s from a floor of three; the growth of "+
			"three hosts one at a time should cost the first at least two lead times more", p99[0], p99[3])
	}
}
