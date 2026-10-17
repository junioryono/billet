package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// QueueWaitObserver counts how long jobs waited, by tier. It satisfies
// internal/alloc's QueueWaitObserver by its method alone, so the allocator
// imports no metrics library.
type QueueWaitObserver struct {
	waits    *prometheus.HistogramVec
	negative *prometheus.CounterVec
}

// queueWaitBuckets run from a runner that was already waiting to a job that
// sat at GitHub for several hours: 2026-10-16's waits reached 55 minutes, and
// the tail is what this exists to show.
var queueWaitBuckets = []float64{
	5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 450, 600, 900, 1200, 1800, 2700, 3600,
	5400, 7200, 10800, 14400, 21600,
}

// QueueWaitObserver registers and returns the queue-wait metrics.
func (r *Registry) QueueWaitObserver() (*QueueWaitObserver, error) {
	o := &QueueWaitObserver{
		waits: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "billet_job_queue_wait_seconds",
			Help: "How long each job waited, from GitHub's queue time to billet recording its start, " +
				"observed once a job when both are known. The queue time is GitHub's clock and the start " +
				"billet's, so a wait is only as exact as the two agree; one below zero is counted in " +
				"billet_job_queue_wait_negative_total instead.",
			Buckets: queueWaitBuckets,
		}, []string{"tier"}),
		negative: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_job_queue_wait_negative_total",
			Help: "Jobs whose start on billet's clock came before GitHub's queue time on GitHub's: " +
				"the clocks disagreed by more than the wait. Not observed in billet_job_queue_wait_seconds, " +
				"where it would read as no wait.",
		}, []string{"tier"}),
	}

	for _, c := range []prometheus.Collector{o.waits, o.negative} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the queue waits: %w", err)
		}
	}

	return o, nil
}

// JobWaited records one job's wait. A negative one is counted, never observed
// as a short wait.
func (o *QueueWaitObserver) JobWaited(tier string, wait time.Duration) {
	if wait < 0 {
		o.negative.WithLabelValues(tier).Inc()

		return
	}

	o.waits.WithLabelValues(tier).Observe(wait.Seconds())
}
