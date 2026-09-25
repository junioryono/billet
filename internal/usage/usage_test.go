package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Captured from one 8-vCPU microVM on the reference host (ubuntu-01,
// 2026-09-25 22:5x UTC, read only): its cgroup's files, its VMM's threads and
// its tap, with the host's /proc/stat and RAPL counter read ten seconds apart.
const (
	refCPUStat = `usage_usec 62338613
user_usec 35884530
system_usec 26454083
nice_usec 0
core_sched.force_idle_usec 0
nr_periods 0
nr_throttled 0
throttled_usec 0
nr_bursts 0
burst_usec 0
`
	refCPUPressure    = "some avg10=0.00 avg60=0.00 avg300=0.00 total=44618\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=44480\n"
	refMemoryPressure = "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	refIOPressure     = "some avg10=0.22 avg60=1.05 avg300=0.37 total=1498421\nfull avg10=0.22 avg60=1.05 avg300=0.37 total=1497869\n"
	refProcStatBefore = "cpu  728198716 39636 158011089 37269636514 62997159 0 4781646 0 688783983 927\ncpu0 7718324 1052 1398966 282654939 284217 0 93100 0 7489700 47\n"
	refProcStatAfter  = "cpu  728214069 39636 158013305 37269744359 62999128 0 4781732 0 688798719 927\ncpu0 7718324 1052 1398966 282654939 284217 0 93100 0 7489700 47\n"
)

// refThreads are the VMM's /proc/<pid>/task/<tid>/stat lines.
var refThreads = map[string]string{
	"315359": "315359 (firecracker-v1.) S 1 315357 315357 0 -1 4194560 81617 0 0 0 18 180 0 0 20 0 11 0 298947558 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 17 2 0 0 0 0 0 0 0 0 0 0 0 0 0",
	"315360": "315360 (fc_api) S 1 315357 315357 0 -1 4194368 57 0 0 0 0 0 0 0 20 0 11 0 298947558 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 45 0 0 0 0 0 0 0 0 0 0 0 0 0",
	"315371": "315371 (fc_vcpu 0) S 1 315357 315357 0 -1 4194368 112468 0 0 0 595 439 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 33 0 0 0 587 0 0 0 0 0 0 0 0 0",
	"315372": "315372 (fc_vcpu 1) S 1 315357 315357 0 -1 4194368 126260 0 0 0 581 353 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 64 0 0 0 580 0 0 0 0 0 0 0 0 0",
	"315373": "315373 (fc_vcpu 2) S 1 315357 315357 0 -1 4194368 30478 0 0 0 483 273 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 4 0 0 0 466 0 0 0 0 0 0 0 0 0",
	"315374": "315374 (fc_vcpu 3) S 1 315357 315357 0 -1 4194368 62269 0 0 0 459 219 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 0 0 0 0 458 0 0 0 0 0 0 0 0 0",
	"315375": "315375 (fc_vcpu 4) S 1 315357 315357 0 -1 4194368 53253 0 0 0 466 338 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 103 0 0 0 463 0 0 0 0 0 0 0 0 0",
	"315376": "315376 (fc_vcpu 5) S 1 315357 315357 0 -1 4194368 65688 0 0 0 285 226 0 0 20 0 11 0 298947565 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 97 0 0 0 279 0 0 0 0 0 0 0 0 0",
	"315377": "315377 (fc_vcpu 6) S 1 315357 315357 0 -1 4194368 47074 0 0 0 322 264 0 0 20 0 11 0 298947566 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 97 0 0 0 320 0 0 0 0 0 0 0 0 0",
	"315378": "315378 (fc_vcpu 7) S 1 315357 315357 0 -1 4194368 89455 0 0 0 379 355 0 0 20 0 11 0 298947566 34383421440 657152 18446744073709551615 1 1 0 0 0 0 0 0 1098912841 0 0 0 -1 116 0 0 0 381 0 0 0 0 0 0 0 0 0",
	"315379": "315379 (kvm-nx-lpage-re) S 1 315357 315357 0 -1 4210752 0 0 0 0 0 0 0 0 20 0 11 0 298947566 34383421440 657152 18446744073709551615 1 1 0 0 0 0 2147221247 0 1098912841 0 0 0 -1 9 0 0 0 0 0 0 0 0 0 0 0 0 0",
}

const refCgroup = "/sys/fs/cgroup/firecracker-v1.16.1/billet-b9eb5fb98c62f06fcc85357ded87995b"

type tree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) tree { return tree{t: t, root: t.TempDir()} }

