package ceph

import (
	"strings"
	"testing"
	"time"
)

// A COUNTER THAT MOVED PROVES THE HOLDER IS ALIVE, and proves it WITHOUT ANY
// AGREEMENT ABOUT THE TIME. That is the whole reason it is a counter and not a
// timestamp: the previous rule -- "the cookie says six hours, so the holder is
// dead" -- is wrong whenever the observer's clock runs ahead, and wrong again
// whenever a publish genuinely takes longer than the bound. The raw write is
// deliberately unbounded and imports up to a terabyte, so that is not
// hypothetical.
func TestBreakableRefusesWhileTheHolderIsStillCounting(t *testing.T) {
	before := heartbeat{counter: 7, present: true}
	after := heartbeat{counter: 8, present: true}

	// Even at an age far past the bound: a holder that is still counting is alive,
	// and an old-but-live publisher is exactly the case that must not be broken.
	ok, why := breakable(before, after, 100*StaleLockAfter)
	if ok {
		t.Fatalf("a lock whose holder is still counting was declared breakable (%q); "+
			"breaking it puts two writers on one image", why)
	}
}

// A COUNTER THAT DID NOT MOVE IS NOT ITSELF PROOF OF DEATH -- the holder may simply
// be between beats -- so the age bound still has to be met as well.
func TestBreakableRequiresBothSilenceAndAge(t *testing.T) {
	silent := heartbeat{counter: 7, present: true}

	if ok, _ := breakable(silent, silent, StaleLockAfter-time.Minute); ok {
		t.Error("a silent but recent lock was declared breakable")
	}

	ok, why := breakable(silent, silent, StaleLockAfter+time.Minute)
	if !ok {
		t.Error("a lock that is both silent and past the bound was not breakable, so a " +
			"leaked lock could never be reclaimed and every publisher refuses forever")
	}

	if !strings.Contains(why, "counter") && !strings.Contains(why, "heartbeat") {
		t.Errorf("the reason does not mention the liveness evidence: %q", why)
	}
}

// A HOLDER THAT NEVER HEARTBEATS AT ALL is the build script, which predates this
// and writes no counter. Its locks must still be reclaimable, or a leak from the
// shell side becomes permanent -- which is precisely the failure that has already
// happened once.
func TestBreakableFallsBackToAgeForAHolderThatNeverCounts(t *testing.T) {
	none := heartbeat{present: false}

	if ok, _ := breakable(none, none, StaleLockAfter-time.Minute); ok {
		t.Error("a recent lock with no heartbeat was declared breakable")
	}

	if ok, _ := breakable(none, none, StaleLockAfter+time.Minute); !ok {
		t.Error("a lock with no heartbeat, past the bound, was not breakable; the build " +
			"script writes no counter and its leaks would become permanent")
	}
}

// A HEARTBEAT THAT APPEARS BETWEEN THE TWO OBSERVATIONS is a holder that just
// started counting -- a newer billet taking over, or one whose first beat landed
// late. That is liveness, not absence.
func TestBreakableTreatsAnAppearingHeartbeatAsAlive(t *testing.T) {
	before := heartbeat{present: false}
	after := heartbeat{counter: 1, present: true}

	if ok, why := breakable(before, after, 100*StaleLockAfter); ok {
		t.Fatalf("a lock whose holder started counting between observations was declared "+
			"breakable (%q)", why)
	}
}

// A COUNTER THAT WENT BACKWARDS means the lock was released and retaken by someone
// else while this was deciding. Breaking it now would break the NEW holder's lock,
// which is the collision this whole mechanism exists to prevent.
func TestBreakableRefusesWhenTheCounterWentBackwards(t *testing.T) {
	before := heartbeat{counter: 9, present: true}
	after := heartbeat{counter: 2, present: true}

	if ok, why := breakable(before, after, 100*StaleLockAfter); ok {
		t.Fatalf("a lock whose counter went backwards -- released and retaken while this "+
			"was deciding -- was declared breakable (%q); that breaks the new holder's lock", why)
	}
}
