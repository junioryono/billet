package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestAFrameRoundTrips(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := WriteFrame(&b, KindData, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, KindResize, ResizePayload(24, 80)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, KindExit, ExitPayload(-1)); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFrame(&b)
	if err != nil || f.Kind != KindData || string(f.Payload) != "hello" {
		t.Fatalf("the data frame reads %+v, %v", f, err)
	}

	f, err = ReadFrame(&b)
	if err != nil {
		t.Fatal(err)
	}
	if rows, cols, err := ParseResize(f.Payload); err != nil || rows != 24 || cols != 80 {
		t.Errorf("the resize frame reads %d, %d, %v", rows, cols, err)
	}

	f, err = ReadFrame(&b)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := ParseExit(f.Payload); err != nil || status != -1 {
		t.Errorf("the exit frame reads %d, %v", status, err)
	}

	if _, err := ReadFrame(&b); !errors.Is(err, io.EOF) {
		t.Errorf("past the last frame: %v", err)
	}
}

// A FRAME NEITHER END WRITES IS REFUSED BEFORE ANYTHING IS ALLOCATED FOR IT, by the sentinel,
// whatever a hostile guest claims its length to be.
func TestAFrameIsHeldToItsBounds(t *testing.T) {
	t.Parallel()

	if _, err := ReadFrame(bytes.NewReader([]byte{99, 0, 0, 0, 0})); !errors.Is(err, ErrFrame) {
		t.Errorf("a kind nobody knows: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{KindData, 0xff, 0xff, 0xff, 0xff})); !errors.Is(err, ErrFrame) {
		t.Errorf("a length past the bound: %v", err)
	}
	if err := WriteFrame(io.Discard, KindData, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrame) {
		t.Errorf("a payload past the bound: %v", err)
	}
	if _, _, err := ParseResize([]byte{1}); !errors.Is(err, ErrFrame) {
		t.Errorf("a short resize: %v", err)
	}
	if _, err := ParseExit(nil); !errors.Is(err, ErrFrame) {
		t.Errorf("a short exit: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{KindData, 0, 0, 0, 5, 1})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a frame cut short: %v", err)
	}
}

// waitDone fails the test if a bridge outlives what should have ended it. A failure guard, never
// a condition: done closes as soon as the bridge returns.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the bridge did not end")
	}
}

// A BRIDGE CARRIES BOTH WAYS AND ENDS WHEN EITHER SIDE DOES, closing the other, so a stream never
// outlives the end it was joining.
func TestABridgeCarriesBothWaysAndEndsWithEitherSide(t *testing.T) {
	t.Parallel()

	operator, nearSide := net.Pipe()
	guest, farSide := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)

		Bridge(t.Context(), nearSide, farSide)
	}()

	go func() {
		_, _ = operator.Write([]byte("up")) //nolint:errcheck // the read at the other end is what is checked
	}()

	buf := make([]byte, 2)
	if _, err := io.ReadFull(guest, buf); err != nil || string(buf) != "up" {
		t.Fatalf("toward the guest: %q, %v", buf, err)
	}

	go func() {
		_, _ = guest.Write([]byte("dn")) //nolint:errcheck // the read at the other end is what is checked
	}()

	if _, err := io.ReadFull(operator, buf); err != nil || string(buf) != "dn" {
		t.Fatalf("toward the operator: %q, %v", buf, err)
	}

	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}

	waitDone(t, done)

	if _, err := operator.Read(buf); err == nil {
		t.Error("the other side stayed open after the bridge ended")
	}
}

// A STOP ENDS A BRIDGE, however quiet both sides are.
func TestAStopEndsABridge(t *testing.T) {
	t.Parallel()

	_, nearSide := net.Pipe()
	_, farSide := net.Pipe()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		Bridge(ctx, nearSide, farSide)
	}()

	cancel()
	waitDone(t, done)
}
