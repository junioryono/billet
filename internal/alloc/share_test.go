package alloc

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func targetTier(label, target string, vcpu int, memory config.ByteSize) config.Tier {
	t := tier(label, vcpu, memory)
	t.Target = target

	return t
}

// shareAllocator is one deployment serving two targets on one large host, with
// a 24 vCPU share on the repository target and none on the organization.
func shareAllocator(t *testing.T) *Allocator {
	t.Helper()

	return newAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{"repo": {VCPU: 24}},
	}, []config.Tier{
		targetTier("repo-8", "repo", 8, 16*config.GiB),
		targetTier("repo-16", "repo", 16, 32*config.GiB),
		targetTier("org-2", config.DefaultTargetName, 2, 4*config.GiB),
	})
}

// A SHARE IS SPENT BY ITS TARGET'S TIERS TOGETHER (#192): a burst of one
// target's jobs stops at its share however much room the host has, and the
// other target's tiers keep the rest of the deployment ceiling.
func TestATargetShareCapsItsTiersTogether(t *testing.T) {
	a := shareAllocator(t)

	if n := headroom(t, a, "repo-8"); n != 3 {
		t.Fatalf("repo-8 headroom = %d, want 3 under a 24 vCPU share", n)
	}

	reserve(t, a, "repo-16")

	if n := headroom(t, a, "repo-8"); n != 1 {
		t.Errorf("after one repo-16, repo-8 headroom = %d, want the 8 vCPU the share has left", n)
	}

	if n := headroom(t, a, "repo-16"); n != 0 {
		t.Errorf("after one repo-16, repo-16 headroom = %d, want 0", n)
	}

	reserve(t, a, "repo-8")

	if _, err := a.Reserve(t.Context(), "repo-8"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a reservation past the share returned %v, want ErrNoCapacity", err)
	}

	// The deployment has 40 vCPU left and the organization target has no share.
	if n := headroom(t, a, "org-2"); n != 20 {
		t.Errorf("org-2 headroom = %d, want the 20 the deployment ceiling leaves", n)
	}
}

// A TIER HELD BY ITS OWN SHARE SAYS SO, which is what lets the admission order
// keep it from holding another target's tiers back; the fleet still has room,
// so nothing else could be the reason.
func TestAdmissionViewReportsATierHeldByItsShare(t *testing.T) {
	a := shareAllocator(t)

	if got := admissionOf(t, a, "repo-8"); got.ShareBound || got.Target != "repo" {
		t.Fatalf("an empty share reported %+v, want target repo and not share-bound", got)
	}

	reserve(t, a, "repo-16")
	reserve(t, a, "repo-8")

	got := admissionOf(t, a, "repo-8")
	if !got.ShareBound {
		t.Errorf("a tier whose target spent its share was not reported share-bound: %+v", got)
	}
	if !got.CanGrow {
		t.Errorf("a share-bound tier was reported unable to grow, though its own target's work "+
			"ending frees the share: %+v", got)
	}

	if org := admissionOf(t, a, "org-2"); org.ShareBound || org.Target != config.DefaultTargetName {
		t.Errorf("the organization tier, which has no share, reported %+v", org)
	}
}

// A FALLBACK IS AUTHORISED AGAINST THE SHARE, as if the lease's current shape
// had been returned first: a larger shape the share cannot hold is refused
// while the deployment and the node still have room.
func TestAResizeCannotTakeMoreThanItsTargetsShare(t *testing.T) {
	cloud := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{config.DefaultTargetName: {VCPU: 16}},
	}, []config.Tier{cloud})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2,
		VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	leases, err := a.Escrow(t.Context(), "cloud", 3)
	if err != nil || len(leases) != 2 {
		t.Fatalf("Escrow = %d leases, %v; want the 2 eight-vCPU shapes a 16 vCPU share holds",
			len(leases), err)
	}

	lease := leases[0]
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	err = a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB)
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "share") {
		t.Fatalf("a fallback past the share returned %v, want ErrNoCapacity naming the share", err)
	}

	if err := a.Release(t.Context(), leases[1].ID, leases[1].Epoch, PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB); err != nil {
		t.Fatalf("a fallback the share holds once the other lease returned: %v", err)
	}
}

