package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
)

// A launch mints a registration and hands it to the provider, with the tier's
// shape and the job's trust class.
func TestLaunchMintsARegistrationAndStartsIt(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, RunID: 101, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(p.launched) != 1 {
		t.Fatalf("started %d instances, want 1", len(p.launched))
	}

	spec := p.launched[0]

	if spec.JITConfig == "" {
		t.Error("the runner was started with no registration; it would take no job")
	}

	if spec.Image != "ubuntu-2404-x64" || spec.VCPU != 2 {
		t.Errorf("the tier's shape did not reach the provider: %+v", spec)
	}

	// A push is repository code, so it is trusted and a container may run it.
	if spec.Trust != provider.TrustTrusted {
		t.Errorf("a push was classified %s", spec.Trust)
	}
}

// THE RUNNER IS NAMED AFTER THE LEASE, which is unique.
//
// Reusing a name is what made docker refuse to start a container after a crash
// left one behind, and GitHub fails the same way from the other side —
// GenerateJitRunnerConfig returns RunnerExistsError for a name already
// registered. A lease id makes both collisions unreachable rather than recovered
// from.
func TestTheRunnerIsNamedAfterItsLease(t *testing.T) {
	jit := &fakeJIT{setID: 7}
	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, jit, p, []config.Tier{dockerTier()}, nil)

	for range 2 {
		lease := assignedLease(t, a)

		if err := r.Launch(t.Context(), lease, Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
	}

	if len(jit.names) != 2 {
		t.Fatalf("minted %d registrations, want 2", len(jit.names))
	}

	if jit.names[0] == jit.names[1] {
		t.Errorf("two launches asked for the same runner name %q; the second would collide "+
			"with the first on GitHub and in the container runtime", jit.names[0])
	}

	for _, name := range jit.names {
		if !strings.HasPrefix(name, "billet-") || len(name) <= len("billet-") {
			t.Errorf("runner name %q does not identify a lease", name)
		}
	}
}

// A pull request is UNTRUSTED, and a container refuses it.
//
// Not caution for its own sake: a scale-set message carries the event and the
// repository but does NOT say whether a pull request came from a fork. billet
// cannot tell a teammate's PR from a stranger's, and those differ by whether
// arbitrary outside code is about to run on the host. Given it cannot tell, it
// assumes the worse one.
func TestPullRequestsAreUntrustedBecauseForkStatusIsUnknowable(t *testing.T) {
	for _, event := range []string{"pull_request", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			if got := provider.Classify(event); got != provider.TrustUntrusted {
				t.Errorf("%s classified as %s; billet cannot tell a fork PR from a same-repo "+
					"one, so it must assume the worse", event, got)
			}
		})
	}
}

// An event billet does not recognise is UNKNOWN, not trusted.
//
// GitHub adds events. A new one must not inherit permission from a switch
// statement written before it existed.
func TestUnrecognisedEventsAreNotTrusted(t *testing.T) {
	for _, event := range []string{"", "some_future_event", "PUSH"} {
		if got := provider.Classify(event); got == provider.TrustTrusted {
			t.Errorf("event %q was trusted by default; a backend sharing the host kernel "+
				"would accept it", event)
		}
	}
}

// A tier whose provider is not this host's is refused rather than run.
//
// Placement should have caught it, so reaching here means the catalog and the
// host disagree — and running the job anyway puts it on a backend its tier was
// never sized or trusted for.
func TestATierForAnotherBackendIsRefused(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	firecracker := dockerTier()
	firecracker.Provider = config.ProviderFirecracker

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{firecracker}, nil)

	err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("ran a firecracker tier's job on a docker host")
	}

	if len(p.launched) != 0 {
		t.Error("something was started despite the refusal")
	}
}

