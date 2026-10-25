// Package flows attributes a host's connection-tracking flows to the jobs whose
// guests opened them, totalled by destination.
//
// The numbers are the host's, never the guest's: a job is root in its guest and
// could report any destinations it liked, but the kernel's connection tracker
// sees every flow a guest's address opens through the host. A flow is the
// guest's when its original source is the address the guest holds and the
// tracker stamped its start inside the time the guest held it: from the DHCP
// grant that gave it the address to the moment its compute began to be
// destroyed. An entry the address's previous holder opened outlives that
// holder's VM, and is never this job's.
//
// ATTRIBUTION IS BY SOURCE ADDRESS, WHICH A GUEST CAN FORGE. The host's guest
// network does not yet drop a packet whose source is not its tap's guest, so a
// root guest can send traffic that the tracker records under another guest's
// address. These totals are for people to read, and no decision is made from
// them.
package flows

import (
	"cmp"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// MaxDestinations is how many destinations one job keeps by name. Beyond it,
// bytes are still counted, under Other, so a job opening flows to every address
// it can think of costs the node a bounded map and loses no total.
const MaxDestinations = 256

// MaxFlows is how many flows one job is counted for. Each is held until the job
// finishes so a flow reported twice is counted once; past this, further flows
// are not counted and the result is marked incomplete rather than letting a job
// grow the node's memory without bound.
const MaxFlows = 1 << 16

// MaxUnclaimed is how many flows from addresses no job is watched at are kept
// for a job that learns its address after the guest opened and closed them: a
// guest asks DHCP for its address after it boots, the node reads the lease up
// to an interval later, and the tracker reports a flow's end only once.
const MaxUnclaimed = 4096

// SlowReply is the wait for a flow's first packet back past which it is slow:
// TCP's initial retransmission timeout (RFC 6298), so a connect slower than it
// is one whose SYN the guest was already sending again. A DNS query over it is
// one a resolver answered after the guest's own first retry would be due.
const SlowReply = time.Second

// replyMatch bounds how far a counted flow's start may lie from the new event
// its wait was timed from: the reading of that event, on the node's clock, is
// the moment the kernel created the entry, or the event is not that flow's.
const replyMatch = time.Second

// keptGenerations is how many generations of one id are kept. ONE IS ALL A WAIT
// IS EVER TAKEN FROM: the kernel reuses an id for a later entry at the same
// address with the same tuple once the first is gone, and with a new event
// missed, or read late, or the two sockets read out of order, two flows of one
// id cannot be told apart by when they began. So an id is timed only when the
// job saw one generation of it and counted one flow with it; a second of
// either, or one dropped past this bound, leaves the id untimed.
const keptGenerations = 2

// errRepliesUnproven says Sync proved every destruction read and could not
// prove the same of the new and reply events: the job is untimed, and its
// byte totals are as complete as they were.
var errRepliesUnproven = errors.New("flows: the listener for first replies did not prove it had read " +
	"every event")

// Flow is one tracked connection, in the tracker's own terms: Orig is the
// direction the guest opened, Reply the other.
type Flow struct {
	// ID is the tracker's id for the entry, unique while the entry exists; a
	// later entry may reuse it, so an entry is ID and Start together.
	ID uint32
	// Start is when the tracker created the entry (nf_conntrack_timestamp);
	// zero when it did not say.
	Start    time.Time
	Protocol uint8
	Source   netip.Addr // the original source: the guest, for a guest's flow
	Dest     netip.Addr // the original destination, before any NAT
	DestPort uint16
	// OrigBytes is what the guest sent, ReplyBytes what it received.
	OrigBytes, ReplyBytes     uint64
	OrigPackets, ReplyPackets uint64
}

// entry is a flow's identity across readings.
type entry struct {
	id    uint32
	start int64
}

func entryOf(f *Flow) entry { return entry{id: f.ID, start: f.Start.UnixNano()} }

// Destination is one remote address a job exchanged traffic with.
type Destination struct {
	Addr netip.Addr
	// Sent and Received are from the guest's side.
	Sent, Received uint64
	Connections    uint64
	// Unanswered is how many of those connections the guest sent on and got no
	// packet back from: a TCP connect whose SYN nothing answered, or a datagram,
	// a DNS query among them, that drew no reply. Where a job stalled before it
	// reached anyone, this is the count that names the destination.
	Unanswered uint64
	// TimedReplies is how many of the answered connections had their first
	// reply timed, SlowReplies how many of those waited longer than SlowReply,
	// and SlowestReply the longest of their waits. A slow resolver or a shaped
	// uplink shows here, where nothing went unanswered.
	TimedReplies, SlowReplies uint64
	SlowestReply              time.Duration
}

// Result is what a job's flows came to.
type Result struct {
	// Destinations, largest total first, at most MaxDestinations.
	Destinations []Destination
	// Other holds the traffic to every destination beyond MaxDestinations; its
	// Addr is the zero address.
	Other Destination
	// Incomplete says flows may have been missed or could not be told apart:
	// the tracker's events were not all read while the job held its address,
	// the job opened more than MaxFlows, a flow carried no start time, the
	// guest's traffic joined an entry its address's previous holder left, or
	// its address changed. The totals are then a lower bound, and say so.
	Incomplete bool
	// UnansweredUnknown says a flow of the job's, or an entry its address's
	// previous holder left, carried no packet counts in either direction, which
	// no flow has while the tracker keeps them, so no destination's Unanswered
	// can be told from none.
	UnansweredUnknown bool
	// RepliesTimed says the tracker's new and reply events were read for the
	// whole of the job's time, so a destination's TimedReplies and its waits
	// are measured; a connection still left untimed then is one whose new
	// event came before the job was watched or could not be matched.
	RepliesTimed bool
}

// Accountant attributes flows to watched jobs. Safe for concurrent use: the
// tracker's listener reports flows while the node starts and finishes jobs.
type Accountant struct {
	mu     sync.Mutex
	byKey  map[string]*watch
	byAddr map[netip.Addr]*watch
	// unclaimed is a ring of the latest flows no watched job held, replayed to
	// a job when its address is learned, and evictedStart the latest start of
	// a flow the ring has dropped: a job whose time began before it may have
	// lost a flow to the ring.
	unclaimed    []Flow
	next         int
	evictedStart time.Time
	// evictedUnstamped is when the ring last dropped a flow with no start
	// time: whose it was cannot be told, so every job granted before then may
	// have lost it.
	evictedUnstamped time.Time
	// evictedCounterless is the latest start of a flow the ring dropped that
	// carried no packet counts, or when it dropped one with no start: a job
	// granted its address at or before it may have lost a stall with it.
	evictedCounterless time.Time
	// down says the tracker's events are not being read now, and lossEnded
	// when the latest such gap closed: a job whose time overlaps either may be
	// missing flows.
	down      bool
	lossEnded time.Time
	// repliesDown says the new and reply events are not being read now, and
	// repliesLossEnded when the latest such gap closed. It starts down: no job
	// is timed until a listener is proved reading.
	repliesDown      bool
	repliesLossEnded time.Time
	// now is the clock an unstamped flow's eviction is dated by.
	now func() time.Time
}

type watch struct {
	addr netip.Addr
	// since and until bound the flows that are this job's by when they
	// started; until is zero while the job runs.
	since, until time.Time
	// counted holds each flow's last reading, so a flow reported more than once
	// (its destruction, and again still open at Final) is counted at its latest
	// totals and never twice.
	counted map[entry]Flow
	// inherited holds the entries the address's previous holder left, at the
	// readings they had when the job began. Traffic the guest sends on one of
	// those tuples is added to the old entry, whose start is the old holder's,
	// so it cannot be counted, only noticed.
	inherited  map[entry]Flow
	incomplete bool
	// unansweredUnknown says a flow that could be the job's and was not
	// counted carried no packet counts: an entry the previous holder left open,
	// whose tuple the guest could reuse unnoticed, a flow with no start time,
	// one past MaxFlows, or one the unclaimed ring dropped.
	unansweredUnknown bool
	// untimed says the new and reply events were not all read while the job
	// held its address. generations holds each id's latest flows as their new
	// and reply events told them, matched to the counted flows only when the
	// job is summarized, after the barrier that proved both read; dropped
	// marks an id that lost one, and held counts every generation kept.
	untimed     bool
	generations map[uint32][]generation
	dropped     map[uint32]bool
	held        int
}

// generation is one flow of an id as its events told it: when its new event
// was read, and, once its first reply was, how long it waited.
type generation struct {
	opened  time.Time
	wait    time.Duration
	replied bool
}

// NewAccountant returns an Accountant watching nothing.
func NewAccountant() *Accountant {
	return &Accountant{byKey: map[string]*watch{}, byAddr: map[netip.Addr]*watch{}, now: time.Now,
		repliesDown: true}
}

// Watch starts attributing flows from addr to the job named key, counting only
// flows the tracker started at or after since, when the guest was granted the
// address. open are the flows the tracker holds for addr now; those that
// started before since are the previous holder's. It reports false, attributing
// nothing, when addr is not a valid address or another watched job holds it:
// two jobs cannot both own an address, and guessing which one a flow is would
// be a fabrication.
func (a *Accountant) Watch(key string, addr netip.Addr, since time.Time, open []Flow) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, held := a.byAddr[addr]; held {
		return false
	}

	if old, ok := a.byKey[key]; ok {
		delete(a.byAddr, old.addr)
	}

	w := &watch{addr: addr, since: since, counted: map[entry]Flow{}, inherited: map[entry]Flow{},
		generations: map[uint32][]generation{}, dropped: map[uint32]bool{}}

	// A GAP IN THE EVENTS, OR A FLOW THE RING DROPPED, SINCE THE GRANT: either
	// may have been this guest's.
	if a.down || !a.lossEnded.Before(since) || !a.evictedStart.Before(since) ||
		!a.evictedUnstamped.Before(since) {
		w.incomplete = true
	}
	if !a.evictedCounterless.Before(since) {
		w.unansweredUnknown = true
	}
	if a.repliesDown || !a.repliesLossEnded.Before(since) {
		w.untimed = true
	}

	for i := range open {
		f := &open[i]
		if f.Source.Unmap() == addr && !f.Start.IsZero() && f.Start.Before(since) {
			w.inherited[entryOf(f)] = *f
		}
	}

	a.byKey[key] = w
	a.byAddr[addr] = w

	// FLOWS THAT ENDED BEFORE THE ADDRESS WAS KNOWN are the job's if they
	// started after the grant, and the start time is what keeps the previous
	// holder's out.
	for i := range a.unclaimed {
		if a.unclaimed[i].Source == addr {
			a.observe(a.unclaimed[i])
		}
	}

	for i := range open {
		a.observe(open[i])
	}

	return true
}

