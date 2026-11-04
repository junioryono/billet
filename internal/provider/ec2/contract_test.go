package ec2

import (
	"strconv"
	"strings"
	"testing"
)

// THE STAMP SAYS WHAT THE IMAGE ACTUALLY CARRIES, per architecture.
//
// The tag is what `billet check` reads INSTEAD of looking at the image, so a
// stamp the build does not honour is worse than no stamp: it is a claim nothing
// verifies. An architecture stamps the contract only when GitHub publishes a
// template for it and the build runs that template's plan; one without is held
// to the floor.
//
// BOTH DIRECTIONS, because a contractFor returning 1 for everything would also
// "not lie" and would silently stop any image from claiming the template.
func TestTheStampedContractMatchesWhatTheArchitectureCarries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		arch  string
		want  int
		scope string
		plan  string
	}{
		{arch: "x64", want: AMIContract, scope: "ami-x64", plan: "runner-images/plan.json"},
		{arch: "arm64", want: AMIContract, scope: "ami-arm64", plan: "runner-images/plan-arm64.json"},
		{arch: "riscv64", want: 1},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			t.Parallel()

			if got := contractFor(tc.arch); got != tc.want {
				t.Errorf("a %s image is stamped contract %d, want %d", tc.arch, got, tc.want)
			}

			scope, plan, err := scopeFor(tc.arch)
			if tc.scope == "" {
				if err == nil {
					t.Errorf("%s has no template and scopeFor named %s", tc.arch, scope)
				}

				return
			}

			if err != nil || scope != tc.scope || plan != tc.plan {
				t.Errorf("a %s build runs scope %q and plan %q (%v), want %q and %q",
					tc.arch, scope, plan, err, tc.scope, tc.plan)
			}

			// AND THE BUILD IS TOLD THAT SCOPE, which is what chooses the plan and the
			// declaration on the builder: an x64 template on an arm64 image is the
			// failure a matching stamp would hide.
			env, err := buildEnvFile(BuildSpec{Arch: tc.arch, nonce: verifyProbeNonce})
			if err != nil {
				t.Fatalf("buildEnvFile: %v", err)
			}

			if want := "BILLET_AMI_SCOPE=" + tc.scope + "\n"; !strings.Contains(env, want) {
				t.Errorf("a %s build's settings do not say %q:\n%s", tc.arch, want, env)
			}
		})
	}

	// AND THE CONSTANT MOVED, so an image built before this carries a lower number
	// and `billet check` can tell.
	if AMIContract < 4 {
		t.Errorf("AMIContract is %d; an image built from GitHub's template must be "+
			"distinguishable from one built by billet's own installers", AMIContract)
	}
}

// THE PROMOTION WRITES THE CONTRACT IT WAS ASKED FOR, and the components beside it
// from contract 4, and this reads the request rather than the constant.
func TestTheImageIsTaggedWithTheContractItWasProvedAt(t *testing.T) {
	t.Parallel()

	for _, contract := range []int{3, AMIContract} {
		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

		f := newFakeEC2(t)
		f.respond = b.serve(f)

		p := newTestProvider(t, f, nil)

		if err := p.promoteContract(t.Context(), "ami-new", contract, "firefox:140.0.4"); err != nil {
			t.Fatalf("promoteContract at contract %d: %v", contract, err)
		}

		got := f.paramsFor(t, "CreateTags")

		if got.Get("Tag.1.Key") != amiContractTag {
			t.Fatalf("the contract is not tag 1; the assertion below reads the wrong tag")
		}

		if v := got.Get("Tag.1.Value"); v != strconv.Itoa(contract) {
			t.Errorf("a promotion at contract %d stamped %q", contract, v)
		}

		key, value := got.Get("Tag.2.Key"), got.Get("Tag.2.Value")

		switch {
		case contract >= 4 && (key != amiComponentsTag || value != "firefox:140.0.4"):
			t.Errorf("a contract %d promotion tagged %s=%q, want %s=firefox:140.0.4",
				contract, key, value, amiComponentsTag)
		case contract < 4 && key != "":
			t.Errorf("a contract %d promotion tagged %s, which no image of it records", contract, key)
		}
	}
}

// A PROMOTION WHOSE COMPONENTS TAG DID NOT LAND IS NOT CONFIRMED: an image that
// carries the contract without the record of what it was completed with is the
// claim the tag exists to make, half made.
func TestAPromotionIsConfirmedOnlyWithItsComponents(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.serve(f)

	p := newTestProvider(t, f, nil)

	b.promoted = strconv.Itoa(AMIContract)
	other := "google-chrome:1.0"
	b.promotedComponents = &other

	if err := p.confirmContract(t.Context(), "ami-new", AMIContract, "firefox:140.0.4"); err == nil {
		t.Error("a promotion was confirmed against an image tagged with other components")
	}

	want := "firefox:140.0.4"
	b.promotedComponents = &want

	if err := p.confirmContract(t.Context(), "ami-new", AMIContract, want); err != nil {
		t.Errorf("a promotion carrying both tags was not confirmed: %v", err)
	}
}
