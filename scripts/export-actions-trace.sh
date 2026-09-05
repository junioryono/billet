#!/bin/bash
# Export a repository's Actions history as a trace internal/replay can replay.
#
# WHAT IT WRITES. One JSON line per completed job, the shape internal/replay's
# ReadTrace reads: when the job was queued (`at`, the job's created_at), the
# runs-on label it asked for (`tier`, its first label unless --label chose one),
# the owner and repository, the workflow it belongs to spelled the way the
# scale-set wire spells one (owner/repo/.github/workflows/ci.yml@refs/heads/x),
# the run id, how long it ran (`duration`, completed_at minus started_at, in
# seconds) and GitHub's conclusion as the wire spells it (`succeeded` for
# success; every other conclusion verbatim). The replay maps labels onto the
# fleet it is given and reports the ones it cannot.
#
# HOW IT ASKS. `gh api --paginate` over the repository's runs since a date, then
# each run's jobs, each answer written to a file before jq reads it, so gh is
# never a pipeline's writer: a failing page is that command's own exit status and
# fails the script rather than truncating the trace. jq runs here rather than
# inside gh, because `gh api --jq` takes no variables and the values below come
# from the command line.
#
#   scripts/export-actions-trace.sh OWNER/REPO --since 2026-03-01 [--label billet-2vcpu] > trace.jsonl
set -euo pipefail

usage() {
	printf 'usage: %s OWNER/REPO --since YYYY-MM-DD [--label LABEL]\n' "$0" >&2
	exit 2
}

repo=""
since=""
label=""

while [ $# -gt 0 ]; do
	case "$1" in
	--since)
		[ $# -ge 2 ] || usage
		since=$2
		shift 2
		;;
	--label)
		[ $# -ge 2 ] || usage
		label=$2
		shift 2
		;;
	-h | --help)
		usage
		;;
	-*)
		printf 'unknown flag %s\n' "$1" >&2
		usage
		;;
	*)
		[ -z "$repo" ] || usage
		repo=$1
		shift
		;;
	esac
done

[ -n "$repo" ] && [ -n "$since" ] || usage

case "$repo" in
*/*) ;;
*)
	printf 'the repository must be spelled OWNER/REPO, got %s\n' "$repo" >&2
	exit 2
	;;
esac

case "$since" in
[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
*)
	printf -- '--since takes a date spelled YYYY-MM-DD, got %s\n' "$since" >&2
	exit 2
	;;
esac

for tool in gh jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf '%s is not on PATH; this exporter needs gh to ask GitHub and jq to shape the answer\n' "$tool" >&2
		exit 1
	fi
done

owner=${repo%%/*}
name=${repo#*/}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# THE RUNS FIRST, TO A FILE, so the loop below reads a complete list and a failed
# page is this command's exit status rather than a shorter loop. --paginate
# concatenates pages, which jq reads as a stream of objects.
gh api --paginate \
	"repos/$repo/actions/runs?status=completed&per_page=100&created=%3E%3D$since" \
	>"$work/runs.json"

jq -r '.workflow_runs[] | [.id, .path, .head_branch] | @tsv' "$work/runs.json" >"$work/runs.tsv"

runs=0
jobs=0

while IFS=$'\t' read -r run_id path branch; do
	[ -n "$run_id" ] || continue

	runs=$((runs + 1))
	workflow="$repo/$path@refs/heads/$branch"

	gh api --paginate "repos/$repo/actions/runs/$run_id/jobs?per_page=100" >"$work/jobs.json"

	# Only a job that ran to completion has a duration to replay. `fromdate`
	# reads GitHub's RFC 3339 stamps; the difference is whole seconds, spelled
	# as a Go duration.
	jq -r --arg owner "$owner" --arg name "$name" --arg workflow "$workflow" \
		--argjson run_id "$run_id" --arg label "$label" '
		.jobs[]
		| select(.status == "completed" and .started_at != null and .completed_at != null)
		| select($label == "" or (.labels | index($label)) != null)
		| {
			at: .created_at,
			tier: (if $label != "" then $label else (.labels[0] // "") end),
			owner: $owner,
			repository: $name,
			workflow: $workflow,
			run_id: $run_id,
			duration: ((((.completed_at | fromdate) - (.started_at | fromdate)) | tostring) + "s"),
			result: (if .conclusion == "success" then "succeeded" else .conclusion end)
		}
		| @json' "$work/jobs.json" >"$work/jobs.jsonl"

	if [ -s "$work/jobs.jsonl" ]; then
		jobs=$((jobs + $(wc -l <"$work/jobs.jsonl")))
		cat "$work/jobs.jsonl"
	fi
done <"$work/runs.tsv"

printf 'exported %d jobs from %d completed runs of %s since %s\n' "$jobs" "$runs" "$repo" "$since" >&2

if [ "$jobs" -eq 0 ]; then
	printf 'no completed jobs matched; nothing to replay\n' >&2
	exit 1
fi
