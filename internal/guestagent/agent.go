// Package guestagent is billet's in-VM agent, `billet guest monitor`: it reads the
// guest's totals and processes from /proc, the steps the runner's log marks and,
// at the end, the job's JUnit results, and reports them to the node's guest-facing
// service in guestreport batches.
//
// WHAT IT REPORTS IS THE GUEST'S OWN, UNVERIFIED VIEW (guestreport says why), and
// it NEVER MAKES THE JOB FAIL: every path the node's contract calls a stop ends the
// agent with no error, a figure it could not read is not sent rather than sent as
// zero, its own errors go to its log without the guest's text, and everything it
// holds is bounded by one batch.
//
// It reads a process's stat, status and io, never its cmdline or environ, which can
// hold a secret.
package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// The final flush: from the request for it (SIGTERM or a connection to the
// socket) until the agent is done with it, inside the contract's 3 s; and how long
// a connection waits for the final batch's answer before it is answered anyway.
//
// exitBudget bounds the agent's return after SIGTERM whatever it is doing, a read
// that never returns included, and settleWait is how long the final flush waits
// for the answer to a request it cancelled.
const (
	finalBudget = 2750 * time.Millisecond
	exitBudget  = 2900 * time.Millisecond
	settleWait  = 250 * time.Millisecond
)

// flushAnswerWait is how long a connection to the socket waits for the final
// batch's answer before it is answered anyway. A VAR so a test over a real socket
// can lengthen it past its own guard, which is what tells a connection answered
// because the flush was done from one answered because this ran out; a test that
// replaces it must stay serial.
var flushAnswerWait = 2 * time.Second

// Config is what one agent reads and where it reports.
type Config struct {
	// Endpoint and Token are the node's guest-facing service and this guest's
	// bearer, from the environment.
	Endpoint string
	Token    string
	// Proc is the /proc tree the guest's totals and processes are read from.
	Proc fs.FS
	// DiagDir is the runner's diagnostics directory, whose Worker log marks the
	// steps; empty marks none.
	DiagDir string
	// JUnitRoots are searched for test results at the final flush.
	JUnitRoots []string
	// Socket is where a connection asks for the final flush; empty listens
	// nowhere.
	Socket string
	// Nice is the nice value the agent sets itself to; 0 leaves it as started.
	Nice int
	// Transport carries the requests to the node; nil is a direct connection.
	Transport http.RoundTripper
	// Log receives the agent's own errors.
	Log io.Writer
	// Version is the agent's, as each batch names it.
	Version string
}

// Run reports to the node until the job ends: a SIGTERM (ctx's cancellation) or a
// connection to the socket asks for the final flush, which sends one last batch.
// After a flush through the socket it sends nothing more and waits for ctx.
//
// It returns nil on every path, a node that is off, gone or full included.
func Run(ctx context.Context, cfg Config) error {
	log := newLimitedLog(cfg.Log)
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		supervised(ctx, cfg, log)
	}()

	select {
	case <-finished:
	case <-ctx.Done():
		// SIGTERM is answered within the contract's 3 s whatever holds the agent: a
		// read the job made never return, a socket that will not close, a log
		// nobody drains. The process ends with the command, and nothing here waits
		// on any of them, its own line included.
		wait := time.NewTimer(exitBudget)
		select {
		case <-finished:
		case <-wait.C:
			go log.always("the final flush did not finish in time; exiting without it")
		}

		wait.Stop()
	}

	return nil
}

func newAgent(n *node, cfg Config, log *limitedLog) *agent {
	return &agent{
		node:       n,
		col:        newCollector(cfg.Proc, log),
		steps:      newTailer(cfg.DiagDir, log),
		junitRoots: cfg.JUnitRoots,
		version:    textOrMark(guestreport.Clean(cfg.Version, guestreport.MaxNameBytes)),
		log:        log,
		results:    make(chan result, 1),
		flushed:    make(chan struct{}),
	}
}

