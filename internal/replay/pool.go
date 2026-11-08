package replay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/pool"
)

// PoolFleet is a pool of rented hosts beside the fleet's fixed ones (ADR-030): the real
// scaler, internal/pool, orders from a source the harness plays, and each host it
// orders joins as a node of the stack once its lead time has passed in simulated time.
//
// WHAT IS REAL AND WHAT IS PLAYED. The scaler, the allocator's orders, its demand
// reading and the placement of jobs on a joined host are billet's. The source and the
// join are the harness's: a host's order is admitted as AdmitPoolEnrollment would
// admit it on the source's word, without a key or a certificate, because the replay's
// wire is the loopback one and carries neither. A release's proof is the node's own
// clearance, which the plane measures on the wall clock (five minutes), so a replay
// records the pool asking for a release and does not wait for the release.
type PoolFleet struct {
	// Archs are each architecture's floor and maximum.
	Archs map[config.Arch]PoolArch
	// Host is the shape of every host the pool orders; its Name and Arch are the
	// order's.
	Host Host
	// LeadTime is how long an order takes to become a node that has joined, in
	// simulated time.
	LeadTime time.Duration
	// Interval is how often the scaler steps, in simulated time. Zero means a minute.
	Interval time.Duration
	// IdleAfter is server.pool.idle_after; zero, the pool never shrinks itself.
	IdleAfter time.Duration
}

// PoolArch is one architecture's part of the pool.
type PoolArch struct {
	Floor, Max int
}

// poolSourceName is the one source a replayed pool orders from.
const poolSourceName = "replay"

// defaultPoolInterval is the step a pool takes when the fleet names none.
const defaultPoolInterval = time.Minute

func (p *PoolFleet) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}

	return defaultPoolInterval
}

func (p *PoolFleet) validate() []error {
	var errs []error
	if len(p.Archs) == 0 {
		errs = append(errs, errors.New("replay: the pool names no architecture"))
	}
	for arch, a := range p.Archs {
		if arch == "" || a.Floor < 0 || a.Max < a.Floor || a.Max == 0 {
			errs = append(errs, fmt.Errorf("replay: the pool's %q needs a floor of 0 or more and a maximum "+
				"at least the floor and above 0", arch))
		}
	}
	if p.Host.VCPU <= 0 || p.Host.Memory <= 0 {
		errs = append(errs, errors.New("replay: the pool's host needs positive vcpu and memory"))
	}
	if p.LeadTime < 0 || p.Interval < 0 || p.IdleAfter < 0 {
		errs = append(errs, errors.New("replay: the pool's durations may not be negative"))
	}

	return errs
}

// PoolReport is what the replayed pool did.
type PoolReport struct {
	// Ordered is every order the scaler placed, in order.
	Ordered []PoolOrderRecord
	// MostJoined is the most hosts joined at once, per architecture.
	MostJoined map[config.Arch]int
	// ReleasesAsked is the orders whose release the scaler asked for, each once.
	ReleasesAsked []string
	// StoppedAbove is, per architecture, how many joined hosts beyond the floor the
	// pool had not asked to release when the replay stopped: an observation cut
	// short, never a pool that would not shrink.
	StoppedAbove map[config.Arch]int
	// Stalled is a replay whose pool stopped stepping because nothing else had
	// happened for longer than any job could need.
	Stalled bool
}

// PoolOrderRecord is one order's life in simulated time, each instant zero until it
// happened.
type PoolOrderRecord struct {
	Order    string
	Arch     config.Arch
	Ordered  time.Time
	Joined   time.Time
	Released time.Time
}

// replayHost is one host the played source holds.
type replayHost struct {
	id, order string
	arch      config.Arch
	shape     string
	token     string
	ordered   time.Time
	joined    time.Time
	released  bool
	stop      func()
}

// replaySource is the source a replayed pool orders from: every order is accepted and
// becomes running at its lead time; a release takes the host back at once and says so.
type replaySource struct {
	mu    sync.Mutex
	clock *Clock
	lead  time.Duration
	arch  map[string]config.Arch
	hosts map[string]*replayHost
	order []string
}

var _ hostsource.Source = (*replaySource)(nil)

func (s *replaySource) Name() string                          { return poolSourceName }
func (s *replaySource) Scope(context.Context) (string, error) { return poolSourceName, nil }

// Order records the host and the join token its boot carries, which the played join
// spends.
func (s *replaySource) Order(_ context.Context, req hostsource.Request) (hostsource.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hosts[req.Order]; ok {
		return s.describe(h), nil
	}
	h := &replayHost{
		id: "replay-" + req.Order, order: req.Order, arch: s.arch[req.Shape], shape: req.Shape,
		token: string(req.Boot.Reveal()), ordered: s.clock.Now(),
	}
	s.hosts[req.Order] = h
	s.order = append(s.order, req.Order)

	return s.describe(h), nil
}

