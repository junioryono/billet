//go:build darwin

package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

// THE RECORD IS THE SDK'S, BYTE FOR BYTE: struct rusage_info_v6 is 464 bytes in
// the macOS 26 SDK, and a field out of place reads every later one from the
// wrong offset without any error.
func TestTheRusageRecordIsTheSDKsSize(t *testing.T) {
	t.Parallel()

	if size := unsafe.Sizeof(rusageInfoV6{}); size != 464 {
		t.Errorf("rusageInfoV6 is %d bytes, want 464", size)
	}
}

// TICKS ARE CONVERTED AT THE TIMEBASE, energy from nJ, and a record no billet
// reading could come from is refused.
func TestTheAccountingRecordIsReadInBilletsUnits(t *testing.T) {
	t.Parallel()

	c, err := processCountersOf(rusageInfoV6{
		ProcStartAbstime: 7, UserTime: 36_580_545_147, SystemTime: 1_616_918_714,
		PhysFootprint: 3, LifetimeMaxPhysFoot: 2, DiskioBytesread: 5, DiskioByteswritten: 6,
		EnergyNJ: 7_400_903_879_162,
	}, 24_000_000)
	if err != nil {
		t.Fatalf("processCountersOf: %v", err)
	}
	want := ProcessCounters{Start: 7, UserMicros: 1_524_189_381, SystemMicros: 67_371_613,
		Footprint: 3, PeakFootprint: 3, DiskRead: 5, DiskWrite: 6, EnergyOK: true, Energy: 7_400_903_879}
	if c != want {
		t.Errorf("read %+v,\nwant %+v", c, want)
	}
	if c, err := processCountersOf(rusageInfoV6{ProcStartAbstime: 7}, 24_000_000); err != nil || c.EnergyOK {
		t.Errorf("a record with no energy estimate read as one (%+v, %v)", c, err)
	}
	for _, bad := range []struct {
		ri   rusageInfoV6
		freq uint64
	}{{rusageInfoV6{ProcStartAbstime: 7}, 0}, {rusageInfoV6{}, 24_000_000}} {
		if _, err := processCountersOf(bad.ri, bad.freq); err == nil {
			t.Errorf("a record with start %d at timebase %d was read", bad.ri.ProcStartAbstime, bad.freq)
		}
	}
}

// THE REAL KERNEL ANSWERS FOR THIS PROCESS: it has a start, the CPU it just
// burned, a footprint, and its own executable's path.
func TestThisProcessReadsItsOwnAccounting(t *testing.T) {
	before, err := Reader{}.ProcessCounters(os.Getpid())
	if err != nil {
		t.Fatalf("read this process: %v", err)
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); {
		_ = unsafe.Pointer(&end)
	}
	after, err := Reader{}.ProcessCounters(os.Getpid())
	if err != nil {
		t.Fatalf("read this process again: %v", err)
	}
	if after.Start == 0 || after.Start != before.Start {
		t.Errorf("this process's start went from %d to %d", before.Start, after.Start)
	}
	if used := (after.UserMicros + after.SystemMicros) - (before.UserMicros + before.SystemMicros); used < 200_000 ||
		used > 30_000_000 {
		t.Errorf("a 300ms busy loop read as %dµs of CPU", used)
	}
	if after.Footprint <= 0 || after.PeakFootprint < after.Footprint {
		t.Errorf("footprint %d, peak %d", after.Footprint, after.PeakFootprint)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	got, err := ProcessPath(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessPath: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("resolve this executable: %v", err)
	}
	gotPath, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve %q: %v", got, err)
	}
	if gotPath != wantPath {
		t.Errorf("ProcessPath is %q, want this executable %q", got, exe)
	}
	if _, err := ProcessPath(1 << 30); err == nil {
		t.Error("ProcessPath answered for a pid no process has")
	}
}
