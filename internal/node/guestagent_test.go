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
// the node's guest listener would, and counts the hellos it answered.
type guestTransport struct {
	service *node.CacheService

	mu     sync.Mutex
	hellos int
}

func (g *guestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
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

func (g *guestTransport) helloed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.hellos > 0
}

// THE IN-VM AGENT SPEAKS THIS NODE'S GUEST ROUTES, not only a stand-in for them:
// billet guest monitor's agent says hello with the bearer the node minted, and on
// SIGTERM sends its final batch, which the node keeps and hands over at the
// destroy as the guest's own report: the hello seen, one batch accepted and none
// refused, the final one among them, and the steps and the test results the agent
// read from the fixtures it was pointed at. The guest routes reach no volume, so
// the service's store and attacher are empty, and any call to one fails the test.
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

	transport := &guestTransport{service: service}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- guestagent.Run(ctx, guestagent.Config{
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

	deadline := time.Now().Add(5 * time.Second)
	for !transport.helloed() {
		if time.Now().After(deadline) {
			t.Fatal("the agent never said hello to the node")
		}

		time.Sleep(10 * time.Millisecond)
	}

	stopped := time.Now()
	cancel()

	if err := <-done; err != nil || time.Since(stopped) > 3*time.Second {
		t.Fatalf("the agent returned %v %v after SIGTERM, want nil within 3s", err, time.Since(stopped))
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

	if merged.Batches != 1 || merged.FirstSeq != 1 || len(merged.Steps) != 4 || len(merged.Tests) != 4 ||
		len(merged.Samples) == 0 {
		t.Errorf("the report holds %d batches from %d, %d steps, %d suites and %d samples; "+
			"want 1 from 1, the fixture log's 4 steps, its 4 suites and the agent's samples",
			merged.Batches, merged.FirstSeq, len(merged.Steps), len(merged.Tests), len(merged.Samples))
	}
}
