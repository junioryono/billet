package ceph

import (
	"testing"
)

func gens(names ...string) []Generation {
	out := make([]Generation, 0, len(names))

	for _, name := range names {
		if gen, ok := ParseGeneration(name); ok {
			out = append(out, gen)
		}
	}

	return out
}

func kept(plan []Reapable) map[string]string {
	out := map[string]string{}

	for _, item := range plan {
		if item.Reason != "" {
			out[item.Generation.Name] = item.Reason
		}
	}

	return out
}

func doomed(plan []Reapable) []string {
	var out []string

	for _, item := range plan {
		if item.Reason == "" {
			out = append(out, item.Generation.Name)
		}
	}

	return out
}

// WHAT @verified RESOLVES TO IS NEVER REMOVED.
//
// This is the one that turns a disk-space chore into an outage: reaping the newest
// verified generation strands every tier that names `@verified`, and they name it
// precisely so that nobody has to think about generations. The fleet would stop
// being able to launch at all.
func TestTheGenerationVerifiedMostRecentlySurvives(t *testing.T) {
	t.Parallel()

	all := gens("g20260101000000", "g20260601000000", "g20260814145813")
	verified := map[string]bool{"g20260814145813": true}

	plan := PlanReap(all, verified, Retention{Keep: 1})

	if _, ok := kept(plan)["g20260814145813"]; !ok {
		t.Fatal("the generation @verified resolves to was reaped, which strands every tier " +
			"that names it")
	}
}

// AND A TIER'S PINNED GENERATION SURVIVES WHETHER OR NOT IT WAS EVER VERIFIED.
//
// Somebody chose it. Whether billet has a verification record for it is not the
// question — a config saying `@g2026…` expects that image to be there, and the
// choice outranks a retention policy that never heard about it.
func TestAPinnedGenerationSurvivesEvenUnverified(t *testing.T) {
	t.Parallel()

	all := gens("g20260101000000", "g20260601000000", "g20260814145813")
	verified := map[string]bool{"g20260814145813": true}

	plan := PlanReap(all, verified, Retention{
		Keep:   1,
		Pinned: []string{"ubuntu-2404-x64@g20260101000000"},
	})

	if _, ok := kept(plan)["g20260101000000"]; !ok {
		t.Error("a generation a tier pins was reaped, so that tier can no longer launch")
	}
}

// ROLLBACK DEPTH IS A COUNT OF VERIFIED GENERATIONS, not of all of them.
//
// The reason to keep more than the current one is that the newest may turn out bad
// AFTER it has been promoted, and unpromote has to have somewhere to land. An
// unverified generation is not somewhere to land — it was never proved to boot — so
// keeping three of those would leave nothing to roll back to while looking like it
// had.
func TestRollbackDepthCountsOnlyVerifiedGenerations(t *testing.T) {
	t.Parallel()

	all := gens(
		"g20260814145813", // verified, newest
		"g20260813000000", // not verified
		"g20260812000000", // verified
		"g20260811000000", // not verified
		"g20260810000000", // verified
		"g20260809000000", // verified, oldest verified
	)
	verified := map[string]bool{
		"g20260814145813": true,
		"g20260812000000": true,
		"g20260810000000": true,
		"g20260809000000": true,
	}

	plan := PlanReap(all, verified, Retention{Keep: 3})

	survived := kept(plan)
	for _, want := range []string{"g20260814145813", "g20260812000000", "g20260810000000"} {
		if _, ok := survived[want]; !ok {
			t.Errorf("%s should have been kept as one of the three newest verified", want)
		}
	}

	if _, ok := survived["g20260809000000"]; ok {
		t.Error("a fourth verified generation was kept against Keep: 3")
	}

	// AND THE UNVERIFIED ONES GO, which is the bulk of what accumulates: every week
	// publishes one, and only the ones that passed are ever worth keeping.
	for _, want := range []string{"g20260813000000", "g20260811000000"} {
		if _, ok := survived[want]; ok {
			t.Errorf("%s was never verified and was kept anyway", want)
		}
	}
}

// THE PLAN IS ORDERED NEWEST FIRST AND KEEPS BY BUILD TIME, not by the order the
// cluster happened to list them.
func TestRetentionIsByBuildTimeNotListOrder(t *testing.T) {
	t.Parallel()

	// Deliberately shuffled: the newest is in the middle.
	all := gens("g20260101000000", "g20260814145813", "g20260601000000")
	verified := map[string]bool{
		"g20260101000000": true,
		"g20260814145813": true,
		"g20260601000000": true,
	}

	plan := PlanReap(all, verified, Retention{Keep: 1})

	if _, ok := kept(plan)["g20260814145813"]; !ok {
		t.Error("the newest verified generation was not the one kept")
	}

	if got := doomed(plan); len(got) != 2 {
		t.Errorf("kept %d generations against Keep: 1", len(all)-len(got))
	}
}

// NOTHING IS REAPED WHEN NOTHING HAS BEEN VERIFIED.
//
// A fleet whose recent generations all failed verification is already in trouble;
// removing the only images it has would turn that into having nothing to boot at
// all. Keep is a count of verified generations, so zero verified means zero kept by
// that rule — and this asserts the pinned ones still hold the line.
func TestAnUnverifiedImageStillKeepsWhatIsPinned(t *testing.T) {
	t.Parallel()

	all := gens("g20260101000000", "g20260814145813")

	plan := PlanReap(all, map[string]bool{}, Retention{
		Keep:   3,
		Pinned: []string{"ubuntu-2404-x64@g20260814145813"},
	})

	if _, ok := kept(plan)["g20260814145813"]; !ok {
		t.Error("the pinned generation was reaped from an image with no verifications")
	}
}

// AND `@verified` IS NOT A GENERATION TO PIN.
//
// A tier naming `@verified` pins nothing — that is the point of it — so treating the
// word as a generation name would add a phantom entry that protects nothing and
// reads as though it did.
func TestTheVerifiedAliasIsNotTreatedAsAPin(t *testing.T) {
	t.Parallel()

	all := gens("g20260814145813")

	plan := PlanReap(all, map[string]bool{}, Retention{
		Keep:   0,
		Pinned: []string{"ubuntu-2404-x64@" + Verified},
	})

	if got := doomed(plan); len(got) != 1 {
		t.Errorf("`@%s` was treated as a pinned generation: %v", Verified, kept(plan))
	}
}
