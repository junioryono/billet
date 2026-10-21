package nodeplane_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	billetnode "github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// targetJIT is one target's credential-holding source, recording what it was
// asked so a test can prove the other target's source was asked nothing.
type targetJIT struct {
	// path is the owner the source names, empty for one that names none.
	path string
	// store, when set, is read at each mint for the owner the lease records,
	// so a test can tell whether it was durable before the registration existed.
	store *fakeStore
	// recovery is what a recovery finds on GitHub.
	recovery nodeplane.JITRunnerRecovery

	mu           sync.Mutex
	described    []string
	minted       []string
	validated    []string
	removed      []string
	recovered    []string
	ownerAtMints []string
}

func (j *targetJIT) GitHubTarget() string { return j.path }

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

	if j.store != nil {
		j.store.mu.Lock()
		j.ownerAtMints = append(j.ownerAtMints, j.store.leaseTargets["l1"])
		j.store.mu.Unlock()
	}

	return returnedRegistration{}, nil
}

func (j *targetJIT) RemoveRunner(_ context.Context, _ int64, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.removed = append(j.removed, name)

	return nil
}

func (j *targetJIT) RecoverRunner(_ context.Context, name string) (nodeplane.JITRunnerRecovery, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.recovered = append(j.recovered, name)

	return j.recovery, nil
}

// asked is everything the source was asked to do with a runner.
func (j *targetJIT) asked() (minted, removed, recovered []string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	return append([]string(nil), j.minted...), append([]string(nil), j.removed...),
		append([]string(nil), j.recovered...)
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

	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, dispatch.Job{RequestID: 7}) }()

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

// A node names a tier by its label and the plane describes the scale set by
// the tier's runs_on, on the tier's own target, both for the node's describe
// and for the set a registration is minted in. Two targets may share the name.
func TestAScaleSetIsDescribedByTheTiersRunsOn(t *testing.T) {
	t.Parallel()

	catalogue := twoTargetCatalogue()
	for i := range catalogue {
		catalogue[i].RunsOn = "shared"
	}

	jitA, jitB := &targetJIT{}, &targetJIT{}

	store := &fakeStore{lease: &alloc.Lease{
		ID: "l1", Tier: "tier-b", Node: "n1", Epoch: 1, RequestID: 7,
	}}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithTierCatalog(catalogue), nodeplane.WithCommandTimeout(time.Minute))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	lease := &alloc.Lease{ID: "l1", Tier: "tier-b", Node: "n1", Epoch: 1,
		RequestID: 7, VCPU: 2, Memory: 8 * config.GiB, GuestOS: config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker}}

	launched := make(chan error, 1)

	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, dispatch.Job{RequestID: 7}) }()

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

	set, _, err := c.Describe(t.Context(), "tier-b", "billet")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if set == nil || set.Name != "shared" {
		t.Fatalf("Describe answered %+v, want the set named by runs_on", set)
	}

	if _, err := c.JITConfig(t.Context(), set.ID, "billet-l1", "_work"); err != nil {
		t.Fatalf("JITConfig: %v", err)
	}

	describedB, mintedB, _ := jitB.snapshot()
	if len(describedB) < 2 || len(mintedB) != 1 {
		t.Fatalf("target B's source described %v and minted %v; want the node's describe, "+
			"the plane's own and one mint", describedB, mintedB)
	}

	for _, name := range describedB {
		if name != "shared" {
			t.Errorf("target B's source described %q, not the tier's runs_on", name)
		}
	}

	if describedA, mintedA, _ := jitA.snapshot(); len(describedA)+len(mintedA) != 0 {
		t.Errorf("target A's source was asked about target B's tier: %v %v", describedA, mintedA)
	}
}

// The owners the target tests serve, by path.
const (
	ownerA = "acme"
	ownerB = "someone/widgets"
)

// queueLaunch queues a launch of lease l1 on n1 and takes it from the queue,
// so the node holds the command that entitles it to the lease's registration.
func queueLaunch(t *testing.T, p *nodeplane.Plane, c *nodeclient.Client, tier string) {
	t.Helper()

	lease := &alloc.Lease{ID: "l1", Tier: tier, Node: "n1", Epoch: 1,
		RequestID: 7, VCPU: 2, Memory: 8 * config.GiB, GuestOS: config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker}}

	// THE LAUNCH'S OWN ANSWER IS NOT WHAT THESE TESTS ASK ABOUT: it waits on a
	// report the node never sends, and ends with the test's context.
	launched := make(chan error, 1)
	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, dispatch.Job{RequestID: 7}) }()

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
}

