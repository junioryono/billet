package server

import (
	"context"
	"slices"

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
