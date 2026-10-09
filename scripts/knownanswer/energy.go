package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

// powerRow is one row of scripts/power-log.sh's CSV. An empty cell is a
// reading that could not be taken, and its ok flag is false.
type powerRow struct {
	epoch     int64
	uptime    float64
	uptimeOK  bool
	delta     int64
	deltaOK   bool
	instances []string
	// inventoryOK is false where the logger could not read the cgroups, which
	// is not the same as finding no microVM.
	inventoryOK bool
}

// readPowerLog reads the CSV by its header's names, so a column added later
// does not move the ones read here.
func readPowerLog(path string) ([]powerRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: read the header: %w", path, err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	for _, name := range []string{"epoch_s", "uptime_s", "rapl_delta_uj", "instances"} {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("%s: no %s column", path, name)
		}
	}
	var rows []powerRow
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var row powerRow
		if row.epoch, err = strconv.ParseInt(rec[col["epoch_s"]], 10, 64); err != nil {
			return nil, fmt.Errorf("%s line %d: epoch_s: %w", path, line, err)
		}
		if s := rec[col["uptime_s"]]; s != "" {
			// ParseFloat ACCEPTS NaN AND Inf, which compare false with everything and
			// would let an interval through every bound.
			if row.uptime, err = strconv.ParseFloat(s, 64); err != nil || math.IsNaN(row.uptime) || math.IsInf(row.uptime, 0) {
				return nil, fmt.Errorf("%s line %d: uptime_s %q is not a time", path, line, s)
			}
			row.uptimeOK = true
		}
		if s := rec[col["rapl_delta_uj"]]; s != "" {
			// BOUNDED, so no sum of a log's rows can wrap: a megajoule in one row is
			// ten thousand seconds at a kilowatt, past any interval the logger keeps.
			if row.delta, err = strconv.ParseInt(s, 10, 64); err != nil || row.delta < 0 || row.delta > maxRowDelta {
				return nil, fmt.Errorf("%s line %d: rapl_delta_uj %q is not an energy", path, line, s)
			}
			row.deltaOK = true
		}
		switch s := rec[col["instances"]]; s {
		case "?":
		case "":
			row.inventoryOK = true
		default:
			row.inventoryOK = true
			row.instances = strings.Split(s, ";")
		}
		rows = append(rows, row)
	}

	return rows, nil
}

// energyLimits are the reconciliation's stated tolerances
// (docs/operating/measurement-validation.md says why).
const (
	// attributedLow and attributedHigh bound the jobs' attributed active energy
	// over the package's measured energy above the idle baseline.
	attributedLow  = 0.85
	attributedHigh = 1.02
	// idleTolerance bounds the quiet ends' measured power around
	// idle_package_watts.
	idleTolerance = 0.05
	// minActiveShare is how much of the window's energy must be above idle for
	// the comparison to mean anything.
	minActiveShare = 0.01
)

// energyJob is one microVM the log saw.
type energyJob struct {
	lease   string
	seen    float64 // seconds between the first and last row that held it
	window  float64 // billet's own measurement window, seconds
	active  int64
	idle    int64
	problem string
}

// energyResult is the reconciliation of one window.
type energyResult struct {
	verdict               verdict
	reason                string
	rows                  int
	from, to              int64
	seconds               float64
	rapl                  int64
	idleWatts             float64
	idleBefore, idleAfter float64
	idleBeforeOK          bool
	idleAfterOK           bool
	idleVerdict           verdict
	activeMeasured        float64
	idlePool              float64
	busySeconds           float64
	// tick is the monitor's sampling interval, from the records; coarse says why
	// the log is too coarse to reproduce it; allowance is what clocks falling at
	// different moments can add to the jobs' share, and high the bound with it.
	tick      float64
	coarse    string
	allowance float64
	high      float64
	// clipped is how far this log's busy intervals fell below the baseline,
	// which it counted as no active energy; low is the lower bound with it.
	clipped float64
	low     float64
	// attributed and attributedIdle are summed in float64, which saturates
	// rather than wrapping as an int64 sum of malformed counters would.
	attributed, attributedIdle float64
	ratio                      float64
	jobs                       []energyJob
}

