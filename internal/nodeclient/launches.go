package nodeclient

import (
	"context"
	"sync"
	"sync/atomic"
)

// launchSet runs launch commands beside the command loop, at most limit at once.
//
// ONLY LAUNCHES OVERLAP, AND ONLY WITH EACH OTHER. Every other command waits for
// the launches in flight before it runs, so a destroy, an inventory or an upgrade
// sees the node exactly as it did when commands ran one at a time: a destroy can
// never overtake the launch of its own lease, and an inventory never counts a
// guest halfway started. What is gained is the launches themselves, which were
// the node's whole throughput: one runner every 30 to 65 seconds on the reference
// deployment on 2026-10-03, with 230 jobs queued and most of the host free.
type launchSet struct {
	slots chan struct{}
	wg    sync.WaitGroup
	// inflight counts the launches started and not yet returned.
	inflight atomic.Int32

	mu  sync.Mutex
	err error
}

func newLaunchSet(limit int) *launchSet {
	return &launchSet{slots: make(chan struct{}, max(limit, 1))}
}

// start runs launch once a slot is free, reporting false if ctx ended first, in
// which case launch did not run.
func (s *launchSet) start(ctx context.Context, launch func() error) bool {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}

	s.inflight.Add(1)
	s.wg.Go(func() {
		defer func() { <-s.slots }()
		defer s.inflight.Add(-1)

		if err := launch(); err != nil {
			s.mu.Lock()
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()
		}
	})

	return true
}

// running is how many launches started here have not yet returned.
func (s *launchSet) running() int { return int(s.inflight.Load()) }

// wait returns once every launch started has finished.
func (s *launchSet) wait() { s.wg.Wait() }

// failed is the first error a launch's report ended the loop with, if any.
func (s *launchSet) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}
