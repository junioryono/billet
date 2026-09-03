#!/bin/sh
# An isolated acceptance run, end to end.
#
# WHAT IT IS FOR is the procedure docs/reference/records/aws-acceptance.md used to describe by hand:
# stand a deployment up beside whatever else is on the machine, let a real GitHub
# Actions job run on it, record what happened, and destroy exactly what it made.
#
# THE TEARDOWN RUNS WHATEVER HAPPENS, and that is the whole reason this is a
# script rather than three commands in a README. An acceptance run launches
# billable compute; a run that failed halfway and left it there is the failure
# worth engineering against, so the teardown is a trap on EXIT, INT and TERM
# rather than a line at the bottom.
#
# `billet acceptance` OWNS THE DANGEROUS HALF. This script does not decide what
# belongs to the run, does not delete anything itself, and does not know how any
# of it is scoped — the command derives an isolated deployment identity and every
# destroy is scoped by it. What is here is the shell around that: where the
# workspace goes, which config to derive from, and making the teardown
# unconditional.
set -eu

usage() {
    cat >&2 <<'USAGE'
usage: scripts/acceptance.sh --config <billet.yaml> [options]

  --config PATH      the config to derive an isolated acceptance deployment FROM
  --workspace PATH   where this run lives (default: a fresh mktemp -d)
  --account ID       refuse unless the AWS credential is in this account
  --region REGION    where to ask sts:GetCallerIdentity (default: the config's)
  --jobs N           how many jobs must finish before the run stops waiting (1)
  --wait DURATION    give up waiting after this long (30m); the teardown still runs
  --keep-workspace   leave the workspace behind after a successful teardown
  --binary PATH      the billet to use (default: build it from this checkout)

It does not dispatch a workflow. Start one that names a tier label the run
prints; `billet acceptance up` reports them.
USAGE
    exit 2
}

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=${here%/scripts}

config=""
workspace=""
account=""
region=""
jobs=1
wait=30m
keep_workspace=no
billet=${BILLET_ACCEPTANCE_BINARY:-}

while [ $# -gt 0 ]; do
    case $1 in
        --config) config=${2:-}; shift 2 ;;
        --workspace) workspace=${2:-}; shift 2 ;;
        --account) account=${2:-}; shift 2 ;;
        --region) region=${2:-}; shift 2 ;;
        --jobs) jobs=${2:-}; shift 2 ;;
        --wait) wait=${2:-}; shift 2 ;;
        --binary) billet=${2:-}; shift 2 ;;
        --keep-workspace) keep_workspace=yes; shift ;;
        -h|--help) usage ;;
        *) echo "acceptance: unknown argument $1" >&2; usage ;;
    esac
done

if [ -z "$config" ]; then
    echo "acceptance: --config is required" >&2
    usage
fi

if [ -z "$billet" ]; then
    echo "Building billet from this checkout..."
    build=$(mktemp -d)
    # WHY THE BUILD DIRECTORY IS NOT THE WORKSPACE: the workspace is removed by a
    # successful teardown, and the binary is what performs it.
    trap 'rm -rf "$build"' EXIT INT TERM
    (cd "$repo" && go build -o "$build/billet" ./cmd/billet)
    billet=$build/billet
fi

if [ -z "$workspace" ]; then
    workspace=$(mktemp -d)
fi

# THE TEARDOWN IS A TRAP, AND IT REPLACES THE BUILD DIRECTORY'S. Set BEFORE `up`
# runs, so a run interrupted between minting the identity and starting anything
# still reaches a teardown that can be scoped to it — the workspace's record and
# state directory are what `down` needs, and `up` writes them before it returns.
#
# A FAILED TEARDOWN MUST NOT EXIT ZERO, and the first version of this did.
#
# `|| true` on the teardown plus `exit "$status"` meant that a run which SUCCEEDED
# and a teardown that then failed — AWS unreachable, a scale set that would not
# delete, a sweep that found a survivor — exited 0. CI would have reported a
# clean acceptance run while compute was still billing, which is the one outcome
# this script exists to make impossible.
#
# THE RUN'S OWN FAILURE STILL WINS, because "the job did not run" is what an
# operator came for and a cleanup problem is something they need in addition. So:
# a non-zero run keeps its status, and a zero run takes the teardown's.
teardown() {
    status=$?

    echo
    echo "=== teardown (run exited $status) ==="

    down_status=0

    if [ "$keep_workspace" = yes ]; then
        "$billet" acceptance down --workspace "$workspace" --keep-workspace || down_status=$?
    else
        "$billet" acceptance down --workspace "$workspace" || down_status=$?
    fi

    if [ -n "${build:-}" ]; then
        rm -rf "$build"
    fi

    if [ "$status" -eq 0 ] && [ "$down_status" -ne 0 ]; then
        echo "acceptance: the run succeeded and the TEARDOWN did not." >&2
        echo "Resources this run created may still exist. Re-run:" >&2
        echo "  $billet acceptance down --workspace $workspace" >&2

        exit "$down_status"
    fi

    exit "$status"
}

# THE ARGUMENTS ARE POSITIONAL PARAMETERS, not a string. A string would have to
# be expanded unquoted to become several arguments, and then a config path with a
# space in it silently becomes two — which for `--workspace` means a run whose
# teardown is scoped to a directory nobody named.
#
# AND EACH OPTIONAL FLAG IS AN `if`, not `[ … ] && …`. The chained form is
# exempt from `set -e` in the middle of a script and NOT as its last command
# (measured), so it is a construct whose safety depends on what follows it. This
# repository has been caught by that shape twice; an `if` has no such question.
set -- --config "$config" --workspace "$workspace"

if [ -n "$account" ]; then
    set -- "$@" --account "$account"
fi

if [ -n "$region" ]; then
    set -- "$@" --region "$region"
fi

# BEFORE THE TRAP, because `up` is what creates the thing the trap tears down —
# and a teardown against a workspace `up` never wrote refuses, loudly, on every
# failed invocation of this script.
"$billet" acceptance up "$@"

trap teardown EXIT INT TERM

"$billet" acceptance run --workspace "$workspace" --jobs "$jobs" --wait "$wait" --no-teardown

# THE EVIDENCE IS WRITTEN BEFORE THE TEARDOWN, which is why `run` was given
# --no-teardown above: it stops the services and writes the evidence, and the trap
# below performs the destroy. Reading it here means an operator sees the summary
# whether or not the teardown then succeeds.
evidence=$workspace/evidence.json
if [ -f "$evidence" ]; then
    echo
    echo "=== evidence ($evidence) ==="
    cat "$evidence"
fi
