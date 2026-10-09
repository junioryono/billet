package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE POWER LOGGER IS EXECUTED AGAINST A FAKE SYSFS, NOT PATTERN-MATCHED. Its
// rules are shell rules: a wrap of the package counter is undone with the
// zone's own range, a gap long enough to hide a wrap leaves the delta empty
// rather than wrong, a BMC that failed or reports its reading deactivated
// leaves its cell empty rather than zero, and a microVM counts only while its
// cgroup holds a process. So it runs here over a fake powercap zone, /proc and
// cgroup tree, with a fake `sleep` that advances them one scripted step per
// interval, so every row is known before the script writes it.

// powerFakes stand in for what the logger runs. `sleep` pops the next step
// from $FAKE_STATE/steps and applies it: the epoch `date` answers, the uptime,
// the counter, what the BMC will say and which microVMs hold a process. `timeout`
// drops its bounds and runs the command, as a timeout that never fires does.
var powerFakes = map[string]string{
	"sleep": `#!/bin/bash
set -eu
s=$FAKE_STATE
IFS= read -r step <"$s/steps" || exit 0
tail -n +2 "$s/steps" >"$s/steps.next"
mv "$s/steps.next" "$s/steps"
read -r epoch uptime energy bmc vms <<<"$step"
printf '%s\n' "$epoch" >"$s/epoch"
printf '%s 0.00\n' "$uptime" >"$s/proc/uptime"
printf '%s\n' "$energy" >"$s/powercap/energy_uj"
printf '%s\n' "$bmc" >"$s/bmc"
for d in "$s"/cgroup/firecracker-v1.16.1/billet-*; do
	if [ -d "$d" ]; then : >"$d/cgroup.procs" 2>/dev/null || true; fi
done
if [ "$vms" != - ]; then
	IFS=, read -r -a names <<<"$vms"
	for n in "${names[@]}"; do
		mkdir -p "$s/cgroup/firecracker-v1.16.1/$n"
		printf '4242\n' >"$s/cgroup/firecracker-v1.16.1/$n/cgroup.procs"
	done
fi
# THE Nth SLEEP LETS turbostat PRINT ITS Nth READING, and returns once it has,
# so the sample after it is the first to see that reading.
c=$(( $(cat "$s/sleeps" 2>/dev/null || echo 0) + 1 ))
printf '%s\n' "$c" >"$s/sleeps"
if [ -e "$s/ts-readings" ] && [ "$c" -le "$(cat "$s/ts-readings")" ]; then
	: >"$s/ts-tick-$c"
	for _ in $(seq 1 500); do [ -e "$s/ts-done-$c" ] && break; /bin/sleep 0.01; done
fi
`,
	"date": `#!/bin/bash
cat "$FAKE_STATE/epoch"
`,
	"timeout": `#!/bin/bash
while [ "$#" -gt 0 ]; do
	case "$1" in
	-k) shift 2 ;;
	-*) shift ;;
	*) break ;;
	esac
done
shift
exec "$@"
`,
	"ipmitool": `#!/bin/bash
[ "$*" = "dcmi power reading" ] || { echo "fake ipmitool: unexpected $*" >&2; exit 90; }
b=$(cat "$FAKE_STATE/bmc")
case "$b" in
fail) echo "Could not open device at /dev/ipmi0" >&2; exit 1 ;;
off) state=deactivated; b=0 ;;
*) state=activated ;;
esac
cat <<EOF

    Instantaneous power reading:                   $b Watts
    Minimum during sampling period:                 70 Watts
    Maximum during sampling period:                350 Watts
    Average power reading over sample period:      180 Watts
    IPMI timestamp:                           Thu Oct  9 12:00:00 2026
    Sampling period:                          00000001 Seconds.
    Power reading state is:                   $state

EOF
`,
	// turbostat prints its header, then one reading per sleep the logger takes
	// (7k.50 for the kth), as many as $FAKE_STATE/ts-readings says, and then
	// nothing more while it stays alive: a turbostat that stopped reporting.
	"turbostat": `#!/bin/bash
s=$FAKE_STATE
printf '%s\n' "$$" >"$s/ts-pid"
printf 'PkgWatt\n'
k=1
while [ "$k" -le "$(cat "$s/ts-readings")" ]; do
	until [ -e "$s/ts-tick-$k" ]; do /bin/sleep 0.01; done
	printf '7%d.50\n' "$k"
	: >"$s/ts-done-$k"
	k=$((k + 1))
done
exec /bin/sleep 600
`,
}

