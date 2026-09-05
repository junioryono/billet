package nodeclient_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// recordingPlane negotiates a chosen version and keeps the body of the
// trusted-runner-group request it is sent.
type recordingPlane struct {
	version int

	mu   sync.Mutex
	body string
}

func (p *recordingPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/register":
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version: p.version, LeaseTTLSeconds: 60, PollSeconds: 30,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case strings.HasSuffix(r.URL.Path, "/trusted-runner-group"):
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		p.mu.Lock()
		p.body = string(b)
		p.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// THE TIER REACHES THE WIRE ONLY FROM THE VERSION THAT ADDED IT, driven through
// the client method rather than the request builder, so reverting the call site
// to always send the field is what fails this.
func TestTheClientSendsTheTierOnlyOnAWireThatKnowsIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		want    bool
	}{
		{nodeapi.VersionTargetedRunnerGroup - 1, false},
		{nodeapi.VersionTargetedRunnerGroup, true},
	} {
		t.Run("wire "+strconv.Itoa(tc.version), func(t *testing.T) {
			t.Parallel()

			plane := &recordingPlane{version: tc.version}
			srv := httptest.NewServer(plane)
			t.Cleanup(srv.Close)

			c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}

			if err := c.Register(t.Context(), testRegistration()); err != nil {
				t.Fatalf("register: %v", err)
			}

			if got := c.WireVersion(); got != tc.version {
				t.Fatalf("negotiated %d, want %d", got, tc.version)
			}

			if err := c.ValidateTrustedRunnerGroup(t.Context(), "billet-4vcpu", "billet-trusted",
				[]string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"}); err != nil {
				t.Fatalf("ValidateTrustedRunnerGroup: %v", err)
			}

			plane.mu.Lock()
			body := plane.body
			plane.mu.Unlock()

			if body == "" {
				t.Fatal("no trusted-runner-group request reached the plane")
			}

			// DECODED, NOT SEARCHED: the field's presence is the version rule and
			// its value is the tier that was asked for, and a body carrying some
			// other tier under that name must fail here.
			var sent map[string]any
			if err := json.Unmarshal([]byte(body), &sent); err != nil {
				t.Fatalf("wire %d: the body is not JSON: %v: %s", tc.version, err, body)
			}

			tier, present := sent["tier"]
			if present != tc.want {
				t.Errorf("wire %d: body carries tier=%v, want %v: %s", tc.version, present, tc.want, body)
			}

			if tc.want && tier != "billet-4vcpu" {
				t.Errorf("wire %d: the tier sent is %v, want billet-4vcpu", tc.version, tier)
			}

			if sent["group"] != "billet-trusted" {
				t.Errorf("wire %d: the group was lost: %s", tc.version, body)
			}
		})
	}
}
