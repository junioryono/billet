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
# writes the evidence and tears down what it made), and exits with its status, which it
# also writes to <run-dir>/run.exit; <run-dir>/run.running says one is live. It watches
# for <run-dir>/stop-requested and, when it appears, sends its own child ONE SIGTERM, as
# an operator's Ctrl-C would: the run's wait is cancelled and it writes its evidence and
# tears down what it made. A second signal would leave the jobs running, so it is never
# sent. Only the run signals, and only its own child, whose pid cannot have been reused
# while it is still that child. A stop requested before it starts means it never starts.
#
# stop requests that, and returns; it signals nothing itself.
#
# judge judges that evidence (`billet acceptance matrix judge`) for the runs named in
# <runs-file> of <owner/repo> into <run-dir>/out, beside a copy of the evidence, and exits
# with the judge's status. It judges nothing for a run with no recorded exit, which may
# still be writing.
#
# down requests a stop, waits up to MATRIX_DOWN_WAIT seconds (default 1800) for a live
# run to end, then destroys whatever the run left (`billet acceptance down`, idempotent)
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
	rm -f "$run_dir/run.exit"
	if [ -e "$run_dir/stop-requested" ]; then
		echo "matrix: a stop was requested before the run started; it does not start" >&2
		echo 130 >"$run_dir/run.exit"
		exit 130
	fi
	if ! : >"$run_dir/run.running"; then
		echo "matrix: could not mark the run live, so nothing could wait for it; not starting it" >&2
		exit 1
	fi
	"$billet" acceptance run --workspace "$ws" --jobs "$3" --wait 150m &
	pid=$!
	signalled=""
	while kill -0 "$pid" 2>/dev/null; do
		if [ -z "$signalled" ] && [ -e "$run_dir/stop-requested" ]; then
			# THE kill ON PATH, not the builtin, so a test can count the signals sent:
			# two of one kind merge in the receiver, which therefore cannot.
			env kill -TERM "$pid" || echo "matrix: could not signal the run ($pid)" >&2
			signalled=1
			echo "matrix: stopping the run early; it writes its evidence and tears down what it made" >&2
		fi
		sleep "${MATRIX_POLL:-2}"
	done
	wait "$pid"
	ran=$?
	echo "$ran" >"$run_dir/run.exit"
	rm -f "$run_dir/run.running"
	if [ "$ran" -ne 0 ]; then
		echo "matrix: the run ended with status $ran" >&2
	fi
	exit "$ran"
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
	if [ -f "$ws/evidence.json" ]; then
		cp "$ws/evidence.json" "$run_dir/out/evidence.json"
	fi
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
		deadline=$((SECONDS + ${MATRIX_DOWN_WAIT:-1800}))
		while [ -e "$run_dir/run.running" ] && [ ! -e "$run_dir/run.exit" ]; do
			if [ "$SECONDS" -ge "$deadline" ]; then
				echo "matrix: the run is still live; nothing is taken down beside it, and its workspace is $ws" >&2
				exit 1
			fi
			sleep "${MATRIX_POLL:-2}"
		done
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