// reconcile compares the jobs' attributed energy with the package's energy
// over the whole log. quiet is how many rows at each end must hold no microVM:
// a job whose life crosses the window's edge has energy on both sides of it.
func reconcile(rows []powerRow, idleWatts float64, quiet int, records recordSource) energyResult {
	res := energyResult{verdict: unmeasured, idleVerdict: unmeasured, rows: len(rows), idleWatts: idleWatts}
	if len(rows) < 2*quiet+1 || len(rows) < 3 {
		res.reason = fmt.Sprintf("%d rows cannot hold %d quiet rows at each end and work between them",
			len(rows), quiet)

		return res
	}
	res.from, res.to = rows[0].epoch, rows[len(rows)-1].epoch
	for i, row := range rows {
		if (i < quiet || i >= len(rows)-quiet) && len(row.instances) > 0 {
			res.reason = fmt.Sprintf("row %d (epoch %d) holds %s inside the quiet ends; start the log "+
				"before the first job and stop it after the last", i+1, row.epoch, strings.Join(row.instances, ", "))

			return res
		}
		if i > 0 && !row.deltaOK {
			res.reason = fmt.Sprintf("row %d (epoch %d) has no RAPL delta, so the window's energy is not known",
				i+1, row.epoch)

			return res
		}
		if !row.uptimeOK {
			res.reason = fmt.Sprintf("row %d (epoch %d) has no uptime, so its interval is not known", i+1, row.epoch)

			return res
		}
		if !row.inventoryOK {
			res.reason = fmt.Sprintf("row %d (epoch %d) could not read which microVMs were running", i+1, row.epoch)

			return res
		}
		if i == 0 {
			continue
		}
		dt := row.uptime - rows[i-1].uptime
		if dt <= 0 {
			res.reason = fmt.Sprintf("row %d (epoch %d) does not come after the row before it", i+1, row.epoch)

			return res
		}
		res.rapl += row.delta
		// THE IDLE BASELINE IS TAKEN PER INTERVAL AND NEVER EXCEEDS WHAT WAS
		// DRAWN, as the node's monitor takes it: an interval below the baseline
		// contributes no active energy rather than a negative amount.
		idle := min(idleWatts*dt*1e6, float64(row.delta))
		res.idlePool += idle
		// ONLY AN INTERVAL A MICROVM WAS ALIVE FOR IS RECONCILED: it was seen at
		// its start or its end, which takes in the interval it started in and
		// the one its tail fell in. The monitor shares out nothing while no job
		// runs, so a quiet interval's noise above the baseline would count
		// against the jobs without being theirs.
		if len(row.instances) > 0 || len(rows[i-1].instances) > 0 {
			res.activeMeasured += float64(row.delta) - idle
			res.busySeconds += dt
			res.clipped += idleWatts*dt*1e6 - idle
		}
	}
	res.seconds = rows[len(rows)-1].uptime - rows[0].uptime
	if res.seconds <= 0 {
		res.reason = "the log's uptime does not advance"

		return res
	}

	res.idleBefore, res.idleBeforeOK = quietPower(rows, 1, firstBusy(rows))
	// EACH ROW'S DELTA IS THE INTERVAL ENDING AT IT, so the quiet stretch
	// before the first busy row ends at the row before it, and the one after
	// the last busy row starts one row later: the interval ending at
	// lastBusy+1 began while a microVM was still alive.
	res.idleAfter, res.idleAfterOK = quietPower(rows, lastBusy(rows)+2, len(rows))
	res.idleVerdict = pass
	for _, end := range []struct {
		w  float64
		ok bool
	}{{res.idleBefore, res.idleBeforeOK}, {res.idleAfter, res.idleAfterOK}} {
		v := pass
		switch {
		case !end.ok:
			v = unmeasured
		case math.Abs(end.w-idleWatts) > idleTolerance*idleWatts:
			v = fail
		}
		res.idleVerdict = worst(res.idleVerdict, v)
	}

	res.jobs = jobsSeen(rows)
	var missing []string
	for i := range res.jobs {
		j := &res.jobs[i]
		rec, err := records(j.lease)
		switch {
		case err != nil:
			j.problem = err.Error()
		case rec.Usage == nil:
			j.problem = "billet recorded no usage report"
		case !rec.Usage.Measured["energy"]:
			j.problem = "billet did not measure its energy"
		case rec.Usage.unusable(energyFields...) != "":
			j.problem = "billet's record " + rec.Usage.unusable(energyFields...)
		case rec.Usage.IntervalMillis <= 0:
			// THE TICK IS WHAT THE COARSE-ROW GUARD HOLDS EVERY ROW TO: a job with
			// none would leave the guard nothing to compare.
			j.problem = fmt.Sprintf("billet's record says the monitor ticked every %d ms", rec.Usage.IntervalMillis)
		case rec.Usage.EnergySource != "rapl":
			j.problem = fmt.Sprintf("its energy source is %q, not rapl split by an idle baseline",
				rec.Usage.EnergySource)
		default:
			j.window = float64(rec.Usage.WindowMillis) / 1000
			// THE FINEST TICK IN THE WINDOW is the one a coarse row must be held to:
			// a 2 s row during a 1 s job is too coarse whatever another job ticked at.
			if t := float64(rec.Usage.IntervalMillis) / 1000; t > 0 && (res.tick == 0 || t < res.tick) {
				res.tick = t
			}
			j.active, j.idle = rec.Usage.EnergyActiveUJ, rec.Usage.EnergyIdleUJ
			res.attributed += float64(j.active)
			res.attributedIdle += float64(j.idle)
		}
		if j.problem != "" {
			missing = append(missing, j.lease)
		}
	}
	// THE MONITOR TAKES THE BASELINE PER TICK, so an interval that dips below it
	// adds nothing and one above adds the excess. A log coarser than the tick
	// averages dips into excesses the monitor counted, and cannot reproduce its
	// figure; and at the same resolution the two clocks still fall at different
	// moments, so the jobs may be attributed up to what the quiet ends show the
	// package dipping below the baseline per second, over every busy second.
	for i := 1; res.tick > 0 && i < len(rows); i++ {
		busy := len(rows[i].instances) > 0 || len(rows[i-1].instances) > 0
		if dt := rows[i].uptime - rows[i-1].uptime; busy && dt > 1.5*res.tick {
			res.coarse = fmt.Sprintf("row %d's interval is %.2f s, coarser than the monitor's %.2f s tick, so the "+
				"baseline cannot be taken as the monitor took it; log the energy check with --ipmitool none, "+
				"which keeps the interval at a second", i+1, dt, res.tick)

			break
		}
	}
	shortBefore, quietBefore := shortfall(rows, 1, firstBusy(rows), idleWatts)
	shortAfter, quietAfter := shortfall(rows, lastBusy(rows)+2, len(rows), idleWatts)
	if quiet := quietBefore + quietAfter; quiet > 0 {
		res.allowance = (shortBefore + shortAfter) / quiet * res.busySeconds
	}
	// AND THE OTHER WAY: an interval this log clipped at the baseline may have
	// been an excess and a dip to the monitor, which counted the excess and
	// nothing for the dip, or the reverse. Neither log can see the other's
	// clipping, so each bound gives way by what could have moved it: the
	// upper by the larger of the quiet ends' estimate and this log's own
	// clipping, the lower by this log's own clipping.
	if res.clipped > res.allowance {
		res.allowance = res.clipped
	}
	switch {
	case len(missing) > 0:
		res.reason = "the window's energy cannot be accounted for without " + strings.Join(missing, ", ")
	case len(res.jobs) == 0:
		res.reason = "no billet microVM ran in the window"
	case res.coarse != "":
		res.reason = res.coarse
	// THE SIGNAL IS WHAT SURVIVES THE CLIPPING: the lower bound gives way by it,
	// so energy above the baseline that clipping could account for is a range
	// that holds an attribution of nothing.
	case res.activeMeasured-res.clipped <= minActiveShare*float64(res.rapl):
		res.reason = fmt.Sprintf("the package drew %.2f kJ above the idle baseline, %.2f kJ of it within what "+
			"two clocks clipping at the baseline can disagree on, too little of the window's %.1f kJ to compare",
			res.activeMeasured/1e9, res.clipped/1e9, float64(res.rapl)/1e9)
	default:
		res.ratio = res.attributed / res.activeMeasured
		res.high = attributedHigh + res.allowance/res.activeMeasured
		res.low = attributedLow * (res.activeMeasured - res.clipped) / res.activeMeasured
		res.verdict = fail
		if res.ratio >= res.low && res.ratio <= res.high {
			res.verdict = pass
		}
	}

	return res
}

