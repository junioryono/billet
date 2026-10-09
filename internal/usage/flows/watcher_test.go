package flows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeTable answers Flows from a map, or with err, and Sync with syncErr,
// recording the order it was asked in.
type fakeTable struct {
	mu      sync.Mutex
	open    map[netip.Addr][]Flow
	err     error
	syncErr error
	synced  int
	calls   []string
	// onFlows, when set, runs before the nth read of the table answers,
	// outside the lock.
	onFlows func(n int)
	flows   int
}

func (t *fakeTable) Flows(addr netip.Addr) ([]Flow, error) {
	t.mu.Lock()
	t.flows++
	n, hook := t.flows, t.onFlows
	t.mu.Unlock()

	if hook != nil {
		hook(n)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.calls = append(t.calls, "flows")

	if t.err != nil {
		return nil, t.err
	}

	return t.open[addr], nil
}

func (t *fakeTable) Sync(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.synced++
	t.calls = append(t.calls, "sync")

	return t.syncErr
}

func (t *fakeTable) set(addr netip.Addr, open ...Flow) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.open == nil {
		t.open = map[netip.Addr][]Flow{}
	}

	t.open[addr] = open
}

// leases is a lease file whose content the test sets.
type leases struct {
	mu   sync.Mutex
	text string
	err  error
}

func (l *leases) set(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.text = text
}

func (l *leases) read(string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return []byte(l.text), l.err
}

var guestMACAddr = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x17}

const leaseTime = time.Hour

// launched is when the job was launched, a moment before the guest asked.
var launched = began.Add(-2 * time.Second)

// leaseLine is a dnsmasq lease for the guest's MAC granted at granted.
func leaseLine(granted time.Time, addr netip.Addr) string {
	return fmt.Sprintf("%d 02:00:00:00:00:17 %s * *\n", granted.Add(leaseTime).Unix(), addr)
}

func newTestWatcher(table Table, l *leases) (*Watcher, *Accountant) {
	acct := NewAccountant()
	acct.Up(launched.Add(-time.Hour))

	w := NewWatcher(acct, table, leaseTime, slog.New(slog.DiscardHandler))
	w.read = l.read

	return w, acct
}

// THE ADDRESS IS LEARNED AFTER THE GUEST HAS OPENED AND CLOSED A FLOW, and
// that flow is still the job's: the guest could open none before its grant,
// and the tracker's report of its end is replayed when the address is learned.
func TestFlowsBeforeTheAddressIsLearnedStillCount(t *testing.T) {
	t.Parallel()

	l := &leases{}
	table := &fakeTable{}
	w, acct := newTestWatcher(table, l)

	w.Watch("lease-1", guestMACAddr, "leases", launched)

	acct.Observe(flow(1, guest, github, 10, 100)) // opened and closed before the lease was read

	l.set(leaseLine(began, guest))
	w.Resolve()

	acct.Observe(flow(2, guest, npm, 1, 2))

	r, measured, err := w.Final(t.Context(), "lease-1", ended)
	if !measured || err != nil {
		t.Fatalf("Final = measured %v, %v", measured, err)
	}

	if len(r.Destinations) != 2 || r.Incomplete {
		t.Errorf("result = %+v, want both flows the guest opened, complete", r)
	}

	if table.synced != 1 {
		t.Errorf("Final synced the tracker %d times, want once before taking the result", table.synced)
	}

	// THE DUMP BEFORE THE BARRIER: the barrier is what proves the destructions
	// reported during the dump were read.
	if n := len(table.calls); n < 2 || table.calls[n-2] != "flows" || table.calls[n-1] != "sync" {
		t.Errorf("Final asked the table %v; want the open flows read and then the barrier", table.calls)
	}
}

// A GRANT IN THE LAUNCH'S OWN SECOND could be the previous guest's renewal:
// it is used, and the job marked incomplete.
func TestAGrantInTheLaunchsOwnSecondIsAmbiguous(t *testing.T) {
	t.Parallel()

	launchedMid := began.Add(800 * time.Millisecond)

	l := &leases{}
	l.set(leaseLine(began, guest))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launchedMid)

	if r, measured, err := w.Final(t.Context(), "lease-1", ended); !measured || !r.Incomplete || err != nil {
		t.Errorf("a grant in the launch's second gave %+v, measured %v, %v; want measured and incomplete", r, measured, err)
	}
}

// A LEASE THAT CANNOT BE READ AFTER THE ADDRESS IS KNOWN means its continued
// ownership cannot be checked: the job is marked incomplete.
func TestALeaseUnreadableAfterTheAddressIsKnownMarksTheJob(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	l.mu.Lock()
	l.err = os.ErrPermission
	l.mu.Unlock()

	w.Resolve()

	if r, measured, err := w.Final(t.Context(), "lease-1", ended); !measured || !r.Incomplete || err != nil {
		t.Errorf("a lease unreadable after resolution gave %+v, measured %v, %v; want incomplete", r, measured, err)
	}
}

