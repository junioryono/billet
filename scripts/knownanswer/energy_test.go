package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// referenceLog is scripts/power-log.sh's own output over a fake sysfs
// (scripts' TestThePowerLogUndoesTheWrapAndLeavesWhatItCouldNotReadEmpty
// writes and compares it): 34 s at the reference host's 73.5 W idle with
// billet-lease-a running for ten of them at 100 W above it and one more
// interval of its tail at 50 W above, so 1050 J above idle, across a wrap of
// the counter.
var referenceLog = filepath.Join("testdata", "power-log", "power.csv")

// leaseA is billet's record of the one microVM in the reference log.
func leaseA(activeJ, idleJ float64, f func(*usage)) map[string]record {
	u := usage{Measured: allGroups(), present: allFields(), EnergyActiveUJ: int64(activeJ * 1e6), EnergyIdleUJ: int64(idleJ * 1e6),
		EnergySource: "rapl", WindowMillis: 10_000, IntervalMillis: 1000}
	if f != nil {
		f(&u)
	}

	return map[string]record{"lease-a": {Lease: "lease-a", RunID: 1, Usage: &u}}
}

func TestTheJobsEnergyReconcilesWithThePackage(t *testing.T) {
	log, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
	rows := log.rows
	res := reconcile(rows, 73.5, 10, fromMap(leaseA(1000, 40, nil)))
	if res.verdict != pass || res.idleVerdict != pass {
		t.Fatalf("verdict %s, idle %s: %s", res.verdict, res.idleVerdict, res.reason)
	}
	// 21 one-second quiet intervals, one two-second one, ten busy ones and the
	// tail; the quiet end after the microVM starts past the tail.
	if res.rapl != 3_549_000_000 || res.seconds != 34 || res.activeMeasured != 1_050_000_000 ||
		res.idleBefore != 73.5 || res.idleAfter != 73.5 {
		t.Errorf("rapl %.0f µJ over %v s, above idle %.0f, idle %v and %v", res.rapl, res.seconds,
			res.activeMeasured, res.idleBefore, res.idleAfter)
	}
	if len(res.jobs) != 1 || res.jobs[0].lease != "lease-a" || res.jobs[0].seen != 9 {
		t.Errorf("jobs = %+v", res.jobs)
	}
	var out bytes.Buffer
	res.write(&out)
	for _, line := range []string{
		"package RAPL: 3.55 kJ, mean 104.4 W",
		"idle before: 73.5 W against idle_package_watts 73.5 W (+0.0%)",
		"above idle over the 11 s a microVM was alive: measured 1.05 kJ, attributed to jobs 1.00 kJ (ratio 0.952",
		"unattributed: 0.05 kJ, 4.5 W while a microVM was alive",
		"overall PASS",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("the report does not say %q:\n%s", line, out.String())
		}
	}
}

func TestTheReconciliationHoldsItsBounds(t *testing.T) {
	log, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
	rows := log.rows
	for _, tc := range []struct {
		activeJ float64
		want    verdict
	}{
		{892, fail}, {893, pass}, {1070, pass}, {1072, fail},
	} {
		if got := reconcile(rows, 73.5, 10, fromMap(leaseA(tc.activeJ, 0, nil))); got.verdict != tc.want {
			t.Errorf("%v J attributed of 1050: %s (ratio %.3f), want %s", tc.activeJ, got.verdict, got.ratio, tc.want)
		}
	}

	// AN IDLE BASELINE THAT IS WRONG IS ITS OWN FINDING: the quiet ends drew
	// 73.5 W, and a configured 60 W is 22% off.
	res := reconcile(rows, 60, 10, fromMap(leaseA(950, 0, nil)))
	if res.idleVerdict != fail || worst(res.verdict, res.idleVerdict) != fail {
		t.Errorf("idle verdict %s with 60 W configured against 73.5 W measured", res.idleVerdict)
	}
}

