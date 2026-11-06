package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/hostsource"
)

// DefaultJudgeEvery is how often one order's join is asked of its source. A host asks
// every five seconds until it is answered, and the console read is one EC2 throttles,
// so a verdict is reused for this long; the console showed a line within 48 s of its
// writing (measured 2026-11-06), so a minute is never the wait that matters.
const DefaultJudgeEvery = 15 * time.Second

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

	mu   sync.Mutex
	last map[judgedKey]judged
}

type judgedKey struct{ order, fingerprint, address string }

type judged struct {
	at        time.Time
	testimony hostsource.Testimony
}

// NewJoins builds the judge over the sources the pool orders from.
func NewJoins(ledger JoinLedger, sources map[string]hostsource.Source, now func() time.Time) *Joins {
	if now == nil {
		now = time.Now
	}

	return &Joins{ledger: ledger, sources: sources, now: now, every: DefaultJudgeEvery, last: map[judgedKey]judged{}}
}

// JudgePoolJoin asks the claim's order's source whether the host it placed is the one
// asking, and answers could-not-tell for an order with no host yet, a source no longer
// configured, or one with no channel to its hosts (phoenixNAP), whose hosts wait for a
// person. One claim is asked at most once per DefaultJudgeEvery.
func (j *Joins) JudgePoolJoin(ctx context.Context, claim hostsource.Claim) hostsource.Testimony {
	key := judgedKey{claim.Order, claim.Fingerprint, claim.Address}
	now := j.now()
	j.mu.Lock()
	for k, v := range j.last {
		if now.Sub(v.at) >= j.every {
			delete(j.last, k)
		}
	}
	if v, ok := j.last[key]; ok {
		j.mu.Unlock()

		return v.testimony
	}
	j.mu.Unlock()

	t := j.judge(ctx, claim)

	j.mu.Lock()
	j.last[key] = judged{at: now, testimony: t}
	j.mu.Unlock()

	return t
}

func (j *Joins) judge(ctx context.Context, claim hostsource.Claim) hostsource.Testimony {
	unknown := func(reason string) hostsource.Testimony {
		return hostsource.Testimony{Verdict: hostsource.VerdictUnknown, Reason: reason}
	}
	o, err := j.ledger.ReadPoolOrder(ctx, claim.Order)
	switch {
	case errors.Is(err, alloc.ErrPoolOrderClosed):
		return unknown("no such pool order")
	case err != nil:
		return unknown("the order could not be read: " + err.Error())
	case o.HostID == "":
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
