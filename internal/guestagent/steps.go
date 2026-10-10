package guestagent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/regularfile"
)

// The bounds of following the runner's log: the bytes one poll reads, the longest
// line held across polls (a longer one is skipped whole), the entries of the
// diagnostics directory looked at, and the marks held for the batches to come.
const (
	stepReadLimit    = 256 << 10
	stepLineLimit    = 64 << 10
	diagEntriesLimit = 4096
	pendingStepLimit = guestreport.MaxReportSteps
)

// The step-start line of the runner's Worker log, measured on a guest on
// 2026-10-10 (runner of billet v0.14.0's image):
//
//	[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='Mark one'
//
// The time is whole seconds, UTC, on the guest's clock.
const (
	stepStampLayout = time.DateTime
	stepMarker      = "Z INFO StepsRunner] Processing step: DisplayName='"
)

// tailer follows the newest Worker_*.log in the runner's diagnostics directory and
// turns its step-start lines into marks. The log appears only once the job starts,
// and a newer one replaces it; a line the runner has not finished writing is held
// until it is.
type tailer struct {
	dir string

	name   string
	held   os.FileInfo
	offset int64

	partial []byte
	// skipping is a line longer than stepLineLimit, dropped up to its end.
	skipping bool

	pending []guestreport.StepMark
	lastAt  int64

	log *limitedLog
}

func newTailer(dir string, log *limitedLog) *tailer {
	return &tailer{dir: dir, log: log}
}

// poll reads what the newest log gained since the last poll, at most
// stepReadLimit bytes of it.
func (t *tailer) poll() {
	if t.dir == "" {
		return
	}

	name, err := newestWorkerLog(t.dir)
	if err != nil {
		t.log.printf("steps", "the runner's diagnostics directory was not read: %v", err)

		return
	}

	if name == "" {
		return
	}

	if name != t.name {
		t.restart(name)
	}

	f, info, err := regularfile.Open(filepath.Join(t.dir, name), regularfile.Options{NoFollow: true})
	if err != nil {
		t.log.printf("steps", "the runner's newest Worker log was not opened: %v", withoutPath(err))

		return
	}
	defer f.Close()

	if (t.held != nil && !os.SameFile(t.held, info)) || info.Size() < t.offset {
		t.restart(name)
	}

	t.held = info

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		t.log.printf("steps", "the runner's Worker log was not read: %v", withoutPath(err))

		return
	}

	data, err := io.ReadAll(io.LimitReader(f, stepReadLimit))
	t.offset += int64(len(data))
	t.consume(data)

	if err != nil {
		t.log.printf("steps", "the runner's Worker log was not read to its end: %v", withoutPath(err))
	}
}

// restart follows name from its beginning.
func (t *tailer) restart(name string) {
	t.name, t.held, t.offset, t.partial, t.skipping = name, nil, 0, nil, false
}

// consume splits data into lines, the first continuing what was held, and keeps
// the last line held unless it is complete.
func (t *tailer) consume(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.hold(data)

			return
		}

		if !t.skipping && len(t.partial)+i <= stepLineLimit {
			t.partial = append(t.partial, data[:i]...)
			t.line(string(t.partial))
		}

		t.partial, t.skipping = t.partial[:0], false
		data = data[i+1:]
	}
}

// hold keeps the start of an unfinished line, or starts skipping it when it is
// longer than any line the agent reads.
func (t *tailer) hold(data []byte) {
	if t.skipping {
		return
	}

	if len(t.partial)+len(data) > stepLineLimit {
		t.partial, t.skipping = t.partial[:0], true

		return
	}

	t.partial = append(t.partial, data...)
}

// line keeps the mark a complete line makes, if it is one. A mark earlier than the
// last one kept is dropped: the node refuses a batch whose steps go back.
func (t *tailer) line(s string) {
	mark, ok := parseStepLine(s)
	if !ok {
		return
	}

	if mark.AtSeconds < t.lastAt {
		t.log.printf("steps-back", "a step mark earlier than the last one was dropped")

		return
	}

	if len(t.pending) >= pendingStepLimit {
		t.log.printf("steps-full", "more step marks than a report keeps; the rest are dropped")

		return
	}

	t.pending = append(t.pending, mark)
	t.lastAt = mark.AtSeconds
}

// take hands over at most a batch's worth of the marks held, oldest first.
func (t *tailer) take() []guestreport.StepMark {
	n := min(len(t.pending), guestreport.MaxBatchSteps)
	if n == 0 {
		return nil
	}

	out := append([]guestreport.StepMark(nil), t.pending[:n]...)
	t.pending = append(t.pending[:0], t.pending[n:]...)

	return out
}

// parseStepLine is the mark a step-start line makes. The name runs to the line's
// LAST quote, because a display name can hold quotes of its own and nothing the
// runner writes after it does.
func parseStepLine(line string) (guestreport.StepMark, bool) {
	line = strings.TrimSuffix(line, "\r")

	const stampEnd = 1 + len(stepStampLayout)
	if len(line) < stampEnd || line[0] != '[' {
		return guestreport.StepMark{}, false
	}

	rest, ok := strings.CutPrefix(line[stampEnd:], stepMarker)
	if !ok {
		return guestreport.StepMark{}, false
	}

	end := strings.LastIndexByte(rest, '\'')
	if end < 0 {
		return guestreport.StepMark{}, false
	}

	at, err := time.Parse(stepStampLayout, line[1:stampEnd])
	if err != nil || at.Unix() <= 0 || at.Unix() > guestreport.MaxValue {
		return guestreport.StepMark{}, false
	}

	return guestreport.StepMark{
		Name:      textOrMark(guestreport.Clean(rest[:end], guestreport.MaxNameBytes)),
		AtSeconds: at.Unix(),
	}, true
}

// newestWorkerLog is the name of the newest Worker_*.log in dir, or "" when there
// is none yet. The runner names each by its UTC start, so the greatest name is the
// newest.
func newestWorkerLog(dir string) (string, error) {
	d, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", withoutPath(err)
	}
	defer d.Close()

	newest, seen := "", 0

	for {
		entries, err := d.ReadDir(256)
		for _, e := range entries {
			seen++

			name := e.Name()
			if strings.HasPrefix(name, "Worker_") && strings.HasSuffix(name, ".log") && name > newest {
				newest = name
			}
		}

		if errors.Is(err, io.EOF) || (err == nil && len(entries) == 0) {
			return newest, nil
		}

		if err != nil {
			return "", withoutPath(err)
		}

		if seen >= diagEntriesLimit {
			return "", fmt.Errorf("more than %d entries", diagEntriesLimit)
		}
	}
}

// withoutPath is err without the path an *os.PathError names: a name in a
// directory the job can write to is the job's text, and the agent's log quotes
// none of it.
func withoutPath(err error) error {
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err
	}

	return err
}
