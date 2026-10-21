package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// THE AUTHORITY IS CONFIRMED BY ITS HOLDER, NOT BY ITS CERTIFICATE (#181):
// FetchCA accepts anything that presents the right public certificate, so a
// responder that replays it, with a deployment of its own choosing, passes
// that check; ConfirmAuthority then asks over a handshake verified against the
// authority, which only the real bootstrap listener completes, and the
// deployment it reads is the control plane's.
//
// Mutation: a ConfirmAuthority that skips verification (InsecureSkipVerify)
// returns the replayer's deployment.
func TestAJoinConfirmsTheAuthorityItFetched(t *testing.T) {
	t.Parallel()

	ca, base, _, _ := enrollableWire(t)

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch the authority: %v", err)
	}

	deployment, err := nodeclient.ConfirmAuthority(t.Context(), base, caPEM)
	if err != nil || deployment != wireDeployment {
		t.Fatalf("the real control plane confirmed (%q, %v), want %q", deployment, err, wireDeployment)
	}

	// A REPLAYER: the public certificate, a deployment of its own, and a
	// serving certificate the authority never signed.
	replay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(nodeapi.CAResponse{
			CAPEM: string(caPEM), Fingerprint: ca.Fingerprint(), Deployment: "fedcba9876543210fedcba9876543210",
		}); err != nil {
			t.Errorf("the replayer could not answer: %v", err)
		}
	}))
	t.Cleanup(replay.Close)

	if _, deployment, err := nodeclient.FetchCA(t.Context(), replay.URL, ca.Fingerprint()); err != nil ||
		deployment != "fedcba9876543210fedcba9876543210" {
		t.Fatalf("the replay did not pass the fingerprint check as the case assumes: (%q, %v)", deployment, err)
	}

	_, err = nodeclient.ConfirmAuthority(t.Context(), replay.URL, caPEM)
	if err == nil || !strings.Contains(err.Error(), "did not prove it holds a key the authority signed") {
		t.Errorf("a replayer was confirmed: %v", err)
	}
}