func (tr tree) write(path, body string) {
	tr.t.Helper()
	full := filepath.Join(tr.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		tr.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		tr.t.Fatalf("write %s: %v", path, err)
	}
}

func (tr tree) remove(path string) {
	tr.t.Helper()
	if err := os.Remove(filepath.Join(tr.root, path)); err != nil {
		tr.t.Fatalf("remove %s: %v", path, err)
	}
}

// referenceVM lays the captured microVM out under a fixture root. The memory
// and io files are written as the new controllers would produce them, since
// the reference host's parent cgroup did not have them enabled.
func referenceVM(t *testing.T) (tree, Target) {
	t.Helper()

	tr := newTree(t)
	tr.write(refCgroup+"/cpu.stat", refCPUStat)
	tr.write(refCgroup+"/cpu.pressure", refCPUPressure)
	tr.write(refCgroup+"/memory.pressure", refMemoryPressure)
	tr.write(refCgroup+"/io.pressure", refIOPressure)
	tr.write(refCgroup+"/memory.current", "8589934592\n")
	tr.write(refCgroup+"/memory.peak", "9663676416\n")
	tr.write(refCgroup+"/memory.events", "low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\noom_group_kill 0\n")
	tr.write(refCgroup+"/io.stat", "251:16 rbytes=1048576 wbytes=4194304 rios=10 wios=40 dbytes=0 dios=0\n"+
		"251:32 rbytes=2097152 wbytes=0 rios=5 wios=0 dbytes=0 dios=0\n")
	for tid, stat := range refThreads {
		tr.write("/proc/315359/task/"+tid+"/stat", stat+"\n")
	}
	// bt-16, the VM's tap: rx=941790 tx=202408832 rxp=9276 txp=12866.
	for name, v := range map[string]string{"rx_bytes": "941790", "tx_bytes": "202408832",
		"rx_packets": "9276", "tx_packets": "12866"} {
		tr.write("/sys/class/net/bt-16/statistics/"+name, v+"\n")
	}

	return tr, Target{CgroupDir: refCgroup, PID: 315359, VCPUThreadPrefix: "fc_vcpu",
		NetDevice: "bt-16", NetHostView: true}
}

// The VMM's threads account for the cgroup's CPU time to within a tick, and
// the vCPU threads are 96.8% of it. Measured on the reference host: the
// threads summed to 62.35 s while the cgroup read 62.34 s.
func TestTheReferenceVMReadsAsItWasMeasured(t *testing.T) {
	tr, target := referenceVM(t)
	s := Reader{Root: tr.root}.Read(target)

	if !s.CPUOK || s.CPUUsage != 62_338_613 || s.CPUUser != 35_884_530 || s.CPUSys != 26_454_083 {
		t.Errorf("cpu = %+v", s)
	}
	if !s.ThreadsOK || s.GuestCPU != 60_370_000 || s.VMMCPU != 1_980_000 {
		t.Errorf("threads: guest %d µs, vmm %d µs, want 60370000 and 1980000", s.GuestCPU, s.VMMCPU)
	}
	if d := s.GuestCPU + s.VMMCPU - s.CPUUsage; d < 0 || d > 10_000*11 {
		t.Errorf("threads and cgroup disagree by %d µs, more than a tick per thread", d)
	}
	// THE TAP IS THE HOST'S VIEW: the host sent 202 MB into the guest.
	if !s.NetOK || s.NetRx != 202_408_832 || s.NetTx != 941_790 || s.NetRxPackets != 12_866 || s.NetTxPackets != 9_276 {
		t.Errorf("net = rx %d tx %d (packets %d/%d), want the guest's view rx 202408832 tx 941790",
			s.NetRx, s.NetTx, s.NetRxPackets, s.NetTxPackets)
	}
	if !s.PressureOK || s.CPUSome != 44_618 || s.CPUFull != 44_480 || s.IOSome != 1_498_421 || s.MemoryFull != 0 {
		t.Errorf("pressure = %+v", s)
	}
	if !s.MemoryOK || s.MemoryCurrent != 8<<30 || s.MemoryPeak != 9<<30 || s.OOMKills != 1 {
		t.Errorf("memory = current %d peak %d oom %d", s.MemoryCurrent, s.MemoryPeak, s.OOMKills)
	}
	if !s.IOOK || s.DiskRead != 3<<20 || s.DiskWrite != 4<<20 {
		t.Errorf("io = read %d write %d, want the sum over both devices", s.DiskRead, s.DiskWrite)
	}
}

