package alloc

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// The allocator's hot paths, against the ledger the run exercises: SQLite by
// default, PostgreSQL under BILLET_TEST_LEDGER=postgres. These are what the
// heartbeat-batching decision in #356 is made on; `go test -run '^$' -bench .
// ./internal/alloc/` runs them, and benchstat compares two runs.

// benchLimits is a ceiling no benchmark reaches, so the measurement is the
// work rather than a refusal.
var benchLimits = Limits{MaxVCPU: 1 << 20, MaxMemory: 1 << 20 * config.GiB}

// benchFleet is an allocator over hosts registered in their own right, each
// with room for 32 two-vCPU jobs, and tiers firecracker places on any of them.
func benchFleet(b *testing.B, hosts, tiers int) (*Allocator, []config.Tier) {
	b.Helper()

	ts := make([]config.Tier, tiers)
	for i := range ts {
		ts[i] = tier(fmt.Sprintf("bench-%d", i), 2, 4*config.GiB)
	}

	a := newBareAllocator(b, benchLimits, ts)

	for i := range hosts {
		reg := testRegistration(fmt.Sprintf("bench-host-%03d", i), config.ProviderFirecracker)
		reg.VCPU, reg.Memory = 64, 128*config.GiB

		if _, err := a.RegisterNode(b.Context(), reg); err != nil {
			b.Fatalf("register host %d: %v", i, err)
		}
	}

	return a, ts
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
// ledger's writer slot, which every renewal of every lease takes once a third
// of the lease TTL.
func BenchmarkHeartbeat(b *testing.B) {
	a, tiers := benchFleet(b, 16, 1)
	leases := escrowN(b, a, tiers[0].Label, 500)

	i := 0

	for b.Loop() {
		lease := leases[i%len(leases)]
		i++

		if err := a.Heartbeat(b.Context(), lease.ID, lease.Epoch); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHeartbeatContended is renewals from many goroutines at once, which is
// what a fleet's listeners and nodes do: they queue for the one writer slot,
// so this is the slot's throughput rather than one renewal's latency.
func BenchmarkHeartbeatContended(b *testing.B) {
	a, tiers := benchFleet(b, 16, 1)
	leases := escrowN(b, a, tiers[0].Label, 500)

	var next atomic.Int64

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease := leases[int(next.Add(1))%len(leases)]

			if err := a.Heartbeat(b.Context(), lease.ID, lease.Epoch); err != nil {
				b.Error(err)

				return
			}
		}
	})
}

// BenchmarkEscrow is buying one lease and handing it back: the headroom check,
// placement and the insert in one transaction, then the release in another.
func BenchmarkEscrow(b *testing.B) {
	a, tiers := benchFleet(b, 16, 4)

	// A FLEET WITH WORK ON IT, so placement weighs real usage.
	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 50)
	}

	for b.Loop() {
		leases, err := a.Escrow(b.Context(), tiers[0].Label, 1)
		if err != nil || len(leases) != 1 {
			b.Fatalf("Escrow = %d leases, %v", len(leases), err)
		}

		if err := a.Release(b.Context(), leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHeadroom is how many more of a tier the allocator would grant now, a
// read through the reader pool that runs the placer over the whole fleet.
func BenchmarkHeadroom(b *testing.B) {
	a, tiers := benchFleet(b, 16, 4)

	for i := range tiers {
		escrowN(b, a, tiers[i].Label, 50)
	}

	for b.Loop() {
		if _, err := a.Headroom(b.Context(), tiers[0].Label); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPlacement is the same purchase as BenchmarkEscrow over fleets of
// growing size, so what placement costs per host is the difference between
// them.
func BenchmarkPlacement(b *testing.B) {
	for _, hosts := range []int{4, 32, 128} {
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			a, tiers := benchFleet(b, hosts, 4)

			for i := range tiers {
				escrowN(b, a, tiers[i].Label, 2*hosts)
			}

			for b.Loop() {
				leases, err := a.Escrow(b.Context(), tiers[0].Label, 1)
				if err != nil || len(leases) != 1 {
					b.Fatalf("Escrow = %d leases, %v", len(leases), err)
				}

				if err := a.Release(b.Context(), leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
