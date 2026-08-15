#!/usr/bin/env bash
#
# Check a built guest image before anybody publishes it.
#
# WHY THIS IS A GATE AND NOT A REPORT. An image that reaches a release is one every
# deployment pulls, and a bad one is not a local problem: it fans out to everybody
# on the next refresh, and the thing that would rebuild it is itself a guest
# booting the image. The cheapest moment to catch it is before the upload, so this
# runs between packing and publishing and its exit status decides whether the
# release happens.
#
# WHAT IT CAN AND CANNOT SEE. This inspects the filesystem: that the runner is
# installed and is the version the manifest claims, that Docker and the agent are
# there, that the units which have to start are enabled. It does NOT boot anything,
# so it cannot see an integration failure -- a kernel missing an option the
# userspace needs, systemd failing to bring the network up, Docker refusing to
# start. Booting is what `billet images verify` does, and the two are complementary
# rather than alternatives.
#
# It is worth having anyway because the failures it DOES catch are the silent ones:
# a build step that failed in a way `set -e` did not notice, a tarball that
# unpacked to the wrong place, an agent that was never installed. Those produce an
# image that boots perfectly and then does nothing, which is the hardest kind to
# diagnose from the outside.
set -euo pipefail

IMAGE="${1:-}"
MANIFEST="${2:-}"

if [ -z "$IMAGE" ]; then
	echo "usage: check-guest-image.sh <rootfs.img> [manifest.json]" >&2
	exit 2
fi

if [ ! -r "$IMAGE" ]; then
	echo "cannot read $IMAGE" >&2
	exit 2
fi

if [ "$(id -u)" -ne 0 ]; then
	echo "this mounts a filesystem image; run it as root" >&2
	exit 2
fi

MNT=$(mktemp -d)
FAILED=0

# UNMOUNTED ON EVERY PATH. A loop mount left behind holds the image file open, and
# the next step in a build is usually the one that wants to upload or delete it.
cleanup() {
	if mountpoint -q "$MNT" 2>/dev/null; then
		umount "$MNT" || true
	fi

	rmdir "$MNT" 2>/dev/null || true
}

trap cleanup EXIT

# READ ONLY, because nothing here should be able to change the artifact it is
# checking -- and because a mount that replays a journal has modified the image,
# which changes the digest the manifest published.
mount -o ro,loop "$IMAGE" "$MNT"

fail() {
	echo "  FAIL  $*" >&2
	FAILED=1
}

pass() { echo "  ok    $*"; }

# --- the runner ------------------------------------------------------------

RUNNER_DIR="$MNT/home/runner/runner"

if [ -x "$RUNNER_DIR/run.sh" ]; then
	pass "the actions runner is installed"
else
	fail "no runner at /home/runner/runner/run.sh; every job would fail to start"
fi

# THE VERSION IS READ FROM THE IMAGE, not from the environment that built it.
# .runner-version is what the runner tarball ships; if the manifest and the image
# disagree, the manifest is describing something else.
if [ -r "$RUNNER_DIR/.runner-version" ]; then
	installed=$(tr -d '[:space:]' <"$RUNNER_DIR/.runner-version")

	pass "runner version $installed"

	if [ -n "$MANIFEST" ] && [ -r "$MANIFEST" ]; then
		claimed=$(jq -r '.runner_version' "$MANIFEST")

		if [ "$installed" != "$claimed" ]; then
			fail "the manifest claims runner $claimed and the image carries $installed"
		else
			pass "the manifest agrees with the image"
		fi
	fi
else
	# Not fatal on its own: older runner tarballs did not ship this file.
	echo "  note  no .runner-version in the image; cannot cross-check the manifest"
fi

# --- docker ----------------------------------------------------------------

if [ -x "$MNT/usr/bin/dockerd" ] || [ -x "$MNT/usr/sbin/dockerd" ]; then
	pass "dockerd is installed"
else
	fail "no dockerd; every container step would fail"
fi

if [ -x "$MNT/usr/bin/docker" ]; then
	pass "the docker client is installed"
else
	fail "no docker client"
fi

# --- billet's agent --------------------------------------------------------

AGENT="$MNT/usr/local/bin/billet-agent"

if [ -x "$AGENT" ]; then
	pass "the billet agent is installed"

	# THE CONTRACT THE IMAGE ACTUALLY SPEAKS, read out of the installed agent.
	# A manifest is free to claim any number; this is the one that will run.
	contract=$(sed -n 's/^WANT_CONTRACT=\([0-9][0-9]*\)$/\1/p' "$AGENT" | head -1)

	if [ -z "$contract" ]; then
		fail "the agent declares no contract version, so nothing can tell whether it speaks
        to this billet"
	else
		pass "the agent speaks contract $contract"

		if [ -n "$MANIFEST" ] && [ -r "$MANIFEST" ]; then
			claimed=$(jq -r '.guest_contract' "$MANIFEST")

			if [ "$contract" != "$claimed" ]; then
				fail "the manifest advertises contract $claimed and the agent speaks $contract;
        a node would accept this image on the manifest's word and then get microVMs
        that boot and never report"
			else
				pass "the manifest's contract matches the agent's"
			fi
		fi
	fi
else
	fail "no billet agent; microVMs would boot and never register"
fi

# --- the units that have to start -----------------------------------------

# ENABLED IS A SYMLINK ON DISK, which is exactly why this can be checked without
# booting: `systemctl enable` in the chroot wrote it, and if it did not, the unit
# is present and inert. That is the failure this catches -- an image where
# everything is installed and nothing starts.
WANTS="$MNT/etc/systemd/system/multi-user.target.wants"

for unit in docker.service billet-agent.service; do
	if [ -L "$WANTS/$unit" ] || [ -e "$WANTS/$unit" ]; then
		pass "$unit is enabled"
	else
		fail "$unit is installed but NOT enabled; it would never start"
	fi
done

# systemd-networkd is wanted by a different target.
if [ -e "$MNT/etc/systemd/system/dbus-org.freedesktop.network1.service" ] ||
	[ -e "$MNT/etc/systemd/system/sockets.target.wants/systemd-networkd.socket" ] ||
	[ -e "$MNT/etc/systemd/system/multi-user.target.wants/systemd-networkd.service" ]; then
	pass "systemd-networkd is enabled"
else
	fail "systemd-networkd is not enabled; the guest would have no network and could not
        reach the metadata service"
fi

# --- the guest's own network unit ------------------------------------------

if [ -r "$MNT/etc/systemd/network/10-eth0.network" ]; then
	pass "the guest network unit is present"
else
	fail "no /etc/systemd/network/10-eth0.network; the guest would not configure eth0"
fi

# --- root is locked --------------------------------------------------------

# NOT COSMETIC. An account with no password is not the same as a locked one, and
# this image exists to run other people's code.
if grep -qE '^root:[!*]' "$MNT/etc/shadow" 2>/dev/null; then
	pass "root cannot log in"
else
	fail "the root account is not locked"
fi

echo

if [ "$FAILED" -ne 0 ]; then
	echo "this image is NOT fit to publish" >&2

	exit 1
fi

echo "the image looks fit to publish"
echo
echo "NOTE: this checked the filesystem's contents, not that it boots. Run"
echo "\`billet images verify\` against a cluster for that; the two catch"
echo "different things and neither replaces the other."
