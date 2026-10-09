package flows

import (
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

// fakeTable answers Flows from a map, or with err.
type fakeTable struct {
	mu   sync.Mutex
	open map[netip.Addr][]Flow
	err  error
}

func (t *fakeTable) Flows(addr netip.Addr) ([]Flow, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.err != nil {
		return nil, t.err
	}

	return t.open[addr], nil
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

// leaseLine is a dnsmasq lease for the guest's MAC granted at granted.
func leaseLine(granted time.Time, addr netip.Addr) string {
	return fmt.Sprintf("%d 02:00:00:00:00:17 %s * *\n", granted.Add(leaseTime).Unix(), addr)
}

func newTestWatcher(table Table, l *leases) *Watcher {
	w := NewWatcher(NewAccountant(), table, leaseTime, slog.New(slog.DiscardHandler))
	w.read = l.read

	return w
}

// THE ADDRESS IS LEARNED AFTER THE GUEST HAS OPENED FLOWS, and those flows are
// still the job's: attribution is by when the tracker says each flow started,
// not by when the node learned where to look.
func TestFlowsBeforeTheAddressIsLearnedStillCount(t *testing.T) {
	t.Parallel()

	l := &leases{}
	acct := NewAccountant()
	w := NewWatcher(acct, &fakeTable{}, leaseTime, slog.New(slog.DiscardHandler))
	w.read = l.read

	w.Watch("lease-1", guestMACAddr, "leases", began)

	// The guest's first flow opens and closes before the node learns its
	// address; the tracker reports its end once, now.
	acct.Observe(flow(1, guest, github, 10, 100))

	l.set(leaseLine(began.Add(2*time.Second), guest))
	w.Resolve()

	acct.Observe(flow(2, guest, npm, 1, 2))

	r, measured, err := w.Final("lease-1")
	if !measured || err != nil {
		t.Fatalf("Final = measured %v, %v", measured, err)
	}

	if len(r.Destinations) != 2 {
		t.Errorf("destinations = %+v, want both flows the guest opened", r.Destinations)
	}
}

// A LEASE FROM BEFORE THE JOB IS THE PREVIOUS GUEST'S. The MAC comes from the
// tap, which a later job reuses, so the file can name the old guest's lease
// until the new guest asks; it is not used until a lease granted since appears.
func TestALeaseGrantedBeforeTheJobIsNotItsAddress(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began.Add(-10*time.Minute), other))

	w := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", began)

	if _, measured, err := w.Final("lease-1"); measured || !errors.Is(err, ErrNoAddress) {
		t.Fatalf("a job whose only lease predates it was measured (%v, %v); its address is the previous guest's", measured, err)
	}

	// Renewed by the new guest: granted after the job began, and now used.
	w.Watch("lease-2", guestMACAddr, "leases", began)
	l.set(leaseLine(began.Add(5*time.Second), guest))

	if _, measured, err := w.Final("lease-2"); !measured || err != nil {
		t.Errorf("a lease granted since the job began was not used: %v, %v", measured, err)
	}
}

// A JOB WHOSE ADDRESS WAS NEVER LEARNED IS UNMEASURED, never zero, and an
// unreadable lease file says why.
func TestAnUnlearnedAddressIsUnmeasured(t *testing.T) {
	t.Parallel()

	l := &leases{err: os.ErrPermission}
	w := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", began)

	r, measured, err := w.Final("lease-1")
	if measured || len(r.Destinations) != 0 {
		t.Errorf("an unresolved job was reported as measured: %+v", r)
	}

	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("Final's error = %v, want the lease file's", err)
	}

	if _, measured, err := w.Final("never"); measured || err == nil {
		t.Error("a job never watched produced a result")
	}
}

// THE FLOWS STILL OPEN AT THE END ARE COUNTED, and if they cannot be read the
// result is a lower bound and says so.
func TestFinalCountsOpenFlowsOrSaysItCouldNot(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	table := &fakeTable{open: map[netip.Addr][]Flow{guest: {flow(3, guest, npm, 5, 50)}}}
	w := newTestWatcher(table, l)
	w.Watch("lease-1", guestMACAddr, "leases", began)

	r, measured, err := w.Final("lease-1")
	if !measured || err != nil || len(r.Destinations) != 1 || r.Destinations[0].Received != 50 {
		t.Fatalf("Final = %+v, %v, %v; want the open flow counted", r, measured, err)
	}

	table.err = errors.New("netlink: permission denied")

	w.Watch("lease-2", guestMACAddr, "leases", began)

	r, measured, err = w.Final("lease-2")
	if !measured || err == nil || !r.Incomplete {
		t.Errorf("an unreadable table gave %+v, measured %v, %v; want a measured, incomplete result and the error", r, measured, err)
	}
}

// FORGETTING A JOB FREES ITS ADDRESS for the next guest to hold it.
func TestForgetFreesTheAddress(t *testing.T) {
	t.Parallel()

	l := &leases{}
	l.set(leaseLine(began, guest))

	w := newTestWatcher(&fakeTable{}, l)
	w.Watch("lease-1", guestMACAddr, "leases", began)

	w.Watch("lease-2", guestMACAddr, "leases", began)
	if _, measured, err := w.Final("lease-2"); measured || err == nil {
		t.Fatalf("two jobs were given one address at once (measured %v, %v)", measured, err)
	}

	w.Forget("lease-1")
	w.Watch("lease-3", guestMACAddr, "leases", began)

	if _, measured, err := w.Final("lease-3"); !measured {
		t.Errorf("the address stayed held after its job was forgotten: %v", err)
	}
}

func TestAnInfiniteOrGarbledExpiryIsNotAGrantTime(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		"0 02:00:00:00:00:17 192.168.100.23 * *\n",
		"soon 02:00:00:00:00:17 192.168.100.23 * *\n",
	} {
		_, err := AddressFor([]byte(line), guestMACAddr)
		if err == nil || errors.Is(err, ErrNoAddress) {
			t.Errorf("%q answered %v, want an error that is not ErrNoAddress", line, err)
		}
	}
}
