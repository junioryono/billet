package scripts_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
case "$energy" in
cut:*) printf '%s' "${energy#cut:}" >"$s/powercap/energy_uj" ;;
*) printf '%s\n' "$energy" >"$s/powercap/energy_uj" ;;
esac
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
	// meter runs the Nth line of $FAKE_STATE/meter-answers on its Nth call: a
	// line of shell, so an answer can be any bytes, any exit status, or late.
	// No mapfile: /bin/bash is 3.2 on macOS.
	"meter": `#!/bin/bash
s=$FAKE_STATE
n=$(( $(cat "$s/meter-calls" 2>/dev/null || echo 0) + 1 ))
printf '%s\n' "$n" >"$s/meter-calls"
i=0
while IFS= read -r line; do
	i=$((i + 1))
	if [ "$i" -eq "$n" ]; then
		eval "$line"
		exit
	fi
done <"$s/meter-answers"
echo "fake meter: no answer $n" >&2
exit 90
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
	// cut writes the counter with no newline, as a read cut short leaves it.
	cut bool
}

func (s step) line() string {
	vms := "-"
	if len(s.vms) > 0 {
		vms = strings.Join(s.vms, ",")
	}
	energy := strconv.FormatInt(s.energy, 10)
	if s.cut {
		energy = "cut:" + energy
	}

	return fmt.Sprintf("%d %d.%02d %s %s %s", s.epoch, s.uptimeCS/100, s.uptimeCS%100, energy, s.bmc, vms)
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

// wantLog is the log the script must write for steps without a meter, header
// first, built independently of the script; bmc says whether the BMC was asked.
func wantLog(steps []step, bmc bool) []string {
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
		watts := s.bmc
		if !bmc || watts == "fail" || watts == "off" {
			watts = ""
		}
		want = append(want, fmt.Sprintf("%d,%d.%02d,%d,%s,%s,,%s", s.epoch, s.uptimeCS/100, s.uptimeCS%100,
			s.energy, delta, watts, strings.Join(s.vms, ";")))
	}

	return want
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

	want := wantLog(steps, true)
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

// A COUNTER READ CUT SHORT IS NO READING: `read` hands back what it got before
// an end of file with no newline, and "123" after 65 kJ would be a wrap.
func TestACounterReadCutShortIsNoReading(t *testing.T) {
	t.Parallel()
	h := newPowerHarness(t, "sleep", "date", "timeout")
	h.plant(t, []step{
		{epoch: 10, uptimeCS: 1000, energy: 65_000_000_000, bmc: "fail"},
		{epoch: 11, uptimeCS: 1100, energy: 123, bmc: "fail", cut: true},
		{epoch: 12, uptimeCS: 1200, energy: 65_147_000_000, bmc: "fail"},
		{epoch: 13, uptimeCS: 1300, energy: 65_220_500_000, bmc: "fail"},
	})
	out := filepath.Join(t.TempDir(), "power.csv")
	if summary, err := h.run(t, "--out", out, "--seconds", "4", "--ipmitool", "none", "--turbostat", "none"); err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	rows := csvRows(t, out)
	if rows[2][2] != "" || rows[2][3] != "" || rows[3][3] != "" || rows[4][3] != "73500000" {
		t.Errorf("counter %q delta %q, then deltas %q %q; want the cut reading empty, no delta across it, "+
			"then 73500000", rows[2][2], rows[2][3], rows[3][3], rows[4][3])
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
		{"no timeout to bound ipmitool", []string{"sleep", "date"}, true, nil,
			"power-log: timeout(1) is required to bound ipmitool\n"},
		{"no timeout to bound the meter", []string{"sleep", "date"}, true, []string{"--meter", "true"},
			"power-log: timeout(1) is required to bound ipmitool and the meter\n"},
		{"a fractional interval", []string{"sleep", "date", "timeout"}, true, []string{"--interval", "0.5"},
			"--interval must be a positive whole number"},
		// GNU timeout reads a duration of zero as no bound at all.
		{"a meter bound of zero", []string{"sleep", "date", "timeout"}, true,
			[]string{"--meter", "true", "--meter-timeout", "0.0"}, "--meter-timeout must be a positive number"},
		{"a meter bound with a unit", []string{"sleep", "date", "timeout"}, true,
			[]string{"--meter", "true", "--meter-timeout", "1s"}, "--meter-timeout must be a positive number"},
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

// runSummary runs power-summary.sh and answers its output and exit status.
func runSummary(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"power-summary.sh"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}

	return out.String(), 0
}

// A LOG WITHOUT A METER IS SUMMARIZED AS IT ALWAYS WAS. These goldens were
// written by power-summary.sh before it read a meter column, and are compared
// byte for byte with the exit status.
func TestThePowerSummaryWithoutAMeterIsUnchanged(t *testing.T) {
	t.Parallel()
	turbostat := filepath.Join(t.TempDir(), "turbostat.csv")
	if err := os.WriteFile(turbostat, []byte("epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances\n"+
		"1,10.00,0,,,,\n2,11.00,73500000,73500000,140,73.10,\n3,12.00,,,141,,\n4,13.50,300000000,226500000,,150.25,billet-a\n"+
		"5,14.50,400000000,100000000,160,99.5,billet-a\n6,15.50,500000000,100000000,161,100.5,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A package that drew nothing over the BMC's rows: main prints its ratio as
	// 0.000, and so does this.
	zero := filepath.Join(t.TempDir(), "zero.csv")
	if err := os.WriteFile(zero, []byte("epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances\n"+
		"1,10.00,0,,150,,\n2,11.00,0,0,151,,\n3,12.00,0,0,152,,\n4,13.00,0,0,153,,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		golden string
		args   []string
		code   int
	}{
		{"zero-rapl.txt", []string{zero}, 0},
		{"reference.txt", []string{"--require-bmc", powerLogFixture}, 0},
		{"reference-window.txt", []string{"--from", "1791540010", "--to", "1791540025", powerLogFixture}, 0},
		{"turbostat.txt", []string{turbostat}, 0},
		{"one-bmc-reading.txt", []string{"--require-bmc", "--to", "3", turbostat}, 0},
	} {
		out, code := runSummary(t, tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d:\n%s", tc.golden, code, tc.code, out)
		}
		golden := filepath.Join("testdata", "power-summary", tc.golden)
		if os.Getenv("BILLET_UPDATE_FIXTURES") == "1" {
			if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if out != string(want) {
			t.Errorf("the summary:\n%s\nwant %s:\n%s", out, golden, want)
		}
	}
}

// meterAnswer is what the fake meter does on one sample, and the two cells the
// log must hold for it.
type meterAnswer struct{ shell, watts, wh string }

func (h powerHarness) plantMeter(t *testing.T, answers []meterAnswer) {
	t.Helper()
	var b strings.Builder
	for _, a := range answers {
		b.WriteString(a.shell + "\n")
	}
	if err := os.WriteFile(filepath.Join(h.state, "meter-answers"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// THE METER IS A READING OR AN EMPTY CELL, NEVER A GUESS: an answer is one line
// of a decimal watts and an optional decimal watt-hour counter, and anything
// else the command does leaves both cells empty. The seven columns before them
// are what the log holds without a meter.
func TestTheMeterIsAReadingOrAnEmptyCell(t *testing.T) {
	t.Parallel()
	answers := []meterAnswer{
		{`echo 133.5 1000.000`, "133.5", "1000.000"},
		{`echo 134`, "134", ""},
		{`echo 135.25 1000.100; exit 1`, "", ""},
		{`echo 0`, "0", ""},
		{`echo abc`, "", ""},
		{`echo -5`, "", ""},
		{`printf '136\n137\n'`, "", ""},
		{`echo 136 1000.2 extra`, "", ""},
		{`printf '136\0'`, "", ""},
		{`echo 1e3`, "", ""},
		{`echo 1.1234567`, "", ""},
		{`echo 136 1e3`, "", ""},
		{`printf '136\r\n'`, "", ""},
		{`:`, "", ""},
		{`printf '%120s137\n' ''`, "137", ""},
		{`printf '%130s137\n' ''`, "", ""},
		{`printf '137.5'`, "137.5", ""},
		{`printf ' 138\t1000.5 \n'`, "138", "1000.5"},
		{`echo 999999.999999 1000.75`, "999999.999999", "1000.75"},
		{`echo 1000000`, "", ""},
		{`echo 139 1000.1234567`, "", ""},
		{`echo 139 .5`, "", ""},
		{`echo 139.`, "", ""},
		{`echo 139 1000000000`, "", ""},
		{`echo 139 999999999.999999`, "139", "999999999.999999"},
		// THE LOG'S DESCRIPTOR IS NOT THE METER'S to write rows into.
		{`echo 0,0,0 >&3; echo 141`, "141", ""},
		// NO FILE THE METER WRITES GROWS PAST 64 KiB: the subshell writing
		// 100000 bytes is stopped there, and the meter still answers. It ignores
		// SIGXFSZ, which macOS's crash reporter takes seconds over, and sees the
		// write fail instead.
		{`(trap '' XFSZ; printf '%100000s' '' >"$FAKE_STATE/big") 2>/dev/null; echo 150`, "150", ""},
		{`echo 140 1000.9`, "140", "1000.9"},
	}
	h := newPowerHarness(t, "sleep", "date", "timeout", "meter")
	steps := referenceLog()[:len(answers)]
	h.plant(t, steps)
	h.plantMeter(t, answers)
	out := filepath.Join(t.TempDir(), "power.csv")
	summary, err := h.run(t, "--out", out, "--seconds", strconv.Itoa(len(steps)), "--ipmitool", "none",
		"--turbostat", "none", "--meter", "meter")
	if err != nil {
		t.Fatalf("%v\n%s", err, summary)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := wantLog(steps, false)
	want[0] += ",meter_watts,meter_wh"
	used, skipped := 0, 0
	for i, a := range answers {
		want[i+1] += "," + a.watts + "," + a.wh
		switch {
		case i == 0:
		case a.watts == "":
			skipped++
		default:
			used++
		}
	}
	if got := strings.TrimSuffix(string(written), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("the log:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if big, err := os.Stat(filepath.Join(h.state, "big")); err != nil || big.Size() != 64<<10 {
		t.Errorf("the meter's 100000-byte file: %v, want it stopped at 64 KiB (stat: %v)", big, err)
	}
	// THE LOGGER'S OWN SUMMARY READS THE METER, and the counter's reset on the
	// last row sends the energy to the integrated watts.
	for _, line := range []string{
		fmt.Sprintf("meter fit: %d rows used, %d skipped for an empty meter cell, 0 for no RAPL interval", used, skipped),
		fmt.Sprintf("its counter went backwards at epoch %d, a reset", steps[len(steps)-1].epoch),
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("the summary does not say %q:\n%s", line, summary)
		}
	}
}

// A METER THAT DOES NOT ANSWER IN TIME NEVER HOLDS THE LOG: one that sleeps,
// one that ignores SIGTERM, and one whose child keeps its stdout open each
// leave the log moving, and the late ones leave empty cells. The child writes
// once the next sample's meter has started, and that meter answers once it has,
// so the write lands in the next answer were it the same file.
func TestASlowMeterNeverHoldsTheLog(t *testing.T) {
	t.Parallel()
	// GNU TIMEOUT IS THE BOUND, and macOS's coreutils timeout has disagreed with
	// itself about exit statuses inside a script (guest_image_psmodule_test.go),
	// so this runs where the logger runs.
	if runtime.GOOS != "linux" {
		t.Skip("this asserts GNU timeout's bound under Linux process behaviour")
	}
	gnuTimeout, err := exec.LookPath("timeout")
	if err != nil {
		t.Skip("no timeout on PATH")
	}
	if version, err := exec.CommandContext(t.Context(), gnuTimeout, "--version").Output(); err != nil ||
		!strings.Contains(string(version), "GNU coreutils") {
		t.Skip("no GNU timeout on PATH")
	}
	h := newPowerHarness(t, "sleep", "date", "meter")
	if err := os.Symlink(gnuTimeout, filepath.Join(h.bin, "timeout")); err != nil {
		t.Fatal(err)
	}
	// NOTHING A METER STARTS OUTLIVES THE TEST, whether the run passed or a broken
	// bound left a meter running. A process is this test's when its environment
	// names this test's own FAKE_STATE, which no other process can, so a reused
	// pid or group is never signalled. The logger's own group is killed at the
	// deadline below.
	t.Cleanup(func() {
		mine := []byte("\x00FAKE_STATE=" + h.state + "\x00")
		entries, err := os.ReadDir("/proc")
		if err != nil {
			t.Logf("read /proc: %v", err)
			return
		}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
			if err != nil || !bytes.Contains(append([]byte{0}, env...), mine) {
				continue
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Logf("kill %d: %v", pid, err)
			}
		}
	})
	answers := []meterAnswer{
		{`/bin/sleep 30; echo 140`, "", ""},
		{`trap '' TERM; /bin/sleep 30; echo 141`, "", ""},
		{`(until [ -e "$FAKE_STATE/fourth" ]; do /bin/sleep 0.05; done; ` +
			`echo a late answer; : >"$FAKE_STATE/written"; exec /bin/sleep 30) & echo 142`, "142", ""},
		{`: >"$FAKE_STATE/fourth"; until [ -e "$FAKE_STATE/written" ]; do /bin/sleep 0.05; done; echo 143 2000`,
			"143", "2000"},
	}
	steps := referenceLog()[:len(answers)]
	h.plant(t, steps)
	h.plantMeter(t, answers)
	out := filepath.Join(t.TempDir(), "power.csv")
	// THE DEADLINE IS PAST WHAT A BOUNDED RUN TAKES (2 s, 3 s and two quick samples)
	// and short of what any one unbounded meter holds it for (30 s).
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(h.bin, "bash"), "power-log.sh", "--powercap",
		filepath.Join(h.state, "powercap"), "--proc", filepath.Join(h.state, "proc"), "--cgroup",
		filepath.Join(h.state, "cgroup"), "--out", out, "--seconds", strconv.Itoa(len(steps)), "--ipmitool", "none",
		"--turbostat", "none", "--meter", "meter", "--meter-timeout", "2")
	cmd.Env = []string{"PATH=" + h.bin, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "FAKE_STATE=" + h.state}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var text bytes.Buffer
	cmd.Stdout, cmd.Stderr = &text, &text
	if err := cmd.Run(); err != nil {
		t.Fatalf("the log did not finish inside 20 s (%v; context %v):\n%s", err, ctx.Err(), text.String())
	}
	var cells []string
	for _, row := range csvRows(t, out)[1:] {
		cells = append(cells, row[7]+"/"+row[8])
	}
	var wantCells []string
	for _, a := range answers {
		wantCells = append(wantCells, a.watts+"/"+a.wh)
	}
	if got, want := strings.Join(cells, " "), strings.Join(wantCells, " "); got != want {
		t.Errorf("meter cells %q, want %q", got, want)
	}
}

// THE SUMMARY FITS THE WALL AS IT FITS THE BMC, says what the fit used, and
// takes the meter's energy from its counter when the counter can be trusted.
func TestThePowerSummaryFitsTheWall(t *testing.T) {
	t.Parallel()
	// The wall is 60 W + 1.1 x RAPL on every row holding both; row 4 has no RAPL
	// interval, row 5 no meter reading, and row 6's interval is two seconds.
	rows := [][3]string{
		{"1,10.00,0,", "150.0", "1000.000"},
		{"2,11.00,0,100000000", "170", "1000.050"},
		{"3,12.00,0,200000000", "280", "1000.100"},
		{"4,13.00,,", "250", ""},
		{"5,14.00,0,150000000", "", "1000.200"},
		{"6,16.00,0,300000000", "225", "1000.300"},
		{"7,17.00,0,50000000", "115", "1000.350"},
	}
	write := func(name string, cell func(i int, r [3]string) [3]string, meterWH bool) string {
		var b strings.Builder
		b.WriteString("epoch_s,uptime_s,rapl_uj,rapl_delta_uj,bmc_watts,turbostat_pkg_watts,instances,meter_watts")
		if meterWH {
			b.WriteString(",meter_wh")
		}
		b.WriteString("\n")
		for i, r := range rows {
			if cell != nil {
				r = cell(i, r)
			}
			b.WriteString(r[0] + ",,,," + r[1])
			if meterWH {
				b.WriteString("," + r[2])
			}
			b.WriteString("\n")
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const fit = "meter: 4 paired readings, mean 197.5 W against RAPL 125.0 W over the same rows\n" +
		"meter/RAPL ratio 1.580, offset 72.5 W\n" +
		"fit: meter = 60.0 W + 1.100 x RAPL, r^2 1.000\n" +
		"meter fit: 4 rows used, 1 skipped for an empty meter cell, 1 for no RAPL interval\n"
	// Integrated: 170 + 280 + 250 + 225 x 2 + 115 J over the six intervals but row 5's.
	const integrated = "meter energy: 1.3 kJ over 6.0 s of the 7.0 s of intervals in the window, integrated from " +
		"its watts (5 of 6 intervals had a reading); "
	for _, tc := range []struct {
		name    string
		cell    func(int, [3]string) [3]string
		meterWH bool
		args    []string
		code    int
		says    []string
	}{
		{"a counter", nil, true, nil, 0, []string{fit,
			"meter energy: 1.3 kJ (0.350 Wh) over 7.0 s, from its cumulative counter (epoch 1 to 7)\n"}},
		{"no counter column", nil, false, nil, 0, []string{fit, integrated + "it gave no cumulative counter\n"}},
		{"no counter reading", func(_ int, r [3]string) [3]string { r[2] = ""; return r }, true, nil, 0,
			[]string{integrated + "it gave no cumulative counter\n"}},
		{"a reset counter", func(i int, r [3]string) [3]string {
			if i == 5 {
				r[2] = "0.100"
			}
			return r
		}, true, nil, 0, []string{integrated + "its counter went backwards at epoch 6, a reset\n"}},
		{"one counter reading", func(i int, r [3]string) [3]string {
			if i != 2 {
				r[2] = ""
			}
			return r
		}, true, nil, 0, []string{integrated + "its counter has no two readings an interval apart\n"}},
		// A RESET IS SEEN ON A ROW WITH NO UPTIME, which can be no end of a span
		// but is still a reading lower than the one before it.
		{"a reset on a row with no uptime", func(i int, r [3]string) [3]string {
			if i == 3 {
				r[0], r[2] = "4,,,", "0.050"
			}
			return r
		}, true, nil, 0, []string{"its counter went backwards at epoch 4, a reset\n"}},
		// A RATIO OVER NO RAPL POWER IS NO RATIO, not zero.
		{"RAPL that drew nothing", func(_ int, r [3]string) [3]string {
			f := strings.Split(r[0], ",")
			if f[3] != "" {
				f[3] = "0"
			}
			r[0] = strings.Join(f, ",")
			return r
		}, true, nil, 0, []string{"meter/RAPL ratio n/a (RAPL drew nothing), offset 197.5 W\n"}},
		// THE METER'S OWN FIGURES DO NOT NEED RAPL: a log with no RAPL interval
		// still says what the meter answered, and is could-not-tell.
		{"no RAPL at all", func(_ int, r [3]string) [3]string {
			f := strings.Split(r[0], ",")
			f[3] = ""
			r[0] = strings.Join(f, ",")
			return r
		}, true, nil, 3, []string{"RAPL: no interval could be measured (could not tell)\n" +
			"meter: no reading paired with RAPL (could not tell)\n" +
			"meter fit: 0 rows used, 0 skipped for an empty meter cell, 6 for no RAPL interval\n" +
			"meter energy: 1.3 kJ (0.350 Wh) over 7.0 s, from its cumulative counter (epoch 1 to 7)\n"}},
		// A COUNTER READING ON A ROW WITH NO UPTIME has no time to span.
		{"a counter reading with no uptime", func(i int, r [3]string) [3]string {
			if i == 6 {
				r[0] = "7,,0,50000000"
			}
			return r
		}, true, nil, 0, []string{"meter energy: 1.1 kJ (0.300 Wh) over 6.0 s, from its cumulative counter (epoch 1 to 6)\n"}},
		{"a window", nil, true, []string{"--from", "3", "--to", "6"}, 0, []string{
			"fit: n/a (fewer than three readings, or no variation)\n" +
				"meter fit: 2 rows used, 1 skipped for an empty meter cell, 1 for no RAPL interval\n",
			"meter energy: 0.7 kJ (0.200 Wh) over 4.0 s, from its cumulative counter (epoch 3 to 6)\n"}},
		{"a meter that never answered", func(_ int, r [3]string) [3]string { r[1], r[2] = "", ""; return r }, true,
			nil, 3, []string{"meter: no reading paired with RAPL (could not tell)\n" +
				"meter fit: 0 rows used, 5 skipped for an empty meter cell, 1 for no RAPL interval\n" +
				"meter energy: could not tell; no interval has a meter reading, and it gave no cumulative counter\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, code := runSummary(t, append(tc.args, write("power.csv", tc.cell, tc.meterWH))...)
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, s := range tc.says {
				if !strings.Contains(out, s) {
					t.Errorf("the summary does not say %q:\n%s", s, out)
				}
			}
		})
	}
}
