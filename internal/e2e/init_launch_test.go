package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/initconfig"
)

// A CONFIG `billet init` GENERATES ACTUALLY RUNS A JOB.
//
// This is the test the docker trial showed was missing: it shipped generating an
// UNTRUSTED tier — which loads, and passes `billet check` — and then the docker
// provider refused its first job at the launch boundary, because a container
// shares the host kernel. A config-load-only test could never see that: the gap
// is between "the config is valid" and "a job launches on it", which is exactly
// the seam this suite covers.
//
// It proves the POLICY the generator produced (trust + runner group + workflow
// allowlist), not the runner image byte-for-byte: a real actions-runner image
// cannot stay running against a fake Actions service, so the label, image and
// command are overridden to the harness's always-running fixture while the
// generated trust/runner_group/workflows are carried through unchanged. What
// launches the container is that policy being accepted end to end.
func TestAGeneratedDockerConfigLaunchesAnAllowedJob(t *testing.T) {
	// The policy the fake Actions service enforces for testGroup.
	const allowedWorkflow = "acme/test/.github/workflows/e2e.yml@refs/heads/main"

	body, _, err := initconfig.Generate(initconfig.Params{
		Org:         "acme",
		Provider:    config.ProviderDocker,
		Image:       testImage,
		VCPU:        8,
		Memory:      16 * config.GiB,
		RunnerGroup: testGroup,
		Workflows:   []string{allowedWorkflow},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Load it the way `github-app create` leaves it plus the App the operator then
	// installs, so the tiers under test are exactly what init decided.
	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}
	if len(cfg.Tiers) == 0 {
		t.Fatal("the generated config has no tiers")
	}

	// The GENERATED tier drives the stack, not a reconstruction of it. Only what
	// the fake fixture needs is overridden — the label the fake serves, an image
	// that pulls fast, and a command that stays up (a real actions-runner exits at
	// once against a fake Actions service). Everything else — the provider, the
	// trust, the runner group, the workflow allowlist, the sizing — is exactly what
	// init emitted, so a generation that produced a Docker-ineligible provider or
	// an oversize shape would fail this test rather than pass it.
	tier := cfg.Tiers[0]
	if tier.Trust != config.WorkloadTrusted || tier.RunnerGroup != testGroup ||
		!slices.Equal(tier.Workflows, []string{allowedWorkflow}) {
		t.Fatalf("the generated tier does not carry a trusted, allowlisted policy: %+v", tier)
	}
	tier.Label = testTier
	tier.Image = testImage
	tier.Command = []string{"sleep", "300"}

	s := newStack(t, withTiers([]config.Tier{tier}))

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5001, "push", testTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job on the generated tier")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5001, "push", testTier))

	// THE ASSERTION THIS TEST IS ABOUT: a container actually runs. On the shipped
	// untrusted generation this never happened — the provider refused the job
	// before a container was created. awaitOneRunning fails the test if no
	// container comes up within its deadline.
	s.awaitOneRunning(t)

	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		fakeactions.JobJSON("JobCompleted", 5001, "push", testTier))

	s.awaitGone(t)
}
