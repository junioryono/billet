package alloc

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

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
// certificate issued under the order's name before that name was reserved is a node that
// could have held work, so the asking seals nothing. It links the order to the name and
// releases it as a joined host, proved again from the link on: a decommission on proof
// taken before it, when the order did not know its node, seals nothing, and the seal on a
// fresh one revokes that certificate.
func TestAHostACertificateWasIssuedForIsReleasedAsJoined(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	placedOrder(t, a, "pa")
	node := PoolNodeName("pa")
	if err := a.RecordIssuedCert(ctx, IssuedCert{Serial: "0f", Node: node, Source: CertIssued,
		NotAfter: "2027-11-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	registerPoolNode(t, a, node)
	if placeableHosts(t, a) != 1 {
		t.Fatal("the node under the order's name is not placeable")
	}
	barrier := requestBarrier(t, a)
	decommissionOnProof(t, a, &now, barrier, node)

	if sealed, err := a.RequestPoolRelease(ctx, "pa", "joined by hand"); err != nil || sealed {
		t.Fatalf("a host a certificate was issued for: sealed %v, %v", sealed, err)
	}
	if o := readOrder(t, a, "pa"); o.State != PoolPlaced || o.NodeName != node || o.ReleaseRequested.IsZero() {
		t.Fatalf("the order after its release was asked for: %+v", o)
	}
	if left := usesLeft(t, a, "pa"); len(left) != 1 || left[0] != 0 {
		t.Fatalf("its tokens were not retired: %v", left)
	}
	waitsUnsealed(t, a, "a decommission taken before the order knew its node")

	registerPoolNode(t, a, node)
	if placeableHosts(t, a) != 0 {
		t.Fatal("the node under the order's name came back into placement")
	}
	decommissionOnProof(t, a, &now, barrier, node)
	if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
		t.Fatalf("sealing on a decommission proved since the link: %v", err)
	}
	if revoked, err := a.CertRevoked(ctx, "0f"); err != nil || !revoked {
		t.Fatalf("the certificate issued under its name was not revoked: %v, %v", revoked, err)
	}
}

// AN IDLE RUN THAT PREDATES A CONTRADICTION PROVES NO DECOMMISSION AFTER IT: an inventory
// reporting compute discards the pool node's run with the contradiction, so a delayed
// empty inventory and a stop leave nothing a decommission can be proved by, and the
// release waits for answers taken since.
func TestARunBeforeAContradictionProvesNoDecommission(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedPoolNode(t, a)
	if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
		t.Fatal(err)
	}
	barrier := requestBarrier(t, a)
	observeNode(t, a, barrier, node, true)
	now = now.Add(computeAbsenceGrace)
	observeNode(t, a, barrier, node, true)

	if _, err := a.Reconcile(ctx, node, []string{"some-instance"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.NodeGone(ctx, node, fenceOf(t, a, node).Epoch); err != nil {
		t.Fatal(err)
	}
	if proven, err := a.Decommission(ctx, DecommissionRequest{Node: node, Actor: "ops"}); err == nil || proven {
		t.Fatalf("a decommission proved by a run older than the compute reported since: proved %v, %v", proven, err)
	}
	waitsUnsealed(t, a, "a run older than the contradiction")
}

// registerPoolNode registers a pool host's node as a placeable x64 Firecracker host that
// answers the compute barrier.
func registerPoolNode(t *testing.T, a *Allocator, node string) {
	t.Helper()
	if _, err := a.RegisterNode(t.Context(), NodeRegistration{Name: node, Provider: config.ProviderFirecracker,
		VCPU: 8, Memory: 32 * config.GiB, Architectures: []config.Arch{config.ArchX64},
		WireMin: 12, WireMax: BarrierWireVersion, WireVersion: BarrierWireVersion}); err != nil {
		t.Fatalf("register %s: %v", node, err)
	}
}

// joinedPoolNode places order pa, admits a node from it with certificate serial 0a, and
// registers that node.
func joinedPoolNode(t *testing.T, a *Allocator) string {
	t.Helper()
	const id, serial = "pa", "0a"
	tok := placedOrder(t, a, id)
	if _, err := a.AdmitPoolEnrollment(t.Context(), admission(t, a, id, "SHA256:"+id, tok, serial)); err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
	node := PoolNodeName(id)
	registerPoolNode(t, a, node)

	return node
}

// decommissionOnProof takes a host the whole way `billet nodes decommission` asks for:
// two empty answers to the compute barrier spanning its grace, the node stopped, and the
// decommission, which must record itself as proved.
func decommissionOnProof(t *testing.T, a *Allocator, now *time.Time, barrier ComputeBarrier, node string) {
	t.Helper()
	observeNode(t, a, barrier, node, true)
	*now = now.Add(computeAbsenceGrace)
	observeNode(t, a, barrier, node, true)
	if err := a.NodeGone(t.Context(), node, fenceOf(t, a, node).Epoch); err != nil {
		t.Fatalf("stop %s: %v", node, err)
	}
	proven, err := a.Decommission(t.Context(), DecommissionRequest{Node: node, Actor: "ops"})
	if err != nil || !proven {
		t.Fatalf("decommission %s on the barrier's proof: proved %v, %v", node, proven, err)
	}
}

// waitsUnsealed asserts that sealing order pa is refused and writes nothing: the order
// stays placed and its node's certificate, serial 0a, stays unrevoked.
func waitsUnsealed(t *testing.T, a *Allocator, why string) {
	t.Helper()
	const id, serial = "pa", "0a"
	if _, err := a.SealPoolRelease(t.Context(), id); !errors.Is(err, ErrPoolReleaseWaits) {
		t.Fatalf("%s: sealing answered %v", why, err)
	}
	if o := readOrder(t, a, id); o.State != PoolPlaced {
		t.Fatalf("%s: a refused seal wrote %s", why, o.State)
	}
	if revoked, err := a.CertRevoked(t.Context(), serial); err != nil || revoked {
		t.Fatalf("%s: a refused seal revoked the certificate: %v, %v", why, revoked, err)
	}
}

// A JOINED HOST LEAVES PLACEMENT WHEN ITS RELEASE IS ASKED FOR, AND IS SEALED ONLY ON
// PROOF IT RUNS NOTHING: no lease, and a decommission on the compute barrier's proof. An
// inventory alone, however empty, is never that proof. The seal revokes its certificate
// first, and a registration after it does not put the host back into placement.
func TestAJoinedHostIsSealedOnlyOnProofItRunsNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	node := joinedPoolNode(t, a)

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
	registerPoolNode(t, a, node)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a registration put a host whose release was asked for back into placement")
	}
	waitsUnsealed(t, a, "a lease on the host")

	for _, l := range leases {
		if err := a.Release(ctx, l.ID, l.Epoch, PhaseDone); err != nil {
			t.Fatalf("release lease %s: %v", l.ID, err)
		}
	}
	now = now.Add(time.Hour)
	if _, err := a.Reconcile(ctx, node, nil); err != nil {
		t.Fatal(err)
	}
	waitsUnsealed(t, a, "an empty inventory and nothing else")

	decommissionOnProof(t, a, &now, requestBarrier(t, a), node)
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
	registerPoolNode(t, a, node)
	if placeableHosts(t, a) != 0 {
		t.Fatal("a registration put a released host back into placement")
	}
}

