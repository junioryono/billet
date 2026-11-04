package nodeplane

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A stream is one connection a node dials back on command, so a terminal reaches its guest with
// no port open on the node (ADR-017, "Access"; #511 can multiplex these later without changing
// what they carry). The plane reads none of what a stream carries.
//
// maxStreamsPerNode bounds what one node holds open.
const maxStreamsPerNode = 8

// ErrTooManyStreams says a node holds as many streams as it may.
var ErrTooManyStreams = errors.New("nodeplane: this node holds as many streams as it may")

// streamWait is a stream a command asked a node to dial back.
type streamWait struct {
	node  string
	token string
	ready chan io.ReadWriteCloser
	// gone says nobody is waiting any longer. Under the book's lock.
	gone bool
}

// streamBook is the streams being waited for and each node's count of those open. A field of its
// own lock: dispatch holds the plane's mutex and a stream arriving must not wait behind it.
type streamBook struct {
	mu      sync.Mutex
	waiting map[string]*streamWait
	open    map[string]int
}

func (b *streamBook) begin(nodeName string) (string, *streamWait, error) {
	id, err := randomHex()
	if err != nil {
		return "", nil, err
	}

	token, err := randomHex()
	if err != nil {
		return "", nil, err
	}

	wait := &streamWait{node: nodeName, token: token, ready: make(chan io.ReadWriteCloser, 1)}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.open[nodeName] >= maxStreamsPerNode {
		return "", nil, fmt.Errorf("%w: %d", ErrTooManyStreams, maxStreamsPerNode)
	}

	if b.waiting == nil {
		b.waiting, b.open = map[string]*streamWait{}, map[string]int{}
	}

	b.waiting[id] = wait
	b.open[nodeName]++

	return id, wait, nil
}

// take gives a waiting stream to its caller once: a token is single use, for the node it was
// sent to and no other.
func (b *streamBook) take(id, nodeName, token string) *streamWait {
	b.mu.Lock()
	defer b.mu.Unlock()

	wait, ok := b.waiting[id]
	if !ok || wait.node != nodeName || subtle.ConstantTimeCompare([]byte(wait.token), []byte(token)) != 1 {
		return nil
	}

	delete(b.waiting, id)

	return wait
}

// abandon forgets a stream that will not be used, closing it if it has arrived. A STREAM TAKEN AND
// STILL BEING UPGRADED IS CLOSED BY deliver, which is why this marks and does not only delete.
func (b *streamBook) abandon(id string, wait *streamWait) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.waiting, id)
	wait.gone = true

	select {
	case stream := <-wait.ready:
		_ = stream.Close()
	default:
	}
}

// deliver hands a stream to whoever waits for it, and closes it if nobody does any longer.
func (b *streamBook) deliver(wait *streamWait, stream io.ReadWriteCloser) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if wait.gone {
		_ = stream.Close()

		return
	}

	wait.ready <- stream
}

// closed gives back a node's slot.
func (b *streamBook) closed(nodeName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.open[nodeName] > 0 {
		b.open[nodeName]--
	}
}

func randomHex() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("nodeplane: make a stream's name: %w", err)
	}

	return hex.EncodeToString(raw), nil
}

// countedStream gives back its node's slot when it closes.
type countedStream struct {
	io.ReadWriteCloser

	once sync.Once
	done func()
}

// Close closes the stream and gives back the slot, once.
func (s *countedStream) Close() error {
	err := s.ReadWriteCloser.Close()
	s.once.Do(s.done)

	return err
}

// OpenGuestStream asks a node for a stream to a lease's guest, and answers it with the node dialled
// back. A node that is not registered here is ErrNoNode; one older than the wire is told by the
// pending command's minWire; either is could-not-tell about the guest. The caller closes it.
func (p *Plane) OpenGuestStream(ctx context.Context, nodeName, leaseID string) (io.ReadWriteCloser, error) {
	p.mu.Lock()
	n, known := p.nodes[nodeName]
	p.mu.Unlock()

	if !known {
		return nil, fmt.Errorf("%w: node %s is not registered", ErrNoNode, nodeName)
	}

	id, wait, err := p.streams.begin(nodeName)
	if err != nil {
		return nil, err
	}

	giveBack := func() {
		p.streams.abandon(id, wait)
		p.streams.closed(nodeName)
	}

	cmdID, err := commandID()
	if err != nil {
		giveBack()

		return nil, err
	}

	res, err := p.dispatch(ctx, n, &pending{
		cmd: nodeapi.Command{ID: cmdID, Kind: nodeapi.CommandAttach,
			Attach: &nodeapi.AttachSpec{Lease: leaseID, Stream: id, Token: wait.token}},
		done:    make(chan nodeapi.CommandResult, 1),
		minWire: nodeapi.VersionJobAttach,
	})
	if err != nil {
		giveBack()

		return nil, err
	}

	if !res.OK {
		giveBack()

		return nil, fmt.Errorf("nodeplane: node %s could not reach the guest: %s", nodeName, res.Error)
	}

	select {
	case stream := <-wait.ready:
		return &countedStream{ReadWriteCloser: stream, done: func() { p.streams.closed(nodeName) }}, nil
	case <-ctx.Done():
		giveBack()

		return nil, ctx.Err()
	}
}

// stream takes a connection a node dialled back. AUTHENTICATED BEFORE IT IS TAKEN: the wire's guard
// has named the node, and a stream's name and single-use token prove a command asked for this one.
// Nothing it carries is read here.
func (h *handler) stream(w http.ResponseWriter, r *http.Request) {
	wait := h.plane.streams.take(r.PathValue("stream"), r.PathValue("node"), r.Header.Get(nodeapi.HeaderStreamToken))
	if wait == nil {
		writeErr(w, http.StatusForbidden, nodeapi.CodeRefused, "no such stream for this node")

		return
	}

	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "", "this connection cannot carry a stream")

		return
	}

	stream, err := upgraded(conn, rw, nodeapi.StreamProtocol)
	if err != nil {
		_ = conn.Close()

		return
	}

	h.plane.streams.deliver(wait, stream)
}

// hijackedStream is a connection taken from net/http: what its buffer already read, then the
// connection.
type hijackedStream struct {
	io.Reader
	net.Conn
}

// Read reads the buffer's bytes first.
func (s *hijackedStream) Read(p []byte) (int, error) { return s.Reader.Read(p) }

// upgraded answers an upgrade on a connection taken from net/http, and is the stream it became.
func upgraded(conn net.Conn, rw *bufio.ReadWriter, protocol string) (io.ReadWriteCloser, error) {
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol +
		"\r\n\r\n"); err != nil {
		return nil, err
	}

	if err := rw.Flush(); err != nil {
		return nil, err
	}

	return &hijackedStream{Reader: rw.Reader, Conn: conn}, nil
}

// UpgradeStream answers an upgrade on an HTTP handler's connection and is the stream it became, for
// the operator's socket.
func UpgradeStream(w http.ResponseWriter, protocol string) (io.ReadWriteCloser, error) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}

	stream, err := upgraded(conn, rw, protocol)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return stream, nil
}
