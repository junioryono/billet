// Package flightrecorder keeps the last two minutes of the process's execution
// trace in memory and writes them to a file when something has already gone
// wrong, so a stall leaves evidence of what every goroutine was doing.
//
// The runtime allows one flight recorder per process; this package is where it
// is started. It writes only under the directory it was given, names every file
// it writes, and removes only files with names it could have written.
package flightrecorder

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
)

// Reason names what a snapshot was taken for. The set is closed: a reason is
// part of a file name.
type Reason string

const (
	// HeartbeatOverrun is a heartbeat pass still running when the next was due.
	HeartbeatOverrun Reason = "heartbeat-overrun"
	// LeadershipLost is the ledger refusing a write because another controller
	// took the deployment.
	LeadershipLost Reason = "leadership-lost"
)

const (
	// Window is how far back a snapshot reaches. Four default heartbeat
	// intervals, so a pass that overran is covered from before it began.
	Window = 2 * time.Minute
	// windowBytes caps the window's memory, and so roughly each file's size.
	// The runtime treats it as a hint.
	windowBytes = 32 << 20
	// Every is the least time between two snapshots for one reason. A stall
	// that repeats every pass leaves one file per Every, not one per pass.
	Every = 10 * time.Minute
	// Keep is how many snapshots the directory holds; the oldest go first.
	Keep = 4
	// stopWait bounds how long Stop waits for a snapshot being written. A
	// stopping controller holds its claim until it exits, and evidence is not
	// worth keeping a successor waiting on a hung disk.
	stopWait = 10 * time.Second
	// DirName is the directory the control plane records into, under its
	// identity directory.
	DirName = "flight-recorder"

	filePrefix = "flight-"
	fileSuffix = ".trace"
	// stampLayout sorts lexically in time order.
	stampLayout = "20060102T150405.000Z"
)

// window is the runtime's flight recorder, as this package uses it.
type window interface {
	Start() error
	Stop()
	WriteTo(w io.Writer) (int64, error)
}

// Recorder holds the runtime's flight recorder and writes its window on
// request. A nil Recorder does nothing, so a caller with the recorder off holds
// a nil one rather than a branch.
type Recorder struct {
	dir    string
	log    *slog.Logger
	window window
	now    func() time.Time
	// stopWait is the constant of that name, a field so a test can shorten it.
	stopWait time.Duration

	mu      sync.Mutex
	last    map[Reason]time.Time
	stopped bool

	// writing serialises the snapshots: the runtime refuses a second WriteTo
	// while one runs, and two reasons close together should both be written.
	writing sync.Mutex
	writes  sync.WaitGroup
}

// Start makes dir, a directory of its own, and starts recording.
func Start(dir string, log *slog.Logger) (*Recorder, error) {
	return start(dir, log, trace.NewFlightRecorder(trace.FlightRecorderConfig{
		MinAge: Window, MaxBytes: windowBytes,
	}), time.Now)
}

func start(dir string, log *slog.Logger, w window, now func() time.Time) (*Recorder, error) {
	// DURABLY, its entry in the identity directory included, or a crash could
	// take the directory and every snapshot installed in it.
	if err := (durablefile.Installer{}).MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("flight recorder: %w", err)
	}

	// A DIRECTORY, NOT A LINK TO ONE: a snapshot lands where the configuration
	// says, and pruning removes files from no other directory.
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("flight recorder: inspect %s: %w", dir, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("flight recorder: %s is not a directory", dir)
	}

	if err := w.Start(); err != nil {
		return nil, fmt.Errorf("flight recorder: start: %w", err)
	}

	return &Recorder{
		dir: dir, log: log, window: w, now: now, stopWait: stopWait, last: map[Reason]time.Time{},
	}, nil
}

// Snapshot writes the window to a file for reason, on a goroutine of its own,
// unless a snapshot for the same reason was taken less than Every ago. It
// returns at once: the callers are a stalled loop and a process being stopped.
func (r *Recorder) Snapshot(reason Reason) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	at := r.now()

	if r.stopped {
		return
	}

	if last, ok := r.last[reason]; ok && at.Sub(last) < Every {
		return
	}

	r.last[reason] = at

	r.writes.Go(func() { r.write(reason, at) })
}

// write installs one snapshot and prunes the directory to Keep.
func (r *Recorder) write(reason Reason, at time.Time) {
	r.writing.Lock()
	defer r.writing.Unlock()

	name := filePrefix + at.UTC().Format(stampLayout) + "-" + string(reason) + fileSuffix

	path, err := durablefile.Installer{}.Install(r.dir, name, 0o600, func(w io.Writer) error {
		_, err := r.window.WriteTo(w)

		return err
	})
	if err != nil {
		r.log.Error("flight recorder: could not write a snapshot", "reason", string(reason), "error", err)

		return
	}

	r.log.Warn("flight recorder: wrote the last moments of this process's execution trace; "+
		"read it with `go tool trace`", "reason", string(reason), "path", path)

	if err := r.prune(); err != nil {
		r.log.Error("flight recorder: could not remove old snapshots", "dir", r.dir, "error", err)
	}
}

// prune removes all but the newest Keep snapshots. Only regular files whose
// names this package writes are counted or removed.
func (r *Recorder) prune() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return err
	}

	var snapshots []string

	for _, e := range entries {
		if e.Type().IsRegular() && isSnapshotName(e.Name()) {
			snapshots = append(snapshots, e.Name())
		}
	}

	if len(snapshots) <= Keep {
		return nil
	}

	slices.Sort(snapshots)

	var errs []error

	for _, name := range snapshots[:len(snapshots)-Keep] {
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%d of %d: %w", len(errs), len(snapshots)-Keep, errs[0])
	}

	return nil
}

// isSnapshotName reports whether name is one write could have chosen.
func isSnapshotName(name string) bool {
	rest, ok := strings.CutPrefix(name, filePrefix)
	if !ok {
		return false
	}

	rest, ok = strings.CutSuffix(rest, fileSuffix)
	if !ok || len(rest) <= len(stampLayout)+1 || rest[len(stampLayout)] != '-' {
		return false
	}

	// THE NAME AS WRITE WOULD SPELL IT, not merely one the parser accepts: it
	// takes a comma for the decimal point, and a file named that way is not one
	// this package wrote.
	stamp := rest[:len(stampLayout)]

	at, err := time.Parse(stampLayout, stamp)
	if err != nil || at.UTC().Format(stampLayout) != stamp {
		return false
	}

	switch Reason(rest[len(stampLayout)+1:]) {
	case HeartbeatOverrun, LeadershipLost:
		return true
	default:
		return false
	}
}

// Stop waits for the snapshots being written, then stops recording. A
// Snapshot after Stop does nothing.
//
// THE WAIT IS BOUNDED, AND IT BOUNDS THE RUNTIME'S STOP TOO. A snapshot still
// being written after stopWait is left to the process's exit; so is stopping
// the runtime's recorder, which waits for that same write and can wait behind
// another consumer of the runtime's trace, a profiler client that stopped
// reading among them.
func (r *Recorder) Stop() {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()

	stopped := make(chan struct{})

	go func() {
		r.writes.Wait()
		r.window.Stop()
		close(stopped)
	}()

	bound := time.NewTimer(r.stopWait)
	defer bound.Stop()

	select {
	case <-stopped:
	case <-bound.C:
		r.log.Error("flight recorder: still writing a snapshot or stopping the recorder when the "+
			"process stopped; it is left unfinished", "dir", r.dir, "waited", r.stopWait)
	}
}
