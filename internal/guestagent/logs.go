package guestagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/regularfile"
)

// The runner's step logs, measured on a real job on 2026-11-04 (runner 2.337.0):
//
//	_diag/pages/<timeline>_<record>_<n>.log
//
// One record for each step that wrote, "Set up job" and "Complete job" among them, and
// one more for the job, which holds every line again. A file is created at its first
// write and deleted once it is uploaded, by the job's end at the latest, so it is
// opened as it appears and read through its descriptor past the unlink. The job's own
// record is a version 5 UUID and a step's a version 4, which is how it is told apart;
// a runner that changed that would cost duplicates within the job's budget, never a
// line lost.
//
// A LINE HAS BEEN THROUGH THE RUNNER'S SECRET MASK BEFORE IT REACHES THE FILE:
// ExecutionContext.Write masks, then logs (actions/runner v2.338.1). Nothing else a job
// writes to disk has, which is why this reads these files and nothing else of a job's.
const (
	logPagesDir = "pages"
	// logStampLimit is the longest prefix a line's time can be; a line with no space
	// inside it has no time.
	logStampLimit = 40
	// logMaxFiles bounds the descriptors held. A job cannot be stopped from creating
	// names here, and what is past the bound is counted lost.
	logMaxFiles = 256
	// logReadLimit is what one pass takes of one file, and logHeldLimit what the agent
	// holds unsent across every file; past it reading waits for the node.
	logReadLimit = 256 << 10
	logHeldLimit = 4 << 20
	// logLineHold is how much of a line is held while it has no end: a longer one is
	// kept cut and the rest skipped.
	logLineHold = 2 * joblog.MaxLineBytes
	// How often the agent looks for what is new, and how long it waits on a node
	// that could not be told anything.
	logPollEvery  = time.Second
	logRetryFirst = time.Second
	logRetryMost  = time.Minute
	// logFinalBudget bounds sending what is left once the agent is told to stop, inside
	// the contract's 3 s, beside the final report and never in its way.
	logFinalBudget = 2500 * time.Millisecond
)

// logFile is one open step log.
type logFile struct {
	f      *os.File
	name   string
	record string
	// assigned says step and stepName are set, which waits for its first line.
	assigned bool
	step     int64
	stepName string

	begun    bool
	partial  []byte
	skipping bool
	lines    []joblog.Line
	first    int64
	lastAt   int64
	done     bool
}

// logTailer follows the runner's step logs and cuts them into chunks. Only the shipper's
// goroutine touches it.
type logTailer struct {
	dir   string
	marks *tailer
	log   *limitedLog

	files map[string]*logFile
	// open are the files being read, in the order they were found.
	open []*logFile
	// steps is each record's step number, so a step's later pages stay its own.
	steps map[string]*logStep
	next  int64
	// marked are the runner's step marks not yet matched to a step log.
	marked []guestreport.StepMark

	seq  uint64
	held int
	lost int64
	buf  []byte
}

// logStep is a step as its logs are numbered.
type logStep struct {
	step int64
	name string
}

func newLogTailer(diag string, log *limitedLog) *logTailer {
	return &logTailer{dir: filepath.Join(diag, logPagesDir), marks: newTailer(diag, log), log: log,
		files: map[string]*logFile{}, steps: map[string]*logStep{}, buf: make([]byte, logReadLimit)}
}

// recordOf is the record a page's name says it holds, and false for a name that is no
// step's page: the job's own record, or anything else a job put here.
func recordOf(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, ".log")
	if !ok {
		return "", false
	}

	parts := strings.Split(stem, "_")
	if len(parts) != 3 || len(parts[1]) != 36 || parts[1][14] == '5' {
		return "", false
	}

	return parts[1], true
}

// scan opens every step log it has not met.
func (t *logTailer) scan() {
	d, err := openDir(t.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.log.printf("logs-dir", "the runner's log directory was not read: %v", withoutPath(err))
		}

		return
	}
	defer d.Close()

	seen := 0

	for {
		entries, err := d.ReadDir(256)
		for _, e := range entries {
			seen++
			t.meet(d, e.Name())
		}

		if err != nil || len(entries) == 0 || seen >= diagEntriesLimit {
			return
		}
	}
}

// meet opens one name in the directory if it is a step log not met before. A file that
// is gone already was missed for good, and is counted so.
func (t *logTailer) meet(dir *os.File, name string) {
	if _, known := t.files[name]; known {
		return
	}

	record, ok := recordOf(name)
	if !ok {
		return
	}

	if len(t.open) >= logMaxFiles {
		t.files[name] = &logFile{done: true}
		t.lost++

		return
	}

	f, _, err := regularfile.OpenAt(dir, name)
	if err != nil {
		t.files[name] = &logFile{done: true}
		t.lost++

		return
	}

	file := &logFile{f: f, name: name, record: record}
	t.files[name] = file
	t.open = append(t.open, file)
}