// Observe records one reading of a flow, from its destruction or a dump. A
// flow from no watched address, or one outside its address's job's time, is
// not that job's.
func (a *Accountant) Observe(f Flow) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.observe(f)
}

func (a *Accountant) observe(f Flow) {
	f.Source = f.Source.Unmap()

	w, ok := a.byAddr[f.Source]
	if !ok {
		a.keepUnclaimed(f)

		return
	}

	// NO START TIME, NO OWNER. The flow could be this guest's or the previous
	// holder's, and counting it either way would be a guess.
	if f.Start.IsZero() {
		w.incomplete = true
		w.unansweredUnknown = w.unansweredUnknown || uncountedPackets(&f)

		return
	}

	key := entryOf(&f)

	if f.Start.Before(w.since) {
		// THE OLD HOLDER'S ENTRY, AND IF THE GUEST HAS SENT ON IT, THIS JOB'S
		// TRAFFIC IS INSIDE IT: only the address's holder sends from it, and the
		// old holder's VM is gone. What came back is no evidence, since a remote
		// goes on answering a guest that has left.
		// AN INHERITED ENTRY WITH NO PACKET COUNTS hides that reuse altogether,
		// so a stall on its tuple could not be noticed. Only an entry open when
		// the job was watched is inherited: one already destroyed is no tuple
		// the guest can send on.
		if base, inherited := w.inherited[key]; inherited {
			if f.OrigPackets > base.OrigPackets {
				w.incomplete = true
			}
			w.unansweredUnknown = w.unansweredUnknown || uncountedPackets(&base)
		}

		return
	}

	// STARTED AFTER THE JOB'S COMPUTE BEGAN TO GO: the next holder's.
	if !w.until.IsZero() && f.Start.After(w.until) {
		return
	}

	prev, seen := w.counted[key]
	if !seen && len(w.counted) >= MaxFlows {
		w.incomplete = true
		w.unansweredUnknown = w.unansweredUnknown || uncountedPackets(&f)

		return
	}

	// A flow's counters only grow. A reading behind the one held is an older
	// report arriving late, and is not allowed to shrink the total.
	if seen && f.OrigBytes+f.ReplyBytes < prev.OrigBytes+prev.ReplyBytes {
		return
	}

	w.counted[key] = f
}

