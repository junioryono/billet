package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
)

// verificationSecret is a value this run invented for one guest to hand back.
//
// INVENTED PER RUN, because the report has to prove that THIS guest read THIS
// registration. A fixed string would be satisfied by a microVM somebody forgot to
// destroy, or by anything else on the bridge that happened to post.
func verificationSecret() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("billet images verify: invent a probe registration: %w", err)
	}

	return "billet-verify-" + hex.EncodeToString(raw), nil
}

// probeLeaseID mints an id of the shape the allocator hands out.
//
// THE SAME SHAPE, because everything downstream reads it as one: the instance name
// encodes it, the jail is named after it, and the socket-path budget is measured
// against its length. A short id would verify a path no real job takes.
func probeLeaseID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("billet images verify: mint a lease id for the probe: %w", err)
	}

	return hex.EncodeToString(raw), nil
}

// hostAddrOnBridge is where a guest can reach this machine.
//
// THE BRIDGE'S OWN ADDRESS, not loopback and not a hostname. The guest is on the
// other side of a tap attached to this bridge, so the only address of this host it
// can route to is the one the bridge holds — 127.0.0.1 is a listener it cannot see,
// and a hostname is a DNS lookup a guest with a bare DHCP lease may not resolve.
//
// This is the piece that cannot be guessed and has to be looked up, which is why it
// is here rather than a flag: an operator asked for a value like `10.217.20.1` would
// be reading it off `ip addr` and typing it back in.
func hostAddrOnBridge(bridge string, port int) (string, error) {
	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return "", fmt.Errorf("billet images verify: find the bridge %s, which is where a guest "+
			"reaches this host: %w", bridge, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("billet images verify: read the addresses of %s: %w", bridge, err)
	}

	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		// IPv4 ONLY. The guest gets its address from the bridge's DHCP, which on
		// every deployment this backend targets is v4 — and a v6 literal would also
		// need bracketing in the URL the guest builds.
		if v4 := ipnet.IP.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), strconv.Itoa(port)), nil
		}
	}

	return "", fmt.Errorf("billet images verify: the bridge %s has no IPv4 address, so a guest "+
		"on it has no way to reach this host; give the bridge an address, or verify on a "+
		"machine whose bridge has one", bridge)
}

// probeDeployment is the identity a verification's microVM is owned by.
//
// NOT THE NODE'S, AND THAT IS THE WHOLE POINT. A probe's lease is invented rather
// than allocated, so the node daemon's sweep — which lists every instance this
// deployment owns and destroys any whose lease it cannot account for — is correct to
// call it an orphan, and would kill it mid-verification. On a five-minute sweep
// against a boot-to-report window of about a minute, that lands inside the window
// often enough to matter, and what an operator sees is the weekly gate reporting
// that a perfectly good image does not work.
//
// A distinct identity puts the probe outside what that sweep will touch: a jail
// whose owner marker is another billet's is "not ours to report and emphatically not
// ours to destroy". The cost is that nothing else will reap one either, which is why
// the command clears its own leftovers before it launches.
func probeDeployment(deployment string) string { return deployment + "-imageverify" }