// A RESOLUTION STILL READING WHEN ITS JOB FINISHES NEVER WATCHES IT: the job is
// gone, and an orphan watch would hold its address from every later guest.
func TestAResolutionCannotOutliveItsJob(t *testing.T) {
	t.Parallel()

	reading, release := make(chan struct{}), make(chan struct{})

	var calls sync.Mutex

	n := 0

	w, _ := newTestWatcher(&fakeTable{}, &leases{})
	w.read = func(string) ([]byte, error) {
		calls.Lock()
		n++
		call := n
		calls.Unlock()

		switch call {
		case 2: // the Resolve tick, which stalls until the job has finished
			close(reading)
			<-release

			return []byte(leaseLine(began, guest)), nil
		case 1, 3: // Watch's own read, and Final's: no address yet
			return nil, nil
		default:
			return []byte(leaseLine(began, guest)), nil
		}
	}

	w.Watch("lease-1", guestMACAddr, "leases", launched)

	done := make(chan struct{})

	go func() {
		defer close(done)

		w.Resolve()
	}()

	<-reading

	if _, measured, err := w.Final(t.Context(), "lease-1", ended); measured || err == nil {
		t.Fatalf("a job with no address yet was measured (%v, %v)", measured, err)
	}

	close(release)
	<-done

	// The address is free: a later guest is watched at it.
	w.Watch("lease-2", guestMACAddr, "leases", launched)

	if _, measured, err := w.Final(t.Context(), "lease-2", ended); !measured {
		t.Errorf("a finished job's late resolution held the address: %v", err)
	}
}

// THE GRANT, NOT THE LAUNCH, BOUNDS A JOB'S FLOWS: the guest has no address
// before the grant, so a flow from that address that started between the
// launch and the grant is the previous holder's.
func TestTheGrantBoundsTheJobsFlows(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	table := &fakeTable{}
	w, acct := newTestWatcher(table, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	acct.Observe(startedAt(flow(1, guest, github, 7, 7), launched.Add(time.Second))) // between launch and grant
	acct.Observe(flow(2, guest, npm, 1, 1))

	r, _, err := w.Final(t.Context(), "lease-1", ended)
	if err != nil {
		t.Fatal(err)
	}

	only(t, r, Destination{Addr: npm, Sent: 1, Received: 1, Connections: 1})
}

// A LEASE FROM BEFORE THE LAUNCH IS THE PREVIOUS GUEST'S. The MAC comes from
// the tap, which a later job reuses, so the file can name the old guest's lease
// until the new guest asks; it is not used until a lease granted since appears.
func TestALeaseGrantedBeforeTheLaunchIsNotItsAddress(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(launched.Add(-10*time.Minute), other))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	if _, measured, err := w.Final(t.Context(), "lease-1", ended); measured || !errors.Is(err, ErrNoAddress) {
		t.Fatalf("a job whose only lease predates it was measured (%v, %v); its address is the previous guest's", measured, err)
	}

	w.Watch("lease-2", guestMACAddr, "leases", launched)
	l.set(leaseLine(began, guest))

	if _, measured, err := w.Final(t.Context(), "lease-2", ended); !measured || err != nil {
		t.Errorf("a lease granted since the launch was not used: %v, %v", measured, err)
	}
}

// A JOB WHOSE ADDRESS WAS NEVER LEARNED IS UNMEASURED, never zero, and an
// unreadable lease file says why.
func TestAnUnlearnedAddressIsUnmeasured(t *testing.T) {
	t.Parallel()

	l := &leases{err: os.ErrPermission}
	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	r, measured, err := w.Final(t.Context(), "lease-1", ended)
	if measured || len(r.Destinations) != 0 {
		t.Errorf("an unresolved job was reported as measured: %+v", r)
	}

	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("Final's error = %v, want the lease file's", err)
	}

	if _, measured, err := w.Final(t.Context(), "never", ended); measured || err == nil {
		t.Error("a job never watched produced a result")
	}
}

// THE FLOWS STILL OPEN AT THE END ARE COUNTED, and an end that cannot be
// proved read (the tracker could not be synced, or the table read) makes the
// result a lower bound that says so.
func TestFinalCountsOpenFlowsOrSaysItCouldNot(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	table := &fakeTable{}
	w, _ := newTestWatcher(table, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)
	table.set(guest, flow(3, guest, npm, 5, 50))

	r, measured, err := w.Final(t.Context(), "lease-1", ended)
	if !measured || err != nil || r.Incomplete {
		t.Fatalf("Final = %+v, %v, %v; want a complete result", r, measured, err)
	}

	only(t, r, Destination{Addr: npm, Sent: 5, Received: 50, Connections: 1})

	table.syncErr = errors.New("the listener is not reading")

	w.Watch("lease-2", guestMACAddr, "leases", launched)

	if r, measured, err = w.Final(t.Context(), "lease-2", ended); !measured || err == nil || !r.Incomplete {
		t.Errorf("an unproved sync gave %+v, measured %v, %v; want a measured, incomplete result and the error", r, measured, err)
	}

	table.syncErr = nil

	w.Watch("lease-3", guestMACAddr, "leases", launched)

	table.err = errors.New("netlink: permission denied")

	if r, measured, err = w.Final(t.Context(), "lease-3", ended); !measured || err == nil || !r.Incomplete {
		t.Errorf("an unreadable table gave %+v, measured %v, %v; want a measured, incomplete result and the error", r, measured, err)
	}
}

