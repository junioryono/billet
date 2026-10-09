#!/bin/bash
# Log the package's RAPL energy beside the BMC's power reading, once a second.
#
#   sudo scripts/power-log.sh --out power.csv [--seconds N] [--interval S]
#        [--ipmitool PATH|none] [--turbostat PATH|none] [--require-bmc]
#        [--powercap DIR] [--proc DIR] [--cgroup DIR]
#
# WHAT IT WRITES. A CSV with one row per sample:
#
#   epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances
#
# epoch_s is the wall clock (whole seconds, for matching a row to a job's
# times); uptime_s is /proc/uptime, the clock every interval is measured on,
# because the wall clock can step. rapl_uj is the package zone's raw counter
# and rapl_delta_uj the energy since the previous row, with the wrap at
# max_energy_range_uj undone. bmc_watts is `ipmitool dcmi power reading`'s
# instantaneous reading, through /dev/ipmi0 (the ipmi_si and ipmi_devintf
# modules), and turbostat_pkg_watts turbostat's latest PkgWatt when it runs.
# instances is every billet microVM with a live cgroup, `;`-separated, which is
# how `knownanswer energy` knows which jobs a window must account for, or `?`
# when the cgroup tree could not be read. The clock, the counter, the microVMs
# and turbostat are read together, before the BMC is asked.
#
# AN EMPTY CELL IS A READING THAT COULD NOT BE TAKEN, never a zero. A failed
# RAPL read, the first row, and a gap long enough for the counter to have
# wrapped unseen (max_energy_range_uj at 1 kW, billet's own bound) all leave
# rapl_delta_uj empty; a BMC that did not answer, answered nonsense, or reports
# its power reading deactivated leaves bmc_watts empty; a sample turbostat
# printed nothing new for leaves turbostat_pkg_watts empty.
#
# It stops after --seconds samples, or on SIGINT or SIGTERM, and then prints
# scripts/power-summary.sh's comparison of the whole log.
set -euo pipefail

die() {
	printf 'power-log: %s\n' "$*" >&2
	exit 2
}

out=
samples=0
interval=1
ipmitool=ipmitool
turbostat=turbostat
require_bmc=0
powercap=/sys/class/powercap/intel-rapl:0
proc=/proc
cgroup=/sys/fs/cgroup
while [ "$#" -gt 0 ]; do
	case "$1" in
	--out) out=${2:?--out needs a path}; shift 2 ;;
	--seconds) samples=${2:?--seconds needs a count}; shift 2 ;;
	--interval) interval=${2:?--interval needs seconds}; shift 2 ;;
	--ipmitool) ipmitool=${2:?--ipmitool needs a path or none}; shift 2 ;;
	--turbostat) turbostat=${2:?--turbostat needs a path or none}; shift 2 ;;
	--require-bmc) require_bmc=1; shift ;;
	--powercap) powercap=${2:?--powercap needs a directory}; shift 2 ;;
	--proc) proc=${2:?--proc needs a directory}; shift 2 ;;
	--cgroup) cgroup=${2:?--cgroup needs a directory}; shift 2 ;;
	*) die "unknown argument '$1'" ;;
	esac
done
[ -n "$out" ] || die "--out is required"
case "$samples" in '' | *[!0-9]*) die "--seconds must be a whole number, not '$samples'" ;; esac
case "$interval" in '' | 0 | *[!0-9]*) die "--interval must be a positive whole number of seconds" ;; esac
# A LOG IS NEVER APPENDED TO: rows from an earlier session would read as one
# window with an hour missing from its middle.
[ ! -e "$out" ] || die "$out exists; a log is never appended to"

# THE ZONE MUST BE READABLE NOW, and its range known, or no delta below can be
# trusted. Both are root-only on the reference host.
max_range=
if ! IFS= read -r max_range <"$powercap/max_energy_range_uj" 2>/dev/null; then
	die "cannot read $powercap/max_energy_range_uj (run as root)"
fi
case "$max_range" in '' | 0 | *[!0-9]*) die "max_energy_range_uj is '$max_range'" ;; esac
probe=
if ! IFS= read -r probe <"$powercap/energy_uj" 2>/dev/null; then
	die "cannot read $powercap/energy_uj (run as root)"
