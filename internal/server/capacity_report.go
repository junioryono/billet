package server

import (
	"context"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// reportCapacity keeps observation writes in ownership order under mu. Reporting
// failure is audible and changes no scheduling decision or lease ownership.
func (l *Listener) reportCapacity(ctx context.Context, sent *int, exchange string) {
	if err := l.publishCapacity(ctx, sent, exchange); err != nil {
		l.log.Warn("could not report listener capacity", "tier", l.tier, "error", err)
	}
}

// publishCapacity is reportCapacity for a caller whose next step depends on the
// observation having reached the ledger. THE WRITE IS BOUNDED so a failed
// observation cannot stall renewal indefinitely.
func (l *Listener) publishCapacity(ctx context.Context, sent *int, exchange string) error {
	if l.alloc == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.releaseGrace)
	defer cancel()

	l.mu.Lock()
	defer l.mu.Unlock()

	if sent != nil {
		value := *sent
		l.capacitySent = &value
		if exchange == "confirmed" {
			l.capacityConfirmed = &value
		}
	}
	if exchange != "" {
		l.capacityExchange = exchange
	}
	report := alloc.ListenerCapacity{
		Sent: l.capacitySent, Confirmed: l.capacityConfirmed, Exchange: l.capacityExchange,
		Waiting: l.waitingFor,
	}

	// GITHUB'S DEMAND AS COUNTED, whatever the order made of it: a drain reads
	// this to say whether assigned work is still waiting (#140).
	if l.demandKnown {
		unstarted, releases := l.waitingFor, l.demandReleases
		report.Unstarted, report.UnstartedReleases = &unstarted, &releases

		// UNDATED UNTIL GITHUB HAS VOUCHED FOR IT under a withdrawn
		// advertisement, which a drain reads as a count from before its seal.
		if !l.demandAt.IsZero() {
			report.UnstartedAt = l.demandAt.UTC().Format(time.RFC3339Nano)
		}
	}

	// THE QUEUE'S OWN RECORD OF WHEN THIS TIER BEGAN WAITING, not a fresh
	// timestamp: the point of the field is how long the oldest work has been
	// waiting, and a value stamped at each observation would always read as
	// "just now".
	if since, progress, ok := l.order.waitingSince(l.tier); ok {
		report.WaitingSince = since.UTC().Format(time.RFC3339Nano)
		report.WaitingProgress = progress.UTC().Format(time.RFC3339Nano)

		// WHO HOLDS THE LINE, asked of the queue now rather than remembered from
		// the refusal, which may be a poll old (#248).
		if holder, holderSince, held := l.order.waiterHeldBy(l.tier); held {
			report.HeldBy = holder
			report.HeldBySince = holderSince.UTC().Format(time.RFC3339Nano)
		}
	}

	// AND WHAT THE ALLOCATOR SAID, which the queue does not know, for any tier
	// with demand it could not buy for, in the queue or not: a tier at its own
	// cap or with no host never waits in line, and is exactly the one whose
	// reason an operator cannot otherwise find.
	if l.waitingFor > 0 && (l.refused.room != nil || l.refused.unread != nil) && !l.refused.byOrder() {
		report.Refusal = l.refused.String()
	}

	// A COUNT WITHOUT A DATE IS NOT A WAIT. The two are written by different
	// parties — the count by this tier's own reconciliation, the date by the
	// queue every listener shares — so a tier the queue has already released
	// reports neither rather than a queue that no longer exists.
	if report.WaitingSince == "" {
		report.Waiting = 0
	}
	for _, lease := range l.held {
		report.Discovery = append(report.Discovery, lease.ID)
	}
	for _, lease := range l.releasing {
		report.Discovery = append(report.Discovery, lease.ID)
	}
	for _, p := range l.acquiring {
		if p.lease != nil {
			report.Pending = append(report.Pending, p.lease.ID)
		}
	}
	slices.Sort(report.Discovery)
	slices.Sort(report.Pending)
	return l.alloc.RecordListenerCapacity(ctx, l.tier, report)
}
