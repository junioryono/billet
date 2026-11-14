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
	"maps"
	"math"
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

// MaxDroppedSources is how many sources the unclaimed ring's drops are told
// apart for. Past it they are folded into one record, which any job granted
// before its times may have lost a flow to, so a host seeing connections from
// more addresses than this costs jobs a wider mark and the node no more memory.
const MaxDroppedSources = 4096

// Why a job's result is Incomplete, as Result.IncompleteWhy names it.
const (
	// WhyEventsGap: the tracker's events were not all read while the job held
	// its address.
	WhyEventsGap = "events_gap"
	// WhyDropped: the unclaimed ring dropped a flow from the job's own address
	// that it may have owned, before the node had learned that address.
	WhyDropped = "unclaimed_dropped"
	// WhyDroppedUntold: the ring dropped a flow since the grant whose source was
	// no longer told apart, past MaxDroppedSources.
	WhyDroppedUntold = "unclaimed_dropped_untold"
	// WhyNoStart: a flow from the job's address carried no start time.
	WhyNoStart = "no_start_time"
	// WhyTooManyFlows: the job opened more than MaxFlows.
	WhyTooManyFlows = "too_many_flows"
	// WhyInherited: the guest sent on an entry its address's previous holder left.
	WhyInherited = "inherited_entry"
	// WhyAddressChanged: the guest's lease came to name another address.
	WhyAddressChanged = "address_changed"
	// WhyLeaseUnread: the guest's lease could not be read back once its address
	// was known.
	WhyLeaseUnread = "lease_unread"
	// WhyTableUnread: the tracker's table could not be read for the job's address.
	WhyTableUnread = "table_unread"
	// WhyGrantAmbiguous: the grant fell in the launch's own second, so it could
	// be the previous guest's renewal.
	WhyGrantAmbiguous = "grant_ambiguous"
	// WhyGrantLate: the grant came over half a lease after the launch, so it may
	// be a renewal hiding earlier flows.
	WhyGrantLate = "grant_late"
	// WhySyncUnproved: the end of the job's flows could not be proved read.
	WhySyncUnproved = "sync_unproved"
)

// SlowReply is the wait for a flow's first packet back past which it is slow:
// TCP's initial retransmission timeout (RFC 6298), so a connect slower than it
// is one whose SYN the guest was already sending again. A DNS query over it is
// one a resolver answered after the guest's own first retry would be due.
const SlowReply = time.Second

// replyMatch bounds how far a counted flow's start may lie from the reading of
// the new event its wait was timed from: that reading is the moment the kernel
// created the entry, or the event is not that flow's. It is what catches a new
// event missed at a listener's start, the one case the event order cannot.
const replyMatch = time.Second

// Flow is one tracked connection, in the tracker's own terms: Orig is the
// direction the guest opened, Reply the other.
type Flow struct {
	// ID is the tracker's id for the entry, unique while the entry exists; a
	// later entry may reuse it, so an entry is ID and Start together.
	ID uint32
	// Start is when the tracker created the entry (nf_conntrack_timestamp);
	// zero when it did not say.
	Start      time.Time
	Protocol   uint8
	Zone       uint16     // the tracker's zone, which keeps apart entries of one tuple
	Source     netip.Addr // the original source: the guest, for a guest's flow
	Dest       netip.Addr // the original destination, before any NAT
	SourcePort uint16
	DestPort   uint16
	// OrigBytes is what the guest sent, ReplyBytes what it received.
	OrigBytes, ReplyBytes     uint64
	OrigPackets, ReplyPackets uint64
	// ReplyWait is how long the flow waited for its first packet back: from
	// reading the tracker's event creating it to reading its first event that
	// saw a reply. ReplyTimed says it was; the entry itself carries neither.
	ReplyWait  time.Duration
	ReplyTimed bool
	// ended says this reading is the flow's destruction, its last: a dump of
	// the same entry read before it and applied after is stale.
	ended bool
}

// generationKey is a flow's identity as its events tell it: the tracker's id,
// which is a 32-bit hash two live entries can share, and the original tuple and
// zone, which they cannot. Complete only for TCP and UDP, whose tuple is the
// ports; a protocol whose tuple has other fields (ICMP's id, type and code) is
// never timed.
type generationKey struct {
	id                   uint32
	protocol             uint8
	zone                 uint16
	dest                 netip.Addr
	sourcePort, destPort uint16
}