// A MINT RECORDS ITS OWNER BEFORE THE REGISTRATION EXISTS, on the lease, and on
// the binding it journals after: a mint whose answer is lost has only the
// lease's record to be recovered by (#93).
func TestAMintRecordsItsOwnerBeforeTheRegistrationExists(t *testing.T) {
	t.Parallel()

	store := &fakeStore{lease: &alloc.Lease{ID: "l1", Tier: "tier-a", Node: "n1", Epoch: 1, RequestID: 7}}
	jitA := &targetJIT{path: ownerA, store: store}
	jitB := &targetJIT{path: ownerB, store: store}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithTierCatalog(twoTargetCatalogue()), nodeplane.WithCommandTimeout(time.Minute))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)
	queueLaunch(t, p, c, "tier-a")

	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err != nil {
		t.Fatalf("JITConfig: %v", err)
	}

	jitA.mu.Lock()
	atMints := append([]string(nil), jitA.ownerAtMints...)
	jitA.mu.Unlock()

	if !slices.Equal(atMints, []string{ownerA}) {
		t.Errorf("the lease recorded owners %q when the registration was minted, want [%s]", atMints, ownerA)
	}

	member, err := store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.GitHubTarget != ownerA {
		t.Errorf("the binding journaled after the mint is %+v (err %v), want owner %q", member, err, ownerA)
	}

	if minted, _, _ := jitB.asked(); len(minted) != 0 {
		t.Errorf("the other owner minted %v", minted)
	}
}

// A STORE THAT CANNOT JOURNAL A RUNNER IS REFUSED BEFORE ANYTHING IS MINTED: a
// registration nothing records is one nothing removes.
func TestAMintIsRefusedBeforeGitHubWhenItsRunnerCannotBeJournaled(t *testing.T) {
	t.Parallel()

	store := &fakeStore{lease: &alloc.Lease{ID: "l1", Tier: "tier-a", Node: "n1", Epoch: 1, RequestID: 7}}
	jitA := &targetJIT{path: ownerA}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithTierCatalog(twoTargetCatalogue()), nodeplane.WithCommandTimeout(time.Minute))

	// ONLY THE LEASE STORE'S METHODS, so the plane finds no pool-runner journal.
	leasesOnly := struct{ nodeplane.LeaseStore }{store}

	srv := httptest.NewServer(nodeplane.Handler(log, p, leasesOnly, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)
	queueLaunch(t, p, c, "tier-a")

	_, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work")
	if err == nil || !strings.Contains(err.Error(), "cannot preserve runner identity") {
		t.Fatalf("a mint the store cannot journal: %v, want a refusal saying so", err)
	}

	if minted, removed, _ := jitA.asked(); len(minted)+len(removed) != 0 {
		t.Errorf("the source minted %v and removed %v for a runner nothing could journal", minted, removed)
	}
}

// A TIER REMOVED FROM THE CONFIG STILL HAS ITS RUNNERS ON THE OWNER THEY WERE
// MINTED ON, and the plane removes and recovers them through that owner's
// credential, asking the other owner nothing (#93). Before the owner was
// recorded, both were refused: the tier named no target.
func TestARemovedTiersRunnerIsRemovedAndRecoveredOnTheOwnerItWasMintedOn(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		lease:        &alloc.Lease{ID: "l1", Tier: "tier-b", Node: "n1", Epoch: 1, RequestID: 7},
		leaseTargets: map[string]string{"l1": ownerB},
		pool: map[string]alloc.PoolRunner{"billet-l1": {
			LeaseID: "l1", Tier: "tier-b", LaunchRequestID: 7, RunnerID: 91,
			RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle, GitHubTarget: ownerB,
		}},
	}
	jitA, jitB := &targetJIT{path: ownerA}, &targetJIT{path: ownerB}

	// ONLY tier-a REMAINS: tier-b, on the second owner, was removed.
	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()[:1]))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)

	if err := c.RemoveRunner(t.Context(), "l1", 91, "billet-l1"); err != nil {
		t.Fatalf("removing a removed tier's runner: %v", err)
	}

	if _, removed, _ := jitB.asked(); !slices.Equal(removed, []string{"billet-l1"}) {
		t.Errorf("the owner the runner was minted on removed %v, want [billet-l1]", removed)
	}

	// A MINT WHOSE ANSWER WAS LOST: no binding, only the lease's record.
	store.mu.Lock()
	store.pool = nil
	store.lease.Phase = alloc.PhaseQuarantine
	store.mu.Unlock()

	jitB.recovery = nodeplane.JITRunnerRecovery{RunnerID: 92, Present: true}

	recovery, err := c.RecoverRunner(t.Context(), "l1", "tier-b", 7, "billet-l1")
	if err != nil || recovery != billetnode.RunnerRecoveryRetired {
		t.Fatalf("recovering a removed tier's runner = %q, err %v", recovery, err)
	}

	if _, removed, recovered := jitB.asked(); !slices.Equal(recovered, []string{"billet-l1"}) ||
		!slices.Equal(removed, []string{"billet-l1", "billet-l1"}) {
		t.Errorf("the owner the runner was minted on recovered %v and removed %v", recovered, removed)
	}

	member, err := store.PoolRunnerByLease(t.Context(), "l1")
	if err != nil || member.GitHubTarget != ownerB || member.Status != alloc.PoolRunnerRetiring {
		t.Errorf("the recovery journaled %+v (err %v), want a retiring member on %q", member, err, ownerB)
	}

	if minted, removed, recovered := jitA.asked(); len(minted)+len(removed)+len(recovered) != 0 {
		t.Errorf("the other owner was asked about the runner: minted %v removed %v recovered %v",
			minted, removed, recovered)
	}
}

