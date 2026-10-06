package ceph

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// THE PURGE READS `full avg10`, and a file it cannot read paces nothing.
func TestTheIOPressureIsReadFromTheFullLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		body  string
		want  float64
		known bool
	}{
		{body: "some avg10=14.19 avg60=15.36 avg300=18.84 total=1\nfull avg10=12.94 avg60=13.94 avg300=17.47 total=2\n",
			want: 12.94, known: true},
		{body: "some avg10=3.00 avg60=1.00 avg300=1.00 total=1\n"},
		{body: "full avg10=nope avg60=1.00\n"},
		{body: ""},
	} {
		got, known := parseIOPressure(bufio.NewScanner(strings.NewReader(tc.body)))
		if got != tc.want || known != tc.known {
			t.Errorf("%q read as %v, %v; want %v, %v", tc.body, got, known, tc.want, tc.known)
		}
	}
}

// A PURGE DELETION WAITS WHILE THE HOST IS STARVED FOR IO, AND NOT FOREVER (#394).
// Every deletion asks first; a starved host holds it for at most purgeQuietMax,
// so a host that is always busy still purges, and a quiet one or one that cannot
// tell is not held at all.
func TestAPurgeDeletionWaitsOutAStarvedHostForABoundedTime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		pressure float64
		known    bool
		pauses   int
	}{
		{name: "starved", pressure: 40, known: true, pauses: int(purgeQuietMax / purgeQuietPoll)},
		{name: "quiet", pressure: 1, known: true},
		{name: "cannot tell", pressure: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			var (
				pauses    int
				beforeRm  []int
				deletions int
			)
			run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
				if slices.Contains(args, "trash") && slices.Contains(args, "rm") {
					beforeRm = append(beforeRm, pauses)
					deletions++
				}

				return f.run(ctx, bin, args)
			}
			c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
				withRunner(run), withIOPressure(
					func() (float64, bool) { return tc.pressure, tc.known },
					func(context.Context, time.Duration) bool { pauses++; return true }))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			f.trash["id-a"] = "cache-v-1790048184-0123456789abcdef01234567"
			f.trash["id-b"] = "cache-v-1790048185-0123456789abcdef01234567"

			if _, err := c.PurgeTrash(t.Context()); err != nil {
				t.Fatalf("PurgeTrash: %v", err)
			}

			if deletions != 2 {
				t.Fatalf("%d deletions ran, want both", deletions)
			}
			// EACH DELETION WAITED ITS OWN TURN, before it ran.
			if want := []int{tc.pauses, 2 * tc.pauses}; !slices.Equal(beforeRm, want) {
				t.Errorf("the deletions ran after %v pauses, want %v", beforeRm, want)
			}
		})
	}
}