// contradictions are what contradicts a pool host's release after its decommission on
// proof: an inventory of its node's current registration reporting compute, and the node
// arriving to register, which the registration handler reports before it decides
// anything, so a refused registration contradicts it too.
var contradictions = map[string]func(t *testing.T, a *Allocator, node string){
	"an inventory reporting compute": func(t *testing.T, a *Allocator, node string) {
		t.Helper()
		if _, err := a.Reconcile(t.Context(), node, []string{"some-instance"}); err != nil {
			t.Fatal(err)
		}
	},
	"the node arriving to register": func(t *testing.T, a *Allocator, node string) {
		t.Helper()
		if err := a.InvalidateBarrierRun(t.Context(), node); err != nil {
			t.Fatal(err)
		}
	},
}

// decommissionProven reads whether the ledger records node's decommission as proved, the
// fact the drain's clearance and a CodeBuild fleet's claim read.
func decommissionProven(t *testing.T, a *Allocator, node string) bool {
	t.Helper()
	var proven int64
	if err := a.db.View(t.Context(), func(q querier) error {
		return q.QueryRowContext(t.Context(),
			`SELECT decommission_proven FROM nodes WHERE name = $1`, node).Scan(&proven)
	}); err != nil {
		t.Fatal(err)
	}

	return proven == 1
}