// AN ADDRESS THAT CHANGES MID-JOB marks the job incomplete: its new address's
// traffic was never followed.
func TestAnAddressThatChangesMarksTheJobIncomplete(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	l.set(leaseLine(began.Add(30*time.Minute), other))
	w.Resolve()

	if r, _, err := w.Final(t.Context(), "lease-1", ended); !r.Incomplete || err != nil {
		t.Errorf("a job whose address changed gave %+v, %v; want incomplete", r, err)
	}

	// A renewal of the same address is not a change.
	l.set(leaseLine(began, guest))
	w.Watch("lease-2", guestMACAddr, "leases", launched)
	l.set(leaseLine(began.Add(30*time.Minute), guest))
	w.Resolve()

	if r, _, err := w.Final(t.Context(), "lease-2", ended); r.Incomplete || err != nil {
		t.Errorf("a renewal of the same address gave %+v, %v; want complete", r, err)
	}
}

// A FIRST GRANT LONG AFTER THE LAUNCH MAY BE A RENEWAL, which hides the flows
// before it: the job is marked incomplete.
func TestAFirstGrantLongAfterTheLaunchIsIncomplete(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(launched.Add(leaseTime/2+time.Minute), guest))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	if r, measured, err := w.Final(t.Context(), "lease-1", ended); !measured || !r.Incomplete || err != nil {
		t.Errorf("a first grant half a lease after the launch gave %+v, measured %v, %v; want incomplete", r, measured, err)
	}
}

// THE PREVIOUS HOLDER'S ENTRIES ARE READ WHEN THE ADDRESS IS LEARNED, so the
// guest's traffic on one of them is noticed.
func TestTheInheritedEntriesAreReadWhenTheAddressIsLearned(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	inherited := startedAt(flow(5, guest, github, 100, 100), began.Add(-time.Minute))
	table := &fakeTable{}
	table.set(guest, inherited)

	w, acct := newTestWatcher(table, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)

	sentOn := inherited
	sentOn.OrigBytes, sentOn.OrigPackets = 200, 200
	acct.Observe(sentOn)

	table.set(guest)

	if r, _, err := w.Final(t.Context(), "lease-1", ended); !r.Incomplete || err != nil {
		t.Errorf("traffic on an entry the previous holder left gave %+v, %v; want incomplete", r, err)
	}
}

// TWO JOBS ARE NEVER GIVEN ONE ADDRESS, and forgetting a job frees its
// address for the next guest to hold it.
func TestOneAddressOneJobAndForgetFreesIt(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	w, _ := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", launched)
	w.Watch("lease-2", guestMACAddr, "leases", launched)

	if _, measured, err := w.Final(t.Context(), "lease-2", ended); measured || err == nil {
		t.Fatalf("two jobs were given one address at once (measured %v, %v)", measured, err)
	}

	w.Forget("lease-1")
	w.Watch("lease-3", guestMACAddr, "leases", launched)

	if _, measured, err := w.Final(t.Context(), "lease-3", ended); !measured {
		t.Errorf("the address stayed held after its job was forgotten: %v", err)
	}
}

// FOLLOWING A JOB AGAIN REPLACES ITS WATCH: the old one is finished and its
// address freed for the new one, which is measured on its own.
func TestFollowingAJobAgainReplacesItsWatch(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	w, acct := newTestWatcher(&fakeTable{}, l)
	w.Watch("job", guestMACAddr, "leases", launched)
	acct.Observe(flow(1, guest, github, 9, 9))

	w.Watch("job", guestMACAddr, "leases", launched)

	r, measured, err := w.Final(t.Context(), "job", ended)
	if !measured || err != nil {
		t.Fatalf("the replacement watch was not measured: %v, %v", measured, err)
	}

	if len(r.Destinations) != 0 {
		t.Errorf("the replacement counted the replaced watch's flows: %+v", r.Destinations)
	}
}

// AN OLD FINAL CAN ONLY FINISH ITS OWN WATCH: one still reading the table when
// the job is followed again does not finish the replacement, which is measured
// on its own.
func TestAnOldFinalCannotFinishItsReplacement(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	reading, release := make(chan struct{}), make(chan struct{})

	table := &fakeTable{}
	table.onFlows = func(n int) {
		if n == 2 { // the first Final's read of the open flows
			close(reading)
			<-release
		}
	}

	w, _ := newTestWatcher(table, l)
	w.Watch("job", guestMACAddr, "leases", launched)

	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, _, err := w.Final(t.Context(), "job", ended); err != nil {
			t.Logf("the old Final: %v", err)
		}
	}()

	<-reading

	// The replacement's guest is leased another address, so it is watched at
	// once, while the old Final still holds its own.
	l.set(leaseLine(began, other))
	w.Watch("job", guestMACAddr, "leases", launched)

	close(release)
	<-done

	w.Resolve()

	if _, measured, err := w.Final(t.Context(), "job", ended); !measured {
		t.Errorf("the old Final finished the replacement's watch: %v", err)
	}
}
