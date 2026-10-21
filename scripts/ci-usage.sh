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
# is chosen from what it used: the cores it ran on over each interval, from the
# guest's /proc/stat, and the memory it held, MemTotal less MemAvailable from
# /proc/meminfo, which leaves out the page cache the kernel would give back.
# Stolen time, when the host ran something else on the vCPU, is not time the job
# ran, so it is reported beside the cores rather than in them. The peak is over
# one interval, so a burst shorter than it is averaged in.
#
# AN INTERVAL THE COUNTERS CANNOT ACCOUNT FOR IS LEFT OUT, never read as a
# number: one in which the CPU count changed, a counter went backwards (the
# kernel lets iowait decrease), or more time was busy than passed. And a sampler
# that stopped early is visible: the report gives the longest gap between two
# samples, and calls its peaks partial when that gap is over three intervals,
# because a peak over a long gap is an average.
#
# A MEASUREMENT, NEVER A GATE. Every mode exits 0 whatever it finds (CI runs
# each step with continue-on-error as well), and report says "no measurement"
# with the reason when it has no interval it can read: a sampler that failed
# must not fail the job it was watching, and a missing number must read as
# missing rather than as zero.
#
# CI_USAGE_PROC names the /proc to read (a test points it at a fake one).
set -uo pipefail

proc=${CI_USAGE_PROC:-/proc}

usage() {
	printf 'usage: %s start DIR | sample DIR | report DIR LABEL\n' "${0##*/}" >&2
	exit 2
}

# sample appends "<epoch> <cpus> <busy> <total> <steal> <mem_total_kib> <mem_available_kib>":
# jiffies across all CPUs from the aggregate cpu line (busy is user, nice,
# system, irq and softirq; total adds idle, iowait and steal), and the CPU count
# from the per-CPU lines. It fails, writing nothing, when either file lacks what
# it needs.
sample() {
	local dir=$1 line
	line=$(awk -v now="$(date +%s)" '
		FILENAME ~ /stat$/ && $1 == "cpu" {
			# user nice system idle iowait irq softirq steal; guest and guest_nice
			# after them are already counted in user and nice.
			for (i = 2; i <= 9 && i <= NF; i++) total += $i
			busy = $2 + $3 + $4 + $7 + $8
			steal = (NF >= 9) ? $9 : 0
		}
		FILENAME ~ /stat$/ && $1 ~ /^cpu[0-9]+$/ { cpus++ }
		FILENAME ~ /meminfo$/ && $1 == "MemTotal:" { memtotal = $2 }
		FILENAME ~ /meminfo$/ && $1 == "MemAvailable:" { memavail = $2 }
		END {
			if (cpus == 0 || total == 0 || memtotal == "" || memavail == "") exit 1
			printf "%s %d %d %d %d %d %d\n", now, cpus, busy, total, steal, memtotal, memavail
		}' "$proc/stat" "$proc/meminfo") || return 1
	printf '%s\n' "$line" >>"$dir/samples"
}

start() {
	local dir=$1 interval=${CI_USAGE_INTERVAL:-5}
	case "$interval" in
	'' | *[!0-9.]* | *.*.* | .* | *.) interval=5 ;;
	esac
	if ! awk -v i="$interval" 'BEGIN { exit !(i > 0) }'; then
		interval=5
	fi
	if ! mkdir -p "$dir" || ! : >"$dir/samples" || ! printf '%s\n' "$interval" >"$dir/interval"; then
		printf 'ci-usage: cannot write to %s; this job is not measured\n' "$dir" >&2
		return 0
	fi
	sample "$dir" 2>/dev/null
	# DETACHED, AND ITS FAILURES ITS OWN: a sample that cannot be read is skipped,
	# never the end of the loop, and nothing here holds the step that started it.
	(
		trap 'exit 0' TERM
		while :; do
			sleep "$interval"
			sample "$dir" 2>/dev/null
		done
	) </dev/null >/dev/null 2>&1 &
	printf '%s\n' "$!" >"$dir/pid" || printf 'ci-usage: cannot record the sampler pid in %s\n' "$dir" >&2
}

