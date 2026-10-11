// An external test package, so the agent it imports joins the node's tests here and
// nowhere else: imported into package node's own tests it changed what gosec's
// taint analysis concluded about the whole package (measured 2026-10-11, fourteen
// findings in files this test does not touch).
package node_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/junioryono/billet/internal/guestagent"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// guestTransport carries the agent's requests to a cache service in-process, as
// the node's guest listener would. It counts the hellos, and for each final batch
// notes the node's answer and whether the agent's run had been cancelled (its
// SIGTERM) when the batch arrived.
type guestTransport struct {
	service *node.CacheService
	run     context.Context

	mu            sync.Mutex
	hellos        int
	finals        []int
	finalsEarly   int
	reportsBefore int
}

func (g *guestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	rec := httptest.NewRecorder()
	g.service.ServeHTTP(rec, req)

	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case req.URL.Path == "/v1/guest/limits":
		g.hellos++
	case req.URL.Path == "/v1/guest/reports" && req.URL.RawQuery == "final=1":
		g.finals = append(g.finals, rec.Code)
		if g.run.Err() == nil {
			g.finalsEarly++
		}
	case req.URL.Path == "/v1/guest/reports" && g.run.Err() == nil:
		g.reportsBefore++
	}

	return rec.Result(), nil
}

func (g *guestTransport) counts() (hellos int, finals []int, early, before int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.hellos, slices.Clone(g.finals), g.finalsEarly, g.reportsBefore
}

// lockedBuffer is the agent's log, which its goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

// The marks and the suites the agent reads from internal/guestagent's fixtures,
// field by field: the runner's Worker log of the probe of 2026-10-10, and the Go,
// pytest and nested result files under testdata/junit.
func fixtureMarks() []guestreport.StepMark {
	// The second of 2026-10-10T02:04 each line names.
	at := func(s int) int64 { return time.Date(2026, 10, 10, 2, 4, s, 0, time.UTC).Unix() }

	return []guestreport.StepMark{
		{Name: "Set up runner", AtSeconds: at(17)},
		{Name: "Mark one", AtSeconds: at(20)},
		{Name: `Run it's "quoted" ('twice')`, AtSeconds: at(21)},
		{Name: "Post Run actions/checkout@v5", AtSeconds: at(34)},
	}
}

func fixtureSuites() []guestreport.SuiteResult {
	return []guestreport.SuiteResult{
		{Name: "github.com/acme/widget/internal/parse", Tests: 4, Failures: 1, Skipped: 1,
			Failed: []string{`github.com/acme/widget/internal/parse.TestParseQuoted/it's_"quoted"`}},
		{Name: "github.com/acme/widget/internal/render", Tests: 3, Failures: 1,
			Failed: []string{"github.com/acme/widget/internal/render.TestRenderWide"}},
		{Name: "pytest", Tests: 3, Errors: 1, Failed: []string{"tests.test_api.test_post"}},
		{Name: "six levels down", Tests: 1},
	}
}

