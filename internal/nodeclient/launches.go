package nodeclient

import (
	"context"
	"sync"
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

	s.wg.Go(func() {
		defer func() { <-s.slots }()

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

// wait returns once every launch started has finished.
func (s *launchSet) wait() { s.wg.Wait() }

// failed is the first error a launch's report ended the loop with, if any.
func (s *launchSet) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}
