package flightrecorder

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWindow stands in for the runtime's recorder, which allows one per
// process. WriteTo writes its content, and blocks while gate is set and open.
type fakeWindow struct {
	mu       sync.Mutex
	started  bool
	stopped  bool
	content  string
	gate     chan struct{}
	entered  chan struct{}
	writeErr error
	// stopGate, when set, holds Stop until it is closed.
	stopGate chan struct{}
	stops    int
}

func (w *fakeWindow) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.started = true

	return nil
}

func (w *fakeWindow) Stop() {
	// COUNTED ON ENTRY, so two stops at once are two even while both wait.
	w.mu.Lock()
	w.stops++
	w.mu.Unlock()

	if w.stopGate != nil {
		<-w.stopGate
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.stopped = true
}

func (w *fakeWindow) WriteTo(out io.Writer) (int64, error) {
	if w.entered != nil {
		w.entered <- struct{}{}
	}

	if w.gate != nil {
		<-w.gate
	}

	if w.writeErr != nil {
		return 0, w.writeErr
	}

	n, err := io.WriteString(out, w.content)

	return int64(n), err
}

// clock is a time a test moves by hand.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
}

var epoch = time.Date(2026, 10, 8, 15, 4, 5, 123_000_000, time.UTC)

func startFake(t *testing.T, dir string, w *fakeWindow) (*Recorder, *clock) {
	t.Helper()

	c := &clock{at: epoch}

	r, err := start(dir, slog.New(slog.DiscardHandler), w, c.now)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(r.Stop)

	return r, c
}

// snapshots lists the directory's entries by name.
func snapshots(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// A SNAPSHOT IS THE WINDOW, under a name that says when and why, readable by
// the service account alone.
func TestASnapshotIsWrittenUnderItsTimeAndReason(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "flight-recorder")
	w := &fakeWindow{content: "the window"}
	r, _ := startFake(t, dir, w)

	r.Snapshot(HeartbeatOverrun)
	r.Stop()

	want := "flight-20261008T150405.123Z-heartbeat-overrun.trace"

	if got := snapshots(t, dir); !slices.Equal(got, []string{want}) {
		t.Fatalf("the directory holds %q, want %q", got, want)
	}

	body, err := os.ReadFile(filepath.Join(dir, want))
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "the window" {
		t.Errorf("the snapshot holds %q, want the window", body)
	}

	info, err := os.Stat(filepath.Join(dir, want))
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the snapshot's mode is %v, want 0600", mode)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("the directory's mode is %v, want 0700", mode)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.started || !w.stopped {
		t.Errorf("the window was started %v and stopped %v, want both", w.started, w.stopped)
	}
}

// ONE SNAPSHOT PER REASON PER Every: a stall that repeats every pass leaves
// one file, and another reason inside the same interval is still written.
func TestOneReasonIsWrittenOncePerInterval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, c := startFake(t, dir, &fakeWindow{content: "w"})

	r.Snapshot(HeartbeatOverrun)
	c.advance(time.Minute)
	r.Snapshot(HeartbeatOverrun)
	r.Snapshot(LeadershipLost)
	c.advance(Every - time.Minute - time.Millisecond)
	r.Snapshot(HeartbeatOverrun)
	c.advance(time.Millisecond)
	r.Snapshot(HeartbeatOverrun)
	r.Stop()

	want := []string{
		"flight-20261008T150405.123Z-heartbeat-overrun.trace",
		"flight-20261008T150505.123Z-leadership-lost.trace",
		"flight-20261008T151405.123Z-heartbeat-overrun.trace",
	}

	if got := snapshots(t, dir); !slices.Equal(got, want) {
		t.Errorf("the directory holds\n%q\nwant\n%q", got, want)
	}
}

// ONLY THE NEWEST Keep REMAIN, and nothing the recorder could not have written
// is counted or removed: another file, a directory, a staging file and a name
// that is nearly a snapshot's all stay.
func TestOnlyTheNewestSnapshotsRemainAndNothingElseIsRemoved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	older := []string{
		"flight-20261001T000000.000Z-heartbeat-overrun.trace",
		"flight-20261002T000000.000Z-leadership-lost.trace",
		"flight-20261003T000000.000Z-heartbeat-overrun.trace",
		"flight-20261004T000000.000Z-heartbeat-overrun.trace",
	}

	foreign := []string{
		"notes.txt",
		"flight-20261001T000000.000Z-something-else.trace",
		"flight-garbage.trace",
		"flight-20261001T000000,000Z-heartbeat-overrun.trace",
		"flight-20261001T000000.000Z-heartbeat-overrun.trace.bak",
		".durable-123",
	}

	for _, name := range slices.Concat(older, foreign) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	nested := "flight-20260930T000000.000Z-heartbeat-overrun.trace"
	if err := os.Mkdir(filepath.Join(dir, nested), 0o700); err != nil {
		t.Fatal(err)
	}

	r, _ := startFake(t, dir, &fakeWindow{content: "w"})

	r.Snapshot(LeadershipLost)
	r.Stop()

	want := slices.Concat(foreign, older[1:], []string{nested, "flight-20261008T150405.123Z-leadership-lost.trace"})
	slices.Sort(want)

	if got := snapshots(t, dir); !slices.Equal(got, want) {
		t.Errorf("the directory holds\n%q\nwant\n%q", got, want)
	}
}

