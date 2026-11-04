package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/joblog"
)

const (
	stepRecord  = "2c512f10-faf8-46f5-9447-a297cf77c166"
	otherRecord = "512338ab-e265-4df1-8f1b-1922c7fca622"
	jobRecord   = "ad86b988-2d51-5fef-8e47-d6501d04ad33"
	timeline    = "fb86-dc66"
)

// pageName is a page as the runner named one, measured 2026-11-04.
func pageName(record string, n int) string { return fmt.Sprintf("%s_%s_%d.log", timeline, record, n) }

// diagWithPages is a runner's diagnostics directory and its pages directory.
func diagWithPages(t *testing.T) (string, string) {
	t.Helper()

	diag := t.TempDir()
	pages := filepath.Join(diag, logPagesDir)
	if err := os.Mkdir(pages, 0o755); err != nil {
		t.Fatal(err)
	}

	return diag, pages
}

func writePage(t *testing.T, pages, name, text string) {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(pages, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// tailerAt is a tailer over diag whose log goes nowhere.
func tailerAt(t *testing.T, diag string) *logTailer {
	t.Helper()

	log, _ := discardLog()

	return newLogTailer(diag, log)
}

// chunks drains every chunk the tailer holds, decoded.
func chunks(t *testing.T, tail *logTailer) []joblog.Chunk {
	t.Helper()

	var out []joblog.Chunk

	for {
		data, ok := tail.nextChunk(joblog.MaxChunkBytes)
		if !ok {
			return out
		}

		c, err := joblog.Decode(data)
		if err != nil {
			t.Fatalf("a chunk the tailer cut does not decode: %v", err)
		}

		out = append(out, c)
	}
}

// A STEP'S PAGE IS READ AND THE JOB'S IS NOT: the job's record holds every line again, and
// is told apart by its UUID's version, as measured. Anything else a job puts here is not
// a page.
func TestOnlyAStepsPagesAreRead(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		pageName(stepRecord, 1):  true,
		pageName(otherRecord, 2): true,
		pageName(jobRecord, 1):   false,
		"notes.txt":              false,
		"a_b_1.log":              false,
		"a_b_c_d.log":            false,
		pageName("short", 1):     false,
	} {
		if _, got := recordOf(name); got != want {
			t.Errorf("%s: %t, want %t", name, got, want)
		}
	}

	diag, pages := diagWithPages(t)
	writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1000000Z from the step\n")
	writePage(t, pages, pageName(jobRecord, 1), "2026-11-04T07:04:45.1000005Z from the step\n")

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()

	got := chunks(t, tail)
	if len(got) != 1 || len(got[0].Lines) != 1 {
		t.Fatalf("the tailer cut %+v, want the step's one line once", got)
	}
}

// WHAT THE RUNNER WROTE COMES THROUGH AS IT WAS MEASURED: a byte order mark first, a time
// before each line, colours removed, and a line of a message with no time of its own kept
// without one.
func TestALineIsReadAsTheRunnerWritesIt(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	writePage(t, pages, pageName(stepRecord, 1), "\xef\xbb\xbf2026-11-04T07:04:45.1897158Z ##[group]Run sleep\r\n"+
		"2026-11-04T07:04:45.1898001Z \x1b[36;1msleep \"$S\"\x1b[0m\n"+
		"continued with no time\n")

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()

	got := chunks(t, tail)
	if len(got) != 1 || len(got[0].Lines) != 3 {
		t.Fatalf("the tailer cut %+v", got)
	}

	want := time.Date(2026, 11, 4, 7, 4, 45, 189_000_000, time.UTC).UnixMilli()
	lines := got[0].Lines
	if lines[0].AtMillis != want || lines[0].Text != "##[group]Run sleep" {
		t.Errorf("the first line is %+v, want its time %d with no mark", lines[0], want)
	}
	if lines[1].Text != `sleep "$S"` {
		t.Errorf("the coloured line is %q", lines[1].Text)
	}
	if lines[2].AtMillis != 0 || lines[2].Text != "continued with no time" {
		t.Errorf("the line with no time is %+v", lines[2])
	}
}

// A PAGE THE RUNNER DELETES IS READ TO ITS END through the descriptor held, which is the
// whole of why a page is opened as it appears: every file was gone by the job's end, as
// measured.
func TestAPageIsReadPastItsDelete(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	name := pageName(stepRecord, 1)
	writePage(t, pages, name, "2026-11-04T07:04:45.1Z first\n")

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()
	if got := chunks(t, tail); len(got) != 1 || got[0].Lines[0].Text != "first" {
		t.Fatalf("before the delete the tailer cut %+v", got)
	}

	// THE WRITE AND THE DELETE LAND BEFORE THE NEXT READ, as a flush and an upload do.
	writePage(t, pages, name, "2026-11-04T07:04:46.1Z after\nno newline")
	if err := os.Remove(filepath.Join(pages, name)); err != nil {
		t.Fatal(err)
	}

	tail.read()
	tail.read()

	got := chunks(t, tail)
	if len(got) != 1 || len(got[0].Lines) != 2 || got[0].Lines[0].Text != "after" || got[0].Lines[1].Text != "no newline" {
		t.Fatalf("after the delete the tailer cut %+v, want the rest and the unfinished line", got)
	}

	tail.read()
	if len(tail.open) != 0 {
		t.Errorf("a finished page is still held: %d", len(tail.open))
	}
}

// A PAGE GONE BEFORE IT WAS OPENED IS COUNTED LOST, a name the job made a directory or a
// link is not read, and nothing here stops the rest.
func TestWhatCannotBeOpenedIsCountedAndNotRead(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	if err := os.Mkdir(filepath.Join(pages, pageName(otherRecord, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(pages, pageName(otherRecord, 2))); err != nil {
		t.Fatal(err)
	}
	writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1Z kept\n")

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()

	got := chunks(t, tail)
	if len(got) != 1 || len(got[0].Lines) != 1 || got[0].Lines[0].Text != "kept" {
		t.Fatalf("the tailer cut %+v", got)
	}
	if tail.lost != 2 {
		t.Errorf("%d pages counted lost, want the directory and the link", tail.lost)
	}
}

// A LINE OF ANY LENGTH COSTS ITS BOUND: it is kept cut, the rest skipped up to its end, and
// the next line is whole.
func TestALongLineIsCut(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1Z "+strings.Repeat("x", 3*logLineHold)+
		"\n2026-11-04T07:04:45.2Z next\n")

	tail := tailerAt(t, diag)
	tail.scan()

	for range 4 {
		tail.read()
	}

	got := chunks(t, tail)
	if len(got) != 1 || len(got[0].Lines) != 2 {
		t.Fatalf("the tailer cut %+v", got)
	}
	if n := len(got[0].Lines[0].Text); n != joblog.MaxLineBytes {
		t.Errorf("the long line is %d bytes, want cut to %d", n, joblog.MaxLineBytes)
	}
	if got[0].Lines[1].Text != "next" {
		t.Errorf("the line after it is %q", got[0].Lines[1].Text)
	}
}

// STEPS ARE NUMBERED AS THEY RAN, BY THEIR FIRST LINE AND NOT BY A DIRECTORY'S ORDER, and
// named from the runner's own marks. A step's later page stays its step. The setup before
// the first mark and the cleanup after the last have none.
func TestStepsAreNumberedAsTheyRanAndNamedFromTheMarks(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	if err := os.WriteFile(filepath.Join(diag, "Worker_20261104-070444-utc.log"), []byte(
		"[2026-11-04 07:04:45Z INFO StepsRunner] Processing step: DisplayName='Hold the runner'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const cleanup = "f93648e4-6338-4be6-a25f-1e53cc6a8e50"

	// Named so a directory's order is not the order they ran in.
	writePage(t, pages, pageName(cleanup, 1), "2026-11-04T07:05:05.4Z cleaning\n")
	writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:44.9Z setting up\n")
	writePage(t, pages, pageName(otherRecord, 1), "2026-11-04T07:04:45.2Z holding\n")
	writePage(t, pages, pageName(otherRecord, 2), "2026-11-04T07:04:50.0Z still holding\n")

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()

	steps := map[string]joblog.Chunk{}
	for _, c := range chunks(t, tail) {
		steps[c.Lines[0].Text] = c
	}

	for text, want := range map[string]struct {
		step int64
		name string
	}{
		"setting up":    {0, ""},
		"holding":       {1, "Hold the runner"},
		"still holding": {1, "Hold the runner"},
		"cleaning":      {2, ""},
	} {
		c, ok := steps[text]
		if !ok {
			t.Errorf("no chunk holds %q", text)

			continue
		}
		if c.Step != want.step || c.StepName != want.name {
			t.Errorf("%q is step %d %q, want %d %q", text, c.Step, c.StepName, want.step, want.name)
		}
	}
}

// A CHUNK TOO LARGE FOR THE NODE IS HALVED UNTIL IT FITS, and every line reaches a chunk.
// A number is spent on each try, so a number is never used twice.
func TestAChunkIsCutToWhatTheNodeTakes(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)

	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "2026-11-04T07:04:45.1Z line %d %x\n", i, i*2654435761)
	}
	writePage(t, pages, pageName(stepRecord, 1), b.String())

	tail := tailerAt(t, diag)
	tail.scan()
	tail.read()

	lines := 0
	seen := map[uint64]bool{}

	for {
		data, ok := tail.nextChunk(4096)
		if !ok {
			break
		}
		if len(data) > 4096 {
			t.Fatalf("a chunk of %d bytes, past the limit", len(data))
		}
		c, err := joblog.Decode(data)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if seen[c.Seq] {
			t.Errorf("number %d was used twice", c.Seq)
		}
		seen[c.Seq] = true
		lines += len(c.Lines)
	}
	if lines != 2000 {
		t.Errorf("%d lines reached a chunk, want 2000", lines)
	}
	if tail.held != 0 {
		t.Errorf("the tailer still counts %d bytes held", tail.held)
	}
}

// A FULL BUFFER WAITS FOR THE NODE AND DOES NOT WAIT FOR ITSELF: what is held is numbered
// and can be sent, after which reading goes on.
func TestAFullBufferDoesNotWedge(t *testing.T) {
	t.Parallel()

	diag, pages := diagWithPages(t)
	writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1Z first\n")

	tail := tailerAt(t, diag)
	tail.held = logHeldLimit
	tail.scan()
	tail.read()
	if _, ok := tail.nextChunk(joblog.MaxChunkBytes); ok {
		t.Fatal("a tailer at its bound read on")
	}

	tail.held = 0
	tail.read()
	if _, ok := tail.nextChunk(joblog.MaxChunkBytes); !ok {
		t.Error("the tailer did not read on once there was room")
	}

	// AND LINES HELD WHEN THE BOUND IS REACHED ARE STILL NUMBERED AND SENT.
	writePage(t, pages, pageName(otherRecord, 1), "2026-11-04T07:04:46.1Z second\n")
	tail.scan()
	tail.read()
	tail.held = logHeldLimit
	tail.read()
	if _, ok := tail.nextChunk(joblog.MaxChunkBytes); !ok {
		t.Error("what was held at the bound cannot be sent")
	}
}

// sendRecorder is the node's log route, answering from a script.
type sendRecorder struct {
	mu      sync.Mutex
	script  []int
	bodies  [][]byte
	retries []string
}

func (s *sendRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.URL.Path != logsPath || req.Header.Get("Content-Type") != logContentType {
		return nil, errors.New("not the log route")
	}

	s.bodies = append(s.bodies, body)

	status := http.StatusNoContent
	if len(s.script) > 0 {
		status, s.script = s.script[0], s.script[1:]
	}

	header := http.Header{}
	if status == http.StatusTooManyRequests {
		header.Set("Retry-After", "3")
	}

	return &http.Response{StatusCode: status, Header: header, Body: http.NoBody, Request: req}, nil
}

func (s *sendRecorder) sent() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.bodies)
}

