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
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// A registered host in these tests is deliberately larger than any budget they
// set, so the deployment-wide ceiling stays the binding constraint and every
// test written before nodes carried capacity keeps measuring what it did.
const (
	testNodeVCPU   = 1 << 20
	testNodeMemory = 1 << 20 * config.GiB
)

// A launch mints a registration and hands it to the provider, with the tier's
// shape and the job's trust class.
func TestLaunchMintsARegistrationAndStartsIt(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, RunID: 101, Event: "push"}); err != nil {
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

	r := New(a, host, jit, p, nil)

	for range 2 {
		lease := assignedLease(t, a)

		if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
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

	// The LEASE is reserved for firecracker; the host runs docker. Setting this
	// up the other way round — a firecracker catalogue over a docker-reserved
	// lease — tested nothing, because the runner reads the lease.
	firecracker := dockerTier()
	firecracker.Provider = config.ProviderFirecracker

	a, host := newAllocatorForTiers(t, openState(t), firecracker)
	registerElsewhere(t, a, config.ProviderFirecracker)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("ran a firecracker lease's job on a docker host")
	}

	if len(p.launched) != 0 {
		t.Error("something was started despite the refusal")
	}
}

// Destroy removes what was started, and is idempotent about what was not.
func TestDestroyRemovesWhatWasStarted(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
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

func TestASpotWarningFailsTheRightLeaseWithItsReason(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	notice := &provider.InterruptionNotice{
		InstanceID: "instance-" + provider.InstanceName(lease.ID),
		Action:     "terminate",
	}
	if err := r.handleInterruption(t.Context(), notice); err != nil {
		t.Fatalf("handleInterruption: %v", err)
	}

	if got, err := a.HistoryOutcome(t.Context(), lease.ID); err != nil {
		t.Fatalf("HistoryOutcome: %v", err)
	} else if got != string(alloc.PhaseFailed) {
		t.Errorf("outcome = %q, want failed", got)
	}
	if got, err := a.HistoryFailureReason(t.Context(), lease.ID); err != nil {
		t.Fatalf("HistoryFailureReason: %v", err)
	} else if got != "ec2 spot interruption: terminate" {
		t.Errorf("failure reason = %q", got)
	}
	if len(p.destroyed) != 1 {
		t.Errorf("destroyed %d instances, want the warned one", len(p.destroyed))
	}
}

func TestAnInterruptionMessageIsAcknowledgedAfterTheFailureIsDurable(t *testing.T) {
	p := &interruptingProvider{
		fakeProvider: &fakeProvider{kind: config.ProviderDocker},
		notices:      make(chan *provider.InterruptionNotice, 1),
		acked:        make(chan *provider.InterruptionNotice, 1),
	}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.WatchInterruptions(ctx)
	}()
	p.notices <- &provider.InterruptionNotice{
		InstanceID: "instance-" + provider.InstanceName(lease.ID),
		Action:     "terminate",
		Receipt:    "secret-receipt",
	}

	select {
	case <-p.acked:
	case <-time.After(time.Second):
		t.Fatal("the handled warning was not acknowledged")
	}
	if reason, err := a.HistoryFailureReason(t.Context(), lease.ID); err != nil {
		t.Fatalf("the message was acknowledged before its history was durable: %v", err)
	} else if reason == "" {
		t.Fatal("the message was acknowledged with no durable failure reason")
	}

	cancel()
	<-done
}

func TestASharedQueueWarningIsNotAcknowledgedByTheWrongNode(t *testing.T) {
	p := &interruptingProvider{
		fakeProvider: &fakeProvider{kind: config.ProviderDocker},
		notices:      make(chan *provider.InterruptionNotice, 1),
		acked:        make(chan *provider.InterruptionNotice, 1),
	}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.WatchInterruptions(ctx)
	}()
	p.notices <- &provider.InterruptionNotice{InstanceID: "instance-owned-by-sibling", Action: "terminate"}

	select {
	case <-p.acked:
		t.Fatal("a node acknowledged its sibling's interruption warning")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	<-done
}

