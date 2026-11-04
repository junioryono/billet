package nodeclient_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// guestTerminal is a Compute whose guest is one end of a pipe the test holds the other end of.
type guestTerminal struct {
	*fakeCompute

	mu     sync.Mutex
	guests []net.Conn
	err    error
	asked  []string
}

func (g *guestTerminal) OpenGuestTerminal(_ context.Context, leaseID string) (io.ReadWriteCloser, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.asked = append(g.asked, leaseID)
	if g.err != nil {
		return nil, g.err
	}

	node, guest := net.Pipe()
	g.guests = append(g.guests, guest)

	return node, nil
}

func (g *guestTerminal) guest(t *testing.T) net.Conn {
	t.Helper()

	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.guests) == 0 {
		t.Fatal("no guest was opened")
	}

	return g.guests[len(g.guests)-1]
}

// A TERMINAL REACHES ITS GUEST OVER A STREAM THE NODE DIALLED BACK, carrying bytes both ways, and
// closing either end ends the other. Through a real plane, a real client and a real upgrade: the
// connection is what net/http hands back, which a fake of it would not prove.
func TestAStreamReachesTheGuest(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	compute := &guestTerminal{fakeCompute: &fakeCompute{}}
	runLoopWith(t, c, compute, 1)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	stream, err := p.OpenGuestStream(t.Context(), p.Nodes()[0], "lease-1")
	if err != nil {
		t.Fatalf("OpenGuestStream: %v", err)
	}
	defer stream.Close()

	if compute.asked[0] != "lease-1" {
		t.Errorf("the node opened %v", compute.asked)
	}

	guest := compute.guest(t)
	deadline := time.Now().Add(30 * time.Second)
	if err := guest.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	sent := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("from the operator"))
		sent <- err
	}()

	got := make([]byte, len("from the operator"))
	if _, err := io.ReadFull(guest, got); err != nil || string(got) != "from the operator" {
		t.Fatalf("the guest read %q, %v", got, err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("write: %v", err)
	}

	go func() { _, _ = guest.Write([]byte("from the guest")) }() //nolint:errcheck // the read at the other end is what is checked

	back := make([]byte, len("from the guest"))
	if _, err := io.ReadFull(stream, back); err != nil || string(back) != "from the guest" {
		t.Fatalf("the operator read %q, %v", back, err)
	}

	// THE GUEST GOING IS THE STREAM ENDING: a VM destroyed under a session ends the session.
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}

	ended := make(chan error, 1)
	go func() {
		_, err := stream.Read(make([]byte, 1))
		ended <- err
	}()

	select {
	case err := <-ended:
		if err == nil {
			t.Error("a stream read after its guest went answered no error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stream outlived its guest")
	}
}

// WHAT THE NODE COULD NOT REACH IS THE ANSWER, and takes no slot for good.
func TestAGuestThatCannotBeReachedSaysSo(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	compute := &guestTerminal{fakeCompute: &fakeCompute{}, err: errors.New("nothing is serving a terminal")}
	runLoopWith(t, c, compute, 1)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	for range 16 {
		_, err := p.OpenGuestStream(t.Context(), p.Nodes()[0], "lease-1")
		if err == nil {
			t.Fatal("a guest that could not be reached opened")
		}

		if !containsText(err, "nothing is serving a terminal") {
			t.Fatalf("the answer was %v", err)
		}
	}
}

// A NODE WHOSE COMPUTE HAS NO TERMINALS SAYS SO, and the loop goes on.
func TestANodeWithNoTerminalsSaysSo(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	runLoopWith(t, c, &fakeCompute{}, 1)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	if _, err := p.OpenGuestStream(t.Context(), p.Nodes()[0], "lease-1"); err == nil {
		t.Error("a node with no terminals opened one")
	}

	if err := p.NewRunner().Destroy(t.Context(), 42); err != nil {
		t.Errorf("the loop did not go on: %v", err)
	}
}

// A STREAM NOBODY ASKED FOR IS REFUSED, whatever the node presents.
func TestAStreamNobodyAskedForIsRefused(t *testing.T) {
	t.Parallel()

	p, c := harnessWithCommandTimeout(t, 30*time.Second)
	runLoopWith(t, c, &fakeCompute{}, 1)
	waitFor(t, func() bool { return len(p.Nodes()) == 1 })

	if _, err := c.DialStream(t.Context(), "no-such-stream", "no-such-token"); err == nil {
		t.Error("a stream no command asked for was taken")
	}
}

func containsText(err error, text string) bool { return strings.Contains(err.Error(), text) }
