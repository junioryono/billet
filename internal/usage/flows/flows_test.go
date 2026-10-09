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
	// began is when the job's guest started; started is a moment after it.
	began   = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	started = began.Add(time.Second)
	guest   = netip.MustParseAddr("192.168.100.23")
	other   = netip.MustParseAddr("192.168.100.24")
	github  = netip.MustParseAddr("140.82.112.4")
	npm     = netip.MustParseAddr("104.16.24.34")
)

func flow(id uint32, src, dst netip.Addr, sent, recv uint64) Flow {
	return Flow{ID: id, Start: started, Protocol: 6, Source: src, Dest: dst, DestPort: 443, OrigBytes: sent, ReplyBytes: recv}
}

// A guest's flows are totalled by destination, from the guest's side: what it
// sent is the original direction, what it received the reply.
func TestFlowsAreTotalledByDestinationFromTheGuestsSide(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	if !a.Watch("lease-1", guest, began) {
		t.Fatal("an unheld address was refused")
	}

	a.Observe(flow(1, guest, github, 100, 5000))
	a.Observe(flow(2, guest, github, 50, 2000))
	a.Observe(flow(3, guest, npm, 10, 900))

	r, ok := a.Final("lease-1", nil)
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
		t.Error("a job with no lost events was marked incomplete")
	}
}

// ANOTHER ADDRESS'S FLOWS ARE NEVER THIS JOB'S, and neither are the flows its
// own address opened before the guest started: those belong to the address's
// previous holder, whose tracker entries outlive its VM. The test watches only
// after the guest's own flow was reported, as a node does when it learns the
// guest's address from DHCP after boot, and the guest's flow still counts.
func TestOnlyFlowsTheGuestOpenedAreItsOwn(t *testing.T) {
	t.Parallel()

	a := NewAccountant()

	previous := flow(7, guest, github, 1_000_000, 1_000_000)
	previous.Start = began.Add(-time.Hour)

	a.Watch("lease-1", guest, began)

	a.Observe(previous)                     // the previous holder's
	a.Observe(flow(8, other, github, 5, 5)) // another guest's
	a.Observe(flow(9, guest, npm, 3, 4))    // this guest's

	atStart := flow(10, guest, npm, 1, 1)
	atStart.Start = began
	a.Observe(atStart) // opened the instant the guest started: its own

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 1 || r.Destinations[0] != (Destination{Addr: npm, Sent: 4, Received: 5, Connections: 2}) {
		t.Fatalf("destinations = %+v, want only the two flows this guest opened", r.Destinations)
	}

	if r.Incomplete {
		t.Error("a job whose flows all carried a start was marked incomplete")
	}
}

// A FLOW WITHOUT A START TIME HAS NO OWNER: it is not counted, and the job is
// marked incomplete rather than silently short.
func TestAFlowWithoutAStartTimeIsNotGuessedAt(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began)

	unstamped := flow(1, guest, github, 9, 9)
	unstamped.Start = time.Time{}
	a.Observe(unstamped)

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 0 {
		t.Errorf("an unstamped flow was counted: %+v", r.Destinations)
	}

	if !r.Incomplete {
		t.Error("a job with an unattributable flow was not marked incomplete")
	}
}

// A FLOW REPORTED TWICE IS COUNTED ONCE, at its latest reading: still open at
// Final and destroyed just before, or read late after a newer reading arrived.
func TestAFlowIsCountedOnceAtItsLatestReading(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began)

	a.Observe(flow(1, guest, github, 10, 100))
	a.Observe(flow(1, guest, github, 40, 400))
	a.Observe(flow(1, guest, github, 20, 200)) // an older reading, arriving late

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 1 {
		t.Fatalf("destinations = %+v", r.Destinations)
	}

	if d := r.Destinations[0]; d.Sent != 40 || d.Received != 400 || d.Connections != 1 {
		t.Errorf("destination = %+v, want one connection at its latest reading (40 sent, 400 received)", d)
	}

	// And a flow still open at Final that was also reported destroyed is one
	// connection, not two.
	a.Watch("lease-2", guest, began)
	a.Observe(flow(2, guest, npm, 5, 50))

	r, _ = a.Final("lease-2", []Flow{flow(2, guest, npm, 5, 50)})
	if len(r.Destinations) != 1 || r.Destinations[0].Connections != 1 || r.Destinations[0].Received != 50 {
		t.Errorf("destinations = %+v, want one connection counted once", r.Destinations)
	}
}

// Flows still open when the job ends are counted at their readings then.
func TestFlowsStillOpenAtTheEndAreCounted(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began)

	r, _ := a.Final("lease-1", []Flow{flow(4, guest, npm, 7, 70), flow(5, other, npm, 1, 1)})
	if len(r.Destinations) != 1 || r.Destinations[0] != (Destination{Addr: npm, Sent: 7, Received: 70, Connections: 1}) {
		t.Fatalf("destinations = %+v, want the guest's open flow and not another's", r.Destinations)
	}
}