// #46. A TEARDOWN THAT WAS MERELY ACCEPTED DOES NOT RELEASE THE CAPACITY.
//
// The listener releases a lease when Destroy returns nil, and its comment says
// why the order is destroy-then-release: freeing first would hand the capacity to
// another tier while this job's guest is still running on the host. That
// reasoning is sound and it silently assumed a synchronous backend. EC2's
// terminate returns on ACCEPTANCE, so "Destroy returned nil" stopped meaning
// "the guest has stopped" the day that backend landed.
//
// What makes it worse than an over-commit is the paths Destroy is reached on — a
// drain, a custody teardown, an operator killing a job — where the guest is still
// working. A new job for the same repository could then start while the old one
// was still finishing a deploy or applying a migration. Two concurrent effects on
// something outside billet, bounded by nothing.
//
// So the node answers ErrCustody, which already means exactly this: I am holding
// this lease's capacity and will release it when the compute is provably gone.
func TestAnAcceptedTeardownHoldsTheCapacityUntilTheComputeIsProvablyGone(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	err := r.Destroy(t.Context(), 11)
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy answered %v; a teardown the backend only ACCEPTED must answer "+
			"ErrCustody, or the listener releases the lease and another job starts while "+
			"this guest is still running", err)
	}

	// THE TERMINATE WAS STILL ISSUED. Holding the capacity is not a substitute for
	// asking the backend to stop; a version of this that took custody without
	// destroying would hold the lease forever against a guest nobody had told to
	// go.
	if len(p.destroyed) != 1 {
		t.Fatalf("issued %d terminate requests, want 1", len(p.destroyed))
	}

	// AND THE CAPACITY IS STILL CHARGED. This is the assertion the whole issue is
	// about: the lease must not be terminal while the guest may still be working.
	held, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease after an accepted teardown: %v", err)
	}

	if held.Phase.Terminal() {
		t.Fatalf("the lease went terminal at phase %s while the guest was still running; "+
			"its capacity is now available to another tier", held.Phase)
	}

	// A tend while the guest is still shutting down changes nothing: the instance
	// is still there, so there is still nothing to prove.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend while the guest was still shutting down: %v", err)
	}

	switch held, err = a.Lease(t.Context(), lease.ID); {
	case errors.Is(err, alloc.ErrLeaseNotFound), err == nil && held.Phase.Terminal():
		t.Fatal("a tend released the capacity while the backend was still reporting the " +
			"instance; the guest is shutting down and another tier can now take its slot")
	case err != nil:
		t.Fatalf("read the lease after a tend: %v", err)
	}

	// The guest finally stops. NOW the capacity comes back, and only now.
	p.settle(provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the guest stopped: %v", err)
	}

	switch held, err = a.Lease(t.Context(), lease.ID); {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		// Terminalized and gone from the open set, which is the outcome wanted.
	case err != nil:
		t.Fatalf("read the lease after the guest stopped: %v", err)
	case !held.Phase.Terminal():
		t.Errorf("the guest is provably gone and the lease is still at %s; its capacity is "+
			"held for compute that no longer exists", held.Phase)
	}
}

// A host backend whose inventory has observed the compute releases on its first
// later absence instead of waiting through a remote consistency grace.
func TestAFastAcceptedTeardownReleasesOnTheFirstAbsentInventory(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 11); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}

	// This sighting is the causal evidence. A later absent inventory cannot be a
	// create that has not materialised yet.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend while the backend still reports the instance: %v", err)
	}
	p.settle(provider.InstanceName(lease.ID))
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the observed instance stopped: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the first absence after a sighting kept the lease: %v", err)
	}
}

// Ordinary teardown starts from a successful host Launch, which is causal even
// if the backend reports teardown asynchronously. If the compute disappears
// before the first inventory pass, custody must not apply the remote grace.
func TestAHostTeardownKeepsItsLaunchObservation(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 15, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 15); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}
	p.settle(provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the host compute disappeared: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("ordinary host teardown lost its causal launch evidence: %v", err)
	}
}

// An accepted remote teardown and its first absent inventory are not proof.
//
// EC2 can return a stale empty DescribeInstances result after TerminateInstances
// succeeds. The instance may then reappear still shutting down, so the lease must
// survive until the instance is seen or absence is sustained for the appropriate
// consistency window.
func TestAnUnobservedTeardownKeepsCapacityAcrossTheFirstAbsentInventory(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderEC2, asyncTeardown: true}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 12, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 12); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}

	name := provider.InstanceName(lease.ID)
	inst := p.live[name]
	delete(p.live, name)

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the eventually-consistent miss: %v", err)
	}
	if held, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("the first miss released capacity for an instance that can still appear: %v", err)
	} else if held.Phase.Terminal() {
		t.Fatalf("the first miss terminalized an unobserved teardown as %s", held.Phase)
	}

	p.add(inst)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the instance materialised: %v", err)
	}
	p.terminate(name)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after EC2 reported the instance terminated: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("explicit terminal proof left the instance's capacity held: %v", err)
	}
}

// A remote inventory can move from present to absent and back while EC2's
// eventually consistent replicas converge. One missing response after a sighting
// is therefore not causal proof that the machine stopped.
func TestARemoteSightingNeedsSustainedAbsence(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderEC2, asyncTeardown: true}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)
	now := time.Now()
	r.now = func() time.Time { return now }

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 13, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 13); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend while visible: %v", err)
	}

	name := provider.InstanceName(lease.ID)
	inst := p.live[name]
	delete(p.live, name)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on the first miss: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("one remote miss released capacity: %v", err)
	}

	p.add(inst)
	now = now.Add(time.Minute)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after inventory returned: %v", err)
	}
	p.settle(name)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on the new first miss: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("a sighting did not reset the absence interval: %v", err)
	}

	now = now.Add(time.Minute)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on a second stale miss: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("two short-window remote misses released capacity: %v", err)
	}

	p.add(inst)
	now = now.Add(time.Minute)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the instance reappeared following two misses: %v", err)
	}
	p.settle(name)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on the first miss after the second reappearance: %v", err)
	}

	now = now.Add(strayGrace)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the full consistency window: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the full consistency window left capacity held: %v", err)
	}
}

