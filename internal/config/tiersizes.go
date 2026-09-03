package config

import (
	"fmt"
	"slices"
)

// DefaultMemoryPerVCPU is the proportion a `sizes` ladder is shaped to when the
// tier says nothing.
//
// THE SAME NUMBER EVERY GENERATED CATALOGUE ALREADY USES, so a config that
// writes `sizes: [2, 4, 8]` and nothing else gets exactly the ladder
// `billet init` would have written for it. Changing it here would silently
// resize every deployment that took the default, which is why it is a named
// constant rather than a literal in the expansion.
const DefaultMemoryPerVCPU = 4 * GiB

// ExpandTierSizes turns every `sizes` entry into one tier per size.
//
// EXPORTED BECAUSE Parse IS NOT THE ONLY ENTRY POINT. `alloc.New` takes a
// catalogue directly and is exported too, so a caller that assembled tiers
// without going through Parse would otherwise hand the allocator an entry whose
// vcpu is zero and whose real sizes nothing ever read. alloc.New refuses such a
// tier by name and points here, which is the alloc.New rule — a safety-critical
// derivation enforced at only one of two entry points is a second entry point
// that does not enforce it.
//
// IT RUNS BEFORE DEFAULTS AND BEFORE VALIDATION, so nothing downstream ever sees
// an unexpanded tier: applyDefaults fills in per-tier values the expansion has to
// have produced first (a macOS tier's inherited concurrency cap among them), and
// validation judges the tiers that will actually exist.
//
// The result is a NEW slice; the caller's is not modified, because Parse is not
// the only caller and a function that rewrote its input would make the
// expansion's idempotence depend on who called it.
func ExpandTierSizes(tiers []Tier) ([]Tier, error) {
	// EXPANDED IN PLACE IN THE ORDER WRITTEN. A tier's position decides nothing
	// today, but the file an operator reads back should look like the file they
	// wrote with the ladder unrolled where they put it.
	out := make([]Tier, 0, len(tiers))

	// WHICH LABELS A LADDER MADE, so the collision message below can be about the
	// ladder only when a ladder is what caused it.
	fromLadder := make(map[string]bool)

	for i := range tiers {
		expanded, err := expandOne(tiers[i])
		if err != nil {
			return nil, err
		}

		if len(tiers[i].Sizes) > 0 {
			for j := range expanded {
				fromLadder[expanded[j].Label] = true
			}
		}

		out = append(out, expanded...)
	}

	if err := refuseDuplicateLabels(out, fromLadder); err != nil {
		return nil, err
	}

	return out, nil
}

