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

// Step moves the clock to t, or one nanosecond past the present if t is not
// later, so that every event the harness delivers is dated at an instant of
// its own.
//
// THE LEDGER'S ORDER MUST BE ITS TIMESTAMPS' ORDER. Two events at one trace
// instant are delivered one after the other, and what the first did to the
// fleet is the state the second was placed against; dated identically, a
// reader sweeping the ledger cannot tell which came first, and a charge made
// before a same-instant release reads as made after it. A nanosecond keeps the
// order and moves nothing a report would notice.
func (c *Clock) Step(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := c.now.Add(time.Nanosecond)
	if t.After(next) {
		next = t.UTC()
	}

	c.now = next
}
