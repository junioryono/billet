// Package relay is the terminal's words: the frames an operator's `billet jobs attach` and
// the server in a job's VM speak to each other, and the copy that joins two streams
// between them.
//
// THE NODE AND THE CONTROL PLANE NEVER READ A FRAME. They carry bytes from one end to the
// other, so nothing in a session can be quoted into their logs or decide anything there.
// Both ends are the only readers, and neither quotes what the other sent in an error.
//
// One stream to a terminal, and a stream's frames need no number: workspaces' multiplexed
// channel (#511) can carry the same frames over a stream of its own.
package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The frame kinds.
const (
	// KindData is bytes for the terminal, in either direction.
	KindData byte = 1
	// KindResize is the operator's window: four bytes of rows and columns, two each.
	KindResize byte = 2
	// KindExit is the guest saying its shell ended: four bytes of its exit status.
	KindExit byte = 3
)

// MaxFrame bounds a frame's payload, which a reader allocates before it knows what it
// holds.
const MaxFrame = 64 << 10

// ErrFrame is a frame neither end writes: a kind nobody knows, or past its bound. It never
// says what the frame held.
var ErrFrame = errors.New("relay: not a frame of this protocol")

// Frame is one frame.
type Frame struct {
	Kind    byte
	Payload []byte
}

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("%w: a payload past its bound", ErrFrame)
	}

	head := make([]byte, 5, 5+len(payload))
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))

	_, err := w.Write(append(head, payload...))

	return err
}

// ReadFrame reads one frame, refusing a kind it does not know and a payload past its bound
// before it allocates for it.
func ReadFrame(r io.Reader) (Frame, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, err
	}

	switch head[0] {
	case KindData, KindResize, KindExit:
	default:
		return Frame{}, ErrFrame
	}

	size := binary.BigEndian.Uint32(head[1:])
	if size > MaxFrame {
		return Frame{}, ErrFrame
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}

	return Frame{Kind: head[0], Payload: payload}, nil
}

// ResizePayload is a window as a resize frame carries it.
func ResizePayload(rows, cols uint16) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint16(out, rows)
	binary.BigEndian.PutUint16(out[2:], cols)

	return out
}

// ParseResize reads a resize frame's window: its rows, then its columns.
func ParseResize(payload []byte) (uint16, uint16, error) {
	if len(payload) != 4 {
		return 0, 0, ErrFrame
	}

	return binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]), nil
}

// ExitPayload is a shell's exit status as an exit frame carries it.
func ExitPayload(status int) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(int32(status)))

	return out
}

// ParseExit reads an exit frame's status.
func ParseExit(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, ErrFrame
	}

	return int(int32(binary.BigEndian.Uint32(payload))), nil
}

// Bridge copies bytes both ways until either side ends or ctx does, then closes both, so a
// stream never outlives its other end. It reads nothing of what it carries, and returns
// once both copies have stopped.
func Bridge(ctx context.Context, a, b io.ReadWriteCloser) {
	var once sync.Once

	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	defer closeBoth()

	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()

	var wg sync.WaitGroup

	wg.Go(func() {
		defer closeBoth()

		_, _ = io.Copy(a, b) //nolint:errcheck // either end stopping is all this waits for, and closing both is the answer to any failure
	})
	wg.Go(func() {
		defer closeBoth()

		_, _ = io.Copy(b, a) //nolint:errcheck // either end stopping is all this waits for, and closing both is the answer to any failure
	})
	wg.Wait()
}
