package server

import (
	"sync/atomic"
	"time"
)

// ListenerObserver is told how long a tier's listener spent on each step of its loop, so
// a tier that has stopped hearing GitHub can be seen without joining billet's times to
// GitHub's by hand (#708). internal/metrics satisfies it by its methods alone, so this
// package imports no metrics library.
//
// CALLED ON THE LISTENER'S OWN GOROUTINES: an implementation returns at once and touches
// nothing that could reach the ledger or the network.
type ListenerObserver interface {
	// PollTook is one poll of GitHub that was answered, a message or none.
	PollTook(tier string, took time.Duration)
	// PollGapTook is from one poll's answer to the next poll being sent, when nothing
	// was asking GitHub for this tier's messages.
	PollGapTook(tier string, took time.Duration)
	// MessageHandled is one delivered message, from its handling starting to it ending.
	MessageHandled(tier string, took time.Duration)
}

// ListenerTimings is where a control plane's listeners report their times, and what the
// metrics endpoint connects to once it is serving, which may be before or after they
// start. A nil one, or one nothing observes, tells nothing.
type ListenerTimings struct {
	observer atomic.Pointer[ListenerObserver]
}

// Observe sets what these timings tell; nil tells nothing. It may be called while
// listeners run.
func (t *ListenerTimings) Observe(o ListenerObserver) {
	if o == nil {
		t.observer.Store(nil)

		return
	}

	t.observer.Store(&o)
}

// Current is what these timings tell now, or nil if nothing.
func (t *ListenerTimings) Current() ListenerObserver {
	if t == nil {
		return nil
	}

	o := t.observer.Load()
	if o == nil {
		return nil
	}

	return *o
}

func (t *ListenerTimings) pollTook(tier string, took time.Duration) {
	if o := t.Current(); o != nil {
		o.PollTook(tier, took)
	}
}

func (t *ListenerTimings) pollGapTook(tier string, took time.Duration) {
	if o := t.Current(); o != nil {
		o.PollGapTook(tier, took)
	}
}

func (t *ListenerTimings) messageHandled(tier string, took time.Duration) {
	if o := t.Current(); o != nil {
		o.MessageHandled(tier, took)
	}
}