func generationOf(f *Flow) generationKey {
	return generationKey{id: f.ID, protocol: f.Protocol, zone: f.Zone, dest: f.Dest.Unmap(),
		sourcePort: f.SourcePort, destPort: f.DestPort}
}

// timedProtocol reports whether a flow of protocol p can be timed: whether its
// identity is whole in generationKey.
func timedProtocol(p uint8) bool { return p == 6 || p == 17 }

// entry is a flow's identity across readings.
type entry struct {
	generation generationKey
	start      int64
}

// entryOf is f's entry: its id and tuple, since two live entries can share an
// id, and its start, since a later entry can reuse both.
func entryOf(f *Flow) entry { return entry{generation: generationOf(f), start: f.Start.UnixNano()} }

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
	// IncompleteWhy names each reason it is Incomplete, sorted, from the Why
	// constants; empty when it is not.
	IncompleteWhy []string
	// UnansweredUnknown says a flow of the job's, or an entry its address's
	// previous holder left, carried no packet counts in either direction, which
	// no flow has while the tracker keeps them, so no destination's Unanswered
	// can be told from none.
	UnansweredUnknown bool
	// RepliesTimed says the tracker's events were read for the whole of the
	// job's time, so a destination's TimedReplies and its waits are measured;
	// a connection still left untimed then is one whose new event came before
	// the job was watched or was not read within replyMatch of its start.
	RepliesTimed bool
	// Openings is the same traffic again, split by when each connection
	// opened: per destination, per bucket of OpeningWidth, at most MaxOpenings,
	// ordered by destination and then by time. It is what lets a report say
	// which step of the job talked to which destination.
	Openings     []Opening
	OpeningWidth time.Duration
}

// MaxOpenings bounds a job's openings. A job whose connections open across
// more buckets than this has its buckets widened, by doubling, until they fit:
// a coarser answer, never a partial one, so the openings always add up to the
// destinations' totals.
//
// 1,024 BECAUSE THE WHOLE REPORT IS ONE REQUEST: at their widest, this many
// openings beside the largest series and every destination at its widest come
// to about 0.93 MB of the plane's 1 MiB body, and 2,048 to about 1.08 MB, which
// would have the plane refuse the job's whole measurement.
const MaxOpenings = 1024

// Opening is the traffic of the connections a job opened to one destination in
// one bucket of time, a bucket being OpeningWidth from Start.
//
// BY WHEN A CONNECTION OPENED, NOT BY WHEN ITS BYTES MOVED: the tracker keeps a
// connection's totals and not their times, so a connection that spans two
// steps is the first one's.
type Opening struct {
	// Dest is the destination's index in Result.Destinations, or -1 for the
	// traffic beyond them, Result.Other.
	Dest                        int
	Start                       time.Time
	Sent, Received, Connections uint64
}

// openingKey is one destination's one bucket, the bucket a Unix second
// divided by the width.
type openingKey struct {
	dest   int
	bucket int64
}

// openingsOf splits a job's counted flows by destination and by the bucket each
// opened in, widening the buckets until there are at most MaxOpenings of them.
// index is each named destination's place in the result; an address it does not
// name is the traffic beyond them.
//
// IT ALWAYS FITS: one bucket wide enough to hold every flow leaves one opening
// per destination, at most MaxDestinations+1, under the bound.
func openingsOf(counted map[entry]Flow, index map[netip.Addr]int) ([]Opening, time.Duration) {
	perSecond := map[openingKey]*Opening{}
	for id := range counted {
		f := counted[id]
		dest, ok := index[f.Dest]
		if !ok {
			dest = -1
		}
		k := openingKey{dest: dest, bucket: f.Start.Unix()}
		o := perSecond[k]
		if o == nil {
			o = &Opening{Dest: dest}
			perSecond[k] = o
		}
		o.Sent = satAdd(o.Sent, f.OrigBytes)
		o.Received = satAdd(o.Received, f.ReplyBytes)
		o.Connections = satAdd(o.Connections, 1)
	}

	width := int64(1)
	buckets := perSecond
	for len(buckets) > MaxOpenings {
		width *= 2
		buckets = map[openingKey]*Opening{}
		for k, o := range perSecond {
			wide := openingKey{dest: k.dest, bucket: floorDiv(k.bucket, width)}
			w := buckets[wide]
			if w == nil {
				w = &Opening{Dest: k.dest}
				buckets[wide] = w
			}
			w.Sent = satAdd(w.Sent, o.Sent)
			w.Received = satAdd(w.Received, o.Received)
			w.Connections = satAdd(w.Connections, o.Connections)
		}
	}

	out := make([]Opening, 0, len(buckets))
	for k, o := range buckets {
		o.Start = time.Unix(k.bucket*width, 0).UTC()
		out = append(out, *o)
	}
	// THE OTHER TRAFFIC LAST, as the destinations it is beyond are listed first.
	slices.SortFunc(out, func(x, y Opening) int {
		xd, yd := x.Dest, y.Dest
		if xd < 0 {
			xd = len(index)
		}
		if yd < 0 {
			yd = len(index)
		}
		if c := cmp.Compare(xd, yd); c != 0 {
			return c
		}

		return x.Start.Compare(y.Start)
	})

	return out, time.Duration(width) * time.Second
}

