#!/bin/bash
# build-ami.sh: build a runner AMI on the EC2 builder by running GitHub's template
# on the builder itself, then billet's EC2 layer (ADR-022).
#
#   build-ami.sh run                 the build unit's ExecStart
#   build-ami.sh record-reboot NEXT  ami-target.sh's reboot: the build is rebooting
#                                    and resumes at step NEXT
#
# The build is a oneshot systemd unit that cloud-init starts, unenabled, on the
# first boot. Its record (BILLET_AMI_STATE) holds its identity, a nonce billet
# invented for this build, its phase (started, rebooting, resumed) and the boot it
# ran in:
#   - no record: the first boot; the build starts at step 1.
#   - rebooting under this build's nonce, written in another boot: the planned
#     reboot; the build records resumed with this boot and continues at the step
#     the reboot named.
#   - anything else: an unplanned restart, reported as a failure.
# It ends in one of two ways, each a block on the serial console, where billet
# reads it:
#   billet-build-begin <nonce>
#   verdict=ok|fail
#   step=<the part of the build that was running>
#   template_step=<the template step that was running, on a failure in it>
#   components=<name:version,...>
#   billet-build-end <nonce>
# repeated for a dwell and then a poweroff on success, and for as long as the
# machine runs on failure, so a stop is never mistaken for success. The template's
# own output goes to the journal, never the console, so it cannot push the verdict
# out of EC2's 64 KiB console tail. The nonce guards against accidental output; it
# is not a secret from a build step, which as root can read the user data.
#
# Environment, for tests only: BILLET_AMI_ROOT prefixes every path the build reads
# or changes on the machine, and BILLET_AMI_CONSOLE, BILLET_AMI_BOOT_ID and
# BILLET_AMI_DWELL_* replace the console, the boot id and the dwell.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
root=${BILLET_AMI_ROOT:-}
env_file=$root/opt/billet-ami/build.env
state=${BILLET_AMI_STATE:-$root/var/lib/billet-ami-build/state}
console=${BILLET_AMI_CONSOLE:-/dev/console}
dwell_rounds=${BILLET_AMI_DWELL_ROUNDS:-8}
dwell_seconds=${BILLET_AMI_DWELL_SECONDS:-15}
boot_id=${BILLET_AMI_BOOT_ID:-/proc/sys/kernel/random/boot_id}

die() {
	echo "build-ami: $*" >&2
	exit 1
}

# shellcheck source=/dev/null
. "$env_file"
nonce=${BILLET_AMI_NONCE:-}
[[ $nonce =~ ^[0-9a-f]{32}$ ]] || die "$env_file names no build nonce"
work=$(dirname "$state")

# THE VERDICT, to the console only, over and over: EC2 posts its console in
# snapshots, and a single block can be missed between two of them.
verdict() {
	local round=0
	while :; do
		{
			printf 'billet-build-begin %s\n' "$nonce"
			printf '%s\n' "$@"
			printf 'billet-build-end %s\n' "$nonce"
		} >"$console" 2>/dev/null || printf '%s\n' "$@"
		round=$((round + 1))
		if [ "$1" = verdict=ok ] && [ "$round" -ge "$dwell_rounds" ]; then
			return 0
		fi
		sleep "$dwell_seconds"
	done
}

# template_step prints the number of the last template step the log announced.
template_step() {
	sed -n 's/^=== \[\([0-9][0-9]*\)\/[0-9][0-9]*\] .* ===$/\1/p' "$log" | tail -n 1
}

# read_state prints the record's value for KEY, or nothing.
read_state() {
	[ -f "$state" ] || return 0
	awk -F= -v key="$1" '$1 == key { print substr($0, length(key) + 2); exit }' "$state"
}

# write_state replaces the record with KEY=VALUE lines, durably: a reboot that
# follows must find the record it wrote, or none.
write_state() {
	install -d -m 0700 "$work"
	printf '%s\n' "$@" >"$state.next"
	sync "$state.next"
	mv -f "$state.next" "$state"
	sync "$work"
}

case "${1:-}" in
record-reboot)
	next=${2:-}
	[[ $next =~ ^[1-9][0-9]*$ ]] || die "record-reboot needs the step the build resumes at"
	[ "$(read_state nonce)" = "$nonce" ] || die "the record is not this build's"
	write_state "nonce=$nonce" "phase=rebooting" "next=$next" "boot=$(cat "$boot_id")"
	exit 0
	;;
run) ;;
*) die "unknown command ${1:-}; want run or record-reboot" ;;
esac

at=start
# A FAILURE IS SAID AND THE MACHINE KEPT, except when this process is ending
# because the template rebooted on purpose: the record says this boot's run asked
# for the reboot, and systemd says the machine is going down. A record alone is not
# that proof, because the reboot request after it can fail.
on_exit() {
	local status=$? n=
	[ "$status" -eq 0 ] && return 0
	if [ "$at" = template ] && [ "$(read_state phase)" = rebooting ] &&
		[ "$(read_state boot)" = "$(cat "$boot_id")" ] &&
		[ "$(systemctl is-system-running 2>/dev/null || true)" = stopping ]; then
		return 0
	fi
	trap - EXIT
	# THE LOG IS WHOLE BEFORE IT IS READ: tee writes it asynchronously, so the
	# build's output is closed and tee waited for, or a step that failed at once
	# could be missing from it.
	if [ -n "${tee_pid:-}" ]; then
		exec >/dev/null 2>&1
		wait "$tee_pid" 2>/dev/null || true
	fi
	[ "$at" = template ] && n=$(template_step)
	verdict verdict=fail "step=$at" "template_step=$n" components=
}
trap on_exit EXIT