// shipperFor is a shipper over diag sending through rec.
func shipperFor(t *testing.T, diag string, rec http.RoundTripper) *logShipper {
	t.Helper()

	n, err := newNode("http://172.31.0.1:7718", testToken, rec)
	if err != nil {
		t.Fatalf("newNode: %v", err)
	}

	log, _ := discardLog()

	return &logShipper{node: n, tail: newLogTailer(diag, log), limit: joblog.MaxChunkBytes, period: time.Second,
		log: log, done: make(chan struct{})}
}

// THE NODE'S ANSWERS MEAN WHAT THE REPORT'S DO: a 429 waits and sends the same chunk, a 400
// drops it and goes on, and a 413, a 401 or a 404 ends the shipping, since the job's budget
// is spent or the node takes none.
func TestTheShipperDoesWhatTheNodeSays(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		script []int
		sends  int
		goesOn bool
	}{
		"accepted":       {nil, 2, true},
		"rate limited":   {[]int{http.StatusTooManyRequests}, 3, true},
		"refused":        {[]int{http.StatusBadRequest}, 2, true},
		"budget spent":   {[]int{http.StatusRequestEntityTooLarge}, 1, false},
		"no such route":  {[]int{http.StatusNotFound}, 1, false},
		"bearer refused": {[]int{http.StatusUnauthorized}, 1, false},
		"could not tell": {[]int{http.StatusInternalServerError}, 3, true},
	} {
		synctest.Test(t, func(t *testing.T) {
			diag, pages := diagWithPages(t)
			writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1Z one\n")
			writePage(t, pages, pageName(otherRecord, 1), "2026-11-04T07:04:46.1Z two\n")

			rec := &sendRecorder{script: tc.script}
			s := shipperFor(t, diag, rec)
			s.tail.scan()
			s.tail.read()

			if got := s.send(context.Background()); got != tc.goesOn {
				t.Errorf("%s: send answered %t, want %t", name, got, tc.goesOn)
			}
			if got := rec.sent(); got != tc.sends {
				t.Errorf("%s: %d sends, want %d", name, got, tc.sends)
			}
		})
	}
}

