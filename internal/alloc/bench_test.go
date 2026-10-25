package alloc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// The allocator's hot paths, against the ledger the run exercises: SQLite by
// default, PostgreSQL under BILLET_TEST_LEDGER=postgres. These are what the
// heartbeat-batching decision in #356 is made on; `scripts/gates/bench.sh` runs them, and
// benchstat compares two runs.

// benchLimits is a ceiling no benchmark reaches, so the measurement is the
// work rather than a refusal.
var benchLimits = Limits{MaxVCPU: 1 << 20, MaxMemory: 1 << 20 * config.GiB}

// benchDeployment is the deployment the benchmark's controller claims.
const benchDeployment = "0123456789abcdef0123456789abcdef"

// slotObserver adds up how long writes held the writer slot while measuring.
type slotObserver struct {
	measuring atomic.Bool
	mu        sync.Mutex
	held      time.Duration
	writes    int
}

func (o *slotObserver) WriteWaited(time.Duration) {}
func (o *slotObserver) WriteRetried()             {}

func (o *slotObserver) WriteHeld(d time.Duration, _ bool) {
	if !o.measuring.Load() {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	o.held += d
	o.writes++
}

// report adds the slot's hold per measured write to the benchmark's results.
func (o *slotObserver) report(b *testing.B) {
	b.Helper()

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.writes > 0 {
		b.ReportMetric(float64(o.held.Microseconds())/float64(o.writes), "slot-µs/write")
	}
}

// benchFleet is an allocator over hosts registered in their own right, each
// with room for 32 two-vCPU jobs, and tiers firecracker places on any of them.
//
// THE LEDGER HOLDS THE CONTROLLER CLAIM, as a control plane's does, so every
// write reads the claim's epoch inside its transaction the way production's
// writes do; an unclaimed handle skips that read.
func benchFleet(b *testing.B, hosts, tiers int) (*Allocator, []config.Tier, *slotObserver) {
	b.Helper()

	ts := make([]config.Tier, tiers)
	for i := range ts {
		ts[i] = tier(fmt.Sprintf("bench-%d", i), 2, 4*config.GiB)
	}

	return benchFleetOf(b, hosts, ts, nil)
}

// benchFleetOf is benchFleet over the tiers given, with extra registered after
// the hosts: a remote node a tier spanning backends falls back to.
func benchFleetOf(
	b *testing.B, hosts int, ts []config.Tier, extra []NodeRegistration,
) (*Allocator, []config.Tier, *slotObserver) {
	b.Helper()

	db := openTestLedger(b)

	if _, err := db.ClaimController(b.Context(), "bench", benchDeployment); err != nil {
		b.Fatalf("claim the ledger: %v", err)
	}

	a, err := New(db, benchLimits, ts)
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	for i := range hosts {
		reg := testRegistration(fmt.Sprintf("bench-host-%03d", i), config.ProviderFirecracker)
		reg.VCPU, reg.Memory = 64, 128*config.GiB

		if _, err := a.RegisterNode(b.Context(), reg); err != nil {
			b.Fatalf("register host %d: %v", i, err)
		}
	}

	for i := range extra {
		if _, err := a.RegisterNode(b.Context(), extra[i]); err != nil {
			b.Fatalf("register %s: %v", extra[i].Name, err)
		}
	}

	obs := &slotObserver{}
	db.Observe(state.Observer(obs))

	return a, ts, obs
}

// The target fleet of ADR-021: 256 hosts holding 4,096 open leases, sixteen a
// host, across four tiers.
const (
	targetHosts         = 256
	targetLeasesPerHost = 16
	targetTiers         = 4
)

// targetTierSet is the target's four tiers.
func targetTierSet() []config.Tier {
	ts := make([]config.Tier, targetTiers)
	for i := range ts {
		ts[i] = tier(fmt.Sprintf("bench-%d", i), 2, 4*config.GiB)
	}

	return ts
}

// targetFleet is benchFleetOf at the target, every tier holding its share of
// the target's open leases, which it returns.
func targetFleet(
	b *testing.B, ts []config.Tier, extra []NodeRegistration,
) (*Allocator, []config.Tier, []*Lease, *slotObserver) {
	b.Helper()

	a, ts, obs := benchFleetOf(b, targetHosts, ts, extra)

	var leases []*Lease
	for i := range ts {
		leases = append(leases, escrowN(b, a, ts[i].Label, targetHosts*targetLeasesPerHost/len(ts))...)
	}

	return a, ts, leases, obs
}

// escrowN holds n leases of tier.
func escrowN(b *testing.B, a *Allocator, tier string, n int) []*Lease {
	b.Helper()

	leases, err := a.Escrow(b.Context(), tier, n)
	if err != nil || len(leases) != n {
		b.Fatalf("Escrow(%s, %d) = %d leases, %v", tier, n, len(leases), err)
	}

	return leases
}

// heartbeatSizes are the fleets the renewal benchmarks run over: the
// 2026-10-08 record's sixteen hosts and 500 leases, and the target's.
var heartbeatSizes = []struct {
	name string
	open func(b *testing.B) (*Allocator, []*Lease, *slotObserver)
}{
	{"leases=500", func(b *testing.B) (*Allocator, []*Lease, *slotObserver) {
		b.Helper()

		a, tiers, obs := benchFleet(b, 16, 1)

		return a, escrowN(b, a, tiers[0].Label, 500), obs
	}},
	{"leases=4096", func(b *testing.B) (*Allocator, []*Lease, *slotObserver) {
		b.Helper()

		a, _, leases, obs := targetFleet(b, targetTierSet(), nil)

		return a, leases, obs
	}},
}

// BenchmarkHeartbeat is one lease renewal: one write transaction holding the
// ledger's writer slot.
func BenchmarkHeartbeat(b *testing.B) {
	for _, size := range heartbeatSizes {
		b.Run(size.name, func(b *testing.B) {
			a, leases, obs := size.open(b)

			obs.measuring.Store(true)

			i := 0

			for b.Loop() {
				lease := leases[i%len(leases)]
				i++

				if err := a.Heartbeat(b.Context(), lease.ID, lease.Epoch); err != nil {
					b.Fatal(err)
				}
			}

			obs.report(b)
		})
	}
}

// BenchmarkHeartbeatContended is renewals from many goroutines at once, which is
// what a fleet's listeners and nodes do: they queue for the one writer slot,
// so ns/op here is the slot's throughput rather than one renewal's latency.
func BenchmarkHeartbeatContended(b *testing.B) {
	for _, size := range heartbeatSizes {
		b.Run(size.name, func(b *testing.B) {
			a, leases, obs := size.open(b)

			var next atomic.Int64

			obs.measuring.Store(true)

			// RunParallel does not reset the timer itself, and the setup above is
			// repeated each time the benchmark is calibrated.
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					lease := leases[int(next.Add(1))%len(leases)]

					if err := a.Heartbeat(b.Context(), lease.ID, lease.Epoch); err != nil {
						b.Error(err)

						return
					}
				}
			})

			obs.report(b)
		})
	}
}