// waitFor is the wait of f's first reply, given how many counted flows carry
// its id: the wait of the id's one generation, when the job saw just one and
// counted just f with that id, the generation began within replyMatch of f's
// start, and it saw a reply; none otherwise, since anything else is an id that
// may have been reused.
func (w *watch) waitFor(f *Flow, flowsWithID int) (time.Duration, bool) {
	kept := w.generations[f.ID]
	if w.dropped[f.ID] || len(kept) != 1 || flowsWithID != 1 {
		return 0, false
	}
	g := kept[0]

	return g.wait, g.replied && f.Start.Sub(g.opened).Abs() <= replyMatch
}

// ObserveNew records that the tracker created flow id from src, read at at: a
// new generation of the id, whose wait for a first reply is timed from here.
//
// PAST MaxFlows GENERATIONS THE JOB IS UNTIMED: a generation not kept would
// leave an earlier one of its id looking like the only candidate.
func (a *Accountant) ObserveNew(id uint32, src netip.Addr, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byAddr[src.Unmap()]
	if !ok {
		return
	}
	if w.held >= MaxFlows {
		w.untimed = true

		return
	}

	kept := w.generations[id]
	if len(kept) == keptGenerations {
		kept = kept[1:]
		w.held--
		w.dropped[id] = true
	}
	w.generations[id] = append(kept, generation{opened: at})
	w.held++
}