// satAdd is a+b held at the largest uint64 rather than wrapping.
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}

	return a + b
}

// floorDiv is a divided by b rounded down, for a second before 1970 as for one
// after it.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}

	return q
}

// Accountant attributes flows to watched jobs. Safe for concurrent use: the
// tracker's listener reports flows while the node starts and finishes jobs.
type Accountant struct {
	mu     sync.Mutex
	byKey  map[string]*watch
	byAddr map[netip.Addr]*watch
	// unclaimed is a ring of the latest flows no watched job held, replayed to
	// a job when its address is learned.
	unclaimed []Flow
	next      int
	// droppedFrom says, by source, what the ring has dropped. A flow is a job's
	// only if its source is the job's address, so a drop from any other source
	// is no flow that job lost, however busy the host. droppedUntold is what was
	// folded out of it past MaxDroppedSources, whose sources are no longer told
	// apart, so any job may have lost those.
	droppedFrom   map[netip.Addr]dropped
	droppedUntold dropped
	// down says the tracker's events are not being read now, and lossEnded
	// when the latest such gap closed: a job whose time overlaps either may be
	// missing flows.
	down      bool
	lossEnded time.Time
	// repliesDown says the tracker's events are not being read now for first
	// replies, and repliesLossEnded when the latest such gap closed. It starts
	// down, where down does not: no job is timed until a listener is proved
	// reading.
	repliesDown      bool
	repliesLossEnded time.Time
	// now is the clock an unstamped flow's eviction is dated by.
	now func() time.Time
}

// dropped is what the unclaimed ring has dropped.
type dropped struct {
	// start is the latest start of a flow it dropped: a job whose time began at
	// or before it may have lost that flow.
	start time.Time
	// unstamped is when it last dropped a flow with no start time, which could
	// have been opened at any time before then.
	unstamped time.Time
	// counterless is the latest start of a flow it dropped that carried no packet
	// counts, or when it dropped one with no start: a job granted its address at
	// or before it may have lost a stall with it.
	counterless time.Time
}

// lost says a job granted its address at since may have lost a flow to d.
func (d dropped) lost(since time.Time) bool {
	return !d.start.Before(since) || !d.unstamped.Before(since)
}

// merged is d and other's latest times.
func (d dropped) merged(other dropped) dropped {
	return dropped{start: later(d.start, other.start), unstamped: later(d.unstamped, other.unstamped),
		counterless: later(d.counterless, other.counterless)}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
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
	inherited map[entry]Flow
	// why holds each reason the job's result is incomplete, from the Why
	// constants: nil while there is none.
	why map[string]struct{}
	// unansweredUnknown says a flow that could be the job's and was not
	// counted carried no packet counts: an entry the previous holder left open,
	// whose tuple the guest could reuse unnoticed, a flow with no start time,
	// one past MaxFlows, or one the unclaimed ring dropped.
	unansweredUnknown bool
	// untimed says the tracker's events were not all read while the job held
	// its address, so its first replies cannot all be told. generations holds,
	// by id and tuple, the latest flow of each as its new and reply events told
	// it; the flow's own destruction, read after them on the same socket in the
	// kernel's order, takes it, and so does a dump of a flow still open.
	untimed     bool
	generations map[generationKey]generation
}

