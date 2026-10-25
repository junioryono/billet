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
	sentinelMark     = 0xb111e7
	sentinelProtocol = 17 // udp
	sentinelTimeout  = 30 // seconds; the entry is deleted at once
	// closeLimit bounds the wait for a netlink call to return once its
	// connection was closed under it.
	closeLimit = time.Second
)

// sentinelID is one Sync's sentinel: a 32-bit nonce carried in its two ports,
// so a destruction from an earlier Sync is never taken for a later one's.
type sentinelID struct{ sport, dport uint16 }

// Tracker reads the kernel's connection tracker: destructions as they happen,
// and the whole table on demand.
type Tracker struct {
	log *slog.Logger

	mu        sync.Mutex
	listening bool
	nonce     uint32
	waiters   map[sentinelID]chan struct{}
	// repliesListening and replyWaiters are the reply listener's: a Sync
	// proves it read up to a moment by its reading the sentinel's new event.
	repliesListening bool
	replyWaiters     map[sentinelID]chan struct{}
}

// NewTracker returns a Tracker that logs to log.
func NewTracker(log *slog.Logger) *Tracker {
	return &Tracker{log: log, waiters: map[sentinelID]chan struct{}{},
		replyWaiters: map[sentinelID]chan struct{}{}}
}

// Run reports every destroyed flow to a until ctx ends. The kernel drops
// events it cannot deliver; while the listener is not reading, for that or
// any other reason, a is told the events are down, and every job whose time
// overlaps the gap is marked incomplete.
//
// BESIDE IT, ON A SOCKET OF ITS OWN, every new flow and every update that saw a
// reply, which time each flow's wait for its first answer. Its own socket, so
// the far busier stream (measured 2026-10-25: 20,000 connections in 2.4 s
// raised 20,000 new and 100,003 update events, against one destruction each)
// can fill and drop without marking a single byte total incomplete; a gap in
// it leaves the jobs it overlapped untimed instead.
func (t *Tracker) Run(ctx context.Context, a *Accountant) {
	var replies sync.WaitGroup

	replies.Add(1)

	go func() {
		defer replies.Done()

		t.runReplies(ctx, a)
	}()

	defer replies.Wait()

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

// runReplies reports new flows and first replies to a until ctx ends, saying
// the timing is down while it is not reading.
func (t *Tracker) runReplies(ctx context.Context, a *Accountant) {
	a.RepliesDown()

	backoff := time.Second

	for ctx.Err() == nil {
		err := t.listenReplies(ctx, a)

		a.RepliesDown()

		if ctx.Err() != nil {
			return
		}

		t.log.Warn("the connection-tracking listener for first replies stopped; jobs running now "+
			"report their destinations without reply times", "error", err, "retry_in", backoff)

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

// listenReplies reads one connection's new and update events. Each is timed
// as it is read: neither carries the entry's own timestamps or counters
// (measured 2026-10-25), so the wait is the time between reading a flow's new
// event and reading its first update that saw a reply, on one clock.
func (t *Tracker) listenReplies(ctx context.Context, a *Accountant) error {
	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("open the connection tracker: %w", err)
	}

	if err := conn.SetReadBuffer(readBuffer); err != nil {
		t.log.Debug("could not raise the connection tracker's read buffer for first replies", "error", err)
	}

	events := make(chan conntrack.Event, 1024)

	errs, err := conn.Listen(events, 1, []netfilter.NetlinkGroup{netfilter.GroupCTNew, netfilter.GroupCTUpdate})
	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("listen for new flows and their replies: %w", err)
	}

	defer closeDraining(conn, events, errs)

	t.setRepliesListening(true)
	defer t.setRepliesListening(false)

	a.RepliesUp(time.Now())

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			// DOWN AS IT FAILS, before the socket is drained and closed: a job
			// finished in that time must not read as timed.
			a.RepliesDown()

			return err
		case ev := <-events:
			at := time.Now()

			if ev.Flow == nil {
				continue
			}

			if ev.Type == conntrack.EventNew && t.replySentinel(ev.Flow) {
				continue
			}

			src := ev.Flow.TupleOrig.IP.SourceAddress

			switch {
			case ev.Type == conntrack.EventNew:
				a.ObserveNew(ev.Flow.ID, src, at)
			case ev.Type == conntrack.EventUpdate && ev.Flow.Status.SeenReply():
				a.ObserveReply(ev.Flow.ID, src, at)
			}
		}
	}
}

func (t *Tracker) setRepliesListening(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.repliesListening = on
}

