package guestagent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// stamp is the Unix second of a Worker log time on 2026-10-10.
func stamp(hms string) int64 {
	t, err := time.Parse(time.DateTime, "2026-10-10 "+hms)
	if err != nil {
		return -1
	}

	return t.Unix()
}

// The Worker log of a real job (the probe of 2026-10-10), one step name holding
// quotes and parentheses of its own.
func TestTheStepsAreTheWorkerLogsStepStartLines(t *testing.T) {
	t.Parallel()

	log, _ := discardLog()
	tl := newTailer("testdata/diag", log)
	tl.poll()

	want := []guestreport.StepMark{
		{Name: "Set up runner", AtSeconds: stamp("02:04:17")},
		{Name: "Mark one", AtSeconds: stamp("02:04:20")},
		{Name: `Run it's "quoted" ('twice')`, AtSeconds: stamp("02:04:21")},
		{Name: "Post Run actions/checkout@v5", AtSeconds: stamp("02:04:34")},
	}

	got := tl.take()
	if len(got) != len(want) {
		t.Fatalf("marks %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mark %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAStepLineIsReadExactly(t *testing.T) {
	t.Parallel()

	const prefix = "[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName="

	for _, tc := range []struct {
		line string
		name string
		ok   bool
	}{
		{prefix + "'Mark one'", "Mark one", true},
		{prefix + "'Mark one'\r", "Mark one", true},
		{prefix + "'it's'", "it's", true},
		{prefix + "''", "?", true},
		{prefix + "'a\tb'", "a?b", true},
		{prefix + "'" + strings.Repeat("x", 100) + "'", strings.Repeat("x", guestreport.MaxNameBytes), true},
		{prefix + "'no closing quote", "", false},
		{prefix + "no quote at all", "", false},
		{"[2026-10-10 02:04:20Z INFO StepsRunner] Evaluating: success()", "", false},
		{"[2026-10-10 02:04:20Z INFO JobRunner] Processing step: DisplayName='x'", "", false},
		{"[2026-13-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='x'", "", false},
		{"[1969-12-31 23:59:59Z INFO StepsRunner] Processing step: DisplayName='x'", "", false},
		{"2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='x'", "", false},
		{"[2026", "", false},
	} {
		mark, ok := parseStepLine(tc.line)

		if ok != tc.ok || (ok && (mark.Name != tc.name || mark.AtSeconds != stamp("02:04:20"))) {
			t.Errorf("%q: %+v %v, want %q %v", tc.line, mark, ok, tc.name, tc.ok)
		}
	}
}

// The runner writes its log as it goes: a line not yet finished is held until it
// is, a line longer than any the agent reads is skipped whole, and the log appears
// only once the job starts.
func TestTheTailerHoldsAnUnfinishedLineUntilItIsFinished(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log, _ := discardLog()
	tl := newTailer(filepath.Join(dir, "_diag"), log)

	tl.poll() // no directory yet

	if err := os.Mkdir(filepath.Join(dir, "_diag"), 0o755); err != nil {
		t.Fatal(err)
	}

	tl.poll() // no log yet

	file := filepath.Join(dir, "_diag", "Worker_20261010-020415-utc.log")
	write := func(s string) {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	write("[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='Mark")
	tl.poll()

	if got := tl.take(); len(got) != 0 {
		t.Fatalf("an unfinished line made marks %+v", got)
	}

	write(" one'\n[2026-10-10 02:04:21Z INFO StepsRunner] Processing step: DisplayName='" +
		strings.Repeat("x", stepLineLimit))
	tl.poll()

	if len(tl.partial) > stepLineLimit || !tl.skipping {
		t.Fatalf("an unfinished line past the bound is held at %d bytes (skipping %v)", len(tl.partial), tl.skipping)
	}
	write(strings.Repeat("y", 10) + "'\n[2026-10-10 02:04:22Z INFO StepsRunner] Processing step: DisplayName='Two'\n")
	tl.poll()

	got := tl.take()
	if len(got) != 2 || got[0].Name != "Mark one" || got[1].Name != "Two" {
		t.Fatalf("marks %+v, want Mark one and Two, the overlong line skipped whole", got)
	}
}

// A newer Worker log replaces the one followed, which is then read from its start.
func TestANewerWorkerLogIsFollowedFromItsStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	line := func(hms, name string) string {
		return "[2026-10-10 " + hms + "Z INFO StepsRunner] Processing step: DisplayName='" + name + "'\n"
	}

	writeFile(t, filepath.Join(dir, "Worker_20261010-020415-utc.log"), line("02:04:20", "first"))
	writeFile(t, filepath.Join(dir, "Runner_20261010-020400-utc.log"), line("02:04:59", "the listener's"))

	log, _ := discardLog()
	tl := newTailer(dir, log)
	tl.poll()

	writeFile(t, filepath.Join(dir, "Worker_20261010-020530-utc.log"), line("02:05:31", "second"))
	tl.poll()

	got := tl.take()
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("marks %+v, want first then second", got)
	}
}

// A mark earlier than one already kept is dropped: the node refuses steps that go
// back.
func TestAStepMarkThatGoesBackIsDropped(t *testing.T) {
	t.Parallel()

	log, logged := discardLog()
	tl := newTailer("", log)

	for _, l := range []string{
		"[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='a'",
		"[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='b'",
		"[2026-10-10 02:04:19Z INFO StepsRunner] Processing step: DisplayName='c'",
		"[2026-10-10 02:04:21Z INFO StepsRunner] Processing step: DisplayName='d'",
	} {
		tl.line(l)
	}

	got := tl.take()
	if len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "d" {
		t.Fatalf("marks %+v, want a, b and d", got)
	}

	if !strings.Contains(logged.String(), "earlier than the last") {
		t.Errorf("the drop was not logged: %q", logged.String())
	}
}

// A batch carries at most MaxBatchSteps marks; the rest wait for the next, and the
// marks held are bounded by what a report keeps.
func TestStepMarksAreTakenABatchsWorthAtATime(t *testing.T) {
	t.Parallel()

	log, _ := discardLog()
	tl := newTailer("", log)

	for range pendingStepLimit + 10 {
		tl.line("[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='s'")
	}

	if len(tl.pending) != pendingStepLimit {
		t.Fatalf("held %d marks, want %d", len(tl.pending), pendingStepLimit)
	}

	if got := tl.take(); len(got) != guestreport.MaxBatchSteps || len(tl.pending) != pendingStepLimit-guestreport.MaxBatchSteps {
		t.Fatalf("took %d and left %d", len(got), len(tl.pending))
	}
}

// The job is root in its VM and can put anything at a log's name: a FIFO there is
// refused at once rather than waited on, and a symlink is not followed.
func TestAWorkerLogThatIsNotARegularFileIsNotRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "Worker_20261010-020415-utc.log"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	log, logged := discardLog()
	tl := newTailer(dir, log)

	done := make(chan struct{})

	go func() {
		defer close(done)

		tl.poll()
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		t.Fatal("a poll waited on a FIFO")
	}

	if !strings.Contains(logged.String(), "was not opened") || tl.name != "Worker_20261010-020415-utc.log" {
		t.Fatalf("the FIFO was not the log refused (following %q): %q", tl.name, logged.String())
	}

	target := filepath.Join(t.TempDir(), "elsewhere.log")
	writeFile(t, target, "[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='through a link'\n")

	if err := os.Symlink(target, filepath.Join(dir, "Worker_20261010-020416-utc.log")); err != nil {
		t.Fatal(err)
	}

	tl.poll()

	if tl.name != "Worker_20261010-020416-utc.log" {
		t.Fatalf("following %q, want the symlink's name, the newest", tl.name)
	}

	if got := tl.take(); len(got) != 0 {
		t.Fatalf("read marks %+v through a symlink", got)
	}

	if !strings.Contains(logged.String(), "was not opened") {
		t.Errorf("the refusal was not logged: %q", logged.String())
	}

	if strings.Contains(logged.String(), "Worker_") {
		t.Errorf("the log quotes a name the job chose: %q", logged.String())
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()

	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
