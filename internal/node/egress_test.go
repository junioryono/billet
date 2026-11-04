package node

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/egress"
	"github.com/junioryono/billet/internal/provider"
)

// fakeEgressHost keeps a record for each guest a Destroy has detached, as the real one
// does: nothing can be taken until then, which is what proves the order of the two.
type fakeEgressHost struct {
	mu         sync.Mutex
	destroyed  map[string]bool
	record     egress.Record
	asked      []string
	taken      []string
	forgotten  []string
	reconciled []map[string]bool
	handedOver int
	verified   int
}

func (f *fakeEgressHost) Take(key string) (egress.Record, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, key)

	if !f.destroyed[key] {
		return egress.Record{}, false
	}

	f.taken = append(f.taken, key)

	return f.record, true
}

func (f *fakeEgressHost) Forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forgotten = append(f.forgotten, key)

	// AS THE REAL ONE DOES: a Take after this finds nothing, so a forget ahead of the
	// report loses it.
	delete(f.destroyed, key)
}

func (f *fakeEgressHost) Reconcile(_ context.Context, running map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reconciled = append(f.reconciled, running)
}

func (f *fakeEgressHost) Verify(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.verified++
}

func (f *fakeEgressHost) HandOver(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.handedOver++
}

// enforcingProvider is a backend that says it can enforce a list, and keeps the specs
// it launched and which guests it destroyed.
type enforcingProvider struct {
	*fakeProvider
	notReady error
	host     *fakeEgressHost
	mu       sync.Mutex
	specs    []provider.Spec
	// names is each instance's name by the id Destroy is given.
	names map[string]string
}

func (p *enforcingProvider) EgressReady() error { return p.notReady }

func (p *enforcingProvider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	p.mu.Lock()
	p.specs = append(p.specs, spec)
	p.mu.Unlock()

	inst, err := p.fakeProvider.Launch(ctx, spec)
	if err == nil {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.names == nil {
			p.names = map[string]string{}
		}

		p.names[inst.ID] = spec.Name
	}

	return inst, err
}

func (p *enforcingProvider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	state, err := p.fakeProvider.Destroy(ctx, id)

	if err == nil && p.host != nil {
		p.mu.Lock()
		name := p.names[id]
		p.mu.Unlock()

		p.host.mu.Lock()
		if p.host.destroyed == nil {
			p.host.destroyed = map[string]bool{}
		}
		p.host.destroyed[name] = true
		p.host.mu.Unlock()
	}

	return state, err
}

func listedSpec() *config.TierEgress { return &config.TierEgress{Allow: []string{"example.com"}} }

// A LIST THIS HOST CANNOT APPLY NEVER STARTS A GUEST, AND MINTS NOTHING: a registration
// with nothing to consume it is a runner left on GitHub for each refused job.
func TestAListThatCannotBeAppliedIsRefusedBeforeARegistrationIsMinted(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		provider provider.Provider
		allow    []string
		want     string
	}{
		"a backend that cannot enforce one": {
			provider: &fakeProvider{kind: config.ProviderDocker},
			allow:    []string{"example.com"}, want: "cannot be enforced",
		},
		"a host that is not ready": {
			provider: &enforcingProvider{
				fakeProvider: &fakeProvider{kind: config.ProviderDocker},
				notReady:     errors.New("converge the host"),
			},
			allow: []string{"example.com"}, want: "converge the host",
		},
		"a list this node cannot read": {
			provider: &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}},
			allow:    []string{"not a name"}, want: "not a name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a, host := newAllocatorWithHost(t)
			jit := &fakeJIT{setID: 7}
			r := New(a, host, jit, tc.provider, nil)

			tier := dockerSpec()
			tier.Egress = &config.TierEgress{Allow: tc.allow}

			lease := assignedLease(t, a)

			err := r.Launch(t.Context(), lease, tier, Job{RequestID: lease.RequestID, Event: "push"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Launch = %v, want a refusal naming %q", err, tc.want)
			}

			if len(jit.names) != 0 {
				t.Fatalf("minted %v for a guest that will never start", jit.names)
			}

			if p, ok := tc.provider.(*enforcingProvider); ok && len(p.specs) != 0 {
				t.Fatalf("launched %d guests", len(p.specs))
			}
		})
	}
}

// launchedPolicy launches a job of tier through a runner and answers its guest's policy.
func launchedPolicy(t *testing.T, egressList *config.TierEgress, observe bool) *egress.Policy {
	t.Helper()

	a, host := newAllocatorWithHost(t)
	p := &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(&fakeEgressHost{}, observe))

	tier := dockerSpec()
	tier.Egress = egressList

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, tier, Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.specs) != 1 {
		t.Fatalf("launched %d guests", len(p.specs))
	}

	return p.specs[0].Egress
}

