#!/usr/bin/env bash
# Usage:
#   matrix-host.sh prepare <run-dir> <base-config> <account> <kms-key-arn> [<guest-role-arn>]
#   matrix-host.sh run <run-dir> <jobs>
#   matrix-host.sh stop <run-dir>
#   matrix-host.sh judge <run-dir> <archs> <runs-file> <owner/repo>
#   matrix-host.sh down <run-dir>
#
# The acceptance matrix's half that runs on the acceptance host, beside the node its
# instances dial (#554). <run-dir> holds this run's billet binary at <run-dir>/billet, and
# everything the run makes goes under it.
#
# prepare stands an isolated deployment up from <base-config> (`billet acceptance up`),
# writes <run-dir>/tiers (one "<backend> <arch> <label>" line per tier the matrix
# dispatches to), <run-dir>/deployment (its id) and <run-dir>/iam.json (the node's policy
# scoped to that id, which the caller puts on this host's role before `run`).
# <guest-role-arn> is the role inside the base config's node.ec2.instance_profile, for the
# policy's iam:PassRole; omit it when the guests carry no instance profile.
#
# run runs the deployment until <jobs> jobs have finished (`billet acceptance run`, which
# writes the evidence and tears down what it made), under matrix-supervise.py beside this
# script: it holds <run-dir>/run.lock for as long as the run lives, writes the run's exit
# to <run-dir>/run.exit, and, when <run-dir>/stop-requested appears, sends the run ONE
# SIGTERM, as an operator's Ctrl-C would, so the run writes its evidence and tears down
# what it made. A stop requested before it starts means it never starts.
#
# stop requests that, and returns; it signals nothing itself.
#
# judge judges that evidence (`billet acceptance matrix judge`) for the runs named in
# <runs-file> of <owner/repo> into <run-dir>/out, beside a copy of the evidence, and exits
# with the judge's status. It judges nothing for a run with no recorded exit, which may
# still be writing.
#
# down requests a stop, waits up to MATRIX_DOWN_WAIT seconds (default 1800) for the run's
# lock to be free (a reboot frees it; nothing else does while the run lives), then destroys whatever the run left (`billet acceptance down`, idempotent)
# and removes the workspace; a run still live after that is said, and nothing is taken
# down beside it. It is the step every path ends with.
set -uo pipefail

usage() {
	sed -n '2,7p' "$0" >&2
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
	echo "matrix: deployment $(cat "$run_dir/deployment") is up; its tiers:"
	cat "$run_dir/tiers"
	;;
run)
	[ "$#" -eq 3 ] || usage
	exec python3 -I "$supervise" run "$run_dir" "$billet" acceptance run --workspace "$ws" --jobs "$3" --wait 150m
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
	if [ ! -s "$run_dir/run.exit" ]; then
		echo "matrix: the run has no recorded exit, so it may still be writing; judging nothing" >&2
		exit 1
	fi
	mkdir -p "$run_dir/out"
	for kept in "$ws/evidence.json" "$run_dir/run.log"; do
		if [ -f "$kept" ]; then
			cp "$kept" "$run_dir/out/"
		fi
	done
	"$billet" acceptance matrix judge --workspace "$ws" --archs "$archs" --runs "$runs" --repository "$repo" \
		--out "$run_dir/out/matrix.json" >"$run_dir/out/matrix.txt"
	judged=$?
	cat "$run_dir/out/matrix.txt"
	exit "$judged"
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
	"$billet" acceptance down --workspace "$ws"
	;;
*)
	usage
	;;
esac
