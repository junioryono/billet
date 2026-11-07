#!/usr/bin/env bash
# Usage:
#   matrix-host.sh prepare <run-dir> <base-config> <account> <kms-key-arn> [<guest-role-arn>]
#   matrix-host.sh run <run-dir> <jobs>
#   matrix-host.sh stop <run-dir>
#   matrix-host.sh judge <run-dir> <archs> <runs-file> <owner/repo>
#   matrix-host.sh stopped <run-dir>
#   matrix-host.sh down <run-dir>
#
# The acceptance matrix's half that runs on the acceptance host, beside the node its
# instances dial (#554). <run-dir> holds this run's billet binary at <run-dir>/billet, and
# everything the run makes goes under it.
#
# prepare stands an isolated deployment up from <base-config> (`billet acceptance up`),
# writes <run-dir>/tiers (one "<backend> <arch> <label>" line per tier the matrix
# dispatches to), <run-dir>/deployment (its id), <run-dir>/iam.json (the node's policy
# scoped to that id, which the caller puts on this host's role before `run`) and
# <run-dir>/teardown.json (the delete of that deployment's cache state objects, which
# decommission needs and the node's policy never grants, and which the caller puts on the
# role only for `down`; empty when the config has no ebs-s3 cache).
# <guest-role-arn> is the role inside the base config's node.ec2.instance_profile, for the
# policy's iam:PassRole; omit it when the guests carry no instance profile.
#
# run runs the deployment until <jobs> jobs have finished (`billet acceptance run
# --no-teardown`, which writes the evidence and stops the services, and leaves the destroy
# to `down`, the one teardown, which runs with the teardown's grant), under
# matrix-supervise.py beside this script: it holds <run-dir>/run.lock for as long as the run lives, writes the run's exit
# to <run-dir>/run.exit, and, when <run-dir>/stop-requested appears, sends the run ONE
# SIGTERM, as an operator's Ctrl-C would, so the run writes its evidence and stops. A stop requested before it starts means it never starts.
#
# stop requests that, and returns; it signals nothing itself.
#
# judge judges that evidence (`billet acceptance matrix judge`) for the runs named in
# <runs-file> of <owner/repo> into <run-dir>/out, beside a copy of the evidence, and exits
# with the judge's status. It judges nothing for a run with no recorded exit, which may
# still be writing.
#
# stopped requests a stop and waits as down does, and takes nothing down: it exits 0 only
# once no run lives in <run-dir> and `billet acceptance stopped` has taken both of the
# run's services' own locks, which is when the caller may put the teardown's grant on
# the role, so no live node ever holds it.
#
# down requests a stop, waits up to MATRIX_DOWN_WAIT seconds (default 1800) for the run's
# lock to be free (a reboot frees it; nothing else does while the run lives), then destroys whatever the run left (`billet acceptance down`, idempotent)
# and removes the workspace; a run still live after that is said, and nothing is taken
# down beside it. It is the step every path ends with. Before the workspace goes, the
# services' logs are kept in <run-dir>/logs on this host, never in <run-dir>/out, which
# the workflow uploads: they name the account's resources, and a cell that failed for a
# reason only the node saw cannot be explained once they are gone (run 42406366812).
set -uo pipefail

usage() {
	sed -n '2,8p' "$0" >&2
	exit 2
}

phase=${1:-}
run_dir=${2:-}
[ -n "$phase" ] && [ -n "$run_dir" ] || usage
billet="$run_dir/billet"
ws="$run_dir/ws"
supervise="$(dirname "$0")/matrix-supervise.py"

