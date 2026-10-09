//go:build linux

package flows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/ti-mo/conntrack"
	"github.com/ti-mo/netfilter"
)

// readBuffer is the event socket's receive buffer. Destructions arrive in
// bursts (a job's build tearing down hundreds of connections at once), and a
// buffer the kernel fills is events it drops; the setting is capped by
// net.core.rmem_max, and a smaller one is still a working listener.
const readBuffer = 8 << 20

// THE SENTINEL. Sync proves the listener has read every destruction up to a
// moment by creating an entry of its own and deleting it: the kernel reports
// destructions to the listener in order, so seeing the sentinel's means every
// earlier one has been read. It lives on loopback addresses no guest uses,
// carries a mark of its own, and exists for the instant between the two calls.
var (
	sentinelSource = netip.MustParseAddr("127.66.0.1")
	sentinelDest   = netip.MustParseAddr("127.66.0.2")
)

const (
	sentinelMark    = 0xb111e7
	sentinelTimeout = 30 // seconds; the entry is deleted at once
)

// Tracker reads the kernel's connection tracker: destructions as they happen,
// and the whole table on demand.
type Tracker struct {
	log *slog.Logger

	mu        sync.Mutex
	listening bool
	nextPort  uint16
	waiters   map[uint16]chan struct{}
}

// NewTracker returns a Tracker that logs to log.
func NewTracker(log *slog.Logger) *Tracker {
	return &Tracker{log: log, waiters: map[uint16]chan struct{}{}, nextPort: 1}
}

// Run reports every destroyed flow to a until ctx ends. The kernel drops
// events it cannot deliver; while the listener is not reading, for that or
// any other reason, a is told the events are down, and every job whose time
// overlaps the gap is marked incomplete.
func (t *Tracker) Run(ctx context.Context, a *Accountant) {
	a.Down()

	backoff := time.Second

	for ctx.Err() == nil {
		err := t.listen(ctx, a)

		t.setListening(false)
		a.Down()

		if ctx.Err() != nil {
			return
		}

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

func (t *Tracker) setListening(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.listening = on
}

func (t *Tracker) listen(ctx context.Context, a *Accountant) error {
	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("open the connection tracker: %w", err)
	}

	if err := conn.SetReadBuffer(readBuffer); err != nil {
		t.log.Debug("could not raise the connection tracker's read buffer", "error", err)
	}

	events := make(chan conntrack.Event, 1024)

	errs, err := conn.Listen(events, 1, []netfilter.NetlinkGroup{netfilter.GroupCTDestroy})
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("listen for destroyed flows: %w", err)
	}

	// CLOSED WHILE THE CHANNELS ARE DRAINED: the library's worker blocks
	// sending on them, and Close waits for the worker.
	defer closeDraining(conn, events, errs)

	t.setListening(true)
	a.Up(time.Now())

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

			if t.sentinel(ev.Flow) {
				continue
			}

			if f, ok := fromConntrack(ev.Flow); ok {
				a.Observe(f)
			}
		}
	}
}

func closeDraining(conn *conntrack.Conn, events <-chan conntrack.Event, errs <-chan error) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = conn.Close()
	}()

	for {
		select {
		case <-done:
			return
		case <-events:
		case <-errs:
		}
	}
}

// sentinel reports whether f is a Sync's sentinel, waking its waiter.
func (t *Tracker) sentinel(f *conntrack.Flow) bool {
	if f.Mark != sentinelMark || f.TupleOrig.IP.SourceAddress.Unmap() != sentinelSource {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if w, ok := t.waiters[f.TupleOrig.Proto.SourcePort]; ok {
		close(w)
		delete(t.waiters, f.TupleOrig.Proto.SourcePort)
	}

	return true
}

// Sync returns once the listener has read every destruction the kernel
// reported before the call, proved by a sentinel entry of its own, or with an
// error saying it could not prove it.
func (t *Tracker) Sync(ctx context.Context) error {
	t.mu.Lock()

	if !t.listening {
		t.mu.Unlock()

		return errors.New("flows: the connection-tracking listener is not reading")
	}

	port := t.nextPort

	t.nextPort++
	if t.nextPort == 0 {
		t.nextPort = 1
	}

	seen := make(chan struct{})
	t.waiters[port] = seen
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.waiters, port)
		t.mu.Unlock()
	}()

	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("flows: open the connection tracker: %w", err)
	}

	defer conn.Close()

	f := conntrack.NewFlow(17, 0, sentinelSource, sentinelDest, port, port, sentinelTimeout, sentinelMark)
	if err := conn.Create(f); err != nil {
		return fmt.Errorf("flows: create the sentinel entry: %w", err)
	}

	if err := conn.Delete(f); err != nil {
		return fmt.Errorf("flows: delete the sentinel entry: %w", err)
	}

	select {
	case <-seen:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("flows: the listener did not report the sentinel's destruction: %w", ctx.Err())
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