report() {
	local dir=$1 label=$2 pid result interval=5
	if [ -s "$dir/pid" ]; then
		pid=$(cat "$dir/pid")
		kill "$pid" 2>/dev/null
		rm -f "$dir/pid"
	fi
	if [ -s "$dir/interval" ]; then
		interval=$(cat "$dir/interval")
	fi
	sample "$dir" 2>/dev/null

	# Lines that are not a whole sample (a write cut short) are skipped, not fatal.
	if ! result=$(awk -v label="$label" -v interval="$interval" '
		NF != 7 { next }
		{
			n++
			if (n > 1) {
				gap = $1 - last
				if (gap > maxgap) maxgap = gap
				dbusy = $3 - busy; dtotal = $4 - total; dsteal = $5 - steal
				if ($2 == cpus && dtotal == 0 && dbusy == 0 && dsteal == 0) {
					# No time passed between the two, as when report samples within
					# the same tick: no interval, and nothing wrong with it.
				} else if ($2 == cpus && dtotal > 0 && dbusy >= 0 && dsteal >= 0 && dbusy + dsteal <= dtotal) {
					intervals++
					sumbusy += dbusy; sumsteal += dsteal; sumtotal += dtotal
					cores = dbusy / dtotal * $2
					if (cores > peak) peak = cores
				} else {
					skipped++
				}
			}
			if (n == 1) first = $1
			last = $1; cpus = $2; busy = $3; total = $4; steal = $5
			used = ($6 - $7) / 1048576
			if (used > mempeak) mempeak = used
			memtotal = $6 / 1048576
		}
		END {
			if (intervals == 0 || sumtotal == 0) exit 1
			# Stamps are whole seconds, so a gap reads up to one second long.
			partial = (maxgap > 3 * interval + 1) ? "true" : "false"
			printf "{\"label\":\"%s\",\"vcpu\":%d,\"mem_total_gib\":%.1f,\"cpu_mean_cores\":%.2f,\"cpu_peak_cores\":%.2f,\"cpu_steal_cores\":%.2f,\"mem_peak_gib\":%.2f,\"samples\":%d,\"skipped_intervals\":%d,\"max_gap_seconds\":%d,\"partial\":%s,\"seconds\":%d}\n",
				label, cpus, memtotal, sumbusy / sumtotal * cpus, peak, sumsteal / sumtotal * cpus, mempeak, n, skipped, maxgap, partial, last - first
		}' "$dir/samples" 2>/dev/null); then
		printf 'ci-usage: no measurement for %s: no interval it can read in %s\n' "$label" "$dir/samples"
		if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
			printf '| %s | no measurement | | | | |\n' "$label" >>"$GITHUB_STEP_SUMMARY" ||
				printf 'ci-usage: cannot write the step summary\n' >&2
		fi
		return 0
	fi

	printf '%s\n' "$result" >"$dir/usage.json" || printf 'ci-usage: cannot write %s/usage.json\n' "$dir" >&2
	printf 'ci-usage: %s\n' "$result"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		awk -v r="$result" 'BEGIN {
			n = split(r, kv, ",")
			for (i = 1; i <= n; i++) { split(kv[i], p, ":"); gsub(/[{}"]/, "", p[1]); gsub(/[{}"]/, "", p[2]); v[p[1]] = p[2] }
			print "| job | runner | mean CPU | peak CPU | peak memory | sampled |"
			print "|---|---|---|---|---|---|"
			printf "| %s | %s vCPU, %s GiB | %s cores (%s stolen) | %s cores%s | %s GiB | %s s, %s samples, longest gap %s s |\n",
				v["label"], v["vcpu"], v["mem_total_gib"], v["cpu_mean_cores"], v["cpu_steal_cores"], v["cpu_peak_cores"],
				(v["partial"] == "true" ? " (partial)" : ""), v["mem_peak_gib"], v["seconds"], v["samples"], v["max_gap_seconds"]
		}' >>"$GITHUB_STEP_SUMMARY" || printf 'ci-usage: cannot write the step summary\n' >&2
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
exit 0
