#!/usr/bin/env bash
# run-runner-images.sh: build a guest by running GitHub's own runner-images
# template, step by step, against a booted Ubuntu target (#250).
#
# WHY GITHUB'S SCRIPTS AND NOT BILLET'S. The image GitHub's jobs run on is built by
# ~80 scripts, most of what it contains is decided there rather than in the
# declaration billet reads, and every gap between the two was found by a job
# failing. Running the scripts themselves makes parity hold by construction; what
# billet does differently is one reviewed file, differences.tsv.
#
# WHAT IT RUNS. scripts/runner-images/plan.json, which a test derives from the
# vendored template (upstream/images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl)
# at the pinned commit, in the template's order, with the template's environment.
#
# WHERE IT RUNS IT. A target driver, BILLET_RI_TARGET, an executable taking:
#   exec ARGV...        run ARGV as root in the target; its status is the step's
#   copy-in SRC DEST    copy a host path into the target with `cp -R` semantics
#                       (DEST an existing directory gets SRC inside it)
#   copy-out SRC DIR    copy a target path into a host directory
#   reboot              reboot the target and return once it runs commands again
# so the same plan runs in a VM, a booted container or a test's fixture.
#
# Environment:
#   BILLET_RI_TARGET         the driver (required)
#   BILLET_RI_IMAGE_VERSION  the image version GitHub's scripts record (required)
#   BILLET_RI_OUT            where downloads land (required)
#   BILLET_RI_DIR            the vendored tree (default: beside this script)
#   BILLET_RI_TOOLSET        the toolset declaration (default: billet's vendored copy)
#   BILLET_RI_PLAN           the plan (default: $BILLET_RI_DIR/plan.json)
#   BILLET_RI_DIFFERENCES    the difference list (default: $BILLET_RI_DIR/differences.tsv)
#   BILLET_RI_NO_PAUSE=1     skip the template's pause_before sleeps (tests only)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
dir=${BILLET_RI_DIR:-$here/runner-images}
plan=${BILLET_RI_PLAN:-$dir/plan.json}
differences=${BILLET_RI_DIFFERENCES:-$dir/differences.tsv}
toolset=${BILLET_RI_TOOLSET:-$here/../internal/runnerimages/toolset-2404.json}

fail() {
	echo "run-runner-images: $*" >&2
	exit 1
}

[ -n "${BILLET_RI_TARGET:-}" ] || fail "BILLET_RI_TARGET names no target driver"
[ -x "$BILLET_RI_TARGET" ] || fail "the target driver $BILLET_RI_TARGET is not executable"
[ -n "${BILLET_RI_IMAGE_VERSION:-}" ] || fail "BILLET_RI_IMAGE_VERSION is not set"
[ -n "${BILLET_RI_OUT:-}" ] || fail "BILLET_RI_OUT is not set"
[ -f "$plan" ] || fail "no plan at $plan"
[ -f "$differences" ] || fail "no difference list at $differences"
command -v jq >/dev/null || fail "jq is required to read the plan"

target() { "$BILLET_RI_TARGET" "$@"; }

# THE PREPARES billet runs before a step that assumes something about the host
# GitHub builds on. Each runs in the target as root. Named in differences.tsv,
# so the file stays the whole account of what differs.
prepare_waagent_conf() {
	target exec sh -c 'test -e /etc/waagent.conf || : >/etc/waagent.conf'
}

# THE daemon.json THE GUEST SHIPS, the one file build-guest-image.sh installs too,
# and it must be the first one dockerd reads, because what it pulls lands in
# whichever store it started with.
prepare_docker_daemon() {
	target exec mkdir -p /etc/docker &&
		target copy-in "$here/../internal/guestassets/docker-daemon.json" /etc/docker/daemon.json
}

prepare_build_user() {
	target exec sh -c 'id runner >/dev/null 2>&1 || useradd --create-home --uid 1001 --shell /bin/bash runner
		printf "runner ALL=(ALL) NOPASSWD:ALL\n" >/etc/sudoers.d/runner
		chmod 0440 /etc/sudoers.d/runner'
}

prepares=" waagent-conf docker-daemon build-user "

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT

# THE DIFFERENCE LIST IS READ WHOLE BEFORE ANYTHING RUNS, so an entry naming a step
# the plan no longer has, or a prepare this script does not implement, stops the
# build rather than being skipped over an hour in. It is kept as two lookup files
# rather than associative arrays, which the bash a Mac runs tests under lacks.
skips=$staging/skips
prepare_steps=$staging/prepares
: >"$skips"
: >"$prepare_steps"
ids=$(jq -r '.[].id' "$plan")
while IFS=$'\t' read -r action id reason || [ -n "$action" ]; do
	case "$action" in
	'' | '#'*) continue ;;
	esac
	[ -n "$id" ] && [ -n "$reason" ] || fail "a difference without a step or a reason: $action"
	if ! grep -qxF -- "$id" <<<"$ids"; then
		fail "differences.tsv names $id, which the plan does not have"
	fi
	case "$action" in
	skip) printf '%s\t%s\n' "$id" "$reason" >>"$skips" ;;
	prepare:*)
		name=${action#prepare:}
		case "$prepares" in
		*" $name "*) ;;
		*) fail "differences.tsv asks for prepare $name, which this runner does not implement" ;;
		esac
		printf '%s\t%s\n' "$id" "$name" >>"$prepare_steps"
		;;
	*) fail "a difference with an action nobody implements: $action" ;;
	esac