func (s *replaySource) describe(h *replayHost) hostsource.Host {
	state := hostsource.StateProvisioning
	switch {
	case h.released:
		state = hostsource.StateReleasing
	case !s.clock.Now().Before(h.ordered.Add(s.lead)):
		state = hostsource.StateRunning
	}

	return hostsource.Host{ID: h.id, Order: h.order, Shape: h.shape, Location: poolSourceName, State: state}
}

func (s *replaySource) Find(_ context.Context, order string) (hostsource.Host, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[order]
	if !ok {
		return hostsource.Host{}, false, nil
	}

	return s.describe(h), true, nil
}

func (s *replaySource) byID(id string) *replayHost {
	for _, h := range s.hosts {
		if h.id == id {
			return h
		}
	}

	return nil
}

func (s *replaySource) Describe(_ context.Context, id string) (hostsource.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.byID(id)
	if h == nil {
		return hostsource.Host{}, fmt.Errorf("replay: no host %s", id)
	}

	return s.describe(h), nil
}

func (s *replaySource) Gone(_ context.Context, id string) (hostsource.Presence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.byID(id); h != nil && h.released {
		return hostsource.PresenceGone, nil
	}

	return hostsource.PresencePresent, nil
}

// Release takes a host back only on a proof for it, as a source does.
func (s *replaySource) Release(_ context.Context, proof hostsource.ReleaseProof) error {
	if err := proof.For(proof.Host()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.byID(proof.Host())
	if h == nil {
		return fmt.Errorf("replay: no host %s", proof.Host())
	}
	h.released = true

	return nil
}

// Terms are a source answering in the lead time, billed by the second, and saying
// gone of a host it took back, so the pool may shrink through it.
func (s *replaySource) Terms(string) (hostsource.Terms, error) {
	return hostsource.Terms{
		LeadTime: s.lead, BillingUnit: time.Second, IdempotentFor: 10 * time.Minute, ConfirmsGone: true,
	}, nil
}

// poolDriver steps the real scaler at the pool's interval and joins the hosts it
// ordered once they are running.
type poolDriver struct {
	t      *testing.T
	fleet  *PoolFleet
	alloc  *alloc.Allocator
	nodes  *nodeStarter
	plane  interface{ Nodes() []string }
	clock  *Clock
	pool   *pool.Pool
	source *replaySource
	report PoolReport
	asked  map[string]bool
	serial int
	// stall is how long the replay may go with nothing but the pool's steps before it
	// stops them; tail is the fixed deadline of a quiet pool's shrink, zero until it
	// went quiet.
	stall time.Duration
	tail  time.Time
	// scheduled is every order whose join is scheduled; lastJoin the latest join.
	scheduled map[string]bool
	lastJoin  time.Time
}

// newPoolDriver builds the scaler as internal/app builds it, over the played source.
//
// A FIRECRACKER TIER FOR THE SCALER ONLY. The pool grows for a Firecracker tier short of
// hosts, and the replay's tiers are the simulated backend, which stands for one here:
// the scaler is handed the same tiers with that one word changed, and the allocator,
// the listeners and the placement keep the simulated tiers they run.
func newPoolDriver(t *testing.T, fleet Fleet, trace Trace, tiers []config.Tier, st *stack, clock *Clock) *poolDriver {
	t.Helper()
	pf := fleet.Pool
	src := &replaySource{clock: clock, lead: pf.LeadTime, arch: map[string]config.Arch{},
		hosts: map[string]*replayHost{}}
	archs := make(map[config.Arch]pool.Policy, len(pf.Archs))
	for arch, a := range pf.Archs {
		shape := "replay-" + string(arch)
		src.arch[shape] = arch
		archs[arch] = pool.Policy{Floor: a.Floor, Max: a.Max, Choices: []pool.Choice{
			{Source: poolSourceName, Shape: shape, Location: poolSourceName},
		}}
	}
	scaled := slices.Clone(tiers)
	for i := range scaled {
		scaled[i].Provider = config.ProviderFirecracker
	}
	next := 0
	p, err := pool.New(st.alloc, pool.Config{
		Archs:     archs,
		Sources:   map[string]hostsource.Source{poolSourceName: src},
		IdleAfter: pf.IdleAfter,
		Tiers:     scaled,
		Boot:      func(_, token string) ([]byte, error) { return []byte(token), nil },
	}, pool.WithClock(clock.Now), pool.WithIDs(func() (string, error) {
		next++

		return fmt.Sprintf("r%03d", next), nil
	}))
	if err != nil {
		t.Fatalf("replay: build the pool: %v", err)
	}

	return &poolDriver{
		t: t, fleet: pf, alloc: st.alloc, nodes: st.nodes, plane: st.plane, clock: clock, pool: p, source: src,
		report: PoolReport{MostJoined: map[config.Arch]int{}}, asked: map[string]bool{}, scheduled: map[string]bool{},
		stall: longestJob(trace) + pf.LeadTime + pf.IdleAfter + 6*time.Hour,
	}
}

// step is one of the scaler's steps; then every order the ledger now holds is
// recorded, every host now running joins, and every host the source took back ends.
// It returns the hosts the step ordered, each to join at its own lead time.
//
// AN ORDER IS DATED BY ITS STEP'S INSTANT, at, not by the clock, which the harness
// moves a nanosecond past an instant another event already took: a host ordered at a
// step joins exactly a lead time later, before the step at that instant, as one that
// joined in the lead time would.
func (d *poolDriver) step(ctx context.Context, at time.Time) []readyAt {
	t := d.t
	t.Helper()
	if err := d.pool.Step(ctx); err != nil {
		t.Logf("replay: the pool's step at %s: %v", d.clock.Now().Format(time.RFC3339), err)
	}

	d.source.mu.Lock()
	var due []readyAt
	for _, id := range d.source.order {
		if h := d.source.hosts[id]; !d.scheduled[id] {
			d.scheduled[id] = true
			h.ordered = at
			due = append(due, readyAt{order: id, at: at.Add(d.source.lead)})
		}
	}
	d.source.mu.Unlock()
	d.observe(ctx)

	return due
}

// readyAt is when an order's host is running and joins.
type readyAt struct {
	order string
	at    time.Time
}

// observe records every order the ledger holds and every release it asked for, joins
// every placed host whose lead time has passed, and ends every host the source took
// back.
func (d *poolDriver) observe(ctx context.Context) {
	t := d.t
	t.Helper()
	orders, err := d.alloc.PoolOrders(ctx)
	if err != nil {
		t.Fatalf("replay: read the pool's orders: %v", err)
	}
	byID := make(map[string]*alloc.PoolOrder, len(orders))
	for i := range orders {
		o := &orders[i]
		byID[o.ID] = o
		if d.recordOf(o.ID) == nil {
			ordered := o.FirstAttempt
			d.source.mu.Lock()
			if h, ok := d.source.hosts[o.ID]; ok {
				ordered = h.ordered
			}
			d.source.mu.Unlock()
			d.report.Ordered = append(d.report.Ordered, PoolOrderRecord{Order: o.ID, Arch: o.Arch, Ordered: ordered})
		}
		if !o.ReleaseRequested.IsZero() && !d.asked[o.ID] {
			d.asked[o.ID] = true
			d.report.ReleasesAsked = append(d.report.ReleasesAsked, o.ID)
		}
	}

	d.source.mu.Lock()
	var ready, gone []*replayHost
	for _, id := range d.source.order {
		h := d.source.hosts[id]
		switch {
		case h.released && h.stop != nil:
			gone = append(gone, h)
		case !h.released && h.joined.IsZero() && !d.clock.Now().Before(h.ordered.Add(d.source.lead)):
			ready = append(ready, h)
		}
	}
	d.source.mu.Unlock()

	for _, h := range ready {
		if o, ok := byID[h.order]; ok && o.State == alloc.PoolPlaced {
			d.join(ctx, h, o)
		}
	}
	for _, h := range gone {
		h.stop()
		h.stop = nil
		if r := d.recordOf(h.order); r != nil {
			r.Released = d.clock.Now()
		}
	}

	joined := map[config.Arch]int{}
	d.source.mu.Lock()
	for _, h := range d.source.hosts {
		if !h.joined.IsZero() && !h.released {
			joined[h.arch]++
		}
	}
	d.source.mu.Unlock()
	for arch, n := range joined {
		d.report.MostJoined[arch] = max(d.report.MostJoined[arch], n)
	}
}

// join admits a running host's order as the source's word would, then starts its node
// and waits until the plane has it.
func (d *poolDriver) join(ctx context.Context, h *replayHost, o *alloc.PoolOrder) {
	t := d.t
	t.Helper()
	d.serial++
	serial := strconv.Itoa(d.serial)
	if _, err := d.alloc.AdmitPoolEnrollment(ctx, alloc.PoolAdmission{
		Order: o.ID, Fingerprint: "SHA256:replay-" + o.ID, CSRPEM: "replay", Token: h.token,
		CertPEM: "replay", Serial: serial, NotAfter: d.clock.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		Seen: o.Updated,
	}); err != nil {
		t.Fatalf("replay: admit pool order %s: %v", o.ID, err)
	}
	name := alloc.PoolNodeName(o.ID)
	host := d.fleet.Host
	host.Name, host.Arch = name, h.arch
	stop := d.nodes.start(ctx, host)

	deadline := time.Now().Add(registrationWait)
	for !slices.Contains(d.plane.Nodes(), name) {
		if time.Now().After(deadline) {
			t.Fatalf("replay: pool host %s never registered over the wire", name)
		}
		time.Sleep(20 * time.Millisecond)
	}

	now := d.clock.Now()
	d.source.mu.Lock()
	h.joined, h.stop = now, stop
	d.source.mu.Unlock()
	if r := d.recordOf(o.ID); r != nil {
		r.Joined = now
	}
	d.lastJoin = now
}

func (d *poolDriver) recordOf(order string) *PoolOrderRecord {
	for i := range d.report.Ordered {
		if d.report.Ordered[i].Order == order {
			return &d.report.Ordered[i]
		}
	}

	return nil
}

// settled reports whether no host is on its way to joining.
func (d *poolDriver) settled() bool {
	d.source.mu.Lock()
	defer d.source.mu.Unlock()
	for _, h := range d.source.hosts {
		if !h.released && h.joined.IsZero() {
			return false
		}
	}

	return true
}

// above is, per architecture, how many joined hosts beyond its floor the pool has not
// asked to release.
func (d *poolDriver) above() map[config.Arch]int {
	serving := map[config.Arch]int{}
	d.source.mu.Lock()
	for _, h := range d.source.hosts {
		if !h.joined.IsZero() && !h.released && !d.asked[h.order] {
			serving[h.arch]++
		}
	}
	d.source.mu.Unlock()
	out := map[config.Arch]int{}
	for arch, n := range serving {
		if excess := n - d.fleet.Archs[arch].Floor; excess > 0 {
			out[arch] = excess
		}
	}

	return out
}

// ticksOn reports whether the pool steps again after the one at now: while anything
// but ticks is still to come, a job is unfinished or a host is on its way; and, for a
// pool that shrinks itself, while hosts beyond the floor are not yet asked to leave,
// until idle_after and a step for each of them, and two to spare, have passed since
// the later of the last completion and the last join. A replay that stops with hosts
// still beyond the floor says so in the report.
func (d *poolDriver) ticksOn(actions *plane, now, lastActivity time.Time) bool {
	last := lastActivity
	if d.lastJoin.After(last) {
		last = d.lastJoin
	}
	// A JOB THAT NEVER FINISHES WOULD TICK FOREVER, so the pool stops once nothing but
	// its own steps has happened for longer than any job of the trace could need, and
	// the report says the replay stalled; a workload still making progress is never cut
	// short by it.
	if now.Sub(last) > d.stall {
		d.report.Stalled = true
		d.report.StoppedAbove = d.above()

		return false
	}
	if !actions.onlyTicksLeft() || !actions.everyJobCompleted() || !d.settled() {
		d.tail = time.Time{}

		return true
	}
	if d.fleet.IdleAfter == 0 {
		return false
	}
	above := d.above()
	excess := 0
	for _, n := range above {
		excess = max(excess, n)
	}
	if excess == 0 {
		return false
	}
	// THE DEADLINE IS FIXED WHEN THE POOL GOES QUIET, from the hosts beyond its floor
	// then, so a release each step does not shorten the time the rest have.
	if d.tail.IsZero() {
		d.tail = last.Add(d.fleet.IdleAfter + time.Duration(excess+2)*d.fleet.interval())
	}
	if now.Before(d.tail) {
		return true
	}
	d.report.StoppedAbove = above

	return false
}

// longestJob is the trace's longest job on any tier.
func longestJob(trace Trace) time.Duration {
	var longest time.Duration
	for _, tier := range trace.Tiers() {
		longest = max(longest, trace.LongestDuration(tier))
	}

	return longest
}

// joinedHosts are the hosts the pool's orders joined as, each there from its join to
// its release, for the report to charge a lease to and to count as room only while it
// was there.
func (d *poolDriver) joinedHosts() []Host {
	out := make([]Host, 0, len(d.report.Ordered))
	for _, o := range d.report.Ordered {
		if o.Joined.IsZero() {
			continue
		}
		h := d.fleet.Host
		h.Name, h.Arch = alloc.PoolNodeName(o.Order), o.Arch
		h.Joined, h.Left = o.Joined, o.Released
		out = append(out, h)
	}

	return out
}

// warmup is how long before the first job a replay starts, so the pool's floor has
// joined by then: a step for each floor host, then a lead time, and a step to spare.
// Nil, a fleet without a pool, needs none.
func (p *PoolFleet) warmup() time.Duration {
	if p == nil {
		return 0
	}
	floor := 0
	for _, a := range p.Archs {
		floor = max(floor, a.Floor)
	}
	if floor == 0 {
		return 0
	}

	return time.Duration(floor+1)*p.interval() + p.LeadTime
}
