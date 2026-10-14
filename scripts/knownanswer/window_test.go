package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// windowedRun is fixtureRun with load windows: the disk job's record carries
// its own load window, and its whole-lease write is what run 9 measured, which
// the median rule fails.
func windowedRun(win *recordWindow) ([]expectation, map[string]record) {
	exps, recs := fixtureRun(9)
	for i := range exps {
		exps[i].LoadStartedAt, exps[i].LoadFinishedAt = "2026-10-13T15:57:00Z", "2026-10-13T15:57:20Z"
	}

	disk := recs["lease-9-disk"]
	u := *disk.Usage
	u.DiskWriteBytes = 90*mib + 1287*mib
	disk.Usage, disk.Window = &u, win
	recs["lease-9-disk"] = disk

	return exps, recs
}

func ptr(v int64) *int64 { return &v }

// loadWindowOf is a window whose edges hold nothing: the whole intervals
// inside it and over it hold what it holds.
func loadWindowOf(written int64) *recordWindow { return boundedWindow(written, written, written) }

// boundedWindow is a window whose whole intervals inside hold inner, whose
// split by time holds split, and whose whole intervals over it hold outer.
func boundedWindow(inner, split, outer int64) *recordWindow {
	return &recordWindow{From: "2026-10-13T15:57:00Z", To: "2026-10-13T15:57:21Z",
		Usage: &windowUsage{SpanMillis: 21_000, CoveredMillis: 21_000, Complete: true, DiskWriteBytes: ptr(split)},
		Inner: &windowSpan{From: "2026-10-13T15:57:00.4Z", To: "2026-10-13T15:57:20.4Z", Usage: windowUsage{
			SpanMillis: 20_000, CoveredMillis: 20_000, Complete: true, DiskWriteBytes: ptr(inner)}},
		Outer: &windowSpan{From: "2026-10-13T15:56:59.4Z", To: "2026-10-13T15:57:21.4Z", Usage: windowUsage{
			SpanMillis: 22_000, CoveredMillis: 22_000, Complete: true, DiskWriteBytes: ptr(outer)}},
	}
}

// THE LOAD WINDOW DECIDES THE DISK VERDICT, with nothing subtracted: the same
// whole-lease figure the median rule fails passes on the bytes written in the
// job's own window, and a window short of the load fails.
func TestTheDiskJobIsJudgedOnItsOwnLoadWindow(t *testing.T) {
	_, without := windowedRun(nil)
	exps, _ := windowedRun(nil)
	if r := resultFor(t, evaluate(exps, fromMap(without)), kindDisk); r.verdict != fail ||
		!strings.Contains(r.metric.rule, "median") {
		t.Fatalf("with no window = %s by %q, want the median rule's FAIL", r.verdict, r.metric.rule)
	}

	for _, tc := range []struct {
		written int64
		want    verdict
	}{
		{2048*mib + 5*mib, pass},
		{2048*mib - 65*mib, fail},
		{2048*mib*110/100 + 65*mib, fail},
	} {
		// The split by time is never what decides: here it alone is inside the
		// tolerance, and the bounds still decide.
		if tc.want == fail {
			exps, recs := windowedRun(boundedWindow(tc.written, 2048*mib, tc.written))
			if r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk); r.verdict != fail {
				t.Errorf("bounds of %d B with a split inside the tolerance = %s, want FAIL", tc.written, r.verdict)
			}
		}

		exps, recs := windowedRun(loadWindowOf(tc.written))
		r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk)
		if r.verdict != tc.want || r.value != float64(tc.written) || !strings.Contains(r.against, "its own load window") ||
			!strings.Contains(r.metric.rule, "load window") {
			t.Errorf("%d B in the window = %s, value %.0f, against %q, rule %q; want %s", tc.written, r.verdict,
				r.value, r.against, r.metric.rule, tc.want)
		}
	}
}

