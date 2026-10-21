#!/usr/bin/env bash
# ci-usage.sh: what a CI job used of the runner it holds.
#
#   ci-usage.sh start DIR          sample every CI_USAGE_INTERVAL seconds (5) into DIR
#   ci-usage.sh sample DIR         take one sample now
#   ci-usage.sh report DIR LABEL   stop the sampler, take a last sample, and write
#                                  what the job used to DIR/usage.json and, under
#                                  CI, a row to $GITHUB_STEP_SUMMARY
#
# A RUNNER IS A MICROVM THAT HOLDS ITS WHOLE RESERVATION for as long as the job
# runs, used or not, and the fleet is short of memory before it is short of CPU
# (500 of 540 GiB reserved against 126 of 196 vCPU, 2026-10-16). So a job's size
# is chosen from what it used: the busy cores over each interval, from the
# guest's /proc/stat, and the memory it held, MemTotal less MemAvailable from
# /proc/meminfo, which leaves out the page cache the kernel would give back.
# The peak is over one interval, so a burst shorter than it is averaged in.
#
# A MEASUREMENT, NEVER A GATE. report exits 0 whatever it finds, and says "no
# measurement" with the reason when it has fewer than two samples or cannot
# read them: a sampler that failed must not fail the job it was watching, and
# a missing number must read as missing rather than as zero.
#
# CI_USAGE_PROC names the /proc to read (a test points it at a fake one).
set -euo pipefail

proc=${CI_USAGE_PROC:-/proc}

usage() {
	printf 'usage: %s start DIR | sample DIR | report DIR LABEL\n' "${0##*/}" >&2
	exit 2
}

# sample appends "<epoch> <cpus> <busy> <total> <mem_total_kib> <mem_available_kib>":
# jiffies across all CPUs from the aggregate cpu line (busy is everything but
# idle and iowait), and the CPU count from the per-CPU lines. It fails, writing
# nothing, when either file lacks what it needs.
sample() {
	local dir=$1 line
	line=$(awk -v now="$(date +%s)" '
		FILENAME ~ /stat$/ && $1 == "cpu" {
			# user nice system idle iowait irq softirq steal; guest and guest_nice
			# after them are already counted in user and nice.
			for (i = 2; i <= 9 && i <= NF; i++) total += $i
			busy = total - $5 - $6
		}
		FILENAME ~ /stat$/ && $1 ~ /^cpu[0-9]+$/ { cpus++ }
		FILENAME ~ /meminfo$/ && $1 == "MemTotal:" { memtotal = $2 }
		FILENAME ~ /meminfo$/ && $1 == "MemAvailable:" { memavail = $2 }
		END {
			if (cpus == 0 || total == 0 || memtotal == "" || memavail == "") exit 1
			printf "%s %d %d %d %d %d\n", now, cpus, busy, total, memtotal, memavail
		}' "$proc/stat" "$proc/meminfo")
	printf '%s\n' "$line" >>"$dir/samples"
}

start() {
	local dir=$1 interval=${CI_USAGE_INTERVAL:-5}
	mkdir -p "$dir"
	: >"$dir/samples"
	sample "$dir" 2>/dev/null || true
	# DETACHED, AND ITS FAILURES ITS OWN: a sample that cannot be read is skipped,
	# never the end of the loop, and nothing here holds the step that started it.
	(
		trap 'exit 0' TERM
		while :; do
			sleep "$interval"
			sample "$dir" 2>/dev/null || true
		done
	) </dev/null >/dev/null 2>&1 &
	printf '%s\n' "$!" >"$dir/pid"
}

report() {
	local dir=$1 label=$2 pid result
	if [ -s "$dir/pid" ]; then
		pid=$(cat "$dir/pid")
		kill "$pid" 2>/dev/null || true
		rm -f "$dir/pid"
	fi
	sample "$dir" 2>/dev/null || true

	if ! result=$(awk -v label="$label" '
		NF != 6 { bad = 1; exit }
		{
			n++
			if (n > 1 && $4 > total && $3 >= busy) {
				dbusy = $3 - busy; dtotal = $4 - total
				sumbusy += dbusy; sumtotal += dtotal
				cores = dbusy / dtotal * $2
				if (cores > peak) peak = cores
			}
			if (n == 1) first = $1
			last = $1; cpus = $2; busy = $3; total = $4
			used = ($5 - $6) / 1048576
			if (used > mempeak) mempeak = used
			memtotal = $5 / 1048576
		}
		END {
			if (bad || n < 2 || sumtotal == 0) exit 1
			printf "{\"label\":\"%s\",\"vcpu\":%d,\"mem_total_gib\":%.1f,\"cpu_mean_cores\":%.2f,\"cpu_peak_cores\":%.2f,\"mem_peak_gib\":%.2f,\"samples\":%d,\"seconds\":%d}\n",
				label, cpus, memtotal, sumbusy / sumtotal * cpus, peak, mempeak, n, last - first
		}' "$dir/samples" 2>/dev/null); then
		printf 'ci-usage: no measurement for %s: fewer than two readable samples in %s\n' "$label" "$dir/samples"
		if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
			printf '| %s | no measurement | | | | |\n' "$label" >>"$GITHUB_STEP_SUMMARY"
		fi
		return 0
	fi

	printf '%s\n' "$result" >"$dir/usage.json"
	printf 'ci-usage: %s\n' "$result"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		awk -v r="$result" 'BEGIN {
			n = split(r, kv, ",")
			for (i = 1; i <= n; i++) { split(kv[i], p, ":"); gsub(/[{}"]/, "", p[1]); gsub(/[{}"]/, "", p[2]); v[p[1]] = p[2] }
			print "| job | runner | mean CPU | peak CPU | peak memory | sampled |"
			print "|---|---|---|---|---|---|"
			printf "| %s | %s vCPU, %s GiB | %s cores | %s cores | %s GiB | %s s, %s samples |\n",
				v["label"], v["vcpu"], v["mem_total_gib"], v["cpu_mean_cores"], v["cpu_peak_cores"],
				v["mem_peak_gib"], v["seconds"], v["samples"]
		}' >>"$GITHUB_STEP_SUMMARY"
	fi
}

[ "$#" -ge 2 ] || usage
case "$1" in
start) start "$2" ;;
sample) sample "$2" ;;
report)
	[ "$#" -eq 3 ] || usage
	report "$2" "$3"
	;;
*) usage ;;
esac