scope=${BILLET_AMI_SCOPE:-}
case "$scope" in
ami-x64) toolset=toolset-2404.json plan=plan.json ;;
ami-arm64) toolset=toolset-2404-arm64.json plan=plan-arm64.json ;;
*) die "$env_file names no AMI scope (ami-x64 or ami-arm64)" ;;
esac
layer=$root/opt/billet-ami/ec2-layer.sh
# shellcheck disable=SC2206 # the accepted names are words, split on purpose
accept=(${BILLET_AMI_ACCEPT:-})

# THE BUILD'S OWN OUTPUT IS KEPT BESIDE ITS RECORD as well as in the journal, so a
# failure can say which template step was running: this run's output alone, so a
# resumed run that fails before its first step names no step of the boot before.
install -d -m 0700 "$work"
log=$work/build.log
: >"$log"
exec > >(tee -a "$log") 2>&1
tee_pid=$!

boot=$(cat "$boot_id")
if [ ! -f "$state" ]; then
	write_state "nonce=$nonce" "phase=started" "boot=$boot"
	from=1
else
	at=resume
	[ "$(read_state nonce)" = "$nonce" ] || die "the record is another build's"
	phase=$(read_state phase)
	if [ "$phase" != rebooting ]; then
		die "this boot found the build in phase '$phase', so it restarted without its planned reboot"
	fi
	recorded=$(read_state boot)
	[ -n "$recorded" ] || die "the record names no boot it was written in"
	if [ "$recorded" = "$boot" ]; then
		die "the build started again in the boot that was to reboot"
	fi
	from=$(read_state next)
	[[ $from =~ ^[1-9][0-9]*$ ]] || die "the record names no step to resume at"
	write_state "nonce=$nonce" "phase=resumed" "boot=$boot"
fi

# THE RUNNER'S OWN FILES OUTSIDE /tmp. The builder is the target, and GitHub's
# cleanup.sh empties /tmp, so the runner's working files there were gone by the
# step after it (measured on a real builder, 2026-10-25); the guest build keeps
# them on the build host, outside the image.
at=template
install -d -m 0700 "$work/tmp"
# THE RUNNER READS ITS PLAN WITH jq, which Canonical's image carries and an
# operator's base image may not; GitHub's template installs it again later.
if ! command -v jq >/dev/null; then
	apt-get -o DPkg::Lock::Timeout=600 update
	DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends jq
fi
TMPDIR="$work/tmp" \
	BILLET_RI_TARGET="$here/ami-target.sh" \
	BILLET_AMI_STATE="$state" \
	BILLET_RI_SCOPE="$scope" \
	BILLET_RI_FROM="$from" \
	BILLET_RI_IMAGE_VERSION="$(date -u +%Y%m%d).billet" \
	BILLET_RI_OUT="$work/out" \
	"$repo/scripts/run-runner-images.sh"

# THE BUILD'S OWN FILES ARE ROOT'S AGAIN. GitHub's configure-system.sh makes
# everything under /opt writable by everyone, and root runs the layer and the
# completer from here next.
chmod -R go-w "$root/opt/billet-ami"
chmod 0600 "$env_file"

# THE COMPONENTS THE DEPLOYMENT ACCEPTS AND THIS TEMPLATE OFFERS, through the
# completer and GitHub's own installers and tests the guest carries (#584). A
# component whose installer is no step of this architecture's template is one
# GitHub's image for it does not carry either, so it is left out and said.
at=components
"$repo/scripts/runner-images/install-completer.sh" "$root/" "$repo/internal/runnerimages/$toolset"
offered=()
plan_file=$repo/scripts/runner-images/$plan
for name in "${accept[@]}"; do
	script=$("$repo/scripts/check-guest-component.sh" installer "$name")
	found=0
	grep -qF "\"id\": \"$script\"" "$plan_file" || found=$?
	case "$found" in
	0) offered+=("$name") ;;
	1) echo "build-ami: $name is accepted and GitHub's template for $scope does not install it, so this image does not carry it" ;;
	*) die "could not read $plan_file to tell whether it installs $name" ;;
	esac
done
components=()
if [ "${#offered[@]}" -gt 0 ]; then
	report=$work/completion
	BILLET_COMPLETION_REPORT="$report" "$root/opt/billet/complete/run" "${offered[@]}"
	for name in "${offered[@]}"; do
		version=$(awk -v name="$name" '$1 == "installed=" name { print $2; exit }' "$report")
		[ -n "$version" ] || die "the completer did not install $name"
		"$repo/scripts/check-guest-component.sh" present "$name" "$root/"
		components+=("$name:$version")
	done
fi
rm -rf "$root/opt/billet/complete"
(
	IFS=,
	printf '%s\n' "${components[*]}"
) >"$root/etc/billet-image-components"
chmod 0644 "$root/etc/billet-image-components"

# THE JOB'S ENVIRONMENT, from what GitHub's scripts wrote, through the one writer
# the guest uses too.
at=environment
"$repo/scripts/runner-images/write-image-env.sh" "$root/etc/environment" "$root/etc/billet-image-env"

at=layer
bash "$layer"

# NOTHING OF THE BUILD STAYS IN THE IMAGE: its unit, its inputs, its record, and
# cloud-init's copies of the user data, which name the payload's URL. The machine
# id goes too, so each instance launched from the image is given its own.
at=cleanup
systemctl disable billet-ami-build.service >/dev/null 2>&1 || true
rm -f "$root/etc/systemd/system/billet-ami-build.service"
rm -rf "$root/opt/billet-ami" "$work" "$root/var/tmp/billet-ri"
cloud-init clean --logs --machine-id
sync

trap - EXIT
verdict verdict=ok step=done template_step= "components=$(
	IFS=,
	printf '%s' "${components[*]}"
)"
poweroff
