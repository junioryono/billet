package alloc

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fullDestinations is the most a report can name: every destination the node
// keeps by name, traffic beyond them, the tap read, and the totals incomplete.
func fullDestinations() *JobDestinations {
	d := &JobDestinations{
		Other:      JobDestination{SentBytes: 7, ReceivedBytes: 11, Connections: 3},
		Incomplete: true,
		Tap:        &TapTotals{SentBytes: 1 << 20, ReceivedBytes: 0},
	}
	for i := range MaxJobDestinations {
		d.Destinations = append(d.Destinations, JobDestination{
			Addr:      fmt.Sprintf("10.%d.%d.%d", i/65536, i/256%256, i%256),
			SentBytes: int64(1000 - i), ReceivedBytes: int64(2 * i), Connections: int64(i % 5),
		})
	}
	d.Destinations[0].Addr = "2001:db8::1"

	return d
}

// EVERY SHAPE OF DESTINATIONS IS KEPT AS SENT: all 256 named in the node's
// order with the rest and the tap, none named at all (a job whose flows were
// totalled and came to nothing, which is not the same as not totalled), and no
// destinations, which reads back as none rather than as an empty total.
func TestDestinationsAreKeptAsSent(t *testing.T) {
	for name, dests := range map[string]*JobDestinations{
		"every destination named": fullDestinations(),
		"none named, no tap":      {},
		"complete, the tap read": {Destinations: []JobDestination{{Addr: "140.82.112.3", SentBytes: 1,
			ReceivedBytes: 2, Connections: 1}}, Tap: &TapTotals{SentBytes: 5, ReceivedBytes: 9}},
		"not totalled": nil,
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
				t.Errorf("read back %+v,\nwant %+v", got.Destinations, usage.Destinations)
			}
		})
	}
}

// THE DESTINATIONS ARE THE WINNING REPORT'S. A first report without them and a
// retry carrying them would otherwise be stored as a pair neither request sent,
// and a retry's destinations must not be added to the first's.
func TestDestinationsComeOnlyWithTheReportThatWon(t *testing.T) {
	for name, tc := range map[string]struct{ first, second *JobDestinations }{
		"the first had none":  {nil, fullDestinations()},
		"the first had some":  {&JobDestinations{Destinations: []JobDestination{{Addr: "10.0.0.1", SentBytes: 1}}}, fullDestinations()},
		"the retry has fewer": {fullDestinations(), &JobDestinations{}},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)
			first, second := measuredUsage(), measuredUsage()
			first.Destinations, second.Destinations = tc.first, tc.second
			if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, first, nil); err != nil {
				t.Fatalf("RecordLeaseUsage: %v", err)
			}
			if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, second, nil); err != nil {
				t.Fatalf("RecordLeaseUsage (retry): %v", err)
			}
			got, err := a.LeaseUsage(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("LeaseUsage: %v", err)
			}
			if !reflect.DeepEqual(got.Destinations, tc.first) {
				t.Errorf("read back %+v, want the first report's %+v", got.Destinations, tc.first)
			}
		})
	}
}

// A STALE HOLDER'S DESTINATIONS ARE NOT KEPT, AND NEITHER ARE ANY AFTER
// RELEASE: they ride the usage report, under its fence, so the lease's current
// holder can still report its own afterwards.
func TestAStaleReportStoresNoDestinations(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	stale := measuredUsage()
	stale.Destinations = fullDestinations()
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch+1, stale, nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("a report at a stale epoch = %v, want ErrFenced", err)
	}

	current := measuredUsage()
	current.Destinations = &JobDestinations{Destinations: []JobDestination{{Addr: "10.0.0.9", Connections: 1}}}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, current, nil); err != nil {
		t.Fatalf("the holder's own report after a stale one: %v", err)
	}
	got, err := a.LeaseUsage(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsage: %v", err)
	}
	if !reflect.DeepEqual(got.Destinations, current.Destinations) {
		t.Errorf("read back %+v, want only the holder's %+v", got.Destinations, current.Destinations)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	other := busyLease(t, a)
	if err := a.Release(t.Context(), other.ID, other.Epoch, PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.RecordLeaseUsage(t.Context(), other.ID, other.Epoch, stale, nil); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("a report after release = %v, want ErrLeaseNotFound", err)
	}
	if _, err := a.LeaseUsage(t.Context(), other.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("LeaseUsage of a lease whose only report came after release = %v, want ErrLeaseNotFound", err)
	}
}

