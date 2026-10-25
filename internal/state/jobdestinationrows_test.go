package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// destRow is one row of job_destinations as RecordJobDestinations reads it:
// the unanswered count is packed only where counted says so, and the reply
// times only where timed does.
type destRow struct {
	ordinal                                 int64
	addr                                    string
	sent, received, connections, unanswered int64
	counted                                 bool
	timedReplies, slowReplies, slowestMs    int64
	timed                                   bool
}

// packRows packs rows into the fixed-width records RecordJobDestinations cuts
// apart, written here from the widths its comment states rather than from the
// allocator's packer, so a statement that cuts in the wrong place is seen.
func packRows(rows ...destRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%03d%-39s%019d%019d%019d", r.ordinal, r.addr, r.sent, r.received, r.connections)
		if r.counted {
			fmt.Fprintf(&b, "%019d", r.unanswered)
		} else {
			b.WriteString(strings.Repeat(" ", 19))
		}
		if r.timed {
			fmt.Fprintf(&b, "%019d%019d%019d", r.timedReplies, r.slowReplies, r.slowestMs)
		} else {
			b.WriteString(strings.Repeat(" ", 3*19))
		}
	}

	return b.String()
}

// writeRows records rows for lease in one statement and one transaction.
func writeRows(ctx context.Context, db *DB, lease string, rows ...destRow) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		return WriteQueries(tx).RecordJobDestinations(ctx, ledgerdb.RecordJobDestinationsParams{
			LeaseID: lease, Packed: packRows(rows...), RowCount: int64(len(rows)),
		})
	})
}

// THE ONE STATEMENT WRITES EVERY ROW IT IS GIVEN EXACTLY: each field cut from
// its place in the record, the longest address and the largest counts whole,
// an address's padding trimmed, an unanswered count or reply times of spaces
// kept as NULL and ones of zero as zero, and nothing at all for no rows.
func TestEveryPackedDestinationIsWrittenAsPacked(t *testing.T) {
	t.Parallel()

	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const most = int64(1<<63 - 1)
	want := []destRow{
		{0, "ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe", most, most, most, most, true, most, most, most, true},
		{1, "140.82.112.3", 1, 22, 333, 0, false, 0, 0, 0, false},
		{2, "::ffff:1.2.3.4", 0, 0, 0, 0, true, 0, 0, 0, true},
		{256, "", 4444, 55555, 7, 6, true, 1, 1, 2500, true},
	}
	if err := writeRows(t.Context(), db, "l1", want...); err != nil {
		t.Fatalf("RecordJobDestinations: %v", err)
	}
	if err := writeRows(t.Context(), db, "none"); err != nil {
		t.Fatalf("RecordJobDestinations with no rows: %v", err)
	}

	if err := db.View(t.Context(), func(q Querier) error {
		got, err := ReadQueries(q).ReadJobDestinations(t.Context(), "l1")
		if err != nil {
			return err
		}
		if len(got) != len(want) {
			t.Fatalf("read back %d rows, want %d: %+v", len(got), len(want), got)
		}
		for i, r := range got {
			if r.SlowReplies.Valid != r.TimedReplies.Valid || r.SlowestReplyMs.Valid != r.TimedReplies.Valid {
				t.Errorf("row %d reads part of a reply time: %+v", i, r)
			}
			if (destRow{r.Ordinal, r.Addr, r.SentBytes, r.ReceivedBytes, r.Connections, r.Unanswered.Int64,
				r.Unanswered.Valid, r.TimedReplies.Int64, r.SlowReplies.Int64, r.SlowestReplyMs.Int64,
				r.TimedReplies.Valid}) != want[i] {
				t.Errorf("row %d reads %+v, want %+v", i, r, want[i])
			}
		}
		none, err := ReadQueries(q).ReadJobDestinations(t.Context(), "none")
		if err != nil {
			return err
		}
		if len(none) != 0 {
			t.Errorf("no rows packed wrote %+v", none)
		}

		return nil
	}); err != nil {
		t.Fatalf("ReadJobDestinations: %v", err)
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

	named := func(ordinal int64, addr string) destRow {
		return destRow{ordinal: ordinal, addr: addr, sent: 1, received: 2, connections: 1}
	}

	// THE MOST A JOB CAN HAVE IS ACCEPTED, so each refusal below is about its
	// one broken thing.
	full := make([]destRow, 0, 257)
	for i := range int64(256) {
		full = append(full, named(i, fmt.Sprintf("10.0.%d.%d", i/256, i%256)))
	}
	full = append(full, named(256, ""))
	if err := writeRows(t.Context(), db, "full", full...); err != nil {
		t.Fatalf("256 destinations and the rest were refused: %v", err)
	}

	negative := named(0, "10.0.0.1")
	negative.received = -1
	unanswered := named(0, "10.0.0.1")
	unanswered.unanswered, unanswered.counted = -1, true
	slowest := named(0, "10.0.0.1")
	slowest.slowestMs, slowest.timed = -1, true
	for name, tc := range map[string]struct {
		lease string
		rows  []destRow
	}{
		"an ordinal past the rest":       {"l", []destRow{named(257, "")}},
		"a negative ordinal":             {"l", []destRow{named(-1, "10.0.0.1")}},
		"a named destination at 256":     {"l", []destRow{named(256, "10.0.0.1")}},
		"the rest at a named ordinal":    {"l", []destRow{named(3, "")}},
		"one address on two rows":        {"l", []destRow{named(0, "10.0.0.1"), named(1, "10.0.0.1")}},
		"two rows at one ordinal":        {"l", []destRow{named(0, "10.0.0.1"), named(0, "10.0.0.2")}},
		"a negative total":               {"l", []destRow{negative}},
		"a negative unanswered count":    {"l", []destRow{unanswered}},
		"a negative slowest reply":       {"l", []destRow{slowest}},
		"a 258th row for a full lease":   {"full", []destRow{named(255, "10.9.9.9")}},
		"a second rest for a full lease": {"full", []destRow{named(256, "")}},
	} {
		err := writeRows(t.Context(), db, tc.lease, tc.rows...)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "constraint") {
			t.Errorf("%s was refused for another reason: %v", name, err)
		}
	}
}
