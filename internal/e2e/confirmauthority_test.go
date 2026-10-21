package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/wirecert"
)

// serveAuthority answers /v1/ca with caPEM and deployment over TLS, presenting
// serving (or httptest's own certificate when serving is nil).
func serveAuthority(t *testing.T, caPEM []byte, deployment string, serving *wirecert.Bundle) string {
	t.Helper()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(nodeapi.CAResponse{CAPEM: string(caPEM), Deployment: deployment}); err != nil {
			t.Errorf("the stand-in could not answer: %v", err)
		}
	}))
	if serving != nil {
		conf, err := wirecert.BootstrapTLS(*serving)
		if err != nil {
			t.Fatalf("bootstrap tls: %v", err)
		}
		srv.TLS = conf
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv.URL
}

// THE AUTHORITY IS CONFIRMED BY ITS HOLDER, NOT BY ITS CERTIFICATE (#181):
// FetchCA accepts anything that presents the right public certificate first,
// so a responder that replays it, with a deployment of its own choosing, or
// that appends a root of its own and presents a certificate that root signed,
// passes that check. ConfirmAuthority trusts only the certificate the
// fingerprint names, which only the real bootstrap listener's handshake
// chains to, and the deployment it reads is the control plane's. Each attack
// is confirmed with the bytes its own fetch returned.
//
// Mutations: a ConfirmAuthority that skips verification returns the
// replayer's deployment; one that trusts the whole fetched bundle confirms the
// appended root.
func TestAJoinConfirmsTheAuthorityItFetched(t *testing.T) {
	t.Parallel()

	ca, base, _, _ := enrollableWire(t)

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch the authority: %v", err)
	}

	deployment, pinned, err := nodeclient.ConfirmAuthority(t.Context(), base, caPEM, ca.Fingerprint())
	if err != nil || deployment != wireDeployment {
		t.Fatalf("the real control plane confirmed (%q, %v), want %q", deployment, err, wireDeployment)
	}
	if got, err := wirecert.FingerprintOfCAPEM(pinned); err != nil || !wirecert.SameFingerprint(got, ca.Fingerprint()) {
		t.Errorf("the confirmed authority is %s (%v), want the pinned %s", got, err, ca.Fingerprint())
	}

	const theirs = "fedcba9876543210fedcba9876543210"

	// A REPLAYER: the public certificate and a serving certificate the
	// authority never signed.
	replay := serveAuthority(t, caPEM, theirs, nil)

	// AN APPENDER: the public certificate, then a root of its own, and a serving
	// certificate for this address that root signed.
	dir := t.TempDir()
	rogue, err := wirecert.LoadOrCreateCA(dir, theirs)
	if err != nil {
		t.Fatal(err)
	}
	rogueServing, err := rogue.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	roguePEM, err := os.ReadFile(wirecert.CACertPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	appender := serveAuthority(t, append(append([]byte{}, caPEM...), roguePEM...), theirs, &rogueServing)

	for name, url := range map[string]string{"a replayer": replay, "an appended root": appender} {
		fetched, got, err := nodeclient.FetchCA(t.Context(), url, ca.Fingerprint())
		if err != nil || got != theirs {
			t.Fatalf("%s did not pass the fingerprint check as the case assumes: (%q, %v)", name, got, err)
		}

		_, _, err = nodeclient.ConfirmAuthority(t.Context(), url, fetched, ca.Fingerprint())
		if err == nil || !strings.Contains(err.Error(), "did not prove it holds a key the authority") {
			t.Errorf("%s was confirmed: %v", name, err)
		}
	}
}
