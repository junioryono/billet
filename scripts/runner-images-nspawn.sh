#!/usr/bin/env bash
# runner-images-nspawn.sh: the target driver run-runner-images.sh builds through,
# for a root filesystem booted under systemd-nspawn.
#
# WHY A BOOTED CONTAINER. GitHub's scripts assume a running system: systemd as
# PID 1, services they start, enable and stop, a reboot in the middle. A chroot
# runs none of that, and a VM needs /dev/kvm, which a builder that is itself a
# microVM does not have. systemd-nspawn --boot runs the image's own systemd over
# the image's own filesystem, so what the scripts ran in is exactly what ships.
# Measured in a Firecracker guest (2026-09-28): nspawn --boot reaches "running".
#
# Commands:
#   start               boot the root filesystem; return once it answers commands
#                       and resolves names
#   stop                power it off and prove its unit is gone
#   exec ARGV...        run ARGV as root in it; the status is ARGV's
#   copy-in SRC DEST    copy a host path in, with `cp -R` semantics
#   copy-out SRC DIR    copy a path out into a host directory
#   reboot              reboot it and wait until the new boot answers
#
# Environment:
#   BILLET_RI_ROOTFS    the mounted root filesystem (required)
#   BILLET_RI_MACHINE   the machine name (default: billet-ri); a build names its
#                       own, so two builds on one host never share one
#   BILLET_RI_STOP_WAIT seconds a poweroff is given before the unit is stopped
#                       (default: 120)
set -euo pipefail

machine=${BILLET_RI_MACHINE:-billet-ri}
unit=$machine-nspawn.service
# THE FORWARDING RULES ARE OWNED, found again by this comment and by nothing else.
rule_tag="billet-runner-images:$machine"

fail() {
	echo "runner-images-nspawn: $*" >&2
	exit 1
}

[ -n "${BILLET_RI_ROOTFS:-}" ] || fail "BILLET_RI_ROOTFS names no root filesystem"

# running answers yes (0), no (1), or could not tell (2), from the unit, which is
# what holds the machine: systemctl is-active says "inactive" or "failed" for a
# unit that is not running, and anything else it cannot vouch for.
running() {
	local state
	state=$(systemctl is-active "$unit" 2>/dev/null) || true
	case "$state" in
	active | activating | reloading | deactivating) return 0 ;;
	inactive | failed) return 1 ;;
	*) return 2 ;;
	esac
}

# leader is the PID of the machine's init, which a reboot replaces.
leader() {
	machinectl show --property=Leader --value "$machine" 2>/dev/null || true
}

answers() {
	systemd-run --machine="$machine" --quiet --wait --pipe -- "$@" </dev/null >/dev/null 2>&1
}

# ready waits until the machine runs commands and resolves a name, and refuses
# after a bound rather than hanging a build on a guest that never came up. A
# machine that runs /bin/true and cannot reach the network would fail an hour of
# apt later instead of here.
ready() {
	local i
	for i in $(seq 1 180); do
		if answers /bin/true && answers getent hosts archive.ubuntu.com; then
			return 0
		fi
		sleep 1
	done
	fail "$machine did not answer commands and resolve names within three minutes"
}

# forwarding adds (-I) or removes (-D) the rules that let the machine's veth link
# through a FORWARD policy of DROP, which the builder's own docker sets. networkd
# masquerades the link and adds no filter exception of its own.
forwarding() {
	command -v iptables >/dev/null 2>&1 || return 0
	iptables -w "$1" FORWARD -i "ve-+" -m comment --comment "$rule_tag" -j ACCEPT &&
		iptables -w "$1" FORWARD -o "ve-+" -m conntrack --ctstate RELATED,ESTABLISHED \
			-m comment --comment "$rule_tag" -j ACCEPT
}

remove_forwarding() {
	command -v iptables >/dev/null 2>&1 || return 0
	while iptables -w -C FORWARD -i "ve-+" -m comment --comment "$rule_tag" -j ACCEPT 2>/dev/null; do
		iptables -w -D FORWARD -i "ve-+" -m comment --comment "$rule_tag" -j ACCEPT
	done
	while iptables -w -C FORWARD -o "ve-+" -m conntrack --ctstate RELATED,ESTABLISHED \
		-m comment --comment "$rule_tag" -j ACCEPT 2>/dev/null; do
		iptables -w -D FORWARD -o "ve-+" -m conntrack --ctstate RELATED,ESTABLISHED \
			-m comment --comment "$rule_tag" -j ACCEPT
	done
}

stop() {
	local status=0
	running || status=$?
	if [ "$status" -eq 2 ]; then
		fail "cannot tell whether $unit is running, so $machine may still hold the filesystem"
	fi
	if [ "$status" -eq 0 ]; then
		machinectl poweroff "$machine" 2>/dev/null || true
		for _ in $(seq 1 "${BILLET_RI_STOP_WAIT:-120}"); do
			running || break
			sleep 1
		done
		# AND IF IT WOULD NOT POWER OFF, ITS UNIT IS STOPPED, which kills what is
		# left; a stop that cannot be proved fails, because the caller unmounts and
		# checks the filesystem next.
		if running; then
			systemctl stop "$unit" || true
		fi
		status=0
		running || status=$?
		[ "$status" -eq 1 ] || fail "$unit is still running or cannot be read after being stopped"
	fi
	remove_forwarding
}

case "${1:-}" in
start)
	status=0
	running || status=$?
	[ "$status" -eq 1 ] || fail "$unit is already running or cannot be read; stop it first"
	# A FAILED UNIT OF THIS NAME, left by an earlier run, would refuse the new one.
	systemctl reset-failed "$unit" 2>/dev/null || true
	forwarding -I
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
	systemd-run --quiet --unit="$unit" --property=Delegate=yes \
		--property=Restart=on-failure --property=RestartForceExitStatus=133 \
		--property=SuccessExitStatus=133 -- \
		systemd-nspawn --boot --quiet --machine="$machine" --directory="$BILLET_RI_ROOTFS" \
		--capability=all --system-call-filter='@keyring bpf' --private-users=no \
		--network-veth --resolv-conf=replace-uplink --timezone=off
	ready
	;;
stop)
	stop
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
	if answers test -d "$dest"; then
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
