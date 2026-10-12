#!/bin/bash
# Summarize a scripts/power-log.sh CSV: RAPL's package power against the BMC's
# whole-server reading, against a wall meter's when the log has one, and
# against turbostat's PkgWatt.
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
# A LOG WITH A meter_watts COLUMN (power-log.sh --meter) gets the same fit for
# the wall meter, which says how many rows it used and how many it skipped for
# an empty meter cell or no RAPL interval; and the meter's own energy over the
# window: from its cumulative meter_wh counter, last reading less first, when
# it has two readings an interval apart and never goes backwards (a meter that
# restarted resets it), and otherwise each row's watts over the interval ending
# at it, summed over the intervals that have a reading. It says which, and why
# the counter was not used.
#
# Exit 0 with a summary, 3 when --require-bmc was given and no row pairs the
# BMC with RAPL, or the log has a meter column and no row pairs the meter with
# RAPL (could not tell), 1 for a log it cannot read, 2 for usage.
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
# pair prints how a reading relates to RAPL over the n rows that hold both.
# With strict, a ratio over no RAPL power is n/a; the BMC keeps the 0.000 it
# has always printed there.
function pair(label, n, sx, sy, sxx, syy, sxy, strict,    mx, my, den, deny, a, b, r2) {
	mx = sx / n; my = sy / n
	printf "%s: %d paired readings, mean %.1f W against RAPL %.1f W over the same rows\n", label, n, my, mx
	if (mx > 0 || !strict) printf "%s/RAPL ratio %.3f, offset %.1f W\n", label, (mx > 0 ? my / mx : 0), my - mx
	else printf "%s/RAPL ratio n/a (RAPL drew nothing), offset %.1f W\n", label, my - mx
	den = n * sxx - sx * sx
	deny = n * syy - sy * sy
	if (n >= 3 && den > 0 && deny > 0) {
		b = (n * sxy - sx * sy) / den
		a = (sy - b * sx) / n
		r2 = (n * sxy - sx * sy) ^ 2 / (den * deny)
		printf "fit: %s = %.1f W + %.3f x RAPL, r^2 %.3f\n", label, a, b, r2
	} else {
		printf "fit: n/a (fewer than three readings, or no variation)\n"
	}
}
# meter takes the wall reading on this row, w being its RAPL power or empty.
function meter(w) {
	if (dt != "" && dt > 0) {
		mall++; mallsec += dt
		if (num($cm)) { mint++; msec += dt; menergy += $cm * dt }
	}
	if (w == "") {
		if (rows > 1) mnorapl++
	} else if (!num($cm)) {
		mempty++
	} else {
		nm++; msx += w; msy += $cm; msxx += w * w; msyy += $cm * $cm; msxy += w * $cm
	}
	if (!cwh || !num($cwh)) return
	# A COUNTER THAT WENT BACKWARDS WAS RESET, and its difference across the
	# reset is no energy at all: every reading is compared, and only a reading
	# on a row with an uptime can be an end of the span.
	if (whseen && $cwh + 0 < whprev + 0 && reset == "") reset = $ce
	whseen = 1; whprev = $cwh
	if (!num(up)) return
	if (whn == 0) { whfirst = $cwh; whfirstup = up; whfirstep = $ce }
	whlast = $cwh; whlastup = up; whlastep = $ce; whn++
}
# wall prints the meter against RAPL, what its fit used, and its own energy.
function wall(    why) {
	if (nm == 0) {
		printf "meter: no reading paired with RAPL (could not tell)\n"
	} else {
		pair("meter", nm, msx, msy, msxx, msyy, msxy, 1)
	}
	printf "meter fit: %d rows used, %d skipped for an empty meter cell, %d for no RAPL interval\n", nm, mempty, mnorapl
	why = ""
	if (!cwh || whn == 0) why = "it gave no cumulative counter"
	else if (reset != "") why = "its counter went backwards at epoch " reset ", a reset"
	else if (whlastup <= whfirstup) why = "its counter has no two readings an interval apart"
	if (why == "") {
		printf "meter energy: %.1f kJ (%.3f Wh) over %.1f s, from its cumulative counter (epoch %s to %s)\n", (whlast - whfirst) * 3.6, whlast - whfirst, whlastup - whfirstup, whfirstep, whlastep
	} else if (mint == 0) {
		printf "meter energy: could not tell; no interval has a meter reading, and %s\n", why
	} else {
		printf "meter energy: %.1f kJ over %.1f s of the %.1f s of intervals in the window, integrated from its watts (%d of %d intervals had a reading); %s\n", menergy / 1000, msec, mallsec, mint, mall, why
	}
}
NR == 1 {
	for (i = 1; i <= NF; i++) col[$i] = i
	ce = need("epoch_s"); cu = need("uptime_s"); cd = need("rapl_delta_uj")
	cb = need("bmc_watts"); ct = need("turbostat_pkg_watts")
	cm = ("meter_watts" in col) ? col["meter_watts"] : 0
	cwh = ("meter_wh" in col) ? col["meter_wh"] : 0
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
	if (cm) meter(w)
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
		if (cm) wall()
		exit 3
	}
	printf "RAPL package: %.1f kJ over %.1f s, mean %.1f W\n", energy / 1e9, seconds, energy / 1e6 / seconds
	if (nb == 0) {
		printf "BMC: no reading paired with RAPL (could not tell)\n"
	} else {
		pair("BMC", nb, sx, sy, sxx, syy, sxy, 0)
	}
	if (cm) wall()
	if (nt == 0) {
		printf "turbostat: no reading paired with RAPL\n"
	} else {
		printf "turbostat: %d paired readings, mean PkgWatt %.1f W against RAPL %.1f W, ratio %.3f\n", nt, tt / nt, tr / nt, (tr > 0 ? tt / tr : 0)
	}
	if (require_bmc && nb == 0) exit 3
	if (cm && nm == 0) exit 3
}
' "$log" || status=$?
exit "$status"
