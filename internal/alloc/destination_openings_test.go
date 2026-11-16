package alloc

import (
	"database/sql"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// splitDestinations is destinations split by when each connection opened, as
// many openings as a report carries: the first destination's traffic across
// every bucket but the last, the rest beyond them in the last, with the widest
// counts the record holds.
func splitDestinations() *JobDestinations {
	d := &JobDestinations{
		Destinations: []JobDestination{{Addr: "140.82.112.4", SentBytes: math.MaxInt64,
			ReceivedBytes: math.MaxInt64, Connections: math.MaxInt64}},
		Other:          JobDestination{SentBytes: 5, ReceivedBytes: 6, Connections: 1},
		OpeningSeconds: 4,
	}
	for i := range MaxDestinationOpenings - 1 {
		d.Openings = append(d.Openings, DestinationOpening{Ordinal: 0, Start: int64(1_794_650_000 + 4*i),
			SentBytes: 1, ReceivedBytes: 2, Connections: 1})
	}
	d.Openings[0] = DestinationOpening{Ordinal: 0, Start: 1_794_650_000, SentBytes: math.MaxInt64 - 3000,
		ReceivedBytes: math.MaxInt64 - 5000, Connections: math.MaxInt64 - 3000}
	d.Openings = append(d.Openings, DestinationOpening{Ordinal: MaxJobDestinations, Start: 1_794_650_008,
		SentBytes: 5, ReceivedBytes: 6, Connections: 1})

	return d
}

// A SPLIT IS KEPT AS SENT, at its bound and with its widest counts, beside the
// destinations it splits; and destinations sent without one read back without
// one, never with an empty split.
func TestOpeningsAreKeptAsSent(t *testing.T) {
	for name, dests := range map[string]*JobDestinations{
		"as many openings as are kept": splitDestinations(),
		"no split": func() *JobDestinations {
			d := splitDestinations().WithoutOpenings()
			return &d
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)
			usage := measuredUsage()
			usage.Destinations = dests
			if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, nil); err != nil {
				t.Fatalf("RecordLeaseUsage: %v", err)
			}
			got, err := a.LeaseUsage(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("LeaseUsage: %v", err)
			}
			if !reflect.DeepEqual(got.JobUsage, usage) {
				t.Errorf("read back %d openings every %d s,\nwant %d every %d s", len(got.Destinations.Openings),
					got.Destinations.OpeningSeconds, len(dests.Openings), dests.OpeningSeconds)
			}
		})
	}
}

// A ROW THE LEDGER DOES NOT HOLD AS WRITTEN IS REFUSED, never read as a split:
// rows the packing cannot hold, a field that is not digits, and openings that
// no longer split the destinations they are read beside.
func TestOpeningsTheLedgerDoesNotHoldAsWrittenAreRefused(t *testing.T) {
	t.Parallel()

	good, err := packOpenings(splitDestinations().Openings[:2])
	if err != nil {
		t.Fatalf("packOpenings: %v", err)
	}
	for _, tc := range []struct {
		name   string
		packed string
		rows   int64
	}{
		{"a row short", good[:len(good)-1], 2},
		{"a row more than the count", good, 1},
		{"a negative count", good, -1},
		{"a field that is not digits", strings.Replace(good, "0", " ", 1), 2},
		{"a sign inside a field", "-" + good[1:], 2},
	} {
		if got, err := unpackOpenings(tc.packed, tc.rows); err == nil {
			t.Errorf("%s: read %+v, want refused", tc.name, got)
		}
	}
	got, err := unpackOpenings(good, 2)
	if err != nil || !reflect.DeepEqual(got, splitDestinations().Openings[:2]) {
		t.Errorf("unpackOpenings(packOpenings(x)) = %+v, %v; want x", got, err)
	}
	if _, err := packOpenings([]DestinationOpening{{Ordinal: MaxJobDestinations + 1, Start: 4}}); err == nil {
		t.Error("an ordinal past the traffic beyond the destinations was packed")
	}
}

// A JOB'S SPLIT IS RETIRED WITH ITS DESTINATIONS, by the same stamp, and its
// summary is kept.
func TestAJobsOpeningsAreRetiredWithItsDestinations(t *testing.T) {
	now := time.Now().UTC()
	a := retentionFleet(t, &now)

	lease := busyLease(t, a)
	usage := measuredUsage()
	usage.Destinations = splitDestinations()
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, nil); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_usage SET recorded_at = $1 WHERE lease_id = $2`,
			cutoff.Add(-time.Hour).Format(time.RFC3339Nano), lease.ID)
		return err
	}); err != nil {
		t.Fatalf("date the job's usage: %v", err)
	}
	openings := func() int {
		t.Helper()
		var n int
		if err := a.db.View(t.Context(), func(q querier) error {
			return q.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM job_destination_openings WHERE lease_id = $1`, lease.ID).Scan(&n)
		}); err != nil {
			t.Fatalf("count the job's openings: %v", err)
		}
		return n
	}
	if openings() != 1 {
		t.Fatal("the job's openings were not recorded")
	}

	if got, err := retire(t, a, cutoff, 10); err != nil || got.Jobs != 1 {
		t.Fatalf("retired %+v (%v), want the job", got, err)
	}
	if openings() != 0 {
		t.Error("the job's openings outlived its retirement")
	}
	if got, err := a.LeaseUsage(t.Context(), lease.ID); err != nil || got.Destinations != nil {
		t.Errorf("after retirement the job's usage is %+v (%v); want its summary with no destinations", got, err)
	}
}
