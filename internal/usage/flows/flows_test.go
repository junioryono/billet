package flows

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	// began is when the job's guest was granted its address, started a moment
	// after it, and ended when its destroy began.
	began   = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	started = began.Add(time.Second)
	ended   = began.Add(time.Hour)

	guest  = netip.MustParseAddr("192.168.100.23")
	other  = netip.MustParseAddr("192.168.100.24")
	github = netip.MustParseAddr("140.82.112.4")
	npm    = netip.MustParseAddr("104.16.24.34")
)

func flow(id uint32, src, dst netip.Addr, sent, recv uint64) Flow {
	return Flow{ID: id, Start: started, Protocol: 6, Source: src, Dest: dst, DestPort: 443,
		OrigBytes: sent, ReplyBytes: recv, OrigPackets: sent, ReplyPackets: recv}
}

func startedAt(f Flow, at time.Time) Flow {
	f.Start = at

	return f
}

func only(t *testing.T, r Result, want Destination) {
	t.Helper()

	if len(r.Destinations) != 1 || r.Destinations[0] != want {
		t.Fatalf("destinations = %+v, want only %+v", r.Destinations, want)
	}
}

// A guest's flows are totalled by destination, from the guest's side: what it
// sent is the original direction, what it received the reply.
func TestFlowsAreTotalledByDestinationFromTheGuestsSide(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	if !a.Watch("lease-1", guest, began, nil) {
		t.Fatal("an unheld address was refused")
	}

	a.Observe(flow(1, guest, github, 100, 5000))
	a.Observe(flow(2, guest, github, 50, 2000))
	a.Observe(flow(3, guest, npm, 10, 900))

	r, ok := a.Final("lease-1", nil, ended)
	if !ok {
		t.Fatal("a watched job had no result")
	}

	want := []Destination{
		{Addr: github, Sent: 150, Received: 7000, Connections: 2},
		{Addr: npm, Sent: 10, Received: 900, Connections: 1},
	}
	if len(r.Destinations) != len(want) {
		t.Fatalf("destinations = %+v, want %+v", r.Destinations, want)
	}

	for i := range want {
		if r.Destinations[i] != want[i] {
			t.Errorf("destination %d = %+v, want %+v", i, r.Destinations[i], want[i])
		}
	}

	if r.Incomplete {
		t.Error("a job with nothing missed was marked incomplete")
	}
}

// ANOTHER ADDRESS'S FLOWS ARE NEVER THIS JOB'S, nor those its own address
// opened before the grant (the previous holder's) or after its destroy began
// (the next holder's). A flow opened at the instant of the grant is its own.
func TestOnlyFlowsInsideTheJobsTimeAreItsOwn(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	a.Observe(startedAt(flow(7, guest, github, 1_000, 1_000), began.Add(-time.Hour))) // the previous holder's
	a.Observe(flow(8, other, github, 5, 5))                                           // another guest's
	a.Observe(startedAt(flow(9, guest, npm, 3, 4), began))                            // at the grant: its own
	a.Observe(flow(10, guest, npm, 1, 1))                                             // its own

	next := startedAt(flow(11, guest, npm, 500, 500), ended.Add(time.Second)) // the next holder's

	r, _ := a.Final("lease-1", []Flow{next}, ended)
	only(t, r, Destination{Addr: npm, Sent: 4, Received: 5, Connections: 2})

	if r.Incomplete {
		t.Error("a job whose flows all carried a start was marked incomplete")
	}
}

// THE DESTROY BOUND APPLIES HOWEVER A FLOW ARRIVED: a next holder's flow
// reported destroyed before Final is no more this job's than one in the dump.
func TestTheDestroyBoundAppliesToFlowsAlreadyCounted(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)
	a.Observe(startedAt(flow(1, guest, npm, 500, 500), ended.Add(time.Second)))
	a.Observe(flow(2, guest, npm, 1, 1))

	r, _ := a.Final("lease-1", nil, ended)
	only(t, r, Destination{Addr: npm, Sent: 1, Received: 1, Connections: 1})
}

// A FLOW WITHOUT A START TIME HAS NO OWNER: it is not counted, and the job is
// marked incomplete rather than silently short.
func TestAFlowWithoutAStartTimeIsNotGuessedAt(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)
	a.Observe(startedAt(flow(1, guest, github, 9, 9), time.Time{}))

	r, _ := a.Final("lease-1", nil, ended)
	if len(r.Destinations) != 0 {
		t.Errorf("an unstamped flow was counted: %+v", r.Destinations)
	}

	if !r.Incomplete {
		t.Error("a job with an unattributable flow was not marked incomplete")
	}
}