// maxRowDelta bounds one row's energy, in µJ.
const maxRowDelta = 1 << 40

// energyFields are the counters the reconciliation reads from each record.
var energyFields = []string{"energy_active_uj", "energy_idle_uj", "interval_ms"}

// shortfall is how far the intervals ending at rows [from, to) fell below the
// baseline, in µJ, and how long they lasted.
func shortfall(rows []powerRow, from, to int, idleWatts float64) (float64, float64) {
	var uj, seconds float64
	for i := max(from, 1); i < to; i++ {
		dt := rows[i].uptime - rows[i-1].uptime
		uj += max(idleWatts*dt*1e6-float64(rows[i].delta), 0)
		seconds += dt
	}

	return uj, seconds
}

func firstBusy(rows []powerRow) int {
	i := slices.IndexFunc(rows, func(r powerRow) bool { return len(r.instances) > 0 })
	if i < 0 {
		return len(rows)
	}

	return i
}

func lastBusy(rows []powerRow) int {
	for i := len(rows) - 1; i >= 0; i-- {
		if len(rows[i].instances) > 0 {
			return i
		}
	}

	return -1
}

// quietPower is the package's mean power over rows [from, to), each row's
// delta being the interval that ends at it. A stretch with no interval in it
// is not known.
func quietPower(rows []powerRow, from, to int) (float64, bool) {
	from = max(from, 1)
	if to-from < 1 {
		return 0, false
	}
	var uj int64
	for _, r := range rows[from:to] {
		uj += r.delta
	}
	seconds := rows[to-1].uptime - rows[from-1].uptime
	if seconds <= 0 {
		return 0, false
	}

	return float64(uj) / 1e6 / seconds, true
}