// expandOne expands a single entry, or returns it unchanged.
func expandOne(t Tier) ([]Tier, error) {
	where := tierWhere(t)

	if len(t.Sizes) == 0 {
		// MEANINGLESS WITHOUT SIZES, AND REFUSED RATHER THAN IGNORED. A tier
		// carrying a memory-per-vCPU proportion and an explicit memory is somebody
		// expecting one of the two to win, and neither answer is billet's to pick.
		if t.MemoryPerVCPU != 0 {
			return nil, fmt.Errorf("%s: memory_per_vcpu shapes a `sizes` ladder and this tier "+
				"has none, so it would decide nothing; set `sizes` or remove it", where)
		}

		return []Tier{t}, nil
	}

	if t.VCPU != 0 || t.Memory != 0 {
		return nil, fmt.Errorf("%s: `sizes` writes vcpu and memory for every tier it "+
			"expands to, so setting either alongside it is two spellings of one value. "+
			"Remove vcpu and memory, or remove `sizes` and write the tiers out", where)
	}

	// A macOS TIER'S CONCURRENCY IS PER HOST, AND A LADDER CANNOT DIVIDE IT.
	//
	// A macOS tier with no explicit max_concurrent inherits its HOST's limit, so
	// three expanded tiers each inherit the whole allowance and validation refuses
	// the file for exceeding it — with a diagnostic about a licence rather than
	// about the shorthand that caused it. Writing the tiers out by hand is how an
	// operator decides which of them gets which share, and that is a decision, not
	// boilerplate.
	if t.GuestOS == GuestMacOS {
		return nil, fmt.Errorf("%s: `sizes` cannot expand a macOS tier. Each expansion "+
			"would inherit its host's whole macOS guest allowance and together they would "+
			"exceed it, so the shares are yours to divide: write the tiers out with an "+
			"explicit max_concurrent each", where)
	}

	perVCPU := t.MemoryPerVCPU
	if perVCPU == 0 {
		perVCPU = DefaultMemoryPerVCPU
	}

	if perVCPU < 0 {
		return nil, fmt.Errorf("%s: memory_per_vcpu must not be negative", where)
	}

	out := make([]Tier, 0, len(t.Sizes))
	seen := make(map[int]bool, len(t.Sizes))

	for _, size := range t.Sizes {
		if size <= 0 {
			return nil, fmt.Errorf("%s: `sizes` entry %d must be more than zero", where, size)
		}

		// A DUPLICATE SIZE IS A MISTAKE RATHER THAN A NO-OP: it produces two tiers
		// with the same label, which validation refuses one layer down with a
		// message about the label rather than about the list that made it.
		if seen[size] {
			return nil, fmt.Errorf("%s: `sizes` lists %d twice", where, size)
		}

		seen[size] = true

		sized := t
		sized.Sizes = nil
		sized.MemoryPerVCPU = 0
		sized.Label = fmt.Sprintf("%s-%dvcpu", t.Label, size)
		sized.VCPU = size
		sized.Memory = ByteSize(size) * perVCPU

		// CLONED, because a Tier holds slices and maps. Copying the struct shares
		// their backing storage, so a later edit to one expansion's workflows —
		// or a provider list normalized in place — would reach every other tier
		// the same entry produced.
		sized.Workflows = slices.Clone(t.Workflows)
		sized.Providers = slices.Clone(t.Providers)
		sized.Command = slices.Clone(t.Command)
		sized.Launch = cloneLaunch(t.Launch)

		out = append(out, sized)
	}

	return out, nil
}

// cloneLaunch copies a tier's per-provider launch map.
func cloneLaunch(in map[ProviderKind]TierLaunch) map[ProviderKind]TierLaunch {
	if in == nil {
		return nil
	}

	out := make(map[ProviderKind]TierLaunch, len(in))
	for k, v := range in {
		v.Command = slices.Clone(v.Command)
		out[k] = v
	}

	return out
}

// refuseDuplicateLabels catches a collision A LADDER produced, and only that.
//
// validateTiers refuses a duplicate label anyway, and this exists because its
// message would name the LABEL: an operator whose `sizes: [2, 4]` collided with a
// hand-written `web-2vcpu` needs to be told which of the two spellings made it,
// not that a label appears twice.
//
// WHICH IS EXACTLY WHY IT ONLY SPEAKS FOR LABELS A LADDER MADE. The first
// version reported every duplicate this way, so two hand-written tiers sharing a
// label — a config with no `sizes` anywhere in it — were told about a ladder
// that does not exist, and a test that had been passing since long before this
// caught it. A diagnostic that is true of one input and false of the next one
// over is worse than the general one it replaced; those go back to validateTiers,
// whose message is the right one for them.
func refuseDuplicateLabels(tiers []Tier, fromLadder map[string]bool) error {
	seen := make(map[string]bool, len(tiers))

	for i := range tiers {
		label := tiers[i].Label
		if seen[label] && fromLadder[label] {
			return fmt.Errorf("tier %q is defined more than once after `sizes` expansion; a "+
				"ladder writes <label>-<n>vcpu, so it collides with a tier already spelled "+
				"that way", label)
		}

		seen[label] = true
	}

	return nil
}

// tierWhere names a tier the way validation names one, so a diagnostic from the
// expansion reads like the diagnostics either side of it.
func tierWhere(t Tier) string {
	if t.Label == "" {
		return "a tier with no label"
	}

	return fmt.Sprintf("tier %q", t.Label)
}
