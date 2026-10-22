package node

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

// readNpmRequest parses raw HTTP/1.1 request bytes as the proxy reads them off
// the guest's connection.
func readNpmRequest(t *testing.T, raw string) *http.Request {
	t.Helper()

	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}

	return req
}

// THE STORE ANSWERS A CLOSED SET, and everything outside it is the registry's
// (ADR-020): each row below is a request npm, pnpm or a job could send, and
// what the store may make of it. A row that is not cacheable is spliced.
func TestOnlyAnAnonymousReadOfANamedObjectIsCacheable(t *testing.T) {
	t.Parallel()

	const host = "Host: registry.npmjs.org\r\n"
	held := func(key string) bool { return key == "full /left-pad" || key == " /left-pad/-/left-pad-1.3.0.tgz" }
	for _, tc := range []struct {
		name    string
		raw     string
		want    bool
		object  npmObject
		variant npmVariant
		path    string
	}{
		{name: "a package document", raw: "GET /left-pad HTTP/1.1\r\n" + host + "\r\n",
			want: true, object: npmPackageDocument, variant: npmFull, path: "/left-pad"},
		{name: "npm's abbreviated document", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept: " + npmClientAbbreviatedAccept + "\r\n\r\n",
			want: true, object: npmPackageDocument, variant: npmAbbreviated, path: "/left-pad"},
		{name: "a scoped document, lower-case escape", raw: "GET /@pnpm%2fexe HTTP/1.1\r\n" + host + "\r\n",
			want: true, object: npmPackageDocument, variant: npmFull, path: "/@pnpm%2fexe"},
		{name: "a scoped document, upper-case escape, keyed as one", raw: "GET /@pnpm%2Fexe HTTP/1.1\r\n" +
			host + "\r\n", want: true, object: npmPackageDocument, variant: npmFull, path: "/@pnpm%2fexe"},
		{name: "pnpm/setup's version document", raw: "GET /@pnpm/exe.linux-x64/12.4.2 HTTP/1.1\r\n" +
			host + "\r\n", want: true, object: npmVersionDocument, variant: npmFull,
			path: "/@pnpm/exe.linux-x64/12.4.2"},
		{name: "a tarball", raw: "GET /left-pad/-/left-pad-1.3.0.tgz HTTP/1.1\r\n" + host + "\r\n",
			want: true, object: npmTarball, path: "/left-pad/-/left-pad-1.3.0.tgz"},
		{name: "a scoped tarball", raw: "GET /@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz HTTP/1.1\r\n" +
			host + "\r\n", want: true, object: npmTarball, path: "/@pnpm/exe.linux-x64/-/exe.linux-x64-12.4.2.tgz"},
		{name: "a HEAD of a held document", raw: "HEAD /left-pad HTTP/1.1\r\n" + host + "\r\n",
			want: true, object: npmPackageDocument, variant: npmFull, path: "/left-pad"},
		{name: "a client that takes gzip and identity", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: gzip, deflate\r\n\r\n", want: true, object: npmPackageDocument,
			variant: npmFull, path: "/left-pad"},

		{name: "a HEAD of what the store lacks", raw: "HEAD /other HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a bearer token", raw: "GET /left-pad HTTP/1.1\r\n" + host + "Authorization: Bearer npm_x\r\n\r\n"},
		{name: "a cookie", raw: "GET /left-pad HTTP/1.1\r\n" + host + "Cookie: a=b\r\n\r\n"},
		{name: "a one-time password", raw: "GET /left-pad HTTP/1.1\r\n" + host + "npm-otp: 123456\r\n\r\n"},
		{name: "a fresh write read", raw: "GET /left-pad?write=true HTTP/1.1\r\n" + host + "\r\n"},
		{name: "an empty query", raw: "GET /left-pad? HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a publish", raw: "PUT /left-pad HTTP/1.1\r\n" + host + "Content-Length: 2\r\n\r\n{}"},
		{name: "a range", raw: "GET /left-pad/-/left-pad-1.3.0.tgz HTTP/1.1\r\n" + host + "Range: bytes=0-1\r\n\r\n"},
		{name: "a search", raw: "GET /-/v1/search?text=x HTTP/1.1\r\n" + host + "\r\n"},
		{name: "an audit", raw: "POST /-/npm/v1/security/advisories/bulk HTTP/1.1\r\n" + host +
			"Content-Length: 2\r\n\r\n{}"},
		{name: "a dist-tag", raw: "GET /left-pad/latest HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a dot segment", raw: "GET /left-pad/../other HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a repeated slash", raw: "GET //left-pad HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a trailing slash", raw: "GET /left-pad/ HTTP/1.1\r\n" + host + "\r\n"},
		{name: "another escape", raw: "GET /left%2dpad HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a double escape", raw: "GET /@pnpm%252fexe HTTP/1.1\r\n" + host + "\r\n"},
		{name: "an unescaped scoped document is not a document", raw: "GET /@pnpm/exe HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a tarball of another package", raw: "GET /left-pad/-/other-1.0.0.tgz HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a name starting with a dot", raw: "GET /.hidden HTTP/1.1\r\n" + host + "\r\n"},
		{name: "an Accept outside the set", raw: "GET /left-pad HTTP/1.1\r\n" + host + "Accept: text/html\r\n\r\n"},
		{name: "two Accept lines", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept: application/json\r\nAccept: */*\r\n\r\n"},
		{name: "a client refusing identity", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: gzip, identity;q=0\r\n\r\n"},
		{name: "a client refusing everything unnamed", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: gzip, *;q=0\r\n\r\n"},
		{name: "an absolute target", raw: "GET https://registry.npmjs.org/left-pad HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a prerelease with a leading zero", raw: "GET /left-pad/1.0.0-01 HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a scoped name past 214 characters", raw: "GET /@" + strings.Repeat("s", 110) + "/" +
			strings.Repeat("n", 110) + "/1.0.0 HTTP/1.1\r\n" + host + "\r\n"},
		{name: "a refusal of identity in capitals", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: gzip, identity;Q=0\r\n\r\n"},
		{name: "an unreadable quality", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: identity;q=none\r\n\r\n"},
		{name: "a prerelease version", raw: "GET /left-pad/1.0.0-rc.1 HTTP/1.1\r\n" + host + "\r\n",
			want: true, object: npmVersionDocument, variant: npmFull, path: "/left-pad/1.0.0-rc.1"},
		{name: "a client that weighs identity low but not zero", raw: "GET /left-pad HTTP/1.1\r\n" + host +
			"Accept-Encoding: gzip, identity;q=0.001\r\n\r\n", want: true, object: npmPackageDocument,
			variant: npmFull, path: "/left-pad"},
	} {
		req := readNpmRequest(t, tc.raw)
		got, ok := classifyNpmRequest(req, held)
		if ok != tc.want {
			t.Errorf("%s: cacheable = %v, want %v", tc.name, ok, tc.want)

			continue
		}
		if !ok {
			continue
		}
		if got.object != tc.object || got.variant != tc.variant || got.path != tc.path {
			t.Errorf("%s: classified as %+v, want object %q variant %q path %q",
				tc.name, got, tc.object, tc.variant, tc.path)
		}
	}
}

// A CLIENT'S OWN VALIDATOR IS KEPT for a local 304, and the two variants of one
// document are two entries.
func TestTheVariantIsPartOfTheKeyAndTheClientsValidatorIsKept(t *testing.T) {
	t.Parallel()

	full, ok := classifyNpmRequest(readNpmRequest(t, "GET /left-pad HTTP/1.1\r\nHost: registry.npmjs.org\r\n"+
		"If-None-Match: \"abc\"\r\n\r\n"), nil)
	if !ok || full.ifNoneMatch != `"abc"` {
		t.Fatalf("a conditional read = %+v, %v", full, ok)
	}
	abbreviated, ok := classifyNpmRequest(readNpmRequest(t, "GET /left-pad HTTP/1.1\r\nHost: registry.npmjs.org\r\n"+
		"Accept: application/vnd.npm.install-v1+json\r\n\r\n"), nil)
	if !ok || abbreviated.key() == full.key() {
		t.Fatalf("the full and abbreviated documents share key %q", full.key())
	}
}

func TestIfNoneMatchComparesWeakly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		list, etag string
		want       bool
	}{
		{`"a"`, `"a"`, true}, {`W/"a"`, `"a"`, true}, {`"b", "a"`, `W/"a"`, true}, {`*`, `"a"`, true},
		{`"b"`, `"a"`, false}, {`"a"`, ``, false},
	} {
		if got := npmETagMatches(tc.list, tc.etag); got != tc.want {
			t.Errorf("npmETagMatches(%q, %q) = %v, want %v", tc.list, tc.etag, got, tc.want)
		}
	}
}