// A FLOW REPORTED TWICE IS COUNTED ONCE, at its latest reading, and an entry
// is its id and its start together: a later entry reusing an id is another
// connection.
func TestAFlowIsCountedOnceAtItsLatestReading(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	a.Observe(flow(1, guest, github, 10, 100))
	a.Observe(flow(1, guest, github, 40, 400))
	a.Observe(flow(1, guest, github, 20, 200)) // an older reading, arriving late

	r, _ := a.Final("lease-1", nil, ended)
	only(t, r, Destination{Addr: github, Sent: 40, Received: 400, Connections: 1})

	a.Watch("lease-2", guest, began, nil)
	a.Observe(flow(2, guest, npm, 5, 50))
	a.Observe(startedAt(flow(2, guest, npm, 1, 1), started.Add(time.Minute))) // the id, reused

	r, _ = a.Final("lease-2", []Flow{flow(2, guest, npm, 5, 50)}, ended)
	only(t, r, Destination{Addr: npm, Sent: 6, Received: 51, Connections: 2})
}

// TRAFFIC THE GUEST ADDS TO AN ENTRY ITS ADDRESS'S PREVIOUS HOLDER LEFT cannot
// be counted, since the entry's start is the old holder's, so it marks the job
// incomplete. Only what the guest sent says so: a remote goes on answering a
// guest that has left.
func TestTrafficOnAnInheritedEntryMarksTheJobIncomplete(t *testing.T) {
	t.Parallel()

	inherited := startedAt(flow(5, guest, github, 100, 100), began.Add(-time.Minute))

	a := NewAccountant()
	a.Watch("answered", guest, began, []Flow{inherited})

	repliedTo := inherited
	repliedTo.ReplyBytes, repliedTo.ReplyPackets = 300, 300

	r, _ := a.Final("answered", []Flow{repliedTo}, ended)
	if r.Incomplete || len(r.Destinations) != 0 {
		t.Errorf("replies to the old holder made the job incomplete or were counted: %+v", r)
	}

	a.Watch("sent", guest, began, []Flow{inherited})

	sentOn := inherited
	sentOn.OrigBytes, sentOn.OrigPackets = 150, 150
	a.Observe(sentOn)

	r, _ = a.Final("sent", nil, ended)
	if !r.Incomplete {
		t.Error("traffic the guest sent on an inherited entry did not mark the job incomplete")
	}
}

// TWO JOBS CANNOT HOLD ONE ADDRESS, and an invalid address is not one: the
// second Watch is refused rather than guessing whose a flow is.
func TestAnAddressIsWatchedForOneJobAtATime(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	if !a.Watch("lease-1", guest, began, nil) {
		t.Fatal("first watch refused")
	}

	if a.Watch("lease-2", guest, began, nil) {
		t.Error("a second job was given an address another job holds")
	}

	if a.Watch("lease-3", netip.Addr{}, began, nil) {
		t.Error("an invalid address was watched")
	}

	a.Observe(flow(1, guest, github, 1, 1))

	if _, ok := a.Final("lease-2", nil, ended); ok {
		t.Error("a refused watch produced a result")
	}

	if r, ok := a.Final("lease-1", nil, ended); !ok || len(r.Destinations) != 1 {
		t.Errorf("the address's holder lost its flow: %+v", r)
	}

	if !a.Watch("lease-2", guest, began, nil) {
		t.Error("an address stayed held after its job finished")
	}

	if _, ok := a.Final("lease-9", nil, ended); ok {
		t.Error("a job never watched produced a result")
	}
}

// AN IPv4-MAPPED ADDRESS IS THE SAME GUEST, either way round.
func TestAMappedAddressIsTheSameGuest(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", netip.AddrFrom16(guest.As16()), began, nil)
	a.Observe(flow(1, guest, github, 1, 2))

	if r, _ := a.Final("lease-1", nil, ended); len(r.Destinations) != 1 {
		t.Fatalf("a flow from the plain spelling of a mapped address was lost: %+v", r)
	}

	a.Watch("lease-2", guest, began, nil)
	a.Observe(flow(2, netip.AddrFrom16(guest.As16()), github, 1, 2))

	if r, _ := a.Final("lease-2", nil, ended); len(r.Destinations) != 1 {
		t.Fatalf("a flow reported with the mapped spelling of a plain address was lost: %+v", r)
	}
}

