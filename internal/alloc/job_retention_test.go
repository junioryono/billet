package alloc

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// retentionFleet is quarantineFleet's node with room for four of its leases.
func retentionFleet(t *testing.T, now *time.Time) *Allocator {
	t.Helper()

	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker
	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small}, WithClock(func() time.Time { return *now }), WithLeaseTTL(30*time.Second))
	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 16, Memory: 64 * config.GiB})

	return a
}

// detailedJob is a busy lease with a usage report carrying a series and
// destinations, and a guest report, each recorded at at.
func detailedJob(t *testing.T, a *Allocator, at time.Time, guest bool) *Lease {
	t.Helper()

	lease := busyLease(t, a)
	usage := measuredUsage()
	usage.Destinations = fullDestinations()
	series := &UsageSeries{Codec: UsageSeriesCodec, Data: []byte{0, 1, 2}}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, series); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	if guest {
		if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); err != nil {
			t.Fatalf("RecordGuestReport: %v", err)
		}
	}
	// recorded_at is the wall clock's, written by the ledger writer; the test
	// sets it to when the job is meant to have been recorded.
	stamp := at.UTC().Format(time.RFC3339Nano)
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE job_usage SET recorded_at = ? WHERE lease_id = ?`,
			stamp, lease.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE job_guest_reports SET recorded_at = ? WHERE lease_id = ?`,
			stamp, lease.ID)
		return err
	}); err != nil {
		t.Fatalf("date the job's detail: %v", err)
	}

	return lease
}

// detailOf says which of a job's detail the ledger still holds, and whether its
// usage summary does.
func detailOf(t *testing.T, a *Allocator, leaseID string) (usage, series, destinations, guest bool, retired string) {
	t.Helper()

	u, err := a.LeaseUsage(t.Context(), leaseID)
	switch {
	case err == nil:
		usage, destinations = true, u.Destinations != nil
	case !errors.Is(err, ErrLeaseNotFound):
		t.Fatalf("LeaseUsage: %v", err)
	}
	switch _, err := a.LeaseUsageSeries(t.Context(), leaseID); {
	case err == nil:
		series = true
	case !errors.Is(err, ErrLeaseNotFound):
		t.Fatalf("LeaseUsageSeries: %v", err)
	}
	switch _, err := a.LeaseGuestReport(t.Context(), leaseID); {
	case err == nil:
		guest = true
	case !errors.Is(err, ErrLeaseNotFound):
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	job, err := a.Job(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}

	return usage, series, destinations, guest, job.DetailRetiredAt
}

// A JOB'S DETAIL PAST THE RETENTION GOES AND ITS SUMMARY STAYS; DETAIL INSIDE IT
// STAYS. The series, the destinations and the guest report of a job recorded
// before the cutoff are deleted and the job is marked retired; a newer job's
// are untouched and unmarked; batches take the oldest first and progress; and
// a pass with nothing older marks nothing.
func TestJobDetailPastTheRetentionGoesAndItsSummaryStays(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	cutoff := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	oldest := detailedJob(t, a, cutoff.Add(-72*time.Hour), true)
	old := detailedJob(t, a, cutoff.Add(-time.Hour), false)
	recent := detailedJob(t, a, cutoff.Add(time.Hour), true)

	first, err := a.RetireJobDetail(t.Context(), cutoff, 1)
	if err != nil {
		t.Fatalf("RetireJobDetail: %v", err)
	}
	if first.Jobs != 1 || first.Series != 1 || first.Destinations == 0 || first.GuestReports != 1 {
		t.Errorf("the first batch of one retired %+v, want the oldest job's series, destinations and guest report",
			first)
	}
	if _, series, _, _, retired := detailOf(t, a, old.ID); !series || retired != "" {
		t.Errorf("a batch of one took the newer of two old jobs (series kept %t, retired %q)", series, retired)
	}

	second, err := a.RetireJobDetail(t.Context(), cutoff, 10)
	if err != nil {
		t.Fatalf("RetireJobDetail: %v", err)
	}
	if second.Jobs != 1 || second.Series != 1 || second.GuestReports != 0 {
		t.Errorf("the second batch retired %+v, want the remaining old job alone", second)
	}

	for _, lease := range []*Lease{oldest, old} {
		usage, series, destinations, guest, retired := detailOf(t, a, lease.ID)
		if !usage || series || destinations || guest || retired == "" {
			t.Errorf("an old job holds usage %t, series %t, destinations %t, guest %t, retired %q; want only its "+
				"usage summary, and marked retired", usage, series, destinations, guest, retired)
		}
	}
	usage, series, destinations, guest, retired := detailOf(t, a, recent.ID)
	if !usage || !series || !destinations || !guest || retired != "" {
		t.Errorf("a job inside the retention holds usage %t, series %t, destinations %t, guest %t, retired %q; "+
			"want all of it, unmarked", usage, series, destinations, guest, retired)
	}

	third, err := a.RetireJobDetail(t.Context(), cutoff, 10)
	if err != nil || third != (RetiredJobDetail{}) {
		t.Errorf("a batch with nothing older retired %+v (%v), want nothing", third, err)
	}
}

// A JOB WITH NO DETAIL IS NEVER MARKED RETIRED, so a report never says a job's
// series was retired when the job never had one.
func TestAJobWithNoDetailIsNotMarkedRetired(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	lease := busyLease(t, a)
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, measuredUsage(), nil); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	got, err := a.RetireJobDetail(t.Context(), now.Add(time.Hour), 10)
	if err != nil || got.Jobs != 0 {
		t.Fatalf("retired %+v (%v) of a job with a usage summary and nothing else", got, err)
	}
	if _, _, _, _, retired := detailOf(t, a, lease.ID); retired != "" {
		t.Errorf("a job with no detail is marked retired %q", retired)
	}
}