// An error breaks an absence sequence. Time before or during that ambiguous
// result cannot age a later miss toward release.
func TestErrorsBeforeAFirstRemoteMissDoNotAgeTheAbsence(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderEC2, asyncTeardown: true}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)
	now := time.Now()
	r.now = func() time.Time { return now }

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 14, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 14); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}
	p.settle(provider.InstanceName(lease.ID))
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on the first miss: %v", err)
	}
	p.findErr = errors.New("DescribeInstances unavailable")
	now = now.Add(2 * strayGrace)
	if err := r.Tend(t.Context()); err == nil {
		t.Fatal("Tend hid the provider error")
	}

	p.findErr = nil
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on the first successful miss: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("an error failed to reset the prior absence interval: %v", err)
	}
	now = now.Add(strayGrace)
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the new uninterrupted absence interval: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the new full absence interval left capacity held: %v", err)
	}
}

// destroyStray's first successful miss starts the grace immediately. Dropping
// that observation adds a whole poll interval before the safety window even
// begins.
func TestAnAmbiguousLaunchCarriesItsFirstAbsentObservationIntoCustody(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderEC2, launchErr: errors.New("lost launch response")}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)
	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 16, Event: "push"}); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Launch = %v, want ErrCustody", err)
	}
	held := r.custodySnapshot()
	if len(held) != 1 {
		t.Fatalf("Launch left %d custody entries, want 1", len(held))
	}
	if !held[0].absentSince.Equal(frozen) {
		t.Fatalf("first successful miss was recorded at %v, want %v", held[0].absentSince, frozen)
	}
}

// A positive Find in destroyStray is provider inventory evidence even when its
// subsequent teardown is unconfirmed. Custody must carry that sighting rather
// than reverting to the never-observed state.
func TestAnAmbiguousLaunchCarriesItsPositiveObservationIntoCustody(t *testing.T) {
	p := &fakeProvider{
		kind: config.ProviderEC2, launchErr: errors.New("lost launch response"),
		startsAnyway: true, asyncTeardown: true,
	}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 17, Event: "push"}); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Launch = %v, want ErrCustody", err)
	}
	held := r.custodySnapshot()
	if len(held) != 1 {
		t.Fatalf("Launch left %d custody entries, want 1", len(held))
	}
	if !held[0].observed {
		t.Fatal("destroyStray discarded its positive provider observation")
	}
}

// A held teardown is visible in the ledger, and a force release is delivered to
// the live node through its heartbeat. The node drops custody before the lease
// becomes terminal; it does not need a working provider read after an operator
// supplies the missing proof.
func TestAnOperatorCanForceAVisibleTeardownThroughItsLiveNode(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 81, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), 81); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	held, err := a.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(held) != 1 || held[0].ID != lease.ID || held[0].Node != host ||
		held[0].State != alloc.PhaseTeardown || held[0].Since == "" {
		t.Fatalf("visible held leases = %+v, want this teardown with node and time", held)
	}

	result, err := a.ForceRelease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}
	if !result.Pending || result.Node != host {
		t.Fatalf("ForceRelease = %+v, want a request addressed to the live holder", result)
	}

	// The operator has independently established that the guest is gone. Make the
	// provider unreadable to prove this path acts on that assertion rather than
	// accidentally requiring the same observation that was stuck.
	delete(p.live, provider.InstanceName(lease.ID))
	p.findErr = errors.New("credentials are broken")

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after force request: %v", err)
	}
	if len(r.heldLeases()) != 0 {
		t.Fatal("the node kept its local custody record after accepting the force request")
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("forced lease is still open: %v", err)
	}
}

func TestRecoveryConsumesAForceReleaseRequestedBeforeRestart(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}
	a, host := newAllocatorWithHost(t)
	first := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)
	if err := first.Launch(t.Context(), lease, dockerSpec(), Job{
		RequestID: 81, Event: "push",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := first.Destroy(t.Context(), 81); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}
	if err := first.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if _, err := a.ForceRelease(t.Context(), lease.ID); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(restarted.heldLeases()) != 0 {
		t.Fatal("recovery retained custody after consuming the pending force release")
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("forced lease is still open after recovery: %v", err)
	}
}

