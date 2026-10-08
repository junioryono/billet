package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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
// failed, and while a read that overran is still running no second one starts.
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

	// reading is set while a read runs, which can outlive the scrape that
	// started it.
	reading atomic.Bool
}

type snapshotResult struct {
	snap Snapshot
	err  error
}

// collect runs one read, bounded by the collector's own timeout.
func (c *snapshotCollector) collect() (Snapshot, error) {
	if !c.reading.CompareAndSwap(false, true) {
		return nil, errors.New("the previous read has not finished")
	}

	done := make(chan snapshotResult, 1)

	go func() {
		defer c.reading.Store(false)

		snap, err := c.read()
		done <- snapshotResult{snap: snap, err: err}
	}()

	wait := time.NewTimer(c.timeout)
	defer wait.Stop()

	select {
	case r := <-done:
		return r.snap, r.err
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
