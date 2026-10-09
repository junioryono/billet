//go:build linux

package usage

import (
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pinnedSource records the one CPU each opened thread is pinned to, read from
// the kernel rather than from the proof, and opens a group that counts on every
// CPU but held.
type pinnedSource struct {
	held     int
	visited  []int
	unpinned []int
}

type pinnedGroup struct{ counts bool }

func (s *pinnedSource) Open(tid int) (CounterGroup, error) {
	set := unix.NewCPUSet(1 << 16)
	if err := unix.SchedGetaffinityDynamic(tid, set); err != nil {
		return nil, err
	}
	cpu := -1
	for c := range len(set) * bits.UintSize {
		if set.IsSet(c) {
			cpu = c
			break
		}
	}
	if set.Count() != 1 {
		s.unpinned = append(s.unpinned, set.Count())
	}
	s.visited = append(s.visited, cpu)

	return pinnedGroup{counts: cpu != s.held}, nil
}

func (pinnedGroup) Opened() [NumEvents]bool { return [NumEvents]bool{true} }

func (g pinnedGroup) Read() (CounterReading, error) {
	r := CounterReading{Enabled: math.MaxInt64}
	if g.counts {
		r.Running = time.Millisecond
	}

	return r, nil
}

func (pinnedGroup) Close() error { return nil }

// THE EXPORTED PROOF VISITS EVERY CPU THIS PROCESS MAY RUN ON, with its thread
// pinned to that CPU alone as the kernel reports it, and refuses at the last
// one when only the last one's counter is held.
func TestTheProofPinsItsThreadToEveryAllowedCPU(t *testing.T) {
	t.Parallel()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	allowed := cpusAllowed(t)
	src := &pinnedSource{held: allowed[len(allowed)-1]}
	err := ProveCounting(src)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("on CPU %d)", src.held)) ||
		!strings.Contains(err.Error(), "never counted") {
		t.Fatalf("ProveCounting with CPU %d held = %v, want a refusal naming it", src.held, err)
	}
	if !slices.Equal(src.visited, allowed) {
		t.Fatalf("proved on CPUs %v, want every allowed CPU %v", src.visited, allowed)
	}
	if len(src.unpinned) != 0 {
		t.Fatalf("opened on threads allowed %v CPUs, want each pinned to one", src.unpinned)
	}
}

// cpusAllowed is the calling thread's affinity as /proc reports it, read apart
// from the mask allowedCPUs reads, so the two are checked against each other.
// The caller locks its thread first: a thread's affinity is its own, and
// /proc/self is the main thread's, which the runtime may have parked.
func cpusAllowed(t *testing.T) []int {
	t.Helper()

	status, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		t.Fatalf("read /proc/thread-self/status: %v", err)
	}
	for line := range strings.Lines(string(status)) {
		list, ok := strings.CutPrefix(line, "Cpus_allowed_list:")
		if !ok {
			continue
		}
		var cpus []int
		for span := range strings.SplitSeq(strings.TrimSpace(list), ",") {
			lo, hi, _ := strings.Cut(span, "-")
			if hi == "" {
				hi = lo
			}
			first, errLo := strconv.Atoi(lo)
			last, errHi := strconv.Atoi(hi)
			if errLo != nil || errHi != nil {
				t.Fatalf("Cpus_allowed_list span %q", span)
			}
			for cpu := first; cpu <= last; cpu++ {
				cpus = append(cpus, cpu)
			}
		}
		if len(cpus) == 0 {
			t.Fatalf("Cpus_allowed_list %q names no CPU", list)
		}

		return cpus
	}
	t.Fatal("/proc/self/status has no Cpus_allowed_list")

	return nil
}

// THE LONGEST ROTATION INTERVAL IS THE ONE THE PROOF WAITS FOR, a PMU whose
// file cannot be read is passed over, and a host where none can be read gets
// the long guess rather than none.
func TestTheProofWaitsForTheSlowestRotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for pmu, ms := range map[string]string{"cpu": "10\n", "uncore": "4\n", "broken": "soon\n"} {
		if err := os.MkdirAll(filepath.Join(dir, pmu), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pmu, "perf_event_mux_interval_ms"), []byte(ms), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := muxInterval(dir); got != 10*time.Millisecond {
		t.Fatalf("muxInterval = %s, want the slowest PMU's 10ms", got)
	}
	if got := muxInterval(t.TempDir()); got != 100*time.Millisecond {
		t.Fatalf("muxInterval with no PMU = %s, want the 100ms guess", got)
	}
}

// THE PROOF AGAINST THIS HOST'S OWN PMU, with the answer the operator expects
// named in BILLET_TEST_REAL_PERF: "counts" where the group of every event can be
// scheduled, "refuses" where a counter is held elsewhere (the reference host
// with nmi_watchdog at 1, measured 2026-10-09). It runs as root, which counting
// needs where perf_event_paranoid is above 1.
func TestTheRealCountersProveWhatTheHostCanCount(t *testing.T) {
	want := os.Getenv("BILLET_TEST_REAL_PERF")
	if want == "" {
		t.Skip("set BILLET_TEST_REAL_PERF=counts or =refuses, as root, to prove this host's counters")
	}
	src, err := HardwareCounters()
	if err != nil {
		t.Fatalf("HardwareCounters: %v", err)
	}
	err = ProveCounting(src)
	switch want {
	case "counts":
		if err != nil {
			t.Fatalf("ProveCounting = %v, want the group to count", err)
		}
	case "refuses":
		if err == nil || !strings.Contains(err.Error(), "never counted") {
			t.Fatalf("ProveCounting = %v, want a group that never counted", err)
		}
	default:
		t.Fatalf("BILLET_TEST_REAL_PERF=%q, want counts or refuses", want)
	}
	t.Logf("ProveCounting = %v", err)
}
