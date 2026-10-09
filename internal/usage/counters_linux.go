//go:build linux

package usage

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

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

// ProveCounting proves src's group of every event counts on this host, on
// every CPU the node may run on, before a node promises counts it would record
// as unmeasured on every job.
//
// ON A THREAD OF ITS OWN THAT IS NEVER UNLOCKED: the proof changes the
// thread's affinity, and a goroutine that exits locked ends its thread rather
// than handing the scheduler a thread pinned to one CPU. The affinity is put
// back as well, because the runtime parks rather than ends the process's main
// thread, and a parked main thread is what /proc/self reports.
func ProveCounting(src CounterSource) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		cpus, original, err := allowedCPUs()
		if err != nil {
			done <- fmt.Errorf("usage: read the CPUs this node may run on: %w", err)
			return
		}
		enough := proofBudget(muxInterval("/sys/bus/event_source/devices"))
		tid := unix.Gettid()
		proof := proveOnEveryCPU(cpus,
			func(cpu int) error {
				one := unix.NewCPUSet(cpu + 1)
				one.Set(cpu)
				return unix.SchedSetaffinityDynamic(0, one)
			},
			func() error {
				return proveCounting(src, tid, func() { spin(5 * time.Millisecond) }, enough, 400)
			})
		if err := unix.SchedSetaffinityDynamic(0, original); err != nil {
			proof = errors.Join(proof, fmt.Errorf("usage: put back the proving thread's CPUs: %w", err))
		}
		done <- proof
	}()

	return <-done
}

// muxInterval is the longest interval at which the kernel rotates the groups
// waiting for a PMU's counters, read from each PMU under dir, or 100 ms where
// none can be read: a longer guess costs only time where a group never counts.
// Measured on the reference host on 2026-10-09: 1 ms on every PMU.
func muxInterval(dir string) time.Duration {
	const unread = 100 * time.Millisecond
	paths, err := filepath.Glob(filepath.Join(dir, "*", "perf_event_mux_interval_ms"))
	if err != nil {
		return unread
	}
	longest := time.Duration(0)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		ms, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || ms <= 0 {
			continue
		}
		longest = max(longest, time.Duration(ms)*time.Millisecond)
	}
	if longest == 0 {
		return unread
	}

	return longest
}

// allowedCPUs is the calling thread's affinity, in a mask grown until the
// kernel's fits: the fixed unix.CPUSet holds 1,024 CPUs, and the kernel answers
// EINVAL to a mask smaller than its own, however few CPUs are allowed.
func allowedCPUs() ([]int, unix.CPUSetDynamic, error) {
	for n := 1024; ; n *= 2 {
		set := unix.NewCPUSet(n)
		err := unix.SchedGetaffinityDynamic(0, set)
		if errors.Is(err, unix.EINVAL) && n < 1<<22 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		var cpus []int
		for cpu := range len(set) * bits.UintSize {
			if set.IsSet(cpu) {
				cpus = append(cpus, cpu)
			}
		}

		return cpus, set, nil
	}
}

// spin keeps the calling thread on a CPU for d.
func spin(d time.Duration) {
	x := uint64(1)
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		x = x*6364136223846793005 + 1442695040888963407
	}
	runtime.KeepAlive(x)
}

// Open opens one group of every event on tid, on whichever CPU it runs.
//
// GUEST MODE IS COUNTED. A vCPU thread spends most of its time inside KVM_RUN
// executing the guest, and exclude_guest would leave only the exits; the perf
// CLI sets it by default, which is why a hand check names the events with :HG.
// Kernel mode is counted too, since the exits are the virtualization's cost.
//
// AN EVENT THE CPU DOES NOT HAVE IS LEFT OUT, never the whole group: the first
// event that opens leads, and Opened says which the rest are. The kernel says
// so with ENOENT (measured on the reference host on 2026-10-09 for
// stalled-cycles-backend and ref-cycles). Any other refusal fails the thread,
// because a group that lost an event to it would not be the group the startup
// proof ran, and a smaller group can count where the whole one never does.
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
		switch {
		case errors.Is(err, unix.ESRCH):
			_ = g.Close()
			return nil, ErrThreadGone
		case errors.Is(err, unix.ENOENT):
			continue
		case err != nil:
			_ = g.Close()
			return nil, fmt.Errorf("usage: open hardware event %d on thread %d: %w", e, tid, err)
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