fi
case "$probe" in '' | *[!0-9]*) die "energy_uj is '$probe'" ;; esac
# A WRAP IS SEEN ONLY IF THE COUNTER CANNOT GO ROUND BETWEEN TWO READINGS: at
# 1 kW, max_energy_range_uj lasts max/1e9 seconds, which is max/1e7 centiseconds.
wrap_cs=$((max_range / 10000000))
[ -d "$cgroup" ] || die "$cgroup is not a directory; --cgroup names the cgroup2 mount"

bmc=1
if [ "$ipmitool" = none ]; then
	bmc=0
elif ! command -v "$ipmitool" >/dev/null 2>&1; then
	bmc=0
	printf 'power-log: %s not found; bmc_watts stays empty\n' "$ipmitool" >&2
fi
if [ "$require_bmc" -eq 1 ] && [ "$bmc" -eq 0 ]; then
	die "--require-bmc and no ipmitool to ask"
fi
command -v timeout >/dev/null 2>&1 || die "timeout(1) is required to bound ipmitool"

work=$(mktemp -d)
ts_pid=
cleanup() {
	if [ -n "$ts_pid" ]; then
		kill "$ts_pid" 2>/dev/null || true
		wait "$ts_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

if [ "$turbostat" != none ] && command -v "$turbostat" >/dev/null 2>&1; then
	"$turbostat" --quiet --Summary --show PkgWatt --interval "$interval" >"$work/turbostat" 2>"$work/turbostat.err" &
	ts_pid=$!
elif [ "$turbostat" != none ]; then
	printf 'power-log: %s not found; turbostat_pkg_watts stays empty\n' "$turbostat" >&2
fi

stop=0
trap 'stop=1' INT TERM

# read_uptime_cs sets up_cs to /proc/uptime in centiseconds, or empty.
read_uptime_cs() {
	up_cs=
	local up rest
	if IFS=' ' read -r up rest <"$proc/uptime" 2>/dev/null; then
		case "$up" in
		*[!0-9.]* | .* | *.) ;;
		*.??) up_cs=${up%.*}${up#*.} ;;
		esac
	fi
	# A LEADING ZERO WOULD READ AS OCTAL in the arithmetic below.
	if [ -n "$up_cs" ]; then
		up_cs=$((10#$up_cs))
	fi
}

# read_bmc sets bmc_w to the instantaneous reading in whole watts, or empty.
read_bmc() {
	bmc_w=
	[ "$bmc" -eq 1 ] || return 0
	local answer line watts= active=0
	# BOUNDED, with -k: an in-band BMC that stops answering would otherwise hold
	# every later sample, and timeout without -k is no bound on a command that
	# ignores SIGTERM.
	if ! answer=$(timeout -k 1 3 "$ipmitool" dcmi power reading 2>/dev/null); then
		return 0
	fi
	while IFS= read -r line; do
		if [[ "$line" =~ ^[[:space:]]*Instantaneous\ power\ reading:[[:space:]]*([0-9]+)[[:space:]]+Watts ]]; then
			watts=${BASH_REMATCH[1]}
		elif [[ "$line" =~ ^[[:space:]]*Power\ reading\ state\ is:[[:space:]]*activated[[:space:]]*$ ]]; then
			active=1
		fi
	done <<<"$answer"
	if [ "$active" -eq 1 ] && [ -n "$watts" ]; then
		bmc_w=$watts
	fi
}

# read_turbostat sets ts_w to the PkgWatt turbostat printed since the last
# sample, or empty. A REPEATED READING IS NOT A NEW ONE: a turbostat that exited
# or stopped printing leaves its last line in the file, and counting it every
# second would pair one old number with every later RAPL interval.
ts_lines=0
read_turbostat() {
	ts_w=
	[ -n "$ts_pid" ] || return 0
	kill -0 "$ts_pid" 2>/dev/null || return 0
	local seen lines last
	# ONE READ GIVES THE COUNT AND THE LINE TOGETHER: a count and a tail taken
	# separately can straddle a line turbostat appended between them, and the
	# next sample would read that line again as new.
	seen=$(awk '{ l = $0 } END { print NR " " l }' "$work/turbostat" 2>/dev/null) || return 0
	lines=${seen%% *}
	last=${seen#* }
	case "$lines" in '' | *[!0-9]*) return 0 ;; esac
	if [ "$lines" -le "$ts_lines" ]; then
		return 0
	fi
	ts_lines=$lines
	case "$last" in
	'' | *[!0-9.]* | .* | *. | *.*.*) ;;
	*) ts_w=$last ;;
	esac
}

