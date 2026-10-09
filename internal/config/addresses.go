package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// LoopbackAddr reports whether an address accepts only from this machine.
//
// SHARED WITH THE SERVER'S OWN DECISION: the wire is served without TLS on exactly
// the addresses this returns true for, so config validation and the listener must
// not answer differently. A wildcard is NOT loopback.
func LoopbackAddr(addr string) bool {
	return isLoopbackHostPort(addr)
}

// isLoopbackHostPort reports whether an address is on this machine.
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// literalLoopback reports whether a listen address names a loopback IP
// literally, so that what was validated is what is bound.
func literalLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip.Unmap().IsLoopback()
}

// addressesOverlap reports whether two listen addresses would contend for one
// socket.
//
// A WILDCARD OVERLAPS EVERYTHING ON ITS PORT, which is the case a string
// comparison misses: ":7717", "0.0.0.0:7717" and "[::]:7717" all accept on the
// address any concrete host on this machine names. Both wildcard forms count,
// because a dual-stack "::" listener takes the v4 address too.
//
// It answers NO for anything it cannot parse. Whether an address is well formed
// is validateHostPort's question, and reporting one malformed address as two
// problems helps nobody.
func addressesOverlap(a, b string) bool {
	aHost, aPort, aOK := socketOf(a)
	bHost, bPort, bOK := socketOf(b)

	if !aOK || !bOK || aPort != bPort {
		return false
	}

	if isWildcardHost(aHost) || isWildcardHost(bHost) {
		return true
	}

	return aHost == bHost
}

// socketOf is a listen address as the socket sees it: the port as a number, so
// 09180 is 9180, and the host as a canonical address, so an IPv4-mapped IPv6
// address is its IPv4 one and an IPv6 address has one spelling. A name stays a
// name, lower-cased: whether it resolves to another listener's address is not
// something a config file can be checked for.
func socketOf(addr string) (string, int, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, false
	}

	n, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, false
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()

		// A ZONE NAMES A LINK, which a loopback or an unspecified address has no
		// use for: [::1%0] binds the socket [::1] does. Any other zone is kept
		// AS WRITTEN and never resolved: which interface a zone names, and
		// whether the kernel uses it at all, is the bind's to decide, and a
		// resolution here that merged two sockets would refuse a configuration
		// that works. So this check refuses only what is provably one socket;
		// one it cannot prove, the bind refuses at startup, naming the address.
		if ip.WithZone("").IsLoopback() || ip.WithZone("").IsUnspecified() {
			ip = ip.WithZone("")
		}

		return ip.String(), n, true
	}

	return strings.ToLower(host), n, true
}

// isWildcardHost reports whether a canonical listen host accepts on every
// interface: an empty host, or an unspecified address however it was written.
//
// AN IPv6 WILDCARD COVERS IPv4 TOO, the one place this check goes past what it
// can prove: Go listens on [::] dual-stack by default, so it takes the IPv4
// addresses on its port, and a configuration that also named one would fail at
// startup on the hosts billet runs on. A host that refuses dual-stack would bind
// both, and is refused anyway.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip.WithZone("").IsUnspecified()
}

// hostLabelRe is ONE DNS label: alphanumeric at both ends, hyphens allowed between,
// 63 characters at most.
//
// PER LABEL RATHER THAN OVER THE WHOLE NAME. An earlier version of this check was
// registryMirrorOriginRe's whole-host shape, which pins only the first and last
// character — so `a..b` (an empty label), `a.-b`, `a.---.b` and a 72-character label
// all passed it. None of those is a name DNS can answer.
//
// ASCII BY CONSTRUCTION, which is load-bearing rather than incidental: isHostname is
// asked BEFORE the host is folded to lower case, so admitting no non-ASCII here is
// what makes the fold below safe. Both cases are allowed for that reason.
var hostLabelRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// maxHostname is a DNS name's length in its textual form, the bound DNS itself
// enforces. Checked because the label rule alone does not imply it: sixty valid
// labels are sixty valid labels and still not a name anything can look up.
const maxHostname = 253

// isHostname reports whether host is a name DNS could answer: every dot-separated
// label is one, and the whole thing is within the length DNS allows.
//
// ASKED OF THE WHOLE HOST BEFORE IT IS CLASSIFIED, not of the VPC endpoint's own
// labels afterwards. Checking only that prefix left two ways to name something
// unaddressable: node.ec2.region is deliberately a SHAPE with no length cap, so a
// 64-character region makes an over-long label in `sqs.<region>.<suffix>` — a
// standard host, never near the private branch — and the 253-character bound sat in
// that branch too. One question asked once about the name as a whole closes both.
func isHostname(host string) bool {
	if host == "" || len(host) > maxHostname {
		return false
	}

	for label := range strings.SplitSeq(host, ".") {
		if !hostLabelRe.MatchString(label) {
			return false
		}
	}

	return true
}

// isLoopbackHost reports whether a host names this machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// CheckHostPort validates a host:port the way config validation will, naming
// the field. Exported for callers that must refuse a bad value by the flag
// that carried it BEFORE it is rendered into a config — the same
// validate-in-both-consumers rule as CheckRunnerGroup.
func CheckHostPort(field, addr string) error { return validateHostPort(field, addr) }

func validateHostPort(field, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s is required", field)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q must be host:port: %w", field, addr, err)
	}
	if host == "" && !strings.HasPrefix(addr, ":") {
		return fmt.Errorf("%s %q has an empty host", field, addr)
	}
	// Validated directly rather than through net.LookupPort, which would accept a
	// service NAME ("http") and resolve it against /etc/services — a listen
	// address that means different things on different machines.
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s %q has an invalid port %q; expected 1-65535", field, addr, port)
	}
	return nil
}