// read takes what each open file gained, while the agent holds less than its bound. A
// file read to its end and unlinked is finished; one still linked may yet grow.
func (t *logTailer) read() {
	for _, file := range t.open {
		if file.done {
			continue
		}

		// NOT A RETURN: what is held has to be numbered below before it can be sent, or
		// a full buffer of unnumbered lines would wait for itself.
		if t.held >= logHeldLimit {
			break
		}

		n, err := file.f.Read(t.buf)
		t.consume(file, t.buf[:n])

		if (n == 0 && errors.Is(err, io.EOF) || err != nil && !errors.Is(err, io.EOF)) && t.finished(file, err) {
			t.closeFile(file)
		}
	}

	t.open = slices.DeleteFunc(t.open, func(f *logFile) bool { return f.done && len(f.lines) == 0 })
	t.assign()
}

// finished says a file at its end has no more to come: nothing links it any longer, or
// reading it failed.
func (t *logTailer) finished(file *logFile, readErr error) bool {
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		t.log.printf("logs-read", "a step log was not read to its end: %v", withoutPath(readErr))

		return true
	}

	info, err := file.f.Stat()
	if err != nil {
		return true
	}

	st, ok := info.Sys().(*syscall.Stat_t)

	return !ok || st.Nlink == 0
}

// closeFile ends a file, keeping an unfinished last line.
func (t *logTailer) closeFile(file *logFile) {
	if len(file.partial) > 0 {
		t.keepLine(file, string(file.partial))
		file.partial = nil
	}

	_ = file.f.Close()
	file.done = true
}

// consume splits data into lines, the first continuing what was held. The runner begins
// each file with a byte order mark.
func (t *logTailer) consume(file *logFile, data []byte) {
	if !file.begun && len(data) > 0 {
		file.begun = true
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	}

	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.holdPart(file, data)

			return
		}

		t.holdPart(file, data[:i])
		t.keepLine(file, string(file.partial))
		file.partial, file.skipping = file.partial[:0], false
		data = data[i+1:]
	}
}

// holdPart keeps the start of an unfinished line, and only its start past the bound.
func (t *logTailer) holdPart(file *logFile, data []byte) {
	if file.skipping {
		return
	}

	if room := logLineHold - len(file.partial); len(data) > room {
		data, file.skipping = data[:room], true
	}

	file.partial = append(file.partial, data...)
}

// keepLine keeps one line, stamped by the runner where it carries a time.
func (t *logTailer) keepLine(file *logFile, line string) {
	line = strings.TrimSuffix(line, "\r")

	at, text := splitStamp(line)
	if at > 0 {
		file.lastAt = at
	}

	if file.first == 0 {
		file.first = at
	}

	text = joblog.Clean(text, joblog.MaxLineBytes)
	file.lines = append(file.lines, joblog.Line{AtMillis: at, Text: text})
	t.held += len(text) + 32
}

// splitStamp is a line's time and its text: Unix milliseconds, zero for a line with no
// time of the runner's, which a message of several lines has after its first.
func splitStamp(line string) (int64, string) {
	i := strings.IndexByte(line[:min(len(line), logStampLimit)], ' ')
	if i < 0 {
		return 0, line
	}

	at, err := time.Parse(time.RFC3339Nano, line[:i])
	if err != nil || at.UnixMilli() <= 0 || at.UnixMilli() > guestreport.MaxValue {
		return 0, line
	}

	return at.UnixMilli(), line[i+1:]
}

// assign numbers the step logs that have lines, by the time of their first, and names
// each from the runner's marks. A step's file is flushed before the next step begins,
// so the order holds; the setup before the first mark and the cleanup after the last
// have no mark and no name.
func (t *logTailer) assign() {
	var waiting []*logFile

	for _, file := range t.open {
		if !file.assigned && len(file.lines) > 0 {
			waiting = append(waiting, file)
		}
	}

	if len(waiting) == 0 {
		return
	}

	t.marks.poll()
	t.marked = append(t.marked, t.marks.take()...)

	slices.SortStableFunc(waiting, func(a, b *logFile) int {
		switch {
		case a.first < b.first:
			return -1
		case a.first > b.first:
			return 1
		default:
			return 0
		}
	})

	for _, file := range waiting {
		step, known := t.steps[file.record]
		if !known {
			step = &logStep{step: t.next, name: t.takeMark(file.first)}
			t.steps[file.record] = step
			t.next++
		}

		file.assigned, file.step, file.stepName = true, step.step, step.name
	}
}

