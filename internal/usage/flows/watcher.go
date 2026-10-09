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

// Table reads the flows the tracker holds now for one source address.
// *Tracker is the real one.
type Table interface {
	Flows(addr netip.Addr) ([]Flow, error)
}

// Watcher follows each job's guest from launch to destroy: it learns the
// guest's address once the guest has asked DHCP for one, attributes flows from
// that address, and at the end reads the flows still open.
//
// THE ADDRESS IS LEARNED LATE, AND THAT IS WHY ATTRIBUTION IS BY START TIME. A
// guest asks for its address after it boots, so the node cannot know it at
// launch; flows the guest opened before the node learned it still count,
// because the tracker stamps each flow's start and the job's start is known.
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
	since     time.Time
	resolved  bool
	failure   error
}

// NewWatcher returns a Watcher attributing flows through acct and reading open
// flows from table.
func NewWatcher(acct *Accountant, table Table, leaseTime time.Duration, log *slog.Logger) *Watcher {
	return &Watcher{acct: acct, table: table, leaseTime: leaseTime, read: os.ReadFile, log: log,
		pending: map[string]*pending{}}
}

// Watch starts following the job named key, whose guest has hardware address
// mac and is leased its address in leaseFile, and which started at since.
func (w *Watcher) Watch(key string, mac net.HardwareAddr, leaseFile string, since time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending[key] = &pending{mac: mac, leaseFile: leaseFile, since: since}
	w.resolve(key, w.pending[key])
}

// Resolve tries once more to learn the address of every guest not yet known.
func (w *Watcher) Resolve() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for key, p := range w.pending {
		w.resolve(key, p)
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

func (w *Watcher) resolve(key string, p *pending) {
	if p.resolved {
		return
	}

	data, err := w.read(p.leaseFile)
	if err != nil {
		p.failure = fmt.Errorf("read the DHCP leases: %w", err)

		return
	}

	addr, err := addressGrantedSince(data, p.mac, p.since, w.leaseTime)
	switch {
	case errors.Is(err, ErrNoAddress):
		// Not asked for yet; the next pass looks again.
		p.failure = err

		return
	case err != nil:
		p.failure = err

		return
	}

	if !w.acct.Watch(key, addr, p.since) {
		p.failure = fmt.Errorf("address %s is already another job's", addr)

		return
	}

	p.resolved, p.failure = true, nil
}

// Final stops following key and returns what its flows came to, whether they
// were measured, and why not. Unmeasured means the guest's address was never
// learned, which says nothing about what it sent: a job with no result here is
// unmeasured, never zero.
func (w *Watcher) Final(key string) (Result, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p, ok := w.pending[key]
	if !ok {
		return Result{}, false, errors.New("never watched")
	}

	delete(w.pending, key)
	w.resolve(key, p)

	if !p.resolved {
		return Result{}, false, p.failure
	}

	addr := w.acct.addressOf(key)

	open, readErr := w.table.Flows(addr)

	r, _ := w.acct.Final(key, open)
	if readErr != nil {
		// The flows still open could not be read, so the ones already counted
		// are a lower bound.
		r.Incomplete = true
	}

	return r, true, readErr
}

// Forget stops following key without a result.
func (w *Watcher) Forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if p, ok := w.pending[key]; ok && p.resolved {
		w.acct.Final(key, nil)
	}

	delete(w.pending, key)
}

// addressGrantedSince is AddressFor, refusing a lease granted before since: a
// guest's MAC is derived from its tap, which a later job reuses, so the lease
// file can still hold the previous guest's lease for the same MAC until the new
// guest asks for its own.
func addressGrantedSince(data []byte, mac net.HardwareAddr, since time.Time, leaseTime time.Duration) (netip.Addr, error) {
	addr, expiry, err := leaseFor(data, mac)
	if err != nil {
		return netip.Addr{}, err
	}

	// dnsmasq writes the expiry in whole seconds; a second of slack keeps a
	// lease granted in the job's first second from reading as older.
	if expiry.Add(-leaseTime).Before(since.Add(-time.Second)) {
		return netip.Addr{}, ErrNoAddress
	}

	return addr, nil
}

// addressOf is the address key is watched at, or the zero address.
func (a *Accountant) addressOf(key string) netip.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()

	if w, ok := a.byKey[key]; ok {
		return w.addr
	}

	return netip.Addr{}
}
