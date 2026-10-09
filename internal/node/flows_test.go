package node

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
	"github.com/junioryono/billet/internal/usage/flows"
)

// recordingFlows is a FlowWatcher that records what it was asked, and when.
type recordingFlows struct {
	mu        sync.Mutex
	watched   map[string]string // key -> lease file
	launched  map[string]time.Time
	finals    []string
	finalAt   time.Time
	until     time.Time
	forgotten []string
}

func newRecordingFlows() *recordingFlows {
	return &recordingFlows{watched: map[string]string{}, launched: map[string]time.Time{}}
}

func (f *recordingFlows) Watch(key string, _ net.HardwareAddr, leaseFile string, launched time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.watched[key], f.launched[key] = leaseFile, launched
}

func (f *recordingFlows) Final(_ context.Context, key string, until time.Time) (flows.Result, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.finals = append(f.finals, key)
	f.finalAt, f.until = time.Now(), until

	return flows.Result{}, true, nil
}

func (f *recordingFlows) Forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forgotten = append(f.forgotten, key)
}

// guestProvider is a measured backend whose guests have a MAC on a bridge, and
// which records when it destroyed each instance.
type guestProvider struct {
	*measuredProvider
	mu          sync.Mutex
	launchedAt  time.Time
	destroyedAt time.Time
}

func (p *guestProvider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	p.mu.Lock()
	p.launchedAt = time.Now()
	p.mu.Unlock()

	return p.measuredProvider.Launch(ctx, spec)
}

func (p *guestProvider) UsageTarget(ctx context.Context, id string) (provider.UsageTarget, error) {
	t, err := p.measuredProvider.UsageTarget(ctx, id)
	t.GuestMAC, t.Bridge = "02:00:00:00:00:17", "billet1"

	return t, err
}

func (p *guestProvider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	state, err := p.measuredProvider.Destroy(ctx, id)

	p.mu.Lock()
	p.destroyedAt = time.Now()
	p.mu.Unlock()

	return state, err
}

// THROUGH THE RUNNER'S OWN LAUNCH AND DESTROY: a guest's flows are followed
// from its bridge's lease file from a moment before the launch, and taken
// after its compute is destroyed, bounded by when the destroy began.
func TestTheRunnerFollowsAGuestsFlowsFromLaunchToDestroy(t *testing.T) {
	root := t.TempDir()
	p := &guestProvider{measuredProvider: &measuredProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, root: root}}
	a, host := newAllocatorWithHost(t)
	rec := newRecordingFlows()
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithMonitor(runningMonitor(t, root)), WithFlows(rec, "/leases"))

	lease := assignedLease(t, a)
	name := provider.InstanceName(lease.ID)

	beforeLaunch := time.Now()

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	rec.mu.Lock()
	leaseFile, launched := rec.watched[name], rec.launched[name]
	rec.mu.Unlock()

	if leaseFile != "/leases/billet1/dnsmasq.leases" {
		t.Fatalf("followed %q, want the guest's bridge's lease file", leaseFile)
	}

	p.mu.Lock()
	enteredLaunch := p.launchedAt
	p.mu.Unlock()

	// BEFORE THE BACKEND'S LAUNCH IS ENTERED, since the guest can open flows
	// while it is still inside it.
	if launched.Before(beforeLaunch) || launched.After(enteredLaunch) {
		t.Errorf("followed from %v, want a moment before the backend's launch began (%v)", launched, enteredLaunch)
	}

	beforeDestroy := time.Now()

	if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	p.mu.Lock()
	destroyedAt := p.destroyedAt
	p.mu.Unlock()

	if len(rec.finals) != 1 || rec.finals[0] != name {
		t.Fatalf("finals = %v, want %s", rec.finals, name)
	}

	if rec.finalAt.Before(destroyedAt) {
		t.Error("the flows were taken before the compute was destroyed, so the guest could still open more")
	}

	if rec.until.Before(beforeDestroy) || rec.until.After(destroyedAt) {
		t.Errorf("the flows were bounded at %v, want the moment the destroy began", rec.until)
	}
}

// A GUEST'S FLOWS ARE FOLLOWED FROM THE LEASE FILE OF THE BRIDGE ITS TAP IS ON,
// and an unusable MAC or bridge follows nothing rather than another guest's
// lease.
func TestStartFlowsFollowsTheGuestsOwnLease(t *testing.T) {
	t.Parallel()

	launched := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		target provider.UsageTarget
		want   string
	}{
		{"a guest on billet1", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "billet1"}, "/leases/billet1/dnsmasq.leases"},
		{"no MAC", provider.UsageTarget{Bridge: "billet1"}, ""},
		{"a MAC that does not parse", provider.UsageTarget{GuestMAC: "nope", Bridge: "billet1"}, ""},
		{"no bridge", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17"}, ""},
		{"a bridge that is a path", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "../etc"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := newRecordingFlows()
			r := &Runner{log: slog.New(slog.DiscardHandler), flows: rec, leaseDir: "/leases"}

			r.startFlows("billet-1", tc.target, launched)

			got, watched := rec.watched["billet-1"]

			switch {
			case tc.want == "" && watched:
				t.Errorf("followed %q for a target with no usable lease", got)
			case tc.want != "" && got != tc.want:
				t.Errorf("followed %q, want %q", got, tc.want)
			case tc.want != "" && !rec.launched["billet-1"].Equal(launched):
				t.Errorf("followed from %v, want the launch time %v", rec.launched["billet-1"], launched)
			}
		})
	}
}

// WITHOUT FLOWS NOTHING IS FOLLOWED, and forgetting a job forgets its flows.
func TestFlowsAreOptionalAndForgottenWithTheJob(t *testing.T) {
	t.Parallel()

	r := &Runner{log: slog.New(slog.DiscardHandler)}
	r.startFlows("billet-1", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "billet1"}, time.Now())
	r.finalFlows(t.Context(), "billet-1", time.Now(), usage.Summary{}, false)
	r.forgetMonitoring("billet-1")

	rec := newRecordingFlows()
	r = &Runner{log: slog.New(slog.DiscardHandler), flows: rec, leaseDir: "/leases"}
	r.forgetMonitoring("billet-2")

	if len(rec.forgotten) != 1 || rec.forgotten[0] != "billet-2" {
		t.Errorf("forgotten = %v, want billet-2", rec.forgotten)
	}
}