// EVERY WAY THE WINDOW'S ENERGY CANNOT BE ACCOUNTED FOR IS UNMEASURED, never a
// PASS on what happened to be there.
func TestAWindowThatCannotBeAccountedForIsUnmeasured(t *testing.T) {
	log, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
	rows := log.rows
	for _, tc := range []struct {
		name    string
		records map[string]record
		rows    func([]powerRow) []powerRow
		says    string
	}{
		{"no record for a microVM the log saw", map[string]record{}, nil, "without lease-a"},
		{"its energy was not measured", leaseA(950, 0, func(u *usage) { u.Measured["energy"] = false }), nil,
			"without lease-a"},
		{"no idle baseline split", leaseA(950, 0, func(u *usage) { u.EnergySource = "rapl-unsplit" }), nil,
			"without lease-a"},
		{"no usage report", map[string]record{"lease-a": {Lease: "lease-a"}}, nil, "without lease-a"},
		{"a microVM at the window's start", leaseA(950, 0, nil), func(r []powerRow) []powerRow {
			r[3].instances = []string{"billet-lease-b"}
			return r
		}, "inside the quiet ends"},
		{"a microVM at the window's end", leaseA(1000, 0, nil), func(r []powerRow) []powerRow {
			r[len(r)-3].instances = []string{"billet-lease-b"}
			return r
		}, "inside the quiet ends"},
		{"a RAPL gap", leaseA(950, 0, nil), func(r []powerRow) []powerRow {
			r[15].deltaOK = false
			return r
		}, "row 16 (epoch 1791540015) has no RAPL delta"},
		{"nothing ran", leaseA(950, 0, nil), func(r []powerRow) []powerRow {
			for i := range r {
				r[i].instances = nil
			}
			return r
		}, "no billet microVM ran"},
		{"too short", leaseA(950, 0, nil), func(r []powerRow) []powerRow { return r[:15] }, "cannot hold 10 quiet rows"},
		{"an inventory that could not be read", leaseA(950, 0, nil), func(r []powerRow) []powerRow {
			r[16].inventoryOK, r[16].instances = false, nil
			return r
		}, "row 17 (epoch 1791540016) could not read which microVMs were running"},
		{"a clock that went back", leaseA(950, 0, nil), func(r []powerRow) []powerRow {
			r[16].uptime = r[15].uptime
			return r
		}, "row 17 (epoch 1791540016) does not come after the row before it"},
		{"an energy counter past any measurement", leaseA(950, 0, func(u *usage) { u.EnergyActiveUJ = math.MaxInt64 }), nil,
			"without lease-a"},
		{"a negative energy counter", leaseA(950, 0, func(u *usage) { u.EnergyIdleUJ = -1 }), nil, "without lease-a"},
		{"no monitor tick in the record", leaseA(950, 0, func(u *usage) { u.IntervalMillis = 0 }), nil, "without lease-a"},
		{"no energy counters in the record", leaseA(950, 0, func(u *usage) {
			u.present = allFields()
			delete(u.present, "energy_active_uj")
		}), nil, "without lease-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]powerRow(nil), rows...)
			if tc.rows != nil {
				in = tc.rows(in)
			}
			res := reconcile(in, 73.5, 10, fromMap(tc.records))
			if res.verdict != unmeasured || !strings.Contains(res.reason, tc.says) {
				t.Errorf("verdict %s (%q), want UNMEASURED saying %q", res.verdict, res.reason, tc.says)
			}
		})
	}
}

// THE IDLE BASELINE IS TAKEN PER INTERVAL, AS THE MONITOR TAKES IT: an
// interval drawing less than the baseline adds no active energy, rather than a
// negative amount that shrinks the window's. Quiet ends at 70 W are inside 5%
// of a 73.5 W baseline; ten busy seconds at 173.5 W are 1000 J above it, and
// subtracting the baseline from the whole window instead would leave 919.5 J
// and fail a correct attribution as over-attribution.
func TestTheIdleBaselineIsTakenPerInterval(t *testing.T) {
	var rows []powerRow
	for i := range 34 {
		row := powerRow{epoch: int64(i), uptime: float64(i), uptimeOK: true, inventoryOK: true}
		if i > 0 {
			row.delta, row.deltaOK = 70_000_000, true
		}
		if i >= 12 && i < 22 {
			row.delta, row.instances = 173_500_000, []string{"billet-lease-a"}
		}
		rows = append(rows, row)
	}
	// The interval after the last busy row is the microVM's tail.
	rows[22].delta = 173_500_000
	res := reconcile(rows, 73.5, 10, fromMap(leaseA(1080, 0, nil)))
	if res.activeMeasured != 1_100_000_000 || res.verdict != pass || res.idleVerdict != pass {
		t.Errorf("above idle %.0f µJ, verdict %s (ratio %.3f), idle %s at %.2f and %.2f W", res.activeMeasured,
			res.verdict, res.ratio, res.idleVerdict, res.idleBefore, res.idleAfter)
	}
}

