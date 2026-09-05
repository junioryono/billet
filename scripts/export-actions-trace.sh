#!/bin/bash
# Export a repository's Actions history as a trace internal/replay can replay.
#
# WHAT IT WRITES. One JSON line per completed job, the shape internal/replay's
# ReadTrace reads: when the job was queued (`at`, the job's created_at), the
# runs-on label that names its tier, the owner and repository, the workflow it
# belongs to spelled the way the scale-set wire spells one
# (owner/repo/.github/workflows/ci.yml@refs/heads/x), the run id, how long it
# ran (`duration`, completed_at minus started_at, in seconds) and GitHub's
# conclusion as the wire spells it (`succeeded` for success; every other
# conclusion verbatim).
#
# WHICH LABEL IS THE TIER IS SAID, NEVER GUESSED. A job's labels come in the
# order the workflow wrote them, and a self-hosted job commonly lists
# `self-hosted`, an OS and an architecture beside the label that routes it, so
# "the first label" is whichever the author typed first. `--label L` exports the
# jobs that asked for L, under tier L; `--prefix P` exports the jobs with exactly
# one label starting with P, under that label, which is how a fleet whose tiers
# are all `billet-*` is exported in one pass. A job matching neither rule, or
# several labels under the prefix, is left out and counted on stderr.
#
# HOW IT ASKS. `gh api --paginate` over the repository's runs since a date, then
# each run's jobs, each answer written to a file before jq reads it, so gh is
# never a pipeline's writer: a failing page is that command's own exit status and
# fails the script rather than truncating the trace. jq runs here rather than
# inside gh, because `gh api --jq` takes no variables and the values below come
# from the command line.
#
# THE TRACE REACHES STDOUT WHOLE OR NOT AT ALL. It is assembled under the work
# directory and written only after the last page has answered, because with the
# documented `> trace.jsonl` a run that failed on its last page would otherwise
# leave a shorter file that reads as a complete, smaller workload. A job whose
# stamps agree to the second is given one second: GitHub's clock is whole
# seconds, and a zero is not a duration the replay can hold. A job that
# completed before it started, or one that completed with no conclusion, is
# not rounded or filled in, it fails the export: that is data nobody can replay
# honestly. A run with no head branch is left out and counted, because its
# workflow reference cannot be spelled; a run on a tag is spelled as a branch
# ref, which is a spelling the replay carries and never checks.
#
#   scripts/export-actions-trace.sh OWNER/REPO --since 2026-03-01 --prefix billet- > trace.jsonl
#   scripts/export-actions-trace.sh OWNER/REPO --since 2026-03-01 --label billet-2vcpu > trace.jsonl
set -euo pipefail

usage() {
	printf 'usage: %s OWNER/REPO --since YYYY-MM-DD (--label LABEL | --prefix PREFIX)\n' "$0" >&2
	exit 2
}

repo=""
since=""
label=""
prefix=""

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
	--prefix)
		[ $# -ge 2 ] || usage
		prefix=$2
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

if [ -n "$label" ] && [ -n "$prefix" ]; then
	printf -- '--label and --prefix are two rules for one question; give one\n' >&2
	exit 2
fi

if [ -z "$label" ] && [ -z "$prefix" ]; then
	printf 'say which label names the tier: --label LABEL or --prefix PREFIX\n' >&2
	exit 2
fi

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

jq -r '.workflow_runs[] | [.id, .path, (.head_branch // "")] | @tsv' "$work/runs.json" >"$work/runs.tsv"

runs=0
jobs=0
skipped=0
unnamed=0
: >"$work/trace.jsonl"

while IFS=$'\t' read -r run_id path branch; do
	[ -n "$run_id" ] || continue

	if [ -z "$branch" ] || [ -z "$path" ]; then
		unnamed=$((unnamed + 1))
		continue
	fi

	runs=$((runs + 1))
	workflow="$repo/$path@refs/heads/$branch"

	gh api --paginate "repos/$repo/actions/runs/$run_id/jobs?per_page=100" >"$work/jobs.json"

	# Only a job that ran to completion has a duration to replay, and only one
	# whose tier the rule names once. `fromdate` reads GitHub's RFC 3339 stamps;
	# the difference is whole seconds, spelled as a Go duration.
	jq -r --arg owner "$owner" --arg name "$name" --arg workflow "$workflow" \
		--argjson run_id "$run_id" --arg label "$label" --arg prefix "$prefix" '
		.jobs[]
		| select(.status == "completed" and .started_at != null and .completed_at != null)
		| . as $job
		| (if $label != ""
		   then (if (.labels | index($label)) != null then [$label] else [] end)
		   else [.labels[] | select(startswith($prefix))]
		   end) as $tiers
		| if ($tiers | length) == 1 then
			{
				at: .created_at,
				tier: $tiers[0],
				owner: $owner,
				repository: $name,
				workflow: $workflow,
				run_id: $run_id,
				duration: ((((.completed_at | fromdate) - (.started_at | fromdate))
					| if . < 0 then error("job \($job.id) completed before it started")
					  elif . == 0 then 1
					  else . end
					| tostring) + "s"),
				result: (if .conclusion == "success" then "succeeded"
					 elif (.conclusion // "") == "" then error("job \($job.id) completed with no conclusion")
					 else .conclusion end)
			} | @json
		  else
			"skipped"
		  end' "$work/jobs.json" >"$work/jobs.out"

	while IFS= read -r line; do
		if [ "$line" = "skipped" ]; then
			skipped=$((skipped + 1))
		else
			jobs=$((jobs + 1))
			printf '%s\n' "$line" >>"$work/trace.jsonl"
		fi
	done <"$work/jobs.out"
done <"$work/runs.tsv"

printf 'exported %d jobs from %d completed runs of %s since %s; left out %d completed jobs the label rule did not name once and %d runs with no branch or workflow path\n' \
	"$jobs" "$runs" "$repo" "$since" "$skipped" "$unnamed" >&2

if [ "$jobs" -eq 0 ]; then
	printf 'no completed jobs matched; nothing to replay\n' >&2
	exit 1
fi

cat "$work/trace.jsonl"
