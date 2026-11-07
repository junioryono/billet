package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A POOL HOST'S IDLENESS STARTS AT ITS NEWEST JOB'S END, OR ITS ADMISSION, and the pool's
// request for its release reads it again in its own transaction: a host holding a lease
// is not idle and nothing is written; a host idle for less than the cutoff stays; one idle
// past it is taken out of placement with the reason, and is idle no more.
func TestAPoolHostsIdlenessDecidesItsReleaseInOneTransaction(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	now := start
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedPoolNode(t, a)

	idle, err := a.PoolIdleHosts(ctx)
	if err != nil || len(idle) != 1 || idle[0].Order != "pa" || idle[0].Node != node ||
		idle[0].Arch != config.ArchX64 || !idle[0].Since.Equal(start) {
		t.Fatalf("a host just admitted: %+v, %v; want it idle since its admission", idle, err)
	}

	now = start.Add(10 * time.Minute)
	leases, err := a.Escrow(ctx, "intel", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("a lease on the host: %v, %v", leases, err)
	}
	if idle, err := a.PoolIdleHosts(ctx); err != nil || len(idle) != 0 {
		t.Fatalf("a host holding a lease is idle: %+v, %v", idle, err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now, "idle"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("releasing a host that holds a lease answered %v", err)
	}
	if o := readOrder(t, a, "pa"); !o.ReleaseRequested.IsZero() {
		t.Fatal("a refused idle release asked for the release")
	}

	ended := start.Add(15 * time.Minute)
	now = ended
	if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
		t.Fatal(err)
	}
	idle, err = a.PoolIdleHosts(ctx)
	if err != nil || len(idle) != 1 || !idle[0].Since.Equal(ended) {
		t.Fatalf("after its job: %+v, %v; want it idle since the job ended", idle, err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", ended.Add(-time.Second), "idle"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("a host idle for less than the cutoff answered %v", err)
	}

	now = ended.Add(30 * time.Minute)
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now.Add(-20*time.Minute), "idle past idle_after"); err != nil {
		t.Fatalf("a host idle past the cutoff: %v", err)
	}
	o := readOrder(t, a, "pa")
	if !o.ReleaseRequested.Equal(now) || o.ReleaseReason != "idle past idle_after" {
		t.Fatalf("the release asked for reads %v %q", o.ReleaseRequested, o.ReleaseReason)
	}
	if n := placeableHosts(t, a); n != 0 {
		t.Fatalf("%d hosts still placeable; the released host was not taken out of placement", n)
	}
	if idle, err := a.PoolIdleHosts(ctx); err != nil || len(idle) != 0 {
		t.Fatalf("a host whose release was asked for is still offered: %+v, %v", idle, err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now, "again"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("a second idle release answered %v", err)
	}
}

// AN UNREADABLE TIME IS NEVER IDLE: a time this binary cannot parse makes the answer
// unknown rather than old, so it never proposes a release.
func TestAnUnreadableIdleTimeIsNeverIdle(t *testing.T) {
	t.Parallel()
	ok := ts(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	for name, c := range map[string]struct{ finished, joined string }{
		"nothing recorded":         {"", ""},
		"an unreadable job end":    {"yesterday", ok},
		"an unreadable admission":  {ok, "2026-13-40"},
		"an unreadable both times": {"x", "y"},
	} {
		if since, read := idleSince(c.finished, c.joined); read {
			t.Errorf("%s: read as idle since %v", name, since)
		}
	}
	later := ts(time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC))
	if since, read := idleSince(later, ok); !read || since.Format(timestampFormat) != later {
		t.Errorf("the later of the two: %v, %t", since, read)
	}
}
