#!/bin/bash
# Run one known-answer job inside a billet guest and record what it expects.
#
#   known-answer-job.sh <baseline|idle|cpu|memory|network|disk> <out-dir>
#
# WHAT IT IS FOR. `billet jobs show --json` reports what the host measured a
# job do. This script makes a job whose answer is known in advance, so the host's
# numbers can be compared with it: N stress-ng workers for T seconds are N*T
# CPU-seconds, a vm worker that keeps X bytes mapped has a peak of at least X, a
# download of S bytes reaches the guest as at least S, fio writing X bytes with
# O_DIRECT reaches the host's disk as about X, and sleeping costs nothing.
# scripts/knownanswer compares; this script only runs the load and writes
# <out-dir>/expectation.json, which the reusable workflow uploads.
#
# EVERY KIND DOES THE SAME PREPARATION, the baseline included. The host measures
# the whole life of the microVM: its boot, the runner, the checkout, the package
# install and only then the load. The checker subtracts the same run's idle job
# from each loaded job, and the idle job from the baseline, so whatever every job
# does beside its load cancels only if every job does it identically.
#
# THE LEASE IS READ FROM THE RUNNER'S NAME. billet names every runner
# `billet-<lease>` (provider.InstanceName), and that is the one key that joins
# this job to the ledger without trusting anything else the job says. A runner
# not named that way is not billet's, and the job refuses to run.
#
# Inputs, from the environment: KA_SECONDS (idle, cpu and memory: how long the
# load runs), KA_CPU_WORKERS (cpu), KA_MEMORY_BYTES (memory), KA_NETWORK_URL and
# optionally KA_NETWORK_BYTES (network: a body of exactly that size, or the job
# fails), KA_DISK_BYTES (disk), KA_GITHUB_JOB_ID (the workflow's
# `job.check_run_id`; may be empty), and the runner's own GITHUB_RUN_ID,
# GITHUB_RUN_ATTEMPT, GITHUB_REPOSITORY, RUNNER_NAME and RUNNER_TEMP.
set -euo pipefail

die() {
	printf 'known-answer: %s\n' "$*" >&2
	exit 1
}

# positive NAME VALUE refuses anything but a positive decimal integer, so every
# number the JSON below carries is one the shell wrote and never a string a
# workflow input chose.
positive() {
	case "$2" in
	'' | 0* | *[!0-9]*) die "$1 must be a positive integer, not '$2'" ;;
	esac
	# FIFTEEN DIGITS, a petabyte, so the one product taken below (workers, which
	# nproc bounds, times seconds, which 21600 bounds) stays far inside 2^63.
	if [ "${#2}" -gt 15 ]; then
		die "$1 is too large: '$2'"
	fi
}

[ "$#" -eq 2 ] || die "usage: known-answer-job.sh <baseline|idle|cpu|memory|network|disk> <out-dir>"
kind=$1
out=$2
case "$kind" in
baseline | idle | cpu | memory | network | disk) ;;
*) die "unknown kind '$kind'" ;;
esac

runner=${RUNNER_NAME:-}
lease=${runner#billet-}
if [ "$lease" = "$runner" ] || [ -z "$lease" ]; then
	die "runner '$runner' is not one billet launched; a billet runner is named billet-<lease>"
fi
case "$lease" in
*[!A-Za-z0-9._-]*) die "runner '$runner' names no lease billet could have chosen" ;;
esac

repository=${GITHUB_REPOSITORY:-}
case "$repository" in
'' | *[!A-Za-z0-9._/-]*) die "GITHUB_REPOSITORY '$repository' is not owner/name" ;;
esac
positive GITHUB_RUN_ID "${GITHUB_RUN_ID:-}"
positive GITHUB_RUN_ATTEMPT "${GITHUB_RUN_ATTEMPT:-}"
job_id=${KA_GITHUB_JOB_ID:-}
case "$job_id" in
*[!0-9]*) die "KA_GITHUB_JOB_ID '$job_id' is not a check run id" ;;
esac

seconds=0
expected_key=
expected_value=0
case "$kind" in
idle | cpu | memory)
	positive KA_SECONDS "${KA_SECONDS:-}"
	# SIX HOURS IS THE LONGEST A GITHUB JOB RUNS.
	if [ "$KA_SECONDS" -gt 21600 ]; then
		die "KA_SECONDS $KA_SECONDS is longer than a job can run"
	fi
	seconds=$KA_SECONDS
	;;
