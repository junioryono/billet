package alloc

import (
	"bytes"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// aGuestReport has every field set to a value no other field shares, its
// arrival times in a zone other than UTC and below the second, so a column
// written from the wrong field or a time cut short reads back different.
func aGuestReport() GuestReport {
	zone := time.FixedZone("node", -7*60*60)
	first := time.Date(2026, 10, 9, 5, 0, 0, 123456789, zone)

	return GuestReport{
		AgentVersion: "billet-agent/0.1.0", Schema: 3, Codec: GuestReportCodec,
		Data:     []byte{0, 1, 2, 250, 251, 255},
		Accepted: 41, Refused: 7, DroppedBytes: 1 << 20,
		Hello: true, FinalSeen: false, NodeRestarted: true,
		FirstReceived: first, LastReceived: first.Add(90 * time.Second),
	}
}

// sameGuestReport compares two reports field by field, times by the instant
// they name, since the ledger keeps them in UTC.
func sameGuestReport(t *testing.T, got, want GuestReport) {
	t.Helper()

	if !got.FirstReceived.Equal(want.FirstReceived) || !got.LastReceived.Equal(want.LastReceived) {
		t.Errorf("arrivals read back %s to %s, want %s to %s", got.FirstReceived, got.LastReceived,
			want.FirstReceived, want.LastReceived)
	}
	got.FirstReceived, got.LastReceived = want.FirstReceived, want.LastReceived
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v,\nwant %+v", got, want)
	}
}

// A GUEST REPORT IS KEPT AS SENT, AND THE FIRST ONE WINS: a retry after a lost
// answer must not replace what the ledger already kept.
func TestALeasesGuestReportIsKeptOnceAndReadBackAsSent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	first := aGuestReport()
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, first); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	second := aGuestReport()
	second.Data, second.Accepted, second.FinalSeen = []byte{9}, 1, true
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, second); err != nil {
		t.Fatalf("RecordGuestReport (retry): %v", err)
	}

	got, err := a.LeaseGuestReport(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	sameGuestReport(t, got.GuestReport, first)
	if got.Node != "epyc-1" {
		t.Errorf("node = %q, want the lease's epyc-1", got.Node)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.RecordedAt); err != nil {
		t.Errorf("recorded_at %q is not a time: %v", got.RecordedAt, err)
	}
}

// A REPORT OF NOTHING READS BACK AS ONE, its arrival times zero rather than a
// time, and its data empty rather than absent.
func TestAGuestReportOfNothingReadsBackAsOne(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch,
		GuestReport{Codec: GuestReportCodec, NodeRestarted: true}); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
	got, err := a.LeaseGuestReport(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	if !got.FirstReceived.IsZero() || !got.LastReceived.IsZero() || len(got.Data) != 0 ||
		got.Hello || !got.NodeRestarted || got.Codec != GuestReportCodec {
		t.Errorf("a report of nothing read back as %+v", got.GuestReport)
	}
	// NULL IN THE ROW, not a time that reads back as zero: what a retention job
	// or a query by hand sees is that no batch arrived.
	var firstNull, lastNull bool
	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT first_received_at IS NULL, last_received_at IS NULL FROM job_guest_reports WHERE lease_id = $1`,
		lease.ID).Scan(&firstNull, &lastNull); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if !firstNull || !lastNull {
		t.Errorf("a report with no arrivals holds first NULL %v, last NULL %v, want both NULL", firstNull, lastNull)
	}
}

// A SUPERSEDED HOLDER CANNOT REPORT, AND NEITHER CAN ANYONE AFTER RELEASE, and
// a lease with no report reads as none kept, never as an empty report.
func TestAGuestReportIsFencedLikeEveryLeaseWrite(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch+1, aGuestReport()); !errors.Is(err, ErrFenced) {
		t.Fatalf("a report at a stale epoch = %v, want ErrFenced", err)
	}
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("a report after release = %v, want ErrLeaseNotFound", err)
	}
	got, err := a.LeaseGuestReport(t.Context(), lease.ID)
	if !errors.Is(err, ErrLeaseNotFound) || !strings.Contains(err.Error(), "no guest report was recorded") {
		t.Fatalf("LeaseGuestReport of a lease never reported = %+v, %v, want ErrLeaseNotFound saying none "+
			"was recorded", got, err)
	}
}

// A REPORT THE LEDGER CANNOT KEEP IS REFUSED BEFORE THE LEDGER IS TOUCHED,
// and nothing is kept for the lease.
func TestAGuestReportTheLedgerCannotKeepIsRefusedBeforeItIsWritten(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	bad := aGuestReport()
	bad.Data = make([]byte, MaxGuestReportBytes+1)
	if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, bad); err == nil ||
		!strings.Contains(err.Error(), "over the 262144 byte bound") {
		t.Fatalf("an oversized report = %v, want it refused by its bound", err)
	}
	if _, err := a.LeaseGuestReport(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("after a refused report, LeaseGuestReport = %v, want ErrLeaseNotFound", err)
	}
}

// A ROW THE LEDGER CANNOT READ BACK IS AN ERROR, never a report of nothing and
// never the lease having none: could-not-tell is its own answer.
func TestAGuestReportRowThatCannotBeReadBackIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, column, value, want string
	}{
		{"data that is not base64", "data", "not base64!", "is not base64"},
		{"an arrival that is not a time", "first_received_at", "yesterday", `"yesterday" is not RFC 3339`},
		{"a last arrival that is not a time", "last_received_at", "tomorrow", `"tomorrow" is not RFC 3339`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)
			if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); err != nil {
				t.Fatalf("RecordGuestReport: %v", err)
			}
			if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(),
					`UPDATE job_guest_reports SET `+tc.column+` = $1 WHERE lease_id = $2`, tc.value, lease.ID)

				return err
			}); err != nil {
				t.Fatalf("corrupt the row: %v", err)
			}

			got, err := a.LeaseGuestReport(t.Context(), lease.ID)
			if err == nil || errors.Is(err, ErrLeaseNotFound) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LeaseGuestReport = %+v, %v, want an error saying %q that is not ErrLeaseNotFound",
					got, err, tc.want)
			}
			if !bytes.Equal(got.Data, nil) || got.Accepted != 0 {
				t.Errorf("an unreadable row still handed back %+v", got.GuestReport)
			}
		})
	}
}