// PAST MaxDestinations THE BYTES ARE STILL COUNTED, under Other, and the
// largest destinations keep their names.
func TestDestinationsBeyondTheCapFoldIntoOther(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	var total uint64

	for i := range MaxDestinations + 10 {
		dst := netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)})
		bytes := uint64(i + 1)
		total += 2 * bytes
		a.Observe(flow(uint32(i+1), guest, dst, bytes, bytes))
	}

	r, _ := a.Final("lease-1", nil, ended)
	if len(r.Destinations) != MaxDestinations {
		t.Fatalf("%d destinations kept, want %d", len(r.Destinations), MaxDestinations)
	}

	if r.Other.Connections != 10 {
		t.Errorf("Other holds %d connections, want the 10 beyond the cap", r.Other.Connections)
	}

	var sum uint64
	for _, d := range r.Destinations {
		sum += d.Sent + d.Received
	}

	if sum+r.Other.Sent+r.Other.Received != total {
		t.Errorf("kept %d + other %d bytes, want every byte (%d)", sum, r.Other.Sent+r.Other.Received, total)
	}

	if r.Destinations[0].Sent != uint64(MaxDestinations+10) {
		t.Errorf("first destination sent %d, want the largest", r.Destinations[0].Sent)
	}
}

// A CONNECTION THAT GOT NOTHING BACK IS COUNTED AGAINST ITS DESTINATION: a SYN
// no one answered, or a DNS query with no reply, is the stall a job's own log
// cannot place. One that was answered, even with nothing but a reset, is not.
// A flow the tracker kept no packet counts for says nothing either way, so a
// result holding one cannot say what went unanswered anywhere. Beyond the cap
// the count is kept in Other.
func TestAConnectionThatGotNothingBackIsCountedAgainstItsDestination(t *testing.T) {
	t.Parallel()

	resolver := netip.MustParseAddr("172.31.1.1")
	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	a.Observe(flow(1, guest, github, 120, 4000))
	stalled := flow(2, guest, github, 180, 0)
	stalled.OrigPackets = 3
	a.Observe(stalled)
	reset := flow(3, guest, github, 60, 40)
	reset.OrigPackets, reset.ReplyPackets = 1, 1
	a.Observe(reset)
	query := flow(4, guest, resolver, 60, 0)
	query.Protocol, query.DestPort, query.OrigPackets = 17, 53, 1
	a.Observe(query)

	r, _ := a.Final("lease-1", nil, ended)
	if r.UnansweredUnknown {
		t.Errorf("a result whose every flow carried packet counts says it cannot tell what went unanswered")
	}
	want := map[netip.Addr]Destination{
		github:   {Addr: github, Sent: 360, Received: 4040, Connections: 3, Unanswered: 1},
		resolver: {Addr: resolver, Sent: 60, Connections: 1, Unanswered: 1},
	}
	if len(r.Destinations) != len(want) {
		t.Fatalf("destinations = %+v, want %+v", r.Destinations, want)
	}
	for _, d := range r.Destinations {
		if d != want[d.Addr] {
			t.Errorf("destination %s = %+v, want %+v", d.Addr, d, want[d.Addr])
		}
	}

	unknown := NewAccountant()
	unknown.Watch("lease-3", guest, began, nil)
	unknown.Observe(stalled)
	unknown.Observe(flow(5, guest, npm, 0, 0))
	if r, _ := unknown.Final("lease-3", nil, ended); !r.UnansweredUnknown {
		t.Errorf("a result holding a flow with no packet counts says it could tell what went unanswered: %+v", r)
	}

	// AN ENTRY THE PREVIOUS HOLDER LEFT OPEN WITHOUT PACKET COUNTS hides the
	// guest's reuse of its tuple, so a stall on it could not be noticed and the
	// result cannot say none went unanswered. One already destroyed before the
	// job was watched, replayed from the unclaimed ring, is no tuple the guest
	// can send on and takes nothing from the job's counts, and neither does one
	// left open with its counts.
	counterless := startedAt(flow(6, guest, npm, 0, 0), began.Add(-time.Minute))
	for name, tc := range map[string]struct {
		destroyedBefore, openAtWatch []Flow
		unknown                      bool
	}{
		"open when watched":        {openAtWatch: []Flow{counterless}, unknown: true},
		"destroyed before watched": {destroyedBefore: []Flow{counterless}},
		"left open with counts": {openAtWatch: []Flow{
			startedAt(flow(6, guest, npm, 5, 5), began.Add(-time.Minute))}},
	} {
		inherits := NewAccountant()
		for _, f := range tc.destroyedBefore {
			inherits.Observe(f)
		}
		inherits.Watch("lease-4", guest, began, tc.openAtWatch)
		inherits.Observe(stalled)
		r, _ := inherits.Final("lease-4", nil, ended)
		if r.UnansweredUnknown != tc.unknown {
			t.Errorf("an inherited entry %s: UnansweredUnknown = %v, want %v", name, r.UnansweredUnknown, tc.unknown)
		}
		if !tc.unknown && (len(r.Destinations) != 1 || r.Destinations[0].Unanswered != 1) {
			t.Errorf("an inherited entry %s took the job's own stall from it: %+v", name, r.Destinations)
		}
	}

	// A COUNTERLESS FLOW THE UNCLAIMED RING DROPPED before the job was
	// watched, and after its grant, could have been its stall; one dropped
	// from before the grant was another holder's.
	for name, tc := range map[string]struct {
		start   time.Time
		unknown bool
	}{
		"started after the grant":  {started, true},
		"started before the grant": {began.Add(-time.Minute), false},
	} {
		ring := NewAccountant()
		ring.Observe(startedAt(flow(8, guest, npm, 0, 0), tc.start))
		for i := range MaxUnclaimed {
			ring.Observe(startedAt(flow(uint32(100+i), other, github, 1, 1), tc.start))
		}
		ring.Watch("lease-7", guest, began, nil)
		if r, _ := ring.Final("lease-7", nil, ended); r.UnansweredUnknown != tc.unknown {
			t.Errorf("a counterless flow the ring dropped, %s: UnansweredUnknown = %v, want %v", name,
				r.UnansweredUnknown, tc.unknown)
		}
	}

	// A FLOW DROPPED UNCOUNTED, for want of a start time or past MaxFlows, that
	// carried no packet counts could have been the stall: the result says it
	// cannot tell, not only that it is incomplete.
	unstamped := flow(7, guest, npm, 0, 0)
	unstamped.Start = time.Time{}
	full := NewAccountant()
	full.Watch("lease-5", guest, began, nil)
	for i := range MaxFlows {
		full.Observe(flow(uint32(i+1), guest, github, 1, 1))
	}
	full.Observe(flow(MaxFlows+1, guest, npm, 0, 0))
	stampless := NewAccountant()
	stampless.Watch("lease-6", guest, began, nil)
	stampless.Observe(unstamped)
	for name, r := range map[string]Result{
		"past MaxFlows":      func() Result { r, _ := full.Final("lease-5", nil, ended); return r }(),
		"with no start time": func() Result { r, _ := stampless.Final("lease-6", nil, ended); return r }(),
	} {
		if !r.Incomplete || !r.UnansweredUnknown {
			t.Errorf("a counterless flow dropped %s: incomplete %v, unanswered unknown %v, want both", name,
				r.Incomplete, r.UnansweredUnknown)
		}
	}

	beyond := NewAccountant()
	beyond.Watch("lease-2", guest, began, nil)
	for i := range MaxDestinations + 3 {
		dst := netip.AddrFrom4([4]byte{10, 2, byte(i / 256), byte(i % 256)})
		// The answered ones are larger, so the three unanswered fall past the cap.
		f := flow(uint32(i+1), guest, dst, 1000, 1000)
		if i >= MaxDestinations {
			f = flow(uint32(i+1), guest, dst, 1, 0)
		}
		beyond.Observe(f)
	}
	r, _ = beyond.Final("lease-2", nil, ended)
	if r.Other.Unanswered != 3 || r.Other.Connections != 3 {
		t.Errorf("Other = %+v, want the three unanswered connections beyond the cap", r.Other)
	}
}