done <"$differences"

# lookup prints column two of every row of FILE whose column one is exactly ID.
lookup() {
	awk -F'\t' -v id="$2" '$1 == id { print $2 }' "$1"
}

# source_path is a plan path on the host: the vendored tree, or the one shared
# copy for a file billet already vendors and verifies elsewhere.
source_path() {
	case "$1" in
	images/ubuntu/toolsets/toolset-2404.json) printf '%s\n' "$toolset" ;;
	*) printf '%s\n' "$dir/upstream/$1" ;;
	esac
}

# pause turns a Go duration of minutes and seconds (5m0s) into a sleep.
pause() {
	local spec=$1 minutes=0 seconds=0
	[ -z "$spec" ] && return 0
	[ "${BILLET_RI_NO_PAUSE:-}" = 1 ] && return 0
	if [[ $spec =~ ^([0-9]+)m([0-9]+)s$ ]]; then
		minutes=${BASH_REMATCH[1]} seconds=${BASH_REMATCH[2]}
	elif [[ $spec =~ ^([0-9]+)s$ ]]; then
		seconds=${BASH_REMATCH[1]}
	else
		fail "a pause_before the runner cannot read: $spec"
	fi
	sleep $((minutes * 60 + seconds))
}

mkdir -p "$BILLET_RI_OUT"
target exec mkdir -p /var/tmp/billet-ri

total=$(jq 'length' "$plan")
n=0
while IFS= read -r step; do
	n=$((n + 1))
	id=$(jq -r .id <<<"$step")
	kind=$(jq -r .kind <<<"$step")
	echo "=== [$n/$total] $id ==="

	reason=$(lookup "$skips" "$id")
	if [ -n "$reason" ]; then
		echo "skipped: $reason"
		continue
	fi
	for name in $(lookup "$prepare_steps" "$id"); do
		echo "prepare $name"
		"prepare_${name//-/_}" || fail "step $n $id: prepare $name failed"
	done

	case "$kind" in
	file)
		destination=$(jq -r .destination <<<"$step")
		if [ "$(jq -r '.download // false' <<<"$step")" = true ]; then
			while IFS= read -r source; do
				target copy-out "$source" "$BILLET_RI_OUT" || fail "step $n $id: copy $source out failed"
			done < <(jq -r '.sources[]' <<<"$step")
			continue
		fi
		while IFS= read -r source; do
			target copy-in "$(source_path "$source")" "$destination" ||
				fail "step $n $id: copy $source to $destination failed"
		done < <(jq -r '.sources[]' <<<"$step")
		;;
	shell)
		pause "$(jq -r '.pause_before // ""' <<<"$step")"
		# A REBOOT IS THE DRIVER'S, because the template's inline `sudo reboot` drops
		# the connection it runs over and only the driver knows how to wait for the
		# target to come back.
		if [ "$(jq -r '.reboots // false' <<<"$step")" = true ]; then
			target reboot || fail "step $n $id: the target did not come back from its reboot"
			continue
		fi

		script=$staging/$n
		if [ "$(jq -r '.script // ""' <<<"$step")" != "" ]; then
			cp "$(source_path "$(jq -r .script <<<"$step")")" "$script"
		else
			# INLINE COMMANDS AS PACKER RUNS THEM: one script, stopping at the first
			# command that fails.
			{
				echo '#!/bin/sh -e'
				jq -r '.inline[]' <<<"$step"
			} >"$script"
		fi
		chmod 0755 "$script"
		remote=/var/tmp/billet-ri/$n
		target copy-in "$script" "$remote" || fail "step $n $id: could not stage the script"

		# THE TEMPLATE'S ENVIRONMENT, passed through env(1) as Packer's {{ .Vars }}
		# does, with the build's image version filled in.
		env=(env)
		while IFS= read -r pair; do
			env+=("${pair//@image_version@/$BILLET_RI_IMAGE_VERSION}")
		done < <(jq -r '(.env // {}) | to_entries[] | "\(.key)=\(.value)"' <<<"$step")

		execute=$(jq -r .execute <<<"$step")
		case "$execute" in
		root) command=("${env[@]}" "$remote") ;;
		root-pwsh) command=("${env[@]}" pwsh -f "$remote") ;;
		user) command=(runuser -u runner -- "${env[@]}" "$remote") ;;
		*) fail "step $n $id: an execute kind the runner does not know: $execute" ;;
		esac
		status=0
		target exec "${command[@]}" || status=$?
		if [ "$status" -ne 0 ]; then
			fail "step $n $id failed with status $status"
		fi
		;;
	*) fail "step $n $id: a kind the runner does not know: $kind" ;;
	esac
done < <(jq -c '.[]' "$plan")

echo "ran $total steps of GitHub's template"
