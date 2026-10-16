package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxIfName is the kernel's IFNAMSIZ limit, less the terminator. Every device name
// billet allocates has to fit inside it.
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
func (p *Provider) addTap(ctx context.Context, tap, bridge string, uid int) error {
	if bridge == "" {
		// Unreachable via Launch — Accepts refuses untrusted work with no bridge and
		// config validation requires the trusted one — so this is the guard that
		// keeps that true rather than a case that happens.
		return fmt.Errorf("firecracker: %s has no bridge to attach to", tap)
	}

	if _, err := p.run(ctx, ipBinary, []string{
		"tuntap", "add", "dev", tap, "mode", "tap", "user", strconv.Itoa(uid),
	}); err != nil {
		// A NAME THAT IS TAKEN IS SOMEBODY ELSE'S DEVICE, AND ITS OWN ERROR.
		//
		// billet ALLOCATES this name rather than deriving it, so two of its own
		// microVMs cannot contend for one — reaching here means something outside
		// billet holds the name, or a claim outlived the device it named. Either
		// way the launch that loses must not unwind it: deleting the device would
		// cut the network out from under whatever is using it.
		if isDeviceExists(err) {
			return fmt.Errorf("%w: %s is already a device on this host, so it is not billet's "+
				"to use", errTapTaken, tap)
		}

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
	if tap == "" {
		return nil
	}

	if _, err := p.run(ctx, ipBinary, []string{"link", "del", "dev", tap}); err != nil {
		if isNoSuchDevice(err) {
			return nil
		}

		return fmt.Errorf("firecracker: remove the network device %s: %w", tap, err)
	}

	return nil
}

// errTapTaken marks a collision on the truncated tap name, so the unwind leaves the
// device alone: it belongs to a microVM this launch did not start.
var errTapTaken = errors.New("the network device name is already taken")

// sysNetDir is where the kernel lists network devices.
const sysNetDir = "/sys/class/net"

// sysTapOwner reads the uid the kernel records as a tap's owner, and whether the
// device exists at all.
//
// A DEVICE THAT IS NOT A TAP HAS NO OWNER FILE, and answers -1, which no jail uid is:
// a bridge or a veth someone named into billet's range is somebody else's. A tap made
// without an owner reads -1 as well. A device that went between the two reads is
// absent, not ownerless.
func sysTapOwner(tap string) (int, bool, error) {
	return sysTapOwnerIn(sysNetDir, tap)
}

// sysTapOwnerIn is sysTapOwner against a device directory of the caller's choosing.
func sysTapOwnerIn(root, tap string) (int, bool, error) {
	if _, err := tapIndex(tap); err != nil {
		return 0, false, fmt.Errorf("%s: %w", bounded(tap), err)
	}

	dev := filepath.Join(root, tap)

	if _, err := os.Lstat(dev); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}

		return 0, false, err
	}

	raw, err := os.ReadFile(filepath.Join(dev, "owner"))
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(dev); errors.Is(statErr, os.ErrNotExist) {
			return 0, false, nil
		}

		return -1, true, nil
	}

	if err != nil {
		return 0, false, err
	}

	owner, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false, fmt.Errorf("the owner of %s reads %q: %w", tap, bounded(string(raw)), err)
	}

	return owner, true, nil
}

// ipBinary is iproute2, which every distribution billet targets installs.
const ipBinary = "ip"

// isDeviceExists reports whether `ip` refused because the name was already taken.
func isDeviceExists(err error) bool {
	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "file exists") || strings.Contains(msg, "already exists")
}

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
