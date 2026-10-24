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
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imageaccept"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// acceptanceServer is a plane whose deployment accepts the given components,
// behind a handler that rewrites a registration's answer when rewrite is set and
// answers the first registration with an outage when outageFirst is. registered
// counts the registrations that reached it.
type acceptanceServer struct {
	URL        string
	registered atomic.Int64
}

func newAcceptanceServer(t *testing.T, accepted []config.ImageComponent,
	rewrite func(*nodeapi.RegisterResponse), outageFirst bool,
) *acceptanceServer {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithCommandTimeout(5*time.Second),
		nodeplane.WithImageAcceptance(accepted))
	p.SetPollWindowForTest(60 * time.Millisecond)

	inner := nodeplane.Handler(log, p, stubStore{}, stubJIT{})
	s := &acceptanceServer{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/register" {
			inner.ServeHTTP(w, r)

			return
		}

		if s.registered.Add(1) == 1 && outageFirst {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		if rewrite == nil {
			inner.ServeHTTP(w, r)

			return
		}

		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)

		var res nodeapi.RegisterResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil {
			w.WriteHeader(rec.Code)
			if _, err := w.Write(rec.Body.Bytes()); err != nil {
				t.Errorf("relay the plane's refusal: %v", err)
			}

			return
		}

		rewrite(&res)

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(res); err != nil {
			t.Errorf("answer the registration: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	s.URL = srv.URL

	return s
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
	srv := newAcceptanceServer(t, []config.ImageComponent{config.ComponentAndroidSDK, config.ComponentFirefox},
		nil, false)

	stateDir := t.TempDir()
	registerOnce(t, srv.URL, stateDir)

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok {
		t.Fatalf("no acceptance was recorded at registration: ok %v, %v", ok, err)
	}

	if want := []string{"android-sdk", "firefox"}; !slices.Equal(rec.Components, want) ||
		rec.Deployment != deployment || !rec.Carried {
		t.Fatalf("the record says %+v, want %v carried for %s", rec, want, deployment)
	}
}

// A PLANE THAT ACCEPTS NOTHING SAYS SO at the version that carries acceptance,
// and the node replaces an earlier, longer record with an empty list.
func TestAPlaneThatAcceptsNothingIsRecordedAsNone(t *testing.T) {
	stateDir := t.TempDir()
	if err := imageaccept.Write(stateDir, deployment, true, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	registerOnce(t, newAcceptanceServer(t, nil, nil, false).URL, stateDir)

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok {
		t.Fatalf("a plane accepting nothing left no record: ok %v, %v", ok, err)
	}

	if len(rec.Components) != 0 || !rec.Carried {
		t.Fatalf("the record says %+v, want none, carried", rec)
	}
}

// AN ANSWER BELOW THE VERSION IS SILENT, NOT "NONE": a registration settled
// below nodeapi.VersionImageAcceptance records that its control plane does not
// say, replacing an earlier record and ignoring the field an answer carries
// anyway.
func TestAnAnswerBelowTheVersionIsRecordedAsSilent(t *testing.T) {
	srv := newAcceptanceServer(t, nil, func(res *nodeapi.RegisterResponse) {
		res.Version = nodeapi.VersionImageAcceptance - 1
		res.ImagesAccept = []string{"codeql"}
	}, false)

	stateDir := t.TempDir()
	if err := imageaccept.Write(stateDir, deployment, true, []string{"android-sdk"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	c := registerOnce(t, srv.URL, stateDir)

	if c.WireVersion() != nodeapi.VersionImageAcceptance-1 {
		t.Fatalf("the registration settled on %d, want the version below acceptance", c.WireVersion())
	}

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok || rec.Carried || len(rec.Components) != 0 || rec.Deployment != deployment {
		t.Fatalf("a silent answer was recorded as %+v (ok %v, %v), want its control plane not saying",
			rec, ok, err)
	}
}

// A RECORD THAT COULD NOT BE REPLACED IS REMOVED, so the image commands refuse
// rather than read an answer the node no longer holds. A directory where the
// record goes refuses the installer's rename and is removable when empty.
func TestARecordThatCouldNotBeReplacedIsRemoved(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Mkdir(imageaccept.Path(stateDir), 0o700); err != nil {
		t.Fatal(err)
	}

	registerOnce(t, newAcceptanceServer(t, []config.ImageComponent{config.ComponentCodeQL}, nil, false).URL,
		stateDir)

	if _, err := os.Lstat(imageaccept.Path(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("what stood in the record's place after a failed write: %v, want it removed", err)
	}
}

// A DRAINING STOP BEFORE THE FIRST REGISTRATION REGISTERS TO RECOVER, AND
// RECORDS IT: the node's first registration meets an outage, a stop asked to
// drain arrives while it waits to retry, and the registration that stop makes
// records the deployment's acceptance like any other.
func TestARegistrationMadeToStopIsRecorded(t *testing.T) {
	srv := newAcceptanceServer(t, []config.ImageComponent{config.ComponentGoogleChrome}, nil, true)
	stateDir := t.TempDir()

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		err := nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
			GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler), Backoff: time.Hour,
			DrainRequested:     func() bool { return true },
			ImageAcceptanceDir: stateDir,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(10 * time.Second)
	for srv.registered.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the node never tried to register")
		}

		time.Sleep(5 * time.Millisecond)
	}

	// THE FIRST ATTEMPT FAILED AND NOTHING WAS RECORDED; the loop waits an hour
	// to retry, so only the stop can register again.
	if _, ok, err := imageaccept.Read(stateDir); ok || err != nil {
		t.Fatalf("a refused registration recorded acceptance: ok %v, %v", ok, err)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not finish")
	}

	if srv.registered.Load() != 2 {
		t.Fatalf("the plane saw %d registrations, want the stop's own second one", srv.registered.Load())
	}

	rec, ok, err := imageaccept.Read(stateDir)
	if err != nil || !ok || !slices.Equal(rec.Components, []string{"google-chrome"}) {
		t.Fatalf("the stop's registration recorded %+v (ok %v, %v), want google-chrome", rec, ok, err)
	}
}
