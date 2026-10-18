package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// UsageObserver counts how the node's job sampler keeps up: how long each
// tick's reads took, each interval of a job it did not see and why, and each
// read whose disk written is a lower bound. It satisfies internal/usage's
// Observer by its methods alone, so the sampler imports no metrics library.
type UsageObserver struct {
	ticks   prometheus.Histogram
	gaps    *prometheus.CounterVec
	causes  map[string]bool
	bounded prometheus.Counter
}

// tickBuckets run from a tenth of a millisecond, a tick over one idle job, to
// ten seconds, ten intervals of the default one.
var tickBuckets = prometheus.ExponentialBucketsRange(0.0001, 10, 16)

// UsageObserver registers and returns the sampler's metrics. causes is every
// name a gap may be counted under; each is reported from the start, and a
// name outside it is counted as "other" rather than as a new label.
func (r *Registry) UsageObserver(causes []string) (*UsageObserver, error) {
	o := &UsageObserver{
		ticks: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "billet_node_usage_tick_seconds",
			Help:    "How long each tick of the job sampler took to read every job it measures.",
			Buckets: tickBuckets,
		}),
		gaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_node_usage_gaps_total",
			Help: "Intervals of a job's series the sampler did not see, by why.",
		}, []string{"cause"}),
		causes: map[string]bool{},
		bounded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "billet_node_usage_io_bounded_total",
			Help: "Reads of a job's disk written that writeback kept moving on every pass, recorded as a " +
				"lower bound the next read corrects.",
		}),
	}

	for _, c := range []prometheus.Collector{o.ticks, o.gaps, o.bounded} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the job sampler's: %w", err)
		}
	}

	// EVERY CAUSE FROM THE START, so a rate over one that has not happened yet
	// reads zero rather than absent.
	for _, cause := range append(causes, otherCause) {
		o.causes[cause] = true
		o.gaps.WithLabelValues(cause)
	}

	return o, nil
}

// otherCause is the label a gap of a cause outside the list is counted under,
// which keeps the label set the closed one it was registered with.
const otherCause = "other"

// TickTook records how long one tick took.
func (o *UsageObserver) TickTook(d time.Duration) { o.ticks.Observe(d.Seconds()) }

// Gap counts one interval not seen, by its cause's name.
func (o *UsageObserver) Gap(cause string) {
	if !o.causes[cause] {
		cause = otherCause
	}

	o.gaps.WithLabelValues(cause).Inc()
}

// IOBounded counts one read whose disk written is a lower bound.
func (o *UsageObserver) IOBounded() { o.bounded.Inc() }
