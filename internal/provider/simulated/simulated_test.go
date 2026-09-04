package simulated

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A registration that must never be retained or rendered.
const secret = "eyJzZXJ2ZXJVcmwiOiJodHRwczovL2V4YW1wbGUiLCJ0b2tlbiI6IlNVUEVSU0VDUkVUIn0="

// clock is a time the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// owner is the deployment every single-deployment test runs as.
const owner = "dep-1"

// newProvider builds a provider over its own host and a clock the test moves.
func newProvider(t *testing.T, opts ...Option) (*Provider, *clock) {
	t.Helper()

	c := newClock()

	p, err := New(owner, append([]Option{WithClock(c.read)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p, c
}

// spec is a launchable spec for one lease.
func spec(lease string, d time.Duration) provider.Spec {
	return provider.Spec{
		Name:      provider.InstanceName(lease),
		Image:     "simulated",
		VCPU:      2,
		Memory:    2 * config.GiB,
		Trust:     provider.TrustTrusted,
		JITConfig: secret,
		Command:   RunFor(d),
	}
}

// Untrusted and UNCLASSIFIED work are refused, by Accepts and by Launch alike.
//
// There is no boundary here at all, so the refusal has to hold even for a caller
// that never asked: a backend that only refuses when asked politely is not a
// boundary. Nothing is recorded for a refused launch.
func TestUntrustedAndUnclassifiedWorkIsRefused(t *testing.T) {
	t.Parallel()

	for name, trust := range map[string]provider.TrustClass{
		"unclassified": provider.TrustUnknown,
		"untrusted":    provider.TrustUntrusted,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, _ := newProvider(t)

			if err := p.Accepts(trust); err == nil {
				t.Fatalf("Accepts admitted %s work to a backend with no boundary", name)
			}

			s := spec("lease-1", time.Minute)
			s.Trust = trust

			_, err := p.Launch(t.Context(), s)
			if err == nil {
				t.Fatalf("Launch modelled %s work", name)
			}

			if !strings.Contains(err.Error(), "no boundary") {
				t.Errorf("the refusal does not say why: %v", err)
			}

			instances, err := p.List(t.Context())
			if err != nil {
				t.Fatalf("List: %v", err)
			}

			if len(instances) != 0 {
				t.Errorf("a refused launch still recorded an instance: %+v", instances)
			}
		})
	}
}

// A spec that cannot be modelled honestly is refused rather than launched.
func TestALaunchWithoutWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mutate func(*provider.Spec)
		want   string
	}{
		"no name":         {func(s *provider.Spec) { s.Name = "" }, "name"},
		"no registration": {func(s *provider.Spec) { s.JITConfig = "" }, "JIT config"},
		"no command":      {func(s *provider.Spec) { s.Command = nil }, "RunFor"},
		"another backend's command": {func(s *provider.Spec) {
			s.Command = []string{"./run.sh"}
		}, "RunFor"},
		"a duration that is not one": {func(s *provider.Spec) {
			s.Command = []string{runnerWord, runForFlag, "soon"}
		}, "not one"},
		"zero duration":     {func(s *provider.Spec) { s.Command = RunFor(0) }, "positive"},
		"negative duration": {func(s *provider.Spec) { s.Command = RunFor(-time.Second) }, "positive"},
		"an extra word": {func(s *provider.Spec) {
			s.Command = append(RunFor(time.Minute), "--now")
		}, "RunFor"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, _ := newProvider(t)

			s := spec("lease-1", time.Minute)
			tc.mutate(&s)

			_, err := p.Launch(t.Context(), s)
			if err == nil {
				t.Fatalf("launched a spec with %s", name)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what is wrong (want %q): %v", tc.want, err)
			}
		})
	}
}

// RunFor and its reader agree, so a tier written with the helper launches.
func TestRunForRoundTrips(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{time.Millisecond, 90 * time.Second, 3 * time.Hour} {
		got, err := durationOf(RunFor(d))
		if err != nil {
			t.Fatalf("durationOf(RunFor(%s)): %v", d, err)
		}

		if got != d {
			t.Errorf("durationOf(RunFor(%s)) = %s", d, got)
		}
	}
}

// A provider without a deployment identity is refused at construction.
func TestAProviderNeedsADeploymentIdentity(t *testing.T) {
	t.Parallel()

	if _, err := New(""); err == nil {
		t.Fatal("New accepted an empty owner; List would then feed a loop that destroys " +
			"every deployment's instances")
	}
}

// An instance is named exactly as the spec says, so reconciliation can recover
// its lease from the name alone, and the backend reports its own kind.
func TestAnInstanceCarriesItsLeaseInItsName(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	if p.Kind() != config.ProviderSimulated {
		t.Errorf("Kind = %s", p.Kind())
	}

	inst, err := p.Launch(t.Context(), spec("lease-7", time.Minute))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if inst.ID == "" {
		t.Error("Launch returned no instance id")
	}

	if lease, ours := provider.LeaseOf(inst.Name); !ours || lease != "lease-7" {
		t.Errorf("instance is named %q, which does not carry lease-7", inst.Name)
	}

	if !inst.Running {
		t.Error("a just-launched instance is not running")
	}
}

