//go:build !linux

package usage

import "errors"

// CountersSupported says this platform can count a thread's hardware events.
const CountersSupported = false

// HardwareCounters has nothing to open: perf_event_open is Linux's, and the one
// backend counted with it, Firecracker, runs only there.
func HardwareCounters() (CounterSource, error) {
	return nil, errors.New("usage: hardware counters are read through perf_event_open, which only Linux has")
}

// ProveCounting has nothing to prove where nothing can be opened.
func ProveCounting(CounterSource) error {
	return errors.New("usage: hardware counters are read through perf_event_open, which only Linux has")
}
