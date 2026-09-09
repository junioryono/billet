package endpoint

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// vectors is the vector table of PR 6b commit 3, the canonical contract both
// billet's Go and its Python are held to: each accepted spelling with the typed
// fields Parse must yield and the canonical text String must print.
var vectors = []struct {
	input     string
	tls       bool
	scheme    string
	kind      HostKind
	host      string
	rooted    bool
	port      uint16
	canonical string
}{
	{"control.example:8443", true, "https", HostDNS, "control.example", false, 8443, "https://control.example:8443"},
	{"control.example.:8443", true, "https", HostDNS, "control.example", true, 8443, "https://control.example.:8443"},
	{"CONTROL.Example:8443", true, "https", HostDNS, "control.example", false, 8443, "https://control.example:8443"},
	{"control.example", true, "https", HostDNS, "control.example", false, 443, "https://control.example:443"},
	{"control.example", false, "http", HostDNS, "control.example", false, 80, "http://control.example:80"},
	{"control.example:08443", true, "https", HostDNS, "control.example", false, 8443, "https://control.example:8443"},
	{"http://control.example:443", false, "http", HostDNS, "control.example", false, 443, "http://control.example:443"},
	{"https://control.example", true, "https", HostDNS, "control.example", false, 443, "https://control.example:443"},
	{"127.0.0.1:8080", false, "http", HostLiteral, "127.0.0.1", false, 8080, "http://127.0.0.1:8080"},
	{"127.0.0.1:8443", true, "https", HostLiteral, "127.0.0.1", false, 8443, "https://127.0.0.1:8443"},
	{"203.0.113.7:8443", false, "http", HostLiteral, "203.0.113.7", false, 8443, "http://203.0.113.7:8443"},
	{"localhost:8080", false, "http", HostDNS, "localhost", false, 8080, "http://localhost:8080"},
	{"[2001:DB8::1]:8443", true, "https", HostLiteral, "2001:db8::1", false, 8443, "https://[2001:db8::1]:8443"},
	{"[2001:db8:0:0:0:0:0:1]:8443", true, "https", HostLiteral, "2001:db8::1", false, 8443, "https://[2001:db8::1]:8443"},
	{"[2001:db8::1:0:0:1]:8443", true, "https", HostLiteral, "2001:db8::1:0:0:1", false, 8443, "https://[2001:db8::1:0:0:1]:8443"},
	{"[2001:db8:0:0:1::1]:8443", true, "https", HostLiteral, "2001:db8::1:0:0:1", false, 8443, "https://[2001:db8::1:0:0:1]:8443"},
	{"[::ffff:192.0.2.1]:8443", true, "https", HostLiteral, "192.0.2.1", false, 8443, "https://192.0.2.1:8443"},
	{"192.0.2.1.:8443", true, "https", HostDNS, "192.0.2.1", true, 8443, "https://192.0.2.1.:8443"},
	{"0.0.0.0:8443", true, "https", HostLiteral, "0.0.0.0", false, 8443, "https://0.0.0.0:8443"},
	{"[::]:8443", true, "https", HostLiteral, "::", false, 8443, "https://[::]:8443"},
	{"xn--bcher-kva.example:8443", true, "https", HostDNS, "xn--bcher-kva.example", false, 8443, "https://xn--bcher-kva.example:8443"},
	{"control.example:1", true, "https", HostDNS, "control.example", false, 1, "https://control.example:1"},
	{"control.example:65535", true, "https", HostDNS, "control.example", false, 65535, "https://control.example:65535"},
}