// BenchmarkHeartbeatMany is a listener's pass over the target's 4,096 leases,
// renewed heartbeatBatch to a transaction. ns/op is the whole pass, and
// slot-µs/lease what each lease's renewal held the writer slot for.
func BenchmarkHeartbeatMany(b *testing.B) {
	a, _, leases, obs := targetFleet(b, targetTierSet(), nil)

	refs := make([]LeaseRef, len(leases))
	for i, l := range leases {
		refs[i] = LeaseRef{ID: l.ID, Epoch: l.Epoch}
	}

	obs.measuring.Store(true)

	passes := 0

	for b.Loop() {
		for i, err := range a.HeartbeatMany(b.Context(), refs) {
			if err != nil {
				b.Fatalf("lease %d: %v", i, err)
			}
		}

		passes++
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()

	if passes > 0 {
		b.ReportMetric(float64(obs.held.Microseconds())/float64(passes*len(refs)), "slot-µs/lease")
	}
}

// purchase buys one lease of tier, timed, and hands it back, not timed: what
// is measured is the purchase alone, the headroom check, placement and the
// insert in one transaction.
func purchase(b *testing.B, a *Allocator, tier string, obs *slotObserver) {
	b.Helper()

	obs.measuring.Store(true)

	leases, err := a.Escrow(b.Context(), tier, 1)
	if err != nil || len(leases) != 1 {
		b.Fatalf("Escrow = %d leases, %v", len(leases), err)
	}

	obs.measuring.Store(false)
	b.StopTimer()

	if err := a.Release(b.Context(), leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
		b.Fatal(err)
	}

	b.StartTimer()
}

// BenchmarkEscrow is buying one lease on a fleet with work on it.
func BenchmarkEscrow(b *testing.B) {
	a, tiers, obs := benchFleet(b, 16, 4)

	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 50)
	}

	for b.Loop() {
		purchase(b, a, tiers[0].Label, obs)
	}

	obs.report(b)
}

