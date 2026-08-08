package node

import (
	"context"
	"errors"
	"sort"
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

	// name overrides what GitHub calls the runner, so a test can prove billet
	// does not name the INSTANCE after it. The two are separate identities and
	// conflating them is what made an orphan unattributable.
	name string
}

func (f *fakeJIT) Describe(context.Context, string, string) (*Set, []string, error) {
	return &Set{ID: f.setID, Name: "billet-2vcpu"}, nil, nil
}

func (f *fakeJIT) JITConfig(_ context.Context, _ int, name, _ string) (Registration, error) {
	f.names = append(f.names, name)

	if f.name != "" {
		return fakeRegistration{name: f.name}, nil
	}

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

	// live is what the backend is actually running, keyed by name. A map rather
	// than a counter because Find and List have to agree with Launch and Destroy:
	// a fake where the four drift apart proves nothing about reconciliation,
	// which exists precisely to compare one against another.
	live map[string]*provider.Instance

	// launchErr makes Launch fail. startsAnyway makes it fail AFTER recording the
	// instance — the ambiguous outcome that a failed launch cannot distinguish
	// from a clean one, and the whole reason Find exists.
	launchErr    error
	startsAnyway bool

	findErr error
	listErr error

	// cancelOnLaunch cancels the caller's context from INSIDE Launch, which is
	// what a real timeout does. Cancelling before the call instead means Bind
	// fails first and the launch path is never reached at all — the test then
	// passes because nothing happened, which is not the same as passing because
	// cleanup worked.
	cancelOnLaunch context.CancelFunc
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

	if f.cancelOnLaunch != nil {
		f.cancelOnLaunch()
	}

	if f.launchErr != nil && !f.startsAnyway {
		return nil, f.launchErr
	}

	inst := &provider.Instance{ID: "instance-" + spec.Name, Name: spec.Name}
	f.add(inst)

	if f.launchErr != nil {
		return nil, f.launchErr
	}

	return inst, nil
}

func (f *fakeProvider) Find(ctx context.Context, name string) (*provider.Instance, bool, error) {
	// CONTEXT IS HONOURED, because a fake that ignores it cannot tell a caller
	// using a cancelled context from one using a fresh one — and that difference
	// is the entire subject of the cancelled-launch test.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if f.findErr != nil {
		return nil, false, f.findErr
	}

	inst, ok := f.live[name]

	return inst, ok, nil
}

