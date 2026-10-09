package app

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/simulated"
	jobusage "github.com/junioryono/billet/internal/usage"
)

// node.monitoring.perf WHERE THE HOST CANNOT COUNT STOPS THE NODE AT STARTUP,
// naming why, rather than running a sampler that records every event
// unmeasured on every job: on a platform without perf_event_open, and on a host
// whose group of events never counts. Where the host proves it counts, the same
// block starts one. Config cannot refuse it, because a config is also validated
// on a machine that will not run it.
//
// WHETHER THIS HOST COUNTS differs between a developer's Linux machine, a CI
// guest with no PMU and the reference host, so this asks only that the node
// agree with the host; TestPerfIsRefusedOnASourceThatNeverCounts holds the
// refusal to sources whose answer is fixed.
func TestPerfStartsOnlyWhereTheHostCanCount(t *testing.T) {
	t.Parallel()

	p, err := simulated.New("perf-test")
	if err != nil {
		t.Fatalf("simulated.New: %v", err)
	}
	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderFirecracker, Monitoring: &config.NodeMonitoringConfig{Perf: true},
	}}

	var proof error
	if src, err := jobusage.HardwareCounters(); err != nil {
		proof = err
	} else {
		proof = jobusage.ProveCounting(src)
	}

	opts, err := nodeMonitorOptions(t.Context(), cfg, p)
	if proof == nil {
		if err != nil || len(opts) != 1 {
			t.Fatalf("perf on a host that counts = %d options, %v; want a sampler", len(opts), err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "node.monitoring.perf is set, but this node cannot count") {
		t.Fatalf("perf on a host that cannot count (%v) = %d options, %v; want a refusal naming perf",
			proof, len(opts), err)
	}

	// AND WITHOUT perf THE SAME NODE STARTS, so the refusal is perf's alone.
	cfg.Node.Monitoring.Perf = false
	if opts, err := nodeMonitorOptions(t.Context(), cfg, p); err != nil || len(opts) != 1 {
		t.Fatalf("monitoring without perf = %d options, %v; want a sampler", len(opts), err)
	}
}

// fixedCounters opens, on any thread, a group that reads r.
type fixedCounters struct{ r jobusage.CounterReading }

func (s fixedCounters) Open(int) (jobusage.CounterGroup, error) { return s, nil }

func (fixedCounters) Opened() [jobusage.NumEvents]bool { return [jobusage.NumEvents]bool{true} }

func (s fixedCounters) Read() (jobusage.CounterReading, error) { return s.r, nil }

func (fixedCounters) Close() error { return nil }

// THE NODE STARTS COUNTING ONLY ON A SOURCE THAT PROVED IT COUNTS: one whose
// group was enabled and never ran is refused on every platform, naming the
// watchdog where the platform can count at all; one that ran is accepted
// exactly where the platform can count.
func TestPerfIsRefusedOnASourceThatNeverCounts(t *testing.T) {
	t.Parallel()

	_, err := provenCounters(fixedCounters{jobusage.CounterReading{Enabled: math.MaxInt64}})
	if err == nil || !strings.Contains(err.Error(), "node.monitoring.perf is set, but this node cannot count") {
		t.Fatalf("a source that never counted = %v, want a refusal naming perf", err)
	}
	if jobusage.CountersSupported && !strings.Contains(err.Error(), "nmi_watchdog") {
		t.Fatalf("a source that never counted = %v, want the watchdog named", err)
	}

	src := fixedCounters{jobusage.CounterReading{Enabled: time.Millisecond, Running: time.Millisecond}}
	got, err := provenCounters(src)
	if jobusage.CountersSupported && (err != nil || got != src) {
		t.Fatalf("a source that counted = %v, %v; want it accepted", got, err)
	}
	if !jobusage.CountersSupported && err == nil {
		t.Fatal("a source that counted, on a platform that cannot = accepted, want a refusal")
	}
}
