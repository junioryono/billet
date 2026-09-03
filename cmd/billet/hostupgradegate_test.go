package main

import (
	"strings"
	"testing"
)

// A RELEASE THAT MOVED UNDERNEATH THE DECISION IS REFUSED, NOT INSTALLED.
//
// The whole point of a rollout resolving a channel ONCE, to one immutable
// manifest, is that every host installs the same bytes. A node that re-resolved
// the channel for itself would defeat that the moment the channel advanced
// mid-rollout, or whenever somebody moved a tag — so the coordinator sends the
// digest it decided on and this run's own resolution has to agree with it.
func TestAnUpgradeRefusesAManifestTheDecisionDidNotName(t *testing.T) {
	t.Parallel()

	resolved := strings.Repeat("b", 64)

	err := checkResolvedDigest(hostUpgradeTarget{
		channel: "stable",
		digest:  strings.Repeat("a", 64),
	}, resolved)
	if err == nil {
		t.Fatal("a run installed a manifest the decision that asked for it did not name")
	}

	// AND IT NAMES BOTH DIGESTS AND SAYS NOTHING HAPPENED, because the operator
	// reading this is deciding whether their fleet is mid-move or their channel is.
	for _, want := range []string{
		strings.Repeat("a", 64), resolved, "stable", "nothing was installed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

// AN OPERATOR RUNNING IT BY HAND PINS NOTHING, and that is not a mismatch.
//
// Requiring the fence on every path would make a fleet-level mechanism a
// precondition for the one path that has no fleet behind it.
func TestAnUnpinnedUpgradeIsNotRefused(t *testing.T) {
	t.Parallel()

	if err := checkResolvedDigest(hostUpgradeTarget{channel: "stable"},
		strings.Repeat("c", 64)); err != nil {
		t.Errorf("an operator's own run was refused for carrying no digest: %v", err)
	}

	// AND CASE IS NOT A DISAGREEMENT. A hex digest is the same digest however it
	// was rendered, and refusing over that would be a fleet stopped by a
	// formatting difference.
	if err := checkResolvedDigest(hostUpgradeTarget{
		channel: "stable", digest: strings.Repeat("A", 64),
	}, strings.Repeat("a", 64)); err != nil {
		t.Errorf("the same digest in a different case was refused: %v", err)
	}
}
