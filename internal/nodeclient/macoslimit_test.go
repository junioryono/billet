package nodeclient_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/macoslimit"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
)

// limitServer is a plane answering registrations with the node's limit, its answer
// rewritten by rewrite when one is given.
func limitServer(t *testing.T, limit int, rewrite func(*nodeapi.RegisterResponse)) *httptest.Server {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithCommandTimeout(5*time.Second),
		nodeplane.WithMacOSLimits(func(string) int { return limit }))
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
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Errorf("read the plane's answer: %v", err)
		}

		rewrite(&res)

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(res); err != nil {
			t.Errorf("answer the registration: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// THE PLANE'S LIMIT REACHES THE NODE'S RECORD, which a verification beside it reads.
func TestTheNodesMacOSLimitIsRecordedAtRegistration(t *testing.T) {
	stateDir := t.TempDir()
	registerOnce(t, limitServer(t, 5, nil).URL, stateDir)

	limit, ok, err := macoslimit.Read(stateDir, deployment)
	if err != nil || !ok || limit != 5 {
		t.Fatalf("recorded %d (ok %v, %v), want 5", limit, ok, err)
	}
}

// AN ANSWER BELOW THE VERSION IS SILENT, NOT ZERO, whatever it carries, and it replaces an
// earlier record, whose number this node no longer holds.
func TestALimitBelowTheVersionIsRecordedAsSilent(t *testing.T) {
	seven := 7
	srv := limitServer(t, 5, func(res *nodeapi.RegisterResponse) {
		res.Version = nodeapi.VersionTartVerified - 1
		res.MacOSVMLimit = &seven
	})

	stateDir := t.TempDir()
	if err := macoslimit.Write(stateDir, deployment, true, 2, time.Now()); err != nil {
		t.Fatal(err)
	}

	registerOnce(t, srv.URL, stateDir)

	if limit, ok, err := macoslimit.Read(stateDir, deployment); err != nil || ok {
		t.Fatalf("Read = %d, %v, %v, want its control plane not saying", limit, ok, err)
	}
}

// A RECORD THAT COULD NOT BE REPLACED IS REMOVED, so a verification refuses rather than
// count against a number the node no longer holds.
func TestALimitThatCouldNotBeReplacedIsRemoved(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Mkdir(macoslimit.Path(stateDir), 0o700); err != nil {
		t.Fatal(err)
	}

	registerOnce(t, limitServer(t, 5, nil).URL, stateDir)

	if _, err := os.Lstat(macoslimit.Path(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("what stood in the record's place after a failed write: %v, want it removed", err)
	}
}
