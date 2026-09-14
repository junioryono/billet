// Package endpoint is the one representation of a control-plane endpoint: a
// scheme, a typed host and a numeric port, parsed from the spellings a
// configuration or a record may use, printed in one canonical text, and
// compared by value. Two things read it: the node client, whose request base
// is this representation's text, and the node's registration record, which the
// inspector compares with the installed configuration. The rules here are the
// contract both billet's Go and its Python are held to, through the vector
// table in endpoint_test.go, never through each other's defaults.
//
// A HOST IS A LITERAL ADDRESS OR A DNS SPELLING, and the two never compare
// equal: `localhost` is a name whose resolution is nobody's business here, and
// no name is ever resolved. A literal is canonicalised (RFC 5952 text; an
// IPv4-mapped address is its IPv4; a zone is refused, never stripped). A name
// is held to a pinned ASCII grammar and lower-cased, and keeps its ROOTEDNESS:
// `control.example.` is a different request base from `control.example`,
// because a resolver searches the unrooted one and takes the rooted one as
// absolute. The scheme is decided by the node's TLS state alone, never by the
// address or the port. A path, userinfo, a query or a fragment refuses: this is
// an endpoint, and a routing component is never silently discarded.
package endpoint

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// The reasons a spelling is refused, each its own error so a caller and a test
// can tell an empty host from a port out of range from a zone.
var (
	// ErrSyntax is a spelling the URL parser refuses (an unbracketed IPv6
	// address, a raw `%` in a host, a non-digit port).
	ErrSyntax = errors.New("endpoint: not a URL")
	// ErrScheme is an explicit scheme that is neither http nor https, or one
	// that contradicts the node's TLS state.
	ErrScheme = errors.New("endpoint: scheme")
	// ErrHost is a spelling with no host.
	ErrHost = errors.New("endpoint: no host")
	// ErrRoute is a routing component beside the endpoint: a path, userinfo, a
	// query or a fragment, empty ones included.
	ErrRoute = errors.New("endpoint: a routing component")
	// ErrZone is a literal address with a zone.
	ErrZone = errors.New("endpoint: a zone")
	// ErrPort is a port that is empty or outside 1 to 65535.
	ErrPort = errors.New("endpoint: port")
	// ErrGrammar is a DNS spelling outside the pinned grammar.
	ErrGrammar = errors.New("endpoint: name")
	// ErrNotCanonical is a text ParseCanonical was given that is not the
	// canonical spelling of the endpoint it names.
	ErrNotCanonical = errors.New("endpoint: not the canonical spelling")
)

// HostKind types a host.
type HostKind int

const (
	// HostLiteral is an IP address.
	HostLiteral HostKind = iota + 1
	// HostDNS is a name, never resolved here.
	HostDNS
)

// String names a kind for a message.
func (k HostKind) String() string {
	switch k {
	case HostLiteral:
		return "literal"
	case HostDNS:
		return "dns"
	}

	return fmt.Sprintf("HostKind(%d)", int(k))
}

// Host is the typed host of an endpoint: Addr for a literal (canonical,
// unmapped, no zone), Name and Rooted for a DNS spelling (lower-cased, the
// trailing dot removed and remembered).
type Host struct {
	Kind   HostKind
	Addr   netip.Addr
	Name   string
	Rooted bool
}

// Endpoint is one representation of a control-plane endpoint.
type Endpoint struct {
	Scheme string
	Host   Host
	Port   uint16
}

// Parse reads a spelling under the node's TLS state: a bare `host:port`, a
// bare host (the scheme's default port), or a URL whose scheme must agree with
// the TLS state.
func Parse(input string, tls bool) (Endpoint, error) {
	scheme := "http"
	if tls {
		scheme = "https"
	}

	raw := input
	if !strings.Contains(raw, "://") {
		raw = scheme + "://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: %q: %w", ErrSyntax, input, err)
	}

	switch u.Scheme {
	case "http", "https":
	default:
		return Endpoint{}, fmt.Errorf("%w: %q must be http or https", ErrScheme, input)
	}

	if u.Scheme != scheme {
		return Endpoint{}, fmt.Errorf("%w: %q is %s, which contradicts a node that %s; drop the scheme or write %s",
			ErrScheme, input, u.Scheme, tlsState(tls), scheme)
	}

	if u.Host == "" {
		return Endpoint{}, fmt.Errorf("%w: %q names no host", ErrHost, input)
	}

	switch {
	case u.User != nil:
		return Endpoint{}, fmt.Errorf("%w: %q carries userinfo", ErrRoute, input)
	case u.Path != "" || u.RawPath != "":
		return Endpoint{}, fmt.Errorf("%w: %q carries a path", ErrRoute, input)
	case u.RawQuery != "" || u.ForceQuery:
		return Endpoint{}, fmt.Errorf("%w: %q carries a query", ErrRoute, input)
	case u.Fragment != "" || strings.HasSuffix(raw, "#"):
		return Endpoint{}, fmt.Errorf("%w: %q carries a fragment", ErrRoute, input)
	}

	// A COLON WITH NOTHING AFTER IT IS AN EMPTY PORT, which the splitter
	// reads as no port at all; no port at all is the scheme's default,
	// because that is what the client dials there.
	if strings.HasSuffix(u.Host, ":") {
		return Endpoint{}, fmt.Errorf("%w: %q has an empty port", ErrPort, input)
	}

	hostPart, portPart, err := net.SplitHostPort(u.Host)
	if err != nil {
		hostPart = strings.TrimSuffix(strings.TrimPrefix(u.Host, "["), "]")
		portPart = ""
	}

	if hostPart == "" {
		return Endpoint{}, fmt.Errorf("%w: %q names no host", ErrHost, input)
	}

	port, err := parsePort(portPart, scheme)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: %q: %w", ErrPort, input, err)
	}

	host, err := parseHost(hostPart)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w (%q)", err, input)
	}

	return Endpoint{Scheme: scheme, Host: host, Port: port}, nil
}

