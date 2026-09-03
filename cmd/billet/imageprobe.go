package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
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

// probeLeaseID is this host's probe, and it is the SAME id every time.
//
// THE SAME SHAPE AS A REAL LEASE, because everything downstream reads it as one: the
// instance name encodes it, the jail is named after it, and the socket-path budget is
// measured against its length. A short id would verify a path no real job takes.
//
// BUT DELIBERATELY NOT RANDOM, and that is what makes cleaning up safe. A random id
// leaves residue nothing can name afterwards, so the only way to find it is to
// enumerate — and enumeration cannot be made safe here: the provider deliberately
// REPORTS jails with no owner marker, because a marker-less jail is a launch
// interrupted between creating the directory and writing the marker and may have a
// mapped disk behind it. Those are indistinguishable from a real node launch caught
// in the same instant, so a reaper that destroyed what it enumerated would
// eventually destroy somebody's actual job.
//
// A fixed id removes the question. Cleanup destroys ONE name, which is this host's
// probe and can be nothing else, and it is idempotent — so residue from a previous
// run is cleared by the same call that would have cleared it anyway.
func probeLeaseID(deployment string) (string, error) {
	// THE MACHINE ID, NOT THE HOSTNAME. Hostnames are not unique: cloned VMs and
	// default images share them routinely, and hashing the SAME hostname yields the
	// SAME id — so two such machines on one Ceph cluster would derive one probe name,
	// and the clone lives in a pool they share. One host's pre-launch cleanup would
	// then try to remove the other's live disk. (An earlier comment here claimed a
	// hash prevented that collision, which had it exactly backwards.)
	//
	// /etc/machine-id is per INSTALLATION and survives a rename, which is the
	// property wanted: the same machine derives the same probe every time, and no
	// other machine derives it at all.
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		// Falling back rather than refusing: a machine without one can still verify
		// an image, it just relies on not sharing a hostname with a cluster peer.
		host, hostErr := os.Hostname()
		if hostErr != nil {
			return "", fmt.Errorf("billet images verify: this host has neither a machine id "+
				"nor a name, and the probe needs one of them to be nameable: %w",
				errors.Join(err, hostErr))
		}

		id = []byte(host)
	}

	// Hashed so the id is the fixed length a lease is, and so nothing about the
	// machine is legible in a name that appears in `rbd ls`.
	// THE DEPLOYMENT IS IN THE HASH TOO. Two billets on one machine — which is a
	// shape this project supports and tests for — would otherwise derive one probe
	// name, and the clone lives in a pool they may share, so one deployment's
	// cleanup would remove the other's live disk.
	seed := append([]byte("billet-images-verify\x00"+deployment+"\x00"), bytes.TrimSpace(id)...)
	sum := sha256.Sum256(seed)

	return hex.EncodeToString(sum[:16]), nil
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
// call it an orphan, and would kill it mid-verification. TWO TIMERS DRIVE IT AND THE
// TIGHTER ONE IS THE CONTROL PLANE'S: a node sweeps on its own five-minute loop, and
// the control plane broadcasts a sweep after every successful reap, which ticks every
// thirty seconds by default. So on a healthy registered node a boot-to-report window
// of about a minute normally spans one of those broadcasts, and what an operator sees
// is the weekly gate reporting that a perfectly good image does not work. It is not
// certain — a failed reap skips the sweep, a broadcast only reaches nodes that are
// live, and a verification can run while the control plane is down — but it does not
// have to be certain to make the gate untrustworthy.
//
// A distinct identity puts the probe outside what that sweep will touch: a jail
// whose owner marker is another billet's is "not ours to report and emphatically not
// ours to destroy". The cost is that nothing else will reap one either, which is why
// the command clears its own leftovers before it launches.
//
// AND IT IS 32 LOWERCASE HEX, WHICH IS NOT COSMETIC: that is the only shape the
// thing which stores it accepts. The owner is written into a jail's `billet-owner`
// marker and read back under the same grammar it was written under — the provider's
// constructor checks it once, and the marker parser checks it again on every List,
// Find and Destroy. A value outside that grammar is refused at construction, which
// on its own makes verification impossible on the one backend that boots a guest
// image — and a marker holding one is worse than that, because List returns an ERROR
// rather than a shorter list, so the node's whole inventory wedges on a directory
// nothing else reaps.
//
// So the identity is CONSTRUCTED inside that grammar rather than the grammar being
// widened to admit it — the same answer deploymentForCheck gives to the same
// question. The hash is domain-separated and COVERS THE DEPLOYMENT, which is not a
// uniqueness guarantee — a truncated hash cannot give one — but it is the difference
// between two billets on one machine colliding with CERTAINTY and colliding only on
// the 128-bit event below. probeLeaseID covers it for the same reason, and because
// the two derivations are independent, one deployment reaching another's probe needs
// both to collide at once. A leftover stays attributable by DERIVATION rather than
// by reading: this host and this deployment compute the same identity every time,
// which is what the pre-launch cleanup already relies on.
//
// What is given up is that a probe identity can no longer be told from a minted one
// by looking at it. Sixteen bytes of a hash live in the same space as the sixteen
// random bytes an identity is minted from, so colliding with a real deployment is
// the event billet already accepts between two deployments.
func probeDeployment(deployment string) string {
	sum := sha256.Sum256([]byte("billet-image-verify-owner\x00" + deployment))

	return hex.EncodeToString(sum[:16])
}
