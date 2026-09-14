package server

import (
	"context"
	"slices"
	"sync"

	"github.com/junioryono/billet/internal/config"
)

// discoveryArbiter gives the whole catalogue one admission turn at a time.
//
// KNOWN UNMET WORK PRECEDES DISCOVERY. Each handled exchange or pre-poll pool
// attempt advances a sorted round robin by one lease, not by a tier's backlog.
// A blocked winner keeps its turn: smaller contenders would spend the returning
// fragments forever before a large shape could fit. The allocator still decides
// whether a lease fits and protects every explicit floor.
//
// A TURN IS PERMISSION TO ESCROW, NEVER PERMISSION TO RELEASE. Listeners lower
// their advertisement through the ordinary shrink path and keep old backing
// until the exchange and its assignments have been handled successfully.
//
// QUEUED WORK ALWAYS BEATS SPECULATIVE DISCOVERY. Under perpetual saturation
// there is no capacity for undiscovered work either; idle discovery resumes
// when eligible known demand no longer needs the returning capacity.
type discoveryArbiter struct {
	mu sync.Mutex

	labels     []string
	entries    map[string]discoveryEntry
	owner      string
	generation uint64
	lastWork   string
	lastIdle   string
	work       bool
	waiting    bool
	granted    bool
}

type discoveryEntry struct {
	possible bool
	demand   bool
	active   int
	held     int
	ceiling  int
}

// offerIdentity keeps zero-request direct jobs separate until they are assigned.
type offerIdentity struct {
	request int64
	job     string
}

func identityOfOffer(job Job) offerIdentity {
	if job.RequestID != 0 {
		return offerIdentity{request: job.RequestID}
	}

	return offerIdentity{job: job.JobID}
}

func newDiscoveryArbiter(tiers []config.Tier) *discoveryArbiter {
	a := &discoveryArbiter{entries: make(map[string]discoveryEntry, len(tiers))}
	for i := range tiers {
		a.labels = append(a.labels, tiers[i].Label)
		a.entries[tiers[i].Label] = discoveryEntry{possible: true, ceiling: tiers[i].MaxConcurrent}
	}
	slices.Sort(a.labels)
	a.choose()

	return a
}

// choose runs with mu held, including at construction before publication.
func (a *discoveryArbiter) choose() {
	work := false
	for _, e := range a.entries {
		work = work || (e.eligible() && e.demand)
	}
	// A BACKED WORK TURN ENDS AFTER ITS EXCHANGE OR POOL ATTEMPT IS HANDLED.
	// Consuming its lease can reach max_concurrent; rotating here would lose the
	// cursor update and let two fast capped tiers repeatedly bypass a third.
	if a.owner != "" && a.granted && a.work {
		return
	}
	if e, ok := a.entries[a.owner]; ok && e.eligible() {
		if a.work && a.waiting && e.demand {
			return
		}
		if !a.work && !work {
			return
		}
	}

	last := a.lastIdle
	if work {
		last = a.lastWork
	}
	owner := ""
	for _, label := range a.labels {
		e := a.entries[label]
		if !e.eligible() || (work && !e.demand) {
			continue
		}
		if owner == "" {
			owner = label
		}
		if label > last {
			owner = label
			break
		}
	}
	if owner == a.owner && work == a.work {
		return
	}
	if a.owner != "" && !a.work && work {
		// A PREEMPTED DISCOVERY TURN JOINS THE BACK OF ITS OWN QUEUE. Otherwise
		// the donor would win again as soon as the recipient's demand is backed.
		a.lastIdle = a.owner
	}
	a.owner, a.work, a.waiting, a.granted = owner, work, false, false
	a.generation++
}

func (e discoveryEntry) eligible() bool {
	return e.possible && (e.ceiling <= 0 || e.active < e.ceiling)
}

// observe changes priorities without touching any listener's leases.
func (a *discoveryArbiter) observe(label string, active, held int, demand bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	e := a.entries[label]
	e.active, e.held, e.demand = active, held, demand
	a.entries[label] = e
	a.choose()
}

func (a *discoveryArbiter) permits(label string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.owner == label
}

// exchanged spends a turn after its poll or pre-poll pool attempt is handled.
func (a *discoveryArbiter) exchanged(label string, generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if generation == 0 || a.owner != label || generation != a.generation {
		return
	}
	if a.work {
		a.lastWork = label
	} else {
		a.lastIdle = label
	}
	a.owner = ""
	a.choose()
}

// observeDemand counts obligations as active even while their ledger phase is
// capacity. Available work is a priority signal, never backing for an assignment.
func (l *Listener) observeDemand(observed *Statistics) {
	if l.arbiter == nil {
		return
	}
	active := l.committedCapacity()
	if observed != l.demandObservation {
		l.demandObservation = observed
		l.claimedSinceStats = 0
	}
	demand := len(l.waitingOffers) > 0
	if observed != nil {
		demand = demand || observed.TotalAssignedJobs > active ||
			observed.TotalAvailableJobs > l.claimedSinceStats
	}
	l.arbiter.observe(l.tier, active, l.idleEscrow(), demand)
}

