package pool

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// shrinkRig is a rig whose pool shrinks itself after idleAfter, with joined hosts o-a and
// o-b idle since the given ages ago.
func shrinkRig(t *testing.T, policy Policy, idleAfter time.Duration, ages map[string]time.Duration) *rig {
	t.Helper()
	r := newRig(t, policy, firecrackerTier("fc"))
	r.pool.cfg.IdleAfter = idleAfter
	r.ledger.idleSince = map[string]time.Time{}
	for id, age := range ages {
		r.joined(id, "host-"+id, false)
		r.ledger.idleSince[id] = r.now.Add(-age)
	}
	r.report("fc", false, 0, "")

	return r
}

func released(r *rig) []string {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	var out []string
	for i := range r.ledger.orders {
		if !r.ledger.orders[i].ReleaseRequested.IsZero() {
			out = append(out, r.ledger.orders[i].ID)
		}
	}

	return out
}

// THE POOL SHRINKS BY DRAIN, ONE HOST A STEP, THE LONGEST IDLE FIRST, AND NEVER BELOW ITS
// FLOOR: a host idle past idle_after on an architecture above its floor, with no demand
// and nothing on its way, has its release asked for; the next step counts it leaving and
// reads the floor again.
func TestThePoolShrinksTheLongestIdleHostToItsFloor(t *testing.T) {
	t.Parallel()
	r := shrinkRig(t, Policy{Floor: 1, Max: 4}, 20*time.Minute,
		map[string]time.Duration{"o-a": 30 * time.Minute, "o-b": time.Hour, "o-c": 45 * time.Minute})

	step(t, r)
	if got := released(r); len(got) != 1 || got[0] != "o-b" {
		t.Fatalf("released %v after one step, want only the longest idle, o-b", got)
	}
	asked := r.ledger.idleReleases[0]
	if !asked.cutoff.Equal(r.now.Add(-20*time.Minute)) || !strings.Contains(asked.reason, "idle since") ||
		!strings.Contains(asked.reason, "idle_after 20m0s") {
		t.Fatalf("asked %+v, want the idle_after cutoff and a reason saying why", asked)
	}

	step(t, r)
	if got := released(r); len(got) != 2 || got[1] != "o-c" {
		t.Fatalf("released %v after two steps, want o-c next", got)
	}
	// AT THE FLOOR: one host serves, and it stays however idle it is.
	step(t, r)
	step(t, r)
	if got := released(r); len(got) != 2 {
		t.Fatalf("released %v, below the floor of 1", got)
	}
}

// NOTHING SHRINKS that the rule does not allow: a pool told no idle_after, a host idle
// less than idle_after, demand on its architecture, demand that could not be read, and an
// order on its way or unknown each keep every host.
func TestThePoolKeepsItsHostsWhenItMayNotShrink(t *testing.T) {
	t.Parallel()
	ages := map[string]time.Duration{"o-a": time.Hour, "o-b": time.Hour}
	for name, setup := range map[string]func(r *rig){
		"no idle_after":       func(r *rig) { r.pool.cfg.IdleAfter = 0 },
		"not idle long":       func(r *rig) { r.pool.cfg.IdleAfter = 2 * time.Hour },
		"demand":              func(r *rig) { r.report("fc", true, 0, "") },
		"demand unreadable":   func(r *rig) { r.report("fc", false, time.Hour, "") },
		"a demand read error": func(r *rig) { r.report("fc", false, 0, "the listener could not say") },
		"an order on its way": func(r *rig) {
			r.ledger.mu.Lock()
			defer r.ledger.mu.Unlock()
			r.ledger.orders = append(r.ledger.orders, alloc.PoolOrder{ID: "o-new", Source: "first",
				Scope: "first:us-west-2", Shape: "z1d.metal", Arch: "x64", State: alloc.PoolPlaced,
				HostID: "host-new", FirstAttempt: r.now, Updated: r.now})
		},
		"an order unknown": func(r *rig) {
			r.ledger.mu.Lock()
			defer r.ledger.mu.Unlock()
			r.ledger.orders = append(r.ledger.orders, alloc.PoolOrder{ID: "o-unk", Source: "first",
				Scope: "first:us-west-2", Shape: "z1d.metal", Arch: "x64", State: alloc.PoolUncertain,
				FirstAttempt: r.now, Updated: r.now})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := shrinkRig(t, Policy{Floor: 0, Max: 4}, 20*time.Minute, ages)
			setup(r)
			// DEMAND THAT COULD NOT BE READ IS THE STEP'S ERROR, and the only one here.
			unread := name == "demand unreadable" || name == "a demand read error"
			if err := r.pool.Step(t.Context()); errors.Is(err, ErrDemandUnknown) != unread {
				t.Fatalf("step answered %v; want demand unknown: %t", err, unread)
			}
			if got := released(r); len(got) != 0 {
				t.Fatalf("released %v", got)
			}
			// NOT EVEN ASKED: the scaler's own rule keeps them, not the ledger's refusal.
			if asked := r.ledger.idleReleases; len(asked) != 0 {
				t.Fatalf("asked for %+v", asked)
			}
		})
	}
}

// A HOST THAT TOOK WORK BETWEEN THE READING AND THE ASKING stays, and the step goes on:
// the release's own transaction reads it again and refuses.
func TestAHostThatTookWorkFirstStays(t *testing.T) {
	t.Parallel()
	r := shrinkRig(t, Policy{Floor: 0, Max: 4}, 20*time.Minute, map[string]time.Duration{"o-a": time.Hour})
	r.ledger.tookWork = map[string]bool{"o-a": true}
	step(t, r)
	if got := released(r); len(got) != 0 || len(r.ledger.idleReleases) != 1 {
		t.Fatalf("released %v after asking %d times; want it asked once and kept", got, len(r.ledger.idleReleases))
	}
}
