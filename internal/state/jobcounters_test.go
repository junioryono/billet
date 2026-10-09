package state

import (
	"database/sql"
	"fmt"
	"testing"
)

// A USAGE ROW WRITTEN BEFORE MIGRATION 58 READS BACK UNCOUNTED, every counter
// NULL rather than zero, with the rest of the row as it was written.
func TestAUsageRowWrittenAtVersion57ReadsBackUncounted(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			// MIGRATION 59 FIRST, which builds on this table: a ledger at 57 has
			// neither.
			`DROP TABLE job_destinations`,
			`ALTER TABLE job_usage DROP COLUMN destinations_incomplete`,
			`ALTER TABLE job_usage DROP COLUMN tap_sent_bytes`,
			`ALTER TABLE job_usage DROP COLUMN tap_received_bytes`,
			`DELETE FROM schema_migrations WHERE version = 59`,
			`ALTER TABLE job_usage DROP COLUMN cycles`,
			`ALTER TABLE job_usage DROP COLUMN instructions`,
			`ALTER TABLE job_usage DROP COLUMN cache_references`,
			`ALTER TABLE job_usage DROP COLUMN cache_misses`,
			`ALTER TABLE job_usage DROP COLUMN branch_misses`,
			`ALTER TABLE job_usage DROP COLUMN frontend_stall_cycles`,
			`DELETE FROM schema_migrations WHERE version = 58`,
			`INSERT INTO job_usage
			 (lease_id, node, recorded_at, source, unmeasured, samples, interval_ms,
			  window_ms, cpu_user_us, cpu_system_us, guest_cpu_us, vmm_cpu_us,
			  memory_peak_bytes, oom_kills, disk_read_bytes, disk_write_bytes,
			  net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets,
			  cpu_some_us, cpu_full_us, memory_some_us, memory_full_us, io_some_us,
			  io_full_us, energy_active_uj, energy_idle_uj, energy_source)
			 VALUES ('l57', 'epyc-1', 't', 'host', 'io', 60, 1000, 60000, 45, 5, 48, 2,
			         8, 0, 0, 0, 7, 3, 5, 9, 1, 0, 0, 0, 0, 0, 0, 0, '')`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 57: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the version 57 ledger: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open the version 57 ledger with version 58: %v", err)
	}
	defer upgraded.Close()

	if err := upgraded.View(t.Context(), func(q Querier) error {
		row, err := ReadQueries(q).ReadJobUsage(t.Context(), "l57")
		if err != nil {
			return err
		}
		if row.Node != "epyc-1" || row.Samples != 60 || row.CpuUserUs != 45 || row.NetRxPackets != 5 {
			t.Errorf("the row written at 57 reads back as %+v", row)
		}
		for name, v := range map[string]sql.NullInt64{
			"cycles": row.Cycles, "instructions": row.Instructions,
			"cache_references": row.CacheReferences, "cache_misses": row.CacheMisses,
			"branch_misses": row.BranchMisses, "frontend_stall_cycles": row.FrontendStallCycles,
		} {
			if v.Valid {
				t.Errorf("%s reads %d on a row written before it existed, want NULL", name, v.Int64)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("ReadJobUsage: %v", err)
	}
}
