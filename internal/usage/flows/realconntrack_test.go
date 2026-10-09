//go:build linux

package flows

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ti-mo/conntrack"
)

// AGAINST THE REAL CONNECTION TRACKER: every guest with a lease on a billet
// bridge has its flows found by the tracker's own dump, from the guest's
// address and no other, and the listener proves it has read every destruction
// up to a moment by creating and deleting a sentinel entry on loopback, the
// one change this makes to the host. Gated: it needs root (CAP_NET_ADMIN), a
// node host with live guests, and BILLET_TEST_REAL_CONNTRACK=1, and opting in
// on a host with no guests fails rather than skips.
func TestTheRealTrackerFindsEachGuestsFlows(t *testing.T) {
	if os.Getenv("BILLET_TEST_REAL_CONNTRACK") != "1" {
		t.Skip("set BILLET_TEST_REAL_CONNTRACK=1 on a node host with live guests, as root")
	}

	leaseDir := os.Getenv("BILLET_TEST_LEASE_DIR")
	if leaseDir == "" {
		leaseDir = "/var/lib/billet-dnsmasq"
	}

	files, err := filepath.Glob(filepath.Join(leaseDir, "*", "dnsmasq.leases"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no lease files under %s (%v)", leaseDir, err)
	}

	tracker := NewTracker(slog.Default())
	guests, withFlows := 0, 0

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}

			mac, err := net.ParseMAC(fields[1])
			if err != nil {
				continue
			}

			addr, err := AddressFor(data, mac)
			if err != nil {
				t.Errorf("%s: the lease for %s did not read back: %v", file, mac, err)

				continue
			}

			got, err := tracker.Flows(addr)
			if err != nil {
				t.Fatalf("dump the tracker: %v", err)
			}

			guests++

			if len(got) > 0 {
				withFlows++
			}

			var sent, received uint64

			for _, f := range got {
				if f.Source != addr {
					t.Errorf("Flows(%s) returned a flow from %s", addr, f.Source)
				}

				sent, received = sent+f.OrigBytes, received+f.ReplyBytes
			}

			t.Logf("%s %s: %d flows open, %d bytes sent and %d received so far",
				filepath.Base(filepath.Dir(file)), addr, len(got), sent, received)
		}
	}

	if guests == 0 || withFlows == 0 {
		t.Fatalf("%d leased guests, %d with flows; a node host with live guests has both", guests, withFlows)
	}

	// THE BARRIER, against the live listener.
	acct := NewAccountant()
	ctx, cancel := context.WithCancel(t.Context())

	var run sync.WaitGroup

	run.Go(func() { tracker.Run(ctx, acct) })

	t.Cleanup(func() {
		cancel()
		run.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)

	for {
		syncCtx, done := context.WithTimeout(t.Context(), 2*time.Second)
		err := tracker.Sync(syncCtx)

		done()

		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the listener never proved it had read the sentinel's destruction: %v", err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	// AND AN ORDINARY DESTRUCTION REACHES THE ACCOUNTANT: an entry from a
	// watched probe address, created and deleted just before Sync, has been
	// read by the time Final runs. On a host without timestamps it arrives
	// unstamped and marks the probe incomplete; with them it is counted. This
	// checks delivery through the real listener, not that Sync is what waited
	// for it: measured on the reference host (2026-10-09), the kernel delivers
	// the event within the Delete call's round trip, so a Sync that returned at
	// once passed five runs of five. The ordering is held by the unit tests.
	// WATCHED FROM NOW, after the listener's startup gap has closed, so the
	// only thing that can mark the probe is its own destruction.
	probe, peer := netip.MustParseAddr("127.66.1.1"), netip.MustParseAddr("127.66.1.2")
	if !acct.Watch("probe", probe, time.Now(), nil) {
		t.Fatal("the probe address could not be watched")
	}

	conn, err := conntrack.Dial(nil)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	f := conntrack.NewFlow(17, 0, probe, peer, 4242, 4242, 30, 0)
	if err := conn.Create(f); err != nil {
		t.Fatalf("create the probe entry: %v", err)
	}

	if err := conn.Delete(f); err != nil {
		t.Fatalf("delete the probe entry: %v", err)
	}

	syncCtx, done := context.WithTimeout(t.Context(), 2*time.Second)
	defer done()

	if err := tracker.Sync(syncCtx); err != nil {
		t.Fatalf("sync after the probe: %v", err)
	}

	r, _ := acct.Final("probe", nil, time.Now())
	if !r.Incomplete && len(r.Destinations) == 0 {
		t.Fatal("a destruction before the barrier had not reached the accountant when Sync returned")
	}
}