// ObserveReply records that flow id from src first saw a reply, read at at.
// Only the first reply after its new event is its wait; every later event is
// the flow's state moving on.
func (a *Accountant) ObserveReply(id uint32, src netip.Addr, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byAddr[src.Unmap()]
	if !ok {
		return
	}
	kept := w.generations[id]
	if len(kept) == 0 || kept[len(kept)-1].replied {
		return
	}
	latest := &kept[len(kept)-1]
	latest.wait, latest.replied = max(at.Sub(latest.opened), 0), true
}

func (a *Accountant) keepUnclaimed(f Flow) {
	if len(a.unclaimed) < MaxUnclaimed {
		a.unclaimed = append(a.unclaimed, f)

		return
	}

	evicted := &a.unclaimed[a.next]
	switch dropped := evicted.Start; {
	case dropped.IsZero():
		a.evictedUnstamped = a.now()
	case dropped.After(a.evictedStart):
		a.evictedStart = dropped
	}
	if uncountedPackets(evicted) {
		at := evicted.Start
		if at.IsZero() {
			at = a.now()
		}
		if at.After(a.evictedCounterless) {
			a.evictedCounterless = at
		}
	}

	a.unclaimed[a.next] = f
	a.next = (a.next + 1) % MaxUnclaimed
}

// Down says the tracker's events stopped being read at the moment it is
// called: every job watched now may miss flows, and so may every job that
// starts before Up.
func (a *Accountant) Down() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.down = true

	for _, w := range a.byKey {
		w.incomplete = true
	}
}

// Up says the tracker's events are being read again, from now.
func (a *Accountant) Up(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.down {
		a.down, a.lossEnded = false, now
	}
}

// RepliesDown says the tracker's new and reply events stopped being read at the
// moment it is called: no job watched now, or started before RepliesUp, is
// timed.
func (a *Accountant) RepliesDown() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.repliesDown = true

	for _, w := range a.byKey {
		w.untimed = true
	}
}

// RepliesUp says the tracker's new and reply events are being read again, from
// now.
func (a *Accountant) RepliesUp(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.repliesDown {
		a.repliesDown, a.repliesLossEnded = false, now
	}
}

// MarkUntimed says key's first replies can no longer all be told: the listener
// for them could not prove it had read every event.
func (a *Accountant) MarkUntimed(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if w, ok := a.byKey[key]; ok {
		w.untimed = true
	}
}