// PAST MaxFlows A JOB IS MARKED INCOMPLETE, never silently short.
func TestAJobOpeningTooManyFlowsIsIncomplete(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began, nil)

	for i := range MaxFlows + 1 {
		a.Observe(flow(uint32(i+1), guest, github, 1, 1))
	}

	r, _ := a.Final("lease-1", nil, ended)
	if !r.Incomplete {
		t.Error("a job with more flows than are counted was not marked incomplete")
	}

	if got := r.Destinations[0].Connections; got != MaxFlows {
		t.Errorf("%d connections counted, want %d", got, MaxFlows)
	}
}

// A GAP IN THE EVENTS MARKS EVERY JOB WHOSE TIME OVERLAPS IT: one watched
// when it opened, one finishing while it is open, one whose grant came before
// it closed, even one whose address was learned only afterwards. A job whose
// whole time follows the gap is complete.
func TestAGapInTheEventsMarksEveryJobItOverlaps(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("watched", guest, began, nil)
	a.Down()

	if r, _ := a.Final("watched", nil, ended); !r.Incomplete {
		t.Error("a job watched when the events stopped was not marked incomplete")
	}

	a.Watch("during", other, began, nil)

	if r, _ := a.Final("during", nil, ended); !r.Incomplete {
		t.Error("a job watched while the events were down was not marked incomplete")
	}

	a.Up(began.Add(time.Minute))

	a.Watch("granted-before", guest, began, nil)

	if r, _ := a.Final("granted-before", nil, ended); !r.Incomplete {
		t.Error("a job granted its address before the gap closed was not marked incomplete")
	}

	a.Watch("after", guest, began.Add(2*time.Minute), nil)

	if r, _ := a.Final("after", nil, ended); r.Incomplete {
		t.Error("a job granted after the gap closed was marked incomplete")
	}
}