// mark says the job's result is incomplete, and why.
func (w *watch) mark(why string) {
	if w.why == nil {
		w.why = map[string]struct{}{}
	}
	w.why[why] = struct{}{}
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
		droppedFrom: map[netip.Addr]dropped{}, repliesDown: true}
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
		generations: map[generationKey]generation{}}

	// A GAP IN THE EVENTS, OR A FLOW THE RING DROPPED FROM THIS ADDRESS, SINCE THE
	// GRANT: either may have been this guest's.
	if a.down || !a.lossEnded.Before(since) {
		w.mark(WhyEventsGap)
	}
	own := a.droppedFrom[addr]
	if own.lost(since) {
		w.mark(WhyDropped)
	}
	if a.droppedUntold.lost(since) {
		w.mark(WhyDroppedUntold)
	}
	if !own.counterless.Before(since) || !a.droppedUntold.counterless.Before(since) {
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
			a.observe(a.unclaimed[i], true)
		}
	}

	for i := range open {
		a.observe(open[i], false)
	}

	return true
}

// Observe records a flow's destruction. A flow from no watched address, or one
// outside its address's job's time, is not that job's.
func (a *Accountant) Observe(f Flow) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.observe(f, true)
}

// observe records one reading of a flow: its destruction when ended, or a dump
// of it still open.
func (a *Accountant) observe(f Flow, ended bool) {
	f.Source = f.Source.Unmap()

	w, ok := a.byAddr[f.Source]
	if !ok {
		a.keepUnclaimed(f)

		return
	}

	// THE FLOW'S OWN GENERATION, in the kernel's order: its new and reply
	// events come before its destruction on the one socket, and an id is
	// reused only after it, so the generation held now is this flow's. A
	// destruction takes it, counted or not, so no later flow can.
	gk := generationOf(&f)
	g, generated := w.generations[gk]
	if ended {
		delete(w.generations, gk)
	}
	if generated && g.replied && timedProtocol(f.Protocol) && f.Start.Sub(g.opened).Abs() <= replyMatch {
		f.ReplyWait, f.ReplyTimed = g.wait, true
	}
	f.ended = ended

	// NO START TIME, NO OWNER. The flow could be this guest's or the previous
	// holder's, and counting it either way would be a guess.
	if f.Start.IsZero() {
		w.mark(WhyNoStart)
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
				w.mark(WhyInherited)
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
		w.mark(WhyTooManyFlows)
		w.unansweredUnknown = w.unansweredUnknown || uncountedPackets(&f)

		return
	}

	// A flow's counters only grow. A reading behind the one held is an older
	// report arriving late, and is not allowed to shrink the total; and after
	// the flow's destruction nothing is newer, so a dump of it taken before and
	// applied after changes nothing, its timing included.
	if seen && (prev.ended && !ended || f.OrigBytes+f.ReplyBytes < prev.OrigBytes+prev.ReplyBytes) {
		return
	}

	// A LATER READING WITHOUT THE GENERATION keeps the wait an earlier one took.
	if !f.ReplyTimed && seen && prev.ReplyTimed {
		f.ReplyWait, f.ReplyTimed = prev.ReplyWait, true
	}

	w.counted[key] = f
}

// ObserveNew records that the tracker created flow f, read at at: a new
// generation of its id and tuple, whose wait for a first reply is timed from
// here. Only f's identity is read; a new event carries no counts.
//
// A NEW EVENT FOR A GENERATION ALREADY HELD makes the job untimed: the kernel
// announces an entry's creation again, beside a later event, when another
// listener missed it, and its reading is then no creation time. One that
// already carries the reply (answered) starts no generation either.
//
// PAST MaxFlows GENERATIONS THE JOB IS UNTIMED: one not kept would leave its
// flow's destruction to find nothing, which is safe, but the job could no
// longer say every answered connection it timed was all it could time.
func (a *Accountant) ObserveNew(f Flow, at time.Time, answered bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byAddr[f.Source.Unmap()]
	if !ok {
		return
	}
	gk := generationOf(&f)
	if _, held := w.generations[gk]; held {
		w.untimed = true

		return
	}
	if answered {
		return
	}
	if len(w.generations) >= MaxFlows {
		w.untimed = true

		return
	}
	w.generations[gk] = generation{opened: at}
}

// ObserveReply records that flow f first saw a reply, read at at.
// Only the first reply after its new event is its wait; every later event is
// the flow's state moving on.
func (a *Accountant) ObserveReply(f Flow, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byAddr[f.Source.Unmap()]
	if !ok {
		return
	}
	gk := generationOf(&f)
	if g, ok := w.generations[gk]; ok && !g.replied {
		g.wait, g.replied = max(at.Sub(g.opened), 0), true
		w.generations[gk] = g
	}
}

