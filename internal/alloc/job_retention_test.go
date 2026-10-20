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

// detail is what detailedJob records for a job, and when.
type detail struct {
	usageAt time.Time
	// guestAt is when the guest report was recorded; zero records none.
	guestAt  time.Time
	finished bool
}

// detailedJob is a busy lease with a usage report carrying a series and
// destinations recorded at d.usageAt, a guest report at d.guestAt, and the
// lease finished if d.finished.
func detailedJob(t *testing.T, a *Allocator, d detail) *Lease {
	t.Helper()

	lease := busyLease(t, a)
	usage := measuredUsage()
	usage.Destinations = fullDestinations()
	series := &UsageSeries{Codec: UsageSeriesCodec, Data: []byte{0, 1, 2}}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, series); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	if !d.guestAt.IsZero() {
		if err := a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, aGuestReport()); err != nil {
			t.Fatalf("RecordGuestReport: %v", err)
		}
	}
	if d.finished {
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
	// recorded_at is the wall clock's, written by the ledger writer; the test
	// sets it to when the detail is meant to have been recorded.
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE job_usage SET recorded_at = $1 WHERE lease_id = $2`,
			d.usageAt.UTC().Format(time.RFC3339Nano), lease.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE job_guest_reports SET recorded_at = $1 WHERE lease_id = $2`,
			d.guestAt.UTC().Format(time.RFC3339Nano), lease.ID)
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

// retire is one retention batch as the control plane runs it: the jobs past
// before listed on a read, at most limit of them, then retired.
func retire(t *testing.T, a *Allocator, before time.Time, limit int) (RetiredJobDetail, error) {
	t.Helper()

	ids, err := a.JobsWithRetirableDetail(t.Context(), before, limit)
	if err != nil {
		return RetiredJobDetail{}, err
	}

	return a.RetireJobDetail(t.Context(), ids)
}

// cutoff is the moment the retention tests retire detail recorded before.
var cutoff = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

// A FINISHED JOB'S DETAIL PAST THE RETENTION GOES AND ITS SUMMARY STAYS; DETAIL
// INSIDE IT STAYS. The series, the destinations and the guest report of a job
// recorded before the cutoff are deleted and the job is marked retired; a newer
// job's are untouched and unmarked; batches take the oldest first and progress;
// and a pass with nothing older marks nothing.
func TestJobDetailPastTheRetentionGoesAndItsSummaryStays(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	oldest := detailedJob(t, a, detail{usageAt: cutoff.Add(-72 * time.Hour), guestAt: cutoff.Add(-72 * time.Hour),
		finished: true})
	old := detailedJob(t, a, detail{usageAt: cutoff.Add(-time.Hour), finished: true})
	recent := detailedJob(t, a, detail{usageAt: cutoff.Add(time.Hour), guestAt: cutoff.Add(time.Hour),
		finished: true})

	first, err := retire(t, a, cutoff, 1)
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

	second, err := retire(t, a, cutoff, 10)
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

	third, err := retire(t, a, cutoff, 10)
	if err != nil || third != (RetiredJobDetail{}) {
		t.Errorf("a batch with nothing older retired %+v (%v), want nothing", third, err)
	}
}

// A JOB IS RETIRED WHOLE AND ONLY ONCE IT IS DONE: an unfinished job keeps its
// detail however old, so none arrives after its retirement; a job whose guest
// report is newer than its usage waits for both; and a job whose usage and
// guest report arrived at different times takes one place in a batch, so a
// batch that comes back full means more may be left.
func TestAJobIsRetiredWholeAndOnlyOnceItIsDone(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	running := detailedJob(t, a, detail{usageAt: cutoff.Add(-48 * time.Hour), guestAt: cutoff.Add(-48 * time.Hour)})
	split := detailedJob(t, a, detail{usageAt: cutoff.Add(-48 * time.Hour), guestAt: cutoff.Add(time.Hour),
		finished: true})
	apartA := detailedJob(t, a, detail{usageAt: cutoff.Add(-30 * time.Hour), guestAt: cutoff.Add(-20 * time.Hour),
		finished: true})
	apartB := detailedJob(t, a, detail{usageAt: cutoff.Add(-29 * time.Hour), guestAt: cutoff.Add(-19 * time.Hour),
		finished: true})

	got, err := retire(t, a, cutoff, 2)
	if err != nil {
		t.Fatalf("RetireJobDetail: %v", err)
	}
	if got.Jobs != 2 || got.GuestReports != 2 {
		t.Errorf("a batch of two retired %+v, want both jobs whose usage and guest report arrived apart", got)
	}
	for _, lease := range []*Lease{apartA, apartB} {
		if _, series, _, guest, retired := detailOf(t, a, lease.ID); series || guest || retired == "" {
			t.Errorf("job %s keeps series %t, guest %t, retired %q", lease.ID, series, guest, retired)
		}
	}

	rest, err := retire(t, a, cutoff, 10)
	if err != nil || rest.Jobs != 0 {
		t.Errorf("the next batch retired %+v (%v); an unfinished job and one with a newer guest report wait", rest, err)
	}
	for _, lease := range []*Lease{running, split} {
		if _, series, _, guest, retired := detailOf(t, a, lease.ID); !series || !guest || retired != "" {
			t.Errorf("job %s lost series %t, guest %t, or was marked %q", lease.ID, !series, !guest, retired)
		}
	}

	later, err := retire(t, a, cutoff.Add(2*time.Hour), 10)
	if err != nil || later.Jobs != 1 || later.GuestReports != 1 {
		t.Errorf("past the newer guest report retired %+v (%v), want that job whole", later, err)
	}
	if _, series, _, guest, _ := detailOf(t, a, running.ID); !series || !guest {
		t.Error("an unfinished job lost its detail")
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
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := retire(t, a, now.Add(time.Hour), 10)
	if err != nil || got.Jobs != 0 {
		t.Fatalf("retired %+v (%v) of a job with a usage summary and nothing else", got, err)
	}
	if _, _, _, _, retired := detailOf(t, a, lease.ID); retired != "" {
		t.Errorf("a job with no detail is marked retired %q", retired)
	}
}

// A WRITE BATCH STAMPS ONLY WHAT IS STILL RETIRABLE: a job it is handed that is
// unfinished, or already retired, is left as it is, since the list was read
// before the batch's transaction.
func TestARetirementBatchChecksWhatItIsHanded(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	running := detailedJob(t, a, detail{usageAt: cutoff.Add(-48 * time.Hour)})
	done := detailedJob(t, a, detail{usageAt: cutoff.Add(-48 * time.Hour), finished: true})

	got, err := a.RetireJobDetail(t.Context(), []string{running.ID, done.ID})
	if err != nil || got.Jobs != 1 {
		t.Fatalf("handed an unfinished and a finished job, retired %+v (%v); want the finished one", got, err)
	}
	if _, series, _, _, retired := detailOf(t, a, running.ID); !series || retired != "" {
		t.Errorf("the unfinished job lost its series (%t) or was marked %q", !series, retired)
	}
	_, _, _, _, first := detailOf(t, a, done.ID)

	now = now.Add(time.Hour)
	again, err := a.RetireJobDetail(t.Context(), []string{done.ID})
	if err != nil || again.Jobs != 0 {
		t.Errorf("handed a retired job again, retired %+v (%v); want nothing", again, err)
	}
	if _, _, _, _, retired := detailOf(t, a, done.ID); retired != first {
		t.Errorf("a retired job's stamp moved from %q to %q", first, retired)
	}
}