// SNAPSHOT RETURNS AT ONCE, even while another snapshot is being written, and
// Stop waits for every snapshot already asked for.
func TestASnapshotNeverHoldsItsCaller(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &fakeWindow{content: "w", gate: make(chan struct{}), entered: make(chan struct{}, 2)}
	r, _ := startFake(t, dir, w)

	released := false
	t.Cleanup(func() {
		if !released {
			close(w.gate)
		}
	})

	returned := make(chan struct{})

	go func() {
		r.Snapshot(HeartbeatOverrun)
		r.Snapshot(LeadershipLost)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot waited for the window to be written")
	}

	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot began writing")
	}

	stopped := make(chan struct{})

	go func() {
		r.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a snapshot was still being written")
	case <-time.After(100 * time.Millisecond):
	}

	released = true
	close(w.gate)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned once the snapshots were written")
	}

	if got := snapshots(t, dir); len(got) != 2 {
		t.Errorf("the directory holds %q, want both snapshots", got)
	}
}

// STOP IS BOUNDED: a snapshot whose write never finishes holds Stop for its
// bound and no longer, and the window is left running rather than stopped
// under it.
func TestStopDoesNotWaitForeverForAWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &fakeWindow{content: "w", gate: make(chan struct{}), entered: make(chan struct{}, 1)}

	r, _ := startFake(t, dir, w)
	r.stopWait = 50 * time.Millisecond

	t.Cleanup(func() { close(w.gate) })

	r.Snapshot(HeartbeatOverrun)

	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot began writing")
	}

	stopped := make(chan struct{})

	go func() {
		r.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited past its bound for a write that never finishes")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stopped {
		t.Error("the window was stopped under a write still in progress")
	}
}

// AND A RUNTIME RECORDER THAT WILL NOT STOP holds Stop for the same bound and
// no longer, with nothing being written.
func TestStopDoesNotWaitForeverForTheRecorderToStop(t *testing.T) {
	t.Parallel()

	w := &fakeWindow{stopGate: make(chan struct{})}

	r, _ := startFake(t, t.TempDir(), w)
	r.stopWait = 50 * time.Millisecond

	var opened atomic.Bool

	t.Cleanup(func() {
		if opened.CompareAndSwap(false, true) {
			close(w.stopGate)
		}
	})

	stopped := make(chan struct{})

	go func() {
		r.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited past its bound for a recorder that would not stop")
	}

	// A SECOND STOP STARTS NO SECOND STOP OF THE RUNTIME'S, which is not safe
	// to run twice at once: once the recorder lets go, it was stopped once.
	r.Stop()

	if opened.CompareAndSwap(false, true) {
		close(w.stopGate)
	}

	r.stopWait = 5 * time.Second
	r.Stop()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stops != 1 {
		t.Errorf("three Stops stopped the runtime's recorder %d times, want once", w.stops)
	}
}

// A SNAPSHOT THAT CANNOT BE WRITTEN LEAVES NOTHING, not even a staging file.
func TestASnapshotThatFailsLeavesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, _ := startFake(t, dir, &fakeWindow{writeErr: errors.New("the runtime refused")})

	r.Snapshot(HeartbeatOverrun)
	r.Stop()

	if got := snapshots(t, dir); len(got) != 0 {
		t.Errorf("a failed snapshot left %q", got)
	}
}

// AFTER STOP, A SNAPSHOT IS NOTHING; and a nil recorder, which is the
// recorder switched off, does nothing either.
func TestASnapshotAfterStopAndANilRecorderDoNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, _ := startFake(t, dir, &fakeWindow{content: "w"})

	r.Stop()
	r.Snapshot(HeartbeatOverrun)

	if got := snapshots(t, dir); len(got) != 0 {
		t.Errorf("a snapshot after Stop wrote %q", got)
	}

	var off *Recorder

	off.Snapshot(LeadershipLost)
	off.Stop()
}

// THE DIRECTORY IS A DIRECTORY, not a link to one and not a file.
func TestTheDirectoryIsRefusedWhenItIsNotOne(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{link, file} {
		w := &fakeWindow{}

		if _, err := start(dir, slog.New(slog.DiscardHandler), w, time.Now); err == nil {
			t.Errorf("%s was accepted as the recorder's directory", dir)
		}

		if w.started {
			t.Errorf("the window was started for a refused directory %s", dir)
		}
	}
}

// THE RUNTIME'S RECORDER WRITES A TRACE: the one test that starts it, since a
// process may hold one at a time.
func TestTheRuntimesRecorderWritesATrace(t *testing.T) {
	dir := t.TempDir()

	r, err := Start(dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	r.Snapshot(LeadershipLost)
	r.Stop()

	names := snapshots(t, dir)
	if len(names) != 1 {
		t.Fatalf("the directory holds %q, want one snapshot", names)
	}

	body, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.HasPrefix(body, []byte("go 1.")) || len(body) <= 16 {
		t.Errorf("the snapshot is not an execution trace: %d bytes beginning %q", len(body), body[:min(len(body), 16)])
	}
}
