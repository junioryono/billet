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
	"time"
)

// syncLimit bounds the wait for the tracker to prove it has read every event
// up to the end of a job; past it the job's flows are marked incomplete.
const syncLimit = 2 * time.Second

// Table reads the connection tracker. *Tracker is the real one.
type Table interface {
	// Flows returns the flows the tracker holds now for one source address.
	Flows(addr netip.Addr) ([]Flow, error)
	// Sync returns once every flow the tracker destroyed before the call has
	// been reported to the Accountant, or with an error saying it could not
	// prove that.
	Sync(ctx context.Context) error
}

// Watcher follows each job's guest from launch to destroy: it learns the
// guest's address once the guest has asked DHCP for one, attributes flows from
// that address, and at the end reads the flows still open.
//
// THE ADDRESS IS LEARNED LATE, AND THAT IS WHY ATTRIBUTION IS BY START TIME. A
// guest asks for its address after it boots, so the node cannot know it at
// launch; flows the guest opened before the node read the lease still count,
// because the tracker stamps each flow's start, and the guest can have opened
// none before the grant that gave it the address.
type Watcher struct {
	acct  *Accountant
	table Table
	// leaseTime is the DHCP lease length the host's dnsmasq grants (the host
	// role's dhcp-range sets an hour). A lease line's grant is its expiry less
	// this, which is how a lease from before the job is told apart.
	leaseTime time.Duration
	read      func(path string) ([]byte, error)
	log       *slog.Logger

	mu      sync.Mutex
	pending map[string]*pending
}

type pending struct {
	mac       net.HardwareAddr
	leaseFile string
	launched  time.Time
	// addr is the address learned, zero until then.
	addr    netip.Addr
	failure error
}

// NewWatcher returns a Watcher attributing flows through acct and reading the
// tracker through table.
func NewWatcher(acct *Accountant, table Table, leaseTime time.Duration, log *slog.Logger) *Watcher {
	return &Watcher{acct: acct, table: table, leaseTime: leaseTime, read: os.ReadFile, log: log,
		pending: map[string]*pending{}}
}

// Watch starts following the job named key, whose guest has hardware address
// mac and is leased its address in leaseFile, and which was launched at
// launched. A key already followed is forgotten first.
func (w *Watcher) Watch(key string, mac net.HardwareAddr, leaseFile string, launched time.Time) {
	w.Forget(key)

	p := &pending{mac: mac, leaseFile: leaseFile, launched: launched}

	w.mu.Lock()
	w.pending[key] = p
	w.mu.Unlock()

	w.resolve(key, p)
}

// Resolve reads every followed guest's lease once more: to learn an address
// not yet known, and to notice one that changed.
func (w *Watcher) Resolve() {
	w.mu.Lock()

	keys := make(map[string]*pending, len(w.pending))
	for k, p := range w.pending {
		keys[k] = p
	}

	w.mu.Unlock()

	for k, p := range keys {
		w.resolve(k, p)
	}
}

// Run resolves every interval until ctx ends.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Resolve()
		}
	}
}