// A QUIET INTERVAL'S NOISE IS NOT THE JOBS': quiet seconds alternating 70 and
// 77 W before the job average the 73.5 W baseline, and taken interval by
// interval their 77 W halves would add 3.5 J each the monitor never shares
// out. Only the intervals a microVM was alive for are reconciled.
func TestQuietNoiseIsNotCountedAgainstTheJobs(t *testing.T) {
	var rows []powerRow
	for i := range 64 {
		row := powerRow{epoch: int64(i), uptime: float64(i), uptimeOK: true, inventoryOK: true}
		if i > 0 {
			row.delta, row.deltaOK = 70_000_000, true
			if i%2 == 1 {
				row.delta = 77_000_000
			}
		}
		if i >= 27 && i < 37 {
			row.delta, row.instances = 173_500_000, []string{"billet-lease-a"}
		}
		// The quiet end after the job is steady at the baseline: only the one
		// before it dips.
		if i > 37 {
			row.delta = 73_500_000
		}
		rows = append(rows, row)
	}
	res := reconcile(rows, 73.5, 10, fromMap(leaseA(1000, 0, nil)))
	// Ten busy intervals and the one after the last busy row (77 W, the tail).
	if res.verdict != pass || res.busySeconds != 11 || res.activeMeasured != 1_003_500_000 {
		t.Errorf("verdict %s (ratio %.3f), %v s busy, above idle %.0f µJ: %s", res.verdict, res.ratio,
			res.busySeconds, res.activeMeasured, res.reason)
	}

	// THE MONITOR'S TICKS FALL BETWEEN THIS LOG'S ROWS, so it may have seen
	// dips this log averaged away: the quiet ends dip 0.875 W below the baseline
	// on average (1.75 W before the job, none after), which over 11 busy seconds
	// allows 9.625 J more than the 1.02 bound, and no more.
	for _, tc := range []struct {
		activeJ float64
		want    verdict
	}{{1032, pass}, {1035, fail}} {
		got := reconcile(rows, 73.5, 10, fromMap(leaseA(tc.activeJ, 0, nil)))
		if got.allowance != 9_625_000 || got.verdict != tc.want {
			t.Errorf("%v J attributed: allowance %.0f µJ, %s (ratio %.4f, bound %.4f), want %s", tc.activeJ,
				got.allowance, got.verdict, got.ratio, got.high, tc.want)
		}
	}
}

// A LOG COARSER THAN THE MONITOR CANNOT REPRODUCE ITS BASELINE: two-second rows
// average a dip and an excess the monitor's one-second ticks counted apart.
func TestALogCoarserThanTheMonitorIsUnmeasured(t *testing.T) {
	var rows []powerRow
	for i := range 34 {
		row := powerRow{epoch: int64(2 * i), uptime: float64(2 * i), uptimeOK: true, inventoryOK: true}
		if i > 0 {
			row.delta, row.deltaOK = 147_000_000, true
		}
		if i >= 12 && i < 22 {
			row.delta, row.instances = 347_000_000, []string{"billet-lease-a", "billet-lease-b"}
		}
		rows = append(rows, row)
	}
	// TWO JOBS, ONE TICKING EVERY SECOND AND ONE EVERY TWO: the two-second rows
	// are coarse for the first whatever the second ticked at.
	recs := leaseA(1000, 0, nil)
	slow := leaseA(1000, 0, func(u *usage) { u.IntervalMillis = 2000 })["lease-a"]
	slow.Lease = "lease-b"
	recs["lease-b"] = slow
	res := reconcile(rows, 73.5, 10, fromMap(recs))
	if res.verdict != unmeasured || !strings.Contains(res.reason, "coarser than the monitor's 1.00 s tick") {
		t.Errorf("verdict %s (%q), want UNMEASURED", res.verdict, res.reason)
	}
}