// EACH WAY A GAP REACHES A JOB, on its own: Down marks a job already watched
// even once the gap has closed, and a job watched during the gap is marked
// even if the gap is said to have closed before its grant.
func TestEachWayAGapReachesAJob(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Up(began.Add(-time.Hour))
	a.Watch("watched-then-gap", guest, began, nil)
	a.Down()
	a.Up(began.Add(time.Minute))

	if r, _ := a.Final("watched-then-gap", nil, ended); !r.Incomplete {
		t.Error("a job watched when the events stopped was complete once they resumed")
	}

	a.Down()
	a.Watch("watched-in-gap", guest, began, nil)
	a.Up(began.Add(-time.Second))

	if r, _ := a.Final("watched-in-gap", nil, ended); !r.Incomplete {
		t.Error("a job watched while the events were down was complete")
	}
}

// A FLOW THAT ENDED BEFORE ITS ADDRESS WAS WATCHED is replayed to the job that
// learns the address, if it started after the grant; the previous holder's is
// not, nor another address's.
func TestUnclaimedFlowsAreReplayedToTheJobThatStartedThem(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Up(began.Add(-time.Hour))

	a.Observe(startedAt(flow(1, guest, github, 1_000, 1_000), began.Add(-time.Minute)))
	a.Observe(flow(2, guest, npm, 3, 30))
	a.Observe(flow(3, other, npm, 9, 9))

	a.Watch("lease-1", guest, began, nil)

	r, _ := a.Final("lease-1", nil, ended)
	only(t, r, Destination{Addr: npm, Sent: 3, Received: 30, Connections: 1})

	if r.Incomplete {
		t.Error("a replay with nothing dropped was marked incomplete")
	}
}

// THE REPLAY BUFFER IS BOUNDED, and a flow it dropped that a job may have
// owned (one that started after the job's grant) marks the job incomplete; a
// dropped flow from before the grant does not.
func TestTheReplayBufferIsBoundedAndSaysWhatItDropped(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Up(began.Add(-time.Hour))

	a.Observe(startedAt(flow(1, guest, github, 1, 1), began.Add(-time.Minute))) // dropped below

	for i := range MaxUnclaimed {
		a.Observe(startedAt(flow(uint32(i+2), other, npm, 1, 1), began.Add(-time.Minute)))
	}

	if len(a.unclaimed) != MaxUnclaimed {
		t.Fatalf("the buffer holds %d flows, want at most %d", len(a.unclaimed), MaxUnclaimed)
	}

	a.Watch("older-dropped", guest, began, nil)

	if r, _ := a.Final("older-dropped", nil, ended); r.Incomplete {
		t.Error("dropping a flow from before the grant marked the job incomplete")
	}

	a.Observe(flow(MaxUnclaimed+5, guest, github, 1, 1)) // started after the grant
	for i := range MaxUnclaimed {
		a.Observe(flow(uint32(MaxUnclaimed+10+i), other, npm, 1, 1))
	}

	a.Watch("newer-dropped", guest, began, nil)

	if r, _ := a.Final("newer-dropped", nil, ended); !r.Incomplete {
		t.Error("a dropped flow the job may have owned did not mark it incomplete")
	}
}

