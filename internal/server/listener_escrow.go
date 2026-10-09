package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
)

// promise is escrow held for a request GitHub has been told billet will run, and
// when that undertaking was made.
//
// `at` IS DIAGNOSTIC ONLY — it drives one stale-promise warning and must not time
// the promise out; see defaultStalePromise.
type promise struct {
	job    dispatch.Job
	actual actualJobIdentity
	lease  *alloc.Lease
	at     time.Time
	// reported keeps a stale promise from logging on every heartbeat.
	reported bool
}

// refillEscrow buys as much escrow as the fleet has room for, up to a configured
// maxCapacity. Nothing in production calls it: capacity is bought per offer in
// handle and per launch in backPoolSlot (#140). It stages a listener holding
// escrow for a test.
func (l *Listener) refillEscrow(ctx context.Context) error {
	return l.refillEscrowTo(ctx, math.MaxInt)
}

// refillEscrowTo tops escrow up to target, as far as the allocator has room.
func (l *Listener) refillEscrowTo(ctx context.Context, target int) error {
	return l.refillEscrowUngated(ctx, target, math.MaxInt)
}

func (l *Listener) refillEscrowUngated(ctx context.Context, target, maxNew int) error {
	return l.refillEscrowLeaving(ctx, target, maxNew, nil)
}

// refillEscrowLeaving is refillEscrowUngated for a purchase made ahead of this
// tier's turn, which must leave room for each waiter in protect (see
// alloc.EscrowLeaving). PROTECT IS AN ARGUMENT AND NOT LISTENER STATE: the
// escrow takes no l.mu on the way, so a heartbeat pass holding the mutex never
// delays a purchase (TestALeaseNeverConfirmedIsStillBoundedByItsTTL).
func (l *Listener) refillEscrowLeaving(ctx context.Context, target, maxNew int, protect []string) error {
	if l.beforeEscrowRefill != nil {
		l.beforeEscrowRefill()
	}
	room, err := l.alloc.Headroom(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: headroom for %s: %w", l.tier, err)
	}

	// Headroom already excludes what this listener holds — those leases are open
	// in the ledger — so this tops the total up rather than doubling it.
	if target != math.MaxInt {
		if ceiling := target - l.capacity(); room > ceiling {
			room = ceiling
		}
	}

	if l.maxCapacity != nil {
		// Capped BEFORE the escrow, not after. Escrowing capacity this listener
		// has promised not to advertise would hold it away from every other tier
		// for nothing.
		if ceiling := *l.maxCapacity - l.capacity(); room > ceiling {
			room = ceiling
		}
	}

	// ONE NEW LEASE PER ARBITRATED GRANT, even if a heartbeat removed committed
	// capacity after the target was computed. Standalone refills have no cap.
	room = min(room, maxNew)
	if room <= 0 {
		return nil
	}

	// STAMPED BEFORE THE CALL, which is the only place a second clock can be safely
	// wrong. Sampling after l.mu lets a slow heartbeat pass hold the mutex and date a
	// lease TTL/3 late; sampling after Escrow returns is no better, because the
	// goroutine can be descheduled between the commit and the sample. Both errors run
	// in the same direction: a lease dated later than it really is stays advertisable
	// after the reaper could already have taken it.
	//
	// Taken beforehand, the error runs the other way — the lease is dated slightly
	// EARLIER than the allocator's own expiry basis, so the worst case is dropping one
	// billet still owns. That costs a re-escrow; the opposite costs two tiers the same
	// machine. The real fix is for Escrow to return the allocator's authoritative
	// expiry, filed with the rest of the lifecycle work.
	created := time.Now()

	leases, err := l.alloc.EscrowLeaving(ctx, l.tier, room, protect)

	// A SEALED DEPLOYMENT IS NOT A BROKEN ONE, and the difference is the whole
	// fleet. An escrow error returns from the listener, one listener returning
	// cancels every other listener, and their teardown destroys the compute they
	// are holding — so surfacing a deliberate seal as an ordinary error would
	// make `drain` the most destructive command billet has, killing exactly the
	// jobs it exists to protect.
	//
	// Having nothing to escrow is the correct reading of a seal anyway: the
	// listener advertises nothing new and carries on heartbeating, settling
	// completions and tearing down what finishes, which is what draining IS.
	if errors.Is(err, alloc.ErrAdmissionSealed) {
		l.log.Info("not escrowing: this deployment is not accepting new work",
			"tier", l.tier, "reason", err)

		return nil
	}

	if err != nil {
		return fmt.Errorf("server: escrow for %s: %w", l.tier, err)
	}

	l.mu.Lock()

	// A POOL PASS IS RE-DATED ONCE, after its batch, not after each purchase in
	// it: re-dated per purchase, the tier lost its place after the first member
	// and every batch shrank to one, so a tier bought one runner each time its
	// listener woke however much room was free (2026-10-03).
	if len(leases) > 0 {
		if l.batchBuying {
			l.batchBought = true
		} else {
			l.mu.Unlock()
			l.order.bought(l.tier)
			l.mu.Lock()
		}
	}

	l.trackHeld(leases)
	l.held = append(l.held, leases...)
	l.sortHeld()

	// THE UNCERTAINTY CLOCK STARTS HERE, at the only point a lease id enters this
	// listener at all — everything after this moves leases between held,
	// acquiring and running, so they are already tracked.
	//
	// Starting it at the first FAILED renewal instead would hand a never-confirmed
	// lease an extra TTL it had not earned: escrowed at t=0 and expiring at t=TTL,
	// a lease whose first heartbeat failed at t=TTL/3 would have its clock set
	// there and stay advertised past t=4TTL/3, long after the reaper could have
	// taken it.
	for _, lease := range leases {
		l.confirmed[lease.ID] = created
	}

	l.mu.Unlock()

	return nil
}