// ONE TRACE, TWO CLOCKS: the package alternates 70 and 80 W second by second
// while a microVM is alive. This log's rows fall on the seconds and see 70 and
// 80, clipping the 70s; the monitor's ticks fall half a second later and see
// 75 every time. Its attribution is what that trace gives at its ticks, and the
// reconciliation must accept it although the two disagree by half: each bound
// gives way by the clipping that could have moved it.
func TestOneTraceSampledByTwoClocksReconciles(t *testing.T) {
	const idle = 73.5
	busy := func(k int) bool { return k >= 11 && k < 51 }
	power := func(k int) float64 { // the package's power over second [k, k+1)
		switch {
		case !busy(k):
			return idle
		case k%2 == 0:
			return 70
		default:
			return 80
		}
	}
	var rows []powerRow
	for i := range 64 {
		row := powerRow{epoch: int64(i), uptime: float64(i), uptimeOK: true, inventoryOK: true}
		if i > 0 {
			row.delta, row.deltaOK = int64(power(i-1)*1e6), true
		}
		if busy(i) {
			row.instances = []string{"billet-lease-a"}
		}
		rows = append(rows, row)
	}
	// The monitor's tick [k+0.5, k+1.5) is half of second k and half of k+1,
	// clipped at the baseline as the monitor clips it, over the microVM's life.
	var monitor float64
	for k := 10; k < 52; k++ {
		monitor += max((power(k)+power(k+1))/2-idle, 0)
	}
	res := reconcile(rows, idle, 10, fromMap(leaseA(monitor, 0, nil)))
	if res.verdict != pass {
		t.Errorf("the monitor's %.1f J against this log's %.1f J (clipped %.1f J): %s, ratio %.3f in [%.3f, %.3f]: %s",
			monitor, res.activeMeasured/1e6, res.clipped/1e6, res.verdict, res.ratio, res.low, res.high, res.reason)
	}
	if res.clipped != 70_000_000 || res.ratio > attributedLow {
		t.Errorf("clipped %.0f µJ, ratio %.3f: the trace does not exercise the lower bound", res.clipped, res.ratio)
	}

	// THE OTHER WAY, with quiet ends that never dip: noise the monitor clipped
	// where this log saw an excess is as large, in expectation, as what this log
	// clipped, so the upper bound gives way by this log's own 70 J of clipping
	// when the quiet ends offer less. 190 J against this log's 130 J passes only
	// with it (1.462 against 1.02 + 70/130 = 1.558).
	over := reconcile(rows, idle, 10, fromMap(leaseA(190, 0, nil)))
	if over.allowance != 70_000_000 || over.verdict != pass {
		t.Errorf("allowance %.0f µJ, %s (ratio %.3f in [%.3f, %.3f])", over.allowance, over.verdict, over.ratio,
			over.low, over.high)
	}
}

// ENERGY ABOVE THE BASELINE THAT CLIPPING COULD ACCOUNT FOR IS NO SIGNAL: busy
// seconds alternating 70 and 77 W measure 70 J above the baseline and clip 70
// J, so the lower bound would fall to zero and an attribution of nothing pass.
func TestEnergyWithinTheClippingIsUnmeasured(t *testing.T) {
	var rows []powerRow
	for i := range 64 {
		row := powerRow{epoch: int64(i), uptime: float64(i), uptimeOK: true, inventoryOK: true}
		if i > 0 {
			row.delta, row.deltaOK = 73_500_000, true
			if i-1 >= 11 && i-1 < 51 {
				row.delta = 70_000_000
				if (i-1)%2 == 1 {
					row.delta = 77_000_000
				}
			}
		}
		if i >= 11 && i < 51 {
			row.instances = []string{"billet-lease-a"}
		}
		rows = append(rows, row)
	}
	for _, attributed := range []float64{0, 70} {
		res := reconcile(rows, 73.5, 10, fromMap(leaseA(attributed, 0, nil)))
		if res.activeMeasured != 70_000_000 || res.clipped != 70_000_000 || res.verdict != unmeasured ||
			!strings.Contains(res.reason, "within what two clocks clipping at the baseline can disagree on") {
			t.Errorf("%v J attributed: above idle %.0f µJ, clipped %.0f µJ, %s (%q)", attributed,
				res.activeMeasured, res.clipped, res.verdict, res.reason)
		}
	}
}

