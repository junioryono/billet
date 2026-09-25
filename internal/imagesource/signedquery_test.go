package imagesource

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
)

// A FETCH THAT FAILS AFTER A REDIRECT DOES NOT REPORT THE SIGNED URL.
//
// A release asset redirects to a pre-signed URL, and http.Client names the URL
// it was fetching when it failed. The redirect here goes to a port nothing
// listens on, so the failure is on the signed hop, as the TLS timeout of
// 2026-09-25 was (#253).
func TestAFailedFetchDoesNotReportTheSignedRedirect(t *testing.T) {
	t.Parallel()

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	dead := closed.Addr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	signed := "http://" + dead + "/release-asset/manifest.json?sig=SECRETSIG&jwt=SECRETJWT#frag"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, signed, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL}}

	_, err = c.get(t.Context(), srv.URL+"/"+ManifestName, MaxManifestBytes)
	if err == nil {
		t.Fatal("a fetch redirected to a closed port succeeded")
	}

	msg := err.Error()
	for _, secret := range []string{"SECRETSIG", "SECRETJWT", "sig=", "jwt=", "#frag"} {
		if strings.Contains(msg, secret) {
			t.Errorf("the error carries %q from the signed URL: %s", secret, msg)
		}
	}

	if !strings.Contains(msg, dead+"/release-asset/manifest.json") {
		t.Errorf("the error lost the host and path of the hop that failed: %s", msg)
	}
}

// A URL WITH NOTHING SIGNED IN IT IS REPORTED UNCHANGED.
func TestWithoutSignedQueryLeavesAPlainURLAlone(t *testing.T) {
	t.Parallel()

	plain := &neturl.Error{Op: "Get", URL: "https://example.test/plain", Err: errors.New("refused")}

	if got := WithoutSignedQuery(plain); got.Error() != plain.Error() {
		t.Errorf("WithoutSignedQuery changed an error with nothing to remove: %v", got)
	}
}