// A WINDOW THAT CANNOT SPEAK FOR THE LOAD IS UNMEASURED, never a verdict: one
// for other seconds, one the series does not cover whole, and one billet
// could not integrate.
func TestAWindowThatIsNotTheLoadsIsUnmeasured(t *testing.T) {
	other := loadWindowOf(2048 * mib)
	other.From = "2026-10-13T15:56:00Z"

	partial := loadWindowOf(2048 * mib)
	partial.Usage.Complete, partial.Usage.CoveredMillis = false, 15_000

	none := &recordWindow{From: "2026-10-13T15:57:00Z", To: "2026-10-13T15:57:21Z",
		Unavailable: "no usage report was recorded"}

	// A burst at an edge: between 1,900 and 2,100 MiB, which the tolerance
	// [1,984, 2,316.8] MiB can neither pass nor fail.
	undecided := boundedWindow(1900*mib, 2048*mib, 2100*mib)

	uncounted := loadWindowOf(2048 * mib)
	uncounted.Usage.DiskWriteBytes = nil

	unbounded := loadWindowOf(2048 * mib)
	unbounded.Outer = nil

	outerPartial := loadWindowOf(2048 * mib)
	outerPartial.Outer.Usage.Complete = false

	outerUncounted := loadWindowOf(2048 * mib)
	outerUncounted.Outer.Usage.DiskWriteBytes = nil

	innerPartial := loadWindowOf(2048 * mib)
	innerPartial.Inner.Usage.Complete = false

	innerUncounted := loadWindowOf(2048 * mib)
	innerUncounted.Inner.Usage.DiskWriteBytes = nil

	// BOUNDS THAT ARE NOT BOUNDS: each of these would otherwise PASS.
	inverted := boundedWindow(3*1024*mib, 2048*mib, 2048*mib)

	elsewhere := loadWindowOf(2048 * mib)
	elsewhere.Outer.From, elsewhere.Outer.To = "2026-10-13T16:00:00Z", "2026-10-13T16:00:22Z"

	innerOutside := loadWindowOf(2048 * mib)
	innerOutside.Inner.From, innerOutside.Inner.To = "2026-10-13T15:56:58Z", "2026-10-13T15:57:18Z"

	unreadableSpan := loadWindowOf(2048 * mib)
	unreadableSpan.Outer.To = "later"

	// A WINDOW BILLET SAYS IT COULD NOT INTEGRATE is not one, whatever it holds.
	disowned := loadWindowOf(2048 * mib)
	disowned.Unavailable = "the usage series could not be read"

	// FIGURES THAT DISAGREE WITH THEMSELVES cover nothing, whatever complete says.
	contradictory := loadWindowOf(2048 * mib)
	contradictory.Usage.CoveredMillis = 0

	outerContradictory := loadWindowOf(2048 * mib)
	outerContradictory.Outer.Usage.SpanMillis = 5_000

	for name, tc := range map[string]struct {
		win  *recordWindow
		want string
	}{
		"another window":      {other, "collect it again"},
		"partial":             {partial, "covers 15000 ms of the 21000 ms"},
		"unavailable":         {none, "no usage report was recorded"},
		"undecided":           {undecided, "cannot decide"},
		"uncounted":           {uncounted, "carries no disk_write_bytes"},
		"unbounded":           {unbounded, "cannot be bounded"},
		"outer partial":       {outerPartial, "cannot be bounded"},
		"outer uncounted":     {outerUncounted, "cannot be bounded"},
		"inner partial":       {innerPartial, "inside the load window"},
		"inner uncounted":     {innerUncounted, "inside the load window"},
		"inverted":            {inverted, "are not bounds"},
		"elsewhere":           {elsewhere, "does not hold the window"},
		"inner outside":       {innerOutside, "is not inside the window"},
		"unreadable span":     {unreadableSpan, "cannot be bounded"},
		"contradictory":       {contradictory, "covers 0 ms"},
		"disowned":            {disowned, "the usage series could not be read"},
		"outer contradictory": {outerContradictory, "cannot be bounded"},
	} {
		exps, recs := windowedRun(tc.win)
		r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk)
		if r.verdict != unmeasured || !strings.Contains(r.reason, tc.want) {
			t.Errorf("%s: %s (%s), want UNMEASURED saying %q", name, r.verdict, r.reason, tc.want)
		}
	}

	exps, recs := windowedRun(loadWindowOf(2048 * mib))
	exps[len(exps)-1].LoadFinishedAt = "not a time"
	if r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk); r.verdict != unmeasured ||
		!strings.Contains(r.reason, "no load window") {
		t.Errorf("an expectation with no load window = %s (%s)", r.verdict, r.reason)
	}
}

