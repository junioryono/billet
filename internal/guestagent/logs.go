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
	// logMaxNames bounds the names remembered over a job's life, and with them the steps.
	// A job can go on creating fresh names while keeping few present, and a real one has a
	// few for each step. Past it nothing new is opened, and that is counted once.
	logMaxNames = 1 << 14
	// logMaxMarks bounds the runner's marks waiting for a step log.
	logMaxMarks = 1 << 12
	// logMarkPolls bounds how far one look at the Worker log goes, in reads of its bound: a step's
	// name must not be taken from a mark that a long Worker log left behind.
	logMarkPolls = 16
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
	// logHandOverEvery is how soon more is handed to the sender while some waits for it.
	logHandOverEvery = 100 * time.Millisecond
	// logRetryStopping is the longest a retry waits once the agent is told to stop.
	logRetryStopping = 200 * time.Millisecond
	// logSendQueue is how many chunks wait for the sender, past the one it holds.
	logSendQueue = 4
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

	// names are the step logs met, opened or not, up to logMaxNames.
	names     map[string]struct{}
	namesFull bool
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
		names: map[string]struct{}{}, steps: map[string]*logStep{}, buf: make([]byte, logReadLimit)}
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
	if _, known := t.names[name]; known {
		return
	}

	record, ok := recordOf(name)
	if !ok {
		return
	}

	if len(t.names) >= logMaxNames {
		if !t.namesFull {
			t.namesFull = true
			t.lost++
		}

		return
	}

	t.names[name] = struct{}{}

	if len(t.open) >= logMaxFiles {
		t.lost++

		return
	}

	f, _, err := regularfile.OpenAt(dir, name)
	if err != nil {
		t.lost++

		return
	}

	t.open = append(t.open, &logFile{f: f, name: name, record: record})
}

// read takes what each open file gained, while the agent holds less than its bound, and
// says whether there may be more to read at once: it read something, or the bound stopped
// it. A file read to its end and unlinked is finished; one still linked may yet grow.
func (t *logTailer) read() bool {
	more := false

	for _, file := range t.open {
		if file.done {
			continue
		}

		// NOT A RETURN: what is held has to be numbered below before it can be sent, or
		// a full buffer of unnumbered lines would wait for itself.
		if t.held >= logHeldLimit {
			more = true

			break
		}

		n, err := file.f.Read(t.buf)
		t.consume(file, t.buf[:n])

		more = more || n > 0

		if (n == 0 && errors.Is(err, io.EOF) || err != nil && !errors.Is(err, io.EOF)) && t.finished(file, err) {
			t.closeFile(file)
		}
	}

	t.open = slices.DeleteFunc(t.open, func(f *logFile) bool { return f.done && len(f.lines) == 0 })
	t.assign()

	return more
}

