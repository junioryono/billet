package metrics

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PowerObserver counts what the node's idle power policy does. It satisfies
// internal/power's Observer by its methods alone, so neither package imports
// the other.
type PowerObserver struct {
	changes *prometheus.CounterVec
	// launch has no labels and no child until the first launch's restore, so a
	// node that has restored nothing reports no latency rather than zero.
	launch *prometheus.GaugeVec
	known  map[string]map[string]bool
}

// PowerObserver registers and returns the idle power policy's metrics. Every
// transition and outcome is reported from the start; a value outside them is
// counted as "other".
func (r *Registry) PowerObserver(transitions, outcomes []string) (*PowerObserver, error) {
	o := &PowerObserver{
		changes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "billet_node_power_changes_total",
			Help: "Changes the node's idle power policy made to this host's CPU ceiling, by transition (cap, " +
				"on a node idle for node.power.idle_after; restore, before a launch, when the node became busy " +
				"otherwise, at its start from an earlier process's record, or at its stop) and outcome (ok, " +
				"failed). A failed restore is retried until one succeeds.",
		}, []string{"transition", "outcome"}),
		launch: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "billet_node_power_last_launch_restore_seconds",
			Help: "How long the latest restore of the CPU's full ceiling before a launch took, from the launch " +
				"asking for it to every value read back. Absent until a launch has restored one.",
		}, nil),
		known: map[string]map[string]bool{"transition": {otherCause: true}, "outcome": {otherCause: true}},
	}

	for _, c := range []prometheus.Collector{o.changes, o.launch} {
		if err := r.reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: register the idle power policy's: %w", err)
		}
	}

	for _, t := range transitions {
		o.known["transition"][t] = true
	}
	for _, out := range outcomes {
		o.known["outcome"][out] = true
	}
	for _, t := range append(transitions, otherCause) {
		for _, out := range append(outcomes, otherCause) {
			o.changes.WithLabelValues(t, out)
		}
	}

	return o, nil
}

func (o *PowerObserver) closed(label, value string) string {
	if o.known[label][value] {
		return value
	}

	return otherCause
}

// PowerChanged counts one cap or restore by how it ended.
func (o *PowerObserver) PowerChanged(transition, outcome string) {
	o.changes.WithLabelValues(o.closed("transition", transition), o.closed("outcome", outcome)).Inc()
}

// PowerLaunchRestore records how long a launch's restore took.
func (o *PowerObserver) PowerLaunchRestore(took time.Duration) {
	o.launch.WithLabelValues().Set(took.Seconds())
}