// RUNNING UNTIL THE DURATION HAS ELAPSED, STOPPED AND TERMINAL AFTERWARDS, AND
// STILL LISTED UNTIL DESTROYED.
//
// The clock is the only thing that moves: there are no timers, so a modelled
// hour costs nothing and the same replay gives the same answers.
func TestAnInstanceRunsForItsDurationAndThenStops(t *testing.T) {
	t.Parallel()

	p, c := newProvider(t)

	inst, err := p.Launch(t.Context(), spec("lease-1", 10*time.Minute))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	c.advance(10*time.Minute - time.Second)

	got, found, err := p.Find(t.Context(), inst.Name)
	if err != nil || !found {
		t.Fatalf("Find before the duration: found=%v err=%v", found, err)
	}

	if !got.Running || got.Terminal {
		t.Fatalf("one second before its duration elapsed the instance reports %+v", got)
	}

	c.advance(time.Second)

	got, found, err = p.Find(t.Context(), inst.Name)
	if err != nil || !found {
		t.Fatalf("Find after the duration: found=%v err=%v", found, err)
	}

	if got.Running {
		t.Errorf("the instance still reports running after its duration: %+v", got)
	}

	// TERMINAL, because the store is authoritative: nothing can make this
	// instance run again, and that is the fact Terminal exists to carry.
	if !got.Terminal {
		t.Errorf("a stopped instance is not reported terminal: %+v", got)
	}

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 1 || instances[0].Running {
		t.Errorf("a stopped instance vanished from the inventory or kept running: %+v", instances)
	}
}