// resolve reads p's lease, and on first learning the address the tracker's
// flows for it, outside the lock, and commits what it found under the lock
// only if p is still key's: a job finished or replaced meanwhile is never
// watched again.
func (w *Watcher) resolve(key string, p *pending) {
	w.mu.Lock()
	known := p.addr
	w.mu.Unlock()

	data, err := w.read(p.leaseFile)

	var (
		addr      netip.Addr
		grant     time.Time
		ambiguous bool
	)

	if err == nil {
		addr, grant, ambiguous, err = leaseGrantedSince(data, p.mac, p.launched, w.leaseTime)
	}

	if err != nil {
		if errors.Is(err, ErrNoAddress) {
			err = fmt.Errorf("the guest has not been granted an address since it was launched: %w", err)
		} else {
			err = fmt.Errorf("read the DHCP leases: %w", err)
		}
	}

	if known.IsValid() {
		// AN ADDRESS THAT CHANGED MID-JOB, OR ONE THAT COULD NOT BE CHECKED: the
		// old one may now be another guest's and the new one's traffic was
		// never followed.
		if err != nil || addr != known {
			w.mu.Lock()
			if w.pending[key] == p {
				w.acct.MarkIncomplete(key)
			}
			w.mu.Unlock()
		}

		return
	}

	if err != nil {
		w.mu.Lock()
		p.failure = err
		w.mu.Unlock()

		return
	}

	open, dumpErr := w.table.Flows(addr)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.pending[key] != p || p.addr.IsValid() {
		return
	}

	if !w.acct.Watch(key, addr, grant, open) {
		p.failure = fmt.Errorf("address %s is already another job's", addr)

		return
	}

	p.addr, p.failure = addr, nil

	// THE PREVIOUS HOLDER'S ENTRIES COULD NOT BE READ, so traffic the guest
	// adds to one of them would go unnoticed; A GRANT IN THE LAUNCH'S OWN
	// SECOND could be the previous guest's renewal; A GRANT HALF A LEASE AFTER
	// THE LAUNCH may be a renewal hiding earlier flows. Each is a job whose
	// flows cannot all be told apart.
	if dumpErr != nil || ambiguous || grant.Sub(p.launched) > w.leaseTime/2 {
		w.acct.MarkIncomplete(key)
	}
}

// Final stops following key and returns what its flows came to, whether they
// were measured, and why not. until is when the job's destroy began. Unmeasured
// means the guest's address was never learned, which says nothing about what
// it sent: a job with no result here is unmeasured, never zero.
func (w *Watcher) Final(ctx context.Context, key string, until time.Time) (Result, bool, error) {
	w.mu.Lock()
	p, ok := w.pending[key]
	w.mu.Unlock()

	if !ok {
		return Result{}, false, errors.New("never watched")
	}

	w.resolve(key, p)

	w.mu.Lock()
	if w.pending[key] == p {
		delete(w.pending, key)
	}
	addr, failure := p.addr, p.failure
	w.mu.Unlock()

	if !addr.IsValid() {
		return Result{}, false, failure
	}

	// THE OPEN FLOWS FIRST, THEN THE BARRIER: an entry that expires while the
	// table is read is skipped by the dump and reported destroyed, and the
	// barrier, created after the dump, is what proves that report was read.
	open, readErr := w.table.Flows(addr)
	if readErr != nil {
		// The flows still open could not be read, so the ones already counted
		// are a lower bound.
		w.acct.MarkIncomplete(key)
	}

	syncCtx, cancel := context.WithTimeout(ctx, syncLimit)
	syncErr := w.table.Sync(syncCtx)

	cancel()

	if syncErr != nil {
		w.acct.MarkIncomplete(key)
	}

	r, _ := w.acct.Final(key, open, until)

	return r, true, errors.Join(readErr, syncErr)
}

// Forget stops following key without a result.
func (w *Watcher) Forget(key string) {
	w.mu.Lock()
	p, ok := w.pending[key]
	delete(w.pending, key)

	resolved := ok && p.addr.IsValid()
	w.mu.Unlock()

	if resolved {
		w.acct.Final(key, nil, time.Now())
	}
}

// leaseGrantedSince is the address leased to mac and when it was granted,
// refusing a lease granted before launched: a guest's MAC is derived from its
// tap, which a later job reuses, so the lease file can still hold the previous
// guest's lease for the same MAC until the new guest asks for its own.
//
// THE GRANT IS READ IN WHOLE SECONDS, the precision dnsmasq writes, and
// compared with the launch's own whole second: a grant in that very second is
// accepted but reported ambiguous, since it could be the previous guest's
// renewal in the moment before its VM was destroyed.
func leaseGrantedSince(data []byte, mac net.HardwareAddr, launched time.Time,
	leaseTime time.Duration,
) (netip.Addr, time.Time, bool, error) {
	addr, expiry, err := leaseFor(data, mac)
	if err != nil {
		return netip.Addr{}, time.Time{}, false, err
	}

	grant := expiry.Add(-leaseTime)
	second := launched.Truncate(time.Second)

	if grant.Before(second) {
		return netip.Addr{}, time.Time{}, false, ErrNoAddress
	}

	return addr, grant, grant.Equal(second), nil
}
