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
#   BILLET_RI_MACHINE   the machine name (default: billet-ri), at most twelve
#                       characters so its host link is exactly ve-<name>; a build
#                       names its own, so two builds on one host never share one
#   BILLET_RI_STOP_WAIT seconds a poweroff is given before the unit is stopped
#                       (default: 120)
#   BILLET_RI_UNIT      for stop: the unit systemd says holds the machine, when a
#                       caller found it by asking rather than by this driver's
#                       own naming (default when unset: <machine>-nspawn.service;
#                       set and empty is refused, never taken for the default)
set -euo pipefail

machine=${BILLET_RI_MACHINE:-billet-ri}
unit=${BILLET_RI_UNIT-$machine-nspawn.service}
if [ -z "$unit" ]; then
	echo "no unit named for machine $machine: an empty holder is not the default one" >&2
	exit 1
fi
# THE HOST SIDE OF THE MACHINE'S LINK, named by nspawn as ve-<machine> when that
# fits an interface name (fifteen characters), which is why the name is bounded.
link=ve-$machine
# THE FORWARDING RULES ARE OWNED, found again by this comment and by nothing else.
rule_tag="billet-runner-images:$machine"

fail() {
	echo "runner-images-nspawn: $*" >&2
	exit 1
}

case "$machine" in
*[!a-z0-9-]* | '') fail "the machine name $machine is not lower-case letters, digits and hyphens" ;;
esac

# THE CGROUP HIERARCHY, a variable so a test can stand one up.
cgroup_root=${BILLET_RI_CGROUP_ROOT:-/sys/fs/cgroup}

[ -n "${BILLET_RI_ROOTFS:-}" ] || fail "BILLET_RI_ROOTFS names no root filesystem"

# running answers yes (0), no (1), or could not tell (2). NO NEEDS ALL THREE: the
# unit is inactive or failed, the machine is no longer registered, and the unit's
# cgroup, which --keep-unit makes the container's own, holds no process. A unit
# can reach "failed" with processes its kill left behind, and a registered
# machine is one systemd still counts as running.
running() {
	local state registered cgroup events
	# A HOST NOT RUN BY SYSTEMD HAS NO UNIT TO BE RUNNING, which is evidence; a
	# systemd host whose systemctl is missing is not.
	if ! command -v systemctl >/dev/null 2>&1; then
		[ -d /run/systemd/system ] && return 2
		return 1
	fi
	state=$(systemctl is-active "$unit" 2>/dev/null) || true
	case "$state" in
	active | activating | reloading | deactivating) return 0 ;;
	inactive | failed) ;;
	*) return 2 ;;
	esac
	if registered=$(machinectl show --property=Name --value "$machine" 2>&1); then
		return 0
	fi
	case "$registered" in
	*"No machine"*) ;;
	*) return 2 ;;
	esac
	cgroup=$(systemctl show --property=ControlGroup --value "$unit" 2>/dev/null) || return 2
	# cgroup.events SAYS WHETHER ANY PROCESS IS LEFT ANYWHERE BELOW IT (cgroup v2's
	# "populated"), read once; a cgroup that is gone holds nothing, and one that
	# cannot be read tells nothing.
	[ -n "$cgroup" ] && [ -e "$cgroup_root$cgroup" ] || return 1
	events=$(cat "$cgroup_root$cgroup/cgroup.events" 2>/dev/null) || return 2
	case "$events" in
	*"populated 1"*) return 0 ;;
	*"populated 0"*) return 1 ;;
	*) return 2 ;;
	esac
}

# leader is the PID of the machine's init, which a reboot replaces.
leader() {
	machinectl show --property=Leader --value "$machine" 2>/dev/null || true
}

# answers runs a probe in the machine, each bounded, so no one probe can outlast
# the wait it belongs to.
answers() {
	timeout -k 5 15 systemd-run --machine="$machine" --quiet --wait --pipe -- "$@" \
		</dev/null >/dev/null 2>&1
}