// COLLECT ASKS FOR EACH JOB'S LOAD WINDOW, ONE SECOND PAST ITS LAST STAMP,
// keeps the window billet answers, asks a billet that does not know --window
// once more without it, and never retries any other failure.
func TestCollectAsksForTheLoadWindow(t *testing.T) {
	const (
		// predates answers as a billet before --window does: it refuses the
		// flag by name, and answers without it.
		predates = "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$FAKE_LOG\"\n" +
			"case \"$*\" in *--window*) echo 'flag provided but not defined: -window' >&2; exit 2;; esac\n" +
			"for last; do :; done\ncat \"$FAKE_RECORDS/$last.json\"\n"
		// broken fails for the disk lease for another reason.
		broken = "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$FAKE_LOG\"\n" +
			"case \"$*\" in *lease-9-disk*) echo 'alloc: the ledger is busy' >&2; exit 1;; esac\n" +
			"for last; do :; done\ncat \"$FAKE_RECORDS/$last.json\"\n"
	)

	for name, tc := range map[string]struct {
		script    string
		asked     int
		collected bool
	}{
		"knows --window": {fakeBillet, 1, true},
		"predates it":    {predates, 2, true},
		"fails":          {broken, 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bin, records, out := filepath.Join(root, "billet"), filepath.Join(root, "answers"), filepath.Join(root, "out")
			if err := forkSafeWriteFile(bin, []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(records, 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(root, "argv.log")
			t.Setenv("FAKE_RECORDS", records)
			t.Setenv("FAKE_LOG", log)

			exps, recs := windowedRun(loadWindowOf(2048 * mib))
			expDir := t.TempDir()
			writeRun(t, expDir, records, exps, recs)

			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"collect", "--out", out, "--expectations", expDir, "--billet", bin},
				&stdout, &stderr)

			argv, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(argv), "lease-9-disk\n"); got != tc.asked {
				t.Errorf("billet was asked about the disk lease %d times, want %d:\n%s", got, tc.asked, argv)
			}
			if !strings.Contains(string(argv), "jobs show --json --window 2026-10-13T15:57:00Z,2026-10-13T15:57:21Z "+
				"lease-9-disk\n") {
				t.Errorf("billet was not asked for the load window:\n%s", argv)
			}

			if !tc.collected {
				if code != exitFail || !strings.Contains(stdout.String(), "alloc: the ledger is busy") {
					t.Errorf("exit %d, want %d naming billet's error:\n%s", code, exitFail, stdout.String())
				}

				return
			}
			if code != exitPass {
				t.Fatalf("exit %d:\n%s%s", code, stdout.String(), stderr.String())
			}
			rec, err := loadRecord(out, "lease-9-disk")
			if err != nil {
				t.Fatal(err)
			}
			// THE RETRY ASKS WITHOUT THE FLAG, and only after the refusal; the
			// window billet answers is the one kept.
			if retried := strings.Contains(string(argv), "jobs show --json lease-9-disk\n"); retried != (tc.asked == 2) {
				t.Errorf("asked again without --window: %v, want %v:\n%s", retried, tc.asked == 2, argv)
			}
			if rec.Window == nil || rec.Window.From != "2026-10-13T15:57:00Z" {
				t.Errorf("the collected record's window = %+v, want billet's answer kept", rec.Window)
			}
		})
	}
}

// THE WINDOW KEEPS ITS FRACTIONS AND ITS ZONE, both ways: what collect asks
// for is what check compares a record's window with, so a job stamped to the
// millisecond in another zone is judged on its load, not called a mismatch.
func TestTheLoadWindowRoundTripsWithFractionsAndZones(t *testing.T) {
	exps, recs := windowedRun(nil)
	for i := range exps {
		exps[i].LoadStartedAt, exps[i].LoadFinishedAt = "2026-10-13T08:57:00.25-07:00", "2026-10-13T08:57:19.75-07:00"
	}

	if got, want := windowArg(&exps[len(exps)-1]), "2026-10-13T15:57:00.25Z,2026-10-13T15:57:20.75Z"; got != want {
		t.Fatalf("windowArg = %q, want %q", got, want)
	}

	// What billet answers for that request, as its own wallStamp writes it.
	win := loadWindowOf(2048 * mib)
	win.From, win.To = "2026-10-13T15:57:00.25Z", "2026-10-13T15:57:20.75Z"
	win.Usage.SpanMillis, win.Usage.CoveredMillis = 20_500, 20_500
	win.Inner.From, win.Inner.To = "2026-10-13T15:57:00.4Z", "2026-10-13T15:57:20.4Z"
	win.Outer.From, win.Outer.To = "2026-10-13T15:56:59.4Z", "2026-10-13T15:57:21.4Z"
	disk := recs["lease-9-disk"]
	disk.Window = win
	recs["lease-9-disk"] = disk

	if r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk); r.verdict != pass {
		t.Errorf("a fractional, zoned window = %s (%s), want PASS", r.verdict, r.reason)
	}
}