// AN OWNER THE PLANE CANNOT SERVE IS REFUSED, NEVER BORROWED: an owner no
// attached source names, two records that disagree, an owner two sources serve
// with no tier to choose between them, and, for a runner minted before owners
// were recorded, a tier the catalogue no longer has.
func TestARunnersOwnerThePlaneCannotServeIsRefusedNotBorrowed(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		tier          string
		leaseTarget   string
		bindingTarget string
		sources       map[string]string
		want          string
	}{
		"an owner no source serves": {
			tier: "tier-b", leaseTarget: "gone-org", bindingTarget: "gone-org",
			sources: map[string]string{"default": ownerA, "personal": ownerB},
			want:    `registered on GitHub target "gone-org", which this control plane has no credential for`,
		},
		"records that disagree": {
			tier: "tier-a", leaseTarget: ownerB, bindingTarget: ownerA,
			sources: map[string]string{"default": ownerA, "personal": ownerB},
			want:    "cannot tell which credential",
		},
		"an owner two sources serve and no tier names": {
			tier: "tier-gone", leaseTarget: ownerA, bindingTarget: ownerA,
			sources: map[string]string{"default": ownerA, "personal": ownerA},
			want:    "2 of this control plane's targets serve",
		},
		"no owner recorded and the tier removed": {
			tier:    "tier-b",
			sources: map[string]string{"default": ownerA, "personal": ownerB},
			want:    `tier "tier-b" names GitHub target ""`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{
				lease:        &alloc.Lease{ID: "l1", Tier: tc.tier, Node: "n1", Epoch: 1, RequestID: 7},
				leaseTargets: map[string]string{"l1": tc.leaseTarget},
				pool: map[string]alloc.PoolRunner{"billet-l1": {
					LeaseID: "l1", Tier: tc.tier, LaunchRequestID: 7, RunnerID: 91,
					RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle, GitHubTarget: tc.bindingTarget,
				}},
			}

			sources := map[string]nodeplane.JITSource{}
			jits := map[string]*targetJIT{}
			for target, path := range tc.sources {
				jits[target] = &targetJIT{path: path}
				sources[target] = jits[target]
			}

			log := slog.New(slog.DiscardHandler)
			p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()[:1]))

			srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil, nodeplane.WithTargetJIT(sources)))
			t.Cleanup(srv.Close)

			err := dial(t, srv.URL).RemoveRunner(t.Context(), "l1", 91, "billet-l1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("removal: %v, want a refusal saying %q", err, tc.want)
			}

			for target, jit := range jits {
				if _, removed, _ := jit.asked(); len(removed) != 0 {
					t.Errorf("target %s's source removed %v", target, removed)
				}
			}
		})
	}
}

