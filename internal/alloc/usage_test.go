package alloc

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func measuredUsage() JobUsage {
	return JobUsage{
		Source: UsageSourceHost, Unmeasured: []string{UsageIO, UsageEnergy},
		Samples: 60, IntervalMillis: 1000, WindowMillis: 60_000,
		CPUUserMicros: 45_000_000, CPUSystemMicros: 5_000_000,
		GuestCPUMicros: 48_000_000, VMMCPUMicros: 2_000_000,
		MemoryPeakBytes: 8 << 30, OOMKills: 1,
		NetRxBytes: 700 << 20, NetTxBytes: 3 << 20, NetRxPackets: 500_000, NetTxPackets: 90_000,
		CPUSomeMicros: 120, MemorySomeMicros: 40, MemoryFullMicros: 10,
	}
}

// processUsage is a tart VM measured by its process's own accounting: no OOM
// count, no network, threads or pressure, and the kernel's own energy.
func processUsage() JobUsage {
	return JobUsage{
		Source:     UsageSourceHost,
		Unmeasured: []string{UsageNet, UsageOOM, UsagePressure, UsageThreads},
		Samples:    600, IntervalMillis: 1000, WindowMillis: 600_000,
		CPUUserMicros: 1_524_189_381, CPUSystemMicros: 67_371_613,
		MemoryPeakBytes: 25_855_595_336, DiskReadBytes: 7_894_355_968, DiskWriteBytes: 18_492_821_504,
		EnergyActiveMicrojoules: 7_400_903_879, EnergySource: EnergyProcess,
	}
}

// AN OOM COUNT IS READ ONLY WITH ITS MEMORY: a report with memory unmeasured
// has no count either, whether or not it names the group.
func TestAnOOMCountIsMeasuredOnlyWithItsMemory(t *testing.T) {
	t.Parallel()

	u := processUsage()
	if u.Measured(UsageOOM) || !u.Measured(UsageMemory) {
		t.Errorf("named unmeasured: oom measured %v, memory measured %v", u.Measured(UsageOOM),
			u.Measured(UsageMemory))
	}
	u = measuredUsage()
	u.Unmeasured = append(u.Unmeasured, UsageMemory)
	if u.Measured(UsageOOM) {
		t.Error("an OOM count read as measured beside unmeasured memory")
	}
	if !measuredUsage().Measured(UsageOOM) {
		t.Error("a measured memory group lost its OOM count")
	}
}

// A VM MEASURED BY ITS PROCESS IS KEPT AS SENT, its energy source with it.
func TestAProcessMeasuredReportIsKeptAsSent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, processUsage(), nil); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	got, err := a.LeaseUsage(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsage: %v", err)
	}
	if !reflect.DeepEqual(got.JobUsage, processUsage()) {
		t.Errorf("read back %+v, want %+v", got.JobUsage, processUsage())
	}
}

// A REPORT IS KEPT AS SENT, ITS SERIES WITH IT, AND THE FIRST ONE WINS: a
// retry after a lost answer must not replace what the ledger already kept.
func TestALeasesUsageIsKeptOnceAndReadBackAsSent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	first := measuredUsage()
	series := &UsageSeries{Codec: UsageSeriesCodec, Data: []byte{0, 1, 2, 250, 251}}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, first, series); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}

	second := first
	second.CPUUserMicros = 1
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, second,
		&UsageSeries{Codec: UsageSeriesCodec, Data: []byte{9}}); err != nil {
		t.Fatalf("RecordLeaseUsage (retry): %v", err)
	}

	got, err := a.LeaseUsage(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsage: %v", err)
	}
	if !reflect.DeepEqual(got.JobUsage, first) {
		t.Errorf("read back %+v, want the first report %+v", got.JobUsage, first)
	}
	if got.Node != "epyc-1" {
		t.Errorf("node = %q, want the lease's epyc-1", got.Node)
	}
	if got.Measured(UsageIO) || !got.Measured(UsageCPU) {
		t.Errorf("unmeasured = %v, want io named and cpu not", got.Unmeasured)
	}

	gotSeries, err := a.LeaseUsageSeries(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsageSeries: %v", err)
	}
	if gotSeries.Codec != UsageSeriesCodec || !bytes.Equal(gotSeries.Data, series.Data) {
		t.Errorf("series = %+v, want the first %+v", gotSeries, series)
	}
}

