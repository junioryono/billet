package node

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	// jitErr fails registration, which is what makes the scale-set cache drop
	// reachable.
	jitErr error

	// describes counts scale-set resolutions, so a test can tell a cached answer
	// from a fresh one.
	describes int
}

func (f *fakeJIT) Describe(context.Context, string, string) (*Set, []string, error) {
	f.describes++

	return &Set{ID: f.setID, Name: "billet-2vcpu"}, nil, nil
}

func (f *fakeJIT) JITConfig(_ context.Context, _ int, name, _ string) (Registration, error) {
	f.names = append(f.names, name)

	if f.jitErr != nil {
		return nil, f.jitErr
	}

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

	// findDelay widens the window inside a custody transition, so two callers
	// that are supposed to be serialized actually overlap if they are not. A race
	// whose window is nanoseconds is one the detector reports only occasionally,
	// which is the same as not testing it.
	findDelay time.Duration
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

	inst := &provider.Instance{ID: "instance-" + spec.Name, Name: spec.Name, Running: true}
	f.add(inst)

	if f.launchErr != nil {
		return nil, f.launchErr
	}

	return inst, nil
}

func (f *fakeProvider) Find(ctx context.Context, name string) (*provider.Instance, bool, error) {
	if f.findDelay > 0 {
		time.Sleep(f.findDelay)
	}

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

// stop marks an instance finished without removing it, which is what a
// container that ran its job and exited looks like: still there, still holding
// its name and its disk, no longer doing anything.
func (f *fakeProvider) stop(name string) {
	if inst, ok := f.live[name]; ok {
		inst.Running = false
	}
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
//
// Atomic because these tests run in parallel. Separate databases made duplicate
// request ids harmless in practice, which is exactly why the race would have sat
// there being reported only by -race on an unlucky run.
var nextRequest atomic.Int64

func assignedLease(t *testing.T, a *alloc.Allocator) *alloc.Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), "billet-2vcpu")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	request := nextRequest.Add(1)

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, request, request); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	lease.RequestID = request

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

func TestRecoverIgnoresInstancesBilletDidNotName(t *testing.T) {
	t.Parallel()

	// Should be unreachable — the provider filters by this deployment's own label
	// — but the action here is destruction, and "should be unreachable" is not a
	// good enough reason to destroy something whose name billet did not choose.
	p := &fakeProvider{kind: config.ProviderDocker}
	p.add(&provider.Instance{ID: "someone-elses", Name: "postgres-dev"})

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(p.destroyed) != 0 {
		t.Fatalf("destroyed an instance billet did not name: %v", p.destroyed)
	}
}

func TestRecoverReportsInstancesItCouldNotDestroy(t *testing.T) {
	t.Parallel()

	// An instance that resists destruction is still holding vCPU and memory the
	// allocator has already handed back out. Returning nil here would let the
	// caller start admitting work against capacity that does not exist.
	p := &fakeProvider{kind: config.ProviderDocker, destroyErr: errors.New("daemon is not responding")}
	p.add(&provider.Instance{ID: "stuck", Name: provider.InstanceName("deadbeef")})

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err == nil {
		t.Fatal("reported success while an instance was still holding capacity")
	}
}

func TestRecoverReportsAProviderThatCannotBeEnumerated(t *testing.T) {
	t.Parallel()

	// Not knowing what is running is not the same as nothing running, and
	// treating it as the latter is how a restart over-commits the host.
	p := &fakeProvider{kind: config.ProviderDocker, listErr: errors.New("docker daemon is not running")}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err == nil {
		t.Fatal("an unreadable provider was treated as an empty one")
	}
}

// ============================================================================
// Sweep: the steady-state pass, which is what makes a failed cleanup temporary.
// ============================================================================

func TestSweepKeepsAnInstanceWhoseLeaseIsLaunching(t *testing.T) {
	t.Parallel()

	// The one that matters most: a sweep that destroys live work is far worse
	// than one that misses an orphan, and this runs on a timer beside every
	// running job.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("the sweep destroyed a job that is starting normally: %v", p.live)
	}
}

func TestSweepDestroysAnInstanceWhoseLeaseWasReaped(t *testing.T) {
	t.Parallel()

	// The case the sweep exists for. The reaper terminalizes the lease of a
	// holder that stopped heartbeating; the container it was running under is an
	// orphan from that moment, and nothing else will ever look at it.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("the orphan survived the sweep: %v", p.live)
	}
}

func TestSweepDestroysAnInstanceWhoseLeaseNeverLaunched(t *testing.T) {
	t.Parallel()

	// A lease in the capacity or assigned phase authorises no compute: the launch
	// path binds and advances to launching BEFORE asking the provider for
	// anything. So an instance carrying such a lease's id is an orphan, and the
	// looser "not terminal" predicate this replaced would have spared it forever.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	lease := assignedLease(t, a)

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, host); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Bound and assigned, but never advanced to launching.
	p.add(&provider.Instance{ID: "ghost", Name: provider.InstanceName(lease.ID)})

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("spared an instance whose lease never reached launching: %v", p.live)
	}
}

func TestSweepDestroysAnInstanceBoundToAnotherNode(t *testing.T) {
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

	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	p.add(&provider.Instance{ID: "stray", Name: provider.InstanceName(lease.ID)})

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("kept an instance whose lease belongs to another host: %v", p.live)
	}
}

func TestRecoverReleasesTheLeaseOfATrueOrphan(t *testing.T) {
	t.Parallel()

	// Capacity has to come back with the container. Leaving the lease for the
	// reaper holds its vCPU and memory for a full TTL after the compute they were
	// paying for is gone — a self-inflicted shortfall on every restart.
	//
	// "True orphan" means the container is not running, so there is no job to
	// protect: an instance that IS running with an open lease is adopted instead,
	// which is the subject of custody_test.go.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	p.stop(provider.InstanceName(lease.ID))

	before, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if before.Leases == 0 {
		t.Fatal("the lease was not open before recovery, so this proves nothing")
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	after, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if after.Leases != 0 || after.VCPU != 0 {
		t.Errorf("capacity stayed held after the compute was destroyed: %d leases, %d vCPU",
			after.Leases, after.VCPU)
	}
}

// A failed registration drops the cached scale-set id.
//
// The id is resolved once per tier and reused. If the scale set was deleted and
// recreated — a teardown, then another control plane — every later launch would
// keep targeting an id that no longer exists, and the only symptom is a tier
// that silently stops working. Clearing it makes the NEXT job re-resolve.
//
// The failed job is NOT retried here: registration has an ambiguous-success case
// of its own, and a blind retry is how one job becomes two runners.
func TestAFailedRegistrationDropsTheCachedScaleSet(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	jit := &fakeJIT{setID: 7}
	r := New(a, host, jit, p, []config.Tier{dockerTier()}, nil)

	// A first launch resolves and caches the id.
	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 1, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 1 {
		t.Fatalf("resolved the scale set %d times for the first launch", jit.describes)
	}

	// A second one uses the cache rather than asking again.
	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 2, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 1 {
		t.Errorf("re-resolved a cached scale set; %d calls", jit.describes)
	}

	// Now registration fails.
	jit.jitErr = errors.New("scale set not found")

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 3, Event: "push"}); err == nil {
		t.Fatal("a launch whose registration failed reported success")
	}

	jit.jitErr = nil

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 4, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 2 {
		t.Errorf("kept using a scale-set id that had just failed; %d resolutions", jit.describes)
	}
}
