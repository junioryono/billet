package alloc

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// joinedPoolHost places order id, admits a node from it with certificate serial, and
// registers that node.
func joinedPoolHost(t *testing.T, a *Allocator, id, serial string) {
	t.Helper()
	tok := placedOrder(t, a, id)
	if _, err := a.AdmitPoolEnrollment(t.Context(), admission(t, a, id, "SHA256:"+id, tok, serial)); err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
	registerPoolNode(t, a, PoolNodeName(id))
}

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
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now, 0, "idle"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("releasing a host that holds a lease answered %v", err)
	}
	if o := readOrder(t, a, "pa"); !o.ReleaseRequested.IsZero() {
		t.Fatal("a refused idle release asked for the release")
	}

	// A LEASE THAT FAILED holds nothing either, and its end starts the host's idleness.
	ended := start.Add(15 * time.Minute)
	now = ended
	if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseFailed); err != nil {
		t.Fatal(err)
	}
	idle, err = a.PoolIdleHosts(ctx)
	if err != nil || len(idle) != 1 || !idle[0].Since.Equal(ended) {
		t.Fatalf("after its job: %+v, %v; want it idle since the job ended", idle, err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", ended.Add(-time.Second), 0, "idle"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("a host idle for less than the cutoff answered %v", err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", ended, 0, ""); err == nil {
		t.Fatal("an idle release with no reason was asked for")
	}

	now = ended.Add(30 * time.Minute)
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now.Add(-20*time.Minute), 0, "idle past idle_after"); err != nil {
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
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now, 0, "again"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("a second idle release answered %v", err)
	}
}

// THE FLOOR IS COUNTED IN THE RELEASE'S OWN TRANSACTION: a person's release of one of two
// hosts, committed after the scaler read the pool, leaves the other needed for a floor of
// one, and the scaler's release of it is refused and writes nothing.
func TestAnIdleReleaseCountsTheFloorInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	joinedPoolHost(t, a, "pa", "0a")
	joinedPoolHost(t, a, "pb", "0b")
	now = now.Add(time.Hour)
	cutoff := now.Add(-20 * time.Minute)

	if idle, err := a.PoolIdleHosts(ctx); err != nil || len(idle) != 2 {
		t.Fatalf("two idle hosts: %+v, %v", idle, err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pb", "a person's word"); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", cutoff, 1, "idle"); !errors.Is(err, ErrPoolAtFloor) {
		t.Fatalf("releasing the floor's last host answered %v", err)
	}
	if o := readOrder(t, a, "pa"); !o.ReleaseRequested.IsZero() {
		t.Fatal("a release refused at the floor was recorded")
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", cutoff, 0, "idle"); err != nil {
		t.Fatalf("with no floor: %v", err)
	}
}

// A HOST ADMITTED WHOSE NODE NEVER REGISTERED SERVES NOTHING: its order names its node from
// the approval, but it is not counted as serving, so it holds no floor up, and its own
// release takes nothing from the floor.
func TestAnUnregisteredHostHoldsNoFloor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	joinedPoolHost(t, a, "pa", "0a")
	tok := placedOrder(t, a, "pb")
	if _, err := a.AdmitPoolEnrollment(ctx, admission(t, a, "pb", "SHA256:pb", tok, "0b")); err != nil {
		t.Fatalf("admit pb: %v", err)
	}
	if o := readOrder(t, a, "pb"); o.NodeName != PoolNodeName("pb") {
		t.Fatalf("pb's order names %q; the approval links its node before it registers", o.NodeName)
	}
	if serving, err := a.PoolServingHosts(ctx); err != nil || serving[config.ArchX64] != 1 {
		t.Fatalf("serving = %v, %v; want only the registered host", serving, err)
	}
	now = now.Add(time.Hour)
	cutoff := now.Add(-20 * time.Minute)
	if err := a.ReleaseIdlePoolHost(ctx, "pa", cutoff, 1, "idle"); !errors.Is(err, ErrPoolAtFloor) {
		t.Fatalf("releasing the floor's only registered host answered %v", err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pb", cutoff, 1, "idle"); err != nil {
		t.Fatalf("releasing a host that never registered, at the floor: %v", err)
	}
}

// AN UNREADABLE TIME IN THE LEDGER IS NEVER IDLE: a host whose newest job's end the
// ledger holds in a form this binary cannot parse is not listed, and its release is
// refused, writing nothing.
func TestAnUnreadableIdleTimeInTheLedgerIsNeverIdle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	joinedPoolNode(t, a)
	leases, err := a.Escrow(ctx, "intel", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("a lease on the host: %v, %v", leases, err)
	}
	if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE job_history SET finished_at = 'yesterday' WHERE lease_id = $1`, leases[0].ID)

		return err
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if idle, err := a.PoolIdleHosts(ctx); err != nil || len(idle) != 0 {
		t.Fatalf("a host with an unreadable job end is listed idle: %+v, %v", idle, err)
	}
	if err := a.ReleaseIdlePoolHost(ctx, "pa", now, 0, "idle"); !errors.Is(err, ErrPoolHostNotIdle) {
		t.Fatalf("releasing it answered %v", err)
	}
	if o := readOrder(t, a, "pa"); !o.ReleaseRequested.IsZero() {
		t.Fatal("a host with an unreadable job end had its release asked for")
	}
}

// AN UNREADABLE TIME IS NEVER IDLE, and the later of the two times is the one read: a
// time this binary cannot parse makes the answer unknown rather than old.
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
		t.Errorf("a job end after the admission: %v, %t", since, read)
	}
	if since, read := idleSince(ok, later); !read || since.Format(timestampFormat) != later {
		t.Errorf("an admission after the job end: %v, %t", since, read)
	}
}
