package guestagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// heldFS is a /proc tree whose opens a test can hold: hold is called with each
// name before it is opened.
type heldFS struct {
	fs.FS

	hold func(name string)
}

func (h heldFS) Open(name string) (fs.File, error) {
	h.hold(name)

	return h.FS.Open(name)
}

// whenever runs do once, the first time cond holds.
type whenever struct {
	mu   sync.Mutex
	done bool
}

func (w *whenever) once(cond bool, do func()) {
	w.mu.Lock()
	run := cond && !w.done
	w.done = w.done || run
	w.mu.Unlock()

	if run {
		do()
	}
}

// The job is root in its VM and can put a FIFO, or a mount that never answers, at
// any name the agent reads. Whatever holds the loop, SIGTERM is answered within
// the contract's 3 s: the agent returns nil and the process ends with it.
func TestSIGTERMIsAnsweredWithinThreeSecondsWhateverHoldsTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		release := make(chan struct{})
		began := time.Now()

		var w whenever

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		cfg.Proc = heldFS{FS: cfg.Proc, hold: func(name string) {
			w.once(name == "stat" && time.Since(began) > 5*time.Second, func() { <-release })
		}}

		r := start(t, cfg)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		took, err := r.stop()
		if err != nil || took > 3*time.Second {
			t.Fatalf("SIGTERM with the loop held returned %v after %v, want nil within 3s", err, took)
		}

		// The line is written from a goroutine of its own, so nothing waits on it.
		synctest.Wait()

		if !strings.Contains(log.String(), "did not finish in time") {
			t.Errorf("the log does not say the flush was given up: %q", log.String())
		}

		close(release)
	})
}

// The /proc the agent reads is opened without waiting: a FIFO at a name answers at
// once and is refused, never read as a figure.
func TestTheProcTreeRefusesWhatIsNotAFileOrADirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if err := os.CopyFS(root, os.DirFS("testdata/proc")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "stat")); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mkfifo(filepath.Join(root, "stat"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, logged := discardLog()
	c := newCollector(ProcFS(root), log)

	done := make(chan struct{})

	go func() {
		defer close(done)

		c.sample(at(0), false)
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		t.Fatal("a sample waited on a FIFO")
	}

	if len(c.samples) != 0 || !strings.Contains(logged.String(), "neither a regular file nor a directory") {
		t.Fatalf("kept %d samples and logged %q; want none, and the refusal", len(c.samples), logged.String())
	}

	if err := os.Remove(filepath.Join(root, "stat")); err != nil {
		t.Fatal(err)
	}

	if err := os.CopyFS(filepath.Join(root, "again"), os.DirFS("testdata/proc")); err != nil {
		t.Fatal(err)
	}

	c = newCollector(ProcFS(filepath.Join(root, "again")), log)
	c.sample(at(0), false)
	c.walkProcesses(at(0), false)
	c.walkProcesses(at(2*time.Second), false)

	if len(c.samples) != 1 || c.samples[0] != fixtureSample || len(c.processes) != 1 {
		t.Fatalf("the fixture through ProcFS gave %d samples and %d process samples", len(c.samples),
			len(c.processes))
	}

	if _, err := ProcFS(root).Open("../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("a name out of the tree answered %v", err)
	}
}

// A FIFO at the diagnostics directory's name, or at a JUnit root's, is refused at
// once rather than waited on.
func TestADirectoryTheJobReplacedWithAFIFOIsNotWaitedOn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fifo := filepath.Join(dir, "_diag")

	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	log, logged := discardLog()
	done := make(chan []guestreport.SuiteResult, 1)

	go func() {
		newTailer(fifo, log).poll()
		done <- collectJUnit([]string{fifo, "testdata/junit"}, time.Now, log)
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case suites := <-done:
		if len(suites) != 4 {
			t.Errorf("the search past a FIFO root kept %d suites, want the fixture's 4", len(suites))
		}
	case <-timer.C:
		t.Fatal("a poll or a search waited on a FIFO")
	}

	if !strings.Contains(logged.String(), "diagnostics directory was not read") ||
		!strings.Contains(logged.String(), "root was not opened") {
		t.Errorf("the refusals were not logged: %q", logged.String())
	}
}