type powerHarness struct{ bin, state string }

func newPowerHarness(t *testing.T, fakes ...string) powerHarness {
	t.Helper()
	root := t.TempDir()
	h := powerHarness{bin: filepath.Join(root, "bin"), state: filepath.Join(root, "state")}
	for _, dir := range []string{h.bin, filepath.Join(h.state, "proc"), filepath.Join(h.state, "powercap"),
		filepath.Join(h.state, "cgroup", "firecracker-v1.16.1")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"bash", "cat", "mkdir", "mktemp", "mv", "rm", "tail", "dirname", "awk", "seq", "kill", "wc"} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("this test needs %s on PATH: %v", tool, err)
		}
		if err := os.Symlink(resolved, filepath.Join(h.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range fakes {
		if err := forkSafeWriteFile(filepath.Join(h.bin, name), []byte(powerFakes[name]), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	return h
}

// step is one sample's state: the wall clock, uptime in centiseconds, the raw
// counter, what the BMC answers ("fail", "off" or watts) and the microVMs
// holding a process.
type step struct {
	epoch    int64
	uptimeCS int64
	energy   int64
	bmc      string
	vms      []string
}

func (s step) line() string {
	vms := "-"
	if len(s.vms) > 0 {
		vms = strings.Join(s.vms, ",")
	}

	return fmt.Sprintf("%d %d.%02d %d %s %s", s.epoch, s.uptimeCS/100, s.uptimeCS%100, s.energy, s.bmc, vms)
}

// plant writes the reference zone's range and the first step as the state before the
// first sample, and the rest as what each sleep applies.
func (h powerHarness) plant(t *testing.T, steps []step) {
	t.Helper()
	first := steps[0]
	files := map[string]string{
		"powercap/max_energy_range_uj": fmt.Sprintf("%d\n", referenceMaxRange),
		"powercap/energy_uj":           fmt.Sprintf("%d\n", first.energy),
		"proc/uptime":                  fmt.Sprintf("%d.%02d 0.00\n", first.uptimeCS/100, first.uptimeCS%100),
		"epoch":                        fmt.Sprintf("%d\n", first.epoch),
		"bmc":                          first.bmc + "\n",
	}
	var rest strings.Builder
	for _, s := range steps[1:] {
		rest.WriteString(s.line() + "\n")
	}
	files["steps"] = rest.String()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(h.state, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, vm := range first.vms {
		dir := filepath.Join(h.state, "cgroup", "firecracker-v1.16.1", vm)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (h powerHarness) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"power-log.sh", "--powercap", filepath.Join(h.state, "powercap"),
		"--proc", filepath.Join(h.state, "proc"), "--cgroup", filepath.Join(h.state, "cgroup")}, args...)
	cmd := exec.CommandContext(t.Context(), filepath.Join(h.bin, "bash"), full...)
	cmd.Env = []string{"PATH=" + h.bin, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "FAKE_STATE=" + h.state}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	return out.String(), err
}

// referenceMaxRange is the reference host's package zone (reference-hardware.md).
const referenceMaxRange = 65_532_610_987

// referenceLog is a quiet minute's worth of the reference host around one
// microVM: twelve quiet samples at the 73.5 W idle baseline, ten with
// billet-lease-a running at 173.5 W, one interval of its tail at 123.5 W
// after it is last seen, twelve quiet again; the counter starts
// just below its range so it wraps in the first quiet stretch; one quiet
// interval is two seconds long; the BMC reads RAPL's whole watts plus 60 W (DCMI answers whole watts) except once when
// it fails and once when it reports its reading deactivated.
func referenceLog() []step {
	var steps []step
	energy := int64(65_400_000_000)
	epoch, uptime := int64(1_791_540_000), int64(100_000)
	for i := range 34 {
		busy := i >= 12 && i < 22
		seconds := int64(1)
		if i == 30 {
			seconds = 2
		}
		watts := 73.5
		switch {
		case busy:
			watts = 173.5
		case i == 22:
			// THE INTERVAL AFTER THE LAST BUSY ROW began while the microVM was
			// alive, so it carries its tail and is no part of the quiet end.
			watts = 123.5
		}
		if i > 0 {
			epoch += seconds
			uptime += 100 * seconds
			energy = (energy + int64(watts*1e6)*seconds) % referenceMaxRange
		}
		s := step{epoch: epoch, uptimeCS: uptime, energy: energy, bmc: strconv.Itoa(int(watts) + 60)}
		switch i {
		case 5:
			s.bmc = "fail"
		case 6:
			s.bmc = "off"
		}
		if busy {
			s.vms = []string{"billet-lease-a"}
		}
		steps = append(steps, s)
	}

	return steps
}

// powerLogFixture is where scripts/knownanswer reads a log this script wrote;
// it is compared here and rewritten with BILLET_UPDATE_FIXTURES=1.
var powerLogFixture = filepath.Join("knownanswer", "testdata", "power-log", "power.csv")

func TestThePowerLogUndoesTheWrapAndLeavesWhatItCouldNotReadEmpty(t *testing.T) {
	t.Parallel()
	h := newPowerHarness(t, "sleep", "date", "timeout", "ipmitool")
	steps := referenceLog()
	h.plant(t, steps)
	out := filepath.Join(t.TempDir(), "power.csv")
	summary, err := h.run(t, "--out", out, "--seconds", strconv.Itoa(len(steps)), "--ipmitool", "ipmitool",
		"--turbostat", "none")
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// THE ROWS, BUILT INDEPENDENTLY OF THE SCRIPT from the steps it was fed.
	want := []string{"epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances"}
	for i, s := range steps {
		delta := ""
		if i > 0 {
			d := s.energy - steps[i-1].energy
			if d < 0 {
				d += referenceMaxRange
			}
			delta = strconv.FormatInt(d, 10)
		}
		bmc := s.bmc
		if bmc == "fail" || bmc == "off" {
			bmc = ""
		}
		want = append(want, fmt.Sprintf("%d,%d.%02d,%d,%s,%s,,%s", s.epoch, s.uptimeCS/100, s.uptimeCS%100,
			s.energy, delta, bmc, strings.Join(s.vms, ";")))
	}
	if got := strings.TrimSuffix(string(written), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("the log:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// The wrap happened, and its delta is the power that was drawn, not a
	// negative number or the counter's range.
	wrapped := false
	for i := 1; i < len(steps); i++ {
		wrapped = wrapped || steps[i].energy < steps[i-1].energy
	}
	if !wrapped {
		t.Fatal("the reference log never wraps; the fixture proves nothing about the wrap")
	}

	for _, line := range []string{
		"rows 34 (epoch 1791540000 to 1791540034), RAPL intervals 33, gaps 0",
		"BMC: 31 paired readings",
		"offset 59.5 W",
		"fit: BMC = 59.5 W + 1.000 x RAPL, r^2 1.000",
		"turbostat: no reading paired with RAPL",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("the summary does not say %q:\n%s", line, summary)
		}
	}

	if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(powerLogFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(powerLogFixture, written, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile(powerLogFixture)
	if err != nil {
		t.Fatalf("%v (run with BILLET_UPDATE_FIXTURES=1 to write the fixture)", err)
	}
	if !bytes.Equal(committed, written) {
		t.Errorf("%s is not what the logger writes today; run with BILLET_UPDATE_FIXTURES=1", powerLogFixture)
	}
}

// A GAP LONG ENOUGH FOR THE COUNTER TO HAVE GONE ROUND IS NOT A DELTA: at 1 kW
// the reference zone lasts 65.5 s, and a reading 70 s later could be any
// number of wraps past the last.
func TestAGapThatCouldHideAWrapLeavesTheDeltaEmpty(t *testing.T) {
	t.Parallel()
	h := newPowerHarness(t, "sleep", "date", "timeout")
	h.plant(t, []step{
		{epoch: 10, uptimeCS: 1000, energy: 1_000_000, bmc: "fail"},
		{epoch: 11, uptimeCS: 1100, energy: 74_500_000, bmc: "fail"},
		{epoch: 81, uptimeCS: 8100, energy: 5_219_500_000, bmc: "fail"},
		{epoch: 82, uptimeCS: 8200, energy: 5_293_000_000, bmc: "fail"},
	})
	out := filepath.Join(t.TempDir(), "power.csv")
	if summary, err := h.run(t, "--out", out, "--seconds", "4", "--ipmitool", "none", "--turbostat", "none"); err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	rows := csvRows(t, out)
	if rows[1][3] != "" || rows[2][3] != "73500000" || rows[3][3] != "" || rows[4][3] != "73500000" {
		t.Errorf("deltas %q %q %q %q, want empty, 73500000, empty (the gap), 73500000",
			rows[1][3], rows[2][3], rows[3][3], rows[4][3])
	}
}

// AN INVENTORY THAT COULD NOT BE TAKEN IS `?`, NOT EMPTY: an empty cell is what
// makes a row quiet, and a cgroup the logger cannot read may hold a microVM.
// Two that can be read are both listed.
func TestAnUnreadableInventoryIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	h := newPowerHarness(t, "sleep", "date", "timeout")
	h.plant(t, []step{
		{epoch: 10, uptimeCS: 1000, energy: 1_000_000, bmc: "fail", vms: []string{"billet-a", "billet-b"}},
		{epoch: 11, uptimeCS: 1100, energy: 74_500_000, bmc: "fail", vms: []string{"billet-a"}},
		{epoch: 12, uptimeCS: 1200, energy: 148_000_000, bmc: "fail"},
	})
	locked := filepath.Join(h.state, "cgroup", "firecracker-v1.16.1", "billet-c")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "power.csv")
	if summary, err := h.run(t, "--out", out, "--seconds", "2", "--ipmitool", "none", "--turbostat", "none"); err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	if err := os.Chmod(filepath.Join(locked, "cgroup.procs"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(locked, "cgroup.procs"), 0o644); err != nil {
			t.Logf("restore the mode: %v", err)
		}
	})
	rows := csvRows(t, out)
	if rows[1][6] != "billet-a;billet-b" || rows[2][6] != "billet-a" {
		t.Errorf("inventories %q and %q, want billet-a;billet-b and billet-a", rows[1][6], rows[2][6])
	}
	out2 := filepath.Join(t.TempDir(), "power.csv")
	if summary, err := h.run(t, "--out", out2, "--seconds", "2", "--ipmitool", "none", "--turbostat", "none"); err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	for _, row := range csvRows(t, out2)[1:] {
		if row[6] != "?" {
			t.Errorf("an unreadable cgroup.procs gave the inventory %q, want ?", row[6])
		}
	}
}

func csvRows(t *testing.T, path string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(body), "\n"), "\n") {
		rows = append(rows, strings.Split(line, ","))
	}

	return rows
}

// TURBOSTAT RUNS BESIDE THE LOG AND DIES WITH IT: each new PkgWatt reaches the
// row after it, a turbostat that has stopped printing is not read again as if
// it had, and no turbostat outlives the script that started it.
func TestTurbostatRunsBesideTheLogAndDiesWithIt(t *testing.T) {
	t.Parallel()
	h := newPowerHarness(t, "sleep", "date", "timeout", "turbostat")
	steps := referenceLog()[:6]
	h.plant(t, steps)
	if err := os.WriteFile(filepath.Join(h.state, "ts-readings"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "power.csv")
	summary, err := h.run(t, "--out", out, "--seconds", "6", "--ipmitool", "none", "--turbostat", "turbostat")
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	// Row 1 precedes any reading; rows 2 and 3 each follow one; rows 4 to 6
	// follow none, so their cells are empty rather than the last reading again.
	var cells []string
	for _, row := range csvRows(t, out)[1:] {
		cells = append(cells, row[5])
	}
	if got := strings.Join(cells, ","); got != ",71.50,72.50,,," {
		t.Errorf("turbostat cells %q, want \",71.50,72.50,,,\"", got)
	}
	if !strings.Contains(summary, "turbostat: 2 paired readings, mean PkgWatt 72.0 W") {
		t.Errorf("the summary:\n%s", summary)
	}
	body, err := os.ReadFile(filepath.Join(h.state, "ts-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Logf("kill %d: %v", pid, err)
			}
			t.Fatalf("turbostat (pid %d) outlived the log", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WHAT THE LOGGER CANNOT DO IT REFUSES BEFORE WRITING A ROW.
func TestThePowerLogRefusesWhatItCannotMeasure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fakes []string
		plant bool
		args  []string
		says  string
	}{
		{"an unreadable zone", []string{"sleep", "date", "timeout"}, false, nil, "cannot read"},
		{"a log that exists", []string{"sleep", "date", "timeout"}, true, []string{"--exists"}, "is never appended to"},
		{"--require-bmc with no BMC", []string{"sleep", "date", "timeout"}, true,
			[]string{"--require-bmc", "--ipmitool", "none"}, "--require-bmc and no ipmitool"},
		{"no timeout to bound ipmitool", []string{"sleep", "date"}, true, nil, "timeout(1) is required"},
		{"a fractional interval", []string{"sleep", "date", "timeout"}, true, []string{"--interval", "0.5"},
			"--interval must be a positive whole number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newPowerHarness(t, tc.fakes...)
			if tc.plant {
				h.plant(t, referenceLog()[:3])
			}
			out := filepath.Join(t.TempDir(), "power.csv")
			args := []string{"--out", out, "--seconds", "3", "--turbostat", "none"}
			for _, a := range tc.args {
				if a == "--exists" {
					if err := os.WriteFile(out, []byte("old\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				args = append(args, a)
			}
			if !slices.Contains(args, "--ipmitool") {
				args = append(args, "--ipmitool", "none")
			}
			text, err := h.run(t, args...)
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exit.ExitCode() != 2 || !strings.Contains(text, tc.says) {
				t.Errorf("err %v, want exit 2 saying %q:\n%s", err, tc.says, text)
			}
			if tc.name != "a log that exists" {
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Errorf("a log was written (stat: %v)", err)
				}
			}
		})
	}
}

// THE SUMMARY'S VERDICT IS ITS EXIT STATUS: a BMC that never answered is
// could-not-tell when the BMC was the point, and a log missing a column is
// refused rather than summarized as zeros.
func TestThePowerSummaryAnswersThreeWays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	noBMC := "epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances\n" +
		"1,10.00,0,,,,\n2,11.00,73500000,73500000,,,\n3,12.00,147000000,73500000,,,\n"
	noColumn := "epoch_s,uptime_s,rapl_uj,bmc_watts,turbostat_pkg_watts,instances\n1,10.00,0,,,\n"
	noRAPL := "epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances\n1,10.00,,,200,,\n"
	for _, tc := range []struct {
		name, log string
		args      []string
		code      int
		says      string
	}{
		{"RAPL without a BMC", noBMC, nil, 0, "BMC: no reading paired with RAPL (could not tell)"},
		{"a BMC that was required", noBMC, []string{"--require-bmc"}, 3, "BMC: no reading paired"},
		{"a window", noBMC, []string{"--from", "3", "--to", "3"}, 0, "rows 1 (epoch 3 to 3), RAPL intervals 1"},
		{"no RAPL at all", noRAPL, nil, 3, "RAPL: no interval could be measured"},
		{"a missing column", noColumn, nil, 1, "no rapl_delta_uj column"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".csv")
			if err := os.WriteFile(path, []byte(tc.log), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", append(append([]string{"power-summary.sh"}, tc.args...), path)...)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			err := cmd.Run()
			code := 0
			if exit, ok := errors.AsType[*exec.ExitError](err); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.code || !strings.Contains(out.String(), tc.says) {
				t.Errorf("exit %d, want %d saying %q:\n%s", code, tc.code, tc.says, out.String())
			}
		})
	}
}