// agent is one run's state. Only run's goroutine touches it; a request to the
// node runs on its own and hands its answer back through results.
type agent struct {
	node       *node
	col        *collector
	steps      *tailer
	junitRoots []string
	version    string
	log        *limitedLog

	seq uint64
	// lim is the node's limits, nil until a hello was answered.
	lim *limits
	// head is the batch cut and not yet accepted, resent as it is. A new batch
	// is cut only when there is none, so at most one batch is ever unsent and
	// what has not been cut waits, bounded, in the collector.
	head     *outgoing
	inflight bool
	// cancelInflight cancels the request in flight, which the final flush does
	// so that nothing it started is sent after the final batch.
	cancelInflight context.CancelFunc
	results        chan result

	started  time.Time
	lastSend time.Time
	backoff  time.Duration

	flushes <-chan struct{}
	// flushed is closed once the final flush is done, which answers every
	// connection waiting for it.
	flushed   chan struct{}
	closeOnce sync.Once
}

// flushDone answers every connection waiting for the final flush.
func (a *agent) flushDone() { a.closeOnce.Do(func() { close(a.flushed) }) }

// supervised is everything the agent does, from its first check of its
// configuration to its flush socket's teardown, so that Run can bound all of it
// by one deadline.
func supervised(ctx context.Context, cfg Config, log *limitedLog) {
	n, err := newNode(cfg.Endpoint, cfg.Token, cfg.Transport)
	if err != nil {
		log.always("%v; nothing is reported", err)

		return
	}

	if cfg.Proc == nil {
		log.always("no /proc tree to read; nothing is reported")

		return
	}

	if cfg.Nice != 0 {
		if err := LowerPriority(cfg.Nice); err != nil {
			log.always("running at the priority it started with: %v", err)
		}
	}

	newAgent(n, cfg, log).serve(ctx, cfg.Socket)
}

// serve runs the agent, and its flush socket when there is one.
func (a *agent) serve(ctx context.Context, socket string) {
	if socket != "" {
		l, err := listenFlush(socket)
		if err != nil {
			a.log.always("the flush socket was not opened, so only SIGTERM flushes: %v", err)
		} else {
			flushes := make(chan struct{}, 1)
			a.flushes = flushes
			done := make(chan struct{})

			go func() {
				defer close(done)

				serveFlush(l, flushes, a.flushed, a.log)
			}()

			defer func() {
				_ = l.Close()
				<-done
			}()
		}
	}

	a.run(ctx)
	// An agent that stopped without a final flush answers whoever is waiting.
	a.flushDone()
}

// outgoing is a batch as it is sent.
type outgoing struct {
	seq  uint64
	body []byte
}

// result is an answer from the node: to a hello, or to the batch in flight.
type result struct {
	hello   bool
	limits  limits
	verdict verdict
	wait    time.Duration
}

// run is the agent's loop. It returns when the agent has stopped.
func (a *agent) run(ctx context.Context) {
	a.started = time.Now()
	a.col.sample(false)
	a.col.walkProcesses(false)
	a.col.readAgentCPU()
	a.steps.poll()

	samples := time.NewTicker(guestreport.SampleInterval)
	defer samples.Stop()

	walks := time.NewTicker(guestreport.ProcessInterval)
	defer walks.Stop()

	next := time.NewTimer(0)
	defer next.Stop()

	for {
		select {
		case <-ctx.Done():
			a.final(ctx)

			return
		case <-a.flushes:
			samples.Stop()
			walks.Stop()
			next.Stop()
			a.final(ctx)
			<-ctx.Done()

			return
		case <-samples.C:
			a.col.sample(false)
			a.steps.poll()
		case <-walks.C:
			a.col.walkProcesses(false)
		case <-next.C:
			if at, again := a.contact(ctx); again {
				next.Reset(time.Until(at))
			}
		case r := <-a.results:
			at, ok := a.answer(r, time.Now())
			if !ok {
				return
			}

			next.Reset(time.Until(at))
		}
	}
}

