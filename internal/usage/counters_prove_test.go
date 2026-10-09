package usage

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// A GROUP ENABLED FOR ENOUGH THAT NEVER COUNTED IS REFUSED, naming the
// watchdog, as soon as it has been; one enabled too briefly to tell (a thread
// descheduled for the proof's rounds) is refused as unknown after every round,
// never blamed on the watchdog; one that counted is accepted at its first
// round. The reading is taken after the work, and the group is closed either
// way.
func TestTheProofRefusesAGroupThatNeverCounted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reading CounterReading
		want    string
		rounds  int
	}{
		{"never counted", CounterReading{Enabled: 20 * time.Millisecond}, "nmi_watchdog=0", 1},
		{"never enabled", CounterReading{}, "unknown", 3},
		{"enabled too briefly", CounterReading{Enabled: 5 * time.Millisecond}, "unknown", 3},
		{"counted", CounterReading{Enabled: time.Millisecond, Running: time.Millisecond}, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := newFakeCounters()
			rounds := 0
			err := proveCounting(src, 7, func() {
				rounds++
				src.set([]int{7}, tc.reading)
			}, 10*time.Millisecond, 3)
			if rounds != tc.rounds {
				t.Fatalf("the proof ran its work %d times, want %d", rounds, tc.rounds)
			}
			if tc.want == "" && err != nil {
				t.Fatalf("a group that counted = %v, want accepted", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("proof = %v, want a refusal naming %q", err, tc.want)
			}
			if tc.want == "unknown" && strings.Contains(err.Error(), "nmi_watchdog") {
				t.Fatalf("proof = %v, want could-not-tell without blaming the watchdog", err)
			}
			if open := src.open(); len(open) != 0 {
				t.Fatalf("groups left open = %v, want none", open)
			}
		})
	}
}

// A GROUP THAT COUNTS ONLY ONCE THE KERNEL ROTATES IT IN IS ACCEPTED, because
// the proof reads again after more work rather than judging the first read.
func TestTheProofWaitsForAGroupToBeRotatedIn(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	rounds := 0
	err := proveCounting(src, 7, func() {
		rounds++
		r := CounterReading{Enabled: time.Duration(rounds) * time.Millisecond}
		if rounds >= 8 {
			r.Running = time.Millisecond
		}
		src.set([]int{7}, r)
	}, 10*time.Millisecond, 100)
	if err != nil || rounds != 8 {
		t.Fatalf("a group rotated in at the eighth round = %v after %d rounds, want accepted after 8", err, rounds)
	}
}

// THE BUDGET IS TEN ROTATIONS, never under 15 ms: a group behind nine others
// at a 5 ms rotation first counts at 45 ms, which three rotations would have
// refused, and a 1 ms rotation still waits the floor.
func TestTheProofBudgetIsTenRotations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ rotation, want time.Duration }{
		{5 * time.Millisecond, 50 * time.Millisecond},
		{time.Millisecond, 15 * time.Millisecond},
		{100 * time.Millisecond, time.Second},
	} {
		if got := proofBudget(tc.rotation); got != tc.want {
			t.Fatalf("proofBudget(%s) = %s, want %s", tc.rotation, got, tc.want)
		}
	}

	src := newFakeCounters()
	rounds := 0
	err := proveCounting(src, 7, func() {
		rounds++
		r := CounterReading{Enabled: time.Duration(rounds) * 5 * time.Millisecond}
		if rounds >= 9 {
			r.Running = time.Millisecond
		}
		src.set([]int{7}, r)
	}, proofBudget(5*time.Millisecond), 100)
	if err != nil || rounds != 9 {
		t.Fatalf("a group first counting at 45 ms of a 5 ms rotation = %v after %d rounds, want accepted", err, rounds)
	}
}