// AN OWNER TWO SOURCES SERVE RESOLVES THROUGH THE CATALOGUE: the source the
// tier's own target names, when its owner is the one recorded, and not the
// other source that happens to serve the same owner.
func TestAnOwnerTwoSourcesServeResolvesThroughTheTiersTarget(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		lease:        &alloc.Lease{ID: "l1", Tier: "tier-a", Node: "n1", Epoch: 1, RequestID: 7},
		leaseTargets: map[string]string{"l1": ownerA},
		pool: map[string]alloc.PoolRunner{"billet-l1": {
			LeaseID: "l1", Tier: "tier-a", LaunchRequestID: 7, RunnerID: 91,
			RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle, GitHubTarget: ownerA,
		}},
	}

	// tier-a's target is "default", and "second" serves the same owner.
	tiersOwn, other := &targetJIT{path: ownerA}, &targetJIT{path: ownerA}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()[:1]))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": tiersOwn, "second": other})))
	t.Cleanup(srv.Close)

	if err := dial(t, srv.URL).RemoveRunner(t.Context(), "l1", 91, "billet-l1"); err != nil {
		t.Fatalf("removal: %v", err)
	}

	if _, removed, _ := tiersOwn.asked(); !slices.Equal(removed, []string{"billet-l1"}) {
		t.Errorf("the tier's own target removed %v, want [billet-l1]", removed)
	}

	if _, removed, _ := other.asked(); len(removed) != 0 {
		t.Errorf("the other source serving the owner removed %v", removed)
	}
}

// A MINT FOR A LAUNCH WHOSE LEASE WAS RECLAIMED IS REFUSED BEFORE GITHUB: the
// owner is written under the LAUNCH's epoch, which a reclaim has moved past,
// never under whatever epoch the lease has reached since.
func TestAMintForAReclaimedLaunchIsRefusedBeforeGitHub(t *testing.T) {
	t.Parallel()

	store := &fakeStore{lease: &alloc.Lease{ID: "l1", Tier: "tier-a", Node: "n1", Epoch: 1, RequestID: 7}}
	jitA := &targetJIT{path: ownerA}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithTierCatalog(twoTargetCatalogue()), nodeplane.WithCommandTimeout(time.Minute))

	srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
		nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA})))
	t.Cleanup(srv.Close)

	c := dial(t, srv.URL)
	queueLaunch(t, p, c, "tier-a")

	// THE REAPER RECLAIMS THE LEASE after the command was given: its epoch moves on.
	store.mu.Lock()
	store.lease.Epoch = 2
	store.lease.Phase = alloc.PhaseQuarantine
	store.mu.Unlock()

	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err == nil {
		t.Fatal("a registration was minted for a launch whose lease was reclaimed")
	}

	if minted, _, _ := jitA.asked(); len(minted) != 0 {
		t.Errorf("the source minted %v for a launch whose lease was reclaimed", minted)
	}

	if owner, _ := store.LeaseGitHubTarget(t.Context(), "l1"); owner != "" {
		t.Errorf("the reclaimed lease recorded owner %q", owner)
	}
}

// AN ENDED LEASE STILL SAYS WHICH OWNER ITS RUNNER WAS MINTED ON, and a removal
// for it reads that record rather than the catalogue, refusing when it and the
// binding disagree.
func TestAnEndedLeasesRunnerIsRemovedOnTheOwnerItsLeaseRecorded(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		bindingTarget string
		wantRemovedB  bool
		wantRefusal   string
	}{
		"a binding that recorded no owner": {wantRemovedB: true},
		"a binding that disagrees":         {bindingTarget: ownerA, wantRefusal: "cannot tell which credential"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ENDED: the ledger reports no live lease, and its job's history names
			// this node.
			store := &fakeStore{
				leaseErr:     alloc.ErrLeaseNotFound,
				historyNode:  map[string]string{"l1": "n1"},
				leaseTargets: map[string]string{"l1": ownerB},
				pool: map[string]alloc.PoolRunner{"billet-l1": {
					LeaseID: "l1", Tier: "tier-b", LaunchRequestID: 7, RunnerID: 91,
					RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle, GitHubTarget: tc.bindingTarget,
				}},
			}
			jitA, jitB := &targetJIT{path: ownerA}, &targetJIT{path: ownerB}

			log := slog.New(slog.DiscardHandler)
			p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(twoTargetCatalogue()[:1]))

			srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil,
				nodeplane.WithTargetJIT(map[string]nodeplane.JITSource{"default": jitA, "personal": jitB})))
			t.Cleanup(srv.Close)

			err := dial(t, srv.URL).RemoveRunner(t.Context(), "l1", 91, "billet-l1")

			switch {
			case tc.wantRefusal != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantRefusal) {
					t.Fatalf("removal: %v, want a refusal saying %q", err, tc.wantRefusal)
				}
			case err != nil:
				t.Fatalf("removal: %v", err)
			}

			_, removedB, _ := jitB.asked()
			if got := slices.Equal(removedB, []string{"billet-l1"}); got != tc.wantRemovedB {
				t.Errorf("the lease's recorded owner removed %v", removedB)
			}

			if _, removedA, _ := jitA.asked(); len(removedA) != 0 {
				t.Errorf("the other owner removed %v", removedA)
			}
		})
	}
}

