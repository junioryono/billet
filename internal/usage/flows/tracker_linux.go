//go:build linux

package flows

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/ti-mo/conntrack"
	"github.com/ti-mo/netfilter"
)

// readBuffer is the event socket's receive buffer. Destructions arrive in
// bursts (a job's build tearing down hundreds of connections at once), and a
// buffer the kernel fills is events it drops; the setting is capped by
// net.core.rmem_max, and a smaller one is still a working listener.
const readBuffer = 8 << 20

// Tracker reads the kernel's connection tracker: destructions as they happen,
// and the whole table on demand.
type Tracker struct {
	log *slog.Logger
}

// NewTracker returns a Tracker that logs to log.
func NewTracker(log *slog.Logger) *Tracker { return &Tracker{log: log} }

// Run reports every destroyed flow to a until ctx ends. The kernel drops
// events it cannot deliver; when the listener fails for that or any other
// reason, every job watched at that moment is marked incomplete and the
// listener is opened again, so a lost window costs the jobs in it their
// completeness and nothing more.
func (t *Tracker) Run(ctx context.Context, a *Accountant) {
	backoff := time.Second

	for ctx.Err() == nil {
		err := t.listen(ctx, a)
		if ctx.Err() != nil {
			return
		}

		a.Lost()
		t.log.Warn("the connection-tracking listener stopped; jobs running now may be missing "+
			"flows, and are marked incomplete", "error", err, "retry_in", backoff)

		wait := time.NewTimer(backoff)

		select {
		case <-ctx.Done():
			wait.Stop()

			return
		case <-wait.C:
		}

		backoff = min(2*backoff, time.Minute)
	}
}

func (t *Tracker) listen(ctx context.Context, a *Accountant) error {
	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("open the connection tracker: %w", err)
	}

	defer conn.Close()

	if err := conn.SetReadBuffer(readBuffer); err != nil {
		t.log.Debug("could not raise the connection tracker's read buffer", "error", err)
	}

	events := make(chan conntrack.Event, 1024)

	errs, err := conn.Listen(events, 1, []netfilter.NetlinkGroup{netfilter.GroupCTDestroy})
	if err != nil {
		return fmt.Errorf("listen for destroyed flows: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		case ev := <-events:
			if ev.Type != conntrack.EventDestroy || ev.Flow == nil {
				continue
			}

			if f, ok := fromConntrack(ev.Flow); ok {
				a.Observe(f)
			}
		}
	}
}

// Flows returns every flow the tracker holds now whose original source is
// addr.
func (t *Tracker) Flows(addr netip.Addr) ([]Flow, error) {
	conn, err := conntrack.Dial(nil)
	if err != nil {
		return nil, fmt.Errorf("open the connection tracker: %w", err)
	}

	defer conn.Close()

	all, err := conn.Dump(nil)
	if err != nil {
		return nil, fmt.Errorf("read the connection tracker: %w", err)
	}

	addr = addr.Unmap()

	var out []Flow

	for i := range all {
		f, ok := fromConntrack(&all[i])
		if ok && f.Source == addr {
			out = append(out, f)
		}
	}

	return out, nil
}

// fromConntrack reads the fields billet counts. A flow without both addresses
// of its original tuple is not one a destination can be named for.
func fromConntrack(cf *conntrack.Flow) (Flow, bool) {
	src, dst := cf.TupleOrig.IP.SourceAddress.Unmap(), cf.TupleOrig.IP.DestinationAddress.Unmap()
	if !src.IsValid() || !dst.IsValid() {
		return Flow{}, false
	}

	return Flow{
		ID:           cf.ID,
		Start:        cf.Timestamp.Start,
		Protocol:     cf.TupleOrig.Proto.Protocol,
		Source:       src,
		Dest:         dst,
		DestPort:     cf.TupleOrig.Proto.DestinationPort,
		OrigBytes:    cf.CountersOrig.Bytes,
		ReplyBytes:   cf.CountersReply.Bytes,
		OrigPackets:  cf.CountersOrig.Packets,
		ReplyPackets: cf.CountersReply.Packets,
	}, true
}