// takeMark is the name of the latest mark at or before a step log's first line, and uses
// up every mark before it. A mark is whole seconds on the same clock.
func (t *logTailer) takeMark(firstMillis int64) string {
	name, used := "", 0

	for i, mark := range t.marked {
		if mark.AtSeconds*1000 > firstMillis {
			break
		}

		name, used = mark.Name, i+1
	}

	t.marked = t.marked[used:]

	return name
}

// nextChunk is the next chunk to send, encoded within limit bytes, or false when no
// file holds lines. A number is spent on each chunk, so a chunk dropped shows as a gap.
func (t *logTailer) nextChunk(limit int) ([]byte, bool) {
	for _, file := range t.open {
		if !file.assigned || len(file.lines) == 0 {
			continue
		}

		count := min(len(file.lines), joblog.MaxChunkLines)

		for {
			t.seq++

			data, err := joblog.Encode(joblog.Chunk{Seq: t.seq, Step: file.step, StepName: file.stepName,
				Lines: file.lines[:count]})
			if err == nil && len(data) <= limit {
				for _, l := range file.lines[:count] {
					t.held -= len(l.Text) + 32
				}

				file.lines = file.lines[count:]

				return data, true
			}

			// HALVED UNTIL IT FITS, and a line that fits nothing alone is left out:
			// a job can write what compresses to no less than itself.
			if count == 1 {
				t.held -= len(file.lines[0].Text) + 32
				file.lines = file.lines[1:]
				t.lost++

				break
			}

			count /= 2
		}
	}

	return nil, false
}

// logShipper sends a job's step logs to the node, until the agent is told to stop and
// then as much of what is left as its budget allows. It decides nothing, and nothing it
// does can reach the job.
type logShipper struct {
	node   *node
	tail   *logTailer
	limit  int
	period time.Duration
	log    *limitedLog
	done   chan struct{}
}

// startLogs ships a job's step logs under what the node's hello offered, until ctx ends.
func startLogs(ctx context.Context, n *node, diag string, offer *logLimits, log *limitedLog) *logShipper {
	s := &logShipper{node: n, tail: newLogTailer(diag, log), limit: min(offer.maxBody, joblog.MaxChunkBytes),
		period: offer.interval, log: log, done: make(chan struct{})}

	go func() {
		defer close(s.done)

		s.run(ctx)
	}()

	return s
}

// wait returns once the shipper has finished.
func (s *logShipper) wait() { <-s.done }

// run is the shipper's loop. A page is opened as soon as it is seen, which is why it
// looks every logPollEvery at the longest however seldom it sends.
func (s *logShipper) run(ctx context.Context) {
	watch := newPagesWatch(s.tail.dir)
	defer watch.close()

	sent := time.Now()

	for ctx.Err() == nil {
		s.tail.scan()
		s.tail.read()

		if time.Since(sent) >= s.period {
			sent = time.Now()

			if !s.send(ctx) {
				return
			}
		}

		watch.wait(ctx, logPollEvery)
	}

	s.finish(ctx)
}

// finish reads what is left and sends it within the final budget. Ending here is what
// makes "Complete job" reach the node: the runner has exited by the time this runs.
func (s *logShipper) finish(ctx context.Context) {
	deadline := time.Now().Add(logFinalBudget)

	fctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()

	for fctx.Err() == nil {
		before := s.tail.held
		s.tail.scan()
		s.tail.read()

		if !s.send(fctx) {
			return
		}

		if s.tail.held == 0 && before == 0 {
			return
		}
	}
}

// send sends every chunk the tailer holds, and reports false when the node has said to
// send nothing more. A chunk the node asks to be sent again is, after its wait, and a
// chunk it refused is gone: its number shows the gap.
func (s *logShipper) send(ctx context.Context) bool {
	backoff := logRetryFirst

	for {
		data, ok := s.tail.nextChunk(s.limit)
		if !ok {
			return true
		}

		for {
			v, wait := s.node.sendLogs(ctx, data)

			switch v {
			case accepted, refused:
			case stop:
				s.log.always("the node stopped taking this job's logs; nothing more is sent")

				return false
			case rateLimited:
				backoff = max(wait, logRetryFirst)

				if !sleepFor(ctx, backoff) {
					return true
				}

				continue
			default:
				s.log.printf("logs-send", "the node's answer to a log chunk could not be told; it is sent again")

				if !sleepFor(ctx, backoff) {
					return true
				}

				backoff = min(2*backoff, logRetryMost)

				continue
			}

			break
		}

		backoff = logRetryFirst

		if ctx.Err() != nil {
			return true
		}
	}
}

// sleepFor waits d, and reports false when ctx ended first.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
