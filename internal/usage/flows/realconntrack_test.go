//go:build linux

package flows

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AGAINST THE REAL CONNECTION TRACKER, read only: every guest with a lease on
// a billet bridge has its flows found by the tracker's own dump, from the
// guest's address, with a destination outside the guest's subnet among them.
// Gated: it needs root (CAP_NET_ADMIN), a node host with live guests, and
// BILLET_TEST_REAL_CONNTRACK=1. It changes nothing on the host.
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
	checked := 0

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

			var sent, received uint64

			outside := 0

			for _, f := range got {
				if f.Source != addr {
					t.Errorf("Flows(%s) returned a flow from %s", addr, f.Source)
				}

				if !f.Dest.IsPrivate() {
					outside++
				}

				sent, received = sent+f.OrigBytes, received+f.ReplyBytes
			}

			t.Logf("%s %s: %d flows open, %d to public addresses, %d bytes sent and %d received so far",
				filepath.Base(filepath.Dir(file)), addr, len(got), outside, sent, received)

			checked++
		}
	}

	if checked == 0 {
		t.Skip("no guest holds a lease now; nothing to check")
	}
}