// jobsSeen is every microVM in the log, by lease, with how long it was seen.
func jobsSeen(rows []powerRow) []energyJob {
	first := map[string]float64{}
	last := map[string]float64{}
	var order []string
	for _, r := range rows {
		for _, name := range r.instances {
			lease := strings.TrimPrefix(name, "billet-")
			if _, ok := first[lease]; !ok {
				first[lease] = r.uptime
				order = append(order, lease)
			}
			last[lease] = r.uptime
		}
	}
	jobs := make([]energyJob, 0, len(order))
	for _, lease := range order {
		jobs = append(jobs, energyJob{lease: lease, seen: last[lease] - first[lease]})
	}

	return jobs
}

func kj(uj float64) string { return fmt.Sprintf("%.2f kJ", uj/1e9) }

func (res energyResult) write(w io.Writer) {
	fmt.Fprintf(w, "window: %d rows, epoch %d to %d, %.1f s\n", res.rows, res.from, res.to, res.seconds)
	if res.seconds > 0 {
		fmt.Fprintf(w, "package RAPL: %s, mean %.1f W\n", kj(float64(res.rapl)), float64(res.rapl)/1e6/res.seconds)
	}
	idle := func(label string, v float64, ok bool) {
		if !ok {
			fmt.Fprintf(w, "idle %s: not measured (no quiet interval)\n", label)
			return
		}
		fmt.Fprintf(w, "idle %s: %.1f W against idle_package_watts %.1f W (%+.1f%%)\n", label, v,
			res.idleWatts, 100*(v-res.idleWatts)/res.idleWatts)
	}
	if res.seconds > 0 {
		idle("before", res.idleBefore, res.idleBeforeOK)
		idle("after", res.idleAfter, res.idleAfterOK)
		fmt.Fprintf(w, "idle baseline %s (each quiet end within ±%.0f%%)\n", res.idleVerdict, 100*idleTolerance)
	}
	for _, j := range res.jobs {
		if j.problem != "" {
			fmt.Fprintf(w, "  %s: seen %.0f s; %s\n", j.lease, j.seen, j.problem)
			continue
		}
		fmt.Fprintf(w, "  %s: seen %.0f s, billet's window %.0f s, active %s, idle share %s\n", j.lease, j.seen,
			j.window, kj(float64(j.active)), kj(float64(j.idle)))
	}
	if res.verdict != unmeasured {
		fmt.Fprintf(w, "above idle over the %.0f s a microVM was alive: measured %s, attributed to jobs %s "+
			"(ratio %.3f, accepted [%.3f, %.3f])\n", res.busySeconds, kj(res.activeMeasured),
			kj(res.attributed), res.ratio, res.low, res.high)
		fmt.Fprintf(w, "allowance: %s for the monitor's ticks falling between this log's rows (the quiet ends' "+
			"dips below the baseline per busy second, or this log's own clipping, whichever is larger); "+
			"clipped %s\n", kj(res.allowance), kj(res.clipped))
		fmt.Fprintf(w, "unattributed: %s, %.1f W while a microVM was alive (the host's own work above idle)\n",
			kj(res.activeMeasured-res.attributed),
			(res.activeMeasured-res.attributed)/1e6/res.busySeconds)
		fmt.Fprintf(w, "jobs' idle shares: %s of the window's %s idle baseline\n", kj(res.attributedIdle),
			kj(res.idlePool))
	} else {
		fmt.Fprintf(w, "reconciliation: %s\n", res.reason)
	}
	fmt.Fprintf(w, "\noverall %s\n", worst(res.verdict, res.idleVerdict))
}

// worst is the overall of two verdicts.
func worst(a, b verdict) verdict {
	switch {
	case a == fail || b == fail:
		return fail
	case a == unmeasured || b == unmeasured:
		return unmeasured
	}

	return pass
}
