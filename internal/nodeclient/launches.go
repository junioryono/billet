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
	// running is one channel per launch in flight, closed when it returns, so a
	// destroy can wait for exactly the launches it arrived behind.
	running map[chan struct{}]struct{}
}

func newLaunchSet(limit int) *launchSet {
	return &launchSet{slots: make(chan struct{}, max(limit, 1)), running: map[chan struct{}]struct{}{}}
}

// inFlight is a channel for each launch running now, each closed when that launch
// returns. Launches started afterwards are not in it.
func (s *launchSet) inFlight() []<-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]<-chan struct{}, 0, len(s.running))
	for done := range s.running {
		out = append(out, done)
	}

	return out
}

// start runs launch once a slot is free, reporting false if ctx ended first, in
// which case launch did not run.
func (s *launchSet) start(ctx context.Context, launch func() error) bool {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}

	done := make(chan struct{})

	s.mu.Lock()
	s.running[done] = struct{}{}
	s.mu.Unlock()

	s.wg.Go(func() {
		defer func() { <-s.slots }()
		defer func() {
			s.mu.Lock()
			delete(s.running, done)
			s.mu.Unlock()
			close(done)
		}()

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

// startBehind runs launch once every channel in behind has closed and then a slot is
// free, without the caller waiting for either.
//
// IN FLIGHT FROM NOW, so a destroy queued while it waits waits for it in turn; AND
// HOLDING NO SLOT WHILE IT WAITS, so launches waiting on teardowns never leave the
// loop blocked behind them for a slot an unrelated launch needs. It always runs: one
// whose command the node has stopped serving by then is refused by the launch
// itself, and reported.
func (s *launchSet) startBehind(behind []<-chan struct{}, launch func() error) {
	done := make(chan struct{})

	s.mu.Lock()
	s.running[done] = struct{}{}
	s.mu.Unlock()

	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, done)
			s.mu.Unlock()
			close(done)
		}()

		for _, earlier := range behind {
			<-earlier
		}

		s.slots <- struct{}{}
		defer func() { <-s.slots }()

		if err := launch(); err != nil {
			s.mu.Lock()
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()
		}
	})
}

// tryStart runs work if a slot is free now, and reports false if none is, in which case work
// did not run. For what is never worth stopping the loop to wait for.
func (s *launchSet) tryStart(work func() error) bool {
	select {
	case s.slots <- struct{}{}:
	default:
		return false
	}

	s.wg.Go(func() {
		defer func() { <-s.slots }()

		if err := work(); err != nil {
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
