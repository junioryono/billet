package node

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// twirpRequest builds a request shaped like the one a cache client sends, so a
// test can vary only the thing it is about.
func twirpRequest(t *testing.T, userAgent string, headers map[string]string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		actionsDownloadPath, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")

	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return req
}

// BUILDKIT IS SPLICED UPSTREAM ON ITS OWN, and that is deliberate — it is why
// the adapter has to speak for it. Admitting anything calling itself buildkit
// would hand local cache authority to any client that types the name.
func TestABuildKitUserAgentAloneIsNotAdmitted(t *testing.T) {
	if actionsLocalRequest(twirpRequest(t, "buildkit/0.27.0", nil)) {
		t.Error("a bare buildkit user agent was admitted to local handling")
	}
}

// The official toolkit client is admitted as it always was.
func TestTheToolkitClientIsStillAdmitted(t *testing.T) {
	if !actionsLocalRequest(twirpRequest(t, actionsUserAgent+"4.0.0", nil)) {
		t.Error("the official cache client was not admitted")
	}
}

// ...and billet's own adapter is admitted by the header it sets, whatever the
// client behind it calls itself.
func TestTheLoopbackAdapterIsAdmittedForAnyClient(t *testing.T) {
	req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
		actionsClientHeader: actionsClientLocal,
		actionsOriginHeader: "http://127.0.0.1:41321",
	})

	if !actionsLocalRequest(req) {
		t.Error("a request from billet's loopback adapter was not admitted")
	}
}

// A SIGNED URL MUST NAME AN ORIGIN THE CLIENT CAN REACH WITHOUT TLS. Built
// against the results host it sends the client back to a certificate it does
// not trust — the x509 failure this path exists to remove, one leg later.
func TestALoopbackClientGetsALoopbackOrigin(t *testing.T) {
	req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
		actionsClientHeader: actionsClientLocal,
		actionsOriginHeader: "http://127.0.0.1:41321",
	})

	if got := actionsLoopbackOrigin(req); got != "http://127.0.0.1:41321" {
		t.Errorf("actionsLoopbackOrigin = %q, want the adapter's own origin", got)
	}
}

// LOOPBACK IS NOT ONLY 127.0.0.1. An adapter on IPv6 is loopback, and the
// brackets have to survive into the URL billet mints — a check written against
// the literal "127.0.0.1" passes every other case here and silently refuses
// this one.
func TestIPv6LoopbackIsAnOriginBilletWillMint(t *testing.T) {
	for _, origin := range []string{
		"http://[::1]:41321",
		"http://[::ffff:127.0.0.1]:41321", // IPv4-mapped, still loopback
		"http://127.0.0.53:41321",         // all of 127/8, not just .1
	} {
		req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
			actionsClientHeader: actionsClientLocal,
			actionsOriginHeader: origin,
		})

		if got := actionsLoopbackOrigin(req); got != origin {
			t.Errorf("actionsLoopbackOrigin(%q) = %q, want it accepted unchanged", origin, got)
		}
	}
}

// Everything else keeps the results host, which is what every existing client
// already follows.
func TestAnOrdinaryClientGetsNoLoopbackOrigin(t *testing.T) {
	req := twirpRequest(t, actionsUserAgent+"4.0.0", map[string]string{
		actionsOriginHeader: "http://127.0.0.1:41321",
	})

	if got := actionsLoopbackOrigin(req); got != "" {
		t.Errorf("actionsLoopbackOrigin = %q for a client that is not the adapter", got)
	}
}

// THE GUEST'S DOCKER GATEWAY IS AN ORIGIN BILLET WILL MINT. The adapter binds
// it so a builder in a container reaches the adapter without network=host, and
// a URL naming a private address is usable only inside the guest's own network,
// exactly as a loopback one is.
func TestTheGuestsDockerGatewayIsAnOriginBilletWillMint(t *testing.T) {
	for _, origin := range []string{
		"http://172.17.0.1:41321",
		"http://10.88.0.1:41321",
		"http://192.168.49.1:41321",
		"http://[fd00::1]:41321",
	} {
		req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
			actionsClientHeader: actionsClientLocal,
			actionsOriginHeader: origin,
		})

		if got := actionsLoopbackOrigin(req); got != origin {
			t.Errorf("actionsLoopbackOrigin(%q) = %q, want it accepted unchanged", origin, got)
		}
	}
}

