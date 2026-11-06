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
# writes the evidence and tears down what it made), and exits with its status. Its pid is
# kept in <run-dir>/run.pid while it runs.
#
# stop ends a run early, as an operator's Ctrl-C would: ONE SIGTERM, which cancels the
# run's wait and lets it write its evidence and tear down what it made. A second signal
# would leave the jobs running, so it is never sent. A run that is not running is
# nothing to stop.
#
# judge judges that evidence (`billet acceptance matrix judge`) for the runs named in
# <runs-file> of <owner/repo> into <run-dir>/out, beside a copy of the evidence, and exits
# with the judge's status.
#
# down destroys whatever the run left (`billet acceptance down`, idempotent) and removes
# the workspace. It is the step every path ends with.
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
	"$billet" acceptance run --workspace "$ws" --jobs "$3" --wait 150m &
	pid=$!
	if ! echo "$pid" >"$run_dir/run.pid"; then
		echo "matrix: could not record the run's pid, so it could not be stopped early; stopping it now" >&2
		kill -TERM "$pid"
	fi
	wait "$pid"
	ran=$?
	rm -f "$run_dir/run.pid"
	if [ "$ran" -ne 0 ]; then
		echo "matrix: the run ended with status $ran" >&2
	fi
	exit "$ran"
	;;
stop)
	[ "$#" -eq 2 ] || usage
	if ! pid=$(cat "$run_dir/run.pid" 2>/dev/null) || [ -z "$pid" ]; then
		echo "matrix: no run is recorded as running; nothing to stop"
		exit 0
	fi
	if ! kill -0 "$pid" 2>/dev/null; then
		echo "matrix: the run ($pid) has already ended"
		exit 0
	fi
	# THE kill ON PATH, not the builtin, so a test can count the signals sent: two of
	# the same kind merge in the receiver, which therefore cannot.
	env kill -TERM "$pid"
	echo "matrix: asked the run ($pid) to stop; it writes its evidence and tears down what it made"
	;;
judge)
	[ "$#" -eq 5 ] || usage
	archs=$3 runs=$4 repo=$5
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