// acquire claims the offers this listener has escrow to back, reserving that
// escrow first.
//
// An acquisition is a PROMISE to run the job, so the lease is moved out of held
// and bound to the request id BEFORE the network call. Checking a count and
// leaving the leases where they were is what allowed one lease to back two
// promises, and what let the heartbeat spend a lease out from under an
// acquisition already in flight.
//
// Claiming fewer offers than were made is normal and not a loss: an unacquired
// offer goes to another scale set or is re-offered, whereas an acquisition
// billet cannot back is a job that goes nowhere at all.
func (l *Listener) acquire(ctx context.Context, available []dispatch.Job) error {
	resolved, err := l.resolveMessage(ctx, &Message{Available: available})
	if err != nil {
		return err
	}
	return l.acquireUnfinished(ctx, resolved.available, nil, nil, resolved.committed)
}

// acquireUnfinished uses resolveActualJob's rule for completion and commitment
// filtering. Wire request IDs are retained only for the AcquireJobs protocol.
func (l *Listener) acquireUnfinished(ctx context.Context, available []resolvedJob,
	finished, held []actualJobIdentity, commitments []jobCommitment,
) error {
	if len(available) == 0 {
		return nil
	}

	committed := l.currentCommitments(commitments)
	eligible := make([]resolvedJob, 0, len(available))
	for i := range available {
		entry := &available[i]
		if containsActual(held, entry.actual) {
			continue
		}
		if containsActual(finished, entry.actual) || containsActual(committed, entry.actual) {
			continue
		}
		eligible = append(eligible, *entry)
	}
	protocolFor := make(map[int64]int64, len(available))
	internalFor := make(map[int64]int64, len(available))
	for i := range eligible {
		entry := &eligible[i]
		protocolID := entry.protocolID
		job := entry.job
		protocolFor[job.RequestID] = protocolID
		if prior, duplicate := internalFor[protocolID]; duplicate && prior != job.RequestID {
			return fmt.Errorf("%w: %s offered distinct jobs %d and %d under the same runner request id %d; the acquisition response cannot distinguish them",
				ErrUntrustworthySession, l.tier, prior, job.RequestID, protocolID)
		}
		internalFor[protocolID] = job.RequestID
	}
	// Validate every wire offer before coalescing acquisition representatives.
	identified := make([]resolvedJob, 0, len(eligible))
	for e := range eligible {
		entry := &eligible[e]
		if i := slices.IndexFunc(identified, func(prior resolvedJob) bool {
			return sameActualJob(prior.actual, entry.actual)
		}); i >= 0 {
			identified[i].actual = mergeActual(identified[i].actual, entry.actual)
			continue
		}
		identified = append(identified, *entry)
	}
	for i := range identified {
		protocolFor[identified[i].job.RequestID] = identified[i].protocolID
	}

	reservedInternal := l.reserve(identified)
	l.reportCapacity(ctx, nil, "")
	if len(reservedInternal) == 0 {
		return nil
	}
	reservedProtocol := make([]int64, 0, len(reservedInternal))
	for _, internalID := range reservedInternal {
		protocolID := protocolFor[internalID]
		reservedProtocol = append(reservedProtocol, protocolID)
	}

	acquiredProtocol, err := l.session.AcquireJobs(ctx, reservedProtocol)
	if err != nil {
		// A FAILED RESPONSE IS NOT A REFUSED ACQUISITION. The request may have
		// committed remotely; withdrawal must not donate its backing to a peer.

		return fmt.Errorf("server: acquire jobs for %s: %w", l.tier, err)
	}

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked for —
	// another scale set can win the same offer. Escrow reserved for an offer billet
	// did not get goes back immediately.
	//
	// A response that is not a subset of the request means billet cannot tell what it
	// has committed to: an id nobody offered for has no reservation to match, and a
	// body wrong about that id may be wrong about the reserved ids too.
	//
	// This STOPS the listener, unlike an unbacked assignment, which declines and
	// carries on. An unbacked assignment is reachable by ordinary races — a heartbeat
	// drops a fenced lease, a restart loses a promise — so killing the control plane
	// over one is disproportionate. A response outside its own contract is not
	// reachable by any race, and stopping is also the remedy: the session is recreated
	// and GitHub redelivers whatever was unacknowledged.
	if extra := missing(acquiredProtocol, reservedProtocol); len(extra) > 0 {
		// KEEP EVERY PROMISE until the session closes. Which commitments are
		// real is exactly what this response failed to establish.

		return fmt.Errorf("%w: %s acquired job requests it did not offer for "+
			"(unrequested %v, requested %v); refusing to continue against a scale-set "+
			"response that is not a subset of its request",
			ErrUntrustworthySession, l.tier, extra, reservedProtocol)
	}

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked
	// for — another scale set can win the same offer. Escrow reserved for an
	// offer billet did not get goes back immediately; holding it would strand
	// capacity waiting for an assignment that is never coming.
	acquiredInternal := make([]int64, 0, len(acquiredProtocol))
	for _, protocolID := range acquiredProtocol {
		acquiredInternal = append(acquiredInternal, internalFor[protocolID])
	}
	l.unreserve(missing(reservedInternal, acquiredInternal))

	return nil
}