// AN UNSTAMPED FLOW THE RING DROPPED could have been any job's: every job
// granted before the drop is marked incomplete.
func TestDroppingAnUnstampedFlowMarksTheJobsItCouldHaveBeen(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.now = func() time.Time { return began.Add(time.Minute) }
	a.Up(began.Add(-time.Hour))

	a.Observe(startedAt(flow(1, guest, github, 1, 1), time.Time{}))

	for i := range MaxUnclaimed {
		a.Observe(startedAt(flow(uint32(i+2), other, npm, 1, 1), began.Add(-time.Minute)))
	}

	a.Watch("lease-1", guest, began, nil)

	if r, _ := a.Final("lease-1", nil, ended); !r.Incomplete {
		t.Error("a dropped unstamped flow did not mark a job granted before the drop incomplete")
	}
}

func TestMarkIncompleteMarksOnlyThatJob(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("marked", guest, began, nil)
	a.Watch("other", other, began, nil)
	a.MarkIncomplete("marked")
	a.MarkIncomplete("never-watched")

	if r, _ := a.Final("marked", nil, ended); !r.Incomplete {
		t.Error("a marked job was complete")
	}

	if r, _ := a.Final("other", nil, ended); r.Incomplete {
		t.Error("marking one job marked another")
	}
}

func TestAddressForReadsTheGuestsLease(t *testing.T) {
	t.Parallel()

	mac := net.HardwareAddr{0x06, 0x00, 0xac, 0x10, 0x00, 0x17}
	file := "1760000000 06:00:ac:10:00:16 192.168.100.22 * 01:06:00:ac:10:00:16\n" +
		"1760000000 06:00:AC:10:00:17 192.168.100.23 runner *\n" +
		"1760000000 * 192.168.100.30 * *\n" +
		"1760000000 20-06:00:ac:10:00:99 192.168.100.31 * *\n"

	addr, err := AddressFor([]byte(file), mac)
	if err != nil || addr != guest {
		t.Fatalf("AddressFor = %v, %v; want %v", addr, err, guest)
	}

	if _, err := AddressFor([]byte(file), net.HardwareAddr{1, 2, 3, 4, 5, 6}); !errors.Is(err, ErrNoAddress) {
		t.Errorf("an absent MAC answered %v, want ErrNoAddress", err)
	}

	if _, err := AddressFor(nil, mac); !errors.Is(err, ErrNoAddress) {
		t.Errorf("an empty file answered %v, want ErrNoAddress", err)
	}
}

// A FILE THIS READER DOES NOT UNDERSTAND IS NOT A FILE WITHOUT THE LEASE:
// each is an error, never ErrNoAddress.
func TestAnUnreadableLeaseFileIsNotAnAbsentLease(t *testing.T) {
	t.Parallel()

	mac := net.HardwareAddr{0x06, 0x00, 0xac, 0x10, 0x00, 0x17}

	for name, file := range map[string]string{
		"short line":     "1760000000 06:00:ac:10:00:17\n",
		"bad mac":        "1760000000 zz:zz 192.168.100.23 * *\n",
		"bad address":    "1760000000 06:00:ac:10:00:17 not-an-ip * *\n",
		"infinite lease": "0 06:00:ac:10:00:17 192.168.100.23 * *\n",
		"garbled expiry": "soon 06:00:ac:10:00:17 192.168.100.23 * *\n",
		"two addresses": "1760000000 06:00:ac:10:00:17 192.168.100.23 * *\n" +
			"1760000000 06:00:ac:10:00:17 192.168.100.24 * *\n",
	} {
		_, err := AddressFor([]byte(file), mac)
		if err == nil || errors.Is(err, ErrNoAddress) {
			t.Errorf("%s: answered %v, want an error that is not ErrNoAddress", name, err)
		}
	}
}

