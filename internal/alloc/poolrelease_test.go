package alloc

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/releaseproof"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// joinedOrder places an order, admits a node from it with certificate serial, and
// registers that node as a placeable x64 Firecracker host.
func joinedOrder(t *testing.T, a *Allocator, id, serial string) string {
	t.Helper()
	tok := placedOrder(t, a, id)
	if _, err := a.AdmitPoolEnrollment(t.Context(), admission(t, a, id, "SHA256:"+id, tok, serial)); err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
	node := PoolNodeName(id)
	registerArchNode(t, a, node, config.ArchX64)

	return node
}

func placeableHosts(t *testing.T, a *Allocator) int {
	t.Helper()
	_, _, hosts, err := a.PlaceableContribution(t.Context())
	if err != nil {
		t.Fatalf("placeable: %v", err)
	}

	return hosts
}

// A HOST NOTHING JOINED FROM IS SEALED BY THE ASKING (ADR-030): it has no drain to prove,
// so the order is sealed in the asking's transaction, its tokens retired so nothing
// joins from it again, and the seal is the proof its source is asked with. A release
// asked twice, or of an order that is not placed, moves nothing.
func TestANeverJoinedHostIsSealedByTheAsking(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")

	sealed, err := a.RequestPoolRelease(ctx, "pa", "it never booted")
	if err != nil || !sealed {
		t.Fatalf("release a host nothing joined from: sealed %v, %v", sealed, err)
	}
	o := readOrder(t, a, "pa")
	if o.State != PoolSealed || o.ReleaseReason != "it never booted" || !o.ReleaseRequested.Equal(now) {
		t.Fatalf("the sealed order: %+v", o)
	}
	if left := usesLeft(t, a, "pa"); len(left) != 1 || left[0] != 0 {
		t.Fatalf("its tokens were not retired: %v", left)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, PoolNodeName("pa"), "SHA256:k", "csr", tok); err == nil {
		t.Fatal("a sealed order took a request")
	}
	if _, err := a.RequestPoolRelease(ctx, "pa", "again"); !errors.Is(err, ErrPoolOrderMoved) {
		t.Fatalf("a second release answered %v", err)
	}
	proof, err := a.PoolReleaseProof(ctx, "pa")
	if err != nil || proof.For("i-pa") != nil || proof.For("i-other") == nil {
		t.Fatalf("the seal's proof: %v, for its host %v", err, proof.For("i-pa"))
	}

	if err := a.RecordPoolOrder(ctx, poolOrder("pb")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pb", "being ordered"); !errors.Is(err, ErrPoolOrderMoved) {
		t.Fatalf("releasing an order being ordered answered %v", err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pa", ""); err == nil {
		t.Fatal("a release with no reason was taken")
	}
}

// A HOST ANYTHING WAS EVER ADMITTED FOR IS NOT RELEASED AS ONE THAT NEVER JOINED: a
// certificate issued under the order's name is a node that could have held work, so the
// asking seals nothing.
func TestAHostACertificateWasIssuedForIsNotUnjoined(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	if err := a.RecordIssuedCert(ctx, IssuedCert{Serial: "0f", Node: PoolNodeName("pa"), Source: CertIssued,
		NotAfter: "2027-11-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pa", "never joined?"); !errors.Is(err, ErrPoolReleaseWaits) ||
		!errors.Is(err, releaseproof.ErrNoProof) {
		t.Fatalf("a host a certificate was issued for answered %v", err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolPlaced || !o.ReleaseRequested.IsZero() {
		t.Fatalf("a refused release wrote %+v", o)
	}
}

// A JOINED HOST LEAVES PLACEMENT WHEN ITS RELEASE IS ASKED FOR, AND IS SEALED ONLY ONCE
// THE LEDGER PROVES IT HOLDS NOTHING: no lease, and, once the quiet period has passed,
// a second inventory from its current registration, listed after the first counted one
// arrived, that reports nothing running. The seal revokes its certificate first, and a
// registration after it does not put the host back into placement.
func TestAJoinedHostIsSealedOnlyOnceItHoldsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedOrder(t, a, "pa", "0a")
	report := func(running ...string) {
		t.Helper()
		now = now.Add(time.Second)
		if _, err := a.Reconcile(ctx, node, running); err != nil {
			t.Fatal(err)
		}
	}
	register := func() {
		t.Helper()
		if _, err := a.RegisterNode(ctx, NodeRegistration{Name: node, Provider: config.ProviderFirecracker, VCPU: 8,
			Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64}}); err != nil {
			t.Fatal(err)
		}
	}
	waits := func(why string) {
		t.Helper()
		if _, err := a.SealPoolRelease(ctx, "pa"); !errors.Is(err, ErrPoolReleaseWaits) {
			t.Fatalf("%s: sealing answered %v", why, err)
		}
		if o := readOrder(t, a, "pa"); o.State != PoolPlaced {
			t.Fatalf("%s: a refused seal wrote %s", why, o.State)
		}
	}

	leases, err := a.Escrow(ctx, "intel", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("a lease on the host: %v, %v", leases, err)
	}
	if placeableHosts(t, a) != 1 {
		t.Fatal("the joined host is not placeable")
	}

	now = now.Add(time.Second)
	if sealed, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil || sealed {
		t.Fatalf("asking for a joined host's release: sealed %v, %v", sealed, err)
	}
	if placeableHosts(t, a) != 0 {
		t.Fatal("a host whose release was asked for is still placeable")
	}
	if more, err := a.Escrow(ctx, "intel", 1); err != nil || len(more) != 0 {
		t.Fatalf("placed %d more leases on a host being released: %v", len(more), err)
	}
	register()
	if placeableHosts(t, a) != 0 {
		t.Fatal("a registration put a host whose release was asked for back into placement")
	}

	now = now.Add(poolReleaseQuiet)
	report()
	report()
	waits("a lease still outstanding")
	for _, l := range leases {
		if err := a.Release(ctx, l.ID, l.Epoch, PhaseDone); err != nil {
			t.Fatalf("release lease %s: %v", l.ID, err)
		}
	}
	// THE REPORT THE LAST SEAL COUNTED FIRST IS NOT THE ONE THAT PROVES: a report the
	// node listed after it arrived is.
	waits("only the report the last attempt counted first")
	report()

	proof, err := a.SealPoolRelease(ctx, "pa")
	if err != nil || proof.For("i-pa") != nil {
		t.Fatalf("sealing a host that holds nothing: %v", err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolSealed {
		t.Fatalf("the order is %s, want sealed", o.State)
	}
	if revoked, err := a.CertRevoked(ctx, "0a"); err != nil || !revoked {
		t.Fatalf("the host's certificate was not revoked: %v, %v", revoked, err)
	}
	register()
	if placeableHosts(t, a) != 0 {
		t.Fatal("a registration put a sealed host back into placement")
	}

	if _, err := a.PoolReleaseProof(ctx, "pa"); err != nil {
		t.Fatalf("the sealed order's proof: %v", err)
	}
	if err := a.PoolOrderGone(ctx, "pa"); !errors.Is(err, ErrPoolOrderMoved) {
		t.Fatalf("gone before released answered %v", err)
	}
	if err := a.PoolOrderReleased(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if err := a.ClosePoolOrder(ctx, "pa", ""); err == nil {
		t.Fatal("a closure without a reason was taken")
	}
	if err := a.PoolOrderGone(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolGone || o.Reason != "its source said the host is gone" {
		t.Fatalf("the order: %+v, want gone on its source's word", o)
	}
}

// A RELEASED ORDER A PERSON CLOSES SAYS SO, and only a released order is closed by hand.
func TestAReleasedOrderAPersonClosesSaysSo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	if err := a.ClosePoolOrder(ctx, "pa", "looked"); !errors.Is(err, ErrPoolOrderMoved) {
		t.Fatalf("closing a placed order answered %v", err)
	}
	if _, err := a.RequestPoolRelease(ctx, "pa", "never booted"); err != nil {
		t.Fatal(err)
	}
	if err := a.PoolOrderReleased(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if err := a.ClosePoolOrder(ctx, "pa", "the console lists no such server"); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolGone || o.Reason != "closed by a person: the console lists no such server" {
		t.Fatalf("the closed order: %+v", o)
	}
}

// A HOST WHOSE RELEASE WAS ASKED FOR BEFORE ITS NODE FIRST REGISTERED IS NOT PLACEABLE
// WHEN IT DOES: the first registration is held to the release as every later one is.
func TestAFirstRegistrationAfterTheReleaseIsNotPlaceable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	if _, err := a.AdmitPoolEnrollment(ctx, admission(t, a, "pa", "SHA256:pa", tok, "0a")); err != nil {
		t.Fatal(err)
	}
	if sealed, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil || sealed {
		t.Fatalf("releasing a joined host before its node registered: %v, %v", sealed, err)
	}
	registerArchNode(t, a, PoolNodeName("pa"), config.ArchX64)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a first registration put a host whose release was asked for into placement")
	}
	if leases, err := a.Escrow(ctx, "intel", 1); err != nil || len(leases) != 0 {
		t.Fatalf("placed %d leases on it: %v", len(leases), err)
	}
}

// WHAT A NODE'S INVENTORY PROVES FOR ITS RELEASE: nothing until the quiet period has
// passed, nothing from a node that is not live or a registration before the current
// one, and only the second report counted, which was listed after the first arrived;
// a launch dispatched since starts the proof again, and a decommission on the compute
// barrier's proof is proof it runs nothing.
func TestWhatANodesInventoryProvesForItsRelease(t *testing.T) {
	t.Parallel()
	requested := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return ts(requested.Add(poolReleaseQuiet + d)) }
	first := at(time.Second)
	cases := map[string]struct {
		order     ledgerdb.PoolOrder
		facts     ledgerdb.ReadNodeReleaseFactsRow
		inventory int
		dispatch  int64
		reportAt  string
	}{
		"decommissioned on proof": {
			facts: ledgerdb.ReadNodeReleaseFactsRow{DecommissionProven: 1}, inventory: 0,
		},
		"a launch dispatched since": {
			order: ledgerdb.PoolOrder{ReleaseDispatch: 3, ReleaseReportAt: first},
			facts: reportingNode(4, at(time.Hour), 0), inventory: -1, dispatch: 4,
		},
		"not live": {
			facts:     func() ledgerdb.ReadNodeReleaseFactsRow { f := reportingNode(0, at(time.Hour), 0); f.Live = 0; return f }(),
			inventory: -1,
		},
		"an earlier registration's": {
			facts: func() ledgerdb.ReadNodeReleaseFactsRow {
				f := reportingNode(0, at(time.Hour), 0)
				f.Epoch = 9
				return f
			}(),
			inventory: -1,
		},
		"within the quiet period":  {facts: reportingNode(0, ts(requested.Add(poolReleaseQuiet)), 0), inventory: -1},
		"the first counted report": {facts: reportingNode(0, first, 0), inventory: -1, reportAt: first},
		"the first counted report again": {
			order: ledgerdb.PoolOrder{ReleaseReportAt: first}, facts: reportingNode(0, first, 0), inventory: -1, reportAt: first,
		},
		"a second report, empty": {
			order: ledgerdb.PoolOrder{ReleaseReportAt: first}, facts: reportingNode(0, at(time.Minute), 0),
			inventory: 0, reportAt: first,
		},
		"a second report, running": {
			order: ledgerdb.PoolOrder{ReleaseReportAt: first}, facts: reportingNode(0, at(time.Minute), 2),
			inventory: 2, reportAt: first,
		},
	}
	for name, c := range cases {
		inventory, dispatch, reportAt, err := releaseInventory(&c.order, &c.facts, requested)
		if err != nil || inventory != c.inventory || dispatch != c.dispatch || reportAt != c.reportAt {
			t.Errorf("%s: %d, %d, %q, %v; want %d, %d, %q", name, inventory, dispatch, reportAt, err,
				c.inventory, c.dispatch, c.reportAt)
		}
	}
}

// reportingNode is a live node at epoch 1 whose current inventory arrived at received with
// running instances, and dispatch launches dispatched to it.
func reportingNode(dispatch int64, received string, running int64) ledgerdb.ReadNodeReleaseFactsRow {
	return ledgerdb.ReadNodeReleaseFactsRow{
		Live: 1, Epoch: 1, DispatchGeneration: dispatch,
		NodeEpoch:  sql.NullInt64{Int64: 1, Valid: true},
		Running:    sql.NullInt64{Int64: running, Valid: true},
		ReceivedAt: sql.NullString{String: received, Valid: true},
	}
}

// A PERSON'S APPROVAL OF A REQUEST LANDING AFTER THE SEAL IS REFUSED WHOLE: the sealed
// order links no node and the request stays undecided.
func TestAnApprovalAfterTheSealIsRefusedWhole(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	spare, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HoldPoolJoin(ctx, "pa", "a key nobody can account for", readOrder(t, a, "pa").Updated); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecordHeldPoolRequest(ctx, "pa", "SHA256:k", "csr-SHA256:k"); err != nil {
		t.Fatal(err)
	}
	if sealed, err := a.RequestPoolRelease(ctx, "pa", "nobody could account for it"); err != nil || !sealed {
		t.Fatalf("sealing a held order nothing joined from: %v, %v", sealed, err)
	}
	if err := a.DecideEnrollment(ctx, PoolNodeName("pa"), "SHA256:k", EnrollApproved, "cert", "0z"); err == nil {
		t.Fatal("an approval after the seal was taken")
	}
	if e, _, err := a.LookupEnrollment(ctx, PoolNodeName("pa")); err != nil || e.State != EnrollPending {
		t.Fatalf("the refused approval decided the request: %+v, %v", e, err)
	}
	if o := readOrder(t, a, "pa"); o.NodeName != "" || o.State != PoolSealed {
		t.Fatalf("the refused approval linked the order: %+v", o)
	}
	if left := usesLeft(t, a, "pa"); !slices.Equal(left, []int{0, 0}) {
		t.Fatalf("tokens left after the seal: %v (spare %q, first %q)", left, spare, tok)
	}
}
