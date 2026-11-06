package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/hostsource"
)

// DefaultJudgeEvery is how often one order's source is asked about it. A host asks
// every five seconds until it is answered, the console read is one EC2 throttles, and a
// caller chooses the key it presents, so the budget is the order's, not the key's: the
// console showed a line within 48 s of its writing (measured 2026-11-06), so a few
// seconds more is never the wait that matters.
const DefaultJudgeEvery = 15 * time.Second

// JudgeTimeout bounds one ask of a source: the enrollment listener answers within its
// ten-second write timeout, so a source slower than this is could-not-tell, answered in
// time, rather than a request that never gets an answer.
const JudgeTimeout = 5 * time.Second

// JoinLedger is what the judge reads of the ledger.
type JoinLedger interface {
	ReadPoolOrder(ctx context.Context, id string) (alloc.PoolOrder, error)
}

// Joins judges a pool host's enrollment by asking its order's source (ADR-030).
type Joins struct {
	ledger  JoinLedger
	sources map[string]hostsource.Source
	now     func() time.Time
	every   time.Duration

	mu sync.Mutex
	// last is each claim's verdict and when it was asked; asked is when each order's
	// source was last asked, and asking the orders being asked now.
	last   map[judgedKey]judged
	asked  map[string]time.Time
	asking map[string]bool
}

// judgedKey is one claim on one order as the order stood: a hold or its release moves
// the order, so a verdict given before either is never reused after it.
type judgedKey struct {
	order, fingerprint, address string
	moved                       time.Time
}

type judged struct {
	at        time.Time
	testimony hostsource.Testimony
}

// NewJoins builds the judge over the sources the pool orders from.
func NewJoins(ledger JoinLedger, sources map[string]hostsource.Source, now func() time.Time) *Joins {
	if now == nil {
		now = time.Now
	}

	return &Joins{
		ledger: ledger, sources: sources, now: now, every: DefaultJudgeEvery,
		last: map[judgedKey]judged{}, asked: map[string]time.Time{}, asking: map[string]bool{},
	}
}

// JudgePoolJoin asks the claim's order's source whether the host it placed is the one
// asking, and answers could-not-tell for an order with no host yet, a source no longer
// configured, or one with no channel to its hosts (phoenixNAP), whose hosts wait for a
// person.
//
// AN ORDER'S SOURCE IS ASKED AT MOST ONCE PER DefaultJudgeEvery, AND ONE ASK AT A TIME,
// whatever key a caller presents: a claim asked within it gets its own verdict back,
// unless the order has moved since (a hold, or its release), and any other claim on the
// order is could-not-tell until the interval passes.
func (j *Joins) JudgePoolJoin(ctx context.Context, claim hostsource.Claim) hostsource.Testimony {
	unknown := func(reason string) hostsource.Testimony {
		return hostsource.Testimony{Verdict: hostsource.VerdictUnknown, Reason: reason}
	}
	o, err := j.ledger.ReadPoolOrder(ctx, claim.Order)
	switch {
	case errors.Is(err, alloc.ErrPoolOrderClosed):
		return unknown("no such pool order")
	case err != nil:
		return unknown("the order could not be read")
	}
	key := judgedKey{order: claim.Order, fingerprint: claim.Fingerprint, address: claim.Address, moved: o.Updated}
	now := j.now()

	j.mu.Lock()
	for k, v := range j.last {
		if now.Sub(v.at) >= j.every {
			delete(j.last, k)
		}
	}
	for order, at := range j.asked {
		if now.Sub(at) >= j.every {
			delete(j.asked, order)
		}
	}
	if v, ok := j.last[key]; ok {
		j.mu.Unlock()

		return v.testimony
	}
	if _, recently := j.asked[claim.Order]; recently || j.asking[claim.Order] {
		j.mu.Unlock()

		return hostsource.Testimony{Verdict: hostsource.VerdictUnknown,
			Reason: "the order's source was asked about it moments ago; it is asked again shortly"}
	}
	j.asking[claim.Order], j.asked[claim.Order] = true, now
	j.mu.Unlock()

	askCtx, cancel := context.WithTimeout(ctx, JudgeTimeout)
	t := j.judge(askCtx, o, claim)
	cancel()

	j.mu.Lock()
	delete(j.asking, claim.Order)
	j.last[key] = judged{at: now, testimony: t}
	j.mu.Unlock()

	return t
}

func (j *Joins) judge(ctx context.Context, o alloc.PoolOrder, claim hostsource.Claim) hostsource.Testimony {
	unknown := func(reason string) hostsource.Testimony {
		return hostsource.Testimony{Verdict: hostsource.VerdictUnknown, Reason: reason}
	}
	if o.HostID == "" {
		return unknown("the order's host is not recorded yet")
	}
	src := j.sources[o.Source]
	if src == nil {
		return unknown("the order's source " + o.Source + " is not configured")
	}
	w, ok := src.(hostsource.Witness)
	if !ok {
		return unknown("source " + o.Source + " has no channel to its hosts, so a person compares the fingerprint")
	}
	claim.HostID = o.HostID

	return w.Witness(ctx, claim)
}
