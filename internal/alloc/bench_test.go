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
// heartbeat-batching decision in #356 is made on; `make bench` runs them, and
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

	obs := &slotObserver{}
	db.Observe(state.Observer(obs))

	return a, ts, obs
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

// BenchmarkHeartbeat is one lease renewal: one write transaction holding the
// ledger's writer slot.
func BenchmarkHeartbeat(b *testing.B) {
	a, tiers, obs := benchFleet(b, 16, 1)
	leases := escrowN(b, a, tiers[0].Label, 500)

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
}

// BenchmarkHeartbeatContended is renewals from many goroutines at once, which is
// what a fleet's listeners and nodes do: they queue for the one writer slot,
// so ns/op here is the slot's throughput rather than one renewal's latency.
func BenchmarkHeartbeatContended(b *testing.B) {
	a, tiers, obs := benchFleet(b, 16, 1)
	leases := escrowN(b, a, tiers[0].Label, 500)

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

// BenchmarkPlacement is the same purchase over fleets of growing size, with
// two leases a host of each tier already placed, so how placement grows with
// the fleet is the difference between them; slot-µs/write is how long the
// purchase held the writer slot.
func BenchmarkPlacement(b *testing.B) {
	for _, hosts := range []int{4, 32, 128} {
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
