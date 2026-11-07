package nodeplane

import (
	"errors"
	"io"
	"testing"
)

// closeCounter is a stream that counts its closes.
type closeCounter struct {
	io.ReadWriter

	closes int
}

func (c *closeCounter) Close() error {
	c.closes++

	return nil
}

// A STREAM IS FOR THE NODE IT WAS ASKED OF, WITH ITS TOKEN, ONCE. Each refusal leaves it for the
// right caller, except the taking itself, after which nobody can have it.
func TestAStreamIsTakenOnceByItsNode(t *testing.T) {
	t.Parallel()

	var b streamBook

	id, wait, err := b.begin("n1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	for name, tc := range map[string]struct{ id, node, token string }{
		"no such stream": {"nope", "n1", wait.token},
		"another node":   {id, "n2", wait.token},
		"a wrong token":  {id, "n1", "wrong"},
		"no token":       {id, "n1", ""},
	} {
		if b.take(tc.id, tc.node, tc.token) != nil {
			t.Errorf("%s: it was taken", name)
		}
	}

	if b.take(id, "n1", wait.token) != wait {
		t.Fatal("its own node with its token could not take it")
	}

	if b.take(id, "n1", wait.token) != nil {
		t.Error("a token was used twice")
	}
}

// A STREAM NOBODY WAITS FOR ANY LONGER IS CLOSED, wherever it had got to: arrived already, or still
// being upgraded when its waiter gave up. Otherwise the node's bridge to a guest would stay open
// with nobody at the other end.
func TestAStreamNobodyWaitsForIsClosed(t *testing.T) {
	t.Parallel()

	var b streamBook

	id, wait, err := b.begin("n1")
	if err != nil {
		t.Fatal(err)
	}

	arrived := &closeCounter{}
	b.deliver(wait, arrived)
	b.abandon(id, wait)

	if arrived.closes != 1 {
		t.Errorf("a stream that had arrived was closed %d times", arrived.closes)
	}

	id, wait, err = b.begin("n1")
	if err != nil {
		t.Fatal(err)
	}

	if b.take(id, "n1", wait.token) == nil {
		t.Fatal("take")
	}

	b.abandon(id, wait)

	late := &closeCounter{}
	b.deliver(wait, late)

	if late.closes != 1 {
		t.Errorf("a stream arriving after its waiter left was closed %d times", late.closes)
	}
}

// A NODE'S STREAMS ARE BOUNDED, and a slot comes back when one closes.
func TestANodesStreamsAreBounded(t *testing.T) {
	t.Parallel()

	var b streamBook

	for range maxStreamsPerNode {
		if _, _, err := b.begin("n1"); err != nil {
			t.Fatalf("begin: %v", err)
		}
	}

	if _, _, err := b.begin("n1"); !errors.Is(err, ErrTooManyStreams) {
		t.Errorf("past the bound: %v", err)
	}

	if _, _, err := b.begin("n2"); err != nil {
		t.Errorf("another node's slots were taken: %v", err)
	}

	b.closed("n1")

	if _, _, err := b.begin("n1"); err != nil {
		t.Errorf("a slot given back was not free: %v", err)
	}
}

// A COUNTED STREAM GIVES ITS SLOT BACK ONCE, however often it is closed: a bridge closes both ends
// and its caller may too.
func TestACountedStreamGivesBackOnce(t *testing.T) {
	t.Parallel()

	given := 0
	s := &countedStream{ReadWriteCloser: &closeCounter{}, done: func() { given++ }}

	_ = s.Close()
	_ = s.Close()

	if given != 1 {
		t.Errorf("gave back %d slots", given)
	}
}
