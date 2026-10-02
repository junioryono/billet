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

# GITHUB'S TESTS ASSERT A FEW THINGS ABOUT AZURE, OR ABOUT THE MACHINE THEY RUN
# ON, that a billet guest or the container it is built in does not have. Each is
# one named test marked skipped, in the copy every step runs its tests from, by a
# prepare on or before the first step that can select it (a script's
# invoke_tests runs a file mid-build, optionally filtered by Pester FullName);
# every other test in the suite still runs.
#
# skip_upstream_test marks the test called NAME in FILE skipped: whatever follows
# the name on its It line (a conditional -Skip, say) gives way to -Skip. EXACTLY
# ONE LINE MUST MATCH, so a renamed or duplicated test stops the build here
# rather than failing it, or running unskipped, an hour later; the file is
# rewritten only when one did.
skip_upstream_test() {
	local program='
		BEGIN { want = "It \"" ENVIRON["name"] "\"" }
		{
			at = index($0, want)
			if (at > 0 && substr($0, 1, at - 1) ~ /^[ \t]*$/ && $0 ~ /\{[ \t]*$/) {
				print substr($0, 1, at - 1) want " -Skip {"
				marked++
				next
			}
			print
		}
		END {
			if (marked != 1) {
				printf "%s has %d tests named \"%s\", want exactly one\n",
					FILENAME, marked + 0, ENVIRON["name"] > "/dev/stderr"
				exit 1
			}
		}'
	target exec sh -c '
		if name=$2 awk "$3" "$1" >"$1.billet"; then
			cat "$1.billet" >"$1" && rm -f "$1.billet"
		else
			rm -f "$1.billet"
			exit 1
		fi' sh "$1" "$2" "$program"
}

# A Firecracker guest's disks are virtio (vd*), neither Azure's sd* nor nvme*.
prepare_system_tests_virtio() {
	skip_upstream_test /imagegeneration/tests/System.Tests.ps1 \
		"All SCSI and NVMe devices have read_ahead_kb set to 128"
}

# THE ROOT FILESYSTEM'S MOUNT OPTIONS reach an Azure VM through GRUB, which a
# billet guest does not boot through, and the build's container sees the
# builder's /proc/cmdline and the image's filesystem as the builder mounted it.
# The GRUB drop-in configure-environment.sh writes is still tested.
prepare_system_tests_rootflags() {
	skip_upstream_test /imagegeneration/tests/System.Tests.ps1 \
		"Kernel command line contains the root filesystem mount options" &&
		skip_upstream_test /imagegeneration/tests/System.Tests.ps1 \
			"Root filesystem is mounted with the relaxed durability options"
}

# billet's apt source names two HTTPS mirrors as two repositories rather than
# GitHub's mirror list; install_cloud_base in build-guest-image.sh writes it.
# install-apt-common.sh is the first step that runs the apt tests.
prepare_apt_tests_two_mirrors() {
	skip_upstream_test /imagegeneration/tests/Apt.Tests.ps1 \
		"Apt sources resolve through the mirror list"
}

# configure-environment.sh writes the root filesystem's mount options as a GRUB
# drop-in and then runs update-grub under bash -e; Ubuntu's cloud root filesystem
# carries no GRUB, and a billet guest's kernel is booted directly. This stand-in
# answers that one call and removes itself, so the image ships no update-grub. A
# real one is refused rather than hidden.
prepare_grub_absent() {
	target exec sh -c '
		stand_in=/usr/sbin/update-grub
		if [ -e "$stand_in" ] || [ -L "$stand_in" ]; then
			echo "$stand_in exists, so this image has a GRUB the stand-in would hide" >&2
			exit 1
		fi
		printf "%s\n" "#!/bin/sh" "# billet-update-grub-stand-in" \
			"echo \"update-grub: a billet guest boots its kernel directly; no GRUB to update\" >&2" \
			"rm -f -- \"\$0\"" >"$stand_in" &&
			chmod 0755 "$stand_in"'
}

prepares=" waagent-conf docker-daemon build-user system-tests-virtio system-tests-rootflags apt-tests-two-mirrors grub-absent "

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

# THE GRUB STAND-IN REMOVES ITSELF WHEN CALLED, so one still present means the
# step it was placed for no longer calls update-grub and the image would ship it.
# Asked of the image after the last step, whatever the placement test concluded.
# The verdict is the exit status: 0 present, 1 absent, anything else a fault.
if target exec sh -c '[ -e /usr/sbin/update-grub ] || [ -L /usr/sbin/update-grub ] || exit 1; grep -qF billet-update-grub-stand-in /usr/sbin/update-grub'; then
	fail "the update-grub stand-in is still in the image: the step it was placed for never called it"
else
	status=$?
	[ "$status" -eq 1 ] || fail "could not check the image for the update-grub stand-in (status $status)"
fi

echo "ran $total steps of GitHub's template"
