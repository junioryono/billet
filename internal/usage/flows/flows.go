// Package flows attributes a host's connection-tracking flows to the jobs whose
// guests opened them, totalled by destination.
//
// The numbers are the host's, never the guest's: a job is root in its guest and
// could report any destinations it liked, but the kernel's connection tracker
// sees every flow a guest's address opens through the host. A flow is the
// guest's when its original source is the address the guest holds and the
// tracker stamped its start at or after the job's: an entry the address's
// previous holder opened outlives that holder's VM, and is never this job's.
package flows

import (
	"cmp"
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

// MaxUnclaimed is how many flows from addresses no job is watched at are kept,
// newest first, for a job that learns its address after the guest opened and
// closed them: a guest asks DHCP for its address after it boots, and the
// tracker reports a flow's end only once.
const MaxUnclaimed = 4096

// Flow is one tracked connection, in the tracker's own terms: Orig is the
// direction the guest opened, Reply the other.
type Flow struct {
	// ID is the tracker's id for the entry, unique while the entry exists.
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

// Destination is one remote address a job exchanged traffic with.
type Destination struct {
	Addr netip.Addr
	// Sent and Received are from the guest's side.
	Sent, Received uint64
	Connections    uint64
}

// Result is what a job's flows came to.
type Result struct {
	// Destinations, largest total first, at most MaxDestinations.
	Destinations []Destination
	// Other holds the traffic to every destination beyond MaxDestinations; its
	// Addr is the zero address.
	Other Destination
	// Incomplete says flows may have been missed while this job was watched:
	// the tracker dropped events this node did not read in time, the job opened
	// more than MaxFlows, or a flow from its address carried no start time and
	// so could not be told apart from a previous holder's. The totals are then a
	// lower bound, and say so.
	Incomplete bool
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
}

type watch struct {
	addr  netip.Addr
	since time.Time
	// counted holds each flow's last reading, so a flow reported more than once
	// (an update, then its destruction, or a flow still open at Final) is
	// counted at its latest totals and never twice.
	counted    map[uint32]Flow
	incomplete bool
}

// NewAccountant returns an Accountant watching nothing.
func NewAccountant() *Accountant {
	return &Accountant{byKey: map[string]*watch{}, byAddr: map[netip.Addr]*watch{}}
}

// Watch starts attributing flows from addr to the job named key, counting only
// flows the tracker started at or after since, the moment the job's guest was
// started. The address may be learned after the guest has opened flows from it;
// since, not the moment of the call, is what separates them from the previous
// holder's. It reports false, attributing nothing, when addr is not a valid
// address or another watched job holds it: two jobs cannot both own an address,
// and guessing which one a flow is would be a fabrication.
func (a *Accountant) Watch(key string, addr netip.Addr, since time.Time) bool {
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

	w := &watch{addr: addr, since: since, counted: map[uint32]Flow{}}

	a.byKey[key] = w
	a.byAddr[addr] = w

	// FLOWS THAT ENDED BEFORE THE ADDRESS WAS KNOWN are the job's if they
	// started after it did, and the start time is what keeps the previous
	// holder's out.
	for i := range a.unclaimed {
		if a.unclaimed[i].Source == addr {
			a.observe(a.unclaimed[i])
		}
	}

	return true
}

// Observe records one reading of a flow, from an update, its destruction or a
// dump. A flow from no watched address, or one that predates its address's
// watch, is ignored.
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

		return
	}

	if f.Start.Before(w.since) {
		return
	}

	prev, seen := w.counted[f.ID]
	if !seen && len(w.counted) >= MaxFlows {
		w.incomplete = true

		return
	}

	// A flow's counters only grow. A reading behind the one held is an older
	// report arriving late, and is not allowed to shrink the total.
	if seen && f.OrigBytes+f.ReplyBytes < prev.OrigBytes+prev.ReplyBytes {
		return
	}

	w.counted[f.ID] = f
}

func (a *Accountant) keepUnclaimed(f Flow) {
	if len(a.unclaimed) < MaxUnclaimed {
		a.unclaimed = append(a.unclaimed, f)

		return
	}

	a.unclaimed[a.next] = f
	a.next = (a.next + 1) % MaxUnclaimed
}

// Lost marks every job watched now as possibly missing flows: the tracker
// dropped events the node did not read in time.
func (a *Accountant) Lost() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, w := range a.byKey {
		w.incomplete = true
	}
}

// Final stops watching key and returns what its flows came to. open are the
// flows the tracker still holds for the job's address at that moment, read
// after the job's guest was stopped; they are counted at those readings. It
// reports false when key was never watched.
func (a *Accountant) Final(key string, open []Flow) (Result, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w, ok := a.byKey[key]
	if !ok {
		return Result{}, false
	}

	for i := range open {
		a.observe(open[i])
	}

	delete(a.byKey, key)
	delete(a.byAddr, w.addr)

	return summarize(w), true
}

func summarize(w *watch) Result {
	byDest := map[netip.Addr]*Destination{}

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

	r := Result{Incomplete: w.incomplete}

	if len(all) > MaxDestinations {
		for _, d := range all[MaxDestinations:] {
			r.Other.Sent += d.Sent
			r.Other.Received += d.Received
			r.Other.Connections += d.Connections
		}

		all = all[:MaxDestinations]
	}

	r.Destinations = all

	return r
}
