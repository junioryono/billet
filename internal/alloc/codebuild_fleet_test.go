package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// codeBuildShapes is one declared compute type, enough to back a registration.
func codeBuildShapes() []config.RemoteShape {
	return []config.RemoteShape{{
		Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB,
		PriceUSDPerHour: config.USDPerHour(10000),
	}}
}

func registerCodeBuildNode(t *testing.T, a *Allocator, name, fleet string) error {
	t.Helper()

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: name, Provider: config.ProviderCodeBuild,
		VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: codeBuildShapes(), CodeBuildFleet: fleet,
	})

	return err
}

// A RESERVED CODEBUILD FLEET IS ONE SHARED POOL, so two nodes drawing on it each
// register its whole capacity as their own contribution — and escrow then
// advertises the fleet twice, promising GitHub more concurrent jobs than AWS will
// run. Neither node's config is wrong on its own, and the two files are on two
// machines, so this is the only place the duplication is visible.
func TestASecondNodeCannotDrawOnAnotherLiveNodesFleet(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	const fleet = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs"

	if err := registerCodeBuildNode(t, a, "cb-1", fleet); err != nil {
		t.Fatalf("the first node was refused: %v", err)
	}

	err := registerCodeBuildNode(t, a, "cb-2", fleet)
	if !errors.Is(err, ErrFleetHeld) {
		t.Fatalf("the second node on one fleet got %v, want ErrFleetHeld", err)
	}

	// THE MESSAGE HAS TO NAME BOTH HOSTS AND THE WAY OUT, because failing closed
	// keeps a machine out of the fleet: an operator who cannot see which other node
	// holds it, or that on-demand compute is an option, has a node that will not
	// start and nothing to act on.
	for _, want := range []string{"cb-1", "cb-2", fleet, "node.codebuild.fleet_arn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// AND RE-REGISTERING THE SAME NODE IS NOT A DUPLICATE. A node reconnects
// constantly — every drain re-registers — so a check that counted its own row
// would let one host lock itself out of its own fleet on its second poll.
func TestANodeMayReRegisterOnItsOwnFleet(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	const fleet = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs"

	for i := range 3 {
		if err := registerCodeBuildNode(t, a, "cb-1", fleet); err != nil {
			t.Fatalf("re-registration %d was refused: %v", i, err)
		}
	}
}

// LOSING LIVENESS DOES NOT RELEASE A FLEET CLAIM, and the first version of this rule
// said it did.
//
// LIVENESS SAYS NOTHING ABOUT REMOTE COMPUTE. `ForgetEveryNode` marks every host
// not-live whenever a control plane STARTS, and a node that merely disconnected is
// not-live while its builds keep running — so a claim scoped to `live = 1` would let a
// differently named node take the fleet across an ordinary restart and advertise its
// whole capacity on top of work the old node still holds. That is the overcommit the
// claim exists to prevent, arriving through the mechanism meant to prevent it.
func TestLosingLivenessDoesNotReleaseAFleetClaim(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	const fleet = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs"

	if err := registerCodeBuildNode(t, a, "cb-old", fleet); err != nil {
		t.Fatalf("the first node was refused: %v", err)
	}

	if err := a.ForgetEveryNode(t.Context()); err != nil {
		t.Fatalf("ForgetEveryNode: %v", err)
	}

	err := registerCodeBuildNode(t, a, "cb-new", fleet)
	if !errors.Is(err, ErrFleetHeld) {
		t.Fatalf("a differently named node took the fleet after liveness was lost: %v", err)
	}

	// THE REMEDY IS NAMED, and it is a command that proves the fleet is idle rather
	// than one that merely forgets the host.
	if !strings.Contains(err.Error(), "billet nodes decommission cb-old") {
		t.Errorf("the refusal does not name the command that would release the claim: %v", err)
	}

	// AND THE ORDINARY REPLACEMENT STILL WORKS, which is what makes the refusal above
	// a boundary rather than a wall: an operator replacing the machine reuses the node
	// NAME, and that needs nothing.
	if err := registerCodeBuildNode(t, a, "cb-old", fleet); err != nil {
		t.Fatalf("the same node could not re-register on its own fleet after losing "+
			"liveness: %v", err)
	}
}

// ONLY A *PROVEN* DECOMMISSION RELEASES THE CLAIM, and the first version of this test
// asserted the opposite.
//
// A FORCED EXCLUSION IS NOT A PROOF. `--force` exists precisely for the host nothing
// could ask, and it records `decommission_proven = 0` so every later drain and `billet
// status` keep saying the exclusion is unproven. So reading only `decommissioned_at`
// released a reserved fleet on the strength of an operator saying "take it out of the
// set" — which says nothing about whether builds are still drawing on it, after which
// the replacement advertises the fleet's whole capacity on top of them. That is the
// same mistake this rule already made once with liveness, one step further on: deriving
// permission from what is broken rather than from what is proved.
func TestOnlyAProvenDecommissionReleasesAFleetClaim(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	const fleet = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs"

	if err := registerCodeBuildNode(t, a, "cb-old", fleet); err != nil {
		t.Fatalf("the first node was refused: %v", err)
	}

	// Refused first, so the assertions below mean something rather than passing
	// because nothing was ever checked.
	if err := registerCodeBuildNode(t, a, "cb-new", fleet); !errors.Is(err, ErrFleetHeld) {
		t.Fatalf("the replacement was admitted before the old node was excluded: %v", err)
	}

	proven, err := a.Decommission(t.Context(), DecommissionRequest{
		Node: "cb-old", Actor: "test", Force: true,
	})
	if err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	// THE PREMISE IS ASSERTED. If a forced exclusion ever came back proven, the case
	// below would be testing something else entirely.
	if proven {
		t.Fatal("a --force exclusion reported itself PROVEN, so this test no longer covers " +
			"the unproven case it is named for")
	}

	err = registerCodeBuildNode(t, a, "cb-new", fleet)
	if !errors.Is(err, ErrFleetHeld) {
		t.Fatalf("a forced exclusion released the fleet claim, so the replacement now "+
			"advertises its whole capacity on top of whatever the old host is still "+
			"running: %v", err)
	}

	// THE REMEDY NAMES THE COMMAND THAT ESTABLISHES A PROOF, not the one that merely
	// forgets the host.
	if !strings.Contains(err.Error(), "billet drain --wait") {
		t.Errorf("the refusal does not name the command that would prove the fleet idle: %v", err)
	}

	// AND A PROVEN EXCLUSION DOES RELEASE IT, or this would be a claim nothing can
	// ever hand over — a fleet permanently stuck to a host that is gone.
	//
	// THE PROOF IS SET DIRECTLY. How a proof is ESTABLISHED is the compute barrier's
	// property and internal/alloc/barrier_test.go owns it end to end; what this test
	// owns is what the fleet predicate does with the answer, so the column is written
	// rather than the whole barrier stack rehearsed.
	if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE nodes SET decommission_proven = 1 WHERE name = 'cb-old'`)

		return err
	}); err != nil {
		t.Fatalf("record the proof: %v", err)
	}

	if err := registerCodeBuildNode(t, a, "cb-new", fleet); err != nil {
		t.Fatalf("the replacement was refused after a PROVEN decommission: %v", err)
	}
}

// A NODE DOES NOT CHANGE FLEETS WHILE IT IS RUNNING WORK.
//
// Without this, a node re-registering from one fleet to another — or to on-demand —
// RELEASES the first fleet's claim while its existing builds still draw on it, after
// which another node may take that fleet and advertise its whole capacity on top of
// them. Site and provider are guarded for exactly this reason; the fleet is the same
// kind of fact and was missing the same guard.
func TestANodeCannotChangeFleetsWhileItHasOutstandingWork(t *testing.T) {
	t.Parallel()

	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderCodeBuild, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 7 * config.GiB, Image: "img",
	}

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB},
		[]config.Tier{tier})

	const (
		fleetA = "arn:aws:codebuild:us-west-2:000000000000:fleet/a"
		fleetB = "arn:aws:codebuild:us-west-2:000000000000:fleet/b"
	)

	if err := registerCodeBuildNode(t, a, "cb-1", fleetA); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// One outstanding lease aimed at that host, which is what the guard counts.
	if _, err := a.Reserve(t.Context(), "cloud"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// ONE LEDGER, SO ONE GOROUTINE. Both cases are registrations against the same
	// allocator, so they are a loop rather than subtests: parallel subtests would
	// share this ledger, and serial ones only add names to a two-line assertion.
	for _, tc := range []struct{ name, to string }{
		{"another fleet", fleetB},
		{"on-demand", ""},
	} {
		err := registerCodeBuildNode(t, a, "cb-1", tc.to)
		if !errors.Is(err, ErrFleetHeld) {
			t.Fatalf("the node moved to %s with work outstanding: %v", tc.name, err)
		}

		// THE WAY BACK IS SPELLED OUT, because this fires during startup — cmd
		// registers the node before it recovers anything — so an operator who edits
		// the fleet with work still bound finds billet refusing to start.
		if !strings.Contains(err.Error(), "node.codebuild.fleet_arn back to") {
			t.Errorf("the refusal to move to %s does not say how to start billet again: %v",
				tc.name, err)
		}
	}

	// AND THE SAME FLEET RE-REGISTERS FREELY, or every reconnect would be refused.
	if err := registerCodeBuildNode(t, a, "cb-1", fleetA); err != nil {
		t.Fatalf("re-registering on the same fleet was refused: %v", err)
	}
}

// ON-DEMAND COMPUTE CARRIES NO SHARED-POOL CLAIM, so an empty fleet must never
// collide with another empty one. Every codebuild node without a fleet stores the
// same empty string, so a check that compared without excluding it would admit
// exactly one on-demand node per deployment.
func TestOnDemandCodeBuildNodesDoNotCollide(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB}, nil)

	for _, name := range []string{"cb-a", "cb-b", "cb-c"} {
		if err := registerCodeBuildNode(t, a, name, ""); err != nil {
			t.Fatalf("on-demand node %s was refused: %v", name, err)
		}
	}
}

// A FLEET IS A CODEBUILD FACT AND NOTHING ELSE'S. Accepting one from another
// backend would record a shared-pool claim against a host that draws on no pool,
// after which the uniqueness refusal keeps a perfectly good second node out of a
// fleet its provider never reads.
func TestOnlyACodeBuildNodeMayReportAFleet(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 64, Memory: 256 * config.GiB,
		CodeBuildFleet: "arn:aws:codebuild:us-west-2:000000000000:fleet/macs",
	})
	if err == nil {
		t.Fatal("a firecracker node reporting a codebuild fleet was accepted")
	}

	if !strings.Contains(err.Error(), "only a codebuild node draws on one") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// A REMOTE BACKEND MUST DECLARE WHAT IT MAY BUY, and a host-backed one must not.
//
// BOTH DIRECTIONS, because each protects a different failure. A codebuild node
// whose catalogue was accepted empty registers fine and placement then cannot
// charge the shape it buys — so the lease is charged the smaller tier request,
// which is an overcommit on a machine nobody can inspect. A firecracker node whose
// catalogue was accepted would be recording a purchase decision about compute
// nobody buys.
func TestShapeCataloguesFollowWhereTheWorkRuns(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cb-1", Provider: config.ProviderCodeBuild,
		VCPU: 64, Memory: 256 * config.GiB,
	})
	if err == nil {
		t.Fatal("a codebuild node with no shape catalogue was accepted")
	}

	// THE DIAGNOSTIC NAMES THE OPERATOR'S OWN KEY. Before the validator took a
	// provider it said node.ec2.instance_types at a codebuild operator.
	if !strings.Contains(err.Error(), "node.codebuild.compute_types") {
		t.Errorf("the refusal names the wrong config key: %v", err)
	}

	_, err = a.RegisterNode(t.Context(), NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderFirecracker,
		VCPU: 64, Memory: 256 * config.GiB, EC2Shapes: codeBuildShapes(),
	})
	if err == nil {
		t.Fatal("a firecracker node reporting purchasable shapes was accepted")
	}

	if !strings.Contains(err.Error(), "runs work on the host itself") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// A PROVIDER BILLET DOES NOT IMPLEMENT IS NOT A PROVIDER. Every placement question
// is answered from this string, and RunsOnHost is an allowlist — so an unknown
// backend would be treated as REMOTE and have three separate decisions taken about
// it: that it declares a shape catalogue, that it is charged the shape it buys, and
// that it is exempt from the host-overcommit comparison.
func TestAnUnimplementedProviderIsRefusedAtRegistration(t *testing.T) {
	t.Parallel()

	a := newBareAllocator(t, Limits{MaxVCPU: 128, MaxMemory: 512 * config.GiB}, nil)

	_, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "mystery-1", Provider: config.ProviderKind("kubernetes"),
		VCPU: 64, Memory: 256 * config.GiB,
	})
	if err == nil {
		t.Fatal("a node running a provider billet does not implement was accepted")
	}

	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}