// AND A FAKE RUNNER'S PURGE READS NOTHING OF THE MACHINE RUNNING THE SUITE.
func TestAFakeRunnerReadsNoHostPressure(t *testing.T) {
	t.Parallel()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(newCacheFake().run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.ioPressure != nil {
		t.Error("a client with a fake runner would pace its purge on the test host's IO")
	}

	production, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if production.ioPressure == nil || production.pause == nil {
		t.Error("a production client does not pace its purge")
	}
}

// paceRecorder is pacedCopy's destination in a test: it keeps what was written
// and the order of writes, syncs and sleeps, and can stall the clock on a write.
type paceRecorder struct {
	data    []byte
	events  []string
	clock   *time.Time
	stallOn int
	stall   time.Duration
	writes  int
}

func (r *paceRecorder) Write(p []byte) (int, error) {
	r.writes++
	r.data = append(r.data, p...)
	r.events = append(r.events, "write "+strconv.Itoa(len(p)))
	if r.writes == r.stallOn {
		*r.clock = r.clock.Add(r.stall)
	}

	return len(p), nil
}

func (r *paceRecorder) Sync() error {
	r.events = append(r.events, "sync")

	return nil
}

// THE IMPORT WRITES NO FASTER THAN ITS RATE, FLUSHES AS IT GOES, AND BANKS NO TIME
// (#394). An unpaced 80 GiB copy went through the page cache at full speed onto a
// saturated cluster. Every write is followed by its own wait, no more than
// flushEvery bytes go between syncs, the final partial chunk arrives, and a write
// that stalls the clock is not made up afterwards with an unpaced burst.
func TestTheImportWritesAtItsRateAndFlushesAsItGoes(t *testing.T) {
	t.Parallel()

	const (
		chunk      = 1 << 20
		rate       = 1 << 20
		flushEvery = 2 << 20
	)
	content := strings.Repeat("0123456789abcdef", (5<<20)/16) + "tail"

	clock := time.Unix(0, 0)
	recorder := &paceRecorder{clock: &clock, stallOn: 2, stall: 30 * time.Second}
	var sleeps []time.Duration
	pace := importPace{
		rate: rate, flushEvery: flushEvery, chunk: chunk,
		now: func() time.Time { return clock },
		sleep: func(d time.Duration) {
			sleeps = append(sleeps, d)
			recorder.events = append(recorder.events, "sleep")
			clock = clock.Add(d)
		},
	}

	written, err := pacedCopy(recorder, strings.NewReader(content), pace)
	if err != nil {
		t.Fatalf("pacedCopy: %v", err)
	}
	if written != int64(len(content)) || string(recorder.data) != content {
		t.Fatalf("wrote %d bytes that are not the image's %d", written, len(content))
	}

	// FLUSHED AS IT GOES: never more than flushEvery unsynced.
	var unsynced, syncs int
	for _, event := range recorder.events {
		switch {
		case event == "sync":
			syncs++
			unsynced = 0
		case strings.HasPrefix(event, "write "):
			n, err := strconv.Atoi(strings.TrimPrefix(event, "write "))
			if err != nil {
				t.Fatalf("the recorder logged %q, which names no byte count: %v", event, err)
			}
			unsynced += n
			if unsynced > flushEvery {
				t.Fatalf("%d bytes were written without a sync: %v", unsynced, recorder.events)
			}
		}
	}
	if syncs < 2 {
		t.Errorf("a 5 MiB copy synced %d times every 2 MiB", syncs)
	}

	// PACED AS IT GOES: no two writes without a wait between them, except across
	// the stalled write, whose lost time is not owed back.
	for i := 1; i < len(recorder.events); i++ {
		if strings.HasPrefix(recorder.events[i], "write ") && strings.HasPrefix(recorder.events[i-1], "write ") {
			t.Errorf("two writes ran with no wait between them: %v", recorder.events)
		}
	}

	// EXACTLY A CHUNK'S WORTH AFTER EACH WRITE, AND NO BANKED TIME. The first chunk
	// waits its second; the stalled one is already a second late and waits
	// nothing; every chunk after it waits its own second again, where an average
	// over the whole copy would have skipped them until it caught up; the tail
	// waits its four bytes' share.
	perByte := float64(time.Second) / float64(rate)
	second := time.Duration(float64(chunk) * perByte)
	tailBytes := len(content) % chunk
	tail := time.Duration(float64(tailBytes) * perByte)
	want := []time.Duration{second, second, second, second, tail}
	if len(sleeps) != len(want) {
		t.Fatalf("the waits were %v, want %v: %v", sleeps, want, recorder.events)
	}
	for i := range want {
		if diff := sleeps[i] - want[i]; diff > time.Microsecond || diff < -time.Microsecond {
			t.Errorf("wait %d was %v, want %v", i, sleeps[i], want[i])
		}
	}
}

// AND THE IMPORT USES IT: writeImage paces the real device write.
func TestWriteImagePacesTheDeviceWrite(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", 3<<20)
	raw, device := stageRaw(t, content)

	clock := time.Unix(0, 0)
	var slept time.Duration
	pace := importPace{
		rate: 1 << 20, flushEvery: 1 << 20, chunk: 1 << 20,
		now: func() time.Time { return clock },
		sleep: func(d time.Duration) {
			slept += d
			clock = clock.Add(d)
		},
	}

	if err := writeImage(raw, device, int64(len(content)), pace); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	got, err := os.ReadFile(device)
	if err != nil || string(got) != content {
		t.Fatalf("the device does not hold the image: %v", err)
	}
	if slept < 3*time.Second-time.Millisecond || slept > 3*time.Second+time.Millisecond {
		t.Errorf("3 MiB at 1 MiB/s waited %v, want 3s", slept)
	}
}

// A PASS YIELDS FOR AT MOST purgePassYieldMax, SHARED BY ITS WORKERS, AND STILL
// DELETES EVERYTHING (#394). A backlog on a host that never quietens must not
// drain at a few deletions a minute while jobs discard more and the pool fills.
func TestAPurgePassYieldsForABoundedTimeAcrossItsWorkers(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	var (
		mu        sync.Mutex
		deletions int
		pauses    atomic.Int64
	)
	// SERIALISED, because the fake is not safe for concurrent calls and four
	// workers make them.
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if slices.Contains(args, "trash") && slices.Contains(args, "rm") {
			deletions++
		}

		return f.run(ctx, bin, args)
	}
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(run), withPurgeWorkers(4), withIOPressure(
			func() (float64, bool) { return 40, true },
			func(context.Context, time.Duration) bool { pauses.Add(1); return true }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const backlog = 12
	for i := range backlog {
		f.trash[fmt.Sprintf("id-%02d", i)] = fmt.Sprintf("cache-v-%d-0123456789abcdef01234567", 1790048184+i)
	}

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}

	if deletions != backlog {
		t.Fatalf("%d of %d deletions ran", deletions, backlog)
	}
	// THE WHOLE PASS'S BUDGET, ONCE, not one for each worker or each deletion:
	// twelve deletions at two minutes each would ask for 96 pauses.
	if got, want := pauses.Load(), int64(purgePassYieldMax/purgeQuietPoll); got != want {
		t.Errorf("the pass paused %d times, want its budget's %d", got, want)
	}
}