// refusals is the table's refused half: each spelling with the ONE reason it
// is refused for, so a refusal for another reason (an empty host where a range
// was meant, a syntax error where a zone was) is a failure.
var refusals = []struct {
	input  string
	tls    bool
	reason error
	words  string
}{
	{"[fe80::1%25eth0]:8443", true, ErrZone, "zone"},
	{"[::ffff:192.0.2.1%25eth0]:8443", true, ErrZone, "zone"},
	{"[fe80::1%eth0]:8443", true, ErrSyntax, "escape"},
	{"https://control.example:8443?", true, ErrRoute, "query"},
	{"https://control.example:8443#", true, ErrRoute, "fragment"},
	{"https://control.example:8443/%2F", true, ErrRoute, "path"},
	{"https://control.example:8443/v1", true, ErrRoute, "path"},
	{"https://control.example:8443/", true, ErrRoute, "path"},
	{"https://u:p@control.example:8443", true, ErrRoute, "userinfo"},
	{"https://control.example:8443?x", true, ErrRoute, "query"},
	{"https://control.example:8443#f", true, ErrRoute, "fragment"},
	{"2001:db8::1:8443", true, ErrSyntax, "port"},
	{"control.example:+8443", true, ErrSyntax, "port"},
	{"control.example:0", true, ErrPort, "1 to 65535"},
	{"control.example:65536", true, ErrPort, "1 to 65535"},
	{"control.example:", true, ErrPort, "empty"},
	{"https://:8443", true, ErrHost, "no host"},
	{"", true, ErrHost, "no host"},
	{"bücher.example:8443", true, ErrGrammar, "ASCII"},
	{"under_score.example:8443", true, ErrGrammar, "underscore"},
	{"a..b:8443", true, ErrGrammar, "empty label"},
	{".a:8443", true, ErrGrammar, "empty label"},
	{"-a.example:8443", true, ErrGrammar, "begins with a hyphen"},
	{"a-.example:8443", true, ErrGrammar, "ends with a hyphen"},
	{"http://x:1", true, ErrScheme, "contradicts"},
	{"https://x:1", false, ErrScheme, "contradicts"},
	{"ftp://x:1", false, ErrScheme, "http or https"},
}

// N1: THE VECTOR TABLE, ROW BY ROW. Parse yields exactly the typed fields and
// String the canonical text; the refused rows refuse for their one reason.
func TestParseYieldsTheVectorTable(t *testing.T) {
	for _, v := range vectors {
		e, err := Parse(v.input, v.tls)
		if err != nil {
			t.Errorf("Parse(%q, tls=%v): %v", v.input, v.tls, err)

			continue
		}

		if e.Scheme != v.scheme || e.Host.Kind != v.kind || e.Host.Rooted != v.rooted || e.Port != v.port {
			t.Errorf("Parse(%q, tls=%v) = scheme %q kind %v rooted %v port %d, want %q %v %v %d",
				v.input, v.tls, e.Scheme, e.Host.Kind, e.Host.Rooted, e.Port, v.scheme, v.kind, v.rooted, v.port)
		}

		switch v.kind {
		case HostLiteral:
			if e.Host.Addr.String() != v.host || e.Host.Name != "" {
				t.Errorf("Parse(%q) literal = %q (name %q), want %q", v.input, e.Host.Addr, e.Host.Name, v.host)
			}
		case HostDNS:
			if e.Host.Name != v.host || e.Host.Addr.IsValid() {
				t.Errorf("Parse(%q) name = %q (addr %v), want %q", v.input, e.Host.Name, e.Host.Addr, v.host)
			}
		}

		if got := e.String(); got != v.canonical {
			t.Errorf("Parse(%q, tls=%v).String() = %q, want %q", v.input, v.tls, got, v.canonical)
		}
	}

	for _, r := range refusals {
		e, err := Parse(r.input, r.tls)
		if !errors.Is(err, r.reason) {
			t.Errorf("Parse(%q, tls=%v) = %+v, %v; want %v", r.input, r.tls, e, err, r.reason)

			continue
		}

		if !strings.Contains(err.Error(), r.words) {
			t.Errorf("Parse(%q, tls=%v): %v does not say %q", r.input, r.tls, err, r.words)
		}
	}
}

// N1, the zone's order: the mapped spelling with a zone is what a
// representation that unmapped before checking the zone would admit, so the
// fixture first establishes that the URL parse hands over a mapped address
// carrying its zone, and then that the representation refuses it as a zone.
func TestAZoneIsRefusedBeforeAnyUnmapping(t *testing.T) {
	u, err := url.Parse("https://[::ffff:192.0.2.1%25eth0]:8443")
	if err != nil {
		t.Fatal(err)
	}

	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil || !addr.Is4In6() || addr.Zone() != "eth0" {
		t.Fatalf("the URL parse hands over %v (%v): want a mapped address with zone eth0", addr, err)
	}

	if addr.Unmap().Zone() != "" {
		t.Fatalf("Unmap keeps the zone (%q); the premise of the order is wrong", addr.Unmap().Zone())
	}

	if _, err := Parse("[::ffff:192.0.2.1%25eth0]:8443", true); !errors.Is(err, ErrZone) {
		t.Errorf("the mapped spelling with a zone: err = %v, want the zone refusal", err)
	}
}