func TestThePowerLogIsReadByItsHeader(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	log, err := readPowerLog(write("reordered.csv",
		"instances,rapl_delta_uj,uptime_s,epoch_s,extra\nbillet-a;billet-b,5,10.50,7,x\n,,10.75,8,y\n?,5,11.00,9,z\n"))
	if err != nil {
		t.Fatal(err)
	}
	rows := log.rows
	if log.meter {
		t.Error("a log with no meter_watts column was read as one with a meter")
	}
	if len(rows) != 3 || rows[0].epoch != 7 || rows[0].uptime != 10.5 || !rows[0].deltaOK || rows[0].delta != 5 ||
		strings.Join(rows[0].instances, ",") != "billet-a,billet-b" || !rows[0].inventoryOK || rows[1].deltaOK ||
		len(rows[1].instances) != 0 || !rows[1].inventoryOK || rows[2].inventoryOK || len(rows[2].instances) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	for name, body := range map[string]string{
		"no instances column":    "epoch_s,uptime_s,rapl_delta_uj\n1,1.00,5\n",
		"a negative delta":       "epoch_s,uptime_s,rapl_delta_uj,instances\n1,1.00,-5,\n",
		"a word for an epoch":    "epoch_s,uptime_s,rapl_delta_uj,instances\nnow,1.00,5,\n",
		"a row past a megajoule": "epoch_s,uptime_s,rapl_delta_uj,instances\n1,1.00,1099511627777,\n",
		"a NaN uptime":           "epoch_s,uptime_s,rapl_delta_uj,instances\n1,NaN,5,\n",
		"an infinite uptime":     "epoch_s,uptime_s,rapl_delta_uj,instances\n1,+Inf,5,\n",
	} {
		if _, err := readPowerLog(write(strings.ReplaceAll(name, " ", "-")+".csv", body)); err == nil {
			t.Errorf("%s was read", name)
		}
	}
}

func TestEnergyExitsWithItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		activeJ float64
		idle    string
		code    int
	}{
		{"reconciled", 950, "73.5", exitPass},
		{"over-attributed", 1100, "73.5", exitFail},
		{"no record", -1, "73.5", exitUnmeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recDir := t.TempDir()
			if tc.activeJ >= 0 {
				body, err := json.Marshal(recordJSON(leaseA(tc.activeJ, 0, nil)["lease-a"]))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(recDir, "lease-a.json"), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"energy", "--power-log", referenceLog, "--records", recDir,
				"--idle-watts", tc.idle}, &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s%s", code, tc.code, stdout.String(), stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"energy", "--power-log", referenceLog, "--records", t.TempDir()},
		&stdout, &stderr); code != exitUsage {
		t.Errorf("energy without --idle-watts exited %d, want %d", code, exitUsage)
	}
	// NaN COMPARES FALSE WITH EVERYTHING, so a NaN baseline would pass both idle
	// ends; it and infinity are refused as flags.
	for _, w := range []string{"NaN", "+Inf", "-1"} {
		if code := run(t.Context(), []string{"energy", "--power-log", referenceLog, "--records", t.TempDir(),
			"--idle-watts", w}, &stdout, &stderr); code != exitUsage {
			t.Errorf("--idle-watts %s exited %d, want %d", w, code, exitUsage)
		}
	}
}

