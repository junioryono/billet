package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// LedgerObserver counts the ledger's writes: how long each waited for the
// single writer slot, how long it held it, and each busy retry. It satisfies
// internal/state's Observer by its methods alone, so the ledger imports no
// metrics library.
type LedgerObserver struct {
	waited  prometheus.Histogram
	held    *prometheus.HistogramVec
	retries prometheus.Counter
}

// writeBuckets run from half a millisecond, a write nobody waited for, to half
// a minute, a slot a stalled holder kept.
var writeBuckets = prometheus.ExponentialBucketsRange(0.0005, 30, 14)

// LedgerObserver registers and returns the ledger's write metrics.
func (r *Registry) LedgerObserver() (*LedgerObserver, error) {
	o := &LedgerObserver{
		waited: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "billet_ledger_write_wait_seconds",
			Help:    "How long each write waited for the ledger's single writer slot.",
			Buckets: writeBuckets,
		}),
		held: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "billet_ledger_write_held_seconds",
			Help:    "How long each write held the writer slot, by whether it committed or rolled back.",
			Buckets: writeBuckets,
		}, []string{"outcome"}),
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "billet_ledger_write_busy_retries_total",
			Help: "Each time a write found the writer slot taken and tried again.",
		}),
	}

	for _, c := range []prometheus.Collector{o.waited, o.held, o.retries} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the ledger's writes: %w", err)
		}
	}

	// BOTH OUTCOMES FROM THE START, so a rate over rolled-back writes reads zero
	// rather than absent until the first one.
	o.held.WithLabelValues("committed")
	o.held.WithLabelValues("rolled_back")

	return o, nil
}

// WriteWaited records how long a write waited for the writer slot.
func (o *LedgerObserver) WriteWaited(d time.Duration) { o.waited.Observe(d.Seconds()) }

// WriteHeld records how long a write held the slot, and how it ended.
func (o *LedgerObserver) WriteHeld(d time.Duration, committed bool) {
	outcome := "rolled_back"
	if committed {
		outcome = "committed"
	}

	o.held.WithLabelValues(outcome).Observe(d.Seconds())
}

// WriteRetried counts a busy retry.
func (o *LedgerObserver) WriteRetried() { o.retries.Inc() }
