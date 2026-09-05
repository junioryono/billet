package nodeplane_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
)

// targetJIT is one target's credential-holding source, recording what it was
// asked so a test can prove the other target's source was asked nothing.
type targetJIT struct {
	mu        sync.Mutex
	described []string
	minted    []string
	validated []string
}

func (j *targetJIT) Describe(_ context.Context, name, _ string) (*nodeplane.JITSet, []string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.described = append(j.described, name)

	return &nodeplane.JITSet{ID: 7, Name: name}, nil, nil
}

func (j *targetJIT) JITConfig(_ context.Context, _ int, name, _ string) (nodeplane.JITRegistration, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.minted = append(j.minted, name)

	return returnedRegistration{}, nil
}

func (*targetJIT) RemoveRunner(context.Context, int64, string) error { return nil }

func (*targetJIT) RecoverRunner(context.Context, string) (nodeplane.JITRunnerRecovery, error) {
	return nodeplane.JITRunnerRecovery{}, nil
}

func (j *targetJIT) ValidateTrustedRunnerGroup(_ context.Context, group string, _ []string) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.validated = append(j.validated, group)

	return nil
}

func (j *targetJIT) snapshot() (described, minted, validated []string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return append([]string(nil), j.described...), append([]string(nil), j.minted...),
		append([]string(nil), j.validated...)
}

// twoTargetCatalogue is a tier on each of two targets.
func twoTargetCatalogue() []config.Tier {
	base := config.Tier{Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64"}

	onA, onB := base, base
	onA.Label, onA.Target = "tier-a", "default"
	onB.Label, onB.Target = "tier-b", "personal"
	onB.Trust, onB.RunnerGroup = config.WorkloadTrusted, "billet"
	onB.Workflows = []string{"someone/widgets/.github/workflows/ci.yml@refs/heads/main"}

	return []config.Tier{onA, onB}
}

// A registration for a tier on target B is minted through B's source, its
// scale set is described through B's source, and A's source is asked nothing:
// the credential that registers a runner is the tier's target's, and minting
// through another owner's App would register the runner on the wrong owner.
func TestARegistrationIsMintedWithItsTiersTargetCredential(t *testing.T) {
	t.Parallel()

	jitA, jitB := &targetJIT{}, &targetJIT{}

	store := &fakeStore{lease: &alloc.Lease{
		ID: "l1", Tier: "tier-b", Node: "n1", Epoch: 1, RequestID: 7,
	}}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithTierCatalog(twoTargetCatalogue()), nodeplane.WithCommandTimeout(time.Minute))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	lease := &alloc.Lease{ID: "l1", Tier: "tier-b", Node: "n1", Epoch: 1,
		RequestID: 7, VCPU: 2, Memory: 8 * config.GiB, GuestOS: config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker}}

	launched := make(chan error, 1)

	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}) }()

	deadline := time.Now().Add(5 * time.Second)
	for p.QueuedForTest("n1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("launch was never queued")
		}

		time.Sleep(time.Millisecond)
	}

	if _, ok, err := c.Poll(t.Context()); err != nil || !ok {
		t.Fatalf("Poll = ok %v, err %v", ok, err)
	}

	// The node's own sequence for a trusted tier: describe, revalidate, mint.
	if _, _, err := c.Describe(t.Context(), "tier-b", "billet"); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if err := c.ValidateTrustedRunnerGroup(t.Context(), "tier-b", "billet", nil); err != nil {
		t.Fatalf("ValidateTrustedRunnerGroup: %v", err)
	}

	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err != nil {
		t.Fatalf("JITConfig: %v", err)
	}

	// B's source is asked more than once — the plane describes the set and
	// revalidates the group itself before it mints — but only ever about B's
	// tier, and it mints exactly once.
	describedB, mintedB, validatedB := jitB.snapshot()
	if len(describedB) == 0 || len(validatedB) == 0 || len(mintedB) != 1 || mintedB[0] != "billet-l1" {
		t.Errorf("target B's source saw described=%v minted=%v validated=%v", describedB, mintedB, validatedB)
	}

	for _, name := range describedB {
		if name != "tier-b" {
			t.Errorf("target B's source described %q", name)
		}
	}

	for _, group := range validatedB {
		if group != "billet" {
			t.Errorf("target B's source validated group %q", group)
		}
	}

	describedA, mintedA, validatedA := jitA.snapshot()
	if len(describedA)+len(mintedA)+len(validatedA) != 0 {
		t.Errorf("target A's source was asked about target B's tier: described=%v minted=%v validated=%v",
			describedA, mintedA, validatedA)
	}

	// And a tier on A goes to A.
	if _, _, err := c.Describe(t.Context(), "tier-a", ""); err != nil {
		t.Fatalf("Describe tier-a: %v", err)
	}

	if describedA, _, _ := jitA.snapshot(); len(describedA) != 1 || describedA[0] != "tier-a" {
		t.Errorf("target A's source described %v, want [tier-a]", describedA)
	}
}

// A node below the wire version that names the tier can still revalidate a
// runner group on a control plane serving ONE target, and is refused, naming
// the version, on one serving several.
func TestAnUntargetedRunnerGroupCheckIsAnsweredOnlyWhereThereIsOneTarget(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	t.Run("one target", func(t *testing.T) {
		t.Parallel()

		jit := &targetJIT{}
		p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()[:1]))

		srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil,
			nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jit})))
		t.Cleanup(srv.Close)

		// An empty tier is what a node below VersionTargetedRunnerGroup sends.
		if err := dial(t, srv.URL).ValidateTrustedRunnerGroup(t.Context(), "", "billet", nil); err != nil {
			t.Fatalf("an untargeted check on a single-target plane was refused: %v", err)
		}

		if _, _, validated := jit.snapshot(); len(validated) != 1 {
			t.Errorf("the only target's source validated %v, want one call", validated)
		}
	})

	t.Run("several targets", func(t *testing.T) {
		t.Parallel()

		jitA, jitB := &targetJIT{}, &targetJIT{}
		p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()))

		srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil,
			nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
		t.Cleanup(srv.Close)

		err := dial(t, srv.URL).ValidateTrustedRunnerGroup(t.Context(), "", "billet", nil)
		if err == nil || !strings.Contains(err.Error(), "wire version 21") {
			t.Fatalf("an untargeted check on a two-target plane: %v, want a refusal naming version 21", err)
		}

		for name, jit := range map[string]*targetJIT{"A": jitA, "B": jitB} {
			if _, _, validated := jit.snapshot(); len(validated) != 0 {
				t.Errorf("target %s's source was asked to validate %v with no tier named", name, validated)
			}
		}
	})
}

// A tier whose target has no attached source is refused, never served by
// another target's credential.
func TestATierWithNoCredentialIsRefusedNotBorrowed(t *testing.T) {
	t.Parallel()

	jitA := &targetJIT{}
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()))

	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA})))
	t.Cleanup(srv.Close)

	_, _, err := dial(t, srv.URL).Describe(t.Context(), "tier-b", "billet")
	if err == nil || !strings.Contains(err.Error(), `GitHub target "personal"`) {
		t.Fatalf("Describe of a tier with no credential: %v", err)
	}

	if described, _, _ := jitA.snapshot(); len(described) != 0 {
		t.Errorf("target A's source described %v for a tier that is not its own", described)
	}
}
