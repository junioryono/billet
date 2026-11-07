package alloc

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/releaseproof"
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
// THE LEDGER SHOWS IT HOLDS NOTHING: no lease, and an inventory from its current
// registration received after placement stopped that reports nothing running. The seal
// revokes its certificate first, and a registration after it does not put the host back
// into placement.
func TestAJoinedHostIsSealedOnlyOnceItHoldsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedOrder(t, a, "pa", "0a")

	leases, err := a.Escrow(ctx, "intel", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("a lease on the host: %v, %v", leases, err)
	}
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
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

	waits := func(why string) {
		t.Helper()
		if _, err := a.SealPoolRelease(ctx, "pa"); !errors.Is(err, ErrPoolReleaseWaits) {
			t.Fatalf("%s: sealing answered %v", why, err)
		}
		if o := readOrder(t, a, "pa"); o.State != PoolPlaced {
			t.Fatalf("%s: a refused seal wrote %s", why, o.State)
		}
	}
	waits("an inventory from before placement stopped")
	now = now.Add(time.Second)
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}
	waits("a lease still outstanding")
	for _, l := range leases {
		if err := a.Release(ctx, l.ID, l.Epoch, PhaseDone); err != nil {
			t.Fatalf("release lease %s: %v", l.ID, err)
		}
	}
	if _, err := a.RegisterNode(ctx, NodeRegistration{Name: node, Provider: config.ProviderFirecracker, VCPU: 8,
		Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64}}); err != nil {
		t.Fatal(err)
	}
	if placeableHosts(t, a) != 0 {
		t.Fatal("a registration put a host whose release was asked for back into placement")
	}
	waits("an inventory from an earlier registration")
	now = now.Add(time.Second)
	if _, err := a.Reconcile(ctx, node, []string{"some-instance"}); err != nil {
		t.Fatal(err)
	}
	waits("an inventory reporting something running")
	epoch, err := a.RegisterNode(ctx, NodeRegistration{Name: node, Provider: config.ProviderFirecracker, VCPU: 8,
		Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64}, Incarnation: "withdrawn"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.NodeWithdrawn(ctx, node, epoch, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}
	waits("a node that is not live")
	if _, err := a.RegisterNode(ctx, NodeRegistration{Name: node, Provider: config.ProviderFirecracker, VCPU: 8,
		Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}

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
	if _, err := a.RegisterNode(ctx, NodeRegistration{Name: node, Provider: config.ProviderFirecracker, VCPU: 8,
		Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64}}); err != nil {
		t.Fatal(err)
	}
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
	if o := readOrder(t, a, "pa"); o.State != PoolGone {
		t.Fatalf("the order is %s, want gone", o.State)
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