// endLines takes each file's unfinished last line as a line, once the runner has exited and
// nothing will end it: a file the runner never unlinked is never closed.
func (t *logTailer) endLines() {
	for _, file := range t.open {
		if !file.done && len(file.partial) > 0 {
			t.keepLine(file, string(file.partial))
			file.partial, file.skipping = file.partial[:0], false
		}
	}

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

	t.pollMarks()

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

// pollMarks reads the Worker log as far as it has grown, within its bound.
func (t *logTailer) pollMarks() {
	for range logMarkPolls {
		before := t.marks.offset
		t.marks.poll()

		for {
			taken := t.marks.take()
			if len(taken) == 0 {
				break
			}

			t.marked = append(t.marked, taken...)
		}

		if t.marks.offset == before {
			break
		}
	}

	t.marked = t.marked[max(0, len(t.marked)-logMaxMarks):]
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
			// ONLY A CHUNK SPENDS ONE: halving must not read as chunks lost.
			data, err := joblog.Encode(joblog.Chunk{Seq: t.seq + 1, Step: file.step, StepName: file.stepName,
				Lines: file.lines[:count]})
			if err == nil && len(data) <= limit {
				t.seq++

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
//
// TWO GOROUTINES, BECAUSE SENDING MUST NOT STOP THE LOOKING. A step that wrote little can
// be created, uploaded and deleted while a request waits on a node, and a file not opened
// before its delete is lost; a wake from inotify keeps nothing. So the reader owns the
// tailer and only ever hands chunks over without waiting, and the sender waits on the node.
type logShipper struct {
	node   *node
	tail   *logTailer
	limit  int
	period time.Duration
	log    *limitedLog

	// queue carries chunks to the sender. next is the one cut and not handed over yet,
	// which only the reader touches.
	queue chan []byte
	next  []byte
	// stopped closes when the sender will send nothing more.
	stopped chan struct{}
	done    chan struct{}
	// watching makes what wakes the reader; a test's cannot be inotify, whose wait a
	// synctest bubble cannot see past.
	watching func(dir string) pagesWaiter
}

// pagesWaiter wakes the reader as a page appears, or when a wait has passed.
type pagesWaiter interface {
	wait(ctx context.Context, d time.Duration)
	close()
}

func newLogShipper(n *node, tail *logTailer, limit int, period time.Duration, log *limitedLog) *logShipper {
	return &logShipper{node: n, tail: tail, limit: limit, period: period, log: log,
		queue: make(chan []byte, logSendQueue), stopped: make(chan struct{}), done: make(chan struct{}),
		watching: func(dir string) pagesWaiter { return newPagesWatch(dir) }}
}

// startLogs ships a job's step logs under what the node's hello offered, until ctx ends.
func startLogs(ctx context.Context, n *node, diag string, offer *logLimits, log *limitedLog) *logShipper {
	s := newLogShipper(n, newLogTailer(diag, log), min(offer.maxBody, joblog.MaxChunkBytes), offer.interval, log)

	go func() {
		defer close(s.done)

		s.run(ctx)
	}()

	return s
}

// wait returns once the shipper has finished.
func (s *logShipper) wait() { <-s.done }

// run is the reader's loop, and joins the sender. A page is opened as soon as it is seen,
// which is why it looks every logPollEvery at the longest however seldom it sends.
func (s *logShipper) run(ctx context.Context) {
	// THE SENDER OUTLIVES ctx BY THE FINAL BUDGET, so a request under way when the agent is
	// told to stop is not cut off, and its chunk is not lost.
	sendCtx, cancelSend := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelSend()

	sender := make(chan struct{})

	go func() {
		defer close(sender)

		s.sendAll(ctx, sendCtx)
	}()

	watch := s.watching(s.tail.dir)
	defer watch.close()

	sent := time.Now()
	handing := false

	for ctx.Err() == nil && !s.senderStopped() {
		s.tail.scan()
		s.tail.read()

		if time.Since(sent) >= s.period {
			sent, handing = time.Now(), true
		}

		wait := logPollEvery

		if handing {
			handing = !s.handOver()
		}

		if handing {
			wait = logHandOverEvery
		}

		watch.wait(ctx, wait)
	}

	// THE BUDGET ENDS THE SENDER, HOWEVER LONG THE NODE IS AWAY, and has to outlast the
	// reader's part of it: what the reader hands over is still to be sent when it returns.
	budget := time.AfterFunc(logFinalBudget, cancelSend)
	defer budget.Stop()

	s.finish()

	close(s.queue)
	<-sender
}

func (s *logShipper) senderStopped() bool {
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

// handOver gives the sender as many chunks as it will take without waiting, and says
// whether that was all of them. A number is spent on a chunk when it is cut, so it is kept
// until the sender has it.
func (s *logShipper) handOver() bool {
	for {
		if s.next == nil {
			data, ok := s.tail.nextChunk(s.limit)
			if !ok {
				return true
			}

			s.next = data
		}

		select {
		case s.queue <- s.next:
			s.next = nil
		default:
			return false
		}
	}
}

// finish reads what is left to its end and hands it over, within the final budget. Ending
// here is what makes "Complete job" reach the node: the runner has exited by the time this
// runs. It is done only when a pass read nothing and nothing is held: a pass that sent all
// it held may have read only the first part of a long file.
func (s *logShipper) finish() {
	stopBy := time.Now().Add(logFinalBudget)

	for time.Now().Before(stopBy) && !s.senderStopped() {
		s.tail.scan()

		more := s.tail.read()
		if !more {
			s.tail.endLines()
		}

		handed := s.handOver()
		if handed && !more {
			break
		}

		if !handed {
			time.Sleep(logHandOverEvery)
		}
	}

	if s.tail.lost > 0 {
		s.log.printf("logs-lost", "%d of this job's step logs were not read", s.tail.lost)
	}
}

// sendAll sends what it is handed until the queue closes or nothing more may be sent.
func (s *logShipper) sendAll(ctx, sendCtx context.Context) {
	for data := range s.queue {
		if !s.deliver(ctx, sendCtx, data) {
			close(s.stopped)

			return
		}
	}
}

// deliver sends one chunk until the node has answered for it, and reports false when nothing
// more should be sent. THE SAME CHUNK UNTIL THEN, whatever ends: a chunk the node asks for
// again is sent again after its wait, and one it refused is gone, its number showing the gap.
func (s *logShipper) deliver(ctx, sendCtx context.Context, data []byte) bool {
	backoff := logRetryFirst

	for sendCtx.Err() == nil {
		v, wait := s.node.sendLogs(sendCtx, data)

		var pause time.Duration

		switch v {
		case accepted, refused:
			return true
		case stop:
			s.log.always("the node stopped taking this job's logs; nothing more is sent")

			return false
		case rateLimited:
			pause = max(wait, logRetryFirst)
		default:
			s.log.printf("logs-send", "the node's answer to a log chunk could not be told; it is sent again")

			pause, backoff = backoff, min(2*backoff, logRetryMost)
		}

		// A STOP CUTS THE WAIT SHORT, and what is left of the budget is all there is.
		if ctx.Err() != nil || !sleepFor(ctx, pause) {
			sleepFor(sendCtx, min(pause, logRetryStopping))
		}
	}

	return false
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
