package config

import (
	"strings"
	"testing"
	"time"
)

// GETTING INTO A JOB'S VM IS OFF UNLESS A TIER SAYS, AND NEVER ON AN UNTRUSTED TIER. Both directions:
// what may be held or attached is accepted, and each thing that would let a person into the wrong
// machine or quietly do nothing is refused, naming its key.
func TestADebugBlockIsHeldToWhatItCanMean(t *testing.T) {
	t.Parallel()

	trusted := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: WorkloadTrusted}

	with := func(base Tier, d *TierDebug) Tier {
		base.Debug = d

		return base
	}

	for name, tc := range map[string]struct {
		tier Tier
		ok   bool
		says string
	}{
		"nothing":       {trusted, true, ""},
		"an empty word": {with(trusted, &TierDebug{}), true, ""},
		"attach":        {with(trusted, &TierDebug{Attach: true}), true, ""},
		"a hold":        {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "5m"}), true, ""},
		"the most":      {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "1h"}), true, ""},

		"an untrusted tier": {with(Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux,
			Trust: WorkloadUntrusted}, &TierDebug{Attach: true}), false, "trusted"},
		"docker": {with(Tier{Provider: ProviderDocker, GuestOS: GuestLinux, Trust: WorkloadTrusted},
			&TierDebug{Attach: true}), false, "debug.attach"},
		"a macOS guest": {with(Tier{Provider: ProviderTart, GuestOS: GuestMacOS, Trust: WorkloadTrusted},
			&TierDebug{Attach: true}), false, "debug.attach"},
		"two backends": {with(Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2}, GuestOS: GuestLinux,
			Trust: WorkloadTrusted}, &TierDebug{Attach: true}), false, "debug.attach"},
		"a hold nobody can reach": {with(trusted, &TierDebug{HoldOnFailure: "5m"}), false, "hold_on_failure"},
		"past the bound":          {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "61m"}), false, "hold_on_failure"},
		"not a duration":          {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "soon"}), false, "hold_on_failure"},
		"negative":                {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "-5m"}), false, "hold_on_failure"},
		"zero":                    {with(trusted, &TierDebug{Attach: true, HoldOnFailure: "0s"}), false, "hold_on_failure"},
	} {
		errs := tc.tier.validateDebug("tiers[0]")
		if (len(errs) == 0) != tc.ok {
			t.Errorf("%s: %v, want accepted %t", name, errs, tc.ok)
		}

		if len(errs) > 0 && !strings.Contains(errs[0].Error(), tc.says) {
			t.Errorf("%s: the refusal does not say %q: %v", name, tc.says, errs[0])
		}
	}
}

// WHAT A READER GETS FOLLOWS THE WORD: no block is neither, and a hold is what it says.
func TestATiersDebugReads(t *testing.T) {
	t.Parallel()

	var none Tier
	if none.Attachable() || none.HoldDuration() != 0 {
		t.Errorf("a tier with no block: %t, %s", none.Attachable(), none.HoldDuration())
	}

	tier := Tier{Debug: &TierDebug{Attach: true, HoldOnFailure: "5m"}}
	if !tier.Attachable() || tier.HoldDuration() != 5*time.Minute {
		t.Errorf("a tier with a block: %t, %s", tier.Attachable(), tier.HoldDuration())
	}
}