func TestReadyNeedsBothSwitchesAndTellsUnreadableFromOff(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, values map[string]string) string {
		t.Helper()

		root := t.TempDir()
		dir := filepath.Join(root, "proc", "sys", "net", "netfilter")

		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		for name, v := range values {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		return root
	}

	if err := Ready(write(t, map[string]string{"nf_conntrack_acct": "1\n", "nf_conntrack_timestamp": "1\n"})); err != nil {
		t.Errorf("both switches on: %v", err)
	}

	for name, tc := range map[string]struct {
		values map[string]string
		says   string
	}{
		"accounting off":    {map[string]string{"nf_conntrack_acct": "0\n", "nf_conntrack_timestamp": "1\n"}, "nf_conntrack_acct is 0"},
		"timestamps off":    {map[string]string{"nf_conntrack_acct": "1\n", "nf_conntrack_timestamp": "0\n"}, "nf_conntrack_timestamp is 0"},
		"timestamps absent": {map[string]string{"nf_conntrack_acct": "1\n"}, "could not tell whether nf_conntrack_timestamp"},
		"garbled":           {map[string]string{"nf_conntrack_acct": "2\n", "nf_conntrack_timestamp": "1\n"}, "could not tell whether nf_conntrack_acct"},
		"empty":             {map[string]string{"nf_conntrack_acct": "", "nf_conntrack_timestamp": "1\n"}, "could not tell whether nf_conntrack_acct"},
	} {
		err := Ready(write(t, tc.values))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: Ready = %v, want an error saying %q", name, err, tc.says)
		}
	}
}