// AND IT KEEPS SAYING SO. The SECOND destroy for the same request must answer
// ErrCustody too, not success.
//
// Found by review. The first destroy moves the request out of `running` and into
// custody, so a later one — a redelivered completion, a cleanup retry, a
// shutdown, or a broadcast leg whose sibling failed — found `running` empty, read
// that as "nothing was ever started here", and returned nil. The caller released
// the lease on it, which is the exact release this whole change exists to
// prevent, reached one call later and by a path no test covered.
func TestASecondDestroyForHeldComputeStillReportsCustody(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 11); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("the first Destroy answered %v, want ErrCustody", err)
	}

	// The guest is STILL shutting down, so nothing has changed about what is
	// known — and the answer must not change either.
	err := r.Destroy(t.Context(), 11)
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("a second Destroy answered %v while the guest was still shutting down; the "+
			"caller reads that as proof and releases the capacity", err)
	}

	held, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease: %v", err)
	}

	if held.Phase.Terminal() {
		t.Errorf("the lease went terminal at %s after a repeated destroy", held.Phase)
	}

	// AND ONCE THE GUEST IS GONE THE ANSWER FLIPS, or the request could never be
	// finished and the capacity would be held for good.
	p.settle(provider.InstanceName(lease.ID))

	if err := r.Destroy(t.Context(), 11); err != nil {
		t.Errorf("a destroy after the guest was provably gone still reported custody: %v", err)
	}
}

// A REQUEST WITH BOTH A CUSTODY ENTRY AND A RUNNING INSTANCE DESTROYS THE
// RUNNING ONE.
//
// Found by review. Checking custody BEFORE looking at `running` skipped the
// running instance entirely — and the listener reads ErrCustody as "the node
// holds MY lease" and drops its record, so the custody entry (which may hold a
// DIFFERENT lease) keeps its own capacity while the running instance is left
// alive with no owner and nothing that will come back for it.
//
// A redelivered assignment after a crash produces exactly this shape, which is
// the same one TestACustodyFailureDoesNotStrandTheListenersLease covers from the
// other side.
func TestARequestWithBothCustodyAndARunningInstanceStillDestroysTheRunningOne(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	// The custody half: a lease held here whose compute is not confirmed gone.
	adopted := assignedLease(t, a)
	if err := a.Bind(t.Context(), adopted.ID, adopted.Epoch, host); err != nil {
		t.Fatalf("Bind adopted lease: %v", err)
	}
	if err := a.Advance(t.Context(), adopted.ID, adopted.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance adopted lease: %v", err)
	}
	r.holdWithOutcome(adopted, provider.InstanceName(adopted.ID), 11, alloc.PhaseDone)

	// The running half: a different lease, for the same request.
	second := assignedLease(t, a)
	running := &provider.Instance{
		ID: "running-" + second.ID, Name: provider.InstanceName(second.ID), Running: true,
	}
	p.add(running)

	r.mu.Lock()
	r.running[11] = running
	r.runningLease[11] = second
	r.mu.Unlock()

	// The ANSWER is not what this asserts — on an asynchronous backend the running
	// half ends in custody too, so ErrCustody is the correct return either way.
	// What separates the fix from the bug is whether the running instance was
	// asked to stop before that answer was given.
	if err := r.Destroy(t.Context(), 11); err != nil && !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy: %v", err)
	}

	if len(p.destroyed) == 0 {
		t.Fatal("the running instance was never asked to stop; custody for a DIFFERENT lease " +
			"answered for it, and the listener drops its record on that answer — so this " +
			"container is left running with no owner")
	}

	if p.destroyed[len(p.destroyed)-1] != running.ID {
		t.Errorf("destroyed %v, want the running instance %s", p.destroyed, running.ID)
	}
}

// AN ACCEPTED TEARDOWN WITH NO LEASE TO HOLD IT KEEPS THE INSTANCE, so the retry
// asks again.
//
// Found by review. The first call returned an error correctly, and then the
// SECOND arrived to maps that had already been cleared, took the idempotent
// branch, and reported success without reissuing the terminate or proving
// anything — the same release hole, one call later.
func TestATeardownWithNoLeaseToHoldIsRetriedRatherThanForgotten(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The state this branch exists for: an instance with no lease recorded beside
	// it, so custody — which is keyed on a lease — has nothing to hold.
	r.mu.Lock()
	delete(r.runningLease, 11)
	r.mu.Unlock()

	if err := r.Destroy(t.Context(), 11); err == nil {
		t.Fatal("an unconfirmed teardown with no lease to hold the capacity reported success")
	}

	before := len(p.destroyed)

	if err := r.Destroy(t.Context(), 11); err == nil {
		t.Error("the retry reported success without reissuing the terminate or proving the " +
			"guest was gone; the caller releases the capacity on that")
	}

	if len(p.destroyed) == before {
		t.Error("the retry did not ask the backend to stop the guest again, so nothing was " +
			"holding the obligation and nothing was acting on it either")
	}
}

