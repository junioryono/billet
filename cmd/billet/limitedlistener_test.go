package main

import (
	"errors"
	"net"
	"testing"
	"time"
)

// A SATURATED LISTENER MUST STILL CLOSE. Accept blocks acquiring a permit, and
// closing the underlying listener does not unblock a channel send -- so before
// the closed channel existed, a full semaphore left the accept goroutine stuck,
// and http.Server.Shutdown waits on that goroutine before its own context
// deadline can apply. The wait is not bounded by the idle timeout either --
// that bounds inactivity between requests, so a holder sending one more request
// before each deadline keeps its permit and the shutdown never finishes.
//
// THE ASSERTION IS THAT ACCEPT UNBLOCKS, not that Close returns nil: a listener
// closed underneath may report an error, and the property under test is that the
// goroutine is no longer stuck.
func TestASaturatedLimitedListenerStillCloses(t *testing.T) {
	t.Parallel()

	// ListenConfig, not net.Listen: the linter requires the context-taking form
	// everywhere, and a test is not an exception to that.
	var lc net.ListenConfig

	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ln := newLimitedListener(inner, 1)

	// Hold the only permit, so the next Accept blocks on the semaphore rather
	// than on the network.
	ln.semaphore <- struct{}{}

	accepted := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		accepted <- err
	}()

	// Ordering insurance only, and NOT load bearing: the sole permit is prefilled
	// and never released, so the old bare-send implementation blocks whether Accept
	// starts before or after Close. The test would prove the same thing without
	// this line; it just makes what is being exercised obvious to a reader.
	time.Sleep(50 * time.Millisecond)

	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close a saturated listener: %v", err)
	}

	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept returned %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept stayed blocked after Close, so a saturated listener cannot be shut down")
	}

	// AND A SECOND CLOSE MUST NOT PANIC. Closing an already-closed channel does,
	// which is what the once is for.
	_ = ln.Close()
}