// Destroy removes what was started, and is idempotent about what was not.
func TestDestroyRemovesWhatWasStarted(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 11); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if len(p.destroyed) != 1 {
		t.Errorf("destroyed %d instances, want 1", len(p.destroyed))
	}

	// Again, and for something never started here. Both run on redelivered
	// completions and on shutdown.
	if err := r.Destroy(t.Context(), 11); err != nil {
		t.Errorf("a second Destroy reported an error: %v", err)
	}

	if err := r.Destroy(t.Context(), 999); err != nil {
		t.Errorf("destroying a request nothing was started for reported an error: %v", err)
	}
}

// An instance that will not die stays tracked.
//
// Forgetting it is how it becomes an orphan nobody can find by request id — and
// the listener above is relying on the failure to hold the lease's capacity.
func TestAnInstanceThatWillNotDieStaysTracked(t *testing.T) {
	p := &fakeProvider{
		kind:       config.ProviderDocker,
		destroyErr: errors.New("the daemon is not answering"),
	}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 11); err == nil {
		t.Fatal("a failed teardown reported success")
	}

	p.destroyErr = nil

	if err := r.Destroy(t.Context(), 11); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}

	// ONE successful removal, from the retry. Zero would mean the instance was
	// forgotten after the first failure — and the give-away is that Destroy would
	// STILL have returned nil, because "nothing tracked" is its idempotent success
	// case. A test that only checked the retry's error would pass against exactly
	// the bug this exists to catch.
	if len(p.destroyed) != 1 {
		t.Errorf("the retry removed %d instances; 0 means the instance was dropped after the "+
			"first failure and became an orphan nothing can find by request id",
			len(p.destroyed))
	}
}

func dockerTier() config.Tier {
	return config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}
}

type fakeJIT struct {
	setID int
	names []string
}

func (f *fakeJIT) Describe(context.Context, string, string) (*Set, []string, error) {
	return &Set{ID: f.setID, Name: "billet-2vcpu"}, nil, nil
}

func (f *fakeJIT) JITConfig(_ context.Context, _ int, name, _ string) (Registration, error) {
	f.names = append(f.names, name)

	return fakeRegistration{name: name}, nil
}

type fakeRegistration struct{ name string }

func (f fakeRegistration) Config() string     { return "encoded-" + f.name }
func (f fakeRegistration) RunnerName() string { return f.name }

type fakeProvider struct {
	kind       config.ProviderKind
	acceptsAll bool
	launched   []provider.Spec
	destroyed  []string
	destroyErr error
}

func (f *fakeProvider) Kind() config.ProviderKind { return f.kind }

// Accepts mirrors a real backend: only established trust is allowed, so a test
// that forgets to classify a job sees the same refusal production would.
func (f *fakeProvider) Accepts(trust provider.TrustClass) error {
	if f.acceptsAll || trust == provider.TrustTrusted {
		return nil
	}

	return errors.New("fake: refusing work that is not established as trusted")
}

func (f *fakeProvider) Launch(_ context.Context, spec provider.Spec) (*provider.Instance, error) {
	f.launched = append(f.launched, spec)

	return &provider.Instance{ID: "instance-" + spec.Name, Name: spec.Name}, nil
}

func (f *fakeProvider) Destroy(_ context.Context, id string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}

	f.destroyed = append(f.destroyed, id)

	return nil
}

// newAllocatorWithHost builds an allocator over a real store with one registered
// host, because Launch BINDS the lease and Bind is where the allocator enforces
// placement — against the provider the host registered, not against a catalog.
// The host registers as docker because that is the only backend that exists.
// When a second one lands this takes the kind as a parameter again.
func newAllocatorWithHost(t *testing.T) (*alloc.Allocator, string) {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB},
		[]config.Tier{dockerTier()})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "test-host"

	if err := a.RegisterNode(t.Context(), host, config.ProviderDocker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a, host
}

// assignedLease reserves and assigns a lease, which is the state Launch expects
// to receive one in.
var nextRequest int64 = 1000

func assignedLease(t *testing.T, a *alloc.Allocator) *alloc.Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), "billet-2vcpu")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	nextRequest++

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, nextRequest, nextRequest); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	lease.RequestID = nextRequest

	return lease
}