// A socket flush while a batch is in flight cancels it and takes its answer first,
// so nothing the agent started reaches the node after the final batch.
func TestAFlushCancelsTheRequestInFlightBeforeTheFinalBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.postScript = []reply{{hold: true}}

		var log syncBuffer

		cfg := testConfig(t, node, &log)

		n, err := newNode(cfg.Endpoint, cfg.Token, cfg.Transport)
		if err != nil {
			t.Fatal(err)
		}

		a := newAgent(n, cfg, newLimitedLog(&log))
		flushes := make(chan struct{}, 1)
		a.flushes = flushes

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})

		go func() {
			defer close(done)

			a.run(ctx)
		}()

		time.Sleep(12 * time.Second)
		synctest.Wait()

		flushed := time.Now()
		flushes <- struct{}{}

		time.Sleep(time.Minute)
		synctest.Wait()

		node.mu.Lock()
		held, ended := node.held, append([]time.Time(nil), node.heldEnded...)
		node.mu.Unlock()

		_, posts := node.snapshot()
		if held != 1 || len(ended) != 1 || len(posts) != 1 || !posts[0].final {
			t.Fatalf("%d held (%d ended) and %d posts; want the held batch cancelled and the final alone", held,
				len(ended), len(posts))
		}

		if ended[0].After(posts[0].at) || ended[0].Sub(flushed) > settleWait {
			t.Errorf("the held batch ended %v after the flush, the final went %v after it", ended[0].Sub(flushed),
				posts[0].at.Sub(flushed))
		}

		cancel()
		<-done
	})
}

// A stop the request in flight brings is honoured: no final batch follows it.
func TestAStopInFlightAtTheFlushSendsNoFinalBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)

		var log syncBuffer

		cfg := testConfig(t, node, &log)

		n, err := newNode(cfg.Endpoint, cfg.Token, cfg.Transport)
		if err != nil {
			t.Fatal(err)
		}

		a := newAgent(n, cfg, newLimitedLog(&log))
		a.lim = &limits{interval: 10 * time.Second, maxBody: guestreport.MaxBatchBytes}

		cancelled := false
		a.inflight, a.cancelInflight = true, func() { cancelled = true }
		a.results <- result{verdict: stop}

		a.final(t.Context())

		if _, posts := node.snapshot(); len(posts) != 0 || !cancelled {
			t.Fatalf("after a stop in flight, %d posts (cancelled %v); want none, and the request cancelled",
				len(posts), cancelled)
		}
	})
}

// The interval runs from when a batch is really sent: a loop held past a batch's
// due time sends it late and the next one a full interval after that, never two
// batches closer than the interval.
func TestABatchSentLateStartsTheIntervalFromWhenItWent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		began := time.Now()

		var w whenever

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		cfg.Proc = heldFS{FS: cfg.Proc, hold: func(name string) {
			w.once(name == "stat" && time.Since(began) >= 8500*time.Millisecond, func() { time.Sleep(8 * time.Second) })
		}}

		r := start(t, cfg)

		time.Sleep(45 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()
		if len(posts) < 3 || posts[0].at.Sub(began) < 16*time.Second {
			t.Fatalf("%d posts, the first at %v; want it late, the loop held from 9s to 17s", len(posts),
				posts[0].at.Sub(began))
		}

		for i := 1; i < len(posts)-1; i++ {
			if gap := posts[i].at.Sub(posts[i-1].at); gap < 10*time.Second {
				t.Errorf("batches %d and %d went %v apart, under the interval", i, i+1, gap)
			}
		}
	})
}

// No batch is cut while the agent's own CPU has never been read: a batch must
// carry it, and a zero would be a claim. What is held waits and goes once it is.
func TestNoBatchGoesBeforeTheAgentsOwnCPUIsRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		began := time.Now()

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		w := watch(t, cfg.Proc)
		w.fail["self/stat"] = eacces
		cfg.Proc = w

		r := start(t, cfg)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		if _, posts := node.snapshot(); len(posts) != 0 {
			t.Fatalf("%d batches went with the agent's CPU never read", len(posts))
		}

		w.mu.Lock()
		delete(w.fail, "self/stat")
		w.mu.Unlock()

		time.Sleep(10 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()
		if len(posts) < 1 || posts[0].batch.Seq != 1 || posts[0].at.Sub(began) != 20*time.Second ||
			posts[0].batch.AgentCPUTicks != 60 || len(posts[0].batch.Samples) < 20 {
			t.Fatalf("%d posts; want batch 1 at 20s, carrying the agent's 60 ticks and the 20s held", len(posts))
		}

		if !strings.Contains(log.String(), "never been read") {
			t.Errorf("the log does not say why nothing went: %q", log.String())
		}
	})
}