// arbitrateEscrow serializes the grant with the ledger purchase. A donor cannot
// pass a stale permission check and take the capacity its successor is awaiting.
func (l *Listener) arbitrateEscrow(ctx context.Context, target int) error {
	a := l.arbiter
	possible, err := l.alloc.PotentialCapacity(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	for label, e := range a.entries {
		e.possible = possible[label] > 0
		a.entries[label] = e
	}
	a.choose()
	if a.owner != l.tier || a.granted {
		return nil
	}
	// WITHDRAW OLD IDLE BACKING BEFORE BUYING A NEW PLACEMENT. Otherwise an
	// unused label holding the preferred local host can force this turn onto
	// cloud fallback. Acquiring and running leases are not idle backing.
	for label, e := range a.entries {
		if label != l.tier && e.held > 0 {
			return nil
		}
	}

	// THE FIRST ESCROW ATTEMPT FIXES THE TURN AMONG CURRENTLY KNOWN CONTENDERS.
	// Before this boundary their sorted order wins over observation arrival;
	// afterwards a large winner keeps its place while headroom returns.
	a.waiting = true

	// ONE NEW LEASE PER TURN bounds a small tier's burst ahead of a large one.
	// Existing held backing can serve the turn without a second purchase.
	before := l.capacity()
	if err := l.refillEscrowUngated(ctx, min(target, l.committedCapacity()+1), 1); err != nil {
		return err
	}
	e := a.entries[l.tier]
	e.held = l.idleEscrow()
	a.entries[l.tier] = e
	if l.capacity() > before || e.held > 0 {
		a.granted = true
	}

	return nil
}

// reconcileAdmissionPool spends a turn consumed before the next poll. A refused
// launch can leave no lease behind, but its grant still forbids a second purchase
// until returned. The next poll needs backing from its own admission turn.
func (l *Listener) reconcileAdmissionPool(ctx context.Context, desired int) error {
	_, turn := l.admissionPoll()
	consumed, err := l.reconcilePool(ctx, desired)
	if err != nil {
		return err
	}
	if turn == 0 {
		return nil
	}

	// A RECONCILIATION THAT TOOK NO HELD BACKING KEEPS ITS TURN AND ITS GRANT.
	// Zero idle escrow cannot say where the grant's backing went: the heartbeat
	// may have dropped it, or a partly handled message may have moved it into an
	// acquisition promise that is still live. Clearing the grant on that reading
	// bought a second live lease in one turn past waiting peers. So nothing is
	// returned or cleared here; if the backing really was lost, the next handled
	// exchange ends the turn, which costs this tier one turn in a rare race and
	// never stalls it.
	if !consumed {
		return nil
	}

	// RETURN ONLY THE CAPTURED TURN. Demand can change the owner during a launch;
	// its completion must never spend the successor's grant.
	l.finishAdmissionTurn(turn)
	if l.isQuiesced() {
		return nil
	}

	return l.prepareEscrow(ctx)
}

func (l *Listener) finishAdmissionTurn(turn uint64) {
	if l.arbiter != nil {
		l.arbiter.exchanged(l.tier, turn)
		l.observeDemand(l.observed)
	}
}

// rememberAvailable retains a refused offer as known demand until it is accepted,
// assigned, completed, or replaced by a source snapshot reporting no available
// work. A timer never turns silence into proof that the queue is empty.
func (l *Listener) rememberAvailable(msg *Message) {
	if l.arbiter == nil {
		return
	}
	if msg.Statistics != nil && msg.Statistics.TotalAvailableJobs == 0 {
		clear(l.waitingOffers)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range msg.Available {
		// A REPEATED OFFER DOES NOT CREATE A SECOND BACKLOG ENTRY. reserve
		// skips existing commitments, so nobody would remove this hint later.
		if l.acquiring[msg.Available[i].RequestID] != nil || l.running[msg.Available[i].RequestID] != nil {
			continue
		}
		l.waitingOffers[identityOfOffer(msg.Available[i])] = true
	}
	for i := range msg.Assigned {
		delete(l.waitingOffers, identityOfOffer(msg.Assigned[i]))
	}
	for i := range msg.Completed {
		delete(l.waitingOffers, identityOfOffer(msg.Completed[i]))
	}
}

// admissionPoll fixes the advertisement and its turn as one decision. Demand
// observed after this point belongs to the outstanding poll's withdrawal window;
// it can stop further escrow but cannot revoke the backing already in that poll.
func (l *Listener) admissionPoll() (int, uint64) {
	if l.arbiter == nil {
		return l.advertisedCapacity(), 0
	}
	a := l.arbiter
	a.mu.Lock()
	defer a.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()

	active := len(l.acquiring) + len(l.running) + len(l.adopted)
	total := active + len(l.held) + len(l.releasing)
	target := active
	var turn uint64
	if a.owner == l.tier {
		target++
		if a.granted {
			turn = a.generation
		}
	}
	if l.maxCapacity != nil {
		target = min(target, *l.maxCapacity)
	}

	return max(min(total, target), 0), turn
}