// A RECOVERY KEEPS THE OWNER IT FOUND THE RUNNER ON, for a member journaled
// before owners were recorded, so a later recovery asks that owner again
// whatever the catalogue names by then; and it keeps none an owner it merely
// asked, and was told the runner is not there, because that proves nothing
// about where the runner is.
func TestARecoveryKeepsOnlyTheOwnerItFoundTheRunnerOn(t *testing.T) {
	t.Parallel()

	legacy := func() *fakeStore {
		return &fakeStore{
			lease: &alloc.Lease{ID: "l1", Tier: "tier-a", Node: "n1", Epoch: 1, RequestID: 7,
				Phase: alloc.PhaseQuarantine},
			pool: map[string]alloc.PoolRunner{"billet-l1": {
				LeaseID: "l1", Tier: "tier-a", LaunchRequestID: 7, RunnerID: 91,
				RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle,
			}},
		}
	}

	serve := func(t *testing.T, store *fakeStore, catalogue []config.Tier, sources map[string]nodeplane.JITSource,
	) *nodeclient.Client {
		t.Helper()

		log := slog.New(slog.DiscardHandler)
		p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithTierCatalog(catalogue))
		srv := httptest.NewServer(nodeplane.Handler(log, p, store, nil, nodeplane.WithTargetJIT(sources)))
		t.Cleanup(srv.Close)

		return dial(t, srv.URL)
	}

	t.Run("found busy", func(t *testing.T) {
		t.Parallel()

		store := legacy()
		jitA := &targetJIT{path: ownerA, recovery: nodeplane.JITRunnerRecovery{RunnerID: 91, Present: true, Busy: true}}
		jitB := &targetJIT{path: ownerB}
		sources := map[string]nodeplane.JITSource{"default": jitA, "personal": jitB}

		// tier-a's target is "default", owner A, which finds the runner busy.
		c := serve(t, store, twoTargetCatalogue(), sources)
		if got, err := c.RecoverRunner(t.Context(), "l1", "tier-a", 7, "billet-l1"); err != nil ||
			got != billetnode.RunnerRecoveryBusy {
			t.Fatalf("recovery = %q, err %v; want busy", got, err)
		}

		member, err := store.PoolRunnerByLease(t.Context(), "l1")
		if err != nil || member.GitHubTarget != ownerA {
			t.Fatalf("the member found busy on %q is journaled with owner %q (err %v)", ownerA,
				member.GitHubTarget, err)
		}

		// THE CATALOGUE MOVES tier-a TO THE OTHER OWNER, and the next recovery
		// still asks the one the runner was found on.
		moved := twoTargetCatalogue()
		moved[0].Target = "personal"
		c = serve(t, store, moved, sources)
		if _, err := c.RecoverRunner(t.Context(), "l1", "tier-a", 7, "billet-l1"); err != nil {
			t.Fatalf("the second recovery: %v", err)
		}

		if _, _, recoveredB := jitB.asked(); len(recoveredB) != 0 {
			t.Errorf("the owner the catalogue names now was asked %v about a runner found on %q",
				recoveredB, ownerA)
		}

		if _, _, recoveredA := jitA.asked(); len(recoveredA) != 2 {
			t.Errorf("the owner the runner was found on was asked %d times, want 2", len(recoveredA))
		}
	})

	t.Run("not there", func(t *testing.T) {
		t.Parallel()

		store := legacy()
		store.pool = nil
		jitA := &targetJIT{path: ownerA}

		c := serve(t, store, twoTargetCatalogue(), map[string]nodeplane.JITSource{"default": jitA})
		if got, err := c.RecoverRunner(t.Context(), "l1", "tier-a", 7, "billet-l1"); err != nil ||
			got != billetnode.RunnerRecoveryRetired {
			t.Fatalf("recovery = %q, err %v; want retired", got, err)
		}

		member, err := store.PoolRunnerByLease(t.Context(), "l1")
		if err != nil || member.GitHubTarget != "" {
			t.Errorf("a member the asked owner did not have is journaled with owner %q (err %v), want none",
				member.GitHubTarget, err)
		}
	})
}
