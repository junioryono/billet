package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ListenerObserver counts how long each tier's listener spent on each step of its
// loop. It satisfies internal/server's ListenerObserver by its methods alone, so the
// server imports no metrics library.
type ListenerObserver struct {
	polls    *prometheus.HistogramVec
	gaps     *prometheus.HistogramVec
	messages *prometheus.HistogramVec
	teardown *prometheus.HistogramVec
}

// listenerBuckets run from a message handled at once to the 953 seconds one tier took to
// hear a completion on 2026-11-04. A healthy poll sits near GitHub's long poll of 50
// seconds, which the buckets around it show apart from the failures either side.
var listenerBuckets = []float64{
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900, 1800,
}

// ListenerObserver registers and returns the listener timing metrics.
func (r *Registry) ListenerObserver() (*ListenerObserver, error) {
	histogram := func(name, help string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: name, Help: help, Buckets: listenerBuckets,
		}, []string{"tier"})
	}

	o := &ListenerObserver{
		polls: histogram("billet_listener_poll_seconds",
			"How long each answered poll of GitHub took, a message or none. A long poll with nothing to "+
				"deliver waits about 50 seconds, so most of this is GitHub having nothing to say."),
		gaps: histogram("billet_listener_poll_gap_seconds",
			"From one poll's answer to the next poll being sent: how long nothing was asking GitHub for "+
				"this tier's messages. A rising tail is a tier that has stopped hearing its jobs start and finish."),
		messages: histogram("billet_listener_message_handling_seconds",
			"How long the listener took to handle each message GitHub delivered."),
		teardown: histogram("billet_listener_teardown_seconds",
			"From a finished job's completion being handed to the cleanup loop to that loop settling it, "+
				"retries included: how long a finished guest stayed charged after billet heard it finished."),
	}

	for _, c := range []prometheus.Collector{o.polls, o.gaps, o.messages, o.teardown} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the listener timings: %w", err)
		}
	}

	return o, nil
}

// PollTook records one answered poll.
func (o *ListenerObserver) PollTook(tier string, took time.Duration) {
	o.polls.WithLabelValues(tier).Observe(took.Seconds())
}

// PollGapTook records one gap between polls.
func (o *ListenerObserver) PollGapTook(tier string, took time.Duration) {
	o.gaps.WithLabelValues(tier).Observe(took.Seconds())
}

// MessageHandled records one delivered message's handling.
func (o *ListenerObserver) MessageHandled(tier string, took time.Duration) {
	o.messages.WithLabelValues(tier).Observe(took.Seconds())
}

// TeardownTook records one completion's teardown.
func (o *ListenerObserver) TeardownTook(tier string, took time.Duration) {
	o.teardown.WithLabelValues(tier).Observe(took.Seconds())
}
