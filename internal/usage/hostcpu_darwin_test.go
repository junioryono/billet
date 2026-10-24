//go:build darwin && arm64

package usage

import (
	"runtime"
	"testing"
	"time"
)

// THIS MAC'S CPU COUNTERS ARE READ WITHOUT ROOT, one per processor, advancing a
// cpu-second each per second: a read of the running kernel, as the node's own
// account reads it.
func TestThisMacsCPUCountersAreRead(t *testing.T) {
	r := Reader{Root: "/"}

	before, err := r.ReadHostCPU()
	if err != nil {
		t.Fatalf("ReadHostCPU: %v", err)
	}
	if before.CPUs != runtime.NumCPU() {
		t.Errorf("read %d processors, and this Mac has %d", before.CPUs, runtime.NumCPU())
	}

	start := time.Now()
	time.Sleep(time.Second)
	after, err := r.ReadHostCPU()
	if err != nil {
		t.Fatalf("ReadHostCPU: %v", err)
	}
	elapsed := time.Since(start)

	total, busy := after.Total-before.Total, after.Busy-before.Busy
	want := float64(elapsed.Microseconds()) * float64(before.CPUs)
	if float64(total) < 0.8*want || float64(total) > 1.2*want {
		t.Errorf("the counters advanced %d µs in %s on %d processors, want about %.0f", total, elapsed,
			before.CPUs, want)
	}
	if busy < 0 || busy > total {
		t.Errorf("busy advanced %d µs of %d", busy, total)
	}
}

// A READER ROOTED ANYWHERE BUT / IS A FIXTURE TREE, read the Linux way, so a
// test's /proc/stat is what answers on a Mac too.
func TestAFixtureTreesCPUIsItsProcStat(t *testing.T) {
	tr := newTree(t)
	tr.write("/proc/stat", procStat(1300, 10500))

	host, err := Reader{Root: tr.root}.ReadHostCPU()
	if err != nil || host.CPUs != 18 || host.Busy != 13_000_000 || host.Total != 118_000_000 {
		t.Fatalf("ReadHostCPU = %+v, %v; want the fixture's 18 cpus, 1300 and 11800 ticks", host, err)
	}
}