// alloc.New RE-APPLIES THE SHARE RULES, because it cannot prove its catalogue
// came through config.Parse.
func TestNewRefusesAShareItCouldNotHonour(t *testing.T) {
	for name, tc := range map[string]struct {
		share config.TargetShare
		tiers []config.Tier
	}{
		"a tier larger than its share": {
			share: config.TargetShare{VCPU: 8},
			tiers: []config.Tier{targetTier("big", "repo", 16, 8*config.GiB)},
		},
		"floors above the share": {
			share: config.TargetShare{VCPU: 16},
			tiers: []config.Tier{func() config.Tier {
				t := targetTier("floored", "repo", 8, 8*config.GiB)
				t.Reserved = 3

				return t
			}()},
		},
		"a share above the deployment ceiling": {
			share: config.TargetShare{VCPU: 128},
			tiers: []config.Tier{targetTier("small", "repo", 2, 4*config.GiB)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(openTestLedger(t), Limits{
				MaxVCPU: 64, MaxMemory: 256 * config.GiB,
				Shares: map[string]config.TargetShare{"repo": tc.share},
			}, tc.tiers)
			if err == nil || !strings.Contains(err.Error(), `target "repo"`) {
				t.Fatalf("New accepted %s: %v", name, err)
			}
		})
	}
}

// A SIBLING'S FLOOR IS KEPT INSIDE THE SHARE. Held only against the host and the
// deployment, a floor left the share free for a burst of the other tier, and the
// reserved tier then could not buy the instance it was promised.
func TestASiblingFloorIsKeptInsideTheShare(t *testing.T) {
	floored := targetTier("floored", "repo", 8, 16*config.GiB)
	floored.Reserved = 1

	a := newAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{"repo": {VCPU: 16}},
	}, []config.Tier{floored, targetTier("burst", "repo", 8, 16*config.GiB)})

	if n := headroom(t, a, "burst"); n != 1 {
		t.Fatalf("burst headroom = %d, want the 1 the share leaves beside the floor", n)
	}

	reserve(t, a, "burst")

	if _, err := a.Reserve(t.Context(), "burst"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a burst into the floor's share returned %v, want ErrNoCapacity", err)
	}

	if n := headroom(t, a, "floored"); n != 1 {
		t.Errorf("the floored tier's headroom = %d, want the 1 its floor kept", n)
	}
}

// A REMOTE FLOOR IS HELD AT THE SHAPE IT BUYS AND NO FURTHER THAN THE SHARE.
// Two floors of a 2 vCPU tier each charged 8 fit an 8 vCPU share once; holding
// the second anyway took the whole deployment from the other target.
func TestARemoteFloorIsHeldNoFurtherThanItsShare(t *testing.T) {
	cloud := config.Tier{
		Label: "cloud", Target: "repo", Provider: config.ProviderEC2,
		GuestOS: config.GuestLinux, VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
		Reserved: 2,
	}
	a := newBareAllocator(t, Limits{
		MaxVCPU: 16, MaxMemory: 64 * config.GiB,
		Shares: map[string]config.TargetShare{"repo": {VCPU: 8}},
	}, []config.Tier{cloud, targetTier("org-2", config.DefaultTargetName, 2, 4*config.GiB)})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{Type: "eight", VCPU: 8, Memory: 16 * config.GiB}},
	}); err != nil {
		t.Fatalf("RegisterNode cloud: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), testRegistration("fc-1", config.ProviderFirecracker)); err != nil {
		t.Fatalf("RegisterNode fc: %v", err)
	}

	if n := headroom(t, a, "org-2"); n != 4 {
		t.Errorf("org-2 headroom = %d, want the 4 left beside one 8 vCPU floor", n)
	}
}