// WHAT IS LEFT REACHES THE NODE AFTER THE STOP, within its budget: the cleanup's lines exist
// only by then, since the runner has just exited.
func TestAStoppedShipperSendsWhatIsLeft(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		diag, pages := diagWithPages(t)
		writePage(t, pages, pageName(stepRecord, 1), "2026-11-04T07:04:45.1Z last words\n")

		rec := &sendRecorder{}
		s := shipperFor(t, diag, rec)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		s.finish(ctx)

		if rec.sent() != 1 {
			t.Fatalf("%d sends after the stop, want the one line left", rec.sent())
		}
		c, err := joblog.Decode(rec.bodies[0])
		if err != nil || c.Lines[0].Text != "last words" {
			t.Errorf("sent %+v, %v", c, err)
		}
	})
}

// A NODE THAT OFFERS NO HOME FOR A LOG GETS NONE, one whose offer cannot be read is no offer,
// and either way the report goes on.
func TestAnOfferIsReadOrIsNone(t *testing.T) {
	t.Parallel()

	for body, want := range map[string]*logLimits{
		`{"schemas":[1],"interval_ms":10000,"max_body":65536}`: nil,
		`{"schemas":[1],"interval_ms":10000,"max_body":65536,"logs":{"interval_ms":5000,"max_body":4096}}`: {
			interval: 5 * time.Second, maxBody: 4096,
		},
		`{"schemas":[1],"interval_ms":10000,"max_body":65536,"logs":{"interval_ms":5000,"max_body":9999999}}`: {
			interval: 5 * time.Second, maxBody: joblog.MaxChunkBytes,
		},
		`{"schemas":[1],"interval_ms":10000,"max_body":65536,"logs":{"interval_ms":0,"max_body":4096}}`: nil,
		`{"schemas":[1],"interval_ms":10000,"max_body":65536,"logs":{"max_body":4096}}`:                 nil,
	} {
		lim, v := readLimits([]byte(body))
		if v != accepted {
			t.Errorf("%s: answered %v, want accepted", body, v)

			continue
		}
		switch {
		case (lim.logs == nil) != (want == nil):
			t.Errorf("%s: offer %+v, want %+v", body, lim.logs, want)
		case want != nil && *lim.logs != *want:
			t.Errorf("%s: offer %+v, want %+v", body, *lim.logs, *want)
		}
	}
}

// NO OFFER, NO SHIPPING, whatever else the agent does: every older node is this.
func TestAnAgentOfferedNoHomeShipsNothing(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		r := start(t, cfg)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}
		// The fake answers the log route as a test failure, so reaching here is the proof
		// nothing was sent, and the report went on.
		if _, posts := node.snapshot(); len(posts) == 0 {
			t.Error("the report did not go on")
		}
	})
}