// BenchmarkHeadroom is how many more of a tier the allocator would grant now, a
// read through the reader pool that runs the placer over the whole fleet.
func BenchmarkHeadroom(b *testing.B) {
	a, tiers, _ := benchFleet(b, 16, 4)

	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 50)
	}

	for b.Loop() {
		if _, err := a.Headroom(b.Context(), tiers[0].Label); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAdmissionView is what every listener reads under `fair` at each
// reconciliation: every tier's admission, in one read over the reader pool,
// for the nine tiers of the reference deployment.
func BenchmarkAdmissionView(b *testing.B) {
	a, tiers, _ := benchFleet(b, 16, 9)

	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 20)
	}

	for b.Loop() {
		if _, err := a.AdmissionView(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAdmissionFor is what a listener reads at each reconciliation and
// refusal: its own tier's admission alone, on the same deployment.
func BenchmarkAdmissionFor(b *testing.B) {
	a, tiers, _ := benchFleet(b, 16, 9)

	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 20)
	}

	for b.Loop() {
		if _, err := a.AdmissionFor(b.Context(), tiers[4].Label); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlacement is the same purchase over fleets of growing size, with
// two leases a host of each tier already placed, so how placement grows with
// the fleet is the difference between them; slot-µs/write is how long the
// purchase held the writer slot.
func BenchmarkPlacement(b *testing.B) {
	for _, hosts := range []int{4, 32, 128, targetHosts} {
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			a, tiers, obs := benchFleet(b, hosts, 4)

			for i := range tiers {
				escrowN(b, a, tiers[i].Label, 2*hosts)
			}

			for b.Loop() {
				purchase(b, a, tiers[0].Label, obs)
			}

			obs.report(b)
		})
	}
}

// BenchmarkPlacementTarget is one purchase on the target fleet: 256 hosts and
// 4,096 open leases (ADR-021).
func BenchmarkPlacementTarget(b *testing.B) {
	a, tiers, _, obs := targetFleet(b, targetTierSet(), nil)

	for b.Loop() {
		purchase(b, a, tiers[0].Label, obs)
	}

	obs.report(b)
}

// BenchmarkPlacementMixed is the target purchase for a tier that falls back
// from the fleet's hosts to the cloud, [firecracker, ec2], with one ec2 node
// whose cheapest fitting shape costs more than the tier asks: its candidates
// charge differently, which is the count that is not one pass.
func BenchmarkPlacementMixed(b *testing.B) {
	ts := targetTierSet()
	ts[0].Provider = ""
	ts[0].Providers = []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}

	cloud := NodeRegistration{
		Name: "bench-cloud", Provider: config.ProviderEC2,
		VCPU: 512, Memory: 1024 * config.GiB,
		WireMin: 12, WireMax: CacheAuthorityWireVersion, WireVersion: CacheAuthorityWireVersion,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	}

	a, tiers, _, obs := targetFleet(b, ts, []NodeRegistration{cloud})

	for b.Loop() {
		purchase(b, a, tiers[0].Label, obs)
	}

	obs.report(b)
}

// BenchmarkPlacementFloors is the target purchase while two other tiers each
// have 32 instances of their reserved floor outstanding, held on the hosts
// before the purchase is placed.
func BenchmarkPlacementFloors(b *testing.B) {
	ts := targetTierSet()
	held := targetHosts * targetLeasesPerHost / len(ts)
	ts[2].Reserved = held + 32
	ts[3].Reserved = held + 32

	a, tiers, _, obs := targetFleet(b, ts, nil)

	for b.Loop() {
		purchase(b, a, tiers[0].Label, obs)
	}

	obs.report(b)
}

// BenchmarkLeaseLife is the writes a job makes after its purchase, on the
// target fleet: assigned, bound, launching, online, busy and released, each its
// own transaction. slot-µs/job is what one job's life holds the writer slot for
// beside its purchase, which is neither timed nor counted.
func BenchmarkLeaseLife(b *testing.B) {
	a, tiers, _, obs := targetFleet(b, targetTierSet(), nil)
	ctx := b.Context()
	jobs := 0

	for b.Loop() {
		b.StopTimer()

		leases, err := a.Escrow(ctx, tiers[0].Label, 1)
		if err != nil || len(leases) != 1 {
			b.Fatalf("Escrow = %d leases, %v", len(leases), err)
		}

		l := leases[0]

		b.StartTimer()
		obs.measuring.Store(true)

		if err := a.Assign(ctx, l.ID, l.Epoch, int64(jobs+1), int64(jobs+1)); err != nil {
			b.Fatalf("Assign: %v", err)
		}
		if err := a.Bind(ctx, l.ID, l.Epoch, l.TargetNode); err != nil {
			b.Fatalf("Bind: %v", err)
		}
		for _, to := range []Phase{PhaseLaunching, PhaseOnline, PhaseBusy} {
			if err := a.Advance(ctx, l.ID, l.Epoch, to); err != nil {
				b.Fatalf("Advance(%s): %v", to, err)
			}
		}
		if err := a.Release(ctx, l.ID, l.Epoch, PhaseDone); err != nil {
			b.Fatalf("Release: %v", err)
		}

		obs.measuring.Store(false)

		jobs++
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()

	if jobs > 0 {
		b.ReportMetric(float64(obs.held.Microseconds())/float64(jobs), "slot-µs/job")
	}
}