// AND IT IS RECORDED AS A JOB THAT FINISHED, not as one that failed.
//
// custody's other discard path is a launch that never started, which is a
// failure — and reusing it wholesale would write "failed" into the durable
// history for every job that completed normally on an EC2 host. An investigation
// reading job_history would see a fleet where nothing ever succeeded.
func TestAnAcceptedTeardownRecordsTheJobAsDone(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker, asyncTeardown: true}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 11); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy: %v", err)
	}

	// A TEND WHILE IT IS STILL SHUTTING DOWN COMES FIRST, because that is the real
	// sequence and because it is load-bearing: an entry whose instance has never
	// been seen holds its absence for strayGrace before believing it (#48), so
	// settling before any observation would test the grace rather than the
	// outcome.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend while the guest was shutting down: %v", err)
	}

	p.settle(provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the guest stopped: %v", err)
	}

	outcome, err := a.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("nothing was archived for a lease whose compute is gone: %v", err)
	}

	if outcome != string(alloc.PhaseDone) {
		t.Errorf("a job that ran to completion on an asynchronous backend was archived as %q, "+
			"want %q", outcome, alloc.PhaseDone)
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

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
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

// dockerSpec is what the control plane sends with a launch for dockerTier.
func dockerSpec() *nodeapi.TierSpec {
	return nodeapi.TierSpecOf(dockerTier())
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

	// asyncTeardown models EC2 rather than docker: Destroy is ACCEPTED and the
	// instance keeps existing until settle is called. This is the shape #46 is
	// about, and no other fake in this suite has it — every one of them destroys
	// synchronously, which is why the gap survived so long.
	asyncTeardown bool

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

	// enteredFind receives once per Find that has begun waiting, so a test can
	// synchronise on the provider actually being busy instead of on a sleep.
	enteredFind chan struct{}

	// launchDelay holds Launch open, which is the situation a slow image pull
	// produces and the one where nobody was renewing the lease.
	launchDelay time.Duration

	// enteredLaunch receives once Launch has begun waiting, so a test can act
	// while the provider is genuinely mid-launch rather than sleeping and hoping.
	enteredLaunch chan struct{}
}

type interruptingProvider struct {
	*fakeProvider
	notices chan *provider.InterruptionNotice
	acked   chan *provider.InterruptionNotice
}

func (p *interruptingProvider) NextInterruption(ctx context.Context) (*provider.InterruptionNotice, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case notice := <-p.notices:
		return notice, nil
	}
}

func (p *interruptingProvider) AcknowledgeInterruption(
	ctx context.Context, notice *provider.InterruptionNotice,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.acked <- notice:
		return nil
	}
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

