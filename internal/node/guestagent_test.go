// An external test package, so the agent it imports joins the node's tests here and
// nowhere else: imported into package node's own tests it changed what gosec's
// taint analysis concluded about the whole package (measured 2026-10-11, fourteen
// findings in files this test does not touch).
package node_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestagent"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// guestTransport carries the agent's requests to a cache service in-process, as
// the node's guest listener would. It counts the hellos it answered and notes, for
// each final batch, whether the agent's run had been cancelled (its SIGTERM) when
// the batch arrived.
type guestTransport struct {
	service *node.CacheService
	run     context.Context

	mu               sync.Mutex
	hellos           int
	finals           int
	finalBeforeSTERM int
}

func (g *guestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	if req.URL.Path == "/v1/guest/reports" && req.URL.RawQuery == "final=1" {
		g.mu.Lock()
		g.finals++
		if g.run.Err() == nil {
			g.finalBeforeSTERM++
		}
		g.mu.Unlock()
	}

	rec := httptest.NewRecorder()
	g.service.ServeHTTP(rec, req)

	if req.URL.Path == "/v1/guest/limits" {
		g.mu.Lock()
		g.hellos++
		g.mu.Unlock()
	}

	return rec.Result(), nil
}

func (g *guestTransport) counts() (hellos, finals, early int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.hellos, g.finals, g.finalBeforeSTERM
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
// nothing more until its SIGTERM, then sends one final batch, which the node keeps
// and hands over at the destroy as the guest's own report: the hello seen, one
// batch accepted and none refused, the final one among them, and the steps and
// test results the agent read from the fixtures it was pointed at, every field.
// The guest routes reach no volume, so the service's store and attacher are empty,
// and any call to one fails the test.
func TestTheInVMAgentSpeaksTheGuestRoutes(t *testing.T) {
	t.Parallel()

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
	transport := &guestTransport{service: service, run: ctx}

	// done is closed when the agent returns, and runErr is what it returned. The
	// cleanup cancels and joins it on every path, a t.Fatal included, and never
	// waits past its bound.
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
			Log:        io.Discard,
			Version:    "v0.15.0",
		})
	}()

	t.Cleanup(func() {
		cancel()

		bound := time.NewTimer(5 * time.Second)
		defer bound.Stop()

		select {
		case <-done:
		case <-bound.C:
			t.Error("the agent had not returned 5s after its run was cancelled")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for hellos, _, _ := transport.counts(); hellos == 0; hellos, _, _ = transport.counts() {
		if time.Now().After(deadline) {
			t.Fatal("the agent never said hello to the node")
		}

		time.Sleep(10 * time.Millisecond)
	}

	// After its hello the agent waits for its interval or its SIGTERM; one that
	// returns now, or sends a final batch now, is not the agent the contract asks for.
	settle := time.NewTimer(200 * time.Millisecond)
	select {
	case <-done:
		settle.Stop()
		t.Fatalf("the agent returned %v before its SIGTERM", runErr)
	case <-settle.C:
	}

	stopped := time.Now()
	cancel()

	bound := time.NewTimer(3 * time.Second)
	select {
	case <-done:
		bound.Stop()
	case <-bound.C:
		t.Fatal("the agent did not return within 3s of its SIGTERM")
	}

	if runErr != nil {
		t.Fatalf("the agent returned %v after %v, want nil", runErr, time.Since(stopped))
	}

	if hellos, finals, early := transport.counts(); hellos != 1 || finals != 1 || early != 0 {
		t.Fatalf("the agent said hello %d times and sent %d final batches, %d before its SIGTERM; want 1, 1, 0",
			hellos, finals, early)
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
}
