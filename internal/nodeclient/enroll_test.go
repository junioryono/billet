package nodeclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

// AN ORDINARY ENROLLMENT CARRIES NO IDENTITY AT ALL, not even as empty keys, because a
// control plane older than wire 43 decodes strictly and would refuse the request; a pool
// host's carries the document and signature it was handed.
func TestOnlyAPoolHostsEnrollmentCarriesAnIdentity(t *testing.T) {
	t.Parallel()
	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"state":"pending","fingerprint":"SHA256:x"}`)); err != nil {
			t.Error(err)
		}
	}))
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)

	csr, _, err := wirecert.NewNodeCSR("pool-pa")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Enroll(t.Context(), srv.URL, "pool-pa", "tok", ca.CertPEM(), csr); err == nil {
		t.Fatal("a pending answer was not ErrNotApproved")
	}
	if _, _, err := EnrollWithIdentity(t.Context(), srv.URL, "pool-pa", "tok", ca.CertPEM(), csr,
		Identity{Document: []byte(`{"instanceId":"i-1"}`), Signature: "c2ln"}); err == nil {
		t.Fatal("a pending answer was not ErrNotApproved")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	for _, key := range []string{"identity_document", "identity_signature"} {
		if _, ok := bodies[0][key]; ok {
			t.Errorf("an ordinary enrollment carries %q", key)
		}
	}
	if bodies[1]["identity_document"] != `{"instanceId":"i-1"}` || bodies[1]["identity_signature"] != "c2ln" {
		t.Errorf("a pool host's enrollment carried %v", bodies[1])
	}
}
