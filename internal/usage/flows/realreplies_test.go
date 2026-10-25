//go:build linux

package flows

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// AGAINST THE REAL CONNECTION TRACKER, A FLOW'S WAIT FOR ITS FIRST REPLY: a UDP
// query on loopback that a server answers after 1.2 s is timed slow against
// its destination, and one answered at once is timed fast, through the
// tracker's own new and reply listener and the dump Final reads them back
// with. Gated: it needs root (CAP_NET_ADMIN), the tracker's switches on, and
// BILLET_TEST_REAL_CONNTRACK=1, and opting in without the switches fails
// rather than skips. Its only change to the host is the two loopback flows,
// which expire on their own.
func TestTheRealTrackerTimesAFlowsFirstReply(t *testing.T) {
	if os.Getenv("BILLET_TEST_REAL_CONNTRACK") != "1" {
		t.Skip("set BILLET_TEST_REAL_CONNTRACK=1 on a host with the tracker's switches on, as root")
	}
	if err := Ready("/"); err != nil {
		t.Fatalf("the tracker's switches are not on: %v", err)
	}

	probe := netip.MustParseAddr("127.66.3.1")
	slowPeer, fastPeer := netip.MustParseAddr("127.66.3.2"), netip.MustParseAddr("127.66.3.3")
	const slow = 1200 * time.Millisecond

	serve := func(addr netip.Addr, delay time.Duration) {
		conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 5300)))
		if err != nil {
			t.Fatalf("listen on %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		go func() {
			buf := make([]byte, 64)
			for {
				_, from, err := conn.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				time.Sleep(delay)
				// A FAILED ANSWER IS THE CLIENT'S TIMEOUT, which fails the test there.
				if _, err := conn.WriteToUDPAddrPort([]byte("answer"), from); err != nil {
					return
				}
			}
		}()
	}
	serve(slowPeer, slow)
	serve(fastPeer, 0)

	tracker := NewTracker(slog.Default())
	acct := NewAccountant()
	ctx, cancel := context.WithCancel(t.Context())

	var run sync.WaitGroup

	run.Go(func() { tracker.Run(ctx, acct) })

	t.Cleanup(func() {
		cancel()
		run.Wait()
	})

	// WATCHED ONCE THE REPLY LISTENER IS READING, so the probe's time has no gap
	// in it and the result is timed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		acct.mu.Lock()
		up := !acct.repliesDown
		acct.mu.Unlock()

		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reply listener never started reading")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ask := func(peer netip.Addr) time.Duration {
		local := net.UDPAddrFromAddrPort(netip.AddrPortFrom(probe, 0))
		conn, err := net.DialUDP("udp", local, net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer, 5300)))
		if err != nil {
			t.Fatalf("dial %s: %v", peer, err)
		}
		defer conn.Close()

		begun := time.Now()
		if _, err := conn.Write([]byte("query")); err != nil {
			t.Fatalf("query %s: %v", peer, err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("bound the wait for %s: %v", peer, err)
		}
		if _, err := conn.Read(make([]byte, 64)); err != nil {
			t.Fatalf("no answer from %s: %v", peer, err)
		}

		return time.Since(begun)
	}
	// PROVED READING BEFORE AND AFTER: a warm-up query and a Sync show both
	// listeners consuming before the probe is watched (the first flow after a
	// listen can arrive without its new event, measured 2026-10-25), and a
	// Sync after the queries shows their events consumed before Final.
	prove := func(when string) {
		syncCtx, done := context.WithTimeout(t.Context(), 5*time.Second)
		defer done()

		if err := tracker.Sync(syncCtx); err != nil {
			t.Fatalf("sync %s: %v", when, err)
		}
	}
	ask(fastPeer)
	prove("after the warm-up")

	since := time.Now()
	if !acct.Watch("probe", probe, since, nil) {
		t.Fatal("the probe address could not be watched")
	}

	slowTook, fastTook := ask(slowPeer), ask(fastPeer)
	prove("after the queries")

	open, err := tracker.Flows(probe)
	if err != nil {
		t.Fatalf("dump the tracker: %v", err)
	}
	r, _ := acct.Final("probe", open, time.Now())

	if !r.RepliesTimed {
		t.Fatal("a job watched while the reply listener was reading is not timed")
	}
	got := map[netip.Addr]Destination{}
	for _, d := range r.Destinations {
		got[d.Addr] = d
	}
	slowDest, fastDest := got[slowPeer], got[fastPeer]
	t.Logf("slow: answered in %s, timed %+v; fast: answered in %s, timed %+v", slowTook, slowDest,
		fastTook, fastDest)

	if slowDest.TimedReplies != 1 || slowDest.SlowReplies != 1 ||
		slowDest.SlowestReply < slow-100*time.Millisecond || slowDest.SlowestReply > slow+500*time.Millisecond {
		t.Errorf("the query answered after %s reads %+v, want one timed slow reply near %s", slowTook,
			slowDest, slow)
	}
	if fastDest.TimedReplies != 1 || fastDest.SlowReplies != 0 || fastDest.SlowestReply > 200*time.Millisecond {
		t.Errorf("the query answered at once reads %+v, want one timed fast reply", fastDest)
	}
}