// Find matches the whole name. `billet-abc` must not find `billet-abcdef`,
// because the caller's next move on a hit is to destroy.
func TestFindMatchesTheWholeName(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	if _, err := p.Launch(t.Context(), spec("abcdef", time.Minute)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, found, err := p.Find(t.Context(), provider.InstanceName("abc")); err != nil || found {
		t.Fatalf("Find(billet-abc) = found %v, err %v; want not found", found, err)
	}

	if _, found, err := p.Find(t.Context(), provider.InstanceName("abcdef")); err != nil || !found {
		t.Fatalf("Find(billet-abcdef) = found %v, err %v; want found", found, err)
	}
}

// AN INVENTORY THAT CANNOT BE READ IS AN ERROR, NEVER A SHORT ANSWER.
//
// Reconciliation frees the capacity of every lease absent from a List, so the
// dangerous failure is a successful empty answer. Under an injected fault List
// and Find return the fault and nothing else; clearing it restores the
// inventory untouched.
func TestAnUnreadableInventoryIsAnErrorNotAShortAnswer(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	if _, err := p.Launch(t.Context(), spec("lease-1", time.Minute)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	fault := errors.New("scripted: the daemon is gone")
	p.FailInventory(fault)

	instances, err := p.List(t.Context())
	if !errors.Is(err, fault) {
		t.Fatalf("List under a fault returned %v instances and error %v; want the fault",
			len(instances), err)
	}

	if len(instances) != 0 {
		t.Errorf("List under a fault returned instances beside its error: %+v", instances)
	}

	inst, found, err := p.Find(t.Context(), provider.InstanceName("lease-1"))
	if !errors.Is(err, fault) || found || inst != nil {
		t.Errorf("Find under a fault = %+v, %v, %v; want the fault alone", inst, found, err)
	}

	p.FailInventory(nil)

	instances, err = p.List(t.Context())
	if err != nil {
		t.Fatalf("List after the fault cleared: %v", err)
	}

	if len(instances) != 1 {
		t.Errorf("the inventory did not survive the fault: %+v", instances)
	}
}

// A context that has ended gets an error, not the inventory.
func TestAnEndedContextGetsNoInventory(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	if _, err := p.Launch(t.Context(), spec("lease-1", time.Minute)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if instances, err := p.List(ctx); !errors.Is(err, context.Canceled) || len(instances) != 0 {
		t.Errorf("List on a cancelled context = %+v, %v; want context.Canceled and nothing",
			instances, err)
	}

	if _, found, err := p.Find(ctx, provider.InstanceName("lease-1")); !errors.Is(err, context.Canceled) || found {
		t.Errorf("Find on a cancelled context = found %v, %v; want context.Canceled", found, err)
	}

	if _, err := p.Launch(ctx, spec("lease-2", time.Minute)); !errors.Is(err, context.Canceled) {
		t.Errorf("Launch on a cancelled context: %v; want context.Canceled", err)
	}

	if state, err := p.Destroy(ctx, "sim-1"); !errors.Is(err, context.Canceled) || state != provider.TeardownRequested {
		t.Errorf("Destroy on a cancelled context = %s, %v; want requested and context.Canceled",
			state, err)
	}
}

// Destroy proves the instance gone, is idempotent, and refuses to speak for an
// id it was not given.
func TestDestroyProvesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	inst, err := p.Launch(t.Context(), spec("lease-1", time.Minute))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	state, err := p.Destroy(t.Context(), inst.ID)
	if err != nil || state != provider.TeardownStopped {
		t.Fatalf("Destroy = %s, %v; want stopped", state, err)
	}

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 0 {
		t.Errorf("a destroyed instance is still listed: %+v", instances)
	}

	// A second destroy is success: teardown runs on paths that have already
	// failed once.
	state, err = p.Destroy(t.Context(), inst.ID)
	if err != nil || state != provider.TeardownStopped {
		t.Errorf("a second Destroy = %s, %v; want stopped", state, err)
	}

	state, err = p.Destroy(t.Context(), "")
	if err == nil || state != provider.TeardownRequested {
		t.Errorf("Destroy of an empty id = %s, %v; want requested and an error", state, err)
	}
}

// A name a crash left behind is refused rather than doubled up.
func TestANameInUseIsRefused(t *testing.T) {
	t.Parallel()

	p, _ := newProvider(t)

	if _, err := p.Launch(t.Context(), spec("lease-1", time.Minute)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	_, err := p.Launch(t.Context(), spec("lease-1", time.Minute))
	if err == nil {
		t.Fatal("two instances were launched under one name; reconciliation would find two " +
			"claims on one lease")
	}

	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the refusal does not say the name is taken: %v", err)
	}

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 1 {
		t.Errorf("the refused launch left the inventory at %d instances", len(instances))
	}
}

// TWO DEPLOYMENTS ON ONE HOST CANNOT SEE OR DESTROY EACH OTHER'S COMPUTE.
//
// The property every backend has to hold, here made real by a shared Host: each
// provider lists only its own, Find does not cross, a destroy by the other's id
// is refused and leaves the instance running, and a launch under a name the
// other holds is refused.
func TestTwoDeploymentsOnOneHostAreKeptApart(t *testing.T) {
	t.Parallel()

	host := NewHost()
	c := newClock()

	one, err := New("dep-1", OnHost(host), WithClock(c.read))
	if err != nil {
		t.Fatalf("New(dep-1): %v", err)
	}

	two, err := New("dep-2", OnHost(host), WithClock(c.read))
	if err != nil {
		t.Fatalf("New(dep-2): %v", err)
	}

	mine, err := one.Launch(t.Context(), spec("lease-1", time.Minute))
	if err != nil {
		t.Fatalf("Launch(dep-1): %v", err)
	}

	theirs, err := two.Launch(t.Context(), spec("lease-2", time.Minute))
	if err != nil {
		t.Fatalf("Launch(dep-2): %v", err)
	}

	instances, err := one.List(t.Context())
	if err != nil {
		t.Fatalf("List(dep-1): %v", err)
	}

	if len(instances) != 1 || instances[0].ID != mine.ID {
		t.Errorf("dep-1 lists %+v; want only its own instance %s", instances, mine.ID)
	}

	if _, found, err := one.Find(t.Context(), theirs.Name); err != nil || found {
		t.Errorf("dep-1 found dep-2's instance: found=%v err=%v", found, err)
	}

	state, err := one.Destroy(t.Context(), theirs.ID)
	if err == nil || state != provider.TeardownRequested {
		t.Errorf("dep-1 destroying dep-2's instance = %s, %v; want requested and a refusal",
			state, err)
	}

	got, found, err := two.Find(t.Context(), theirs.Name)
	if err != nil || !found || !got.Running {
		t.Errorf("dep-2's instance did not survive dep-1's destroy: found=%v running=%v err=%v",
			found, got != nil && got.Running, err)
	}

	if _, err := two.Launch(t.Context(), spec("lease-1", time.Minute)); err == nil {
		t.Error("dep-2 launched under a name dep-1 holds; two claims on one name on one host")
	}
}

// THE REGISTRATION IS RETAINED NOWHERE.
//
// It is a credential until the runner consumes it, and this backend has no
// runner to consume it. After a launch, no rendering of the provider, the host
// or an instance contains it, through any of the verbs a log or a debugger
// would use.
func TestTheRegistrationIsNeverRetained(t *testing.T) {
	t.Parallel()

	host := NewHost()

	p, _ := newProvider(t, OnHost(host))

	inst, err := p.Launch(t.Context(), spec("lease-1", time.Minute))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for name, rendered := range map[string]string{
		"provider %v":  fmt.Sprintf("%v", p),
		"provider %+v": fmt.Sprintf("%+v", p),
		"provider %#v": fmt.Sprintf("%#v", p),
		"host %+v":     fmt.Sprintf("%+v", host),
		"host %#v":     fmt.Sprintf("%#v", host),
		"record %+v":   fmt.Sprintf("%+v", host.instances[inst.Name]),
		"instance %+v": fmt.Sprintf("%+v", inst),
		"list %+v":     fmt.Sprintf("%+v", instances),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("%s renders the registration:\n%s", name, rendered)
		}
	}
}
