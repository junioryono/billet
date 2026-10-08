package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Family is one gauge a snapshot reports.
type Family struct {
	Name   string
	Help   string
	Labels []string
}

// Sample is one value of a family, its label values in the family's order.
type Sample struct {
	Labels []string
	Value  float64
}

// Snapshot is what one read reported, by family name.
type Snapshot map[string][]Sample

// upFamily says whether a snapshot source's last read succeeded.
const upFamily = "billet_scrape_up"

// RegisterSnapshot registers families that read reports together, read afresh
// at every scrape and bounded by timeout, under ctx.
//
// COULD NOT TELL IS NOT ZERO. A read that fails reports none of its families,
// so a dashboard shows a gap rather than an empty fleet, and sets
// billet_scrape_up{source} to 0.
//
// THE BOUND IS THE COLLECTOR'S, NOT ONLY THE READ'S. A deadline asks a read to
// stop; one that does not listen would hold the scrape, and the runtime's
// metrics with it. So the scrape waits at most timeout and reports the source
// failed, and while a read is still running no second one starts: a scrape
// that arrives meanwhile waits for it.
func (r *Registry) RegisterSnapshot(ctx context.Context, source string, families []Family,
	timeout time.Duration, read func(context.Context) (Snapshot, error),
) error {
	c := &snapshotCollector{
		source:   source,
		families: map[string]*prometheus.Desc{},
		labels:   map[string]int{},
		up: prometheus.NewDesc(upFamily, "1 when the source's last read succeeded, 0 when it failed and "+
			"its metrics were left out.", nil, prometheus.Labels{"source": source}),
		timeout: timeout,
		read: func() (Snapshot, error) {
			bounded, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			return read(bounded)
		},
	}

	for _, f := range families {
		c.families[f.Name] = prometheus.NewDesc(f.Name, f.Help, f.Labels, nil)
		c.labels[f.Name] = len(f.Labels)
	}

	if err := r.reg.Register(c); err != nil {
		return fmt.Errorf("metrics: register %s: %w", source, err)
	}

	return nil
}

type snapshotCollector struct {
	source   string
	families map[string]*prometheus.Desc
	labels   map[string]int
	up       *prometheus.Desc
	timeout  time.Duration
	read     func() (Snapshot, error)

	// mu guards current, the read in flight, which can outlive the scrape that
	// started it.
	mu      sync.Mutex
	current *snapshotFlight
}

// onJoin runs when a scrape waits for a read another scrape started. A TEST
// HOOK, nil in production: a scrape joining a flight is otherwise not
// observable, and a test that only hoped two scrapes overlapped would pass
// whether or not they shared one read.
var onJoin func()

// snapshotFlight is one read and, once done is closed, what it returned.
type snapshotFlight struct {
	done chan struct{}
	snap Snapshot
	err  error
}

// collect answers with one read, bounded by the collector's own timeout.
//
// ONE READ AT A TIME, SHARED. A scrape that arrives while a read is running
// waits for that read, within its own bound, rather than starting another or
// being turned away: two scrapers at once both get an answer, and a read that
// overran does not have a second stacked behind it on every scrape.
func (c *snapshotCollector) collect() (Snapshot, error) {
	c.mu.Lock()

	f := c.current
	if f != nil && onJoin != nil {
		onJoin()
	}

	if f == nil {
		f = &snapshotFlight{done: make(chan struct{})}
		c.current = f

		go func() {
			snap, err := c.read()

			c.mu.Lock()
			f.snap, f.err = snap, err
			c.current = nil
			c.mu.Unlock()

			close(f.done)
		}()
	}

	c.mu.Unlock()

	wait := time.NewTimer(c.timeout)
	defer wait.Stop()

	select {
	case <-f.done:
		return f.snap, f.err
	case <-wait.C:
		return nil, fmt.Errorf("no answer within %s", c.timeout)
	}
}

func (c *snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	for _, d := range c.families {
		ch <- d
	}
}

func (c *snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	snap, err := c.collect()
	if err != nil {
		slog.Default().Warn("a metrics source could not be read; its metrics are left out of this scrape",
			"source", c.source, "error", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)

		return
	}

	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)

	for name, samples := range snap {
		desc, ok := c.families[name]
		if !ok {
			continue
		}

		for _, s := range samples {
			if len(s.Labels) != c.labels[name] {
				continue
			}

			m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, s.Value, s.Labels...)
			if err != nil {
				continue
			}

			ch <- m
		}
	}
}
