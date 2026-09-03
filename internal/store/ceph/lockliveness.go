package ceph

import (
	"fmt"
	"time"
)

// heartbeat is one observation of a lock holder's liveness counter.
//
// A COUNTER RATHER THAN A TIMESTAMP, AND THAT IS THE WHOLE POINT. Deciding a
// holder is dead because its cookie passed an age bound requires the observer and
// the holder to agree about the time, and they do not have to: a clock running
// ahead makes a lock taken seconds ago look ancient. A counter that MOVED is proof
// of life that needs no agreement about anything.
type heartbeat struct {
	counter uint64
	present bool
}

// breakable reports whether a held lock may be taken from its holder, given two
// observations of its heartbeat separated by an observation window.
//
// TWO INDEPENDENT CONDITIONS, BOTH REQUIRED. The holder must be silent across the
// window, AND the lock must be older than the bound. Either alone is wrong:
//
//   - Silence alone is not death. A holder between beats is silent, and breaking
//     on that would put two writers on one image for the sake of a few seconds.
//   - Age alone is not death either, which is the bug this replaces. A publish can
//     genuinely run longer than its chosen bound and an observer's clock can
//     simply be ahead. Both produce an old-but-live holder, and breaking its lock
//     is the exact corruption the lock exists to prevent.
func breakable(
	before, after heartbeat,
	cookieAge, staleAfter time.Duration,
) (bool, string) {
	// A COUNTER THAT MOVED IS LIVENESS, at any age, under any clocks.
	if before.present && after.present && after.counter > before.counter {
		return false, fmt.Sprintf("its heartbeat counter moved from %d to %d while this was "+
			"watching, so the holder is alive", before.counter, after.counter)
	}

	// BACKWARDS MEANS RELEASED AND RETAKEN while this was deciding. The lock on
	// offer now belongs to somebody else, and breaking it breaks the new holder.
	if before.present && after.present && after.counter < before.counter {
		return false, fmt.Sprintf("its heartbeat counter went backwards, from %d to %d, so "+
			"the lock was released and retaken while this was deciding",
			before.counter, after.counter)
	}

	// A HEARTBEAT THAT APPEARED is a holder that has just started counting -- a
	// newer billet that took over, or one whose first beat landed late.
	if !before.present && after.present {
		return false, "its holder started heartbeating while this was watching"
	}

	// SILENT, OR NEVER COUNTING AT ALL. The build script predates the heartbeat and
	// writes no counter, so its locks reach here with nothing observed -- and they
	// still have to be reclaimable, or a leak from that side becomes permanent.
	// Age is the only evidence left, and it is why the bound is set far past any
	// real run rather than tuned close to one.
	if cookieAge < staleAfter {
		return false, fmt.Sprintf("it is only %s old", cookieAge.Round(time.Second))
	}

	return true, fmt.Sprintf("its heartbeat counter did not move while this was watching and "+
		"it has been held for %s, longer than the %s bound",
		cookieAge.Round(time.Second), staleAfter)
}