// A FLOW'S WAIT FOR ITS FIRST REPLY IS TIMED FROM ITS NEW EVENT TO ITS FIRST
// EVENT THAT SAW A REPLY, and counted against its destination: a slow answer
// is the stall nothing else records, because nothing went unanswered. A flow
// that was never answered, one whose new event was not read, and one whose
// start is not the moment its new event was read (an id reused across the two
// sockets) are left untimed rather than given a wait that is not theirs; and
// only the first reply is the wait, every later update being the flow moving
// on.
func TestAFlowsFirstReplyIsTimedAgainstItsDestination(t *testing.T) {
	t.Parallel()

	resolver := netip.MustParseAddr("172.31.1.1")
	a := NewAccountant()
	a.RepliesUp(began.Add(-time.Hour))
	a.Watch("lease-1", guest, began, nil)

	answer := func(id uint32, dst netip.Addr, wait time.Duration) Flow {
		a.ObserveNew(id, guest, started)
		a.ObserveReply(id, guest, started.Add(wait))
		a.ObserveReply(id, guest, started.Add(wait+time.Minute))

		return flow(id, guest, dst, 100, 100)
	}
	a.Observe(answer(1, github, 40*time.Millisecond))
	a.Observe(answer(2, github, 2500*time.Millisecond))
	a.Observe(answer(3, resolver, 1600*time.Millisecond))
	a.ObserveNew(4, guest, started)
	a.Observe(flow(4, guest, github, 60, 0))
	a.Observe(flow(5, guest, github, 100, 100))
	a.ObserveNew(6, guest, started.Add(10*time.Second))
	a.ObserveReply(6, guest, started.Add(10*time.Second+time.Millisecond))
	a.Observe(flow(6, guest, github, 100, 100))
	// A READING WITH NOTHING BACK IN IT, beside a reply event, is counted as the
	// reading says, unanswered, and not also as a timed reply.
	stale := answer(7, resolver, 50*time.Millisecond)
	stale.ReplyBytes, stale.ReplyPackets = 0, 0
	a.Observe(stale)

	r, _ := a.Final("lease-1", nil, ended)
	if !r.RepliesTimed {
		t.Fatal("a job watched while the reply events were read is not timed")
	}
	want := map[netip.Addr]Destination{
		github: {Addr: github, Sent: 460, Received: 400, Connections: 5, Unanswered: 1,
			TimedReplies: 2, SlowReplies: 1, SlowestReply: 2500 * time.Millisecond},
		resolver: {Addr: resolver, Sent: 200, Received: 100, Connections: 2, Unanswered: 1,
			TimedReplies: 1, SlowReplies: 1, SlowestReply: 1600 * time.Millisecond},
	}
	if len(r.Destinations) != len(want) {
		t.Fatalf("destinations = %+v, want %+v", r.Destinations, want)
	}
	for _, d := range r.Destinations {
		if d != want[d.Addr] {
			t.Errorf("destination %s = %+v,\nwant %+v", d.Addr, d, want[d.Addr])
		}
	}

	// AN ID REUSED WITHIN A SECOND never lends one flow's wait to the other. An
	// earlier flow destroyed before the reuse was read is timed by the only wait
	// there was, its own; once both waits are held, either could be either
	// flow's, so a flow read then is left untimed.
	for name, order := range map[string]bool{"destroy first": true, "reply first": false} {
		reused := NewAccountant()
		reused.RepliesUp(began.Add(-time.Hour))
		reused.Watch("lease-r", guest, began, nil)
		first := flow(9, guest, github, 100, 100)
		second := startedAt(flow(9, guest, npm, 100, 100), started.Add(100*time.Millisecond))
		reused.ObserveNew(9, guest, started)
		reused.ObserveReply(9, guest, started.Add(20*time.Millisecond))
		if order {
			reused.Observe(first)
		}
		reused.ObserveNew(9, guest, second.Start)
		reused.ObserveReply(9, guest, second.Start.Add(50*time.Millisecond))
		if !order {
			reused.Observe(first)
		}
		reused.Observe(second)
		r, _ := reused.Final("lease-r", nil, ended)
		for _, d := range r.Destinations {
			ownWait := d.Addr == github && order
			switch {
			case ownWait && (d.TimedReplies != 1 || d.SlowestReply != 20*time.Millisecond):
				t.Errorf("%s: the flow destroyed before the reuse lost its own wait: %+v", name, d)
			case !ownWait && d.TimedReplies != 0:
				t.Errorf("%s: an id reused within a second left %s timed: %+v", name, d.Addr, d)
			}
		}
	}

	// BEYOND THE CAP the timed counts are summed under Other and its slowest
	// wait is the slowest of theirs.
	beyond := NewAccountant()
	beyond.RepliesUp(began.Add(-time.Hour))
	beyond.Watch("lease-2", guest, began, nil)
	for i := range MaxDestinations + 2 {
		dst := netip.AddrFrom4([4]byte{10, 3, byte(i / 256), byte(i % 256)})
		f := flow(uint32(i+1), guest, dst, 1000, 1000)
		if i >= MaxDestinations {
			f = flow(uint32(i+1), guest, dst, 1, 1)
			beyond.ObserveNew(f.ID, guest, started)
			beyond.ObserveReply(f.ID, guest, started.Add(time.Duration(i-MaxDestinations)*time.Second+1))
		}
		beyond.Observe(f)
	}
	r, _ = beyond.Final("lease-2", nil, ended)
	if r.Other.TimedReplies != 2 || r.Other.SlowReplies != 1 || r.Other.SlowestReply != time.Second+1 {
		t.Errorf("Other = %+v, want two timed replies beyond the cap, one slow, the slowest just over 1s",
			r.Other)
	}
}

// A JOB IS TIMED ONLY WHILE THE REPLY EVENTS WERE READ FOR ALL OF ITS TIME: an
// accountant no listener ever fed, a gap that closed after the job's grant,
// and a gap during the job each leave it untimed, which a report says as not
// timed rather than as no slow replies.
func TestAJobIsTimedOnlyWhileTheReplyEventsWereRead(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prepare, during func(*Accountant)
		timed           bool
	}{
		"read throughout":                   {prepare: func(a *Accountant) { a.RepliesUp(began.Add(-time.Hour)) }, timed: true},
		"no listener ever read them":        {},
		"a gap that closed after the grant": {prepare: func(a *Accountant) { a.RepliesUp(began.Add(time.Second)) }},
		"a gap during the job": {prepare: func(a *Accountant) { a.RepliesUp(began.Add(-time.Hour)) },
			during: func(a *Accountant) { a.RepliesDown(); a.RepliesUp(started.Add(time.Minute)) }},
	} {
		a := NewAccountant()
		if tc.prepare != nil {
			tc.prepare(a)
		}
		a.Watch("lease", guest, began, nil)
		if tc.during != nil {
			tc.during(a)
		}
		if r, _ := a.Final("lease", nil, ended); r.RepliesTimed != tc.timed {
			t.Errorf("%s: RepliesTimed = %v, want %v", name, r.RepliesTimed, tc.timed)
		}
	}
}
