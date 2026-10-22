package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// GitObserver counts what the node's Git proxy does. It satisfies
// internal/node's GitObserver by its methods alone, so the node imports no
// metrics library.
type GitObserver struct {
	requests  *prometheus.CounterVec
	durations *prometheus.HistogramVec
	refreshes *prometheus.CounterVec
	refreshed prometheus.Histogram
	lookups   *prometheus.CounterVec
	known     map[string]map[string]bool
}

// GitLabels is every value each of the Git proxy's labels may take, as the
// node package names them.
type GitLabels struct {
	Routes, Reasons, Statuses, RefreshOutcomes, LookupOutcomes []string
}

// gitRequestBuckets run from a hundredth of a second, an advertisement served
// from the mirror, to an hour, a large clone on a slow line.
var gitRequestBuckets = prometheus.ExponentialBucketsRange(0.01, 3600, 18)

// GitObserver registers and returns the Git proxy's metrics. Every label value
// in labels is reported from the start, so a rate over one that has not
// happened yet reads zero rather than absent; a value outside them is counted
// as "other".
func (r *Registry) GitObserver(labels GitLabels) (*GitObserver, error) {
	o := &GitObserver{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_node_git_requests_total",
			Help: "Git requests the node's proxy answered, by where (mirror, github, refused), why, and the " +
				"class of the status it answered with (none for a request that ended before it answered).",
		}, []string{"route", "reason", "status"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "billet_node_git_request_seconds",
			Help:    "How long each Git request through the node's proxy took, by where it was answered.",
			Buckets: gitRequestBuckets,
		}, []string{"route"}),
		refreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_node_git_refreshes_total",
			Help: "Refreshes of a Git mirror, by how each ended: ok, unverified (GitHub did not name the same " +
				"repository after the fetch), too_large, busy (no fetch slot), disk_full, closed, or failed.",
		}, []string{"outcome"}),
		refreshed: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "billet_node_git_refresh_seconds",
			Help:    "How long each refresh of a Git mirror took, whatever its outcome.",
			Buckets: gitRequestBuckets,
		}),
		lookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_node_git_repository_lookups_total",
			Help: "Questions to GitHub's API about which repository a name is, one with every advertisement " +
				"and after every refresh, by its answer: ok, refused, rate_limited, or failed. Any answer but ok " +
				"sends that request to GitHub.",
		}, []string{"outcome"}),
		known: map[string]map[string]bool{},
	}

	for _, c := range []prometheus.Collector{o.requests, o.durations, o.refreshes, o.refreshed, o.lookups} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the Git proxy's: %w", err)
		}
	}

	for name, values := range map[string][]string{"route": labels.Routes, "reason": labels.Reasons,
		"status": labels.Statuses, "refresh": labels.RefreshOutcomes, "lookup": labels.LookupOutcomes} {
		o.known[name] = map[string]bool{otherCause: true}
		for _, v := range values {
			o.known[name][v] = true
		}
	}
	for _, route := range append(labels.Routes, otherCause) {
		o.durations.WithLabelValues(route)
		for _, reason := range append(labels.Reasons, otherCause) {
			for _, status := range append(labels.Statuses, otherCause) {
				o.requests.WithLabelValues(route, reason, status)
			}
		}
	}
	for _, outcome := range append(labels.RefreshOutcomes, otherCause) {
		o.refreshes.WithLabelValues(outcome)
	}
	for _, outcome := range append(labels.LookupOutcomes, otherCause) {
		o.lookups.WithLabelValues(outcome)
	}

	return o, nil
}

// closed is value when it is one of label's known values, and "other" when not.
func (o *GitObserver) closed(label, value string) string {
	if o.known[label][value] {
		return value
	}

	return otherCause
}

// GitRequest counts one request and how long it took.
func (o *GitObserver) GitRequest(route, reason, status string, took time.Duration) {
	route = o.closed("route", route)
	o.requests.WithLabelValues(route, o.closed("reason", reason), o.closed("status", status)).Inc()
	o.durations.WithLabelValues(route).Observe(took.Seconds())
}

// GitRefresh counts one refresh by how it ended, and how long it took.
func (o *GitObserver) GitRefresh(outcome string, took time.Duration) {
	o.refreshes.WithLabelValues(o.closed("refresh", outcome)).Inc()
	o.refreshed.Observe(took.Seconds())
}

// GitLookup counts one question to GitHub's API by its answer.
func (o *GitObserver) GitLookup(outcome string) {
	o.lookups.WithLabelValues(o.closed("lookup", outcome)).Inc()
}
