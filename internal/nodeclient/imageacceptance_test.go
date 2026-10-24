package nodeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imageaccept"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// acceptanceServer is a plane whose deployment accepts the given components,
// behind a handler that rewrites a registration's answer when rewrite is set.
func acceptanceServer(t *testing.T, accepted []config.ImageComponent,
	rewrite func(*nodeapi.RegisterResponse),
) string {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithCommandTimeout(5*time.Second),
		nodeplane.WithImageAcceptance(accepted))
	p.SetPollWindowForTest(60 * time.Millisecond)

	inner := nodeplane.Handler(log, p, stubStore{}, stubJIT{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/register" || rewrite == nil {
			inner.ServeHTTP(w, r)

			return
		}

		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)

		var res nodeapi.RegisterResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil {
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())

			return
		}

		rewrite(&res)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// registerOnce runs the loop against base, recording into stateDir, until its
// first accepted registration has been recorded, which is before recovery starts.
func registerOnce(t *testing.T, base, stateDir string) *nodeclient.Client {
	t.Helper()

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	compute := &fakeCompute{recoverStarted: make(chan struct{}), recoverGate: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
			GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond,
			ImageAcceptanceDir: stateDir,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	// A stopping node recovers before it drains, so the held recovery is let go
	// first or the stop waits on it forever.
	t.Cleanup(func() {
		close(compute.recoverGate)
		cancel()
		<-done
	})

	select {
	case <-compute.recoverStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the node never registered")
	}

	return c
}

// THE CONTROL PLANE'S ACCEPTANCE REACHES THE NODE'S RECORD: a plane whose
// deployment accepts two components answers a registration with them, and the
// node records them under its deployment before it recovers anything.
func TestTheDeploymentsImageAcceptanceIsRecordedAtRegistration(t *testing.T) {
	base := acceptanceServer(t, []config.ImageComponent{config.ComponentAndroidSDK, config.ComponentFirefox}, nil)

	stateDir := t.TempDir()
	registerOnce(t, base, stateDir)

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok {
		t.Fatalf("no acceptance was recorded at registration: ok %v, %v", ok, err)
	}

	if want := []string{"android-sdk", "firefox"}; !slices.Equal(rec.Components, want) ||
		rec.Deployment != deployment {
		t.Fatalf("the record says %v for %s, want %v for %s", rec.Components, rec.Deployment, want, deployment)
	}
}

// A PLANE THAT ACCEPTS NOTHING SAYS SO at the version that carries acceptance,
// and the node records an empty list rather than nothing.
func TestAPlaneThatAcceptsNothingIsRecordedAsNone(t *testing.T) {
	stateDir := t.TempDir()
	registerOnce(t, acceptanceServer(t, nil, nil), stateDir)

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok {
		t.Fatalf("a plane accepting nothing left no record: ok %v, %v", ok, err)
	}

	if len(rec.Components) != 0 {
		t.Fatalf("the record says %v, want none", rec.Components)
	}
}

// AN ANSWER BELOW THE VERSION IS SILENT, NOT "NONE": a registration settled
// below nodeapi.VersionImageAcceptance leaves an earlier record as it was, even
// when the answer carries the field anyway.
func TestAnAnswerBelowTheVersionLeavesTheRecordAlone(t *testing.T) {
	base := acceptanceServer(t, nil, func(res *nodeapi.RegisterResponse) {
		res.Version = nodeapi.VersionImageAcceptance - 1
		res.ImagesAccept = []string{"codeql"}
	})

	stateDir := t.TempDir()
	if err := imageaccept.Write(stateDir, deployment, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(imageaccept.Path(stateDir))
	if err != nil {
		t.Fatal(err)
	}

	c := registerOnce(t, base, stateDir)

	if c.WireVersion() != nodeapi.VersionImageAcceptance-1 {
		t.Fatalf("the registration settled on %d, want the version below acceptance", c.WireVersion())
	}

	after, err := os.ReadFile(imageaccept.Path(stateDir))
	if err != nil || string(after) != string(before) {
		t.Fatalf("a silent answer changed the record from %q to %q (%v)", before, after, err)
	}
}
