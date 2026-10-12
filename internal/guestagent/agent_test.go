package guestagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// testConfig is an agent reading the fixtures and reporting to node.
func testConfig(t *testing.T, node *fakeNode, log *syncBuffer) Config {
	t.Helper()

	return Config{
		Endpoint:   "http://172.31.0.1:7718",
		Token:      testToken,
		Proc:       fixtureProc(t),
		DiagDir:    "testdata/diag",
		JUnitRoots: []string{"testdata/junit"},
		Transport:  node,
		Log:        log,
		Version:    "v0.15.0",
	}
}

// running is an agent started in the bubble: stop cancels its context as SIGTERM
// does and answers how long it took to return.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, cfg Config) *running {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan error, 1)}

	go func() { r.done <- Run(ctx, cfg) }()

	return r
}

// stop is SIGTERM: it answers how long the agent took to return, and its error.
func (r *running) stop() (time.Duration, error) {
	at := time.Now()
	r.cancel()
	err := <-r.done

	return time.Since(at), err
}

// returned reports whether the agent has returned, and with what.
func (r *running) returned() (bool, error) {
	synctest.Wait()

	select {
	case err := <-r.done:
		return true, err
	default:
		return false, nil
	}
}

// A healthy run: one hello, a batch every interval numbered from 1, every one of
// them what the fixtures hold, and on SIGTERM exactly one final batch, carrying the
// test results, within 3 s.
func TestAHealthyRunSendsABatchEachIntervalAndOneFinalOnSIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)

		var log syncBuffer

		began := time.Now()
		r := start(t, testConfig(t, node, &log))

		time.Sleep(35 * time.Second)
		synctest.Wait()

		took, err := r.stop()
		if err != nil || took > 3*time.Second {
			t.Fatalf("SIGTERM returned %v after %v, want nil within 3s", err, took)
		}

		hellos, posts := node.snapshot()
		if len(hellos) != 1 || len(posts) != 4 {
			t.Fatalf("%d hellos and %d posts, want 1 and 4 (log: %s)", len(hellos), len(posts), log.String())
		}

		for i, p := range posts {
			if p.batch.Seq != uint64(i+1) || p.final != (i == 3) || p.status != http.StatusNoContent {
				t.Errorf("post %d: seq %d, final %v, status %d", i, p.batch.Seq, p.final, p.status)
			}

			if i < 3 && p.at.Sub(began) != time.Duration(i+1)*10*time.Second {
				t.Errorf("batch %d was sent at %v, want %v", i+1, p.at.Sub(began), time.Duration(i+1)*10*time.Second)
			}

			if p.batch.AgentVersion != "v0.15.0" || p.batch.TicksPerSecond != 100 || p.batch.AgentCPUTicks != 60 {
				t.Errorf("batch %d's header: %q, %d ticks/s, %d agent ticks", i+1, p.batch.AgentVersion,
					p.batch.TicksPerSecond, p.batch.AgentCPUTicks)
			}
		}

		var last int64

		for _, p := range posts {
			if len(p.batch.Samples) == 0 || len(p.batch.Processes) == 0 {
				t.Errorf("batch %d carries %d samples and %d process samples", p.batch.Seq, len(p.batch.Samples),
					len(p.batch.Processes))
			}

			for _, s := range p.batch.Samples {
				want := fixtureSample
				want.AtMillis = s.AtMillis

				if s != want || s.AtMillis <= last {
					t.Errorf("batch %d's sample %+v is not the fixture's, in order", p.batch.Seq, s)
				}

				last = s.AtMillis
			}
		}

		if got := posts[0].batch.Steps; len(got) != 4 || got[1].Name != "Mark one" {
			t.Errorf("the first batch's steps %+v, want the fixture log's four", got)
		}

		for _, p := range posts[1:] {
			if len(p.batch.Steps) != 0 {
				t.Errorf("batch %d resends steps %+v", p.batch.Seq, p.batch.Steps)
			}
		}

		if len(posts[3].batch.Tests) != 4 || len(posts[0].batch.Tests) != 0 {
			t.Errorf("tests in the final batch %d, in the first %d; want 4 and none", len(posts[3].batch.Tests),
				len(posts[0].batch.Tests))
		}
	})
}

