package nodeclient_test

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeclient"
)

// THE CONFIG WRITES host:port, AND A REQUEST NEEDS A URL.
//
// `billet node` could not make one call. node.server_addr is validated as
// host:port — no scheme, because that is what an operator writes and what can be
// checked — and it was handed straight to the client as a base URL. Every
// request then died at construction with "first path segment in URL cannot
// contain colon". The check that stood in its place could not fail either:
// url.Parse accepts "127.0.0.1:7717" happily, reading the host as a scheme. And
// every test dialled an httptest server, whose URL already carries one, so the
// suite was green throughout.
func TestTheBaseAddressAcceptsWhatTheConfigHolds(t *testing.T) {
	t.Parallel()

	c, err := nodeclient.New(nodeclient.Options{Base: "127.0.0.1:7717", Node: "n1"})
	if err != nil {
		t.Fatalf("a bare host:port — which is exactly what node.server_addr holds — was "+
			"refused: %v", err)
	}

	if got := c.BaseForTest(); got != "http://127.0.0.1:7717" {
		t.Errorf("base is %q, want a URL requests can actually be built from", got)
	}
}

// A NODE WITH A CERTIFICATE DIALS https, and nothing has to remember to say so.
func TestTheBaseAddressFollowsTheCertificate(t *testing.T) {
	t.Parallel()

	c, err := nodeclient.New(nodeclient.Options{
		Base: "billet.example:7717",
		Node: "n1",
		TLS:  &tls.Config{MinVersion: tls.VersionTLS13},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if got := c.BaseForTest(); !strings.HasPrefix(got, "https://") {
		t.Errorf("a node holding a certificate dials %q; the control plane requires TLS, so "+
			"this connection is refused at the handshake", got)
	}
}

// A SCHEME THAT CONTRADICTS THE CERTIFICATE IS A CONFIGURATION ERROR.
//
// Not a fallback: the handshake fails either way, and refusing here says which
// setting caused it instead of leaving an operator reading TLS errors.
func TestABaseAddressMayNotContradictTheCertificate(t *testing.T) {
	t.Parallel()

	_, err := nodeclient.New(nodeclient.Options{
		Base: "http://billet.example:7717",
		Node: "n1",
		TLS:  &tls.Config{MinVersion: tls.VersionTLS13},
	})
	if err == nil {
		t.Error("a node with a certificate was configured to dial plain http, so it would " +
			"never present it")
	}

	if _, err := nodeclient.New(nodeclient.Options{
		Base: "https://billet.example:7717",
		Node: "n1",
	}); err == nil {
		t.Error("a node with no certificate was configured to dial https, where the control " +
			"plane will reject the handshake")
	}
}
