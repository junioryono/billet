//go:build darwin

package usage

import (
	"os"
	"path/filepath"
	"syscall"
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

// rusageMicros is the user and system CPU this process has used, as getrusage
// reports it.
func rusageMicros(t *testing.T) (user, system int64) {
	t.Helper()

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}

	return ru.Utime.Sec*1_000_000 + int64(ru.Utime.Usec), ru.Stime.Sec*1_000_000 + int64(ru.Stime.Usec)
}

// userMicros is the user CPU this process has used, as getrusage reports it.
func userMicros(t *testing.T) int64 {
	t.Helper()

	user, _ := rusageMicros(t)

	return user
}

// bracketSlack is how far billet's reading may exceed getrusage's over a window
// that contains it: the two count the same task's time at different
// resolutions (mach ticks and microseconds), never by more than a few ms.
const bracketSlack = 50_000

// spinSink keeps spin's arithmetic from being optimised away.
var spinSink uint64

// spin burns user CPU in a batch of arithmetic with no system call in it.
func spin() {
	x := spinSink
	for i := range uint64(2_000_000) {
		x = x*6364136223846793005 + i
	}

	spinSink = x
}

// THE REAL KERNEL ANSWERS FOR THIS PROCESS: it has a start, the CPU it just
// burned, a footprint, and its own executable's path.
func TestThisProcessReadsItsOwnAccounting(t *testing.T) {
	// getrusage's own readings bracket billet's two: whatever CPU billet's
	// reader sees between its reads, getrusage saw at least that between these.
	outerUser0, outerSystem0 := rusageMicros(t)

	before, err := Reader{}.ProcessCounters(os.Getpid())
	if err != nil {
		t.Fatalf("read this process: %v", err)
	}

	// THE LOOP BURNS USER CPU, NOT WALL TIME, until getrusage, the kernel's other
	// account of this process, says 300ms more of it was used. A 300ms wall-clock
	// loop on a machine starved by the rest of `make check` received 143ms to
	// 194ms of CPU (2026-10-03 and 2026-10-05, #188) and failed the floor below
	// while the reader was right. The work between polls is arithmetic with no
	// system call, so the time waited for is user time and the reader's own user
	// time is held to it; a loop of getrusage calls alone spends much of its CPU
	// in the kernel, where it could hide a reader losing user time. The 30s bound
	// is how long the test waits for that CPU, not a proof it would never come.
	start := userMicros(t)
	for limit := time.Now().Add(30 * time.Second); userMicros(t)-start < 300_000; {
		if time.Now().After(limit) {
			t.Fatal("30s of wall time gave this process less than 300ms of user CPU")
		}

		spin()
	}

	after, err := Reader{}.ProcessCounters(os.Getpid())
	if err != nil {
		t.Fatalf("read this process again: %v", err)
	}

	outerUser1, outerSystem1 := rusageMicros(t)
	if after.Start == 0 || after.Start != before.Start {
		t.Errorf("this process's start went from %d to %d", before.Start, after.Start)
	}
	if used := after.UserMicros - before.UserMicros; used < 200_000 || used > 30_000_000 {
		t.Errorf("300ms of user CPU by getrusage read as %dµs of user CPU", used)
	}

	// AND NO MORE THAN THE KERNEL SAW, both kinds: a reader over-reporting by a
	// unit or a timebase, or reading the wrong field of the record for system
	// time, passes every floor and fails here.
	if used, most := after.UserMicros-before.UserMicros, outerUser1-outerUser0+bracketSlack; used > most {
		t.Errorf("billet read %dµs of user CPU where getrusage saw at most %dµs", used, most-bracketSlack)
	}

	if used, most := after.SystemMicros-before.SystemMicros, outerSystem1-outerSystem0+bracketSlack; used < 0 || used > most {
		t.Errorf("billet read %dµs of system CPU where getrusage saw %dµs", used, most-bracketSlack)
	}
	if used := (after.UserMicros + after.SystemMicros) - (before.UserMicros + before.SystemMicros); used < 200_000 ||
		used > 30_000_000 {
		t.Errorf("300ms of user CPU by getrusage read as %dµs of CPU", used)
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