// AN EXPECTATION WHOSE LOAD WINDOW CANNOT BE READ IS UNMEASURED, never judged
// by the median rule as if billet were too old to answer.
func TestAnUnreadableLoadWindowIsUnmeasured(t *testing.T) {
	for name, stamps := range map[string][2]string{
		"malformed": {"2026-10-13T15:57:00Z", "soon"},
		"reversed":  {"2026-10-13T15:57:20Z", "2026-10-13T15:57:00Z"},
		"absent":    {"", ""},
	} {
		exps, recs := windowedRun(nil)
		disk := recs["lease-9-disk"]
		u := *disk.Usage
		u.DiskWriteBytes = 90*mib + 2048*mib
		disk.Usage = &u
		recs["lease-9-disk"] = disk
		exps[len(exps)-1].LoadStartedAt, exps[len(exps)-1].LoadFinishedAt = stamps[0], stamps[1]

		if r := resultFor(t, evaluate(exps, fromMap(recs)), kindDisk); r.verdict != unmeasured ||
			!strings.Contains(r.reason, "cannot be read") {
			t.Errorf("%s: %s (%s), want UNMEASURED", name, r.verdict, r.reason)
		}
	}
}

// COLLECT SAYS SO TOO, and fails, while still collecting the record.
func TestCollectFailsOnAnUnreadableLoadWindow(t *testing.T) {
	root := t.TempDir()
	bin, records, out := filepath.Join(root, "billet"), filepath.Join(root, "answers"), filepath.Join(root, "out")
	if err := forkSafeWriteFile(bin, []byte(fakeBillet), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RECORDS", records)
	t.Setenv("FAKE_LOG", filepath.Join(root, "argv.log"))

	exps, recs := windowedRun(nil)
	exps[len(exps)-1].LoadFinishedAt = "soon"
	expDir := t.TempDir()
	writeRun(t, expDir, records, exps, recs)

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"collect", "--out", out, "--expectations", expDir, "--billet", bin},
		&stdout, &stderr); code != exitFail {
		t.Errorf("exit %d, want %d:\n%s%s", code, exitFail, stdout.String(), stderr.String())
	}
	for _, line := range []string{`lease-9-disk: the expectation's load window "2026-10-13T15:57:00Z" to "soon" cannot be read`,
		"collected 6 of 6", "1 expectations give a load window that cannot be read"} {
		if !strings.Contains(stdout.String(), line) {
			t.Errorf("collect did not say %q:\n%s", line, stdout.String())
		}
	}
}

// networkWindowOf is a window over the network job's load whose whole
// intervals inside it hold inner received bytes and whose whole intervals over
// it hold outer.
func networkWindowOf(inner, outer int64) *recordWindow {
	w := boundedWindow(0, 0, 0)
	w.Usage.DiskWriteBytes, w.Inner.Usage.DiskWriteBytes, w.Outer.Usage.DiskWriteBytes = nil, nil, nil
	w.Usage.NetRxBytes, w.Inner.Usage.NetRxBytes, w.Outer.Usage.NetRxBytes = ptr(inner), ptr(inner), ptr(outer)

	return w
}

// THE NETWORK JOB IS JUDGED ON ITS OWN LOAD WINDOW TOO: every job's setup
// downloads packages, so a whole-lease figure the median rule fails passes on
// what the window received, and the bounds still decide.
func TestTheNetworkJobIsJudgedOnItsOwnLoadWindow(t *testing.T) {
	for _, tc := range []struct {
		inner, outer int64
		want         verdict
	}{
		{100 * mib, 103 * mib, pass},
		{90 * mib, 95 * mib, fail},
		{111 * mib, 120 * mib, fail},
		{94 * mib, 100 * mib, unmeasured},
		{100 * mib, 120 * mib, unmeasured},
	} {
		exps, recs := windowedRun(nil)
		network := recs["lease-9-network"]
		u := *network.Usage
		u.NetRxBytes += 160 * mib
		network.Usage, network.Window = &u, networkWindowOf(tc.inner, tc.outer)
		recs["lease-9-network"] = network

		r := resultFor(t, evaluate(exps, fromMap(recs)), kindNetwork)
		if r.verdict != tc.want || !strings.Contains(r.metric.rule, "load window") {
			t.Errorf("%d to %d B received in the window = %s by %q (%s); want %s", tc.inner, tc.outer,
				r.verdict, r.metric.rule, r.reason, tc.want)
		}
	}

	exps, recs := windowedRun(nil)
	network := recs["lease-9-network"]
	network.Window = loadWindowOf(100 * mib)
	recs["lease-9-network"] = network

	if r := resultFor(t, evaluate(exps, fromMap(recs)), kindNetwork); r.verdict != unmeasured ||
		!strings.Contains(r.reason, "carries no net_rx_bytes") {
		t.Errorf("a window with no received bytes = %s (%s), want UNMEASURED naming net_rx_bytes", r.verdict,
			r.reason)
	}
}