// AND THE HALF-REMOVED FINISHES WAIT ON THE SAME BUDGET (#394). On the pass that
// finishes them, four trash deletions spend 32 of the budget's 40 pauses, the
// first half-removed image waits the remaining 8, and the second none: a phase
// that asked nothing, or kept a budget of its own, pauses 32 or 48 times.
func TestHalfRemovedFinishesShareThePassYieldBudget(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	var (
		pauses   int
		beforeRm []int
	)
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") && !slices.ContainsFunc(args, func(arg string) bool {
			return arg == "trash" || arg == "snap" || arg == "lock"
		}) {
			beforeRm = append(beforeRm, pauses)
		}

		return f.run(ctx, bin, args)
	}
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(run), withIOPressure(
			func() (float64, bool) { return 40, true },
			func(context.Context, time.Duration) bool { pauses++; return true }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour).Unix()
	for _, image := range []string{
		fmt.Sprintf("billet-cache/cache-v-%d-0123456789abcdef01234567", old),
		fmt.Sprintf("billet-cache/cache-g-%d-0123456789abcdef01234567", old),
	} {
		f.halfRemoved[image] = true
	}

	// THE FIRST SIGHTING ONLY RECORDS, with nothing in the trash to delete.
	clock := time.Now()
	c.clock = func() time.Time { return clock }
	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("first PurgeTrash: %v", err)
	}
	if pauses != 0 {
		t.Fatalf("a pass that deleted nothing paused %d times", pauses)
	}

	clock = clock.Add(2 * halfRemovedRecheck)
	for i := range 4 {
		f.trash[fmt.Sprintf("id-%d", i)] = fmt.Sprintf("cache-v-%d-0123456789abcdef01234567", 1790048184+i)
	}

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("second PurgeTrash: %v", err)
	}

	budget := int(purgePassYieldMax / purgeQuietPoll)
	if pauses != budget {
		t.Errorf("the pass paused %d times, want its one budget's %d", pauses, budget)
	}
	if want := []int{budget, budget}; !slices.Equal(beforeRm, want) {
		t.Errorf("the half-removed finishes ran after %v pauses, want %v", beforeRm, want)
	}
}