// THE ORIGIN COMES FROM THE CLIENT, so it is checked rather than believed.
// billet will not mint a signed URL pointing at an address of someone else's
// choosing: a redirect out of the guest is a cache entry fetched from, or
// uploaded to, wherever the header said.
func TestAnOriginBillletWillNotMintIsRefused(t *testing.T) {
	for _, origin := range []string{
		"http://evil.example",         // a name
		"http://8.8.8.8:41321",        // a public address
		"http://[2001:db8::1]:41321",  // a public address, v6
		"http://0.0.0.0:41321",        // parses as an IP and is neither loopback nor private
		"http://169.254.169.254",      // link-local, and the metadata service besides
		"https://127.0.0.1:41321",     // TLS is the thing being avoided
		"http://127.0.0.1:41321/path", // an origin has no path
		"http://127.0.0.1:41321?a=b",  // nor a query
		"http://user@127.0.0.1:41321", // nor credentials
		"http://localhost:41321",      // a name, not an address billet resolved
		"",                            // absent
		"://nonsense",                 // unparseable
	} {
		req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
			actionsClientHeader: actionsClientLocal,
			actionsOriginHeader: origin,
		})

		if got := actionsLoopbackOrigin(req); got != "" {
			t.Errorf("actionsLoopbackOrigin(%q) = %q, want it refused", origin, got)
		}
	}
}

// THE URL IS THE POINT, and until now nothing asserted it. Every test above
// passes if actionsSignedURL ignores its origin argument, or if actionsResponse
// threads "" into create and find — which is the whole mechanism.
func TestASignedURLMovesOnlyItsOrigin(t *testing.T) {
	var s CacheService

	archive := &actionsArchive{ID: "abc123", Signature: "sig-value"}

	got := s.actionsSignedURL(archive, "http://127.0.0.1:41321")
	want := "http://127.0.0.1:41321" + actionsBlobPrefix + "abc123?sig=sig-value"

	if got != want {
		t.Errorf("actionsSignedURL = %q, want %q", got, want)
	}
}

// ...and an ordinary client's URL is untouched, which is what every existing
// deployment already follows.
func TestASignedURLWithoutAnOriginKeepsTheResultsHost(t *testing.T) {
	var s CacheService

	got := s.actionsSignedURL(&actionsArchive{ID: "abc123", Signature: "sig-value"}, "")

	if !strings.HasPrefix(got, "https://"+actionsResultsHost+actionsBlobPrefix) {
		t.Errorf("actionsSignedURL = %q, want it unchanged for a non-adapter client", got)
	}
}

// A CALLER CLAIMING THE ADAPTER WITHOUT A USABLE ORIGIN MUST NOT BE ADMITTED.
// Served locally, it gets an https results-host URL it cannot verify — the x509
// failure this path exists to remove, arriving after storage was allocated.
func TestTheAdapterHeaderWithoutAUsableOriginIsNotAdmitted(t *testing.T) {
	for _, origin := range []string{"", "http://evil.example", "http://127.0.0.1:0"} {
		headers := map[string]string{actionsClientHeader: actionsClientLocal}
		if origin != "" {
			headers[actionsOriginHeader] = origin
		}

		if actionsLocalRequest(twirpRequest(t, "buildkit/0.27.0", headers)) {
			t.Errorf("admitted an adapter request whose origin was %q", origin)
		}
	}
}

// A port that parses but cannot be dialled mints a URL the client cannot use.
func TestAnUndialablePortIsRefused(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:99999",
		"http://127.0.0.1:",
	} {
		req := twirpRequest(t, "buildkit/0.27.0", map[string]string{
			actionsClientHeader: actionsClientLocal,
			actionsOriginHeader: origin,
		})

		if got := actionsLoopbackOrigin(req); got != "" {
			t.Errorf("actionsLoopbackOrigin(%q) = %q, want it refused", origin, got)
		}
	}
}