# read_instances sets vms to the billet microVMs whose cgroup holds a process,
# or to `?` when the tree could not be read: an inventory that could not be
# taken is not an empty one, and an empty one is what makes a row quiet.
read_instances() {
	vms=
	local parent d first
	if [ ! -d "$cgroup" ] || [ ! -r "$cgroup" ] || [ ! -x "$cgroup" ]; then
		vms='?'
		return 0
	fi
	for parent in "$cgroup"/firecracker*; do
		[ -d "$parent" ] || continue
		if [ ! -r "$parent" ] || [ ! -x "$parent" ]; then
			vms='?'
			return 0
		fi
		for d in "$parent"/billet-*; do
			[ -d "$d" ] || continue
			# cgroupfs files report a size of zero, so `-s` says nothing: read a
			# line. `read` answers 1 for an empty file and for one it could not
			# open alike, so the open is proved by the group having run at all.
			first=
			opened=0
			{
				opened=1
				IFS= read -r first || true
			} 2>/dev/null <"$d/cgroup.procs" || true
			if [ "$opened" -eq 0 ]; then
				# A CGROUP REMOVED SINCE THE GLOB is a microVM that is gone, not
				# one that could not be read.
				[ -d "$d" ] || continue
				vms='?'
				return 0
			fi
			[ -n "$first" ] || continue
			vms=${vms:+$vms;}${d##*/}
		done
	done
}

exec 3>"$out"
printf 'epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances\n' >&3

prev_uj=
prev_cs=
n=0
while [ "$stop" -eq 0 ]; do
	# THE CLOCK, THE COUNTER, THE INVENTORY AND TURBOSTAT ARE READ TOGETHER,
	# before the BMC: a BMC call can take seconds, and a microVM that exited
	# during it would otherwise be missing from a row whose energy it drew.
	read_uptime_cs
	cur_uj=
	if IFS= read -r cur_uj <"$powercap/energy_uj" 2>/dev/null; then
		case "$cur_uj" in '' | *[!0-9]*) cur_uj= ;; esac
	fi
	read_instances
	read_turbostat
	epoch=$(date +%s)
	read_bmc

	delta=
	if [ -n "$cur_uj" ] && [ -n "$prev_uj" ] && [ -n "$up_cs" ] && [ -n "$prev_cs" ] &&
		[ "$((up_cs - prev_cs))" -gt 0 ] && [ "$((up_cs - prev_cs))" -lt "$wrap_cs" ]; then
		if [ "$cur_uj" -ge "$prev_uj" ]; then
			delta=$((cur_uj - prev_uj))
		else
			delta=$((max_range - prev_uj + cur_uj))
		fi
	fi
	uptime_text=
	if [ -n "$up_cs" ]; then
		uptime_text=$((up_cs / 100)).$(printf '%02d' $((up_cs % 100)))
	fi
	printf '%s,%s,%s,%s,%s,%s,%s\n' "$epoch" "$uptime_text" "$cur_uj" "$delta" "$bmc_w" "$ts_w" "$vms" >&3

	prev_uj=$cur_uj
	prev_cs=$up_cs
	n=$((n + 1))
	if [ "$samples" -gt 0 ] && [ "$n" -ge "$samples" ]; then
		break
	fi
	sleep "$interval" || true
done
exec 3>&-

summary=(bash "$(dirname "$0")/power-summary.sh")
if [ "$require_bmc" -eq 1 ]; then
	summary+=(--require-bmc)
fi
"${summary[@]}" "$out"