// EACH GROUP ANSWERS FOR ITSELF. The reference host's jobs had no memory or io
// controller, and that must read as unmeasured, not as zero bytes, while CPU
// time is still reported.
func TestAMissingControllerIsUnmeasuredNotZero(t *testing.T) {
	tr, target := referenceVM(t)
	for _, f := range []string{"memory.current", "memory.peak", "memory.events", "io.stat"} {
		tr.remove(refCgroup + "/" + f)
	}
	s := Reader{Root: tr.root}.Read(target)
	if s.MemoryOK || s.IOOK {
		t.Errorf("memory ok %v io ok %v, want both unmeasured", s.MemoryOK, s.IOOK)
	}
	if !s.CPUOK || !s.ThreadsOK || !s.NetOK || !s.PressureOK {
		t.Errorf("a missing controller took other groups with it: %+v", s)
	}

	// A process that is not the VMM (no vCPU thread) is not split.
	target.VCPUThreadPrefix = "no_such_thread"
	if s := (Reader{Root: tr.root}).Read(target); s.ThreadsOK {
		t.Error("a pid with no vCPU threads was split into guest and VMM time")
	}
}

func TestParsersRefuseWhatTheyCannotRead(t *testing.T) {
	if _, err := parseFlatKeyed("usage_usec\n"); err == nil {
		t.Error("a key with no value parsed")
	}
	if _, _, err := parsePressure("full avg10=0 total=5\n"); err == nil {
		t.Error("a pressure file with no some line parsed")
	}
	if _, _, err := parsePressure("some avg10=0.00\n"); err == nil {
		t.Error("a pressure line with no total parsed")
	}
	if _, _, _, err := parseTaskStat("315371 fc_vcpu 0 S 1"); err == nil {
		t.Error("a task stat with no (comm) parsed")
	}
	comm, utime, stime, err := parseTaskStat(refThreads["315371"])
	if err != nil || comm != "fc_vcpu 0" || utime != 595 || stime != 439 {
		t.Errorf("fc_vcpu 0 = %q %d %d %v, want the name with its space and 595/439", comm, utime, stime, err)
	}
	// A name with a parenthesis in it is cut at the LAST one.
	comm, utime, _, err = parseTaskStat("7 (a) b) S 1 1 1 0 -1 0 0 0 0 0 42 7 0")
	if err != nil || comm != "a) b" || utime != 42 {
		t.Errorf("a name holding ')' = %q %d %v", comm, utime, err)
	}
}