// A LOG WITHOUT A METER IS REPORTED AS IT ALWAYS WAS. These goldens were
// written by the energy check before it read a meter column, and are compared
// byte for byte, exit status included, through the command a runbook runs.
func TestTheEnergyReportWithoutAMeterIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		activeJ float64
		alter   func(*usage)
		code    int
	}{
		{"pass", 1000, nil, exitPass},
		{"fail", 1100, nil, exitFail},
		{"unmeasured", 1000, func(u *usage) { u.EnergySource = "rapl-unsplit" }, exitUnmeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recDir := t.TempDir()
			body, err := json.Marshal(recordJSON(leaseA(tc.activeJ, 40, tc.alter)["lease-a"]))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(recDir, "lease-a.json"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"energy", "--power-log", referenceLog, "--records", recDir,
				"--idle-watts", "73.5"}, &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s%s", code, tc.code, stdout.String(), stderr.String())
			}
			golden := filepath.Join("testdata", "power-log", "energy-"+tc.name+".txt")
			if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
				if err := os.WriteFile(golden, stdout.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stdout.Bytes(), want) {
				t.Errorf("the report:\n%s\nwant %s:\n%s", stdout.String(), golden, want)
			}
		})
	}
}

// meterLog writes the reference log with a meter_watts column: the wall reads
// 50 W plus 1.2 times each interval's RAPL power (138.2 W at the 73.5 W idle,
// 258.2 W busy, 198.2 W over the tail), and 138.2 W on the first row. cell
// may replace a row's reading; "" leaves it empty.
func meterLog(t *testing.T, cell func(i int, w string) string) string {
	t.Helper()
	log, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("epoch_s,uptime_s,rapl_delta_uj,instances,meter_watts\n")
	for i, r := range log.rows {
		delta, w := "", "138.2"
		if r.deltaOK {
			delta = strconv.FormatInt(r.delta, 10)
			w = strconv.FormatFloat(50+1.2*float64(r.delta)/1e6/(r.uptime-log.rows[i-1].uptime), 'f', 1, 64)
		}
		if cell != nil {
			w = cell(i, w)
		}
		fmt.Fprintf(&b, "%d,%.2f,%s,%s,%s\n", r.epoch, r.uptime, delta, strings.Join(r.instances, ";"), w)
	}
	path := filepath.Join(t.TempDir(), "power.csv")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// energyRun runs the energy check over a log with lease-a's record at 1000 J.
func energyRun(t *testing.T, logPath string, extra ...string) (string, int) {
	t.Helper()
	recDir := t.TempDir()
	body, err := json.Marshal(recordJSON(leaseA(1000, 40, nil)["lease-a"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recDir, "lease-a.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), append([]string{"energy", "--power-log", logPath, "--records", recDir,
		"--idle-watts", "73.5"}, extra...), &stdout, &stderr)
	if stderr.Len() > 0 {
		t.Errorf("stderr: %s", stderr.String())
	}

	return stdout.String(), code
}

// THE WALL IS REPORTED BESIDE THE VERDICT AND NEVER MOVES IT: the package's
// reconciliation reads exactly as it does without a meter, and the wall's
// lines follow it.
func TestTheEnergyCheckReadsTheWallBesideRAPL(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "power-log", "energy-pass.txt"))
	if err != nil {
		t.Fatal(err)
	}
	rapl := strings.TrimSuffix(string(golden), "\noverall PASS\n")
	for _, tc := range []struct {
		name  string
		cell  func(int, string) string
		extra []string
		says  []string
	}{
		{"every interval read", nil, nil, []string{
			"wall meter: idle 138.2 W before, 138.2 W after (22 of 22 quiet intervals read)\n",
			"wall meter above its idle of 138.2 W while a microVM was alive: 1.26 kJ; the package's RAPL above " +
				"idle_package_watts over the same intervals, unclipped: 1.05 kJ; attributed to jobs: 1.00 kJ\n",
			"wall meter: RAPL/wall 0.833, attributed/wall 0.794 (no verdict",
		}},
		// A QUIET INTERVAL WITHOUT A READING leaves the idle to the rest: the
		// fourth row's would have been 20 W above it.
		{"a quiet interval missed", func(i int, w string) string {
			if i == 3 {
				return ""
			}
			return w
		}, nil, []string{"(21 of 22 quiet intervals read)", "RAPL/wall 0.833"}},
		{"the idle from the quiet ends", func(i int, w string) string {
			if i == 3 {
				return "158.2"
			}
			return w
		}, nil, []string{"idle 140.0 W before, 138.2 W after (22 of 22 quiet intervals read)",
			// The idle is the mean over both ends' 22 seconds, 139.1 W.
			"wall meter above its idle of 139.1 W while a microVM was alive: 1.25 kJ;"}},
		{"a busy interval missed", func(i int, w string) string {
			if i == 15 {
				return ""
			}
			return w
		}, nil, []string{"wall meter: could not tell; row 16 (epoch 1791540015) ends an interval a microVM was " +
			"alive for and has no meter reading"}},
		// THE TAIL IS A BUSY INTERVAL: it began while the microVM was alive.
		{"the tail missed", func(i int, w string) string {
			if i == 22 {
				return ""
			}
			return w
		}, nil, []string{"wall meter: could not tell; row 23 (epoch 1791540022)"}},
		{"no quiet reading", func(i int, w string) string {
			if i < 12 || i > 22 {
				return ""
			}
			return w
		}, nil, []string{"wall meter: idle not measured before, not measured after (0 of 22 quiet intervals read)\n" +
			"wall meter: could not tell; no quiet interval has a meter reading"}},
		{"a wall that never rose", func(int, string) string { return "138.2" }, nil,
			[]string{"while a microVM was alive: 0.00 kJ", "wall meter: no ratio"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := energyRun(t, meterLog(t, tc.cell), tc.extra...)
			if code != exitPass {
				t.Errorf("exit %d, want %d: the wall moved the verdict", code, exitPass)
			}
			if !strings.HasPrefix(out, rapl) || !strings.HasSuffix(out, "\noverall PASS\n") {
				t.Errorf("the package's reconciliation changed with a meter:\n%s\nwant it to begin:\n%s", out, rapl)
			}
			for _, line := range tc.says {
				if !strings.Contains(out, line) {
					t.Errorf("the report does not say %q:\n%s", line, out)
				}
			}
		})
	}

	// A MICROVM WITH NO RECORD leaves the jobs' sum short of the window's, so it
	// is no share of the wall, while the wall and the package still are; here
	// lease-b, seen first, has none, and lease-a, seen last, has one.
	path := meterLog(t, nil)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	two := strings.Replace(string(body), ",billet-lease-a,", ",billet-lease-b;billet-lease-a,", 1)
	if two == string(body) {
		t.Fatal("the log holds no row to add lease-b to")
	}
	if err := os.WriteFile(path, []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := energyRun(t, path)
	for _, line := range []string{
		"unclipped: 1.05 kJ; attributed to jobs: could not tell, a microVM's record could not be used\n",
		"wall meter: RAPL/wall 0.833, attributed/wall could not tell (no verdict",
	} {
		if code != exitUnmeasured || !strings.Contains(out, line) {
			t.Errorf("exit %d; the report does not say %q:\n%s", code, line, out)
		}
	}

	// A WINDOW THE PACKAGE COULD NOT BE RECONCILED OVER has no busy interval
	// to read the wall over, and says so rather than printing zeros.
	out, code = energyRun(t, meterLog(t, nil), "--quiet-rows", "20")
	if code != exitUnmeasured || !strings.Contains(out, "wall meter: could not tell; the window has no interval a "+
		"microVM was alive for to compare") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	// A RAPL GAP PART WAY THROUGH THE JOB stops the reconciliation after it has
	// counted busy seconds but before it has the window's length, and the wall
	// is not read over the part before the gap.
	gap := meterLog(t, nil)
	body, err = os.ReadFile(gap)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	f := strings.Split(lines[16], ",")
	f[2] = ""
	lines[16] = strings.Join(f, ",")
	if err := os.WriteFile(gap, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = energyRun(t, gap)
	if code != exitUnmeasured || !strings.Contains(out, "reconciliation: row 16 (epoch 1791540015) has no RAPL delta, "+
		"so the window's energy is not known\nwall meter: idle not measured before, not measured after "+
		"(0 of 0 quiet intervals read)\nwall meter: could not tell; the window has no interval a microVM was alive "+
		"for to compare\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}

	// AND A WINDOW THAT WAS READ WHOLE BUT HELD NO MICROVM has none either (its
	// quiet ends, at the busy power, fail the idle baseline, which the wall
	// leaves alone).
	quiet := meterLog(t, nil)
	body, err = os.ReadFile(quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quiet, []byte(strings.ReplaceAll(string(body), "billet-lease-a", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = energyRun(t, quiet)
	if code != exitFail || !strings.Contains(out, "reconciliation: no billet microVM ran in the window\n"+
		"wall meter: idle not measured before, not measured after (0 of 0 quiet intervals read)\n"+
		"wall meter: could not tell; the window has no interval a microVM was alive for to compare\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// THE PACKAGE'S RISE IS SET AGAINST THE WALL'S UNCLIPPED: the wall is not
// clipped at its idle, so neither is the package here, whatever the
// reconciliation clipped. Busy seconds alternate 70 and 80 W against a 73.5 W
// baseline, and the wall reads 50 W plus 1.2 times the package throughout. The
// interval ending at row 21 is two seconds long, so a reading taken as energy
// rather than power would be seen.
func TestTheWallIsSetAgainstThePackageUnclipped(t *testing.T) {
	const idle = 73.5
	var rows []powerRow
	for i := range 64 {
		row := powerRow{epoch: int64(i), uptime: float64(i), uptimeOK: true, inventoryOK: true, meterW: 50 + 1.2*idle,
			meterOK: true}
		dt := 1.0
		if i >= 20 {
			row.uptime++
			if i == 20 {
				dt = 2
			}
		}
		if i > 0 {
			p := idle
			if i-1 >= 11 && i-1 < 51 {
				p = 70 + 10*float64((i-1)%2)
			}
			row.delta, row.deltaOK, row.meterW = int64(p*dt*1e6), true, 50+1.2*p
		}
		if i >= 11 && i < 51 {
			row.instances = []string{"billet-lease-a"}
		}
		rows = append(rows, row)
	}
	res := reconcile(rows, idle, 10, fromMap(leaseA(60, 0, nil)))
	if res.clipped == 0 {
		t.Fatal("the trace clips nothing, so it cannot tell clipped from unclipped")
	}
	var rapl float64
	for i := 1; i < len(rows); i++ {
		if len(rows[i].instances) > 0 || len(rows[i-1].instances) > 0 {
			rapl += float64(rows[i].delta) - idle*1e6*(rows[i].uptime-rows[i-1].uptime)
		}
	}
	w := wallReference(rows, res)
	if w.reason != "" || math.Abs(w.rapl-rapl) > 1 || math.Abs(w.above-1.2*rapl) > 1 {
		t.Errorf("wall above %.0f µJ, package %.0f µJ (%q), want the package %.0f µJ and the wall 1.2 times it",
			w.above, w.rapl, w.reason, rapl)
	}
}

// A METER CELL THE LOGGER COULD NOT HAVE WRITTEN IS A LOG THAT IS NOT ONE, and
// is refused rather than read as a reading or as an empty cell.
func TestAMeterCellIsAPowerOrEmpty(t *testing.T) {
	for _, cell := range []string{"-1", "NaN", "+Inf", "lots", "1000000", "1e3", "+100", ".5", "0x1p3", "1.1234567", "12."} {
		path := meterLog(t, func(i int, w string) string {
			if i == 5 {
				return cell
			}
			return w
		})
		_, err := readPowerLog(path)
		if err == nil || !strings.Contains(err.Error(), "line 7: meter_watts \""+cell+"\" is not a power") {
			t.Errorf("meter_watts %q: %v", cell, err)
		}
	}
	log, err := readPowerLog(meterLog(t, func(i int, w string) string {
		switch i {
		case 4:
			return ""
		case 5:
			return "0"
		}
		return w
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !log.meter || log.rows[4].meterOK || !log.rows[5].meterOK || log.rows[5].meterW != 0 ||
		!log.rows[6].meterOK || log.rows[6].meterW != 138.2 {
		t.Errorf("meter %v, rows 5 to 7: %+v %+v %+v", log.meter, log.rows[4], log.rows[5], log.rows[6])
	}
}
