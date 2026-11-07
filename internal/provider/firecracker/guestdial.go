package firecracker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// The guest's vsock, as the VMM exposes it inside the jail. The node connects to the VMM's
// socket and asks for a port; nothing listens on any network, in the guest or here.
const (
	// guestVsockCID is the guest's context id, private to its VMM.
	guestVsockCID = 3
	// guestVsockSocket is the socket inside the jail, beside the API's.
	guestVsockSocket = "run/vsock.sock"
	// vsockHandshakeTimeout bounds asking the VMM for a port.
	vsockHandshakeTimeout = 5 * time.Second
	// vsockAnswerLimit bounds what is read of the VMM's answer, which a guest cannot
	// write but a VMM the guest has taken over could.
	vsockAnswerLimit = 64
)

// vsockPath is the socket a jail's VMM exposes its guest's vsock on, from the host.
func (j jail) vsockPath() string { return filepath.Join(j.root(), guestVsockSocket) }

var _ provider.GuestDialer = (*Provider)(nil)

// DialGuest connects to a port of a running guest over its vsock.
//
// WHOSE VMM ANSWERS IS CHECKED, for the reason newVMMAPIAs gives: the socket is in a jail the
// jailed account owns, and a VMM that has been taken over can replace it with a link to another
// lease's. A guest with nothing listening is provider.ErrNoGuestListener, which says nothing about
// whether the guest is there.
func (p *Provider) DialGuest(ctx context.Context, instance string, port int) (io.ReadWriteCloser, error) {
	j := p.jailFor(instance)

	res, err := resourcesOf(j)
	if err != nil {
		return nil, fmt.Errorf("firecracker: read what %s was allocated: %w", instance, err)
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, "unix", j.vsockPath())
	if err != nil {
		return nil, fmt.Errorf("firecracker: reach %s's vsock: %w", instance, err)
	}

	stream, err := handshakeVsock(conn, res.UID, port)
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("firecracker: %s: %w", instance, err)
	}

	return stream, nil
}

// vsockStream is a vsock connection once its handshake is done: what the handshake's buffer
// already read, then the connection.
type vsockStream struct {
	*bufio.Reader
	net.Conn
}

// Read reads the buffer's bytes first.
func (s *vsockStream) Read(p []byte) (int, error) { return s.Reader.Read(p) }

// handshakeVsock asks the VMM on conn, which must be running as uid, for a port of its guest.
// Firecracker answers `OK <host port>` and otherwise closes, which is what a guest with nothing
// listening gets (documentation; no probe here reached that case, and either way it is not
// a stream).
func handshakeVsock(conn net.Conn, uid, port int) (io.ReadWriteCloser, error) {
	peer, err := peerUID(conn)
	if err != nil {
		return nil, fmt.Errorf("%w: tell whose VMM answers: %w", errVMMPeer, err)
	}

	if uid <= 0 || peer != uid {
		return nil, fmt.Errorf("%w: answered by uid %d, not the microVM's uid %d", errVMMPeer, peer, uid)
	}

	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		return nil, err
	}

	if _, err := io.WriteString(conn, "CONNECT "+strconv.Itoa(port)+"\n"); err != nil {
		return nil, err
	}

	reader := bufio.NewReaderSize(conn, 4096)

	line, err := readVsockAnswer(reader)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(line, "OK ") {
		return nil, provider.ErrNoGuestListener
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}

	return &vsockStream{Reader: reader, Conn: conn}, nil
}

// readVsockAnswer reads one bounded line. It does not quote what it read: a VMM that has been taken
// over can answer anything. THE VMM CLOSING IS ITS ANSWER, that nothing listens; a read that failed
// any other way, a deadline among them, says nothing of the guest and is not that answer.
func readVsockAnswer(r *bufio.Reader) (string, error) {
	var b strings.Builder

	for b.Len() < vsockAnswerLimit {
		c, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return "", provider.ErrNoGuestListener
		}

		if err != nil {
			return "", fmt.Errorf("read the VMM's answer on the guest's vsock: %w", err)
		}

		if c == '\n' {
			return b.String(), nil
		}

		b.WriteByte(c)
	}

	return "", provider.ErrNoGuestListener
}