// The host was 13.9% busy over the ten seconds, across 128 CPUs.
func TestTheHostsCPUIsReadAsBusyAndTotal(t *testing.T) {
	before, totalBefore, cpus, err := parseHostCPU(refProcStatBefore + strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	if err != nil || cpus != 128 {
		t.Fatalf("cpus = %d, %v", cpus, err)
	}
	after, totalAfter, _, err := parseHostCPU(refProcStatAfter)
	if err != nil {
		t.Fatal(err)
	}
	if busy, total := after-before, totalAfter-totalBefore; busy != 17_655 || total != 127_469 {
		t.Errorf("busy %d of %d ticks, want 17655 of 127469", busy, total)
	}
}

// THE COUNTER WRAPS at max_energy_range_uj (65,532,610,987 on the reference
// host), and a delta across the wrap is the energy, not a negative number.
func TestEnergyAcrossTheWrapIsTheEnergy(t *testing.T) {
	const maxRange = 65_532_610_987
	if d := energyDelta(38_419_855_704, 39_826_460_727, maxRange); d != 1_406_605_023 {
		t.Errorf("reference delta = %d µJ, want 1406605023 (140.7 W over 10 s)", d)
	}
	if d := energyDelta(maxRange-1_000, 4_000, maxRange); d != 5_000 {
		t.Errorf("delta across the wrap = %d, want 5000", d)
	}
}

func TestASeriesRoundTripsAndKeepsItsTotalsWhenCut(t *testing.T) {
	var points []Point
	for i := range int64(5000) {
		points = append(points, Point{OffsetMillis: i * 1000, CPUUsage: i * 700_000,
			MemoryCurrent: 1<<30 + (i%7)*4096, NetRx: i * i, EnergyActive: i * 3_000_000})
	}

	data, stride, err := EncodeSeries(points, 1<<20)
	if err != nil || stride != 1 {
		t.Fatalf("EncodeSeries = stride %d, %v", stride, err)
	}
	got, err := DecodeSeries(data)
	if err != nil || len(got) != len(points) || got[4321] != points[4321] {
		t.Fatalf("round trip lost points: %d of %d, %v", len(got), len(points), err)
	}

	small, stride, err := EncodeSeries(points, 2048)
	if err != nil || stride < 2 || len(small) > 2048 {
		t.Fatalf("a series cut to 2 KiB = %d bytes at stride %d, %v", len(small), stride, err)
	}
	cut, err := DecodeSeries(small)
	if err != nil {
		t.Fatal(err)
	}
	if last := cut[len(cut)-1]; last != points[len(points)-1] {
		t.Errorf("the cut series ends at %+v, want the last point %+v", last, points[len(points)-1])
	}
	if _, err := DecodeSeries([]byte("not a series")); err == nil {
		t.Error("garbage decoded")
	}
}

// ENERGY IS SHARED BY CPU TIME, and the idle baseline by reserved vCPUs.
//
// Over the reference interval the host was busy 176.55 s of CPU and this VM
// used 7.23 s of it, so it is charged 4.1% of the package energy above the
// baseline; with a 73.5 W baseline on 128 CPUs, an 8-vCPU job also holds 1/16
// of 735 J of idle.
func TestAJobsEnergyIsItsShareOfTheHostsBusyTime(t *testing.T) {
	tr, target := referenceVM(t)
	tr.write("/proc/stat", refProcStatBefore+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	tr.write(raplZone+"/max_energy_range_uj", "65532610987\n")
	tr.write(raplZone+"/energy_uj", "38419855704\n")

	now := time.Date(2026, 9, 25, 22, 50, 0, 0, time.UTC)
	m := NewMonitor(tr.root, Options{Interval: 10 * time.Second, RAPL: true, IdleWatts: 73.5,
		Now: func() time.Time { return now }})
	m.Tick()
	m.Start("vm", target, 8)

	now = now.Add(10 * time.Second)
	tr.write("/proc/stat", refProcStatAfter+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	tr.write(raplZone+"/energy_uj", "39826460727\n")
	tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 69571715", 1))
	m.Tick()

	s, ok := m.Final("vm")
	if !ok || !s.Measured.Energy || !s.EnergySplit {
		t.Fatalf("energy measured %v split %v", s.Measured.Energy, s.EnergySplit)
	}
	// active = 1406.605 J - 735 J = 671.605 J; share = 7.233102 s / 176.55 s.
	wantActive := (1_406_605_023.0 - 735_000_000.0) * 7_233_102.0 / 176_550_000.0
	if d := float64(s.EnergyActive) - wantActive; d < -1 || d > 1 {
		t.Errorf("active energy %d µJ, want %.0f", s.EnergyActive, wantActive)
	}
	if s.EnergyIdle != 735_000_000/16 {
		t.Errorf("idle energy %d µJ, want %d", s.EnergyIdle, 735_000_000/16)
	}

	// A gap long enough for the counter to wrap unseen (65.5 s at 1 kW) makes
	// the job's energy could-not-tell, never a small number. Everything else
	// about the interval is ordinary, so the gap is the only reason.
	now = now.Add(2 * time.Minute)
	tr.write("/proc/stat", "cpu  728314069 39636 158013305 37269744359 62999128 0 4781732 0 688798719 927\n"+
		strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	tr.write(raplZone+"/energy_uj", "40826460727\n")
	tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613", "usage_usec 79571715", 1))
	m.Tick()
	if s, _ := m.Final("vm"); s.Measured.Energy {
		t.Error("energy across a gap the counter could have wrapped in was reported as measured")
	}
}

// A job whose cgroup vanished mid-life keeps what it measured before, and a
// monitor with RAPL off never claims energy.
func TestALostReadKeepsTheLastMeasurement(t *testing.T) {
	tr, target := referenceVM(t)
	now := time.Now()
	m := NewMonitor(tr.root, Options{Interval: time.Second, Now: func() time.Time { return now }})
	m.Start("vm", target, 8)
	tr.remove(refCgroup + "/cpu.stat")
	now = now.Add(time.Second)
	m.Tick()

	s, ok := m.Final("vm")
	if !ok || !s.Measured.CPU || s.Latest.CPUUsage != 62_338_613 {
		t.Errorf("cpu measured %v usage %d, want the last reading kept", s.Measured.CPU, s.Latest.CPUUsage)
	}
	if s.Measured.Energy {
		t.Error("energy measured with RAPL off")
	}
	if s.MemoryPeak != 9<<30 || s.Samples != 3 {
		t.Errorf("peak %d samples %d", s.MemoryPeak, s.Samples)
	}

	m.Forget("vm")
	if _, ok := m.Final("vm"); ok {
		t.Error("a forgotten job was still summarised")
	}
}
