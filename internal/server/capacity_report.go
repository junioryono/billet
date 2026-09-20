package server

import (
	"context"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// reportCapacity keeps observation writes in ownership order under mu. Reporting
// failure is audible and changes no scheduling decision or lease ownership.
// THE WRITE IS BOUNDED so a failed observation cannot stall renewal indefinitely.
func (l *Listener) reportCapacity(ctx context.Context, sent *int, exchange string) {
	if l.alloc == nil {
		return
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

	// THE QUEUE'S OWN RECORD OF WHEN THIS TIER BEGAN WAITING, not a fresh
	// timestamp: the point of the field is how long the oldest work has been
	// waiting, and a value stamped at each observation would always read as
	// "just now".
	if since, ok := l.order.waitingSince(l.tier); ok {
		report.WaitingSince = since.UTC().Format(time.RFC3339Nano)
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
	if err := l.alloc.RecordListenerCapacity(ctx, l.tier, report); err != nil {
		l.log.Warn("could not report listener capacity", "tier", l.tier, "error", err)
	}
}
