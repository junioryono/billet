// Package nodeending is the node's own record of how each of its processes
// ended, and the judgement of a stop and of a start that reads it (#127).
//
// A SERVICE MANAGER'S EXIT IS NOT A DRAIN. The node exits 0 after a real drain,
// after handing its compute to the next process, after a drain a second signal
// cut short, and when superseded; and systemd's own account of a stop is lost to
// `reset-failed`, unit garbage collection and the next start. Measured on
// systemd 255 (2026-10-17): a stop killed at TimeoutStopSec leaves
// failed/failed/timeout, `reset-failed` turns that into inactive/dead/success,
// and a clean stop clears InvocationID. So the node writes its ending itself, in
// a file only it and an operator acknowledging a stop write, and a stop is proved
// by that record rather than by an exit status.
//
// A leaf: the standard library, durablefile and regularfile, nothing else of
// billet's.
package nodeending

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// Ending is how one node process ended.
type Ending string

const (
	// Running is written when a process starts and replaced when it ends, so an
	// entry still saying it names a process that was killed or crashed.
	Running Ending = "running"
	// Drained held nothing when it stopped and knew what its host runs.
	Drained Ending = "drained"
	// Idle never registered, held nothing and was not asked to drain.
	Idle Ending = "idle"
	// HandedOff left its compute running for the next process to adopt, after
	// the control plane recorded its withdrawal.
	HandedOff Ending = "handed-off"
	// LeftRunning stopped waiting with compute still running: a second signal,
	// or a stop that never learned what its host runs.
	LeftRunning Ending = "left-running"
	// Superseded found another process holding its name.
	Superseded Ending = "superseded"
	// Failed returned an error.
	Failed Ending = "failed"
	// Unrecorded is an operator's acknowledgement of a stop no record covers.
	Unrecorded Ending = "unrecorded"
	// Stopped is systemd's inactive/dead/success for a process that kept no
	// record (a node binary from before the record), written by Observe: the
	// floor, and weaker than any ending a node wrote.
	Stopped Ending = "stopped"
)

// Role is what a process was started as.
type Role string

const (
	// RoleNode is the node service.
	RoleNode Role = "node"
	// RoleProbe is an upgrade's readiness probe, which touches no compute and
	// which no judgement reads.
	RoleProbe Role = "probe"
)

// Source is who wrote an entry's ending.
type Source string

const (
	// SourceNode is the process itself.
	SourceNode Source = "node"
	// SourceSystemd is billet recording what systemd said of a stop no node
	// entry covers, so the evidence outlives reset-failed and garbage collection.
	SourceSystemd Source = "systemd"
	// SourceOperator is an acknowledgement of a stop no record covers.
	SourceOperator Source = "operator"
)

