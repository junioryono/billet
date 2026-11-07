package tart

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func registryServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	return srv, strings.TrimPrefix(srv.URL, "https://")
}

func answer(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("answer: %v", err)
	}
}

// THE BEARER DANCE, ANONYMOUSLY, and the digest is of the bytes received.
func TestARegistryDigestIsOfTheBytesItServed(t *testing.T) {
	t.Parallel()

	var authorized bool

	srv, host := registryServer(t, nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/acme/macos/manifests/26":
			if r.Header.Get("Accept") != ociManifest {
				t.Errorf("Accept = %q", r.Header.Get("Accept"))
			}

			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:acme/macos:pull"`)
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			authorized = true

			answer(t, w, []byte("the manifest"))
		case "/token":
			if r.URL.Query().Get("service") != "reg" || r.URL.Query().Get("scope") != "repository:acme/macos:pull" {
				t.Errorf("token query = %q", r.URL.RawQuery)
			}

			if r.Header.Get("Authorization") != "" {
				t.Errorf("sent a credential to the realm: %q", r.Header.Get("Authorization"))
			}

			answer(t, w, []byte(`{"token":"tok"}`))
		default:
			t.Errorf("asked %s", r.URL.Path)
		}
	})

	digest, err := registryDigest(t.Context(), srv.Client(), host+"/acme/macos:26")
	if err != nil {
		t.Fatalf("registryDigest: %v", err)
	}

	if !authorized || digest != digestOf("the manifest") {
		t.Fatalf("digest = %q, authorized = %v", digest, authorized)
	}
}

func TestARegistryHeaderNamingOtherBytesRefuses(t *testing.T) {
	t.Parallel()

	srv, host := registryServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:0000")
		answer(t, w, []byte("the manifest"))
	})

	_, err := registryDigest(t.Context(), srv.Client(), host+"/a/b:1")
	if err == nil || !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("registryDigest = %v", err)
	}
}

// A REFUSAL AND AN ABSENT TAG ARE NOT ANSWERS ABOUT WHAT THE TAG NAMES.
func TestARegistryThatWillNotAnswerIsAnError(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"credentials required": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
			want: errRegistryNeedsCredentials,
		},
		"a token realm not over TLS": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="http://example.invalid/token"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
			want: errRegistryNeedsCredentials,
		},
		"forbidden": {
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			want:    errRegistryNeedsCredentials,
		},
		"missing": {
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		},
		"too long": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				answer(t, w, make([]byte, manifestLimit+1))
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, host := registryServer(t, tc.handler)

			_, err := registryDigest(t.Context(), srv.Client(), host+"/a/b:1")
			if err == nil {
				t.Fatal("answered")
			}

			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("registryDigest = %v, want %v", err, tc.want)
			}
		})
	}
}

// A REDIRECT THAT LEAVES TLS IS REFUSED: Go keeps a bearer across one to the same host.
func TestARegistryRedirectOffTLSIsRefused(t *testing.T) {
	t.Parallel()

	srv, host := registryServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/manifest", http.StatusFound)
	})

	client := registryClient()
	client.Transport = srv.Client().Transport

	_, err := registryDigest(t.Context(), client, host+"/a/b:1")
	if err == nil || !strings.Contains(err.Error(), "not TLS") {
		t.Fatalf("registryDigest = %v", err)
	}
}

func TestARemoteReferenceSplitsAtItsLastColon(t *testing.T) {
	t.Parallel()

	ref, err := parseRemoteRef("registry.example:5000/a/b:7")
	if err != nil || ref != (remoteRef{"registry.example:5000", "a/b", "7"}) {
		t.Fatalf("parseRemoteRef = %+v, %v", ref, err)
	}

	for _, bad := range []string{"local", "a/b", "ghcr.io/a@sha256:0"} {
		if _, err := parseRemoteRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}

	got, err := pullReference("ghcr.io/a/b:7", "sha256:0")
	if err != nil || got != "ghcr.io/a/b@sha256:0" {
		t.Fatalf("pullReference = %q, %v", got, err)
	}
}