func TestAGuestsPolicyFollowsItsTierAndTheNode(t *testing.T) {
	t.Parallel()

	if got := launchedPolicy(t, listedSpec(), false); !got.Listed() || !got.AllowsName("example.com") ||
		got.AllowsName("other.example") {
		t.Errorf("a listed tier's guest: %+v", got)
	}

	if got := launchedPolicy(t, listedSpec(), true); !got.Listed() {
		t.Errorf("a listed tier on a node watching names read as open: %+v", got)
	}

	if got := launchedPolicy(t, nil, true); got == nil || got.Listed() {
		t.Errorf("an open tier with flows on: %+v, want it watched", got)
	}

	// AND A NODE NOT ASKED FOR NAMES CHANGES NOTHING FOR AN OPEN TIER.
	if got := launchedPolicy(t, nil, false); got != nil {
		t.Errorf("an open tier with flows off: %+v, want no policy", got)
	}
}

// THROUGH THE RUNNER'S OWN DESTROY, WITH NOTHING SAMPLED: a tier's list is enforced on a
// node with no node.monitoring, and what it refused is still reported, once, after the
// compute has gone, and forgotten afterwards. The fake has nothing to take before the
// destroy, so a report made ahead of it would report nothing.
func TestAJobsNetworkIsReportedAfterItsComputeGoesWithNothingSampled(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	eh := &fakeEgressHost{record: egress.Record{
		Listed: true, Queries: 3, RefusedQueries: 1, Refused: []egress.Refused{{Name: "evil.example", Count: 1}},
		Names:         map[netip.Addr][]string{netip.MustParseAddr("198.51.100.1"): {"github.com"}},
		DeniedPackets: 5, OutsideResolverPackets: 2, IPv6Packets: 4,
		Denied: []egress.Denied{{Addr: netip.MustParseAddr("203.0.113.7"), Protocol: "tcp", Port: 443, Packets: 5}},
	}}
	p := &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, host: eh}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(eh, false))

	tier := dockerSpec()
	tier.Egress = listedSpec()

	lease := assignedLease(t, a)
	name := provider.InstanceName(lease.ID)

	if err := r.Launch(t.Context(), lease, tier, Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	eh.mu.Lock()
	taken, forgotten := slices.Clone(eh.taken), slices.Clone(eh.forgotten)
	eh.mu.Unlock()

	if !slices.Equal(taken, []string{name}) {
		t.Fatalf("took %q, want the guest's record once, after its compute went", taken)
	}

	if !slices.Contains(forgotten, name) {
		t.Fatalf("the record was not forgotten after the job: %q", forgotten)
	}

	got, err := a.LeaseEgress(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseEgress: %v", err)
	}

	if !got.Listed || got.RefusedQueries != 1 || got.DeniedPackets != 5 || got.OutsideResolverPackets != 2 ||
		got.IPv6Packets != 4 || len(got.Names) != 1 || got.Names[0].Names[0] != "github.com" ||
		len(got.Denied) != 1 || got.Denied[0].Port != 443 {
		t.Fatalf("the ledger holds %+v", got.JobEgress)
	}
}

// A FAILED DESTROY OWES NO REPORT: its guest may still be running.
func TestAGuestWhoseDestroyFailedIsNotReported(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	eh := &fakeEgressHost{destroyed: map[string]bool{}}
	fp := &fakeProvider{kind: config.ProviderDocker, destroyErr: errors.New("could not tell")}
	p := &enforcingProvider{fakeProvider: fp, host: eh}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(eh, false))

	tier := dockerSpec()
	tier.Egress = listedSpec()

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, tier, Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), lease.RequestID); err == nil {
		t.Fatal("Destroy reported success")
	}

	eh.mu.Lock()
	defer eh.mu.Unlock()

	if len(eh.asked) != 0 {
		t.Fatalf("asked for %q's record although the guest may still run", eh.asked)
	}
}

// AN ADOPTED JOB'S NETWORK IS REPORTED BY THE CUSTODY THAT ENDS IT, on the path that took a
// lease across a handover, with or without a sample.
func TestAnAdoptedJobsNetworkIsReportedByItsCustody(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	eh := &fakeEgressHost{record: egress.Record{Listed: true, Queries: 3, Incomplete: true}}
	p := &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, host: eh}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(eh, false))

	lease := assignedLease(t, a)
	name := provider.InstanceName(lease.ID)

	eh.destroyed = map[string]bool{name: true}

	entry := &custody{leaseID: lease.ID, name: name}
	entry.epoch.Store(lease.Epoch)
	entry.measured.Store(true)

	r.settleHeldUsage(t.Context(), entry, heldUsage{}, true)

	got, err := a.LeaseEgress(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseEgress: %v", err)
	}

	if !got.Listed || got.Queries != 3 || !got.Incomplete {
		t.Fatalf("the ledger holds %+v", got.JobEgress)
	}
}

