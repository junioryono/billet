package ceph

import (
	"slices"
	"strings"
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

	plan := PlanReap(all, verified, map[string]string{}, Retention{Keep: 1})

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

	plan := PlanReap(all, verified, map[string]string{}, Retention{
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

	plan := PlanReap(all, verified, map[string]string{}, Retention{Keep: 3})

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

	plan := PlanReap(all, verified, map[string]string{}, Retention{Keep: 1})

	if _, ok := kept(plan)["g20260814145813"]; !ok {
		t.Error("the newest verified generation was not the one kept")
	}

	if got := doomed(plan); len(got) != 2 {
		t.Errorf("kept %d generations against Keep: 1", len(all)-len(got))
	}
}

// ROLLING FLEETS NEED ONE ROLLBACK CHAIN PER GUEST CONTRACT.
//
// Publishing a generation for a new binary must not make the last generation an
// older binary can boot eligible for collection. Both binaries may be running
// during an ordinary host-by-host upgrade.
func TestRetentionKeepsVerifiedGenerationsPerGuestContract(t *testing.T) {
	t.Parallel()

	all := gens(
		"g20260814145813", // contract 7, newest overall
		"g20260813000000", // contract 6, newest that old nodes can boot
		"g20260812000000", // contract 7 rollback
		"g20260811000000", // contract 6 rollback
	)
	verified := map[string]bool{
		"g20260814145813": true,
		"g20260813000000": true,
		"g20260812000000": true,
		"g20260811000000": true,
	}
	contracts := map[string]string{
		"g20260814145813": "7",
		"g20260813000000": "6",
		"g20260812000000": "7",
		"g20260811000000": "6",
	}

	plan := PlanReap(all, verified, contracts, Retention{Keep: 1})
	survived := kept(plan)

	for _, want := range []string{"g20260814145813", "g20260813000000"} {
		if _, ok := survived[want]; !ok {
			t.Errorf("%s is the newest verified generation for its guest contract and was reaped", want)
		}
	}
	for _, want := range []string{"g20260812000000", "g20260811000000"} {
		if _, ok := survived[want]; ok {
			t.Errorf("%s exceeded the per-contract Keep: 1 retention", want)
		}
	}
}

func TestReapableRemembersWhetherVerificationExistedAtPlanTime(t *testing.T) {
	t.Parallel()

	gen := gens("g20260814145813")[0]
	plan := PlanReap([]Generation{gen}, map[string]bool{gen.Name: true},
		map[string]string{gen.Name: "6"}, Retention{Keep: 0})

	if len(plan) != 1 || !plan[0].Verified || plan[0].Reason != "" {
		t.Fatalf("plan = %#v, want a deliberately expired verified generation", plan)
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

	plan := PlanReap(all, map[string]bool{}, map[string]string{}, Retention{
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

	plan := PlanReap(all, map[string]bool{}, map[string]string{}, Retention{
		Keep:   0,
		Pinned: []string{"ubuntu-2404-x64@" + Verified},
	})

	if got := doomed(plan); len(got) != 1 {
		t.Errorf("`@%s` was treated as a pinned generation: %v", Verified, kept(plan))
	}
}

// EVERY KEY GOES WITH THE GENERATION, not just the two that existed when this loop
// was written.
//
// KernelKey was added later and the cleanup was not updated, so a reaped
// generation left its kernel pairing behind -- and the kernel reaper reads exactly
// those keys to decide what is still needed. A dead generation's key would keep
// its kernel alive forever, which is the opposite of what reaping is for.
func TestReapRemovesEveryMetadataKeyOfTheGenerationsItRemoves(t *testing.T) {
	f := &importFake{snapshots: []string{"g20260814072427"}}

	plan := []Reapable{{Generation: Generation{Name: "g20260814072427"}}}

	if _, err := importClient(t, f).Reap(t.Context(), "ubuntu-2404-x64", plan, Retention{}); err != nil {
		t.Fatalf("reap: %v", err)
	}

	for _, key := range []string{VerifiedKey, RunnerVersionKey, KernelKey, GuestContractKey} {
		want := key + ".g20260814072427"

		if !f.ranWith("image-meta", "remove", want) {
			t.Errorf("the reap left %s behind; the kernel reaper reads these keys to decide "+
				"what is still needed, so a dead generation's key keeps its kernel alive "+
				"forever", want)
		}
	}
}

func TestReapRecomputesEveryContractBucketUnderThePublishLock(t *testing.T) {
	all := gens("g20260815000000", "g20260814000000", "g20260813000000")
	verified := map[string]bool{
		"g20260815000000": true,
		"g20260814000000": true,
		"g20260813000000": true,
	}
	contracts := map[string]string{
		"g20260815000000": "7",
		"g20260814000000": "6",
		"g20260813000000": "7",
	}
	retention := Retention{Keep: 1}
	plan := PlanReap(all, verified, contracts, retention)

	// The newest generation moves from contract 7 to 6 after the preview. The
	// oldest generation's own metadata did not change, but it is now the only
	// verified contract-7 rollback and must survive the locked replan.
	f := &importFake{
		snapshots: []string{"g20260815000000", "g20260814000000", "g20260813000000"},
		metadata: strings.Join([]string{
			VerifiedKey + ".g20260815000000  now",
			VerifiedKey + ".g20260814000000  now",
			VerifiedKey + ".g20260813000000  now",
			GuestContractKey + ".g20260815000000  6",
			GuestContractKey + ".g20260814000000  6",
			GuestContractKey + ".g20260813000000  7",
		}, "\n"),
	}

	removed, err := importClient(t, f).Reap(t.Context(), "ubuntu-2404-x64", plan, retention)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if slices.Contains(removed, "g20260813000000") {
		t.Fatalf("removed the last contract-7 generation after another generation changed buckets: %v", removed)
	}
}