func (a *Accountant) keepUnclaimed(f Flow) {
	if len(a.unclaimed) < MaxUnclaimed {
		a.unclaimed = append(a.unclaimed, f)

		return
	}

	a.noteDropped(&a.unclaimed[a.next])
	a.unclaimed[a.next] = f
	a.next = (a.next + 1) % MaxUnclaimed
}

// noteDropped records that the ring dropped f, under its source.
func (a *Accountant) noteDropped(f *Flow) {
	held, known := a.droppedFrom[f.Source]
	switch {
	case f.Start.IsZero():
		held.unstamped = a.now()
	case f.Start.After(held.start):
		held.start = f.Start
	}
	if uncountedPackets(f) {
		at := f.Start
		if at.IsZero() {
			at = a.now()
		}
		held.counterless = later(held.counterless, at)
	}

	// PAST THE BOUND EVERY SOURCE IS FOLDED INTO ONE RECORD, which errs toward a
	// job marked for a flow that was not its own and never away from one it lost.
	if !known && len(a.droppedFrom) >= MaxDroppedSources {
		for _, d := range a.droppedFrom {
			a.droppedUntold = a.droppedUntold.merged(d)
		}
		clear(a.droppedFrom)
	}
	a.droppedFrom[f.Source] = held
}

// Down says the tracker's events stopped being read at the moment it is
// called: every job watched now may miss flows, and so may every job that
// starts before Up.
func (a *Accountant) Down() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.down, a.repliesDown = true, true

	for _, w := range a.byKey {
		w.mark(WhyEventsGap)
		w.untimed = true
	}
}

// Up says the tracker's events are being read again, from now.
func (a *Accountant) Up(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.down {
		a.down, a.lossEnded = false, now
	}
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

// MarkIncomplete says key's flows can no longer all be told apart, for the
// reason why names: one of the Why constants.
func (a *Accountant) MarkIncomplete(key, why string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if w, ok := a.byKey[key]; ok {
		w.mark(why)
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
		a.observe(open[i], false)
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

	for id := range w.counted {
		f := w.counted[id]

		d, ok := byDest[f.Dest]
		if !ok {
			d = &Destination{Addr: f.Dest}
			byDest[f.Dest] = d
		}

		// SATURATING, AS THE OPENINGS ARE: a total that wrapped would be less
		// than the openings it is split into, and a report holding both would be
		// refused whole.
		d.Sent = satAdd(d.Sent, f.OrigBytes)
		d.Received = satAdd(d.Received, f.ReplyBytes)
		d.Connections = satAdd(d.Connections, 1)
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
		// ONLY A FLOW THE READING SAYS WAS ANSWERED is timed.
		if f.ReplyTimed && f.ReplyPackets > 0 {
			d.TimedReplies++
			if f.ReplyWait > SlowReply {
				d.SlowReplies++
			}
			d.SlowestReply = max(d.SlowestReply, f.ReplyWait)
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

	r := Result{Incomplete: len(w.why) > 0, UnansweredUnknown: unknown, RepliesTimed: !w.untimed}
	if r.Incomplete {
		r.IncompleteWhy = slices.Sorted(maps.Keys(w.why))
	}

	named := min(len(all), MaxDestinations)
	index := make(map[netip.Addr]int, named)
	for i := range named {
		index[all[i].Addr] = i
	}
	r.Openings, r.OpeningWidth = openingsOf(w.counted, index)

	if len(all) > MaxDestinations {
		for _, d := range all[MaxDestinations:] {
			r.Other.Sent = satAdd(r.Other.Sent, d.Sent)
			r.Other.Received = satAdd(r.Other.Received, d.Received)
			r.Other.Connections = satAdd(r.Other.Connections, d.Connections)
			r.Other.Unanswered = satAdd(r.Other.Unanswered, d.Unanswered)
			r.Other.TimedReplies = satAdd(r.Other.TimedReplies, d.TimedReplies)
			r.Other.SlowReplies = satAdd(r.Other.SlowReplies, d.SlowReplies)
			r.Other.SlowestReply = max(r.Other.SlowestReply, d.SlowestReply)
		}

		all = all[:MaxDestinations]
	}

	r.Destinations = all

	return r
}
