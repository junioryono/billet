package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// fakeOf is the fake provisioner behind a target, or a failed test.
func fakeOf(t *testing.T, target Target) *fakeProvisioner {
	t.Helper()

	prov, ok := target.Provisioner.(*fakeProvisioner)
	if !ok {
		t.Fatalf("target %s does not hold a fake provisioner: %T", target.Config.Name, target.Provisioner)
	}

	return prov
}

// twoTargets is an organization target and a repository target, each with its
// own provisioner, and one tier on each.
func twoTargets() (Target, Target, []config.Tier) {
	org := Target{
		Config:      config.GitHubTarget{Name: "default", Org: "acme"},
		Provisioner: &fakeProvisioner{},
	}
	repo := Target{
		Config:      config.GitHubTarget{Name: "personal", Repository: "someone/widgets"},
		Provisioner: &fakeProvisioner{},
	}

	onOrg := tier("billet-4vcpu-a")
	onOrg.Target = "default"

	onRepo := tier("billet-4vcpu-b")
	onRepo.Target = "personal"

	return org, repo, []config.Tier{onOrg, onRepo}
}

// A tier on target B is reconciled and polled through B's provisioner, and A's
// never hears of it: the credential that creates a scale set is that target's,
// and a scale set created with the wrong one lands on the wrong owner.
func TestEachTierGoesThroughItsOwnTargetsProvisioner(t *testing.T) {
	org, repo, tiers := twoTargets()

	var (
		mu       sync.Mutex
		sessions = map[string][]string{}
	)

	// Each provisioner records which labels it was asked to open sessions for,
	// and the first poll on either ends the run.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	for name, target := range map[string]Target{"org": org, "repo": repo} {
		prov := fakeOf(t, target)
		prov.newSession = func(label string) Session {
			mu.Lock()
			sessions[name] = append(sessions[name], label)
			mu.Unlock()

			return &fakeSession{onPoll: func(int) { cancel() }}
		}
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	if err := New(a, nil, tiers, "test-owner", slog.New(slog.DiscardHandler),
		WithTargets(org, repo)).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if got := sessions["org"]; len(got) != 1 || got[0] != "billet-4vcpu-a" {
		t.Errorf("the organization's provisioner opened sessions for %v, want only its own tier", got)
	}

	if got := sessions["repo"]; len(got) != 1 || got[0] != "billet-4vcpu-b" {
		t.Errorf("the repository's provisioner opened sessions for %v, want only its own tier", got)
	}

	// AND THE SCALE SETS WERE CREATED ON THE RIGHT SIDE: each fake numbered the
	// sets it created, so a tier reconciled through the wrong provisioner would
	// leave one fake with two and the other with none.
	for name, target := range map[string]Target{"org": org, "repo": repo} {
		prov := fakeOf(t, target)
		if len(prov.labels) != 1 {
			t.Errorf("the %s provisioner created %d scale sets, want 1: %v", name, len(prov.labels), prov.labels)
		}
	}
}

func TestATrustedTierUnderARepositoryTargetIsRefusedBeforeAnyListener(t *testing.T) {
	org, repo, tiers := twoTargets()

	tiers[1].Trust = config.WorkloadTrusted
	tiers[1].RunnerGroup = "billet"
	tiers[1].Workflows = []string{"someone/widgets/.github/workflows/ci.yml@refs/heads/main"}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	err := New(a, nil, tiers, "test-owner", slog.New(slog.DiscardHandler),
		WithTargets(org, repo)).Run(t.Context())
	if err == nil {
		t.Fatal("Run accepted a trusted tier under a repository target")
	}

	if !strings.Contains(err.Error(), `trusted under repository target "personal"`) {
		t.Errorf("the refusal does not name the rule: %v", err)
	}

	// NOTHING WAS RECONCILED. The refusal is the first thing Run does with the
	// tiers, so neither provisioner created a scale set.
	for name, target := range map[string]Target{"org": org, "repo": repo} {
		if prov := fakeOf(t, target); len(prov.labels) != 0 {
			t.Errorf("the %s provisioner created scale sets before the refusal: %v", name, prov.labels)
		}
	}
}

func TestATierNamingNoDeclaredTargetStopsRun(t *testing.T) {
	org, repo, tiers := twoTargets()

	for name, target := range map[string]string{"an unknown target": "elsewhere", "no target": ""} {
		t.Run(name, func(t *testing.T) {
			tiers := append([]config.Tier(nil), tiers...)
			tiers[1].Target = target

			a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

			err := New(a, nil, tiers, "test-owner", slog.New(slog.DiscardHandler),
				WithTargets(org, repo)).Run(t.Context())
			if err == nil || !strings.Contains(err.Error(), "which this control plane does not serve") {
				t.Fatalf("Run with %s: %v", name, err)
			}
		})
	}
}

// With ONE target declared, a tier naming none resolves to it — which is every
// deployment written before targets existed.
func TestASingleTargetServesTiersThatNameNone(t *testing.T) {
	org, _, tiers := twoTargets()
	tiers = tiers[:1]
	tiers[0].Target = ""

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	prov := fakeOf(t, org)
	prov.newSession = func(string) Session {
		return &fakeSession{onPoll: func(int) { cancel() }}
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	if err := New(a, nil, tiers, "test-owner", slog.New(slog.DiscardHandler),
		WithTargets(org)).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(prov.labels) != 1 {
		t.Errorf("the only target's provisioner created %d scale sets, want 1", len(prov.labels))
	}
}
