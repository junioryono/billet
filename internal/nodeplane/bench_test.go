package nodeplane

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// BenchmarkPlaneDispatch is one command to every host and every answer back:
// queued by the plane, taken by each host's long poll, and reported, which is
// the round trip every launch, destroy and sweep makes. Over one host and over
// eight, which answer in parallel.
func BenchmarkPlaneDispatch(b *testing.B) {
	for _, hosts := range []int{1, 8} {
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			db, err := state.Open(b.Context(), ledgertest.Dir(b))
			if err != nil {
				b.Fatal(err)
			}

			b.Cleanup(func() { _ = db.Close() })

			tier := testTier()

			a, err := alloc.New(db, alloc.Limits{MaxVCPU: 1 << 10, MaxMemory: 1 << 10 * config.GiB},
				[]config.Tier{tier})
			if err != nil {
				b.Fatal(err)
			}

			p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
				WithRegistrar(a), WithTierCatalog([]config.Tier{tier}))

			// JOINED BEFORE THE LEDGER CLOSES: the benchmark's context ends just
			// before its cleanups run, which wakes every host's long poll.
			var answering sync.WaitGroup

			b.Cleanup(answering.Wait)

			for i := range hosts {
				name := fmt.Sprintf("n%d", i)
				incarnation := name + "-1"

				if _, err := p.Register(b.Context(), nodeapi.RegisterRequest{
					Version: nodeapi.Version, Node: name, Provider: config.ProviderDocker,
					Deployment: deployment, Incarnation: incarnation, VCPU: 8, Memory: 32 * config.GiB,
				}); err != nil {
					b.Fatalf("register %s: %v", name, err)
				}

				answering.Go(func() { answerEvery(b, p, name, incarnation) })
			}

			runner := p.NewRunner()

			for b.Loop() {
				if err := runner.Tend(b.Context()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// answerEvery is a host that answers every command OK until the benchmark ends.
func answerEvery(b *testing.B, p *Plane, name, incarnation string) {
	b.Helper()

	for {
		cmd, took, err := p.Poll(b.Context(), name, incarnation)

		switch {
		case b.Context().Err() != nil:
			return
		case err != nil:
			b.Errorf("%s could not poll: %v", name, err)

			return
		case !took:
			continue
		}

		if err := p.Result(name, incarnation, nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			b.Errorf("%s could not report: %v", name, err)

			return
		}
	}
}