// MarkIncomplete says key's flows can no longer all be told apart: its address
// changed, or the end of its flows could not be proved read.
func (a *Accountant) MarkIncomplete(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if w, ok := a.byKey[key]; ok {
		w.incomplete = true
	}
}

// Final stops watching key and returns what its flows came to. open are the
// flows the tracker still holds for the job's address, read after its compute
// was destroyed; they are counted at those readings. until is when the destroy
// began: a flow that started after it is the next holder's. It reports false
// when key was never watched.
func (a *Accountant) Final(key string, open []Flow, until time.Time) (Result, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byKey[key]
	if !ok {
		return Result{}, false
	}

	w.until = until

	for i := range open {
		a.observe(open[i])
	}

	// THE BOUND APPLIES TO WHAT WAS ALREADY COUNTED TOO: a destruction reported
	// while the compute was being destroyed can be the next holder's.
	for key := range w.counted {
		if f := w.counted[key]; f.Start.After(until) {
			delete(w.counted, key)
		}
	}

	delete(a.byKey, key)
	delete(a.byAddr, w.addr)

	return summarize(w), true
}

// uncountedPackets says the tracker kept no packet counts for f: a flow begins
// with its originator's packet, so none counted in either direction is
// accounting that was off, not traffic that was absent.
func uncountedPackets(f *Flow) bool { return f.OrigPackets == 0 && f.ReplyPackets == 0 }

func summarize(w *watch) Result {
	byDest := map[netip.Addr]*Destination{}
	unknown := w.unansweredUnknown

	// HOW MANY COUNTED FLOWS CARRY EACH ID: two are a reuse, and neither is
	// timed.
	flowsWithID := map[uint32]int{}
	for key := range w.counted {
		flowsWithID[key.id]++
	}

	for id := range w.counted {
		f := w.counted[id]

		d, ok := byDest[f.Dest]
		if !ok {
			d = &Destination{Addr: f.Dest}
			byDest[f.Dest] = d
		}

		d.Sent += f.OrigBytes
		d.Received += f.ReplyBytes
		d.Connections++
		// UNANSWERED ONLY WHERE THE GUEST'S OWN PACKETS WERE COUNTED: a flow
		// begins with the guest's packet, so one with none counted either way is
		// a tracker that kept no counts, and the whole result cannot say what
		// went unanswered.
		switch {
		case uncountedPackets(&f):
			unknown = true
		case f.OrigPackets > 0 && f.ReplyPackets == 0:
			d.Unanswered++
		}
		// TIMED HERE, after the barrier proved both sockets read, so a flow whose
		// destruction was read before its reply still takes its wait; and only a
		// flow the reading says was answered.
		if wait, timed := w.waitFor(&f, flowsWithID[f.ID]); timed && f.ReplyPackets > 0 {
			d.TimedReplies++
			if wait > SlowReply {
				d.SlowReplies++
			}
			d.SlowestReply = max(d.SlowestReply, wait)
		}
	}

	all := make([]Destination, 0, len(byDest))
	for _, d := range byDest {
		all = append(all, *d)
	}

	// LARGEST FIRST, AND THE ORDER IS TOTAL: equal totals fall back to the
	// address, so the same flows always keep the same destinations by name.
	slices.SortFunc(all, func(x, y Destination) int {
		if c := cmp.Compare(y.Sent+y.Received, x.Sent+x.Received); c != 0 {
			return c
		}

		return x.Addr.Compare(y.Addr)
	})

	r := Result{Incomplete: w.incomplete, UnansweredUnknown: unknown, RepliesTimed: !w.untimed}

	if len(all) > MaxDestinations {
		for _, d := range all[MaxDestinations:] {
			r.Other.Sent += d.Sent
			r.Other.Received += d.Received
			r.Other.Connections += d.Connections
			r.Other.Unanswered += d.Unanswered
			r.Other.TimedReplies += d.TimedReplies
			r.Other.SlowReplies += d.SlowReplies
			r.Other.SlowestReply = max(r.Other.SlowestReply, d.SlowestReply)
		}

		all = all[:MaxDestinations]
	}

	r.Destinations = all

	return r
}
