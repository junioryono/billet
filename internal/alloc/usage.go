package alloc

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordLeaseUsage keeps what the host measured a lease's job do.
//
// FENCED ON THE EPOCH, in the same transaction, so a superseded holder cannot
// report, and a lease that has ended is refused (ErrLeaseNotFound): the node
// reports before it destroys the compute, and anything arriving after release
// is not about a lease this ledger is holding. The first report is kept.
func (a *Allocator) RecordLeaseUsage(
	ctx context.Context, leaseID string, epoch int64, usage JobUsage, series *UsageSeries,
) error {
	if err := usage.Validate(); err != nil {
		return err
	}
	if series != nil {
		if err := series.Validate(); err != nil {
			return err
		}
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		q := state.WriteQueries(tx)
		var counters JobCounters
		if usage.Counters != nil {
			counters = *usage.Counters
		}
		won, err := q.RecordJobUsage(ctx, ledgerdb.RecordJobUsageParams{
			LeaseID: lease.ID, Node: lease.Node, RecordedAt: nowStamp(),
			Source: usage.Source, Unmeasured: strings.Join(usage.Unmeasured, ","),
			Samples: usage.Samples, IntervalMs: usage.IntervalMillis, WindowMs: usage.WindowMillis,
			CpuUserUs: usage.CPUUserMicros, CpuSystemUs: usage.CPUSystemMicros,
			GuestCpuUs: usage.GuestCPUMicros, VmmCpuUs: usage.VMMCPUMicros,
			MemoryPeakBytes: usage.MemoryPeakBytes, OomKills: usage.OOMKills,
			DiskReadBytes: usage.DiskReadBytes, DiskWriteBytes: usage.DiskWriteBytes,
			NetRxBytes: usage.NetRxBytes, NetTxBytes: usage.NetTxBytes,
			NetRxPackets: usage.NetRxPackets, NetTxPackets: usage.NetTxPackets,
			CpuSomeUs: usage.CPUSomeMicros, CpuFullUs: usage.CPUFullMicros,
			MemorySomeUs: usage.MemorySomeMicros, MemoryFullUs: usage.MemoryFullMicros,
			IoSomeUs: usage.IOSomeMicros, IoFullUs: usage.IOFullMicros,
			EnergyActiveUj: usage.EnergyActiveMicrojoules, EnergyIdleUj: usage.EnergyIdleMicrojoules,
			EnergySource: usage.EnergySource,
			Cycles:       nullCount(counters.Cycles), Instructions: nullCount(counters.Instructions),
			CacheReferences:     nullCount(counters.CacheReferences),
			CacheMisses:         nullCount(counters.CacheMisses),
			BranchMisses:        nullCount(counters.BranchMisses),
			FrontendStallCycles: nullCount(counters.FrontendStallCycles),
		})
		if err != nil {
			return fmt.Errorf("alloc: record the usage of lease %s: %w", leaseID, err)
		}

		// ONLY THE REPORT THAT WON WRITES A SERIES, so the stored pair is always
		// one request's.
		if series == nil || won == 0 {
			return nil
		}
		if err := q.RecordJobSeries(ctx, ledgerdb.RecordJobSeriesParams{
			LeaseID: lease.ID, Codec: int64(series.Codec),
			Series: base64.StdEncoding.EncodeToString(series.Data),
		}); err != nil {
			return fmt.Errorf("alloc: record the usage series of lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// RecordedUsage is a lease's usage as the ledger kept it.
type RecordedUsage struct {
	JobUsage
	Node       string
	RecordedAt string
}

// LeaseUsage reads what the host measured a lease's job do. A lease with no
// report is ErrLeaseNotFound: nothing was measured, which is not zero.
func (a *Allocator) LeaseUsage(ctx context.Context, leaseID string) (RecordedUsage, error) {
	var out RecordedUsage
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobUsage(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no usage was recorded for lease %s", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the usage of lease %s: %w", leaseID, err)
		}
		var unmeasured []string
		if row.Unmeasured != "" {
			unmeasured = strings.Split(row.Unmeasured, ",")
		}
		out = RecordedUsage{Node: row.Node, RecordedAt: row.RecordedAt, JobUsage: JobUsage{
			Source: row.Source, Unmeasured: unmeasured, Samples: row.Samples,
			IntervalMillis: row.IntervalMs, WindowMillis: row.WindowMs,
			CPUUserMicros: row.CpuUserUs, CPUSystemMicros: row.CpuSystemUs,
			GuestCPUMicros: row.GuestCpuUs, VMMCPUMicros: row.VmmCpuUs,
			MemoryPeakBytes: row.MemoryPeakBytes, OOMKills: row.OomKills,
			DiskReadBytes: row.DiskReadBytes, DiskWriteBytes: row.DiskWriteBytes,
			NetRxBytes: row.NetRxBytes, NetTxBytes: row.NetTxBytes,
			NetRxPackets: row.NetRxPackets, NetTxPackets: row.NetTxPackets,
			CPUSomeMicros: row.CpuSomeUs, CPUFullMicros: row.CpuFullUs,
			MemorySomeMicros: row.MemorySomeUs, MemoryFullMicros: row.MemoryFullUs,
			IOSomeMicros: row.IoSomeUs, IOFullMicros: row.IoFullUs,
			EnergyActiveMicrojoules: row.EnergyActiveUj, EnergyIdleMicrojoules: row.EnergyIdleUj,
			EnergySource: row.EnergySource,
			Counters: countersOf(row.Cycles, row.Instructions, row.CacheReferences, row.CacheMisses,
				row.BranchMisses, row.FrontendStallCycles),
		}}
		return nil
	})
	return out, err
}

// nullCount is a hardware counter as its column holds it: NULL where it was
// not counted.
func nullCount(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: *v, Valid: true}
}

// countersOf reads a row's hardware counters back: nil when none was counted,
// which is every row from before migration 58 and every job not counted since.
func countersOf(cycles, instructions, refs, misses, branchMisses, stalls sql.NullInt64) *JobCounters {
	count := func(v sql.NullInt64) *int64 {
		if !v.Valid {
			return nil
		}
		n := v.Int64
		return &n
	}
	c := JobCounters{
		Cycles: count(cycles), Instructions: count(instructions), CacheReferences: count(refs),
		CacheMisses: count(misses), BranchMisses: count(branchMisses), FrontendStallCycles: count(stalls),
	}
	if c == (JobCounters{}) {
		return nil
	}

	return &c
}

// LeaseUsageSeries reads a lease's encoded series. A lease with none is
// ErrLeaseNotFound.
func (a *Allocator) LeaseUsageSeries(ctx context.Context, leaseID string) (UsageSeries, error) {
	var out UsageSeries
	err := a.db.View(ctx, func(q querier) error {
		row, err := state.ReadQueries(q).ReadJobSeries(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: %w: no usage series was recorded for lease %s",
				ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the usage series of lease %s: %w", leaseID, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Series)
		if err != nil {
			return fmt.Errorf("alloc: the usage series of lease %s is not base64: %w", leaseID, err)
		}
		out = UsageSeries{Codec: int(row.Codec), Data: data}
		return nil
	})
	return out, err
}