// A REPORT THAT LEAVES DESTINATIONS OUT SAYS SO, on its own and not because something else
// was left out too: `complete: yes` over 64 of 65 would be a false claim.
func TestAReportThatTrimsItsDeniedListIsIncomplete(t *testing.T) {
	t.Parallel()

	rec := egress.Record{Listed: true, DeniedPackets: 100}
	for i := range alloc.MaxEgressDenied + 1 {
		rec.Denied = append(rec.Denied, egress.Denied{
			Addr: netip.AddrFrom4([4]byte{198, 18, 0, byte(i)}), Protocol: "tcp", Port: 1, Packets: 1,
		})
	}

	if out := jobEgressOf(rec, nil); !out.Incomplete || len(out.Denied) != alloc.MaxEgressDenied {
		t.Fatalf("incomplete %v with %d denied", out.Incomplete, len(out.Denied))
	}

	rec.Denied = rec.Denied[:alloc.MaxEgressDenied]
	if out := jobEgressOf(rec, nil); out.Incomplete {
		t.Fatal("a report that trimmed nothing says it is incomplete")
	}
}

func TestAReportHoldsWithinItsBounds(t *testing.T) {
	t.Parallel()

	addr := func(i int) netip.Addr { return netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)}) }

	rec := egress.Record{Listed: true, Names: map[netip.Addr][]string{}, DeniedPackets: 2, OutsideResolverPackets: 9}

	for i := range alloc.MaxEgressNamedAddrs + 10 {
		rec.Names[addr(i)] = []string{"a.example"}
	}

	for range alloc.MaxEgressDenied + 1 {
		rec.Denied = append(rec.Denied, egress.Denied{Addr: addr(0), Protocol: "udp", Port: 53, Packets: 1})
	}

	for range alloc.MaxEgressRefusedNames + 1 {
		rec.Refused = append(rec.Refused, egress.Refused{Name: "a.example", Count: 1})
	}

	rec.Queries, rec.RefusedQueries = 100, 100

	// THE LAST ADDRESS BY ORDER IS THE ONE A FLOW REACHED, AND IT MUST STILL BE NAMED.
	wanted := addr(alloc.MaxEgressNamedAddrs + 9)
	destinations := &alloc.JobDestinations{Destinations: []alloc.JobDestination{{Addr: wanted.String()}}}

	out := jobEgressOf(rec, destinations)

	if len(out.Names) != alloc.MaxEgressNamedAddrs || out.Names[0].Addr != wanted.String() {
		t.Fatalf("named %d addresses, first %s; want the job's own destinations first", len(out.Names), out.Names[0].Addr)
	}

	if !out.Incomplete {
		t.Error("addresses were dropped and the report does not say it is a lower bound")
	}

	if len(out.Denied) != alloc.MaxEgressDenied || len(out.RefusedNames) != alloc.MaxEgressRefusedNames {
		t.Errorf("denied %d, refused %d", len(out.Denied), len(out.RefusedNames))
	}

	// ONE COUNTER READ AND THE OTHER NOT MUST STILL BE A REPORT THE PLANE KEEPS. Duplicate
	// names above are not, so this is checked on a report without them.
	if out.OutsideResolverPackets != 2 {
		t.Errorf("outside = %d, want it held to the denied count", out.OutsideResolverPackets)
	}

	small := jobEgressOf(egress.Record{Listed: true, DeniedPackets: 2, OutsideResolverPackets: 9}, nil)
	if err := small.Validate(); err != nil {
		t.Errorf("the plane would refuse this: %v", err)
	}
}

// A SWEEP ASKS WHETHER A LIST'S RULES STILL HOLD: a healthy node may not register again for the
// length of a job, and something that took them away has left it open until something looks.
func TestASweepChecksThatListsAreStillInForce(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	eh := &fakeEgressHost{}
	p := &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, host: eh}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(eh, true))

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	eh.mu.Lock()
	defer eh.mu.Unlock()

	if eh.verified != 1 {
		t.Fatalf("verified %d times", eh.verified)
	}
}

// A HANDOVER GIVES AN OPEN GUEST'S DNS BACK WHETHER OR NOT ANYTHING IS SAMPLED, and a
// recovery judges what is left only from a List that succeeded.
func TestAHandOverAndARecoveryReachTheHost(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	eh := &fakeEgressHost{}
	p := &enforcingProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, host: eh}
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithEgress(eh, true))

	lease := assignedLease(t, a)
	name := provider.InstanceName(lease.ID)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	eh.mu.Lock()
	reconciled := slices.Clone(eh.reconciled)
	eh.mu.Unlock()

	if len(reconciled) != 1 || !reconciled[0][name] {
		t.Fatalf("reconciled %v, want the one recovery, showing the guest", reconciled)
	}

	r.HandOver(t.Context())

	eh.mu.Lock()
	handed := eh.handedOver
	eh.mu.Unlock()

	if handed != 1 {
		t.Fatalf("handed over %d times", handed)
	}

	p.listErr = errors.New("the list failed")

	if err := r.Recover(t.Context()); err == nil {
		t.Fatal("Recover succeeded on a failed list")
	}

	eh.mu.Lock()
	defer eh.mu.Unlock()

	if len(eh.reconciled) != 1 {
		t.Fatalf("a recovery with no List judged the rules: %v", eh.reconciled)
	}
}
