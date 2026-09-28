#!/usr/bin/env bash
# runner-images-nspawn.sh: the target driver run-runner-images.sh builds through,
# for a root filesystem booted under systemd-nspawn.
#
# WHY A BOOTED CONTAINER. GitHub's scripts assume a running system: systemd as
# PID 1, services they start, enable and stop, a reboot in the middle. A chroot
# runs none of that, and a VM needs /dev/kvm, which a builder that is itself a
# microVM does not have. systemd-nspawn --boot runs the image's own systemd over
# the image's own filesystem, so what the scripts ran in is exactly what ships.
#
# Commands:
#   start               boot the root filesystem and wait until it runs commands
#   stop                power it off and wait until it is gone
#   exec ARGV...        run ARGV as root in it; the status is ARGV's
#   copy-in SRC DEST    copy a host path in, with `cp -R` semantics
#   copy-out SRC DIR    copy a path out into a host directory
#   reboot              reboot it and wait until the new boot runs commands
#
# Environment:
#   BILLET_RI_ROOTFS    the mounted root filesystem (required)
#   BILLET_RI_MACHINE   the machine name (default: billet-ri)
set -euo pipefail

machine=${BILLET_RI_MACHINE:-billet-ri}

fail() {
	echo "runner-images-nspawn: $*" >&2
	exit 1
}

[ -n "${BILLET_RI_ROOTFS:-}" ] || fail "BILLET_RI_ROOTFS names no root filesystem"

# leader is the PID of the machine's init, which a reboot replaces; empty when
# the machine is not running.
leader() {
	machinectl show --property=Leader --value "$machine" 2>/dev/null || true
}

# ready waits until the machine answers a command, and refuses after a bound
# rather than hanging a build on a guest that never came up.
ready() {
	local i
	for i in $(seq 1 180); do
		if systemd-run --machine="$machine" --quiet --wait --pipe -- /bin/true >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	fail "$machine did not start answering commands within three minutes"
}

case "${1:-}" in
start)
	[ -z "$(leader)" ] || fail "$machine is already running"
	# EVERY CAPABILITY, because the scripts install and run docker, mount
	# filesystems and start services: the container is a build environment for an
	# image, not a boundary around it.
	#
	# ITS OWN NETWORK, NATted by the host's networkd (--network-veth): the builder
	# is itself a guest that may run docker, and two dockerds in one network
	# namespace both claim docker0. The uplink's DNS servers, because the host's
	# stub resolver on 127.0.0.53 is not reachable from another namespace.
	#
	# A REBOOT IS NSPAWN EXITING 133, which it does when the container reboots; the
	# unit restarts it on exactly that status, as systemd-nspawn@.service does, and
	# a poweroff (status 0) stays down.
	systemd-run --quiet --unit="$machine-nspawn" --collect --property=Delegate=yes \
		--property=Restart=on-failure --property=RestartForceExitStatus=133 \
		--property=SuccessExitStatus=133 -- \
		systemd-nspawn --boot --quiet --machine="$machine" --directory="$BILLET_RI_ROOTFS" \
		--capability=all --system-call-filter='@keyring bpf' --private-users=no \
		--network-veth --resolv-conf=replace-uplink --timezone=off --bind=/dev/fuse
	ready
	;;
stop)
	[ -n "$(leader)" ] || exit 0
	machinectl poweroff "$machine"
	for _ in $(seq 1 120); do
		[ -z "$(leader)" ] && exit 0
		sleep 1
	done
	machinectl terminate "$machine" || true
	fail "$machine did not power off within two minutes and was terminated"
	;;
exec)
	shift
	[ "$#" -gt 0 ] || fail "exec needs a command"
	# --pipe CARRIES THE STATUS: systemd-run --wait --pipe exits with the command's
	# own exit status, which is the step's verdict.
	exec systemd-run --machine="$machine" --quiet --wait --pipe --collect \
		--setenv=HOME=/root --setenv=DEBIAN_FRONTEND=noninteractive -- "$@"
	;;
copy-in)
	[ "$#" -eq 3 ] || fail "copy-in needs a source and a destination"
	src=$2 dest=$3
	# `cp -R` SEMANTICS: a destination that is an existing directory receives the
	# source inside it. Asked of the machine, because a path is resolved inside it.
	if systemd-run --machine="$machine" --quiet --wait --pipe -- test -d "$dest" >/dev/null 2>&1; then
		dest=${dest%/}/$(basename "$src")
	fi
	# THROUGH machinectl, NEVER A HOST cp INTO $BILLET_RI_ROOTFS: a host copy would
	# follow the container's absolute symlinks onto the host's own filesystem.
	machinectl copy-to "$machine" "$src" "$dest"
	;;
copy-out)
	[ "$#" -eq 3 ] || fail "copy-out needs a source and a directory"
	machinectl copy-from "$machine" "$2" "${3%/}/$(basename "$2")"
	;;
reboot)
	before=$(leader)
	[ -n "$before" ] || fail "$machine is not running, so it cannot reboot"
	machinectl reboot "$machine"
	for _ in $(seq 1 180); do
		now=$(leader)
		if [ -n "$now" ] && [ "$now" != "$before" ]; then
			ready
			exit 0
		fi
		sleep 1
	done
	fail "$machine did not come back from its reboot within three minutes"
	;;
*)
	fail "unknown command ${1:-}; want start, stop, exec, copy-in, copy-out or reboot"
	;;
esac