func (f *fakeProvider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	f.launched = append(f.launched, spec)

	if f.cancelOnLaunch != nil {
		f.cancelOnLaunch()
	}

	if f.launchDelay > 0 {
		if f.enteredLaunch != nil {
			select {
			case f.enteredLaunch <- struct{}{}:
			default:
			}
		}

		select {
		case <-time.After(f.launchDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
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
	// Announced BEFORE the wait, so a test can synchronise on the provider being
	// genuinely inside Find rather than sleeping and hoping the scheduler
	// obliged.
	if f.enteredFind != nil {
		select {
		case f.enteredFind <- struct{}{}:
		default:
		}
	}

	// CANCEL-AWARE. An unconditional Sleep left a goroutine parked for the full
	// delay after the test ended — an hour, in the test that models a wedged
	// daemon. Waiting on the context too means the delay ends when the caller
	// does.
	if f.findDelay > 0 {
		select {
		case <-time.After(f.findDelay):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
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

func (f *fakeProvider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	if err := ctx.Err(); err != nil {
		return provider.TeardownRequested, err
	}

	if f.destroyErr != nil {
		return provider.TeardownRequested, f.destroyErr
	}

	f.destroyed = append(f.destroyed, id)

	// AN ASYNCHRONOUS BACKEND LEAVES THE INSTANCE EXACTLY WHERE IT WAS, which is
	// the whole point of modelling one: EC2's TerminateInstances returns on
	// acceptance and the guest keeps running through `shutting-down` — a state
	// List reports as running on purpose. A fake that deleted the instance here
	// and merely returned a different enum would agree with the bug, because
	// every later observation would say "gone" no matter what the code believed.
	if f.asyncTeardown {
		return provider.TeardownRequested, nil
	}

	for name, inst := range f.live {
		if inst.ID == id {
			delete(f.live, name)
		}
	}

	return provider.TeardownStopped, nil
}

// settle is the guest finally stopping, some time after the terminate request
// was accepted. Named rather than by id, because that is what a test holds: the
// name is derived from the lease and the id is the backend's own.
//
// Only meaningful with asyncTeardown.
func (f *fakeProvider) settle(name string) { delete(f.live, name) }

// terminate leaves the provider's causal terminal record visible, as EC2 does
// for targeted DescribeInstances calls after billing and execution have ended.
func (f *fakeProvider) terminate(name string) {
	if inst, ok := f.live[name]; ok {
		inst.Running = false
		inst.Terminal = true
	}
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

	return newAllocatorForTiers(t, db, dockerTier())
}

// newAllocatorWithEC2Host gives tests of remote-provider consistency a lease
// that can genuinely bind to an EC2 node. A Docker lease cannot pass the same
// provider check production relies on, and weakening that check for a test would
// make its setup contradict the behavior it claims to exercise.
func newAllocatorWithEC2Host(t *testing.T) (*alloc.Allocator, string) {
	t.Helper()

	db := openState(t)
	tier := dockerTier()
	tier.Provider = config.ProviderEC2
	tier.Image = "ami-test"

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "test-ec2"
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderEC2,
		VCPU: 8, Memory: 32 * config.GiB,
		EC2Shapes: []config.EC2InstanceType{{
			Type: "test.large", VCPU: 2, Memory: 8 * config.GiB,
		}},
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a, host
}

// newAllocatorForTiers builds an allocator over a chosen catalogue.
//
// The catalogue matters because the LEASE is the authority on which backends it
// may run on: the ledger snapshots that list at reserve time so a config edit
// under an in-flight lease cannot reclassify it. A test that wants the runner to
// refuse a host has to arrange the mismatch HERE — handing the runner a
// different live catalogue proves nothing, because the runner does not read it.
func newAllocatorForTiers(t *testing.T, db *state.DB, tiers ...config.Tier) (*alloc.Allocator, string) {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "test-host"

	// THE HOST UNDER TEST RUNS DOCKER, and it is the one returned. Its name is
	// what a test hands the runner, so this is the machine whose refusals the
	// tests are about.
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderDocker,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return a, host
}

// registerElsewhere gives the fleet a host the TIER accepts but the runner under
// test is not.
//
// Escrow chooses a machine now, so a tier no registered host can serve cannot be
// RESERVED at all — which is correct, and which makes "the runner refuses a host
// outside its lease's list" untestable unless the lease can be placed somewhere
// else first. The mismatch has to be at bind, not at reserve.
func registerElsewhere(t *testing.T, a *alloc.Allocator, kind config.ProviderKind) {
	t.Helper()

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "elsewhere", Provider: kind,
		VCPU: testNodeVCPU, Memory: testNodeMemory,
	}); err != nil {
		t.Fatalf("RegisterNode elsewhere: %v", err)
	}
}

// openState opens a throwaway ledger.
func openState(t *testing.T) *state.DB {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// nextRequest is atomic because these tests run in parallel: separate databases
// make duplicate request ids harmless in practice, which is exactly why such a
// race would be reported only by -race on an unlucky run.
var nextRequest atomic.Int64

// assignedLease reserves and assigns a lease, which is the state Launch expects
// to receive one in.
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(ctx, lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
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
	r := New(a, host, &fakeJIT{setID: 7, name: "github-picked-this"}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("the sweep destroyed a job that is starting normally: %v", p.live)
	}
}

func TestSweepDestroysAnInstanceWhoseLeaseIsTerminal(t *testing.T) {
	t.Parallel()

	// The case the sweep exists for: a lease that has finished while its
	// container did not. It is named for what it stages — a terminal lease — and
	// not for the reaper, which no longer terminalizes a lease with compute
	// behind it. That one is quarantined and adopted; see
	// TestQuarantinedComputeIsAdoptedRatherThanDestroyed.
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
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

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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

	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{Name: "other-host", Provider: config.ProviderDocker, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
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

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
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

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, jit, p, nil)

	// A first launch resolves and caches the id.
	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 1, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 1 {
		t.Fatalf("resolved the scale set %d times for the first launch", jit.describes)
	}

	// A second one uses the cache rather than asking again.
	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 2, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 1 {
		t.Errorf("re-resolved a cached scale set; %d calls", jit.describes)
	}

	// Now registration fails.
	jit.jitErr = errors.New("scale set not found")

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 3, Event: "push"}); err == nil {
		t.Fatal("a launch whose registration failed reported success")
	}

	jit.jitErr = nil

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 4, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if jit.describes != 2 {
		t.Errorf("kept using a scale-set id that had just failed; %d resolutions", jit.describes)
	}
}

// A host running the tier's FALLBACK backend runs the job.
//
// The node had its own equality check against the single `provider:` field,
// which is empty whenever `providers:` is used — so every legitimate fallback
// was refused one layer below the allocator that had just permitted it. Two
// checks of the same rule, and only one of them updated, is the shape of bug
// that survives a green test suite.
func TestAHostRunningTheFallbackBackendIsAccepted(t *testing.T) {
	t.Parallel()

	// The tier prefers firecracker; this host runs docker, which is second on the
	// list.
	// The lease is reserved for [firecracker, docker]; this host runs docker,
	// which is second on the list.
	tier := dockerTier()
	tier.Provider = ""
	tier.Providers = []config.ProviderKind{config.ProviderFirecracker, config.ProviderDocker}

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorForTiers(t, openState(t), tier)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("a host running the lease's fallback backend refused the job: %v", err)
	}

	if len(p.launched) != 1 {
		t.Fatal("nothing was started on the fallback host")
	}
}

// A host running a backend the tier never named is still refused.
func TestAHostOutsideTheTiersListIsRefused(t *testing.T) {
	t.Parallel()

	tier := dockerTier()
	tier.Provider = ""
	tier.Providers = []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorForTiers(t, openState(t), tier)
	registerElsewhere(t, a, config.ProviderFirecracker)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("ran a job on a backend its lease never named")
	}

	if len(p.launched) != 0 {
		t.Fatal("started something before refusing")
	}
}

// THE LEASE OUTRANKS THE CATALOGUE.
//
// The ledger snapshots a tier's acceptable backends at reserve time so an
// operator editing the config cannot reclassify work that is already in flight.
// The runner used to check the LIVE catalogue instead, which contradicted that
// from both directions: remove a provider and it refused a lease the ledger
// still permitted, so the listener released it and GitHub had to reassign the
// job; add one and it waved through a lease Bind would refuse.
//
// Two authorities for one fact is the bug. This pins which one wins.
func TestTheLeaseOutranksALaterCatalogueEdit(t *testing.T) {
	t.Parallel()

	// Reserved when the tier still accepted docker.
	reserved := dockerTier()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorForTiers(t, openState(t), reserved)
	lease := assignedLease(t, a)

	// The operator then edits the config to drop docker and restarts. The runner
	// gets the NEW catalogue; the lease still carries the old list.
	edited := dockerTier()
	edited.Provider = ""
	edited.Providers = []config.ProviderKind{config.ProviderFirecracker}

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("a catalogue edit reclassified a lease that was already placed, so the "+
			"listener will release it and GitHub must reassign the job: %v", err)
	}

	if len(p.launched) != 1 {
		t.Error("the job did not start")
	}
}

