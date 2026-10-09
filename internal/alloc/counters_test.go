package alloc

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func count(n int64) *int64 { return &n }

// countedUsage is measuredUsage with the hardware counters of a job whose CPU
// had no frontend stall event.
func countedUsage() JobUsage {
	u := measuredUsage()
	u.Counters = &JobCounters{
		Cycles: count(4_000_000_000), Instructions: count(4_840_000_000),
		CacheReferences: count(0), CacheMisses: count(35_816_000), BranchMisses: count(9_000_000),
	}

	return u
}

// EVERY COUNTER IS KEPT AS SENT, AND ONE THAT WAS NOT COUNTED STAYS UNCOUNTED:
// a nil event reads back nil, never zero, and a counted zero reads back zero.
func TestHardwareCountersAreKeptAsSent(t *testing.T) {
	for name, usage := range map[string]JobUsage{
		"counted":     countedUsage(),
		"not counted": measuredUsage(),
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)
			if err := a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage, nil); err != nil {
				t.Fatalf("RecordLeaseUsage: %v", err)
			}
			got, err := a.LeaseUsage(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("LeaseUsage: %v", err)
			}
			if !reflect.DeepEqual(got.JobUsage, usage) {
				t.Errorf("read back %+v,\nwant %+v", got.Counters, usage.Counters)
			}
		})
	}
}

func TestHardwareCountersTheLedgerCannotKeepAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		counters JobCounters
		want     string
	}{
		{"a negative count", JobCounters{Cycles: count(10), BranchMisses: count(-1)}, "branch_misses is negative"},
		{"a block that counts nothing", JobCounters{}, "count nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u := measuredUsage()
			u.Counters = &tc.counters
			if err := u.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}
	if err := countedUsage().Validate(); err != nil {
		t.Fatalf("counters with one event uncounted were refused: %v", err)
	}
}