// brokenBody is an answer's body that fails partway.
type brokenBody struct{ io.Reader }

func (brokenBody) Close() error { return nil }

// statusTransport answers every request with status and header, and a body that
// fails after a whole limits answer, so only the failure says it was cut.
type statusTransport struct {
	status int
	header http.Header
}

func (s statusTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: s.status, Header: s.header, Body: brokenBody{io.MultiReader(
		strings.NewReader(`{"schemas":[1],"interval_ms":10000,"max_body":65536}`), iotestErr{})}}, nil
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A report's answer is its status and headers: a body that fails does not turn a
// stop, a refusal or a 429 into an answer nobody can tell. The limits are the
// body, so there it does.
func TestAnAnswersVerdictDoesNotHangOnItsBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status int
		header http.Header
		want   verdict
		wait   time.Duration
	}{
		{http.StatusRequestEntityTooLarge, nil, stop, 0},
		{http.StatusUnauthorized, nil, stop, 0},
		{http.StatusNotFound, nil, stop, 0},
		{http.StatusBadRequest, nil, refused, 0},
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, rateLimited, 7 * time.Second},
		{http.StatusNoContent, nil, accepted, 0},
	} {
		n, err := newNode("http://172.31.0.1:7718", testToken, statusTransport{status: tc.status, header: tc.header})
		if err != nil {
			t.Fatal(err)
		}

		if v, wait := n.report(t.Context(), []byte("x"), false); v != tc.want || wait != tc.wait {
			t.Errorf("%d with a broken body is %v (wait %v), want %v (wait %v)", tc.status, v, wait, tc.want, tc.wait)
		}
	}

	n, err := newNode("http://172.31.0.1:7718", testToken, statusTransport{status: http.StatusOK})
	if err != nil {
		t.Fatal(err)
	}

	if _, v := n.hello(t.Context()); v != unknown {
		t.Errorf("limits with a broken body are %v, want could-not-tell", v)
	}
}

// A process's io names each counter once; one named twice, or one missing, is
// could-not-tell rather than a zero.
func TestAProcessIOFileWithACounterTwiceIsCouldNotTell(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		"read_bytes: 10\nread_bytes: 20\n",
		"read_bytes: 10\nwrite_bytes: 1\nwrite_bytes: 2\n",
		"read_bytes: 10\n",
		"",
	} {
		if r, w, err := ioBytes([]byte(data)); !errors.Is(err, errCouldNotTell) {
			t.Errorf("%q read as %d and %d (%v), want could-not-tell", data, r, w, err)
		}
	}

	if r, w, err := ioBytes([]byte("rchar: 5\nread_bytes: 10\nwrite_bytes: 20\n")); err != nil || r != 10 || w != 20 {
		t.Errorf("a whole io file read as %d and %d (%v)", r, w, err)
	}
}

// A result file of many small suites costs a batch's worth to hold, not one per
// suite, and keeps the ones that rank first.
func TestAFileOfManySuitesIsHeldABatchsWorthAtATime(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer

	b.WriteString("<testsuites>")

	for i := range 5000 {
		failure := ""
		if i == 4321 {
			failure = "<failure/>"
		}

		fmt.Fprintf(&b, `<testsuite name="s%d"><testcase name="t">%s</testcase></testsuite>`, i, failure)
	}

	b.WriteString("</testsuites>")

	w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}

	suites, ok := w.parse(&b, "many.xml")
	if !ok || len(suites) != guestreport.MaxBatchSuites || suites[0].Name != "s4321" {
		t.Fatalf("kept %d suites (ok %v), the first %q; want a batch's worth, the failing one first", len(suites),
			ok, suites[0].Name)
	}
}

// The socket's directory is searchable by the runner's user whatever the umask
// made of it, and set through the descriptor that was judged.
func TestTheSocketsDirectoryIsOpenedToTheRunnersUser(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(socketDir(t), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	l, err := listenFlush(filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}

	_ = l.Close()

	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the directory is %v (%v), want 0755", info.Mode().Perm(), err)
	}
}
