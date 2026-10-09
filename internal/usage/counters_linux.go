//go:build linux

package usage

import (
	"errors"
	"fmt"

	"github.com/elastic/go-perf"
	"golang.org/x/sys/unix"
)

// CountersSupported says this platform can count a thread's hardware events.
const CountersSupported = true

// hardwareEvents maps each Event to the kernel's generic hardware event.
var hardwareEvents = [NumEvents]perf.HardwareCounter{
	Cycles:          perf.CPUCycles,
	Instructions:    perf.Instructions,
	CacheReferences: perf.CacheReferences,
	CacheMisses:     perf.CacheMisses,
	BranchMisses:    perf.BranchMisses,
	FrontendStalls:  perf.StalledCyclesFrontend,
}

// perfSource counts threads with perf_event_open.
type perfSource struct{}

// HardwareCounters is the CPU's hardware counters, read through
// perf_event_open. It needs the privilege to count another process's threads,
// which a node running as root has.
func HardwareCounters() (CounterSource, error) { return perfSource{}, nil }

// Open opens one group of every event on tid, on whichever CPU it runs.
//
// GUEST MODE IS COUNTED. A vCPU thread spends most of its time inside KVM_RUN
// executing the guest, and exclude_guest would leave only the exits; the perf
// CLI sets it by default, which is why a hand check names the events with :HG.
// Kernel mode is counted too, since the exits are the virtualization's cost.
//
// AN EVENT THAT WILL NOT OPEN IS LEFT OUT, never the whole group: the first
// event that opens leads, and Opened says which the rest are.
func (perfSource) Open(tid int) (CounterGroup, error) {
	g := &perfGroup{}
	for e, hw := range hardwareEvents {
		// THE LEADER OPENS DISABLED and the group is enabled whole below, so no
		// event counts in a group that is not yet the one read: the group's
		// times are the leader's, and a partial group that ran before the full
		// one proved unschedulable would scale its first instants over the job.
		attr := &perf.Attr{
			CountFormat: perf.CountFormat{Enabled: true, Running: true, Group: g.leader == nil},
			Options: perf.Options{Disabled: g.leader == nil,
				ExcludeGuest: false, ExcludeHost: false, ExcludeKernel: false},
		}
		if err := hw.Configure(attr); err != nil {
			continue
		}
		ev, err := perf.Open(attr, tid, perf.AnyCPU, g.leader)
		if errors.Is(err, unix.ESRCH) {
			_ = g.Close()
			return nil, ErrThreadGone
		}
		if err != nil {
			continue
		}
		if g.leader == nil {
			g.leader = ev
		} else {
			g.members = append(g.members, ev)
		}
		g.events = append(g.events, Event(e))
		g.opened[e] = true
	}
	if g.leader == nil {
		return nil, fmt.Errorf("usage: no hardware event could be counted on thread %d", tid)
	}
	if err := g.leader.Enable(); err != nil {
		_ = g.Close()
		if errors.Is(err, unix.ESRCH) {
			return nil, ErrThreadGone
		}
		return nil, fmt.Errorf("usage: enable the counters of thread %d: %w", tid, err)
	}

	return g, nil
}

// perfGroup is one thread's open events, the first leading.
type perfGroup struct {
	leader  *perf.Event
	members []*perf.Event
	events  []Event
	opened  [NumEvents]bool
}

func (g *perfGroup) Opened() [NumEvents]bool { return g.opened }

// Read reads the whole group at once: nr, time enabled, time running, then each
// event's value in the order the group opened them.
//
// READ HERE RATHER THAN THROUGH THE LIBRARY, whose group read decodes whatever
// came back without checking how much did: a short read (end of file, from a
// group in error) would read as every count being zero.
func (g *perfGroup) Read() (CounterReading, error) {
	fd, err := g.leader.FD()
	if err != nil {
		return CounterReading{}, err
	}
	want := 8 * (3 + len(g.events))
	buf := make([]byte, want)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return CounterReading{}, fmt.Errorf("usage: read a counter group: %w", err)
	}
	if n != want {
		return CounterReading{}, fmt.Errorf("usage: a counter group read %d bytes, want %d", n, want)
	}

	return decodeGroupRead(buf, g.events)
}

// Close closes the members before their leader.
func (g *perfGroup) Close() error {
	var errs []error
	for _, ev := range g.members {
		errs = append(errs, ev.Close())
	}
	if g.leader != nil {
		errs = append(errs, g.leader.Close())
	}

	return errors.Join(errs...)
}