// Entry is one process.
type Entry struct {
	InvocationID   string     `json:"invocation_id"`
	PID            int        `json:"pid"`
	BootID         string     `json:"boot_id"`
	Release        string     `json:"release"`
	Role           Role       `json:"role"`
	RunsOnHost     bool       `json:"runs_on_host"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	DrainRequested bool       `json:"drain_requested"`
	Ending         Ending     `json:"ending"`
	Source         Source     `json:"source"`
	Acknowledged   bool       `json:"acknowledged"`
}

// Record is the newest entries, oldest first.
type Record struct {
	Schema  int     `json:"schema"`
	Entries []Entry `json:"entries"`
	// Trimmed says a node process's entry has been dropped to keep the record
	// to MaxEntries, so a judgement that reads past the oldest entry kept
	// cannot tell what the dropped ones left.
	Trimmed bool `json:"trimmed,omitempty"`
}

const (
	// FileName is the record's name in the node's state directory.
	FileName = "endings.json"
	// lockName is the flock beside it that every read-modify-write holds.
	lockName = "endings.lock"
	// Schema is the record's format.
	Schema = 1
	// MaxEntries is how many entries the record keeps.
	MaxEntries = 16
	// maxBytes bounds a read: sixteen entries are a few kilobytes.
	maxBytes = 256 << 10
)

// ErrNoEntry is End's refusal when this process's entry is gone from the record.
var ErrNoEntry = errors.New("this process has no entry in the ending record")

// Path is the record's path in dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// bootIDPath is where Linux names the current boot; a Mac has nothing there.
var bootIDPath = "/proc/sys/kernel/random/boot_id"

// BootID is the current boot's id, empty where the host has none: the one
// read, for the process that records itself and the command that judges it.
func BootID() string {
	body, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(body))
}

// Read reads the record in dir. A missing file is a zero Record and present
// false; anything else that is not a valid record is an error, which every
// caller treats as could-not-tell. The record is evidence only when this
// process's own user wrote it and no one else could have: owned by the reader
// (root, where it is judged) and writable by no group or other.
func Read(dir string) (Record, bool, error) {
	f, info, err := regularfile.Open(Path(dir), regularfile.Options{NoFollow: true})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Record{}, false, nil
	case err != nil:
		return Record{}, false, fmt.Errorf("read %s: %w", Path(dir), err)
	}
	defer func() { _ = f.Close() }()

	if err := ownedByReader(info); err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", Path(dir), err)
	}

	body, err := regularfile.ReadAllLimited(f, Path(dir), maxBytes)
	if err != nil {
		return Record{}, false, fmt.Errorf("read %s: %w", Path(dir), err)
	}

	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return Record{}, false, fmt.Errorf("%s is not an ending record: %w", Path(dir), err)
	}

	if err := rec.validate(); err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", Path(dir), err)
	}

	return rec, true, nil
}

func (r *Record) validate() error {
	if r.Schema != Schema {
		return fmt.Errorf("schema %d, want %d", r.Schema, Schema)
	}

	for i := range r.Entries {
		e := &r.Entries[i]

		switch e.Ending {
		case Running, Drained, Idle, HandedOff, LeftRunning, Superseded, Failed, Unrecorded, Stopped:
		default:
			return fmt.Errorf("entry %d has ending %q", i, e.Ending)
		}

		switch e.Role {
		case RoleNode, RoleProbe:
		default:
			return fmt.Errorf("entry %d has role %q", i, e.Role)
		}

		switch e.Source {
		case SourceNode, SourceSystemd, SourceOperator:
		default:
			return fmt.Errorf("entry %d has source %q", i, e.Source)
		}
	}

	return nil
}

// update reads the record under the lock, lets change edit it, and writes it
// back durably. An unreadable record is not overwritten: the evidence in it is
// what every later judgement needs.
func update(dir string, change func(*Record) error) error {
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()

	rec, present, err := Read(dir)
	if err != nil {
		return err
	}

	if !present {
		rec = Record{Schema: Schema}
	}

	if err := change(&rec); err != nil {
		return err
	}

	rec.trim()

	return install(dir, rec)
}

// trim keeps the record to MaxEntries, dropping probes' entries before any
// node process's, since no judgement reads a probe, and never the newest entry,
// which may be a process that has yet to end.
func (r *Record) trim() {
	for len(r.Entries) > MaxEntries {
		drop := slices.IndexFunc(r.Entries[:len(r.Entries)-1], func(e Entry) bool { return e.Role == RoleProbe })
		if drop < 0 {
			drop, r.Trimmed = 0, true
		}

		r.Entries = slices.Delete(slices.Clone(r.Entries), drop, drop+1)
	}
}

// install writes rec durably as the record in dir.
func install(dir string, rec Record) error {
	_, err := durablefile.Installer{}.Install(dir, FileName, 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc.Encode(rec)
	})

	return err
}

// lock takes the record's flock, creating the lock file beside it.
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE|noFollow, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the ending record's lock in %s: %w", dir, err)
	}

	if err := flock(f); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("lock the ending record in %s: %w", dir, err)
	}

	return func() { _ = f.Close() }, nil
}

// Handle is one process's entry, from its start to its ending.
type Handle struct {
	dir   string
	key   Entry
	ended bool
	now   func() time.Time
}

// Begin appends e as Running and returns the handle that ends it. A process
// must not touch compute when this fails: without an entry its ending cannot be
// proved later.
func Begin(dir string, e Entry, now func() time.Time) (*Handle, error) {
	if now == nil {
		now = time.Now
	}

	e.Ending, e.Source, e.EndedAt, e.Acknowledged = Running, SourceNode, nil, false
	if e.StartedAt.IsZero() {
		e.StartedAt = now().UTC()
	}

	if err := update(dir, func(r *Record) error {
		r.Entries = append(r.Entries, e)

		return nil
	}); err != nil {
		return nil, err
	}

	return &Handle{dir: dir, key: e, now: now}, nil
}

// Ended reports whether End has written this process's ending.
func (h *Handle) Ended() bool { return h.ended }

// End writes this process's ending once; a later call does nothing.
func (h *Handle) End(ending Ending, drainRequested bool) error {
	if h.ended {
		return nil
	}

	at := h.now().UTC()

	err := update(h.dir, func(r *Record) error {
		for i := len(r.Entries) - 1; i >= 0; i-- {
			e := &r.Entries[i]
			if e.PID == h.key.PID && e.BootID == h.key.BootID && e.StartedAt.Equal(h.key.StartedAt) {
				e.Ending, e.EndedAt, e.DrainRequested = ending, &at, drainRequested

				return nil
			}
		}

		return ErrNoEntry
	})
	if err == nil {
		h.ended = true
	}

	return err
}

// ErrRecordChanged is Observe's refusal when the record no longer says what
// the judgement it records read: the invocation has an entry of its own now,
// or the history beneath it no longer admits a start.
var ErrRecordChanged = errors.New("the ending record changed since the stop was judged")

// Observe appends what systemd said of a stop of e.InvocationID, which no
// node entry covers, so the evidence outlives reset-failed and the unit's
// garbage collection. It re-reads the record under its lock first: an
// invocation that has an entry of its own is the node's to speak for, a stop
// already observed is not observed twice, and a clean stop is recorded only
// over a history that admits a start, as the judgement it records required.
func Observe(dir string, e Entry, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}

	if e.InvocationID == "" {
		return errors.New("a stop systemd observed is recorded only for a known invocation")
	}

	at := now().UTC()
	e.Source, e.EndedAt, e.Role, e.Acknowledged = SourceSystemd, &at, RoleNode, false

	return update(dir, func(r *Record) error {
		for i := range r.Entries {
			old := &r.Entries[i]
			if old.InvocationID != e.InvocationID || old.Role != RoleNode {
				continue
			}

			if old.Source == SourceSystemd {
				return nil
			}

			return fmt.Errorf("%w: invocation %s has an entry of its own", ErrRecordChanged, e.InvocationID)
		}

		if n := newestNode(r.Entries); e.Ending == Stopped && n >= 0 {
			if s := standing(*r, n, forStart, e.BootID); s.Verdict != Proved {
				return fmt.Errorf("%w: %s", ErrRecordChanged, s.Why)
			}
		}

		r.Entries = append(r.Entries, e)

		return nil
	})
}

// Acknowledge marks the newest node entry acknowledged, only when it is the
// invocation named. unrecorded instead appends an acknowledged Unrecorded entry,
// for a stop no entry covers at all.
func Acknowledge(dir, invocation string, unrecorded bool, now func() time.Time) (Entry, error) {
	if now == nil {
		now = time.Now
	}

	var got Entry

	err := update(dir, func(r *Record) error {
		if unrecorded {
			at := now().UTC()
			got = Entry{
				Role: RoleNode, StartedAt: at, EndedAt: &at, Ending: Unrecorded,
				Source: SourceOperator, Acknowledged: true,
			}
			r.Entries = append(r.Entries, got)

			return nil
		}

		i := newestNode(r.Entries)
		if i < 0 {
			return errors.New("the ending record has no node entry to acknowledge; " +
				"acknowledge --unrecorded acknowledges a stop no entry covers")
		}

		if r.Entries[i].InvocationID != invocation {
			return fmt.Errorf("the newest node entry is invocation %q, not %q; only the newest "+
				"stop is acknowledged", r.Entries[i].InvocationID, invocation)
		}

		r.Entries[i].Acknowledged = true
		got = r.Entries[i]

		return nil
	})

	return got, err
}

// Reset sets aside a record that cannot be read, renaming it beside itself
// rather than deleting the evidence, and starts a new one holding an
// acknowledged Unrecorded entry: the operator's word that the host runs nothing
// a node would not adopt, which is all a start then rests on. A record that can
// be read is refused, since its newest entry is what to acknowledge.
func Reset(dir string, now func() time.Time) (string, Entry, error) {
	if now == nil {
		now = time.Now
	}

	unlock, err := lock(dir)
	if err != nil {
		return "", Entry{}, err
	}
	defer unlock()

	_, present, readErr := Read(dir)

	switch {
	case readErr == nil && present:
		return "", Entry{}, errors.New("the ending record can be read; acknowledge its newest entry instead")
	case readErr == nil:
		return "", Entry{}, errors.New("there is no ending record to reset; acknowledge --unrecorded instead")
	}

	at := now().UTC()
	aside := Path(dir) + ".unreadable-" + at.Format("20060102T150405Z")

	if err := os.Rename(Path(dir), aside); err != nil {
		return "", Entry{}, fmt.Errorf("set the unreadable record aside: %w", err)
	}

	e := Entry{Role: RoleNode, StartedAt: at, EndedAt: &at, Ending: Unrecorded, Source: SourceOperator,
		Acknowledged: true}

	if err := install(dir, Record{Schema: Schema, Entries: []Entry{e}}); err != nil {
		return aside, Entry{}, err
	}

	return aside, e, nil
}

// newestNode is the index of the newest entry that is not a probe's, or -1.
func newestNode(entries []Entry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role != RoleProbe {
			return i
		}
	}

	return -1
}
