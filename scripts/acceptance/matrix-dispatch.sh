#!/usr/bin/env bash
# Usage: matrix-dispatch.sh <owner/repo> <tag> <label>...
#
# Dispatches the acceptance matrix's workflows in <owner/repo> for each tier label, and
# waits for every run to finish: the cold, two-writer and failed-writer runs together,
# then, once the cold runs have finished and their publications have had time to land,
# the warm runs with the same tag. It judges nothing (a run's conclusion is not the
# matrix's verdict; `billet acceptance matrix judge` reads the ledger), and exits 0 when
# every run it started has completed, 1 when one could not be found or did not finish in
# time. Needs gh, authenticated to dispatch workflows in <owner/repo>.
#
# MATRIX_SETTLE (seconds, default 150) is how long after the cold runs the warm ones are
# sent. It bounds the wait for a publication, and proves nothing: a publication that has
# not landed makes a warm cell fail, never pass. MATRIX_TIMEOUT (seconds, default 5400) is
# how long any one run may take to finish, and MATRIX_LIST_WAIT (seconds, default 120) how
# long GitHub may take to list a run it was sent.
set -uo pipefail

repo=${1:?usage: matrix-dispatch.sh <owner/repo> <tag> <label>...}
tag=${2:?usage: matrix-dispatch.sh <owner/repo> <tag> <label>...}
shift 2
if [ "$#" -eq 0 ]; then
	echo "matrix-dispatch.sh: name at least one tier label" >&2
	exit 1
fi
settle=${MATRIX_SETTLE:-150}
timeout=${MATRIX_TIMEOUT:-5400}
poll=${MATRIX_POLL:-15}
list_wait=${MATRIX_LIST_WAIT:-120}

# dispatch sends one workflow for one label and prints the run's id once GitHub lists
# it, found by the run-name every matrix workflow sets from its inputs.
dispatch() {
	local workflow=$1 label=$2 title id deadline
	title="${workflow%.yml} $label $tag"
	if ! gh workflow run "$workflow" -R "$repo" -f runs_on="$label" -f tag="$tag" >/dev/null; then
		echo "could not dispatch $workflow for $label" >&2
		return 1
	fi
	deadline=$((SECONDS + list_wait))
	while :; do
		id=$(gh run list -R "$repo" --workflow "$workflow" --event workflow_dispatch --limit 50 \
			--json databaseId,displayTitle --jq ".[] | select(.displayTitle == \"$title\") | .databaseId" | head -n 1)
		if [ -n "$id" ]; then
			echo "$id"
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "GitHub lists no run of $workflow titled \"$title\" ${list_wait}s after it was sent" >&2
			return 1
		fi
		sleep "$poll"
	done
}

# await waits for each run id to complete, whatever its conclusion.
await() {
	local id status deadline
	for id in "$@"; do
		deadline=$((SECONDS + timeout))
		while :; do
			status=$(gh run view "$id" -R "$repo" --json status --jq .status)
			if [ "$status" = completed ]; then
				echo "run $id completed: $(gh run view "$id" -R "$repo" --json conclusion --jq .conclusion)"
				break
			fi
			if [ "$SECONDS" -ge "$deadline" ]; then
				echo "run $id did not complete within ${timeout}s (it is ${status:-unknown})" >&2
				return 1
			fi
			sleep "$poll"
		done
	done
}

cold=()
others=()
for label in "$@"; do
	id=$(dispatch matrix-cold.yml "$label") || exit 1
	cold+=("$id")
	for workflow in matrix-writers.yml matrix-writer-fails.yml; do
		id=$(dispatch "$workflow" "$label") || exit 1
		others+=("$id")
	done
done

await "${cold[@]}" || exit 1
echo "the cold runs are done; the warm ones are sent in ${settle}s, once their publications have had time to land"
sleep "$settle"

warm=()
for label in "$@"; do
	id=$(dispatch matrix-warm.yml "$label") || exit 1
	warm+=("$id")
done

await "${others[@]}" "${warm[@]}" || exit 1
echo "every matrix run for tag $tag has completed"
