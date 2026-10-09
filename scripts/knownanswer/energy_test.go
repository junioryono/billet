package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
	u := usage{Measured: allGroups(), EnergyActiveUJ: int64(activeJ * 1e6), EnergyIdleUJ: int64(idleJ * 1e6),
		EnergySource: "rapl", WindowMillis: 10_000}
	if f != nil {
		f(&u)
	}

	return map[string]record{"lease-a": {Lease: "lease-a", RunID: 1, Usage: &u}}
}

func TestTheJobsEnergyReconcilesWithThePackage(t *testing.T) {
	rows, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
	res := reconcile(rows, 73.5, 10, fromMap(leaseA(1000, 40, nil)))
	if res.verdict != pass || res.idleVerdict != pass {
		t.Fatalf("verdict %s, idle %s: %s", res.verdict, res.idleVerdict, res.reason)
	}
	// 21 one-second quiet intervals, one two-second one, ten busy ones and the
	// tail; the quiet end after the microVM starts past the tail.
	if res.rapl != 3_549_000_000 || res.seconds != 34 || res.activeMeasured != 1_050_000_000 ||
		res.idleBefore != 73.5 || res.idleAfter != 73.5 {
		t.Errorf("rapl %d µJ over %v s, above idle %.0f, idle %v and %v", res.rapl, res.seconds,
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
		"above idle: measured 1.05 kJ, attributed to jobs 1.00 kJ (ratio 0.952, accepted [0.85, 1.02])",
		"unattributed: 0.05 kJ, 1.5 W over the window",
		"overall PASS",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("the report does not say %q:\n%s", line, out.String())
		}
	}
}

func TestTheReconciliationHoldsItsBounds(t *testing.T) {
	rows, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
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
	rows, err := readPowerLog(referenceLog)
	if err != nil {
		t.Fatal(err)
	}
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

func TestThePowerLogIsReadByItsHeader(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	rows, err := readPowerLog(write("reordered.csv",
		"instances,rapl_delta_uj,uptime_s,epoch_s,extra\nbillet-a;billet-b,5,10.50,7,x\n,,10.75,8,y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].epoch != 7 || rows[0].uptime != 10.5 || !rows[0].deltaOK || rows[0].delta != 5 ||
		strings.Join(rows[0].instances, ",") != "billet-a,billet-b" || rows[1].deltaOK || len(rows[1].instances) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	for name, body := range map[string]string{
		"no instances column": "epoch_s,uptime_s,rapl_delta_uj\n1,1.00,5\n",
		"a negative delta":    "epoch_s,uptime_s,rapl_delta_uj,instances\n1,1.00,-5,\n",
		"a word for an epoch": "epoch_s,uptime_s,rapl_delta_uj,instances\nnow,1.00,5,\n",
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
}
