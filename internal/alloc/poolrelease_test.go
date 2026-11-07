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

// decommissionedOnProof marks a node as `billet nodes decommission` does on the compute
// barrier's proof that it runs nothing.
func decommissionedOnProof(t *testing.T, a *Allocator, node string) {
	t.Helper()
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE nodes SET decommission_proven = 1, live = 0, drained = 1 WHERE name = $1`, node)

		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A JOINED HOST LEAVES PLACEMENT WHEN ITS RELEASE IS ASKED FOR, AND IS SEALED ONLY ON
// PROOF IT RUNS NOTHING: no lease, and a decommission on the compute barrier's proof
// that no inventory of its current registration since contradicts. An inventory alone,
// however empty, is never that proof. The seal revokes its certificate first, and a
// registration after it does not put the host back into placement.
func TestAJoinedHostIsSealedOnlyOnProofItRunsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedOrder(t, a, "pa", "0a")
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

	for _, l := range leases {
		if err := a.Release(ctx, l.ID, l.Epoch, PhaseDone); err != nil {
			t.Fatalf("release lease %s: %v", l.ID, err)
		}
	}
	now = now.Add(time.Hour)
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}
	waits("an empty inventory and nothing else")

	decommissionedOnProof(t, a, node)
	if _, err := a.Reconcile(ctx, node, []string{"some-instance"}); err != nil {
		t.Fatal(err)
	}
	waits("a decommission an inventory since contradicts")
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}

	proof, err := a.SealPoolRelease(ctx, "pa")
	if err != nil || proof.For("i-pa") != nil {
		t.Fatalf("sealing a host proved to run nothing: %v", err)
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
	if err := a.PoolOrderGone(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolGone || o.Reason != "its source said the host is gone" {
		t.Fatalf("the order: %+v, want gone on its source's word", o)
	}
}

// A HOST ADMITTED BUT WHOSE NODE NEVER REGISTERED WAS NEVER PLACEABLE, so it is sealed
// as soon as nothing holds it, its credentials revoked; a first registration after the
// seal is not placeable.
func TestAnAdmittedHostThatNeverRegisteredIsSealed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	tok := placedOrder(t, a, "pa")
	if _, err := a.AdmitPoolEnrollment(ctx, admission(t, a, "pa", "SHA256:pa", tok, "0a")); err != nil {
		t.Fatal(err)
	}
	if sealed, err := a.RequestPoolRelease(ctx, "pa", "it never came up"); err != nil || sealed {
		t.Fatalf("releasing an admitted host: %v, %v", sealed, err)
	}
	proof, err := a.SealPoolRelease(ctx, "pa")
	if err != nil || proof.For("i-pa") != nil {
		t.Fatalf("sealing a host that never registered: %v", err)
	}
	if revoked, err := a.CertRevoked(ctx, "0a"); err != nil || !revoked {
		t.Fatalf("its certificate was not revoked: %v, %v", revoked, err)
	}
	registerArchNode(t, a, PoolNodeName("pa"), config.ArchX64)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a first registration after the seal is placeable")
	}
}

// AN UNJOINED ORDER'S SEAL FENCES ITS NAME: a node registering under the name the order
// would have given it is not placeable, though nothing linked it.
func TestASealedUnjoinedOrderFencesItsName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	if sealed, err := a.RequestPoolRelease(ctx, "pa", "it never booted"); err != nil || !sealed {
		t.Fatalf("seal: %v, %v", sealed, err)
	}
	registerArchNode(t, a, PoolNodeName("pa"), config.ArchX64)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a node under a sealed order's name is placeable")
	}
	registerArchNode(t, a, PoolNodeName("pa"), config.ArchX64)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a node registering again under a sealed order's name is placeable")
	}
}

// A SEALED OR RELEASED ORDER A PERSON CLOSES SAYS SO, and no other is closed by hand.
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

// WHAT A REGISTERED NODE IS PROVED TO RUN FOR ITS RELEASE: nothing only on a
// decommission on the compute barrier's proof, and then not if an inventory of its
// current registration reports compute; an inventory alone proves nothing.
func TestWhatARegisteredNodeIsProvedToRun(t *testing.T) {
	t.Parallel()
	inventory := func(epoch, running int64) ledgerdb.ReadNodeReleaseFactsRow {
		return ledgerdb.ReadNodeReleaseFactsRow{
			Epoch:     2,
			NodeEpoch: sql.NullInt64{Int64: epoch, Valid: true},
			Running:   sql.NullInt64{Int64: running, Valid: true},
		}
	}
	proven := func(f ledgerdb.ReadNodeReleaseFactsRow) ledgerdb.ReadNodeReleaseFactsRow {
		f.DecommissionProven = 1

		return f
	}
	for name, c := range map[string]struct {
		facts ledgerdb.ReadNodeReleaseFactsRow
		want  int
	}{
		"an empty inventory alone":                   {inventory(2, 0), -1},
		"decommissioned on proof":                    {proven(ledgerdb.ReadNodeReleaseFactsRow{Epoch: 2}), 0},
		"decommissioned, reporting nothing":          {proven(inventory(2, 0)), 0},
		"decommissioned, reporting compute":          {proven(inventory(2, 3)), 3},
		"decommissioned, an old registration's word": {proven(inventory(1, 3)), 0},
	} {
		if got := releaseInventory(&c.facts); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
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