case "$phase" in
prepare)
	[ "$#" -eq 5 ] || [ "$#" -eq 6 ] || usage
	base=$3 account=$4 kms_key_arn=$5 guest_role_arn=${6:-}
	if ! "$billet" acceptance up --config "$base" --workspace "$ws" --account "$account"; then
		echo "matrix: the deployment could not be stood up" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix tiers --workspace "$ws" >"$run_dir/tiers"; then
		echo "matrix: the workspace's tiers could not be read" >&2
		exit 1
	fi
	if [ ! -s "$run_dir/tiers" ]; then
		echo "matrix: the base config gives the matrix no tier to dispatch to" >&2
		exit 1
	fi
	if ! jq -er '.deployment_id | select(. != "")' "$ws/acceptance.json" >"$run_dir/deployment"; then
		echo "matrix: the workspace records no deployment id" >&2
		exit 1
	fi
	iam=("$billet" init iam --config "$ws/billet.yaml" --kms-key-arn "$kms_key_arn")
	if [ -n "$guest_role_arn" ]; then
		iam+=(--role-arn "$guest_role_arn")
	fi
	if ! "${iam[@]}" >"$run_dir/iam.json"; then
		echo "matrix: the node's policy could not be rendered" >&2
		exit 1
	fi
	if ! "$billet" acceptance matrix teardown-policy --workspace "$ws" >"$run_dir/teardown.json"; then
		echo "matrix: the teardown's policy could not be rendered" >&2
		exit 1
	fi
	echo "matrix: deployment $(cat "$run_dir/deployment") is up; its tiers:"
	cat "$run_dir/tiers"
	;;
run)
	[ "$#" -eq 3 ] || usage
	exec python3 -I "$supervise" run "$run_dir" "$billet" acceptance run --workspace "$ws" --jobs "$3" --wait 210m --no-teardown
	;;
stop)
	[ "$#" -eq 2 ] || usage
	if [ ! -d "$run_dir" ]; then
		echo "matrix: no run directory at $run_dir; nothing to stop"
		exit 0
	fi
	: >"$run_dir/stop-requested" || exit 1
	echo "matrix: a stop is requested; a live run is signalled once by its own supervisor"
	;;
judge)
	[ "$#" -eq 5 ] || usage
	archs=$3 runs=$4 repo=$5
	mkdir -p "$run_dir/out"
	# THE LOG FIRST, whatever is judged: it is what explains a run with no exit.
	if [ -f "$run_dir/run.log" ]; then
		cp "$run_dir/run.log" "$run_dir/out/"
	fi
	if [ ! -s "$run_dir/run.exit" ]; then
		echo "matrix: the run has no recorded exit, so it may still be writing; judging nothing" >&2
		exit 1
	fi
	if [ -f "$ws/evidence.json" ]; then
		cp "$ws/evidence.json" "$run_dir/out/"
	fi
	"$billet" acceptance matrix judge --workspace "$ws" --archs "$archs" --runs "$runs" --repository "$repo" \
		--out "$run_dir/out/matrix.json" >"$run_dir/out/matrix.txt"
	judged=$?
	cat "$run_dir/out/matrix.txt"
	exit "$judged"
	;;
stopped)
	[ "$#" -eq 2 ] || usage
	if [ -d "$run_dir" ]; then
		: >"$run_dir/stop-requested" || exit 1
		if ! python3 -I "$supervise" await "$run_dir" "${MATRIX_DOWN_WAIT:-1800}"; then
			echo "matrix: the run is still live; its workspace is $ws" >&2
			exit 1
		fi
	fi
	# AND ITS SERVICES, by their own locks: the run returns once its stop's grace is
	# over, and a node still draining past it holds none of the run's lock.
	if [ -f "$ws/acceptance.json" ] && ! "$billet" acceptance stopped --workspace "$ws"; then
		echo "matrix: a service of the run may still live; its workspace is $ws" >&2
		exit 1
	fi
	echo "matrix: no run lives in $run_dir"
	;;
down)
	[ "$#" -eq 2 ] || usage
	if [ -d "$run_dir" ]; then
		: >"$run_dir/stop-requested" || exit 1
		if ! python3 -I "$supervise" await "$run_dir" "${MATRIX_DOWN_WAIT:-1800}"; then
			echo "matrix: the run is still live; nothing is taken down beside it, and its workspace is $ws" >&2
			exit 1
		fi
	fi
	if [ ! -f "$ws/acceptance.json" ]; then
		echo "matrix: no workspace at $ws; nothing to take down"
		exit 0
	fi
	for log in server.log node.log; do
		[ -f "$ws/$log" ] || continue
		if ! { mkdir -p "$run_dir/logs" && cp "$ws/$log" "$run_dir/logs/$log"; }; then
			echo "matrix: could not keep $ws/$log; the teardown removes it" >&2
		fi
	done
	"$billet" acceptance down --workspace "$ws"
	;;
*)
	usage
	;;
esac
