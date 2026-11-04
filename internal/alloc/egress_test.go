package alloc

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
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

// A TIER WITH AN ALLOWLIST IS NOT PLACED ON A HOST TOO OLD TO HONOUR IT, or placement
// keeps choosing a host the plane then refuses to launch on while a newer one sits idle.
// An open tier still uses every host.
func TestAListedTierIsPlacedOnlyWhereItCanBeHonoured(t *testing.T) {
	t.Parallel()

	open := tier("open", 2, 4*config.GiB)
	listed := tier("listed", 2, 4*config.GiB)
	listed.Network = &config.TierNetwork{Egress: &config.TierEgress{Allow: []string{"example.com"}}}

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{open, listed})

	old := testRegistration("old-host", config.ProviderFirecracker)
	old.WireMin, old.WireVersion, old.WireMax = 12, EgressWireVersion-1, EgressWireVersion-1

	if _, err := a.RegisterNode(t.Context(), old); err != nil {
		t.Fatalf("RegisterNode(old): %v", err)
	}

	if got := headroom(t, a, "listed"); got != 0 {
		t.Fatalf("listed headroom on an old host = %d, want 0", got)
	}

	if got := headroom(t, a, "open"); got == 0 {
		t.Fatal("an open tier could not use an old host")
	}

	current := testRegistration("new-host", config.ProviderFirecracker)
	current.WireMin, current.WireVersion, current.WireMax = 12, EgressWireVersion, EgressWireVersion

	if _, err := a.RegisterNode(t.Context(), current); err != nil {
		t.Fatalf("RegisterNode(new): %v", err)
	}

	leases, err := a.Escrow(t.Context(), "listed", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}

	if leases[0].TargetNode != "new-host" {
		t.Fatalf("listed lease placed on %q, want new-host", leases[0].TargetNode)
	}
}

// A ROW THE LEDGER CANNOT READ BACK IS AN ERROR OF ITS OWN, never no report, which would read
// as a job nothing watched, and a row it would refuse today is no better.
func TestAnEgressRowThatCannotBeReadIsNotNoReport(t *testing.T) {
	for name, report := range map[string]string{
		"not JSON":       "nope",
		"a name refused": `{"listed":true,"queries":1,"refused_queries":1,"refused_names":[{"name":"a\u001b.example","count":1}]}`,
	} {
		// A FLEET FOR EACH: one's capacity holds one busy lease.
		now := time.Now().UTC()
		a := quarantineFleet(t, &now)
		lease := busyLease(t, a)

		if err := a.RecordLeaseEgress(t.Context(), lease.ID, lease.Epoch, anEgressReport()); err != nil {
			t.Fatalf("RecordLeaseEgress: %v", err)
		}

		if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE job_egress SET report = $1 WHERE lease_id = $2`, report, lease.ID)

			return err
		}); err != nil {
			t.Fatalf("stage the row: %v", err)
		}

		_, err := a.LeaseEgress(t.Context(), lease.ID)
		if err == nil || errors.Is(err, ErrLeaseNotFound) {
			t.Errorf("%s: LeaseEgress = %v, want an error that is not no report", name, err)
		}
	}
}

// A TIER THE ALLOCATOR COULD NOT PLACE ON A BACKEND THAT ENFORCES ITS LIST IS REFUSED, whoever
// built the catalogue: this package takes tiers from code as well as from a file.
func TestTheAllocatorRefusesAListOnABackendThatCannotEnforceIt(t *testing.T) {
	t.Parallel()

	listed := tier("listed", 2, 4*config.GiB)
	listed.Provider = config.ProviderDocker
	listed.Network = &config.TierNetwork{Egress: &config.TierEgress{Allow: []string{"example.com"}}}

	db, _ := openTestLedgerPair(t, false)

	_, err := New(db, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB}, []config.Tier{listed})
	if err == nil || !strings.Contains(err.Error(), "would run open") {
		t.Fatalf("New = %v, want the list refused", err)
	}
}