// A THREAD THAT CANNOT BE OPENED OR READ IS NO PROOF, and the error says why.
func TestTheProofRefusesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	refused := errors.New("perf_event_open: no such device")
	src.openErr[7] = refused
	if err := proveCounting(src, 7, func() {}, time.Millisecond, 1); !errors.Is(err, refused) {
		t.Fatalf("an unopenable thread = %v, want %v", err, refused)
	}

	src = newFakeCounters()
	unread := errors.New("short read")
	src.readErr[8] = unread
	err := proveCounting(src, 8, func() {
		src.set([]int{8}, CounterReading{Enabled: time.Millisecond, Running: time.Millisecond})
	}, time.Millisecond, 1)
	if !errors.Is(err, unread) {
		t.Fatalf("an unreadable group = %v, want %v", err, unread)
	}
	if open := src.open(); len(open) != 0 {
		t.Fatalf("groups left open after a failed read = %v, want none", open)
	}
}

// A CPU WHERE THE GROUP NEVER COUNTS IS REFUSED BY NUMBER, however many CPUs
// before it counted, and every CPU is proved on with the thread pinned to it;
// one that cannot be pinned to is refused too.
func TestTheProofCoversEveryCPU(t *testing.T) {
	t.Parallel()

	held := map[int]bool{5: true}
	on := -1
	var proved []int
	src := newFakeCounters()
	prove := func() error {
		return proveCounting(src, 7, func() {
			proved = append(proved, on)
			r := CounterReading{Enabled: time.Millisecond, Running: time.Millisecond}
			if held[on] {
				r.Running = 0
			}
			src.set([]int{7}, r)
		}, time.Millisecond, 1)
	}
	unpinnable := map[int]error{}
	pin := func(cpu int) error {
		if err := unpinnable[cpu]; err != nil {
			return err
		}
		on = cpu

		return nil
	}

	if err := proveOnEveryCPU([]int{0, 1, 2, 3}, pin, prove); err != nil {
		t.Fatalf("four CPUs that count = %v, want accepted", err)
	}
	if want := []int{0, 1, 2, 3}; !slices.Equal(proved, want) {
		t.Fatalf("proved on CPUs %v, want %v", proved, want)
	}

	err := proveOnEveryCPU([]int{0, 1, 5, 6}, pin, prove)
	if err == nil || !strings.Contains(err.Error(), "on CPU 5") || !strings.Contains(err.Error(), "never counted") {
		t.Fatalf("a CPU whose counter is held = %v, want a refusal naming CPU 5", err)
	}

	unpinned := errors.New("invalid argument")
	unpinnable[9] = unpinned
	err = proveOnEveryCPU([]int{0, 9}, pin, prove)
	if !errors.Is(err, unpinned) || !strings.Contains(err.Error(), "CPU 9") {
		t.Fatalf("a CPU that cannot be pinned to = %v, want a refusal naming CPU 9", err)
	}
	if err := proveOnEveryCPU(nil, pin, prove); err == nil {
		t.Fatal("no CPU at all = accepted, want a refusal")
	}
}

// fixedSource opens, on any thread, a group that reads r.
type fixedSource struct{ r CounterReading }

func (s fixedSource) Open(int) (CounterGroup, error) { return s, nil }

func (fixedSource) Opened() [NumEvents]bool { return [NumEvents]bool{true} }

func (s fixedSource) Read() (CounterReading, error) { return s.r, nil }

func (fixedSource) Close() error { return nil }

// THE EXPORTED PROOF IS THE ONE THAT REFUSES, on every platform: a group that
// never counted is refused everywhere, and one that counted is accepted exactly
// where this platform can count at all.
func TestTheExportedProofRefusesAGroupThatNeverCounted(t *testing.T) {
	t.Parallel()

	// Enabled past any budget a rotation interval the kernel accepts can set.
	never := fixedSource{CounterReading{Enabled: math.MaxInt64}}
	err := ProveCounting(never)
	if err == nil || (CountersSupported && !strings.Contains(err.Error(), "never counted")) {
		t.Fatalf("ProveCounting of a group that never counted = %v, want a refusal that says so", err)
	}
	counted := fixedSource{CounterReading{Enabled: time.Millisecond, Running: time.Millisecond}}
	if err := ProveCounting(counted); (err == nil) != CountersSupported {
		t.Fatalf("ProveCounting of a group that counted = %v, with CountersSupported %v", err, CountersSupported)
	}
}
