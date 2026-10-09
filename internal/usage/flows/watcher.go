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
// launched.
func (w *Watcher) Watch(key string, mac net.HardwareAddr, leaseFile string, launched time.Time) {
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

// resolve reads p's lease outside the lock and records what it found under it,
// unless the job has finished meanwhile.
func (w *Watcher) resolve(key string, p *pending) {
	data, err := w.read(p.leaseFile)

	var (
		addr  netip.Addr
		grant time.Time
	)

	if err == nil {
		addr, grant, err = leaseGrantedSince(data, p.mac, p.launched, w.leaseTime)
	}

	w.mu.Lock()
	current := w.pending[key] == p
	known := p.addr
	w.mu.Unlock()

	if !current {
		return
	}

	if err != nil {
		if errors.Is(err, ErrNoAddress) {
			err = fmt.Errorf("the guest has not been granted an address since it was launched: %w", err)
		} else {
			err = fmt.Errorf("read the DHCP leases: %w", err)
		}

		w.mu.Lock()
		p.failure = err
		w.mu.Unlock()

		return
	}

	if known.IsValid() {
		// AN ADDRESS THAT CHANGED MID-JOB: the old one may now be another
		// guest's and the new one's traffic was never followed.
		if addr != known {
			w.acct.MarkIncomplete(key)
		}

		return
	}

	open, dumpErr := w.table.Flows(addr)

	if !w.acct.Watch(key, addr, grant, open) {
		w.mu.Lock()
		p.failure = fmt.Errorf("address %s is already another job's", addr)
		w.mu.Unlock()

		return
	}

	if dumpErr != nil {
		// The previous holder's entries could not be read, so traffic the guest
		// adds to one of them would go unnoticed.
		w.acct.MarkIncomplete(key)
	}

	// A GRANT THIS LONG AFTER THE LAUNCH MAY BE A RENEWAL, which dnsmasq records
	// the same way, and the flows before it would then be dropped as an older
	// holder's. A guest asks within seconds of booting, so this is a lease file
	// that could not be read for half a lease.
	if grant.Sub(p.launched) > w.leaseTime/2 {
		w.acct.MarkIncomplete(key)
	}

	w.mu.Lock()
	p.addr, p.failure = addr, nil
	w.mu.Unlock()
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
	delete(w.pending, key)
	addr, failure := p.addr, p.failure
	w.mu.Unlock()

	if !addr.IsValid() {
		return Result{}, false, failure
	}

	// EVERY FLOW THAT ENDED BEFORE NOW IS READ BEFORE THE RESULT IS TAKEN, or
	// the result says it could not be proved.
	syncCtx, cancel := context.WithTimeout(ctx, syncLimit)
	syncErr := w.table.Sync(syncCtx)

	cancel()

	if syncErr != nil {
		w.acct.MarkIncomplete(key)
	}

	open, readErr := w.table.Flows(addr)
	if readErr != nil {
		// The flows still open could not be read, so the ones already counted
		// are a lower bound.
		w.acct.MarkIncomplete(key)
	}

	r, _ := w.acct.Final(key, open, until)

	return r, true, errors.Join(syncErr, readErr)
}

// Forget stops following key without a result.
func (w *Watcher) Forget(key string) {
	w.mu.Lock()
	p, ok := w.pending[key]
	delete(w.pending, key)
	w.mu.Unlock()

	if ok && p.addr.IsValid() {
		w.acct.Final(key, nil, time.Now())
	}
}

// leaseGrantedSince is the address leased to mac and when it was granted,
// refusing a lease granted before launched: a guest's MAC is derived from its
// tap, which a later job reuses, so the lease file can still hold the previous
// guest's lease for the same MAC until the new guest asks for its own.
//
// THE GRANT IS READ IN WHOLE SECONDS, the precision dnsmasq writes, and
// compared with the launch's own whole second: a grant the launch's second
// cannot be told from is accepted, since the previous guest's VM is destroyed
// before its tap, and so its MAC, can be claimed again.
func leaseGrantedSince(data []byte, mac net.HardwareAddr, launched time.Time, leaseTime time.Duration) (netip.Addr, time.Time, error) {
	addr, expiry, err := leaseFor(data, mac)
	if err != nil {
		return netip.Addr{}, time.Time{}, err
	}

	grant := expiry.Add(-leaseTime)
	if grant.Before(launched.Truncate(time.Second)) {
		return netip.Addr{}, time.Time{}, ErrNoAddress
	}

	return addr, grant, nil
}