// contact sends the node what is due: a hello until one is answered, then the
// unsent batch, or a new one. Its answer sets when to contact the node next; when
// nothing was sent, contact says when instead. The interval is counted from when a
// batch is really sent, not from when it fell due, so a loop held up past a due
// time never sends two batches closer than the interval.
func (a *agent) contact(ctx context.Context) (time.Time, bool) {
	if a.inflight {
		return time.Time{}, false
	}

	rctx, cancel := context.WithTimeout(ctx, requestTimeout)

	if a.lim == nil {
		a.inflight, a.cancelInflight = true, cancel

		go func() {
			defer cancel()

			lim, v := a.node.hello(rctx)
			a.results <- result{hello: true, limits: lim, verdict: v}
		}()

		return time.Time{}, false
	}

	if a.head == nil {
		b, ok := a.cut(nil)
		if !ok {
			cancel()

			a.lastSend = time.Now()

			return a.lastSend.Add(a.period()), true
		}

		a.head = &b
	}

	a.inflight, a.cancelInflight = true, cancel
	a.lastSend = time.Now()
	body := a.head.body

	go func() {
		defer cancel()

		v, wait := a.node.report(rctx, body, false)
		a.results <- result{verdict: v, wait: wait}
	}()

	return time.Time{}, false
}

// answer acts on the node's answer and says when to contact it next, or false when
// the agent stops.
func (a *agent) answer(r result, now time.Time) (time.Time, bool) {
	a.inflight, a.cancelInflight = false, nil

	if r.hello {
		switch r.verdict {
		case accepted:
			a.lim, a.backoff = &r.limits, 0
			// The first batch carries an interval of samples.
			return later(now, a.started.Add(a.period())), true
		case stop:
			a.log.always("the node does not take guest reports from this guest; nothing is reported")

			return time.Time{}, false
		default:
			a.backoff = min(max(2*a.backoff, firstBackoff), maxLimitBackoff)
			a.log.printf("hello", "the node's limits could not be read; asking again")

			return now.Add(a.backoff), true
		}
	}

	due := a.lastSend.Add(a.period())

	switch r.verdict {
	case accepted:
		a.head = nil
	case rateLimited:
		// The same batch, unchanged, after the node's wait and never sooner than
		// the interval.
		due = later(due, now.Add(r.wait))
	case refused:
		a.head = nil
	case stop:
		a.log.always("the node stopped taking this guest's reports; nothing more is sent")

		return time.Time{}, false
	default:
		a.log.printf("report", "the node's answer to a batch could not be told; it is sent again")
	}

	return due, true
}

// period is how often the agent sends: the node's interval, and never more often
// than guestreport.BatchInterval.
func (a *agent) period() time.Duration {
	return max(a.lim.interval, guestreport.BatchInterval)
}

// cut is a new batch of everything the agent holds, with tests when it is the
// final one, numbered next and encoded to fit what the node takes. A batch that
// cannot be encoded is dropped and its number spent, so the node sees the gap. No
// batch is cut while the agent's own CPU has never been read, because a batch must
// carry it and a zero would be a claim: what is held waits for the next try.
func (a *agent) cut(tests []guestreport.SuiteResult) (outgoing, bool) {
	a.col.readAgentCPU()

	if !a.col.haveAgentCPU {
		a.log.printf("self-unknown", "the agent's own CPU has never been read, so no batch is sent yet")

		return outgoing{}, false
	}

	a.seq++

	samples, processes := a.col.take()
	b := guestreport.Batch{
		Seq:            a.seq,
		AgentVersion:   a.version,
		TicksPerSecond: userHZ,
		AgentCPUTicks:  a.col.agentCPU,
		Samples:        samples,
		Processes:      processes,
		Steps:          a.steps.take(),
		Tests:          tests,
	}

	body, err := fit(&b, a.lim.maxBody)
	if err != nil {
		a.log.always("batch %d was dropped, as it could not be encoded within %d bytes: %v", a.seq, a.lim.maxBody, err)

		return outgoing{}, false
	}

	return outgoing{seq: a.seq, body: body}, true
}

