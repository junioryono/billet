package ceph

import (
	"bufio"
	"context"
	"os"
	"slices"
	"strings"
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

// THE IMPORT WRITES NO FASTER THAN ITS RATE (#394), and every byte arrives. An
// unpaced 80 GiB copy went through the page cache at full speed onto a saturated
// cluster; paced, the bytes due at any moment are what the rate allows.
func TestTheImportWritesAtItsRate(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("0123456789abcdef", 5<<20/16) // 5 MiB
	raw, device := stageRaw(t, content)

	clock := time.Unix(0, 0)
	var slept time.Duration
	pace := importPace{
		rate:       1 << 20,
		flushEvery: 2 << 20,
		now:        func() time.Time { return clock },
		sleep: func(d time.Duration) {
			slept += d
			clock = clock.Add(d)
		},
	}

	if err := writeImage(raw, device, int64(len(content)), pace); err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	got, err := os.ReadFile(device)
	if err != nil {
		t.Fatalf("read the device: %v", err)
	}
	if string(got) != content {
		t.Fatalf("the device holds %d bytes that are not the image's %d", len(got), len(content))
	}
	if slept < 5*time.Second-time.Millisecond || slept > 5*time.Second+time.Millisecond {
		t.Errorf("5 MiB at 1 MiB/s waited %v, want 5s", slept)
	}
}