// A CONTRADICTED DECOMMISSION STAYS CONTRADICTED FOR THE RELEASE: neither a later empty
// report nor a control-plane start, which forgets every inventory, makes it proof again;
// only a fresh decommission on the barrier's proof does. The arrival is contradicted with
// no barrier run left to discard, as Decommission leaves it. The decommission itself is
// left as the person recorded it, for the readers that are not the release's.
func TestAContradictedDecommissionStaysContradicted(t *testing.T) {
	t.Parallel()
	for name, contradict := range contradictions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := joinedPoolNode(t, a)
			if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
				t.Fatal(err)
			}
			barrier := requestBarrier(t, a)
			decommissionOnProof(t, a, &now, barrier, node)

			contradict(t, a, node)
			waitsUnsealed(t, a, "a decommission contradicted since")
			if !decommissionProven(t, a, node) {
				t.Fatal("the release's contradiction rewrote the decommission other readers read")
			}
			if _, err := a.Reconcile(ctx, node, nil); err != nil {
				t.Fatal(err)
			}
			waitsUnsealed(t, a, "an empty inventory after the contradiction")
			if err := a.ForgetEveryNode(ctx); err != nil {
				t.Fatal(err)
			}
			waitsUnsealed(t, a, "a control-plane start after the contradiction")

			registerPoolNode(t, a, node)
			decommissionOnProof(t, a, &now, barrier, node)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("sealing on a fresh decommission: %v", err)
			}
		})
	}
}

// A SEALED RELEASE THE LEDGER NO LONGER PROVES IS NOT RELEASED: a contradiction landing
// after the seal, when the source's first release failed, refuses the release proof at
// every ask after, across a later empty report and a control-plane start, and a person
// closes the order.
func TestASealedReleaseTheLedgerNoLongerProvesIsNotReleased(t *testing.T) {
	t.Parallel()
	for name, contradict := range contradictions {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := joinedPoolNode(t, a)
			if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
				t.Fatal(err)
			}
			decommissionOnProof(t, a, &now, requestBarrier(t, a), node)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("seal: %v", err)
			}
			if _, err := a.PoolReleaseProof(ctx, "pa"); err != nil {
				t.Fatalf("the proof before any contradiction: %v", err)
			}

			contradict(t, a, node)
			refused := func(why string) {
				t.Helper()
				if _, err := a.PoolReleaseProof(ctx, "pa"); !errors.Is(err, ErrPoolReleaseContradicted) {
					t.Fatalf("%s: the release proof answered %v", why, err)
				}
			}
			refused("a contradiction after the seal")
			if _, err := a.Reconcile(ctx, node, nil); err != nil {
				t.Fatal(err)
			}
			refused("an empty inventory after the contradiction")
			if err := a.ForgetEveryNode(ctx); err != nil {
				t.Fatal(err)
			}
			refused("a control-plane start after the contradiction")

			if err := a.ClosePoolOrder(ctx, "pa", "took it back by hand"); err != nil {
				t.Fatalf("a person closing a contradicted seal: %v", err)
			}
		})
	}
}

// ANYTHING STILL HOLDING A HOST KEEPS ITS RELEASE UNSEALED, whatever else is proved: an
// escrow aimed at it, a quarantined lease, a lease in custody.
func TestAnythingHoldingAHostKeepsItsReleaseUnsealed(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]struct {
		phase  Phase
		onNode bool
	}{
		"an escrow aimed at it": {PhaseCapacity, false},
		"a quarantined lease":   {PhaseQuarantine, true},
		"a lease in custody":    {PhaseCustody, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			a := poolAllocator(t, &now)
			ctx := t.Context()
			node := joinedPoolNode(t, a)
			leases, err := a.Escrow(ctx, "intel", 1)
			if err != nil || len(leases) != 1 {
				t.Fatalf("a lease on the host: %v, %v", leases, err)
			}
			if _, err := a.RequestPoolRelease(ctx, "pa", "the pool shrinks"); err != nil {
				t.Fatal(err)
			}
			if err := a.Release(ctx, leases[0].ID, leases[0].Epoch, PhaseDone); err != nil {
				t.Fatal(err)
			}
			decommissionOnProof(t, a, &now, requestBarrier(t, a), node)

			// BY HAND, because nothing billet does puts a lease on a host decommissioned on
			// proof; this isolates the seal's own count from the decommission's.
			setLease := func(phase Phase, onNode bool) {
				t.Helper()
				held := sql.NullString{String: node, Valid: onNode}
				if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						`UPDATE leases SET phase = $1, node = $2, target_node = $3 WHERE id = $4`,
						string(phase), held, node, leases[0].ID)

					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			setLease(hold.phase, hold.onNode)
			waitsUnsealed(t, a, name)

			setLease(PhaseDone, hold.onNode)
			if _, err := a.SealPoolRelease(ctx, "pa"); err != nil {
				t.Fatalf("sealing once %s ended: %v", name, err)
			}
		})
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
	// AN ARRIVAL WHOSE REGISTRATION WAS REFUSED leaves no node row, so nothing was ever
	// placed on it, and it does not hold the release.
	if err := a.InvalidateBarrierRun(ctx, PoolNodeName("pa")); err != nil {
		t.Fatal(err)
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
