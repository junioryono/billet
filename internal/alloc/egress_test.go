package alloc

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func anEgressReport() JobEgress {
	return JobEgress{
		Listed: true, Queries: 10, RefusedQueries: 3,
		RefusedNames:  []EgressRefusal{{Name: "evil.example", Count: 3}},
		Names:         []EgressNames{{Addr: "198.51.100.1", Names: []string{"github.com", "edge.cdn.example"}}},
		DeniedPackets: 9, OutsideResolverPackets: 2, IPv6Packets: 4, Incomplete: true,
		Denied: []EgressDenial{{Addr: "203.0.113.7", Protocol: "tcp", Port: 443, Packets: 7}},
	}
}

// AN EGRESS REPORT IS KEPT AS SENT, AND THE FIRST ONE WINS: a retry after a lost answer
// must not replace what the ledger already kept.
func TestALeasesEgressReportIsKeptOnceAndReadBackAsSent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	first := anEgressReport()
	if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, first); err != nil {
		t.Fatalf("RecordLeaseEgress: %v", err)
	}

	second := anEgressReport()
	second.Queries, second.Listed = 99, false
	second.RefusedQueries, second.RefusedNames, second.DeniedPackets, second.OutsideResolverPackets = 0, nil, 0, 0
	second.IPv6Packets, second.Denied = 0, nil

	if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, second); err != nil {
		t.Fatalf("RecordLeaseEgress (retry): %v", err)
	}

	got, err := a.LeaseEgress(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseEgress: %v", err)
	}

	if !reflect.DeepEqual(got.JobEgress, first) {
		t.Errorf("read back %+v,\nwant %+v", got.JobEgress, first)
	}

	if got.Node != "epyc-1" {
		t.Errorf("node = %q, want the lease's epyc-1", got.Node)
	}

	if _, err := time.Parse(time.RFC3339Nano, got.RecordedAt); err != nil {
		t.Errorf("recorded_at %q is not a time: %v", got.RecordedAt, err)
	}
}

func TestAnEgressReportIsFencedLikeEveryLeaseWrite(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch+1, anEgressReport()); !errors.Is(err, ErrFenced) {
		t.Fatalf("a report at a stale epoch = %v, want ErrFenced", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, anEgressReport()); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("a report after release = %v, want ErrLeaseNotFound", err)
	}

	got, err := a.LeaseEgress(t.Context(), lease.ID)
	if !errors.Is(err, ErrLeaseNotFound) || !strings.Contains(err.Error(), "no egress report was recorded") {
		t.Fatalf("LeaseEgress of a lease never reported = %+v, %v, want ErrLeaseNotFound saying none was "+
			"recorded", got, err)
	}
}

// A REPORT THE LEDGER CANNOT KEEP IS REFUSED BEFORE THE LEDGER IS TOUCHED, which is what
// holds a guest's name to bytes a terminal prints as themselves.
func TestAnEgressReportTheLedgerCannotKeepIsRefusedBeforeItIsWritten(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	bad := anEgressReport()
	bad.RefusedNames[0].Name = "a\x1b[31m.example"

	err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, bad)
	if err == nil || !strings.Contains(err.Error(), "not one") {
		t.Fatalf("RecordLeaseEgress = %v", err)
	}

	if _, err := a.LeaseEgress(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("LeaseEgress = %v, want nothing kept", err)
	}
}

// A JOB'S EGRESS REPORT IS DETAIL, RETIRED WITH THE REST OF IT: found by retention even
// when it is all the job has, as a listed job on a node sampling nothing, and gone once
// the job is stamped. One inside the retention stays.
func TestAnEgressReportIsRetiredWithItsJobsDetail(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	reported := func(at time.Time) *Lease {
		lease := busyLease(t, a)

		if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, anEgressReport()); err != nil {
			t.Fatalf("RecordLeaseEgress: %v", err)
		}

		if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}

		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE job_egress SET recorded_at = $1 WHERE lease_id = $2`,
				at.UTC().Format(time.RFC3339Nano), lease.ID)

			return err
		}); err != nil {
			t.Fatalf("date the report: %v", err)
		}

		return lease
	}

	old := reported(cutoff.Add(-48 * time.Hour))
	recent := reported(cutoff.Add(48 * time.Hour))

	got, err := retire(t, a, cutoff, 10)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}

	if got.Jobs != 1 || got.Egress != 1 {
		t.Fatalf("retired %+v, want the one old job and its report", got)
	}

	if _, err := a.LeaseEgress(t.Context(), old.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("the retired report reads %v", err)
	}

	if _, err := a.LeaseEgress(t.Context(), recent.ID); err != nil {
		t.Fatalf("the report inside the retention reads %v", err)
	}
}