// A VERDICT WHOSE REST IS MISSING IS NOT READ AS NOTHING BEYOND THE NAMED
// DESTINATIONS: the ledger no longer holds what was written with it.
func TestDestinationsWithoutTheirRestAreNotGuessed(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)
	usage := measuredUsage()
	usage.Destinations = &JobDestinations{Destinations: []JobDestination{{Addr: "10.0.0.1", SentBytes: 1}}}
	if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, nil); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM job_destinations WHERE lease_id = $1 AND addr = ''`,
			lease.ID)
		return err
	}); err != nil {
		t.Fatalf("remove the rest's row: %v", err)
	}
	if _, err := a.LeaseUsage(t.Context(), lease.ID); err == nil ||
		!strings.Contains(err.Error(), "without the row for the traffic beyond them") {
		t.Fatalf("LeaseUsage = %v, want it to say the rest's row is missing", err)
	}
}

func TestDestinationsTheLedgerCannotKeepAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*JobUsage)
		want   string
	}{
		{"one more than the node keeps by name", func(u *JobUsage) {
			u.Destinations.Destinations = append(u.Destinations.Destinations, JobDestination{Addr: "10.255.0.0"})
		}, "names 257 destinations"},
		{"a name that is not an address", func(u *JobUsage) {
			u.Destinations.Destinations[3].Addr = "example.com"
		}, `"example.com" is not an address`},
		{"an empty address", func(u *JobUsage) { u.Destinations.Destinations[3].Addr = "" }, `"" is not an address`},
		{"an address not in its canonical spelling", func(u *JobUsage) {
			u.Destinations.Destinations[0].Addr = "2001:DB8::1"
		}, `"2001:DB8::1" is not an address in its canonical form`},
		{"an address with a zone", func(u *JobUsage) {
			u.Destinations.Destinations[0].Addr = "fe80::1%eth0\nlease      forged"
		}, "not an address in its canonical form"},
		{"one address named twice", func(u *JobUsage) {
			u.Destinations.Destinations[5].Addr = u.Destinations.Destinations[9].Addr
		}, "10.0.0.9 is named twice"},
		{"the rest naming an address", func(u *JobUsage) {
			u.Destinations.Other.Addr = "10.0.0.1"
		}, `beyond the named destinations names an address ("10.0.0.1")`},
		{"a negative total", func(u *JobUsage) {
			u.Destinations.Destinations[7].ReceivedBytes = -1
		}, "destination 10.0.0.7 has a negative total"},
		{"a negative rest", func(u *JobUsage) {
			u.Destinations.Other.Connections = -1
		}, "destination other has a negative total"},
		{"a negative tap", func(u *JobUsage) { u.Destinations.Tap.SentBytes = -1 }, "tap's totals are negative"},
		{"a tap beside a network unmeasured", func(u *JobUsage) {
			u.Unmeasured = append(u.Unmeasured, UsageNet)
			u.NetRxBytes, u.NetTxBytes, u.NetRxPackets, u.NetTxPackets = 0, 0, 0, 0
		}, "names net unmeasured and gives the tap's totals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u := measuredUsage()
			u.Destinations = fullDestinations()
			tc.mutate(&u)
			if err := u.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}

	u := measuredUsage()
	u.Destinations = fullDestinations()
	if err := u.Validate(); err != nil {
		t.Fatalf("the most a report can name was refused: %v", err)
	}
	u.Destinations.Tap = nil
	u.Unmeasured = append(u.Unmeasured, UsageNet)
	u.NetRxBytes, u.NetTxBytes, u.NetRxPackets, u.NetTxPackets = 0, 0, 0, 0
	if err := u.Validate(); err != nil {
		t.Fatalf("destinations with no tap beside a network unmeasured were refused: %v", err)
	}
}