// reserve moves escrow from held into acquiring, one lease per offer, and
// returns the request ids it could back.
func (l *Listener) reserve(available []resolvedJob) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	ids := make([]int64, 0, len(available))

	for i := range available {
		job := &available[i].job
		// Already promised, and therefore NOT returned to the caller.
		//
		// Returning it looked harmless and was not: the caller unreserves whatever
		// GitHub does not grant, so a re-offer of a request billet had already
		// acquired would tear down the earlier, successful promise the moment the
		// second acquisition came back partial. The lease then backed the next
		// offer instead, and the assignment for the original request found
		// nothing. unreserve may only undo reservations THIS call created.
		if _, ok := l.acquiring[job.RequestID]; ok {
			continue
		}

		if _, ok := l.running[job.RequestID]; ok {
			continue
		}

		// AND NOT WHILE ITS LAST CONTAINER IS STILL OWED. A request whose compute
		// this listener has not managed to destroy is not a free request id: the
		// cleanup retry addresses a Destroy BY REQUEST ID, so taking the same id
		// again would give the old retry the power to destroy the new job's
		// container and release the new job's lease. Request id alone cannot tell
		// two incarnations apart, so the id stays occupied until the first one is
		// discharged.
		if entry, ok := l.cleanup[job.RequestID]; ok {
			// SAID ONCE. GitHub re-offers a job nobody acquires for as long as it is
			// queued, so warning per offer turns one stuck obligation into a line
			// every poll for as long as the node stays away.
			if !entry.declined {
				entry.declined = true

				l.log.Warn("declining a job whose previous run left compute billet has not "+
					"managed to destroy; it stays queued until that is cleaned up, and this "+
					"is reported once rather than on every offer",
					"tier", l.tier, "request", job.RequestID)
			}

			continue
		}

		if len(l.held) == 0 {
			l.log.Warn("declining an offer with no escrow to back it",
				"tier", l.tier, "request", job.RequestID)

			continue
		}

		l.acquiring[job.RequestID] = &promise{lease: l.held[0], at: time.Now(), job: *job, actual: available[i].actual}
		l.held = l.held[1:]

		ids = append(ids, job.RequestID)
	}

	return ids
}

// unreserve returns promised escrow to held.
func (l *Listener) unreserve(ids []int64) {
	if len(ids) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, id := range ids {
		if p, ok := l.acquiring[id]; ok {
			delete(l.acquiring, id)
			l.held = append(l.held, p.lease)
		}
	}
	l.sortHeld()
}

// releasePromise returns request-scoped escrow once an existing anonymous pool
// member already backs the assignment. Keeping it would reserve a second
// machine for one desired runner.
func (l *Listener) releasePromise(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if promised, ok := l.acquiring[id]; ok {
		delete(l.acquiring, id)
		l.held = append(l.held, promised.lease)
		l.sortHeld()
	}
}

func (l *Listener) trackHeld(leases []*alloc.Lease) {
	for _, lease := range leases {
		if _, tracked := l.heldOrder[lease.ID]; tracked {
			continue
		}
		l.nextHeldOrder++
		l.heldOrder[lease.ID] = l.nextHeldOrder
	}
}

// sortHeld keeps provider preference ahead of fallback placement, then restores
// the allocator's issuance order within a provider after unreserve appends.
func (l *Listener) sortHeld() {
	slices.SortStableFunc(l.held, func(a, b *alloc.Lease) int {
		if rank := cmp.Compare(a.PreferenceRank, b.PreferenceRank); rank != 0 {
			return rank
		}
		return cmp.Compare(l.heldOrder[a.ID], l.heldOrder[b.ID])
	})
}

// missing returns the ids that were asked for and not granted.
func missing(asked, granted []int64) []int64 {
	if len(granted) == 0 {
		return asked
	}

	got := make(map[int64]struct{}, len(granted))
	for _, id := range granted {
		got[id] = struct{}{}
	}

	var lost []int64

	for _, id := range asked {
		if _, ok := got[id]; !ok {
			lost = append(lost, id)
		}
	}

	return lost
}