// ParseCanonical reads a text that must be an endpoint's own canonical
// spelling, under the scheme it carries: what the registration record holds,
// and what a comparison with a configuration is made over.
func ParseCanonical(text string) (Endpoint, error) {
	tls := strings.HasPrefix(text, "https://")
	if !tls && !strings.HasPrefix(text, "http://") {
		return Endpoint{}, fmt.Errorf("%w: %q carries no scheme", ErrNotCanonical, text)
	}

	e, err := Parse(text, tls)
	if err != nil {
		return Endpoint{}, err
	}

	if e.String() != text {
		return Endpoint{}, fmt.Errorf("%w: %q spells %q", ErrNotCanonical, text, e.String())
	}

	return e, nil
}

// String is the canonical spelling: the scheme, the host (a literal in RFC
// 5952 text, an IPv6 one bracketed; a name lower-cased with its trailing dot
// when rooted) and the port, always written.
func (e Endpoint) String() string {
	var host string

	switch e.Host.Kind {
	case HostLiteral:
		if e.Host.Addr.Is6() {
			host = "[" + e.Host.Addr.String() + "]"
		} else {
			host = e.Host.Addr.String()
		}
	case HostDNS:
		host = e.Host.Name
		if e.Host.Rooted {
			host += "."
		}
	}

	return e.Scheme + "://" + host + ":" + strconv.Itoa(int(e.Port))
}

// Equal is equality of scheme, typed host and port.
func (e Endpoint) Equal(o Endpoint) bool {
	if e.Scheme != o.Scheme || e.Port != o.Port || e.Host.Kind != o.Host.Kind {
		return false
	}

	switch e.Host.Kind {
	case HostLiteral:
		return e.Host.Addr == o.Host.Addr
	case HostDNS:
		return e.Host.Name == o.Host.Name && e.Host.Rooted == o.Host.Rooted
	}

	return false
}

func tlsState(tls bool) string {
	if tls {
		return "has a certificate to present"
	}

	return "has no certificate to present"
}

// parsePort is decimal digits between 1 and 65535, or the scheme's default
// when there are none. The URL parser has already refused a non-digit.
func parsePort(text, scheme string) (uint16, error) {
	if text == "" {
		if scheme == "https" {
			return 443, nil
		}

		return 80, nil
	}

	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", text)
	}

	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d is outside 1 to 65535", n)
	}

	return uint16(n), nil
}

// parseHost types a host: a literal when netip accepts it (a zone refused
// before any unmapping, never stripped; a mapped address unmapped), else a DNS
// spelling under the grammar.
func parseHost(text string) (Host, error) {
	if addr, err := netip.ParseAddr(text); err == nil {
		if addr.Zone() != "" {
			return Host{}, fmt.Errorf("%w: %q carries the zone %q, which names an interface of one host and is not an endpoint",
				ErrZone, text, addr.Zone())
		}

		return Host{Kind: HostLiteral, Addr: addr.Unmap()}, nil
	}

	name := text
	rooted := false

	if strings.HasSuffix(name, ".") {
		rooted = true
		name = strings.TrimSuffix(name, ".")
	}

	for i := range len(name) {
		if name[i] >= 0x80 {
			return Host{}, fmt.Errorf("%w: %q is not ASCII; an internationalised name is admitted only as its xn-- form",
				ErrGrammar, text)
		}
	}

	name = strings.ToLower(name)

	for label := range strings.SplitSeq(name, ".") {
		if label == "" {
			return Host{}, fmt.Errorf("%w: %q has an empty label", ErrGrammar, text)
		}

		if strings.HasPrefix(label, "-") {
			return Host{}, fmt.Errorf("%w: the label %q of %q begins with a hyphen", ErrGrammar, label, text)
		}

		if strings.HasSuffix(label, "-") {
			return Host{}, fmt.Errorf("%w: the label %q of %q ends with a hyphen", ErrGrammar, label, text)
		}

		for i := range len(label) {
			c := label[i]

			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			case c == '_':
				return Host{}, fmt.Errorf("%w: %q carries an underscore, which is not a hostname", ErrGrammar, text)
			default:
				return Host{}, fmt.Errorf("%w: %q carries %q, which is not a hostname character", ErrGrammar, text, c)
			}
		}
	}

	return Host{Kind: HostDNS, Name: name, Rooted: rooted}, nil
}