// A SUPERSEDED HOLDER CANNOT REPORT, AND NEITHER CAN ANYONE AFTER RELEASE. The
// node reports before it destroys the compute; a report that arrives once the
// lease has ended is not about a lease this ledger holds.
func TestAUsageReportIsFencedLikeEveryLeaseWrite(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch+1, measuredUsage(), nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("a report at a stale epoch = %v, want ErrFenced", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, measuredUsage(), nil); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("a report after release = %v, want ErrLeaseNotFound", err)
	}
	if _, err := a.LeaseUsage(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("LeaseUsage of a lease never measured = %v, want ErrLeaseNotFound", err)
	}
}

func TestAUsageReportTheLedgerCannotKeepIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*JobUsage)
		want   string
	}{
		{"no samples", func(u *JobUsage) { u.Samples = 0 }, "at least one sample"},
		{"a guest source", func(u *JobUsage) { u.Source = "guest" }, "not one this control plane"},
		{"a negative byte count", func(u *JobUsage) { u.NetRxBytes = -1 }, "net_rx_bytes is negative"},
		{"an unknown group", func(u *JobUsage) { u.Unmeasured = []string{"gpu"} }, `"gpu" is not a usage group`},
		{"a group named twice", func(u *JobUsage) { u.Unmeasured = []string{UsageIO, UsageIO} }, "twice"},
		{"energy unmeasured and reported", func(u *JobUsage) { u.EnergyIdleMicrojoules = 1 }, "names energy unmeasured"},
		{"energy measured with no source", func(u *JobUsage) { u.Unmeasured = nil }, `energy source ""`},
		{"cpu unmeasured and reported", func(u *JobUsage) { u.Unmeasured = append(u.Unmeasured, UsageCPU) },
			"names cpu unmeasured"},
		{"threads unmeasured and reported", func(u *JobUsage) {
			u.Unmeasured = append(u.Unmeasured, UsageThreads)
		}, "names threads unmeasured"},
		{"unsplit energy with an idle share", func(u *JobUsage) {
			u.Unmeasured = []string{UsageIO}
			u.EnergySource, u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules = EnergyRAPLUnsplit, 5, 1
		}, "no idle share"},
		{"process energy with an idle share", func(u *JobUsage) {
			u.Unmeasured = []string{UsageIO}
			u.EnergySource, u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules = EnergyProcess, 5, 1
		}, "process energy has no idle share"},
		{"an oom count unmeasured and reported", func(u *JobUsage) {
			u.Unmeasured = append(u.Unmeasured, UsageOOM)
		}, "names oom unmeasured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := measuredUsage()
			tc.mutate(&u)
			if err := u.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}

	if err := measuredUsage().Validate(); err != nil {
		t.Fatalf("a well-formed report was refused: %v", err)
	}
	if err := processUsage().Validate(); err != nil {
		t.Fatalf("a VM measured by its process was refused: %v", err)
	}
	for _, s := range []UsageSeries{
		{Codec: 2, Data: []byte{1}},
		{Codec: UsageSeriesCodec},
		{Codec: UsageSeriesCodec, Data: make([]byte, MaxUsageSeriesBytes+1)},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("series codec %d of %d bytes was accepted", s.Codec, len(s.Data))
		}
	}
}

// ONLY THE REPORT THAT WON THE SUMMARY WRITES A SERIES. A first report with no
// series and a retry carrying one would otherwise be stored as a pair neither
// request sent.
func TestASeriesIsKeptOnlyWithTheSummaryItCameWith(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, measuredUsage(), nil); err != nil {
		t.Fatalf("RecordLeaseUsage without a series: %v", err)
	}
	later := measuredUsage()
	later.CPUUserMicros++
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, later,
		&UsageSeries{Codec: UsageSeriesCodec, Data: []byte{1}}); err != nil {
		t.Fatalf("RecordLeaseUsage with a series: %v", err)
	}

	if _, err := a.LeaseUsageSeries(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("LeaseUsageSeries = %v, want none: the series came with a report that lost", err)
	}
}
