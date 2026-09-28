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

# EVERY TARGET CALL READS NOTHING: a step that reads stdin would otherwise read the
# plan this script is iterating, and a plan read to its end is a build that
# reports success over steps it never ran.
target() { "$BILLET_RI_TARGET" "$@" </dev/null; }

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

# GITHUB'S SYSTEM TESTS ASSERT AZURE'S DISKS: one requires an sd* block device
# with the read-ahead its udev rule sets, and a Firecracker guest's disks are
# virtio (vd*). That one assertion is marked skipped, in the tests the final step
# runs, and every other test in the suite still runs; the edit is proved, so a
# changed test file stops the build rather than failing it an hour later.
prepare_system_tests_virtio() {
	target exec sh -c '
		test_file=/imagegeneration/tests/System.Tests.ps1
		sed -i "s/^\( *It \"All sd\* devices have read_ahead_kb set to 128\"\) {/\1 -Skip {/" "$test_file"
		grep -q "It \"All sd\* devices have read_ahead_kb set to 128\" -Skip {" "$test_file"'
}

prepares=" waagent-conf docker-daemon build-user system-tests-virtio "

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

# EXACTLY THREE FIELDS, EACH SAID ONCE. A read that collapses tabs would take a
# missing field from the next one; a step named twice, or skipped and prepared,
# would have one of its entries silently ignored.
problems=$(awk -F'\t' '
	/\r/ { printf "line %d carries a carriage return\n", NR; next }
	/^#/ || /^$/ { next }
	NF != 3 || $1 == "" || $2 == "" || $3 == "" {
		printf "line %d: want three tab-separated fields, an action, a step and a reason\n", NR
		next
	}
	seen[$1 FS $2]++ { printf "line %d: %s for %s is listed twice\n", NR, $1, $2 }
	$1 == "skip" { skipped[$2] = 1 }
	{ entries[$2]++ }
	END {
		for (id in skipped) {
			if (entries[id] > 1) {
				printf "%s is skipped and given another difference, so one of them would be ignored\n", id
			}
		}
	}
' "$differences")
[ -z "$problems" ] || fail "differences.tsv: $problems"

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

# EVERY STEP STARTS IN THE ENVIRONMENT THE STEPS BEFORE IT BUILT. Packer runs a
# step over ssh and sudo, and PAM's pam_env loads /etc/environment into both: a
# step reads what configure-environment.sh wrote there (AGENT_TOOLSDIRECTORY,
# say). A service started for a step loads nothing, so this wrapper reads the
# file through pam-environment.awk, the one reader of it the build has, and then
# the step's own variables apply on top. A file it cannot read fails the step.
with_environment=/var/tmp/billet-ri/with-environment
cat >"$staging/with-environment" <<'WRAPPER'
#!/bin/sh
file=${BILLET_RI_ENVIRONMENT:-/etc/environment}
reader=${BILLET_RI_ENVIRONMENT_READER:-/var/tmp/billet-ri/pam-environment.awk}
if [ -e "$file" ]; then
	normalized=$(mktemp) || exit 1
	if ! awk -f "$reader" "$file" >"$normalized"; then
		echo "with-environment: could not read $file" >&2
		rm -f "$normalized"
		exit 1
	fi
	while IFS= read -r line; do
		export "$line"
	done <"$normalized"
	rm -f "$normalized"
fi
exec "$@"
WRAPPER
chmod 0755 "$staging/with-environment"
target copy-in "$staging/with-environment" "$with_environment" || fail "could not stage the environment wrapper"
target copy-in "$dir/pam-environment.awk" /var/tmp/billet-ri/pam-environment.awk ||
	fail "could not stage the environment reader"

total=$(jq 'length' "$plan")
n=0
# THE PLAN IS READ ON ITS OWN DESCRIPTOR, never on stdin, which the steps must not
# be able to reach.
while IFS= read -r step <&3; do
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
		root) command=("$with_environment" "${env[@]}" "$remote") ;;
		root-pwsh) command=("$with_environment" "${env[@]}" pwsh -f "$remote") ;;
		user) command=(runuser -u runner -- "$with_environment" "${env[@]}" "$remote") ;;
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
done 3< <(jq -c '.[]' "$plan")

# EVERY STEP WAS REACHED, which is what "the template ran" means.
[ "$n" -eq "$total" ] || fail "reached $n of the plan's $total steps"
echo "ran $total steps of GitHub's template"