// THE SIZE COMES FROM THE LEASE, not the live catalogue.
//
// The ledger escrowed a lease's vCPU and memory when it was reserved and is
// still accounting for those numbers. Reading the tier meant a label edited from
// 2 vCPU to 16 started a 16-vCPU guest against 2 vCPU of reservation — and
// enough of those physically over-commit the machine while every ledger total
// still balances, which is the worst shape a capacity bug can take.
//
// The same two-authorities defect as the provider list, one field over.
func TestLaunchSizesTheGuestFromTheLeaseNotTheCatalogue(t *testing.T) {
	t.Parallel()

	// Reserved small.
	small := dockerTier()
	small.VCPU = 2
	small.Memory = 4 * config.GiB

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorForTiers(t, openState(t), small)
	lease := assignedLease(t, a)

	// The operator then edits the label to be much bigger and restarts.
	big := small
	big.VCPU = 16
	big.Memory = 64 * config.GiB

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(p.launched) != 1 {
		t.Fatal("nothing was started")
	}

	spec := p.launched[0]

	if spec.VCPU != 2 {
		t.Errorf("started a %d vCPU guest against a 2 vCPU reservation; the host is now "+
			"over-committed and the ledger still balances", spec.VCPU)
	}

	if spec.Memory != 4*config.GiB {
		t.Errorf("started a %s guest against a 4GiB reservation", spec.Memory)
	}
}

// A JOB WHOSE LISTENER DIED IS NOT AN ORPHAN.
//
// The reaper quarantines a lease whose holder stopped heartbeating, and the
// whole reason its capacity stays charged is that a container may still be
// running for it. But the set the node reads to tell "something is waiting for
// this" is the LAUNCHED set, which does not include quarantine — so a live job
// looked like an orphan to both recovery and the sweep, and was destroyed.
//
// GitHub does not requeue a job a runner has already started, so that is a
// failed build for a machine that was working correctly.
func TestQuarantinedComputeIsAdoptedRatherThanDestroyed(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// THE REAL REAPER, on a lease that has genuinely aged out. Staging the phase
	// by hand would prove nothing about the path this is protecting.
	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("the sweep destroyed a job whose listener had died; GitHub does not requeue "+
			"one a runner has already started, so that build is lost: %v", p.live)
	}

	// AND IT IS HELD, not merely spared: custody renews the lease and releases it
	// when the container exits. Sparing it without adopting it would leave the
	// capacity charged with nothing left to resolve it.
	if got := len(r.heldLeases()); got != 1 {
		t.Errorf("%d instances are in custody; a spared container nothing is holding is a "+
			"lease that can never be resolved", got)
	}
}

