package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// TLSRouterConfig says this control plane sits behind a TLS-passthrough router, which
// sends each connection to its deployment by the name in its ClientHello and never
// terminates anything.
//
// IT IS WHAT LETS A LOOPBACK LISTENER SERVE MUTUAL TLS. A loopback wire otherwise
// serves plain HTTP, because nothing is between two processes on one box; here the
// router's whole fleet is, and the certificates a deployment's nodes present still
// reach the deployment that issued them.
type TLSRouterConfig struct {
	// NodeAddr is the host:port nodes dial, through the router. It must be a DNS name:
	// a client sends no SNI for an IP address, so the router could not route it.
	NodeAddr string `yaml:"node_addr"`
	// EnrollAddr is the host:port an enrolling machine dials, a name of its own because
	// the router sends it to bootstrap_listen. Required when bootstrap_listen is set,
	// and refused otherwise.
	EnrollAddr string `yaml:"enroll_addr,omitempty"`
}

// WireServesTLS reports whether this control plane's node wire demands certificates:
// anywhere but loopback, and on loopback behind a router. It is the one answer to
// what the transport is.
func (s *ServerConfig) WireServesTLS() bool {
	return s.TLSRouter != nil || !LoopbackAddr(s.Listen)
}

// RouterHosts are the names a router's nodes reach this control plane by, which its
// certificate has to carry. Both listeners present one certificate.
func (s *ServerConfig) RouterHosts() []string {
	if s.TLSRouter == nil {
		return nil
	}

	var hosts []string

	for _, addr := range []string{s.TLSRouter.NodeAddr, s.TLSRouter.EnrollAddr} {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			hosts = append(hosts, host)
		}
	}

	return hosts
}

// validateTLSRouter checks the block against the listeners behind it.
func (c *Config) validateTLSRouter() []error {
	router := c.Server.TLSRouter
	if router == nil {
		return nil
	}

	var errs []error

	if err := checkRouterAddr("server.tls_router.node_addr", router.NodeAddr); err != nil {
		errs = append(errs, err)
	}

	bootstrap := strings.TrimSpace(c.Server.BootstrapListen)

	switch {
	case router.EnrollAddr == "" && bootstrap != "":
		errs = append(errs, errors.New("server.tls_router.enroll_addr is required with server.bootstrap_listen: "+
			"an enrolling machine reaches that listener through the router, by a name of its own"))
	case router.EnrollAddr != "" && bootstrap == "":
		errs = append(errs, errors.New("server.tls_router.enroll_addr is set and server.bootstrap_listen is not, "+
			"so the router would have nothing to send it to"))
	case router.EnrollAddr != "":
		if err := checkRouterAddr("server.tls_router.enroll_addr", router.EnrollAddr); err != nil {
			errs = append(errs, err)
		}

		// THE NAME, THE WAY A ROUTER READS IT: two spellings of one name are one route,
		// and enrollment would reach the listener that demands a certificate.
		if name := routerName(router.EnrollAddr); name != "" && name == routerName(router.NodeAddr) {
			errs = append(errs, errors.New("server.tls_router.enroll_addr and node_addr name the same host, and a "+
				"router tells the two listeners apart by name"))
		}
	}

	// THE LISTENERS ARE THE ROUTER'S, not the fleet's: on anything but loopback the
	// wire would be reachable around the router, by whoever can reach the port.
	if !LoopbackAddr(c.Server.Listen) {
		errs = append(errs, fmt.Errorf("server.tls_router is set and server.listen is %q; a deployment behind "+
			"a router listens on loopback, where only the router reaches it", c.Server.Listen))
	}

	if bootstrap != "" && !LoopbackAddr(bootstrap) {
		errs = append(errs, fmt.Errorf("server.tls_router is set and server.bootstrap_listen is %q; a deployment "+
			"behind a router listens on loopback, where only the router reaches it", bootstrap))
	}

	// TWO SPELLINGS OF ONE LIST. The names nodes dial are the router's addresses, and a
	// certificate naming others is one a node would refuse.
	if len(c.Server.NodeTLSHosts) > 0 {
		errs = append(errs, errors.New("server.node_tls_hosts is set beside server.tls_router, which already says "+
			"what nodes dial; remove node_tls_hosts"))
	}

	return errs
}

// checkRouterAddr holds an address to what a router can route: a name a client sends
// as SNI, on a port.
func checkRouterAddr(key, addr string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("%s %q must be host:port: %w", key, addr, err)
	}

	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return fmt.Errorf("%s %q needs a port the router listens on", key, addr)
	}

	// A CLIENT SENDS NO SNI FOR AN IP ADDRESS (RFC 6066, and crypto/tls keeps to it),
	// so that connection would be refused at the router with nothing here saying why.
	if net.ParseIP(host) != nil {
		return fmt.Errorf("%s %q must name a DNS name: a client sends no SNI for an IP address, so a router "+
			"could not tell whose connection it was", key, addr)
	}

	// A CERTIFICATE HAS TO CARRY IT AND A ROUTER HAS TO MATCH IT, so a name either would
	// read differently from the client is refused here rather than at a handshake. A
	// single label is a name like any other: crypto/tls sends it.
	if !validRouterName(host) {
		return fmt.Errorf("%s %q must name a DNS name: letters, digits and hyphens in dot-separated labels, "+
			"with no trailing dot", key, addr)
	}

	return nil
}

// validRouterName holds a host to the syntax of a DNS name, whose last label is not a
// number: that spelling is an address to a resolver, whatever net.ParseIP makes of it.
func validRouterName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}

	labels := strings.Split(host, ".")

	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, c := range label {
			if !isRouterNameChar(c) {
				return false
			}
		}
	}

	_, err := strconv.Atoi(labels[len(labels)-1])

	return err != nil
}

func isRouterNameChar(c rune) bool {
	return c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// routerName is an address's host as a router compares it: names are case-insensitive,
// and a port does not tell a router's two routes apart.
func routerName(addr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return ""
	}

	return strings.ToLower(host)
}