// A TIER THAT LEFT THE CATALOGUE STILL HOLDS EVERY SHARE. Which target its
// open leases belonged to cannot be told, so forgetting them would sell their
// room a second time.
func TestALeaseOfARemovedTierStillCountsAgainstTheShare(t *testing.T) {
	limits := Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{"repo": {VCPU: 24}},
	}

	before := newAllocator(t, limits, []config.Tier{
		targetTier("repo-old", "repo", 8, 16*config.GiB),
		targetTier("repo-8", "repo", 8, 16*config.GiB),
	})
	reserve(t, before, "repo-old")
	reserve(t, before, "repo-old")

	after, err := New(before.db, limits, []config.Tier{targetTier("repo-8", "repo", 8, 16*config.GiB)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if n := headroom(t, after, "repo-8"); n != 1 {
		t.Errorf("repo-8 headroom = %d, want the 1 left beside the removed tier's 16 vCPU", n)
	}
}

// SHARE-BOUND IS MEASURED AT THE CHARGED SHAPE. A 2 vCPU tier charged 8 with 4
// left in its share cannot buy, though its request would fit.
func TestShareBoundIsMeasuredAtTheChargedShape(t *testing.T) {
	cloud := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{config.DefaultTargetName: {VCPU: 12}},
	}, []config.Tier{cloud})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{Type: "eight", VCPU: 8, Memory: 16 * config.GiB}},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	if admissionOf(t, a, "cloud").ShareBound {
		t.Fatal("an empty share reported the tier share-bound")
	}

	reserve(t, a, "cloud")

	if got := admissionOf(t, a, "cloud"); !got.ShareBound || !got.TargetShared {
		t.Errorf("4 vCPU left of the share against an 8 vCPU charge reported %+v, want share-bound", got)
	}
}

// SHARE-BOUND TESTS EACH SHAPE WHOLE. The least vCPU of one host's shape and the
// least memory of another's describe a shape nobody sells, and a tier judged by
// it would hold other targets back for room it can never use.
func TestShareBoundTestsEachChargedShapeWhole(t *testing.T) {
	cloud := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	a := newBareAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{
			config.DefaultTargetName: {VCPU: 4, Memory: 16 * config.GiB},
		},
	}, []config.Tier{cloud})

	for name, shape := range map[string]config.EC2InstanceType{
		"cloud-wide": {Type: "wide", VCPU: 4, Memory: 32 * config.GiB},
		"cloud-tall": {Type: "tall", VCPU: 8, Memory: 16 * config.GiB},
	} {
		if _, err := a.RegisterNode(t.Context(), NodeRegistration{
			Name: name, Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
			EC2Shapes: []config.EC2InstanceType{shape},
		}); err != nil {
			t.Fatalf("RegisterNode %s: %v", name, err)
		}
	}

	if got := admissionOf(t, a, "cloud"); !got.ShareBound {
		t.Errorf("no charged shape fits a 4 vCPU, 16GiB share, yet the tier was not share-bound: %+v", got)
	}
}

// A RESIZE CREDITS THE RETURNED SHAPE BEFORE THE FLOORS ARE HELD. Credited only
// afterwards, a sibling's floor measured against the share without it held
// nothing, and the fallback then took the room that floor was owed.
func TestAResizeKeepsASiblingFloorItsShare(t *testing.T) {
	cloud := config.Tier{
		Label: "cloud", Target: "repo", Provider: config.ProviderEC2,
		GuestOS: config.GuestLinux, VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test",
	}
	floored := targetTier("floored", "repo", 12, 16*config.GiB)
	floored.Reserved = 1

	a := newBareAllocator(t, Limits{
		MaxVCPU: 64, MaxMemory: 256 * config.GiB,
		Shares: map[string]config.TargetShare{"repo": {VCPU: 16}},
	}, []config.Tier{cloud, floored})

	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{
			{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			{Type: "sixteen", VCPU: 16, Memory: 32 * config.GiB},
		},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// The floored tier has no host yet, so its floor holds nothing and the
	// cloud tier buys inside the whole share.
	lease := reserve(t, a, "cloud")
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// A host arrives that can keep the floor: 12 of the share are now owed to it.
	if _, err := a.RegisterNode(t.Context(), testRegistration("fc-1", config.ProviderFirecracker)); err != nil {
		t.Fatalf("RegisterNode fc: %v", err)
	}

	err := a.Resize(t.Context(), lease.ID, lease.Epoch, "sixteen", 16, 32*config.GiB)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("a fallback into the sibling floor's share returned %v, want ErrNoCapacity", err)
	}
}