// replySentinel reports whether f is a Sync's sentinel, read by the reply
// listener, waking its waiter.
func (t *Tracker) replySentinel(f *conntrack.Flow) bool {
	id, ok := sentinelOf(f)
	if !ok {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if w, ok := t.replyWaiters[id]; ok {
		close(w)
		delete(t.replyWaiters, id)
	}

	return true
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

// sentinel reports whether f is a Sync's sentinel, waking its waiter. The
// whole identity is checked: the mark, both addresses, the protocol and the
// nonce in the ports.
func (t *Tracker) sentinel(f *conntrack.Flow) bool {
	id, ok := sentinelOf(f)
	if !ok {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if w, ok := t.waiters[id]; ok {
		close(w)
		delete(t.waiters, id)
	}

	return true
}

// sentinelOf is f's sentinel identity, when f is a sentinel: the mark, both
// addresses and the protocol, and the nonce in its ports.
func sentinelOf(f *conntrack.Flow) (sentinelID, bool) {
	orig := f.TupleOrig
	if f.Mark != sentinelMark || orig.IP.SourceAddress.Unmap() != sentinelSource ||
		orig.IP.DestinationAddress.Unmap() != sentinelDest || orig.Proto.Protocol != sentinelProtocol {
		return sentinelID{}, false
	}

	return sentinelID{sport: orig.Proto.SourcePort, dport: orig.Proto.DestinationPort}, true
}

// Sync returns once the listener has read every destruction the kernel
// reported before the call, and the reply listener every new flow and reply,
// proved by a sentinel entry of its own: its destruction read by the one, its
// creation by the other. It answers with an error saying what it could not
// prove, errRepliesUnproven when only the reply listener's reading is in
// doubt. Cancelling ctx ends it, the netlink calls included.
func (t *Tracker) Sync(ctx context.Context) error {
	t.mu.Lock()

	if !t.listening {
		t.mu.Unlock()

		return errors.New("flows: the connection-tracking listener is not reading")
	}

	var id sentinelID

	for {
		t.nonce++
		id = sentinelID{sport: uint16(t.nonce >> 16), dport: uint16(t.nonce)}

		if _, taken := t.waiters[id]; !taken {
			break
		}
	}

	seen := make(chan struct{})
	t.waiters[id] = seen

	// A REPLY LISTENER NOT READING NOW proves nothing, and its waiter is never
	// registered.
	var replySeen chan struct{}
	if t.repliesListening {
		replySeen = make(chan struct{})
		t.replyWaiters[id] = replySeen
	}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		if t.waiters[id] == seen {
			delete(t.waiters, id)
		}
		if replySeen != nil && t.replyWaiters[id] == replySeen {
			delete(t.replyWaiters, id)
		}
		t.mu.Unlock()
	}()

	conn, err := conntrack.Dial(nil)
	if err != nil {
		return fmt.Errorf("flows: open the connection tracker: %w", err)
	}

	f := conntrack.NewFlow(sentinelProtocol, 0, sentinelSource, sentinelDest, id.sport, id.dport,
		sentinelTimeout, sentinelMark)

	placed := make(chan error, 1)

	go func() {
		if err := conn.Create(f); err != nil {
			placed <- fmt.Errorf("flows: create the sentinel entry: %w", err)

			return
		}

		if err := conn.Delete(f); err != nil {
			placed <- fmt.Errorf("flows: delete the sentinel entry: %w", err)

			return
		}

		placed <- nil
	}()

	// CLOSED ON EVERY PATH, and on cancellation while a call is still in the
	// kernel, which is what makes that call return.
	select {
	case err := <-placed:
		_ = conn.Close()

		if err != nil {
			return err
		}
	case <-ctx.Done():
		_ = conn.Close()

		wait := time.NewTimer(closeLimit)

		select {
		case <-placed:
		case <-wait.C:
		}

		wait.Stop()

		return fmt.Errorf("flows: place the sentinel entry: %w", ctx.Err())
	}

	select {
	case <-seen:
	case <-ctx.Done():
		return fmt.Errorf("flows: the listener did not report the sentinel's destruction: %w", ctx.Err())
	}

	// THE CREATION IS READ BEFORE THE DESTRUCTION in the kernel's order, but on
	// another socket, so it is waited for in its own right.
	if replySeen == nil {
		return fmt.Errorf("%w: it is not reading", errRepliesUnproven)
	}
	select {
	case <-replySeen:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: it did not report the sentinel's creation: %w", errRepliesUnproven, ctx.Err())
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
