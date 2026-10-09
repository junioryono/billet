#!/bin/bash
# Summarize a scripts/power-log.sh CSV: RAPL's package power against the BMC's
# whole-server reading, and against turbostat's PkgWatt.
#
#   scripts/power-summary.sh [--from EPOCH] [--to EPOCH] [--require-bmc] power.csv
#
# Each row's RAPL power is its rapl_delta_uj over the uptime since the row
# before, so an interval is measured on the clock that cannot step. The BMC and
# turbostat are compared on the rows where both they and RAPL have a reading,
# never against a mean taken over other rows: a BMC that answered only during
# the quiet half of a log would otherwise compare its idle with RAPL's load.
#
# The BMC measures the whole server at the wall (fans, memory, disks, NICs, the
# power supply's loss) and RAPL the CPU package alone, so they are not expected
# to agree; what is reported is how they relate. ratio is BMC over RAPL, offset
# BMC minus RAPL, both over the paired rows, and the least-squares line
# BMC = a + b x RAPL with its r^2.
#
# Exit 0 with a summary, 3 when --require-bmc was given and no row pairs the
# BMC with RAPL (could not tell), 1 for a log it cannot read, 2 for usage.
set -euo pipefail

usage() {
	printf 'usage: power-summary.sh [--from EPOCH] [--to EPOCH] [--require-bmc] power.csv\n' >&2
	exit 2
}

from=
to=
require_bmc=0
while [ "$#" -gt 1 ]; do
	case "$1" in
	--from) from=${2:-}; shift 2 ;;
	--to) to=${2:-}; shift 2 ;;
	--require-bmc) require_bmc=1; shift ;;
	*) usage ;;
	esac
done
[ "$#" -eq 1 ] || usage
log=$1
case "$from" in *[!0-9]*) usage ;; esac
case "$to" in *[!0-9]*) usage ;; esac
[ -r "$log" ] || {
	printf 'power-summary: cannot read %s\n' "$log" >&2
	exit 1
}

# THE VERDICT IS AWK'S EXIT STATUS, captured before anything reads its output.
status=0
awk -F, -v from="$from" -v to="$to" -v require_bmc="$require_bmc" '
function need(name) {
	if (!(name in col)) { printf "power-summary: the log has no %s column\n", name > "/dev/stderr"; bad = 1; exit 1 }
	return col[name]
}
function num(s) { return s ~ /^[0-9]+(\.[0-9]+)?$/ }
NR == 1 {
	for (i = 1; i <= NF; i++) col[$i] = i
	ce = need("epoch_s"); cu = need("uptime_s"); cd = need("rapl_delta_uj")
	cb = need("bmc_watts"); ct = need("turbostat_pkg_watts")
	next
}
{
	inwindow = (from == "" || $ce + 0 >= from + 0) && (to == "" || $ce + 0 <= to + 0)
	up = $cu; dt = ""
	if (num(up) && num(prevup)) dt = up - prevup
	prevup = up
	if (!inwindow) next
	rows++
	if (first == "") first = $ce
	last = $ce
	w = ""
	if ($cd != "" && num($cd) && dt != "" && dt > 0) {
		w = ($cd / 1e6) / dt
		energy += $cd; seconds += dt; intervals++
	} else if (rows > 1) gaps++
	if (w == "") next
	if (num($cb)) {
		nb++; sx += w; sy += $cb; sxx += w * w; syy += $cb * $cb; sxy += w * $cb
	}
	if (num($ct)) { nt++; tr += w; tt += $ct }
}
END {
	if (bad) exit 1
	printf "rows %d (epoch %s to %s), RAPL intervals %d, gaps %d\n", rows, first, last, intervals, gaps
	if (intervals == 0) {
		printf "RAPL: no interval could be measured (could not tell)\n"
		exit 3
	}
	printf "RAPL package: %.1f kJ over %.1f s, mean %.1f W\n", energy / 1e9, seconds, energy / 1e6 / seconds
	if (nb == 0) {
		printf "BMC: no reading paired with RAPL (could not tell)\n"
	} else {
		mx = sx / nb; my = sy / nb
		printf "BMC: %d paired readings, mean %.1f W against RAPL %.1f W over the same rows\n", nb, my, mx
		printf "BMC/RAPL ratio %.3f, offset %.1f W\n", (mx > 0 ? my / mx : 0), my - mx
		den = nb * sxx - sx * sx
		deny = nb * syy - sy * sy
		if (nb >= 3 && den > 0 && deny > 0) {
			b = (nb * sxy - sx * sy) / den
			a = (sy - b * sx) / nb
			r2 = (nb * sxy - sx * sy) ^ 2 / (den * deny)
			printf "fit: BMC = %.1f W + %.3f x RAPL, r^2 %.3f\n", a, b, r2
		} else {
			printf "fit: n/a (fewer than three readings, or no variation)\n"
		}
	}
	if (nt == 0) {
		printf "turbostat: no reading paired with RAPL\n"
	} else {
		printf "turbostat: %d paired readings, mean PkgWatt %.1f W against RAPL %.1f W, ratio %.3f\n", nt, tt / nt, tr / nt, (tr > 0 ? tt / tr : 0)
	}
	if (require_bmc && nb == 0) exit 3
}
' "$log" || status=$?
exit "$status"