// The node says when guest monitoring is off (404), when this bearer has no
// session (401), and when it speaks no schema the agent does: the agent sends
// nothing, asks nothing more, and returns nil at once.
func TestTheAgentSendsNothingToANodeThatTakesNoReports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply reply
	}{
		{"monitoring off", reply{status: http.StatusNotFound}},
		{"no session", reply{status: http.StatusUnauthorized}},
		{"another schema", reply{status: http.StatusOK, body: `{"schemas":[2],"interval_ms":10000,"max_body":65536}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				node := newFakeNode(t)
				node.helloScript = []reply{tc.reply}

				var log syncBuffer

				r := start(t, testConfig(t, node, &log))

				done, err := r.returned()
				if !done || err != nil {
					t.Fatalf("the agent returned %v (%v), want at once with nil", done, err)
				}

				time.Sleep(time.Minute)

				hellos, posts := node.snapshot()
				if len(hellos) != 1 || len(posts) != 0 {
					t.Fatalf("%d hellos and %d posts, want 1 and none", len(hellos), len(posts))
				}

				if !strings.Contains(log.String(), "does not take guest reports") {
					t.Errorf("the log says %q", log.String())
				}
			})
		})
	}
}

// A hello nobody can answer is could-not-tell: the agent keeps collecting and asks
// again with a backoff that doubles, and sends once it is answered.
func TestAHelloNobodyAnswersIsAskedAgainWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.helloScript = []reply{
			{status: http.StatusServiceUnavailable},
			{drop: true},
			{status: http.StatusOK, body: `{"schemas":[1]}`},
			{status: http.StatusOK, body: `{"schemas":[1],"interval_ms":0,"max_body":65536}`},
			{status: http.StatusTemporaryRedirect},
		}

		var log syncBuffer

		began := time.Now()
		r := start(t, testConfig(t, node, &log))

		time.Sleep(40 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		hellos, posts := node.snapshot()

		var times []time.Duration
		for _, h := range hellos {
			times = append(times, h.Sub(began))
		}

		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 31 * time.Second}
		if fmt.Sprint(times) != fmt.Sprint(want) {
			t.Fatalf("hellos at %v, want %v", times, want)
		}

		// The first batch goes as soon as the hello is answered, the interval
		// since the agent started having passed, and carries what it held. The
		// answer and the 31 s sample fall due at one instant and the loop takes
		// them in either order, so that sample is in batch 1 or the final, and
		// together the two carry each of the 41 exactly once.
		if len(posts) != 2 || posts[0].at.Sub(began) != 31*time.Second || posts[0].batch.Seq != 1 ||
			!posts[1].final || posts[1].batch.Seq != 2 {
			t.Fatalf("%d posts, want batch 1 at 31s, then the final", len(posts))
		}

		var stamps []time.Duration
		for _, p := range posts {
			for _, s := range p.batch.Samples {
				stamps = append(stamps, time.UnixMilli(s.AtMillis).Sub(began))
			}
		}

		var every []time.Duration
		for s := range 41 {
			every = append(every, time.Duration(s)*time.Second)
		}

		if first := len(posts[0].batch.Samples); (first != 31 && first != 32) || fmt.Sprint(stamps) != fmt.Sprint(every) {
			t.Fatalf("batch 1 carries %d samples, and the two carry them at %v, want 31 or 32 in batch 1 and one each second from 0 to 40 s",
				first, stamps)
		}
	})
}

// A 429 is waited out, at most 60 s and never sooner than the interval, and the
// same batch is resent byte for byte.
func TestA429IsWaitedOutAndTheSameBatchResent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.postScript = []reply{
			{status: http.StatusTooManyRequests, retryAfter: "30"},
			{status: http.StatusTooManyRequests, retryAfter: "3600"},
			{status: http.StatusTooManyRequests, retryAfter: "1"},
			{status: http.StatusTooManyRequests, retryAfter: "soon"},
		}

		var log syncBuffer

		began := time.Now()
		r := start(t, testConfig(t, node, &log))

		time.Sleep(135 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()

		want := []struct {
			at  time.Duration
			seq uint64
		}{{10 * time.Second, 1}, {40 * time.Second, 1}, {100 * time.Second, 1}, {110 * time.Second, 1}, {120 * time.Second, 1}, {130 * time.Second, 2}}

		if len(posts) < len(want) {
			t.Fatalf("%d posts, want at least %d", len(posts), len(want))
		}

		for i, w := range want {
			if posts[i].at.Sub(began) != w.at || posts[i].batch.Seq != w.seq || posts[i].final {
				t.Errorf("post %d: seq %d at %v (final %v), want seq %d at %v", i, posts[i].batch.Seq,
					posts[i].at.Sub(began), posts[i].final, w.seq, w.at)
			}

			if w.seq == 1 && !bytes.Equal(posts[i].body, posts[0].body) {
				t.Errorf("post %d resent batch 1 with other bytes", i)
			}
		}
	})
}

// 413 (the job's budget is spent), 401 and 404 (the session or the route is gone)
// stop the agent for good: it returns nil, and sends nothing more, not even a
// final batch.
func TestAStopAnswerEndsTheAgent(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				node := newFakeNode(t)
				node.postScript = []reply{{status: http.StatusNoContent}, {status: status}}

				var log syncBuffer

				r := start(t, testConfig(t, node, &log))

				time.Sleep(25 * time.Second)

				done, err := r.returned()
				if !done || err != nil {
					t.Fatalf("after a %d the agent returned %v (%v), want nil", status, done, err)
				}

				time.Sleep(time.Minute)

				if _, posts := node.snapshot(); len(posts) != 2 || finals(posts) != 0 {
					t.Fatalf("%d posts (%d final), want the two before the stop", len(posts), finals(posts))
				}
			})
		})
	}
}

// A 400 is the node refusing this batch: it is dropped, and the next is a new one.
func TestA400DropsTheBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.postScript = []reply{{status: http.StatusBadRequest}}

		var log syncBuffer

		r := start(t, testConfig(t, node, &log))

		time.Sleep(25 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()
		if len(posts) != 3 || posts[0].batch.Seq != 1 || posts[1].batch.Seq != 2 || posts[2].batch.Seq != 3 {
			t.Fatalf("posts %d, want seqs 1, 2 and the final 3", len(posts))
		}
	})
}

// An answer the contract does not name, or none, is could-not-tell: the same batch
// is resent at the next interval, and what has not been cut since waits in the
// collector, so the agent never holds more than one batch unsent, never 4, and
// numbers its batches without a gap.
func TestAnAnswerNobodyCanTellResendsTheSameBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.postScript = []reply{
			{status: http.StatusInternalServerError},
			{drop: true},
			{status: http.StatusTemporaryRedirect, location: "http://elsewhere.example/"},
			{status: http.StatusOK},
			{drop: true},
			{status: http.StatusBadGateway},
		}

		var log syncBuffer

		r := start(t, testConfig(t, node, &log))

		time.Sleep(85 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()

		var seqs []uint64
		for _, p := range posts {
			seqs = append(seqs, p.batch.Seq)
		}

		if fmt.Sprint(seqs) != "[1 1 1 1 1 1 1 2 3]" {
			t.Fatalf("seqs %v, want batch 1 seven times, then 2, then the final 3", seqs)
		}

		for i := range 7 {
			if !bytes.Equal(posts[i].body, posts[0].body) {
				t.Errorf("resend %d of batch 1 carried other bytes", i)
			}
		}

		// Batch 2 carries the minute the agent held while batch 1 went unanswered,
		// up to a batch's bound less the place kept for the final reading.
		if n := len(posts[7].batch.Samples); n != guestreport.MaxBatchSamples-1 {
			t.Errorf("batch 2 carries %d samples, want the %d the collector held", n, guestreport.MaxBatchSamples-1)
		}
	})
}

// The node's interval is how often the agent may send, and the agent never sends
// more often than guestreport.BatchInterval whatever the node says.
func TestTheAgentSendsNoMoreOftenThanTheInterval(t *testing.T) {
	for _, tc := range []struct {
		intervalMS int64
		every      time.Duration
	}{{1000, 10 * time.Second}, {30_000, 30 * time.Second}} {
		t.Run(fmt.Sprint(tc.intervalMS), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				node := newFakeNode(t)
				node.intervalMS = tc.intervalMS

				var log syncBuffer

				r := start(t, testConfig(t, node, &log))

				time.Sleep(95 * time.Second)
				synctest.Wait()

				if _, err := r.stop(); err != nil {
					t.Fatal(err)
				}

				_, posts := node.snapshot()
				if len(posts) != int(95*time.Second/tc.every)+1 {
					t.Fatalf("%d posts, want one every %v and the final", len(posts), tc.every)
				}

				for i := 1; i < len(posts)-1; i++ {
					if gap := posts[i].at.Sub(posts[i-1].at); gap != tc.every {
						t.Errorf("batches %d and %d were %v apart, want %v", i, i+1, gap, tc.every)
					}
				}
			})
		})
	}
}

// SIGTERM before any hello was answered still sends the final batch, once, after
// asking for the limits.
func TestSIGTERMBeforeAHelloStillSendsTheFinalBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.helloScript = []reply{{drop: true}}

		var log syncBuffer

		r := start(t, testConfig(t, node, &log))

		time.Sleep(500 * time.Millisecond)
		synctest.Wait()

		took, err := r.stop()
		if err != nil || took > 3*time.Second {
			t.Fatalf("returned %v after %v", err, took)
		}

		hellos, posts := node.snapshot()
		if len(hellos) != 2 || len(posts) != 1 || !posts[0].final || posts[0].batch.Seq != 1 {
			t.Fatalf("%d hellos and %d posts, want 2 and the final alone", len(hellos), len(posts))
		}
	})
}

// The node's max_body bounds every body; the test results give way, never the
// readings, and a batch that cannot fit is dropped rather than sent over.
func TestTheFinalBatchFitsTheNodesMaxBody(t *testing.T) {
	root := t.TempDir()

	// Names that do not compress, none twice.
	for i := range 4 {
		var cases strings.Builder
		for j := range guestreport.MaxFailedNames {
			sum := sha256.Sum256([]byte{byte(i), byte(j)})
			fmt.Fprintf(&cases, `<testcase name="%x%x"><failure/></testcase>`, sum, sum[:16])
		}

		writeFile(t, filepath.Join(root, fmt.Sprintf("r%d.xml", i)),
			fmt.Sprintf(`<testsuite name="s%d">%s</testsuite>`, i, cases.String()))
	}

	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		node.maxBody = 6000

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		cfg.JUnitRoots = []string{root}
		r := start(t, cfg)

		time.Sleep(15 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()
		final := posts[len(posts)-1]

		if !final.final || len(final.body) > 6000 || len(final.batch.Tests) == 0 {
			t.Fatalf("the final batch: %d bytes, %d suites (final %v); want within 6000 with suites", len(final.body),
				len(final.batch.Tests), final.final)
		}

		if len(final.batch.Tests[0].Failed) >= guestreport.MaxFailedNames || final.batch.Tests[0].Failures != 50 {
			t.Errorf("the names were not halved (%d) or the count changed with them (%d)",
				len(final.batch.Tests[0].Failed), final.batch.Tests[0].Failures)
		}
	})
}

func TestABatchThatCannotFitIsRefusedNotCut(t *testing.T) {
	t.Parallel()

	b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Samples: []guestreport.Sample{fixtureSample}}

	if _, err := fit(&b, 10); err == nil {
		t.Fatal("a batch over its limit with nothing to give way was encoded")
	}

	if len(b.Samples) != 1 {
		t.Fatalf("fit cut the readings: %+v", b.Samples)
	}
}
