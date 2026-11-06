#!/usr/bin/env bash
# Usage:
#   matrix-host.sh prepare <run-dir> <base-config> <account> <role-arn> <kms-key-arn>
#   matrix-host.sh run <run-dir> <jobs> <archs>
#   matrix-host.sh down <run-dir>
#
# The acceptance matrix's half that runs on the acceptance host, beside the node its
# instances dial (#554). <run-dir> holds this run's billet binary at <run-dir>/billet, and
# everything the run makes goes under it.
#
# prepare stands an isolated deployment up from <base-config> (`billet acceptance up`),
# writes <run-dir>/tiers (one "<backend> <arch> <label>" line per tier the matrix
# dispatches to), <run-dir>/deployment (its id) and <run-dir>/iam.json (the node's policy
# scoped to that id, which the caller attaches to this host's role before `run`).
#
# run runs the deployment until <jobs> jobs have finished (`billet acceptance run`, which
# tears down what it made), then judges the evidence (`billet acceptance matrix judge
# --archs <archs>`) into <run-dir>/out. It exits non-zero when either did: the run's
# failure is said first, and the judge still runs, because a run that stopped waiting can
# have evidence worth judging.
#
# down destroys whatever the run left (`billet acceptance down`, idempotent) and removes
# the workspace. It is the step every path ends with.
set -uo pipefail

usage() {
	sed -n '2,5p' "$0" >&2
	exit 2
}

phase=${1:-}
run_dir=${2:-}
[ -n "$phase" ] && [ -n "$run_dir" ] || usage
billet="$run_dir/billet"
ws="$run_dir/ws"

case "$phase" in
prepare)
	[ "$#" -eq 6 ] || usage
	base=$3 account=$4 role_arn=$5 kms_key_arn=$6
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
	if ! "$billet" init iam --config "$ws/billet.yaml" --role-arn "$role_arn" --kms-key-arn "$kms_key_arn" \
		>"$run_dir/iam.json"; then
		echo "matrix: the node's policy could not be rendered" >&2
		exit 1
	fi
	echo "matrix: deployment $(cat "$run_dir/deployment") is up; its tiers:"
	cat "$run_dir/tiers"
	;;
run)
	[ "$#" -eq 4 ] || usage
	jobs=$3 archs=$4
	mkdir -p "$run_dir/out"
	"$billet" acceptance run --workspace "$ws" --jobs "$jobs" --wait 150m
	ran=$?
	if [ "$ran" -ne 0 ]; then
		echo "matrix: the run ended with status $ran; judging what it recorded" >&2
	fi
	"$billet" acceptance matrix judge --workspace "$ws" --archs "$archs" --out "$run_dir/out/matrix.json" \
		>"$run_dir/out/matrix.txt"
	judged=$?
	cat "$run_dir/out/matrix.txt"
	if [ -f "$ws/evidence.json" ]; then
		cp "$ws/evidence.json" "$run_dir/out/evidence.json"
	fi
	if [ "$ran" -ne 0 ]; then
		exit "$ran"
	fi
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
