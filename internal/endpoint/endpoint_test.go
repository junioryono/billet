package endpoint

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// vectorsPath is the ONE vector table both billet's Go and its Python are
// held to: `ansible_collections/junioryono/billet/tests/fixtures/endpoint-vectors.json`,
// read from the package directory. A row added to one language's reading
// alone is a row the other never sees, which is why there is no Go literal.
const vectorsPath = "../../ansible_collections/junioryono/billet/tests/fixtures/endpoint-vectors.json"

type vectorRow struct {
	Input     string   `json:"input"`
	TLS       bool     `json:"tls"`
	Scheme    string   `json:"scheme"`
	Kind      string   `json:"kind"`
	Host      string   `json:"host"`
	Rooted    bool     `json:"rooted"`
	Port      uint16   `json:"port"`
	Canonical string   `json:"canonical"`
	Refused   string   `json:"refused"`
	Words     []string `json:"words"`
}

type vector struct {
	input     string
	tls       bool
	scheme    string
	kind      HostKind
	host      string
	rooted    bool
	port      uint16
	canonical string
}

type refusal struct {
	input  string
	tls    bool
	reason error
	words  []string
}

var refusalKinds = map[string]error{
	"syntax": ErrSyntax, "scheme": ErrScheme, "host": ErrHost, "route": ErrRoute,
	"zone": ErrZone, "port": ErrPort, "grammar": ErrGrammar,
}

// loadVectors reads the table, refusing an empty one, a duplicated input and
// TLS pair, a row of neither shape, and a refused kind the package does not
// name, so a table that drifted from either language is a failed test.
func loadVectors(t *testing.T) ([]vector, []refusal) {
	t.Helper()

	body, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("the vector table: %v", err)
	}

	var doc struct {
		Vectors []vectorRow `json:"vectors"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the vector table: %v", err)
	}

	if len(doc.Vectors) == 0 {
		t.Fatal("the vector table is empty")
	}

	seen := map[[2]string]bool{}

	var (
		vectors  []vector
		refusals []refusal
	)

	for i := range doc.Vectors {
		r := &doc.Vectors[i]
		key := [2]string{r.Input, strconv.FormatBool(r.TLS)}
		if seen[key] {
			t.Fatalf("the vector table lists %q (tls %v) twice", r.Input, r.TLS)
		}

		seen[key] = true

		switch {
		case r.Refused != "" && r.Canonical == "":
			reason, ok := refusalKinds[r.Refused]
			if !ok {
				t.Fatalf("%q is refused for %q, which is not a kind this package names", r.Input, r.Refused)
			}

			if len(r.Words) == 0 {
				t.Fatalf("%q is refused with no words to assert", r.Input)
			}

			refusals = append(refusals, refusal{r.Input, r.TLS, reason, r.Words})
		case r.Refused == "" && r.Canonical != "":
			var kind HostKind

			switch r.Kind {
			case "literal":
				kind = HostLiteral
			case "dns":
				kind = HostDNS
			default:
				t.Fatalf("%q has host kind %q", r.Input, r.Kind)
			}

			vectors = append(vectors, vector{r.Input, r.TLS, r.Scheme, kind, r.Host, r.Rooted, r.Port, r.Canonical})
		default:
			t.Fatalf("%q is a row of neither shape", r.Input)
		}
	}

	return vectors, refusals
}

// N1: THE VECTOR TABLE, ROW BY ROW. Parse yields exactly the typed fields and
// String the canonical text; the refused rows refuse for their one reason.
func TestParseYieldsTheVectorTable(t *testing.T) {
	vectors, refusals := loadVectors(t)

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

		for _, word := range r.words {
			if !strings.Contains(err.Error(), word) {
				t.Errorf("Parse(%q, tls=%v): %v does not say %q", r.input, r.tls, err, word)
			}
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
	vectors, _ := loadVectors(t)

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
	vectors, _ := loadVectors(t)

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
// resolution is nobody's business here. The prohibition is STRUCTURAL, over
// the package's source: the only selector on `net` is SplitHostPort, no
// resolver type or lookup is named, and no import beyond net, net/netip and
// net/url reaches the network; a seam production never called proved nothing.
func TestNoNameIsEverResolved(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "endpoint.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, imp := range file.Imports {
		switch path := strings.Trim(imp.Path.Value, `"`); path {
		case "errors", "fmt", "net", "net/netip", "net/url", "strconv", "strings":
		default:
			t.Errorf("endpoint.go imports %q, which the representation has no business with", path)
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "net" && sel.Sel.Name != "SplitHostPort" {
			t.Errorf("endpoint.go names net.%s at %s; the representation splits a host from its port and does nothing else with net",
				sel.Sel.Name, fset.Position(sel.Pos()))
		}

		return true
	})

	for _, s := range []string{"localhost:8443", "control.example:8443", "192.0.2.1.:8443", "xn--bcher-kva.example:8443"} {
		e, err := Parse(s, true)
		if err != nil {
			t.Fatal(err)
		}

		if e.Host.Kind != HostDNS {
			t.Errorf("%q is typed %v, want a DNS spelling", s, e.Host.Kind)
		}
	}
}