func (f *fakeProvider) List(ctx context.Context) ([]*provider.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if f.listErr != nil {
		return nil, f.listErr
	}

	var out []*provider.Instance

	for _, inst := range f.live {
		out = append(out, inst)
	}

	// Sorted so a test asserting on order is not flaky; map iteration is random
	// by design and a reconciliation bug that only appears in one ordering is
	// exactly the kind this suite should be able to reproduce.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

func (f *fakeProvider) Destroy(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if f.destroyErr != nil {
		return f.destroyErr
	}

	f.destroyed = append(f.destroyed, id)

	for name, inst := range f.live {
		if inst.ID == id {
			delete(f.live, name)
		}
	}

	return nil
}

// add records an instance as running.
func (f *fakeProvider) add(inst *provider.Instance) {
	if f.live == nil {
		f.live = make(map[string]*provider.Instance)
	}

	f.live[inst.Name] = inst
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

// ============================================================================
// A launch error is not proof nothing started.
// ============================================================================

func TestLaunchRemovesWhatAFailedLaunchStarted(t *testing.T) {
	t.Parallel()

	// The ambiguous outcome: the backend created the instance and THEN failed to
	// report it. Nothing in the error distinguishes this from a launch that never
	// began, which is why the code has to go and look.
	p := &fakeProvider{
		kind:         config.ProviderDocker,
		launchErr:    errors.New("context deadline exceeded"),
		startsAnyway: true,
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if len(p.live) != 0 {
		t.Fatalf("a failed launch left %d instance(s) running: %v", len(p.live), p.live)
	}

	if len(p.destroyed) != 1 {
		t.Fatalf("the stray was not destroyed: destroyed %v", p.destroyed)
	}
}

func TestLaunchDoesNotDestroyWhenNothingStarted(t *testing.T) {
	t.Parallel()

	// The unambiguous failure. Find answers "nothing there", and the code must
	// take that answer rather than destroying on suspicion — a Destroy issued
	// against an id that was never created is at best noise and at worst aimed at
	// something else's instance.
	p := &fakeProvider{
		kind:      config.ProviderDocker,
		launchErr: errors.New("no such image"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if len(p.destroyed) != 0 {
		t.Fatalf("destroyed something that was never started: %v", p.destroyed)
	}
}

func TestLaunchSurvivesAFailedLaunchOnACancelledContext(t *testing.T) {
	t.Parallel()

	// The usual reason a launch fails is that the caller's context ran out, and
	// cleanup that inherits that context cannot run. So the cancellation happens
	// DURING the launch, exactly as a timeout would: everything before it
	// succeeds, and only the cleanup afterwards has to cope with a dead context.
	//
	// Cancelling BEFORE the call instead — the obvious way to write this — makes
	// Bind fail first, so the launch path is never reached and the test passes
	// because nothing happened.
	ctx, cancel := context.WithCancel(t.Context())

	p := &fakeProvider{
		kind:           config.ProviderDocker,
		launchErr:      context.Canceled,
		startsAnyway:   true,
		cancelOnLaunch: cancel,
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(ctx, lease, Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch on a cancelled context reported success")
	}

	// Proves the launch path was actually reached. Without this, the assertion
	// below is satisfied by a Bind that failed before anything ever started.
	if len(p.launched) != 1 {
		t.Fatal("the provider was never asked to launch; this test proved nothing")
	}

	if len(p.live) != 0 {
		t.Fatalf("a cancelled launch left an instance behind: %v", p.live)
	}
}

func TestLaunchNamesTheInstanceAfterTheLease(t *testing.T) {
	t.Parallel()

	// GitHub's runner name and billet's instance name are different things, and
	// the instance must carry billet's — it is the only thing that survives a
	// crash to say which lease the instance belongs to.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7, name: "github-picked-this"}, p,
		[]config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	want := provider.InstanceName(lease.ID)

	if got := p.launched[0].Name; got != want {
		t.Errorf("instance named %q, want %q — a name without the lease in it cannot be reconciled", got, want)
	}
}

// ============================================================================
// Reconcile: what survived a crash, and whether anything still wants it.
// ============================================================================

func TestReconcileDestroysAnInstanceWhoseLeaseIsGone(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	// A previous process launched this and died. The lease was reaped, so nothing
	// wants the container any more — but the container does not know that.
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// A fresh runner, because a restart is what this models: the in-memory map is
	// empty and the instance is all that is left.
	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("the orphan survived reconciliation: %v", p.live)
	}
}

func TestReconcileKeepsAnInstanceWhoseLeaseIsStillOpen(t *testing.T) {
	t.Parallel()

	// The mirror image, and the one that matters more: reconciliation that
	// destroys live work is far worse than reconciliation that misses an orphan.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("reconciliation destroyed a running job: %v", p.live)
	}

	if len(p.destroyed) != 0 {
		t.Fatalf("reconciliation destroyed %v, whose lease was still open", p.destroyed)
	}
}

func TestReconcileIgnoresInstancesBilletDidNotName(t *testing.T) {
	t.Parallel()

	// Should be unreachable — the provider filters by billet's own label — but
	// the action here is destruction, and "should be unreachable" is not a good
	// enough reason to destroy something whose name billet did not choose.
	p := &fakeProvider{kind: config.ProviderDocker}
	p.add(&provider.Instance{ID: "someone-elses", Name: "postgres-dev"})

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(p.destroyed) != 0 {
		t.Fatalf("destroyed an instance billet did not name: %v", p.destroyed)
	}
}

func TestReconcileReportsInstancesItCouldNotDestroy(t *testing.T) {
	t.Parallel()

	// An orphan that resists destruction is still holding vCPU and memory the
	// allocator has already handed back out. Returning nil here would let the
	// caller start admitting work against capacity that does not exist.
	p := &fakeProvider{kind: config.ProviderDocker, destroyErr: errors.New("daemon is not responding")}
	p.add(&provider.Instance{ID: "stuck", Name: provider.InstanceName("deadbeef")})

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("reported success while an orphan was still holding capacity")
	}
}

func TestReconcileReportsAProviderThatCannotBeEnumerated(t *testing.T) {
	t.Parallel()

	// Not knowing what is running is not the same as nothing running, and
	// treating it as the latter is how a restart over-commits the host.
	p := &fakeProvider{kind: config.ProviderDocker, listErr: errors.New("docker daemon is not running")}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("an unreadable provider was treated as an empty one")
	}
}

func TestReconcileDestroysAnInstanceBoundToAnotherNode(t *testing.T) {
	t.Parallel()

	// A lease open on a DIFFERENT host does not justify an instance running on
	// this one. Scoping the query to this node is what makes that true, and a
	// query over every node would have spared this orphan forever.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	if err := a.RegisterNode(t.Context(), "other-host", config.ProviderDocker); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	lease := assignedLease(t, a)

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "other-host"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	p.add(&provider.Instance{ID: "stray", Name: provider.InstanceName(lease.ID)})

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("kept an instance whose lease belongs to another host: %v", p.live)
	}
}
