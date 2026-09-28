package usage

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// energyHost is the reference VM with a host that has RAPL and a clock the test
// moves, and the monitor already past its first tick with the job started.
type energyHost struct {
	tr     tree
	target Target
	now    *time.Time
	m      *Monitor
}

func newEnergyHost(t *testing.T, idleWatts float64) energyHost {
	t.Helper()

	tr, target := referenceVM(t)
	tr.write("/proc/stat", refProcStatBefore+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	tr.write(raplZone+"/max_energy_range_uj", "65532610987\n")
	tr.write(raplZone+"/energy_uj", "38419855704\n")
	now := time.Date(2026, 9, 25, 22, 50, 0, 0, time.UTC)
	h := energyHost{tr: tr, target: target, now: &now}
	h.m = runMonitor(t, tr.root, Options{Interval: 10 * time.Second, RAPL: true, IdleWatts: idleWatts,
		Now: func() time.Time { return *h.now }})
	h.m.Tick()

	return h
}

// advance moves the clock and the host's counters by one reference interval:
// 17,655 busy ticks, 1,406,605,023 µJ, and jobCPU µs for the job.
func (h energyHost) advance(d time.Duration, jobCPU string) {
	*h.now = h.now.Add(d)
	h.tr.write("/proc/stat", refProcStatAfter+strings.Repeat("cpu1 0 0 0 0 0 0 0 0 0 0\n", 127))
	h.tr.write(raplZone+"/energy_uj", "39826460727\n")
	h.tr.write(refCgroup+"/cpu.stat", strings.Replace(refCPUStat, "usage_usec 62338613",
		"usage_usec "+jobCPU, 1))
}

// finalOf is the summary of the one job every test here starts, "vm", which
// must still exist and still have its CPU measured.
func finalOf(t *testing.T, m *Monitor) Summary {
	t.Helper()

	s, ok := m.Final("vm")
	if !ok {
		t.Fatal("Final found no job")
	}
	if !s.Measured.CPU {
		t.Fatal("Final lost the CPU measurement too, so this proves nothing about energy")
	}

	return s
}

// A JOB STARTED HALFWAY THROUGH AN INTERVAL HOLDS IDLE FOR HALF OF IT, not for
// the part of the interval before it existed.
func TestIdleIsChargedOnlyForTheTimeAJobExisted(t *testing.T) {
	h := newEnergyHost(t, 73.5)
	*h.now = h.now.Add(5 * time.Second)
	h.m.Start("vm", h.target, 8)
	h.advance(5*time.Second, "69571715")
	h.m.Tick()

	// 735 J of idle in the interval, 8 of 128 CPUs, for 5 of its 10 seconds.
	if got, want := finalOf(t, h.m).EnergyIdle, int64(735_000_000/16/2); got != want {
		t.Errorf("idle = %d µJ, want %d", got, want)
	}
}

// A BASELINE ABOVE THE MEASURED DRAW NEVER HANDS OUT ENERGY THAT WAS NOT
// MEASURED: the idle pool is capped at what the package counter advanced, and a
// job holding every CPU is given exactly that, still measured.
func TestTheIdlePoolIsNeverMoreThanWasMeasured(t *testing.T) {
	h := newEnergyHost(t, 500) // 5,000 J over ten seconds; the counter moved 1,406.6 J
	h.m.Start("vm", h.target, 128)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()

	s := finalOf(t, h.m)
	if !s.Measured.Energy || s.EnergyIdle != 1_406_605_023 || s.EnergyActive != 0 {
		t.Errorf("a job holding every CPU got %d µJ idle and %d active (measured %v); want exactly "+
			"the 1406605023 the package moved, as idle", s.EnergyIdle, s.EnergyActive, s.Measured.Energy)
	}
}

// JOBS THAT RAN LONGER THAN THE HOST WAS BUSY ARE AN INCONSISTENT INTERVAL,
// could-not-tell, never clipped into a plausible share.
func TestAnImpossibleCPUDeltaIsNotAttributed(t *testing.T) {
	h := newEnergyHost(t, 0)
	h.m.Start("vm", h.target, 8)
	// 200 s of CPU in an interval where the whole host was busy 176.55 s.
	h.advance(10*time.Second, "262338613")
	h.m.Tick()

	if finalOf(t, h.m).Measured.Energy {
		t.Error("energy from an interval whose jobs outran the host was reported as measured")
	}
}

// A SAMPLER THAT STOPPED (a node shutting down while its jobs drain) has missed
// intervals, and a job's energy without them is not its energy.
func TestEnergyIsNotMeasuredAcrossIntervalsTheSamplerMissed(t *testing.T) {
	h := newEnergyHost(t, 0)
	h.m.Start("vm", h.target, 8)
	h.advance(10*time.Second, "69571715")
	h.m.Tick()
	if !finalOf(t, h.m).Measured.Energy {
		t.Fatal("a job sampled every interval has no energy, so this proves nothing")
	}

	*h.now = h.now.Add(time.Minute)
	if finalOf(t, h.m).Measured.Energy {
		t.Error("energy was reported as measured after six intervals with no tick")
	}
}

// A SECOND PACKAGE MAKES ENERGY COULD-NOT-TELL: package 0's energy shared by
// CPU time on every socket would charge one socket to jobs on another.
func TestASecondPackageRefusesEnergy(t *testing.T) {
	tr := newTree(t)
	tr.write(raplZone+"/max_energy_range_uj", "65532610987\n")
	tr.write(raplZone+"/energy_uj", "1\n")
	if _, err := (Reader{Root: tr.root}).ReadEnergy(); err != nil {
		t.Fatalf("one package: %v", err)
	}
	tr.write(secondPackage+"/energy_uj", "1\n")
	if _, err := (Reader{Root: tr.root}).ReadEnergy(); err == nil ||
		!strings.Contains(err.Error(), "more than one package") {
		t.Fatalf("two packages = %v, want refused", err)
	}
}

// A PID THE KERNEL HAS GIVEN TO ANOTHER PROCESS IS NOT READ AS THIS JOB'S, even
// a replacement VMM with the same vCPU thread names.
func TestAReusedPidIsNotReadAsTheJobsThreads(t *testing.T) {
	tr, target := referenceVM(t)
	h := runMonitor(t, tr.root, Options{Interval: time.Second})
	h.Start("vm", target, 8)

	reborn := strings.Replace(refThreads["315359"], " 298947558 ", " 398947558 ", 1)
	if reborn == refThreads["315359"] {
		t.Fatal("the start time was not found in the captured stat line")
	}
	tr.write("/proc/315359/stat", reborn+"\n")
	for tid, stat := range refThreads {
		tr.write("/proc/315359/task/"+tid+"/stat", strings.Replace(stat, " 595 439 ", " 99595 99439 ", 1)+"\n")
	}

	s := (Reader{Root: tr.root}).Read(target)
	if s.ThreadsOK || !s.CPUOK {
		t.Errorf("threads ok %v, cpu ok %v: want the reused pid refused and the cgroup still read",
			s.ThreadsOK, s.CPUOK)
	}
	if got := finalOf(t, h).Latest.GuestCPU; got != 60_370_000 {
		t.Errorf("guest time = %d µs, want the 60370000 read before the pid was reused", got)
	}

	target.PIDStart = 0
	tr.write("/proc/315359/stat", refThreads["315359"]+"\n")
	if (Reader{Root: tr.root}).Read(target).ThreadsOK {
		t.Error("threads were read for a target whose process start was never recorded")
	}
}

// stuckTarget is a copy of target whose cpu.stat is a FIFO: a read of it blocks
// until something opens the FIFO for writing.
func stuckTarget(t *testing.T, tr tree, target Target) (Target, string) {
	t.Helper()

	stuck := target
	stuck.CgroupDir = "/sys/fs/cgroup/stuck"
	if err := os.MkdirAll(filepath.Join(tr.root, stuck.CgroupDir), 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(tr.root, stuck.CgroupDir, "cpu.stat")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	return stuck, fifo
}

// waitForReader waits until something has the FIFO open for reading, which is
// when a non-blocking open for writing stops failing, and returns that writer
// held open so the reader stays blocked in read. The caller must close it.
func waitForReader(t *testing.T, fifo string) *os.File {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			return w
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing ever opened the fifo for reading")

	return nil
}

// finalWithin asks for key's final summary and fails unless it comes within
// about the limit.
func finalWithin(t *testing.T, m *Monitor, key string) Summary {
	t.Helper()

	type answer struct {
		s    Summary
		ok   bool
		took time.Duration
	}
	finished := make(chan answer, 1)
	go func() {
		begun := time.Now()
		s, ok := m.Final(key)
		finished <- answer{s, ok, time.Since(begun)}
	}()
	select {
	case a := <-finished:
		if !a.ok || a.took > testLimit+time.Second {
			t.Errorf("Final(%s) answered %v after %s, want an answer within about %s", key, a.ok,
				a.took, testLimit)
		}
		return a.s
	case <-time.After(10 * time.Second):
		t.Fatalf("Final(%s) waited on a stuck sampler", key)
	}

	return Summary{}
}

// A SAMPLER STUCK ON A READ HOLDS UP NO TEARDOWN FOR LONGER THAN THE LIMIT.
// First the sampler takes the stuck job's own Final and blocks reading it,
// proved blocked in the FIFO before anything is asserted; then another job's
// Final cannot reach the sampler at all. Both answer from what was already
// known, and the sampler is released and proved to finish.
func TestAStuckSamplerHoldsUpNoTeardownPastTheLimit(t *testing.T) {
	tr, target := referenceVM(t)
	m := runMonitor(t, tr.root, Options{Interval: time.Second})
	m.Start("vm", target, 8)
	fifo := stuckJob(t, tr, target, m)

	stuckFinal := make(chan Summary, 1)
	go func() { stuckFinal <- finalWithin(t, m, "stuck") }()
	writer := waitForReader(t, fifo)
	select {
	case <-stuckFinal:
	case <-time.After(10 * time.Second):
		t.Fatal("the stuck job's own Final never answered")
	}

	if s := finalWithin(t, m, "vm"); s.Latest.CPUUsage != 62_338_613 {
		t.Errorf("Final(vm) answered cpu %d, want the reading Start took", s.Latest.CPUUsage)
	}

	// Released, the sampler finishes the read it was stuck in and serves again.
	if _, err := writer.WriteString("usage_usec 1\nuser_usec 1\nsystem_usec 0\n"); err != nil {
		t.Errorf("write the fifo: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Errorf("close the fifo: %v", err)
	}
	m.Forget("stuck")
	served := make(chan struct{})
	go func() {
		defer close(served)
		m.Tick()
	}()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the released sampler never served again")
	}
}

func encodedRaw(t *testing.T, raw []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// A HOSTILE ROW IS REFUSED BEFORE IT COSTS MEMORY, AND WHATEVER DECODES IS A
// SERIES A MONITOR COULD HAVE WRITTEN.
func TestAHostileSeriesIsRefused(t *testing.T) {
	header := func(points uint64) []byte {
		raw := binary.AppendUvarint(nil, uint64(len(SeriesColumns)))
		return binary.AppendUvarint(raw, points)
	}
	column := func(values ...int64) []byte {
		var raw []byte
		for _, v := range values {
			raw = binary.AppendVarint(raw, v)
		}
		return raw
	}
	twoPoints := func(cols map[int][]int64) []byte {
		raw := header(2)
		for c := range SeriesColumns {
			deltas, ok := cols[c]
			if !ok {
				deltas = []int64{0, 0}
			}
			raw = append(raw, column(deltas...)...)
		}
		return raw
	}

	for name, raw := range map[string][]byte{
		"a column that overflows":    twoPoints(map[int][]int64{1: {math.MaxInt64, 1}}),
		"a count that goes negative": twoPoints(map[int][]int64{1: {5, -6}}),
		"time that goes backwards":   twoPoints(map[int][]int64{0: {1000, -1}}),
	} {
		if _, err := DecodeSeries(encodedRaw(t, raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}

	// EACH BOUND ALONE. A count above what a monitor keeps, with every byte the
	// count needs, is refused by the point bound; a count the bytes cannot hold
	// is refused by the byte bound. Measured after compressing, so the budget is
	// the decoder's own: allocating per point here would be ~50 MiB.
	many := uint64(4 * maxDecodedPoints)
	for _, tc := range []struct {
		name   string
		data   []byte
		budget uint64
	}{
		// Every byte the count needs is there, so only the point bound refuses it.
		{"too many points", encodedRaw(t, append(header(many),
			make([]byte, many*uint64(len(SeriesColumns)))...)), 32 << 20},
		// Within the point bound, far short of its bytes, so only the byte bound
		// refuses it before allocating ~10 MiB of points.
		{"too few bytes", encodedRaw(t, append(header(maxDecodedPoints-1), make([]byte, 100)...)),
			2 << 20},
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodeSeries(tc.data)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("%s: decoded", tc.name)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > tc.budget {
			t.Errorf("%s: refusing it allocated %d KiB, over its %d KiB budget", tc.name, grew>>10,
				tc.budget>>10)
		}
	}

	// A level that falls (memory freed) is ordinary and decodes.
	if _, err := DecodeSeries(encodedRaw(t, twoPoints(map[int][]int64{2: {5, -3}}))); err != nil {
		t.Errorf("a falling memory level was refused: %v", err)
	}
}