# ready waits until the machine runs commands and resolves a name, and refuses at
# a deadline rather than hanging a build on a guest that never came up. A
# machine that runs /bin/true and cannot reach the network would fail an hour of
# apt later instead of here.
ready() {
	local deadline=$((SECONDS + 180))
	while [ "$SECONDS" -lt "$deadline" ]; do
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
# THE MACHINE'S OWN LINK ONLY, by its exact name: a pattern would let every
# other container's link through too.
forwarding() {
	command -v iptables >/dev/null 2>&1 || return 0
	iptables -w "$1" FORWARD -i "$link" -m comment --comment "$rule_tag" -j ACCEPT &&
		iptables -w "$1" FORWARD -o "$link" -m conntrack --ctstate RELATED,ESTABLISHED \
			-m comment --comment "$rule_tag" -j ACCEPT
}

# tagged reports whether the listed ruleset still carries this machine's tag, as
# the whole comment: iptables -S quotes a comment holding a colon, and a
# substring would also match a machine whose name extends this one.
tagged() {
	local status=0
	grep -Eq -- "--comment \"?$rule_tag\"?( |\$)" <<<"$1" || status=$?
	case "$status" in
	0) return 0 ;;
	1) return 1 ;;
	*) fail "could not search the FORWARD chain for $machine's rules" ;;
	esac
}

# remove_forwarding deletes this machine's rules by the argument vectors it
# inserted them with, as often as the listed ruleset still shows them, and proves
# none is left: a ruleset it cannot read is not a ruleset without them. (Only this
# shape of rule was ever installed: no build ran before it.)
remove_forwarding() {
	command -v iptables >/dev/null 2>&1 || return 0
	local rules attempt
	for attempt in 1 2 3 4 5 6 7 8; do
		rules=$(iptables -w -S FORWARD) || fail "cannot read the FORWARD chain to remove $machine's rules"
		tagged "$rules" || return 0
		iptables -w -D FORWARD -i "$link" -m comment --comment "$rule_tag" -j ACCEPT 2>/dev/null || true
		iptables -w -D FORWARD -o "$link" -m conntrack --ctstate RELATED,ESTABLISHED \
			-m comment --comment "$rule_tag" -j ACCEPT 2>/dev/null || true
	done
	# THE LAST REMOVAL IS READ BACK TOO, so success on the last attempt is success.
	rules=$(iptables -w -S FORWARD) || fail "cannot read the FORWARD chain after removing $machine's rules"
	tagged "$rules" || return 0
	fail "$machine's forwarding rules are still installed after $attempt removals"
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
			# stop ALSO CANCELS A PENDING RESTART, which Restart= would otherwise
			# queue after the container went down.
			systemctl stop "$unit" || true
			machinectl terminate "$machine" 2>/dev/null || true
		fi
		status=0
		running || status=$?
		[ "$status" -eq 1 ] || fail "$unit is still running or cannot be read after being stopped"
	fi
	remove_forwarding
}

case "${1:-}" in
start)
	# BOUNDED ONLY WHERE A LINK IS NAMED FROM IT: a machine an earlier naming left
	# behind can still be stopped by its own name.
	[ "${#machine}" -le 12 ] || fail "the machine name $machine is longer than twelve characters"
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
	#
	# A CLOSED DEVICE POLICY, which --keep-unit leaves to the unit: with every
	# capability and no user namespace, the build could otherwise make or mount a
	# node for the builder's own disk and open it. Only the tun device and
	# terminals are let through. systemd-nspawn@.service also allows loop and
	# device-mapper block devices, for --image= and encrypted images; a directory
	# boot needs neither, and those classes include the builder's own disks.
	systemd-run --quiet --unit="$unit" --property=Delegate=yes \
		--property=Restart=on-failure --property=RestartForceExitStatus=133 \
		--property=SuccessExitStatus=133 \
		--property=DevicePolicy=closed \
		--property="DeviceAllow=/dev/net/tun rwm" --property="DeviceAllow=char-pts rw" -- \
		systemd-nspawn --boot --quiet --machine="$machine" --directory="$BILLET_RI_ROOTFS" \
		--keep-unit --capability=all --system-call-filter='@keyring bpf' --private-users=no \
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