// N2: EQUALITY is over the scheme, the typed host and the port, nothing less
// and nothing more.
func TestEqualIsOverSchemeTypedHostAndPort(t *testing.T) {
	// Every canonical text re-parses to an equal endpoint.
	for _, v := range vectors {
		e, err := Parse(v.input, v.tls)
		if err != nil {
			t.Fatal(err)
		}

		again, err := ParseCanonical(v.canonical)
		if err != nil {
			t.Errorf("ParseCanonical(%q): %v", v.canonical, err)

			continue
		}

		if !e.Equal(again) || !again.Equal(e) {
			t.Errorf("Parse(%q) and ParseCanonical(%q) are not equal", v.input, v.canonical)
		}
	}

	equal := [][2]string{
		{"https://[::ffff:192.0.2.1]:8443", "https://192.0.2.1:8443"},
		{"https://[2001:DB8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://[2001:db8:0:0:0:0:0:1]:8443", "https://[2001:db8::1]:8443"},
		{"https://CONTROL.example:8443", "https://control.example:8443"},
		{"https://control.example:08443", "https://control.example:8443"},
	}

	for _, pair := range equal {
		a, err := Parse(pair[0], true)
		if err != nil {
			t.Fatal(err)
		}

		b, err := Parse(pair[1], true)
		if err != nil {
			t.Fatal(err)
		}

		if !a.Equal(b) {
			t.Errorf("%q and %q are not equal", pair[0], pair[1])
		}
	}

	different := []struct {
		a, b   string
		tlsA   bool
		tlsB   bool
		reason string
	}{
		{"control.example:8443", "control.example:8444", true, true, "the port"},
		{"control.example:8443", "control.example:8443", true, false, "the scheme"},
		{"control.example.:8443", "control.example:8443", true, true, "rootedness"},
		{"localhost:8443", "127.0.0.1:8443", true, true, "a name and a literal"},
		{"control-a.example:8443", "control-b.example:8443", true, true, "two names"},
		{"192.0.2.1:8443", "192.0.2.2:8443", true, true, "two IPv4 literals"},
		{"[2001:db8::1]:8443", "[2001:db8::2]:8443", true, true, "two IPv6 literals"},
		{"192.0.2.1.:8443", "192.0.2.1:8443", true, true, "a rooted name and a literal"},
	}

	for _, d := range different {
		a, err := Parse(d.a, d.tlsA)
		if err != nil {
			t.Fatal(err)
		}

		b, err := Parse(d.b, d.tlsB)
		if err != nil {
			t.Fatal(err)
		}

		if a.Equal(b) || b.Equal(a) {
			t.Errorf("%q (tls %v) and %q (tls %v) are equal; they differ by %s", d.a, d.tlsA, d.b, d.tlsB, d.reason)
		}
	}
}

// ParseCanonical admits exactly the canonical spelling, under its own scheme,
// and nothing looser: a record that says `https://CONTROL.example:8443` or
// `https://control.example` was not written by billet.
func TestParseCanonicalAdmitsOnlyTheCanonicalText(t *testing.T) {
	for _, v := range vectors {
		e, err := ParseCanonical(v.canonical)
		if err != nil {
			t.Errorf("ParseCanonical(%q): %v", v.canonical, err)

			continue
		}

		if e.String() != v.canonical || (e.Scheme == "https") != strings.HasPrefix(v.canonical, "https://") {
			t.Errorf("ParseCanonical(%q) = %q, scheme %q", v.canonical, e, e.Scheme)
		}
	}

	for _, s := range []string{"https://CONTROL.example:8443", "https://control.example", "control.example:8443",
		"https://[2001:DB8::1]:8443", "https://[::ffff:192.0.2.1]:8443", "https://control.example:08443",
		"https://control.example:8443/", "ftp://control.example:8443", ""} {
		if e, err := ParseCanonical(s); !errors.Is(err, ErrNotCanonical) && err == nil {
			t.Errorf("ParseCanonical(%q) = %q, %v; want a refusal", s, e, err)
		}
	}
}

// A representation never resolves a name: `localhost` is a DNS spelling whose
// resolution is nobody's business here, and the resolver seam is never asked.
func TestNoNameIsEverResolved(t *testing.T) {
	saved := resolveForTest
	called := false
	resolveForTest = func(string) { called = true }

	t.Cleanup(func() { resolveForTest = saved })

	for _, s := range []string{"localhost:8443", "control.example:8443", "192.0.2.1.:8443", "xn--bcher-kva.example:8443"} {
		e, err := Parse(s, true)
		if err != nil {
			t.Fatal(err)
		}

		if e.Host.Kind != HostDNS {
			t.Errorf("%q is typed %v, want a DNS spelling", s, e.Host.Kind)
		}
	}

	if called {
		t.Error("a name was resolved")
	}
}
