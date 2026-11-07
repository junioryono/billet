#!/usr/bin/env bash
# Usage:
#   matrix-dispatch.sh images <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh cold <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh warm <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh writers <owner/repo> <tag> <runs-file> <label>...
#
# Dispatches the acceptance matrix's workflows in <owner/repo> for each tier label and
# appends the id of every run it starts to <runs-file>, which is what `billet acceptance
# matrix judge --runs` reads. `images` runs first and alone on each tier: it sends the
# image store's cold run, waits for it, waits MATRIX_SETTLE seconds, and sends and waits
# for the warm one, because every job on a tier publishes an image-store generation and
# none may run beside that pair. `cold` sends the cold and failed-writer runs and waits for
# the cold ones to finish; `warm` waits MATRIX_SETTLE seconds more, so the cold runs'
# publications have time to land, sends the warm runs with the same tag, and waits for
# every run in <runs-file> to finish. `writers` comes last, and sends each label's
# two-writer run only once the one before it has finished, because each writer a holds a
# runner while it waits for its writer b, and two architectures' at once could hold every
# slot (the workflow calls it once per architecture, so each has its own deadline). Every
# phase is a step of its own with an installation token of its own, because a token lasts
# an hour.
#
# It judges nothing (a run's conclusion is not the matrix's verdict; the judge reads the
# ledger). It exits 0 when every run it waited for completed, and 1 when one could not
# be dispatched, was never listed, or did not finish within the phase's MATRIX_TIMEOUT.
# Needs gh, authenticated to dispatch workflows in <owner/repo>.
#
# MATRIX_SETTLE (seconds, default 150) bounds the wait for a publication and proves
# nothing: one that has not landed makes a warm cell fail, never pass. MATRIX_TIMEOUT
# (seconds, default 2700) is the phase's whole deadline, inside a token's hour, and
# MATRIX_LIST_WAIT (seconds, default 120) how long GitHub may take to list a run sent.
set -uo pipefail

usage() {
	sed -n '2,6p' "$0" >&2
	exit 2
}

[ "$#" -ge 5 ] || usage
phase=$1 repo=$2 tag=$3 runs=$4
shift 4
settle=${MATRIX_SETTLE:-150}
timeout=${MATRIX_TIMEOUT:-2700}
poll=${MATRIX_POLL:-15}
list_wait=${MATRIX_LIST_WAIT:-120}
deadline=$((SECONDS + timeout))

# dispatch sends one workflow for one label, appends the run's id to the runs file once
# GitHub lists it (found by the run-name every matrix workflow sets from its inputs),
# and prints it.
dispatch() {
	local workflow=$1 label=$2 title id listed
	title="${workflow%.yml} $label $tag"
	if ! gh workflow run "$workflow" -R "$repo" -f runs_on="$label" -f tag="$tag" >/dev/null; then
		echo "could not dispatch $workflow for $label" >&2
		return 1
	fi
	listed=$((SECONDS + list_wait))
	while :; do
		id=$(gh run list -R "$repo" --workflow "$workflow" --event workflow_dispatch --limit 50 \
			--json databaseId,displayTitle --jq ".[] | select(.displayTitle == \"$title\") | .databaseId" | head -n 1)
		if [ -n "$id" ]; then
			# RECORDED OR FAILED: a run the judge is not told of is a run whose cells
			# read as not run, and that has to be this script's failure, said here.
			if ! echo "$id" >>"$runs"; then
				echo "dispatched run $id of $workflow for $label, and could not record it in $runs" >&2
				return 1
			fi
			echo "$id"
			return 0
		fi
		if [ "$SECONDS" -ge "$listed" ]; then
			echo "GitHub lists no run of $workflow titled \"$title\" ${list_wait}s after it was sent" >&2
			return 1
		fi
		sleep "$poll"
	done
}

# await waits for each run id to complete, whatever its conclusion, within the phase's
# one deadline.
await() {
	local id status
	for id in "$@"; do
		while :; do
			status=$(gh run view "$id" -R "$repo" --json status --jq .status)
			if [ "$status" = completed ]; then
				echo "run $id completed: $(gh run view "$id" -R "$repo" --json conclusion --jq .conclusion)"
				break
			fi
			if [ "$SECONDS" -ge "$deadline" ]; then
				echo "run $id did not complete within the phase's ${timeout}s (it is ${status:-unknown})" >&2
				return 1
			fi
			sleep "$poll"
		done
	done
}

case "$phase" in
images)
	cold=()
	for label in "$@"; do
		id=$(dispatch matrix-images-cold.yml "$label") || exit 1
		cold+=("$id")
	done
	await "${cold[@]}" || exit 1
	echo "the image store's cold runs are done; its warm ones are sent in ${settle}s"
	sleep "$settle"
	warm=()
	for label in "$@"; do
		id=$(dispatch matrix-images-warm.yml "$label") || exit 1
		warm+=("$id")
	done
	await "${warm[@]}" || exit 1
	echo "the image store's runs are done"
	;;
cold)
	cold=()
	for label in "$@"; do
		id=$(dispatch matrix-cold.yml "$label") || exit 1
		cold+=("$id")
		dispatch matrix-writer-fails.yml "$label" >/dev/null || exit 1
	done
	await "${cold[@]}" || exit 1
	echo "the cold runs are done"
	;;
warm)
	echo "the warm runs are sent in ${settle}s, once the cold runs' publications have had time to land"
	sleep "$settle"
	for label in "$@"; do
		dispatch matrix-warm.yml "$label" >/dev/null || exit 1
	done
	every=()
	while read -r id; do
		[ -n "$id" ] && every+=("$id")
	done <"$runs"
	if [ "${#every[@]}" -eq 0 ]; then
		echo "$runs names no run" >&2
		exit 1
	fi
	await "${every[@]}" || exit 1
	echo "every matrix run for tag $tag has completed"
	;;
writers)
	for label in "$@"; do
		id=$(dispatch matrix-writers.yml "$label") || exit 1
		await "$id" || exit 1
	done
	echo "the two-writer runs are done"
	;;
*)
	usage
	;;
esac