esac
case "$kind" in
cpu)
	positive KA_CPU_WORKERS "${KA_CPU_WORKERS:-}"
	vcpus=$(nproc)
	positive nproc "$vcpus"
	# MORE WORKERS THAN vCPUs DO NOT RUN N*T SECONDS: they share the vCPUs, the
	# guest is busy for vCPUs*T, and the expectation would be wrong rather than
	# the measurement.
	if [ "$KA_CPU_WORKERS" -gt "$vcpus" ]; then
		die "$KA_CPU_WORKERS cpu workers cannot each run on a guest with $vcpus vCPUs"
	fi
	expected_key=cpu_seconds
	expected_value=$((KA_CPU_WORKERS * seconds))
	;;
memory)
	positive KA_MEMORY_BYTES "${KA_MEMORY_BYTES:-}"
	expected_key=memory_peak_bytes
	expected_value=$KA_MEMORY_BYTES
	;;
network)
	url=${KA_NETWORK_URL:-}
	case "$url" in
	https://* | http://*) ;;
	*) die "KA_NETWORK_URL must be an http or https URL, not '$url'" ;;
	esac
	if [ -n "${KA_NETWORK_BYTES:-}" ]; then
		positive KA_NETWORK_BYTES "$KA_NETWORK_BYTES"
	fi
	expected_key=net_rx_bytes
	;;
disk)
	positive KA_DISK_BYTES "${KA_DISK_BYTES:-}"
	expected_key=disk_write_bytes
	expected_value=$KA_DISK_BYTES
	;;
idle)
	expected_key=cpu_seconds
	expected_value=0
	;;
esac

mkdir -p "$out"
work=${RUNNER_TEMP:-$out}/known-answer-work
mkdir -p "$work"

# THE SAME PREPARATION FOR EVERY KIND (see the header).
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq stress-ng fio curl

load_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
case "$kind" in
baseline) ;;
idle) sleep "$seconds" ;;
cpu)
	stress-ng --cpu "$KA_CPU_WORKERS" --cpu-method matrixprod --timeout "${seconds}s" --metrics-brief
	;;
memory)
	# --vm-keep MAPS ONCE AND KEEPS REWRITING, so every byte is resident for the
	# whole run rather than mapped and unmapped each pass.
	stress-ng --vm 1 --vm-bytes "$KA_MEMORY_BYTES" --vm-keep --timeout "${seconds}s" --metrics-brief
	;;
network)
	# TO /dev/null, so the download writes nothing to the guest's disk, and with
	# no retry, since a retried transfer reaches the guest more than once.
	got=$(curl --fail --silent --show-error --location --output /dev/null \
		--write-out '%{size_download}' "$url")
	positive "the downloaded size" "$got"
	if [ -n "${KA_NETWORK_BYTES:-}" ] && [ "$got" != "$KA_NETWORK_BYTES" ]; then
		die "the body was $got bytes and $KA_NETWORK_BYTES were expected; the known answer is not known"
	fi
	expected_value=$got
	;;
disk)
	fio --name=known-answer --filename="$work/fio.dat" --rw=write --bs=1M \
		--size="$KA_DISK_BYTES" --direct=1 --ioengine=libaio --iodepth=16 --end_fsync=1
	rm -f "$work/fio.dat"
	;;
esac
load_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)

expected='{}'
if [ -n "$expected_key" ]; then
	expected="{\"$expected_key\":$expected_value}"
fi

printf 'known-answer: kind=%s lease=%s run=%s attempt=%s github_job_id=%s\n' \
	"$kind" "$lease" "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "${job_id:-unknown}"
printf 'known-answer: expected %s\n' "$expected"

printf '{"schema":1,"kind":"%s","lease":"%s","repository":"%s","run_id":%s,"run_attempt":%s,"github_job_id":"%s","seconds":%s,"load_started_at":"%s","load_finished_at":"%s","expected":%s}\n' \
	"$kind" "$lease" "$repository" "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "$job_id" "$seconds" \
	"$load_started" "$load_finished" "$expected" >"$out/expectation.json.tmp"
mv "$out/expectation.json.tmp" "$out/expectation.json"
