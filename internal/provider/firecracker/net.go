package firecracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/provider"
)

// tapName is the host network device a microVM's interface is backed by.
//
// DERIVED FROM THE LEASE RATHER THAN STORED, for the same reason the instance name
// is: teardown has to find it again after a crash with nothing but the id, and a
// side table that has to survive that is a side table that will not.
//
// TRUNCATED, BECAUSE A NETWORK DEVICE NAME CANNOT HOLD A LEASE ID. The kernel's
// limit is 15 characters and `billet-` plus 32 hex digits is 39, so this keeps a
// prefix. That trades a guarantee for a probability: two concurrent leases sharing
// their first 13 hex digits would collide, which is 52 bits, and the collision is
// LOUD rather than silent — `ip tuntap add` refuses a name that exists, so the
// second launch fails instead of quietly attaching two guests to one device.
func tapName(instance string) string {
	lease, ours := provider.LeaseOf(instance)
	if !ours {
		// Not reachable through Launch, which refuses such a name outright. Kept
		// total anyway: this function is also called on the teardown path, where
		// returning something derived from whatever it was given beats a panic in a
		// control plane that bans them.
		lease = instance
	}

	name := tapPrefix + lease
	if len(name) > maxIfName {
		name = name[:maxIfName]
	}

	return name
}

// tapPrefix marks a device as billet's on a host that may have others.
const tapPrefix = "bt"

// maxIfName is the kernel's IFNAMSIZ limit, less the terminator.
const maxIfName = 15

// addTap creates the host end of a guest's network and attaches it to a bridge.
//
// THE VMM CANNOT CREATE ONE ITSELF. It is jailed and unprivileged by the time it
// opens the device, so the tap has to exist first — measured, firecracker answers a
// missing one with `Open tap device failed: … Operation not permitted`, which reads
// like a permissions bug in the VMM and is not.
//
// `user` HANDS IT TO THE JAILED ACCOUNT. Without it the device belongs to root and
// the dropped VMM cannot open it, which produces the identical error from the
// opposite cause.
func (p *Provider) addTap(ctx context.Context, tap, bridge string) error {
	if bridge == "" {
		// Unreachable via Launch — Accepts refuses untrusted work with no bridge and
		// config validation requires the trusted one — so this is the guard that
		// keeps that true rather than a case that happens.
		return fmt.Errorf("firecracker: %s has no bridge to attach to", tap)
	}

	if _, err := p.run(ctx, ipBinary, []string{
		"tuntap", "add", "dev", tap, "mode", "tap", "user", fmt.Sprint(p.uid),
	}); err != nil {
		return fmt.Errorf("firecracker: create the network device %s: %w", tap, err)
	}

	for _, args := range [][]string{
		{"link", "set", "dev", tap, "master", bridge},
		{"link", "set", "dev", tap, "up"},
	} {
		if _, err := p.run(ctx, ipBinary, args); err != nil {
			// UNWOUND HERE rather than left to the caller. A tap attached to nothing
			// is invisible to the guest and to the bridge, and it survives every
			// sweep because nothing enumerates network devices looking for orphans.
			return fmt.Errorf("firecracker: attach %s to bridge %s: %w",
				tap, bridge, joinCleanup(err, p.deleteTap(ctx, tap)))
		}
	}

	return nil
}

// deleteTap removes a host network device, tolerating one that is not there.
//
// IDEMPOTENT BY THE MESSAGE, because `ip` reports both "I removed nothing" and real
// failures as exit status 1. Matched narrowly on the phrasing iproute2 uses, so a
// permissions failure or a busy device still fails loudly — a tap left attached to
// a bridge is a device the next launch under the same lease will collide with.
func (p *Provider) deleteTap(ctx context.Context, tap string) error {
	if _, err := p.run(ctx, ipBinary, []string{"link", "del", "dev", tap}); err != nil {
		if isNoSuchDevice(err) {
			return nil
		}

		return fmt.Errorf("firecracker: remove the network device %s: %w", tap, err)
	}

	return nil
}

// ipBinary is iproute2, which every distribution billet targets installs.
const ipBinary = "ip"

// isNoSuchDevice reports whether an `ip` failure was "there was nothing there".
func isNoSuchDevice(err error) bool {
	msg := strings.ToLower(err.Error())

	for _, phrase := range []string{
		"cannot find device",
		"no such device",
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}

	return false
}

// joinCleanup renders a cleanup failure beside the failure that caused it, without
// letting it become the headline.
func joinCleanup(cause, cleanup error) error {
	if cleanup == nil {
		return cause
	}

	return fmt.Errorf("%w (and the cleanup after it failed too: %w)", cause, cleanup)
}