// final takes a last reading, searches for test results, and sends the final
// batch, once, whatever asked for it. A request still in flight is cancelled first
// and its answer awaited a little, so nothing the agent started is sent after the
// final batch and a stop it brings is honoured. A batch still unsent is left: the
// final one carries what came after it, and the node sees its number missing.
func (a *agent) final(ctx context.Context) {
	defer a.flushDone()

	start := time.Now()

	fctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), start.Add(finalBudget))
	defer cancel()

	if !a.settle() {
		return
	}

	a.col.sample(true)
	a.col.walkProcesses(true)
	a.steps.poll()

	tests := collectJUnit(a.junitRoots, time.Now, a.log)

	if a.lim == nil {
		lim, v := a.node.hello(fctx)
		if v != accepted {
			a.log.always("the node's limits were not read in time; the final batch is not sent")

			return
		}

		a.lim = &lim
	}

	b, ok := a.cut(tests)
	if !ok {
		return
	}

	if v, _ := a.node.report(fctx, b.body, true); v != accepted {
		a.log.always("the node did not accept the final batch")
	}
}

// settle cancels the request in flight, if there is one, and acts on its answer
// if it comes within settleWait. It reports false when that answer is a stop.
func (a *agent) settle() bool {
	if !a.inflight {
		return true
	}

	a.cancelInflight()

	wait := time.NewTimer(settleWait)
	defer wait.Stop()

	select {
	case r := <-a.results:
		_, ok := a.answer(r, time.Now())

		return ok
	case <-wait.C:
		return true
	}
}

// fit encodes b within limit bytes. Only test results give way: the failed names
// are halved, then the lowest-ranked suites dropped, until it fits.
func fit(b *guestreport.Batch, limit int) ([]byte, error) {
	for {
		data, err := guestreport.EncodeBatch(*b)
		if err == nil && len(data) <= limit {
			return data, nil
		}

		if err != nil && !errors.Is(err, guestreport.ErrTooLarge) {
			return nil, err
		}

		if !shrinkTests(b) {
			return nil, errors.New("larger than the node takes")
		}
	}
}

// shrinkTests halves every suite's failed names, or, when there are none, drops
// the last suite. It reports false when there is nothing left to drop.
func shrinkTests(b *guestreport.Batch) bool {
	halved := false

	for i := range b.Tests {
		if n := len(b.Tests[i].Failed); n > 0 {
			b.Tests[i].Failed = b.Tests[i].Failed[:n/2]
			if n/2 == 0 {
				b.Tests[i].Failed = nil
			}

			halved = true
		}
	}

	if halved {
		return true
	}

	if len(b.Tests) == 0 {
		return false
	}

	b.Tests = b.Tests[:len(b.Tests)-1]
	if len(b.Tests) == 0 {
		b.Tests = nil
	}

	return true
}

// later is the later of two times.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// logEvery is how often one kind of recurring trouble is logged.
const logEvery = 5 * time.Minute

// limitedLog is the agent's own log: one line per event, a recurring kind of
// trouble at most once per logEvery. No line carries the guest's text. Its lock
// guards the record of what was logged and is never held across a write, which a
// log nobody drains can hold for good.
type limitedLog struct {
	mu   sync.Mutex
	w    io.Writer
	last map[string]time.Time
}

func newLimitedLog(w io.Writer) *limitedLog {
	if w == nil {
		w = io.Discard
	}

	return &limitedLog{w: w, last: map[string]time.Time{}}
}

// printf logs one line of a recurring kind, unless that kind was logged within
// logEvery.
func (l *limitedLog) printf(kind, format string, args ...any) {
	l.mu.Lock()

	now := time.Now()
	if at, ok := l.last[kind]; ok && now.Sub(at) < logEvery {
		l.mu.Unlock()

		return
	}

	l.last[kind] = now
	l.mu.Unlock()

	l.always(format, args...)
}

// always logs one line, in one write.
func (l *limitedLog) always(format string, args ...any) {
	fmt.Fprintf(l.w, "billet guest monitor: "+format+"\n", args...)
}