// THE IN-VM AGENT SPEAKS THIS NODE'S GUEST ROUTES, not only a stand-in for them:
// billet guest monitor's agent says hello with the bearer the node minted, sends
// nothing more before its interval or its SIGTERM, then sends one final batch the
// node accepts with 204, which the node keeps and hands over at the destroy as the
// guest's own report: the hello seen, one batch accepted and none refused, the
// final one among them, and the steps and test results the agent read from the
// fixtures it was pointed at, every field. The guest routes reach no volume, so
// the service's store and attacher are empty, and any call to one fails the test.
//
// IN A SYNCTEST BUBBLE, because the transport is in-process: synctest.Wait is the
// barrier that says the agent has done all it will do before its SIGTERM, the
// agent's 3 s is fake time, and the bubble ends only when every goroutine the
// agent started has, so a worker left running fails the test (measured
// 2026-10-11: a cache service starts no goroutine that outlives its bubble).
func TestTheInVMAgentSpeaksTheGuestRoutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := node.NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
			struct{ storecontract.Store }{}, struct{ provider.VolumeAttacher }{}, nil)
		if err != nil {
			t.Fatalf("NewCacheService: %v", err)
		}

		service.MonitorGuests()

		credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		transport := &guestTransport{service: service, run: ctx}

		var log lockedBuffer

		done := make(chan struct{})

		var runErr error

		go func() {
			defer close(done)

			runErr = guestagent.Run(ctx, guestagent.Config{
				Endpoint:   "http://172.20.0.1:7718",
				Token:      credentials.Token,
				Proc:       os.DirFS("../guestagent/testdata/proc"),
				DiagDir:    "../guestagent/testdata/diag",
				JUnitRoots: []string{"../guestagent/testdata/junit"},
				Transport:  transport,
				Log:        &log,
				Version:    "v0.15.0",
			})
		}()

		// Short of its ten-second interval, an agent that has said hello has
		// nothing to send until its SIGTERM.
		time.Sleep(5 * time.Second)
		synctest.Wait()

		select {
		case <-done:
			t.Fatalf("the agent returned %v before its SIGTERM", runErr)
		default:
		}

		if hellos, finals, _, before := transport.counts(); hellos != 1 || len(finals) != 0 || before != 0 {
			t.Fatalf("before its SIGTERM the agent said hello %d times and sent %d final and %d other batches; "+
				"want 1, 0, 0", hellos, len(finals), before)
		}

		stopped := time.Now()
		cancel()
		<-done

		if took := time.Since(stopped); runErr != nil || took > 3*time.Second {
			t.Fatalf("the agent returned %v %v after its SIGTERM, want nil within 3s", runErr, took)
		}

		if hellos, finals, early, _ := transport.counts(); hellos != 1 || len(finals) != 1 ||
			finals[0] != http.StatusNoContent || early != 0 {
			t.Fatalf("the agent said hello %d times and sent final batches answered %v, %d before its SIGTERM; "+
				"want 1, one answered 204, 0", hellos, finals, early)
		}

		if strings.Contains(log.String(), "did not accept") || strings.Contains(log.String(), "dropped") {
			t.Errorf("the agent's log reports a batch gone wrong: %q", log.String())
		}

		report, ok := service.TakeGuestReport(t.Context(), "billet-one")
		if !ok {
			t.Fatal("the node had no report of the agent's guest to take")
		}

		if err := report.Validate(); err != nil {
			t.Fatalf("the report is one the ledger refuses: %v", err)
		}

		if !report.Hello || report.Accepted != 1 || report.Refused != 0 || !report.FinalSeen ||
			report.AgentVersion != "v0.15.0" {
			t.Fatalf("the node kept hello %t, accepted %d, refused %d, final %t, agent %q; "+
				"want true, 1, 0, true, v0.15.0", report.Hello, report.Accepted, report.Refused, report.FinalSeen,
				report.AgentVersion)
		}

		merged, err := guestreport.Decode(report.Data)
		if err != nil {
			t.Fatalf("the node's report of the agent's batch does not decode: %v", err)
		}

		if merged.Batches != 1 || merged.FirstSeq != 1 || len(merged.Samples) == 0 {
			t.Errorf("the report holds %d batches from %d and %d samples; want 1 from 1 and the agent's samples",
				merged.Batches, merged.FirstSeq, len(merged.Samples))
		}

		if !slices.Equal(merged.Steps, fixtureMarks()) {
			t.Errorf("the report's steps are %+v, want the fixture log's %+v", merged.Steps, fixtureMarks())
		}

		if want := fixtureSuites(); !slices.EqualFunc(merged.Tests, want, func(a, b guestreport.SuiteResult) bool {
			return a.Name == b.Name && a.Tests == b.Tests && a.Failures == b.Failures && a.Errors == b.Errors &&
				a.Skipped == b.Skipped && slices.Equal(a.Failed, b.Failed)
		}) {
			t.Errorf("the report's suites are %+v, want the fixtures' %+v", merged.Tests, want)
		}
	})
}
