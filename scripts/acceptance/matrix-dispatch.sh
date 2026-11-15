#!/usr/bin/env bash
# Usage:
#   matrix-dispatch.sh images-cold <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh await <owner/repo> <tag> <runs-file> -
#   matrix-dispatch.sh images-warm <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh cold <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh warm <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh writers <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh writers-apart <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh lost <owner/repo> <tag> <runs-file> <label>...
#   matrix-dispatch.sh store-unreachable <owner/repo> <tag> <runs-file> <label>...
#
# Dispatches the acceptance matrix's workflows in <owner/repo> for each tier label and
# appends the id of every run it starts to <runs-file>, which is what `billet acceptance
# matrix judge --runs` reads. The image store's runs go first and alone on each tier,
# because every job on a tier publishes an image-store generation and none may run beside
# the cold and warm pair, in three steps, each with a token of its own, since a pool's tier
# has no host until its first job asks for one and a host takes about 45 minutes to boot,
# join and import the guest image (#829): `images-cold` sends the cold runs and waits for
# nothing, `await` waits for every run in <runs-file> within MATRIX_TIMEOUT, and
# `images-warm` waits MATRIX_SETTLE seconds and sends and waits for the warm ones (the
# per-job EC2 lane waits for the cold runs' publication itself, and settles 0). `cold`
# sends the cold and failed-writer runs and waits for
# the cold ones to finish; `warm` waits MATRIX_SETTLE seconds more, so the cold runs'
# publications have time to land, sends the warm runs with the same tag, and waits for
# every run in <runs-file> to finish. `writers` comes last, and sends each label's
# two-writer run only once the one before it has finished, because each writer a holds a
# runner while it waits for its writer b, and two architectures' at once could hold every
# slot (the workflow calls it once per architecture, so each has its own deadline).
# `writers-apart` sends every label's at once and waits for them all, for tiers that share
# no host, as a pool's architectures do: one waiting for another would leave its host
# idle long enough for the pool to release it. `lost` sends every label's instance-lost run
# and returns once each one's writer is holding its disk, appending "<label> <runner>" for
# each to <runs-file>.lost, the runner being the writer's instance name, which the caller
# terminates; `await` then waits for the runs. `store-unreachable` sends every label's
# store-unreachable run at once and waits for them all; the caller has cut the node off
# from its store first, and nothing else may run beside them, since the breaker they open
# sends every job on the node cold. Every
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
	sed -n '2,12p' "$0" >&2
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

# holding waits, within the phase's deadline, for a run's writer to be holding its disk
# (its hold step running) and appends "<label> <runner>" to <runs-file>.lost. A writer that
# ended first, or one whose runner GitHub does not name, fails the phase: it can no longer
# be lost while it holds the disk.
holding() {
	local id=$1 label=$2 line status runner step
	while :; do
		# EVERY FIELD CHECKED IN jq, BEFORE IT IS JOINED: a runner name carrying the joining
		# character or a newline could otherwise forge the fields after it. A field that is
		# not what GitHub's API gives is "!", which nothing below accepts. Joined on a
		# character IFS does not fold, since with a tab an empty runner collapsed and the
		# step's state was read as the runner's. The first writer only.
		line=$(gh api "repos/$repo/actions/runs/$id/jobs" --jq '
			def word: if type == "string" and test("^[a-z_]*$") then . else "!" end;
			[.jobs[] | select(.name == "writer")] | first // empty |
			[(.status // "" | word),
			 (.runner_name // "" | if type == "string" and test("^(billet-[0-9a-f]{32})?$") then . else "!" end),
			 ([.steps[]? | select(.name == "Hold the disk until the instance is lost") | .status][0] // "" | word)] |
			join("|")')
		IFS='|' read -r status runner step <<<"$line"
		if [ "$step" = in_progress ]; then
			if [ -z "$runner" ]; then
				echo "run $id's writer holds its disk and GitHub names no runner for it" >&2
				return 1
			fi
			# ONLY A NAME BILLET GIVES, which is its lease's: the caller puts it on a command
			# line run on the acceptance host, and nothing GitHub says is quoted there.
			if ! [[ "$runner" =~ ^billet-[0-9a-f]{32}$ ]]; then
				echo "run $id's writer holds its disk on a runner billet would not name; nothing is recorded" >&2
				return 1
			fi
			if ! echo "$label $runner" >>"$runs.lost"; then
				echo "run $id's writer holds its disk on $runner, and that could not be recorded in $runs.lost" >&2
				return 1
			fi
			echo "run $id's writer holds its disk on $runner"
			return 0
		fi
		if [ "$status" = completed ]; then
			echo "run $id's writer ended before it held its disk" >&2
			return 1
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "run $id's writer did not hold its disk within the phase's ${timeout}s (it is ${status:-unknown})" >&2
			return 1
		fi
		sleep "$poll"
	done
}

case "$phase" in
images-cold)
	for label in "$@"; do
		dispatch matrix-images-cold.yml "$label" >/dev/null || exit 1
	done
	echo "the image store's cold runs are sent"
	;;
await)
	every=()
	while read -r id; do
		[ -n "$id" ] && every+=("$id")
	done <"$runs"
	if [ "${#every[@]}" -eq 0 ]; then
		echo "$runs names no run" >&2
		exit 1
	fi
	await "${every[@]}" || exit 1
	echo "every run sent so far has completed"
	;;
images-warm)
	echo "the image store's warm runs are sent in ${settle}s"
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
writers-apart)
	writers=()
	for label in "$@"; do
		id=$(dispatch matrix-writers.yml "$label") || exit 1
		writers+=("$id")
	done
	await "${writers[@]}" || exit 1
	echo "the two-writer runs are done"
	;;
lost)
	lost=()
	for label in "$@"; do
		id=$(dispatch matrix-instance-lost.yml "$label") || exit 1
		lost+=("$id:$label")
	done
	for sent in "${lost[@]}"; do
		holding "${sent%%:*}" "${sent#*:}" || exit 1
	done
	echo "every instance-lost writer holds its disk"
	;;
store-unreachable)
	unreachable=()
	for label in "$@"; do
		id=$(dispatch matrix-store-unreachable.yml "$label") || exit 1
		unreachable+=("$id")
	done
	await "${unreachable[@]}" || exit 1
	echo "the store-unreachable runs are done"
	;;
*)
	usage
	;;
esac
