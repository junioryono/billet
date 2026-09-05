package main

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/rollout"
)

// ledgerWithForwardRollout is ledgerWithRollout with the ordinary policy: the
// rollout did NOT allow a downgrade, which is what every automatic rollout says.
func ledgerWithForwardRollout(t *testing.T, target string) *config.Config {
	t.Helper()

	original := upgradeRoot
	upgradeRoot = t.TempDir()
	t.Cleanup(func() { upgradeRoot = original })

	cfg := &config.Config{Server: &config.ServerConfig{IdentityDir: t.TempDir()}}

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("openStateAdmin: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := rollout.New(db).Start(t.Context(), rollout.StartRequest{
		Channel: "stable", TargetVersion: target,
		TargetDigest: "1111111111111111111111111111111111111111111111111111111111111111",
		PriorVersion: "v0.8.0", CreatedBy: "channel", Nodes: []string{"epyc-1"},
		Policy: rollout.Policy{Cohort: 1, FailureBudget: 1},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return cfg
}

// runningAs stands a release in for the "(devel)" a test binary reports. NOT
// parallel, and nothing in this file is: the seam is a package variable, and the
// parallel tests of this package read it once the sequential ones are done.
func runningAs(t *testing.T, release string) {
	t.Helper()

	previous := runningRelease
	runningRelease = func() string { return release }

	t.Cleanup(func() { runningRelease = previous })
}

// A ROLLOUT WHOSE TARGET IS OLDER THAN THE RELEASE RUNNING HERE IS NOT FOLLOWED,
// IN THE SHAPE PRODUCTION HAD: the running release in the bare form a release
// binary reported through v0.9.1, the rollout's target as a tag.
//
// The guard existed and read this pair as could-not-tell, so a control plane
// moved to v0.9.1 by hand obeyed the morning's rollout to v0.9.0 and downgraded
// itself (2026-09-05). The pair is the test; the sentence is the assertion.
func TestAStaleRolloutBehindTheRunningReleaseIsNotFollowed(t *testing.T) {
	for _, running := range []string{"0.9.1", "v0.9.1"} {
		runningAs(t, running)

		cfg := ledgerWithForwardRollout(t, "v0.9.0")

		target, act, err := rolloutInstruction(t.Context(), cfg)
		if err != nil {
			t.Fatalf("rolloutInstruction with %s running: %v", running, err)
		}

		if act {
			t.Fatalf("with %s running, the rollout to v0.9.0 was followed: %+v", running, target)
		}
	}
}

// AND A ROLLOUT TO THE RELEASE RUNNING HERE, IN THE OTHER SPELLING, IS ONE THIS
// HOST IS ALREADY ON: it settles rather than moving.
func TestARolloutToTheRunningReleaseInTheOtherSpellingSettles(t *testing.T) {
	runningAs(t, "0.9.1")

	cfg := ledgerWithForwardRollout(t, "v0.9.1")

	if _, act, err := rolloutInstruction(t.Context(), cfg); err != nil || act {
		t.Fatalf("a host on 0.9.1 was asked to move to v0.9.1 (act=%v, err=%v)", act, err)
	}

	settled, err := readSettled()
	if err != nil {
		t.Fatalf("readSettled: %v", err)
	}

	if settled != 1 {
		t.Fatalf("the host did not settle on decision 1 (settled %d)", settled)
	}
}

// A FORWARD ROLLOUT IS STILL FOLLOWED, whichever spelling the running release
// has, so the guard did not become a refusal of everything.
func TestAForwardRolloutIsFollowedFromTheBareForm(t *testing.T) {
	runningAs(t, "0.9.1")

	cfg := ledgerWithForwardRollout(t, "v0.9.2")

	target, act, err := rolloutInstruction(t.Context(), cfg)
	if err != nil {
		t.Fatalf("rolloutInstruction: %v", err)
	}

	if !act || target.pin != "v0.9.2" {
		t.Fatalf("the forward rollout was not followed (act=%v, target=%+v)", act, target)
	}
}