// AND RECOVERY DOES NOT DESTROY IT EITHER, which is the same defect reached by
// the other path: a node that restarts while the job is still running.
func TestRecoveryAdoptsQuarantinedCompute(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	// A FRESH RUNNER over the same provider: the process restarted, and its maps
	// are empty.
	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("recovery destroyed a running job whose lease was quarantined: %v", p.live)
	}

	if got := len(restarted.heldLeases()); got != 1 {
		t.Errorf("recovery spared %d instances without taking custody of them", got)
	}
}

// COMPUTE ALREADY IN CUSTODY SURVIVES ITS LEASE BEING QUARANTINED.
//
// This is the case the other quarantine tests do not reach, and it is the one
// where the two mechanisms meet. The node took custody of a launch whose result
// was lost — so it holds the lease at some epoch. Heartbeats then fail for
// longer than the TTL while the container keeps running, the reaper quarantines
// the lease and BUMPS ITS EPOCH, and the node's next heartbeat is fenced.
//
// Reading that fence as "the lease is gone" destroyed the job: quarantine is the
// one fence that means the opposite — capacity still charged, nobody else
// holding it, and the container is exactly what the quarantine is protecting.
func TestCustodySurvivesItsLeaseBeingQuarantined(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The launch's result was lost, so the node holds it at the CURRENT epoch.
	live, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if err := r.AssumeCustody(t.Context(), live, 11); err != nil {
		t.Fatalf("AssumeCustody: %v", err)
	}

	// Nothing heartbeats for a TTL, and the reaper quarantines it — bumping the
	// epoch out from under the custody entry.
	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("custody destroyed a running job because its lease had been quarantined "+
			"under it; that is the one fence which means the job must be KEPT: %v", p.live)
	}

	// AND IT IS STILL HELD, at the new epoch — so the next tick renews rather
	// than being fenced again forever.
	if got := len(r.heldLeases()); got != 1 {
		t.Fatalf("the entry was dropped from custody, so nothing renews the lease it is "+
			"holding: %d", got)
	}

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("second Tend: %v", err)
	}

	if len(p.live) != 1 {
		t.Errorf("the job survived one tick and not the next: %v", p.live)
	}
}

// A successful Launch result is not the same as a provider inventory sighting.
//
// EC2 can return an instance id before DescribeInstances can see it. If the
// launch report is then lost, the faster custody cadence must not read that first
// stale absence as proof and release capacity under the instance about to appear.
func TestAssumeCustodyKeepsCapacityAcrossAFirstAbsentInventory(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderEC2}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 27, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	live, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := r.AssumeCustody(t.Context(), live, 27); err != nil {
		t.Fatalf("AssumeCustody: %v", err)
	}

	name := provider.InstanceName(lease.ID)
	inst := p.live[name]
	delete(p.live, name)

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the stale absence: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); err != nil {
		t.Fatalf("the first inventory miss released the lost-result launch: %v", err)
	}

	p.add(inst)
}

// A host provider returns from Launch only after its local runtime created the
// compute. That causal result is enough to trust its first later absence; the
// remote consistency grace must not stall a local handoff or drain.
func TestAssumeCustodyTrustsAHostLaunchBeforeItsFirstAbsence(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 29, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	live, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := r.AssumeCustody(t.Context(), live, 29); err != nil {
		t.Fatalf("AssumeCustody: %v", err)
	}
	p.settle(provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the host compute disappeared: %v", err)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("the host launch waited through a remote consistency grace: %v", err)
	}
}

// A HOST RUNNING NOTHING SAYS SO, EVERY SWEEP.
//
// Quarantine happens on the REAPER's clock, not the node's. A control plane that
// restarts sees its nodes re-register within seconds — before the leases they
// were holding have expired — so the inventory that arrives with a registration
// is taken BEFORE the quarantine it would resolve, and nothing looked again.
//
// The capacity of a job that finished during the outage was then held until an
// operator ran `billet leases release --force`. Every plane restart permanently
// shrank the fleet by its already-finished in-flight leases, and the early
// return for "this host is running nothing" is what made the case unreachable —
// a host running nothing is exactly the one whose capacity should come back.
func TestASweepReportsAnEmptyInventory(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The job finishes and its container goes, while the control plane is not
	// watching.
	running, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, inst := range running {
		if _, err := p.Destroy(t.Context(), inst.ID); err != nil {
			t.Fatalf("destroy: %v", err)
		}
	}

	// The lease ages out and is quarantined, AFTER any registration this node
	// would have made.
	if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if err := a.AgeQuarantineForTest(t.Context(), lease.ID); err != nil {
		t.Fatalf("age: %v", err)
	}

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	held, err := a.Quarantined(t.Context())
	if err != nil {
		t.Fatalf("Quarantined: %v", err)
	}

	if len(held) != 0 {
		t.Errorf("%d lease(s) still hold capacity for compute this host says it is not "+
			"running; nothing else will ever ask again: %+v", len(held), held)
	}
}
