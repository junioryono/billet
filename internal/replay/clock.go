package replay

import (
	"sync"
	"time"
)

// Clock is the one instant the allocator and the simulated provider read.
//
// VIRTUAL, NOT A WALL OFFSET. The end-to-end suite's clock is time.Now plus an
// offset, which is right for a scenario that waits on real things and wants one
// duration to pass faster. A replay wants the opposite: two runs of one trace
// must date every lease identically, and an offset from a wall clock cannot. So
// this holds an instant that moves only when the harness moves it, and every
// event in one step is stamped with exactly the same time.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at an instant.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start.UTC()}
}

// Now reports the instant. It is the func the allocator and provider hold.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// AdvanceTo moves the clock forward to t. A t not after the present leaves it
// where it is: the harness's events are ordered, and a clock that could step
// back would let the ledger date a completion before its assignment.
func (c *Clock) AdvanceTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t.After(c.now) {
		c.now = t.UTC()
	}
}
