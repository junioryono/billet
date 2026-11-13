package ebss3

import (
	"context"
	"sync"
	"time"
)

const (
	// detachYield bounds how long a detach waits for this store's attaches to see their
	// devices before it asks EC2 anyway. Fifteen attaches at once to a z1d.metal had
	// their devices within 5.5 s (2026-11-13).
	detachYield = 30 * time.Second
	// detachYieldPoll is how often a waiting detach looks again.
	detachYieldPoll = 100 * time.Millisecond
	// detachSettleLimit bounds one wait for EC2 to call a detached volume available,
	// so no stalled detach holds every other one: one alone took 10.5 s on a z1d.metal
	// (2026-11-13).
	detachSettleLimit = 2 * time.Minute
)

// attachQueue orders this store's own changes to its instance's attachments.
//
// EC2 CARRIES THEM OUT ONE AT A TIME PER INSTANCE, IN ORDER, AND A DETACH TAKES ABOUT
// 7.5 S. Measured on a z1d.metal and a c7g.metal (2026-11-13): an attach asked for one
// second after eight detaches had its device 60 s and 69.6 s later, against 1.7 s alone;
// after one detach, 9.6 s. On a pool host every guest's volume is attached to the one
// instance, and a phase of jobs ending detaches as the next phase's jobs attach, so an
// attach waited behind every detach before it and missed the breaker's deadline (#861).
// Attaches still go at once (fifteen took 5.5 s); a detach goes one at a time, and only
// once no attach of this store is waiting for its device, or detachYield has passed, so
// an attach waits behind at most the one detach EC2 is already carrying out.
//
// IT ALSO KNOWS THE NAMES ITS OWN ATTACHMENTS HOLD, so an attach asks EC2 only for a name
// none of them holds, rather than walking from /dev/sdf through every one in use: one
// run spent 205 refused AttachVolume calls doing that (2026-11-12). A name another
// process's volume holds is still refused by EC2, and the walk goes on past it.
//
// A nil queue orders nothing, as a store that attaches nothing to its own instance needs.
type attachQueue struct {
	mu sync.Mutex
	// attaching are this store's volumes between their attach's request and their device.
	attaching map[string]int
	// names are the device names this store's attachments hold, each to its volume.
	names map[string]string
	// unsettled is a volume EC2 accepted a detach of and has not been seen to finish:
	// the next detach waits for it first, so EC2 is never carrying out two of this
	// store's at once.
	unsettled string
	// detach holds the one detach in flight.
	detach chan struct{}
	yield  time.Duration
	poll   time.Duration
}

func newAttachQueue() *attachQueue {
	return &attachQueue{attaching: map[string]int{}, names: map[string]string{},
		detach: make(chan struct{}, 1), yield: detachYield, poll: detachYieldPoll}
}

// attachStarted counts volume's attach as waiting for its device until the returned
// function is called.
func (q *attachQueue) attachStarted(volume string) func() {
	if q == nil {
		return func() {}
	}
	q.mu.Lock()
	q.attaching[volume]++
	q.mu.Unlock()

	var once sync.Once

	return func() {
		once.Do(func() {
			q.mu.Lock()
			q.attaching[volume]--
			if q.attaching[volume] <= 0 {
				delete(q.attaching, volume)
			}
			q.mu.Unlock()
		})
	}
}

// reconcile forgets every name held by a volume that is neither attached here, by
// EC2's listing, nor being attached by this store: a detach whose end was never seen
// would otherwise keep its name for the life of the process.
func (q *attachQueue) reconcile(attached map[string]time.Time) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for name, volume := range q.names {
		if _, on := attached[volume]; !on && q.attaching[volume] == 0 {
			delete(q.names, name)
		}
	}
}

// settling is the volume whose detach was accepted and not seen to finish, if any.
func (q *attachQueue) settling() string {
	if q == nil {
		return ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.unsettled
}

// settle records whether volume's detach is unsettled.
func (q *attachQueue) settle(volume string, unsettled bool) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case unsettled:
		q.unsettled = volume
	case q.unsettled == volume:
		q.unsettled = ""
	}
}

// reserve takes name for volume unless one of this store's attachments holds it.
func (q *attachQueue) reserve(name, volume string) bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, held := q.names[name]; held {
		return false
	}
	q.names[name] = volume

	return true
}

// release gives name back, if volume still holds it.
func (q *attachQueue) release(name, volume string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.names[name] == volume {
		delete(q.names, name)
	}
}

// forget gives back every name volume holds: it is detached.
func (q *attachQueue) forget(volume string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for name, holder := range q.names {
		if holder == volume {
			delete(q.names, name)
		}
	}
}

// detachTurn waits for this store's turn to detach a volume: the one detach slot, then
// no attach waiting for its device, for at most yield. The returned function ends the
// turn, once EC2 has carried the detach out.
func (q *attachQueue) detachTurn(ctx context.Context) (func(), error) {
	if q == nil {
		return func() {}, nil
	}
	select {
	case q.detach <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	end := func() { <-q.detach }

	yielded := time.NewTimer(q.yield)
	defer yielded.Stop()
	poll := time.NewTicker(q.poll)
	defer poll.Stop()
	for {
		q.mu.Lock()
		waiting := len(q.attaching)
		q.mu.Unlock()
		if waiting == 0 {
			return end, nil
		}
		select {
		case <-ctx.Done():
			end()

			return nil, ctx.Err()
		case <-yielded.C:
			return end, nil
		case <-poll.C:
		}
	}
}
