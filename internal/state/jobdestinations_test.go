package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// A USAGE ROW WRITTEN BEFORE MIGRATION 59 READS BACK WITH ITS DESTINATIONS NOT
// TOTALLED: the verdict and the tap NULL rather than zero, no destination rows,
// and the rest of the row (the counters migration 58 added included) as it was
// written.
func TestAUsageRowWrittenAtVersion58ReadsBackWithoutDestinations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DROP TABLE job_destinations`,
			`ALTER TABLE job_usage DROP COLUMN destinations_incomplete`,
			`ALTER TABLE job_usage DROP COLUMN tap_sent_bytes`,
			`ALTER TABLE job_usage DROP COLUMN tap_received_bytes`,
			`DELETE FROM schema_migrations WHERE version = 59`,
			`INSERT INTO job_usage
			 (lease_id, node, recorded_at, source, unmeasured, samples, interval_ms,
			  window_ms, cpu_user_us, cpu_system_us, guest_cpu_us, vmm_cpu_us,
			  memory_peak_bytes, oom_kills, disk_read_bytes, disk_write_bytes,
			  net_rx_bytes, net_tx_bytes, net_rx_packets, net_tx_packets,
			  cpu_some_us, cpu_full_us, memory_some_us, memory_full_us, io_some_us,
			  io_full_us, energy_active_uj, energy_idle_uj, energy_source, cycles)
			 VALUES ('l58', 'epyc-1', 't', 'host', 'io', 60, 1000, 60000, 45, 5, 48, 2,
			         8, 0, 0, 0, 7, 3, 5, 9, 1, 0, 0, 0, 0, 0, 0, 0, '', 4000)`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("rewind to version 58: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the version 58 ledger: %v", err)
	}

	upgraded, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("open the version 58 ledger with version 59: %v", err)
	}
	defer upgraded.Close()

	if err := upgraded.View(t.Context(), func(q Querier) error {
		row, err := ReadQueries(q).ReadJobUsage(t.Context(), "l58")
		if err != nil {
			return err
		}
		if row.Node != "epyc-1" || row.Samples != 60 || row.NetTxBytes != 3 || row.Cycles.Int64 != 4000 {
			t.Errorf("the row written at 58 reads back as %+v", row)
		}
		for name, v := range map[string]sql.NullInt64{
			"destinations_incomplete": row.DestinationsIncomplete,
			"tap_sent_bytes":          row.TapSentBytes, "tap_received_bytes": row.TapReceivedBytes,
		} {
			if v.Valid {
				t.Errorf("%s reads %d on a row written before it existed, want NULL", name, v.Int64)
			}
		}
		dests, err := ReadQueries(q).ReadJobDestinations(t.Context(), "l58")
		if err != nil {
			return err
		}
		if len(dests) != 0 {
			t.Errorf("a lease from before migration 59 reads %d destinations, want none", len(dests))
		}

		return nil
	}); err != nil {
		t.Fatalf("read the upgraded ledger: %v", err)
	}
}

// THE TABLE ITSELF HOLDS A JOB TO 257 ROWS, ONE OF THEM THE REST, whatever
// the code above it sends: an ordinal past 256, a named destination at the
// rest's ordinal, the rest with an address, one address on two rows, and a
// negative total are each refused by the schema.
func TestTheDestinationsTableRefusesWhatNoReportCanSay(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	write := func(ctx context.Context, rows ...ledgerdb.RecordJobDestinationParams) error {
		return db.Tx(ctx, func(tx *sql.Tx) error {
			for _, r := range rows {
				if err := WriteQueries(tx).RecordJobDestination(ctx, r); err != nil {
					return err
				}
			}

			return nil
		})
	}
	named := func(lease string, ordinal int64, addr string) ledgerdb.RecordJobDestinationParams {
		return ledgerdb.RecordJobDestinationParams{LeaseID: lease, Ordinal: ordinal, Addr: addr,
			SentBytes: 1, ReceivedBytes: 2, Connections: 1}
	}

	// THE MOST A JOB CAN HAVE IS ACCEPTED, so each refusal below is about its
	// one broken thing.
	full := make([]ledgerdb.RecordJobDestinationParams, 0, 257)
	for i := range int64(256) {
		full = append(full, named("full", i, fmt.Sprintf("10.0.%d.%d", i/256, i%256)))
	}
	full = append(full, named("full", 256, ""))
	if err := write(t.Context(), full...); err != nil {
		t.Fatalf("256 destinations and the rest were refused: %v", err)
	}

	negative := named("l", 0, "10.0.0.1")
	negative.ReceivedBytes = -1
	for name, rows := range map[string][]ledgerdb.RecordJobDestinationParams{
		"an ordinal past the rest":       {named("l", 257, "")},
		"a negative ordinal":             {named("l", -1, "10.0.0.1")},
		"a named destination at 256":     {named("l", 256, "10.0.0.1")},
		"the rest at a named ordinal":    {named("l", 3, "")},
		"one address on two rows":        {named("l", 0, "10.0.0.1"), named("l", 1, "10.0.0.1")},
		"two rows at one ordinal":        {named("l", 0, "10.0.0.1"), named("l", 0, "10.0.0.2")},
		"a negative total":               {negative},
		"a 258th row for a full lease":   {named("full", 255, "10.9.9.9")},
		"a second rest for a full lease": {named("full", 256, "")},
	} {
		err := write(t.Context(), rows...)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "constraint") {
			t.Errorf("%s was refused for another reason: %v", name, err)
		}
	}
}
