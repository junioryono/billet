package nodeplane

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// NOTHING ASKED, SO NOTHING NOTICED.
//
// Expiry ran only where its answer was needed — picking a node, listing the
// fleet, broadcasting a destroy — which was enough while that was all it
// decided. It is not enough once liveness decides what a tier ADVERTISES: an
// idle deployment does none of those three things, so a host that crashed on a
// quiet afternoon kept its capacity advertised until somebody happened to launch
// something, and GitHub went on assigning against a machine that was gone.
//
// The assertion is deliberately NOT p.Nodes(): that method expires as a side
// effect, so a test asking it would pass against a plane with no timer at all —
// the question would be answering itself.
func TestASilentNodeIsNoticedWithNobodyAsking(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	// A short poll window, so staleAfter and therefore the tick are short.
	p := testPlane(t, WithClock(clock.now), WithPollTimeout(20*time.Millisecond))
	register(t, p, "n1", config.ProviderDocker)

	go p.Watch(t.Context())

	if p.countForTest() != 1 {
		t.Fatalf("the node did not register: %d", p.countForTest())
	}

	clock.advancePastSilence()

	deadline := time.Now().Add(5 * time.Second)
	for p.countForTest() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a silent node was never expired, so its capacity stayed advertised while " +
				"nothing was asking about the fleet")
		}

		time.Sleep(time.Millisecond)
	}
}

// countForTest reports how many nodes the plane holds, WITHOUT expiring any.
func (p *Plane) countForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.nodes)
}