// TWO JOBS CANNOT HOLD ONE ADDRESS, and an invalid address is not one: the
// second Watch is refused rather than guessing whose a flow is.
func TestAnAddressIsWatchedForOneJobAtATime(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	if !a.Watch("lease-1", guest, began) {
		t.Fatal("first watch refused")
	}

	if a.Watch("lease-2", guest, began) {
		t.Error("a second job was given an address another job holds")
	}

	if a.Watch("lease-3", netip.Addr{}, began) {
		t.Error("an invalid address was watched")
	}

	a.Observe(flow(1, guest, github, 1, 1))

	if _, ok := a.Final("lease-2", nil); ok {
		t.Error("a refused watch produced a result")
	}

	if r, ok := a.Final("lease-1", nil); !ok || len(r.Destinations) != 1 {
		t.Errorf("the address's holder lost its flow: %+v", r)
	}

	// Once the holder has finished, the address is free again.
	if !a.Watch("lease-2", guest, began) {
		t.Error("an address stayed held after its job finished")
	}

	if _, ok := a.Final("lease-9", nil); ok {
		t.Error("a job never watched produced a result")
	}
}

// AN IPv4-MAPPED ADDRESS IS THE SAME GUEST: the tracker may report either
// spelling, and the guest's flows must not be lost to the difference.
func TestAMappedAddressIsTheSameGuest(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", netip.AddrFrom16(guest.As16()), began)
	a.Observe(flow(1, guest, github, 1, 2))

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 1 {
		t.Fatalf("a flow from the plain spelling of a mapped address was lost: %+v", r)
	}

	// And the other way round: a plain watch, a mapped report.
	a.Watch("lease-2", guest, began)
	a.Observe(flow(2, netip.AddrFrom16(guest.As16()), github, 1, 2))

	r, _ = a.Final("lease-2", nil)
	if len(r.Destinations) != 1 {
		t.Fatalf("a flow reported with the mapped spelling of a plain address was lost: %+v", r)
	}
}

// PAST MaxDestinations THE BYTES ARE STILL COUNTED, under Other, and the
// largest destinations keep their names.
func TestDestinationsBeyondTheCapFoldIntoOther(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began)

	var total uint64

	for i := range MaxDestinations + 10 {
		dst := netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)})
		bytes := uint64(i + 1)
		total += 2 * bytes
		a.Observe(flow(uint32(i+1), guest, dst, bytes, bytes))
	}

	r, _ := a.Final("lease-1", nil)
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

	// The smallest ten were folded: the first kept is the largest.
	if r.Destinations[0].Sent != uint64(MaxDestinations+10) {
		t.Errorf("first destination sent %d, want the largest", r.Destinations[0].Sent)
	}
}

// PAST MaxFlows A JOB IS MARKED INCOMPLETE, never silently short.
func TestAJobOpeningTooManyFlowsIsIncomplete(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("lease-1", guest, began)

	for i := range MaxFlows + 1 {
		a.Observe(flow(uint32(i+1), guest, github, 1, 1))
	}

	r, _ := a.Final("lease-1", nil)
	if !r.Incomplete {
		t.Error("a job with more flows than are counted was not marked incomplete")
	}

	if got := r.Destinations[0].Connections; got != MaxFlows {
		t.Errorf("%d connections counted, want %d", got, MaxFlows)
	}
}

// A LOST WINDOW MARKS THE JOBS IN IT, and only those.
func TestLostEventsMarkTheJobsWatchedThen(t *testing.T) {
	t.Parallel()

	a := NewAccountant()
	a.Watch("during", guest, began)
	a.Lost()
	a.Watch("after", other, began)

	if r, _ := a.Final("during", nil); !r.Incomplete {
		t.Error("a job watched while events were lost was not marked incomplete")
	}

	if r, _ := a.Final("after", nil); r.Incomplete {
		t.Error("a job watched after the loss was marked incomplete")
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
		"short line":  "1760000000 06:00:ac:10:00:17\n",
		"bad mac":     "1760000000 zz:zz 192.168.100.23 * *\n",
		"bad address": "1760000000 06:00:ac:10:00:17 not-an-ip * *\n",
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

// A FLOW THAT ENDED BEFORE ITS ADDRESS WAS WATCHED is replayed to the job that
// learns the address, if it started after the job did; the previous holder's
// is not.
func TestUnclaimedFlowsAreReplayedToTheJobThatStartedThem(t *testing.T) {
	t.Parallel()

	a := NewAccountant()

	previous := flow(1, guest, github, 1_000, 1_000)
	previous.Start = began.Add(-time.Minute)

	a.Observe(previous)
	a.Observe(flow(2, guest, npm, 3, 30))
	a.Observe(flow(3, other, npm, 9, 9)) // another address, never this job's

	a.Watch("lease-1", guest, began)

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 1 || r.Destinations[0] != (Destination{Addr: npm, Sent: 3, Received: 30, Connections: 1}) {
		t.Fatalf("destinations = %+v, want only the guest's own flow replayed", r.Destinations)
	}
}

// THE REPLAY BUFFER IS BOUNDED, keeping the newest flows.
func TestUnclaimedFlowsAreBounded(t *testing.T) {
	t.Parallel()

	a := NewAccountant()

	a.Observe(flow(1, guest, github, 1, 1)) // the oldest, pushed out below

	for i := range MaxUnclaimed {
		a.Observe(flow(uint32(i+2), other, npm, 1, 1))
	}

	a.Observe(flow(MaxUnclaimed+2, guest, npm, 2, 2)) // the newest

	if len(a.unclaimed) != MaxUnclaimed {
		t.Fatalf("the buffer holds %d flows, want at most %d", len(a.unclaimed), MaxUnclaimed)
	}

	a.Watch("lease-1", guest, began)

	r, _ := a.Final("lease-1", nil)
	if len(r.Destinations) != 1 || r.Destinations[0].Addr != npm {
		t.Errorf("destinations = %+v, want only the newest flow; the oldest was pushed out", r.Destinations)
	}
}
