package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// The bounds on one delivery. A destination that hangs costs its own queue this
// long a try and nothing else.
const (
	sendTimeout        = 10 * time.Second
	sendDialTimeout    = 5 * time.Second
	sendTLSTimeout     = 5 * time.Second
	sendHeaderTimeout  = 8 * time.Second
	maxResponseSkimmed = 64 * 1024
)

// ErrPermanent marks a delivery that trying again cannot help: the destination
// answered that it will not take this, or was never given a usable address.
var ErrPermanent = errors.New("alerts: the destination will not accept this")

// Sender delivers notices to one destination.
//
// IT HOLDS A CREDENTIAL IN AN UNEXPORTED FIELD, so it redacts itself: fmt, encoding/json
// and slog reach an unexported field by reflection without calling its methods, so
// the URL's own redaction does not cover a struct holding one. On value receivers, so
// a Sender reached as a value, or through an exported field, is covered too; one held
// in an unexported field is not, since fmt calls no method through one.
type Sender struct {
	name    string
	url     URL
	reason  string
	timeout time.Duration
	client  *http.Client
}

// NewSender builds the sender for one destination's address.
func NewSender(name string, address URL) *Sender {
	dialer := &net.Dialer{Timeout: sendDialTimeout, KeepAlive: 30 * time.Second}

	return &Sender{name: name, url: address, timeout: sendTimeout, client: &http.Client{
		Timeout: sendTimeout,
		// A REDIRECT IS REFUSED, NOT FOLLOWED: a notice is not for wherever a destination
		// sends it, and a redirect is how an address the operator never named would
		// receive it. The refusal's own error is discarded with the rest.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// NOTHING IS READ FROM A CONNECTION BEFORE ITS REQUEST IS WRITTEN. The
			// transport reports bytes it finds on a connection that owes it no answer
			// through the standard logger, quoting them, and a destination that writes the
			// moment it is reached would otherwise land there. An HTTPS connection is
			// gated after billet's own handshake, so its first write is the request; one
			// through a proxy is handshaken by the transport, after the gate's first write.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err //nolint:wrapcheck // classed by sendFailureClass, never quoted.
				}

				return quietUntilWritten(conn), nil
			},
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialTLS(ctx, dialer, network, addr)
			},
			TLSHandshakeTimeout:   sendTLSTimeout,
			ResponseHeaderTimeout: sendHeaderTimeout,
			// NO CONNECTION IS KEPT. An idle HTTP/1.1 connection that a destination
			// writes to unasked is reported by the transport through the standard
			// logger, quoting what it wrote, and a destination can write the address
			// back; a closed connection is never read again. A notice is rare enough
			// that a fresh connection each costs nothing. HTTP/1.1 only, for the same
			// reason: one connection, one request.
			DisableKeepAlives: true,
			ForceAttemptHTTP2: false,
		},
	}}
}

// dialTLS dials and handshakes a destination itself, verifying its certificate for
// the name dialled, and gates the connection on its first write after the handshake.
func dialTLS(ctx context.Context, dialer *net.Dialer, network, addr string) (net.Conn, error) {
	raw, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err //nolint:wrapcheck // classed by sendFailureClass, never quoted.
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = raw.Close()

		return nil, err //nolint:wrapcheck // classed by sendFailureClass, never quoted.
	}

	handshake, cancel := context.WithTimeout(ctx, sendTLSTimeout)
	defer cancel()

	conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(handshake); err != nil {
		_ = raw.Close()

		return nil, err //nolint:wrapcheck // classed by sendFailureClass, never quoted.
	}

	return quietUntilWritten(conn), nil
}

// gatedConn reads nothing until it has written once, or until it is closed.
type gatedConn struct {
	net.Conn

	wrote, closed         chan struct{}
	wroteOnce, closedOnce sync.Once
}

func quietUntilWritten(conn net.Conn) net.Conn {
	return &gatedConn{Conn: conn, wrote: make(chan struct{}), closed: make(chan struct{})}
}

// Read waits for the first write: a destination's bytes before the request are not
// an answer to anything.
func (c *gatedConn) Read(b []byte) (int, error) {
	select {
	case <-c.wrote:
	case <-c.closed:
		return 0, net.ErrClosed
	}

	return c.Conn.Read(b) //nolint:wrapcheck // the connection's own error, for the transport.
}

func (c *gatedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.wroteOnce.Do(func() { close(c.wrote) })

	return n, err //nolint:wrapcheck // the connection's own error, for the transport.
}

func (c *gatedConn) Close() error {
	c.closedOnce.Do(func() { close(c.closed) })

	return c.Conn.Close() //nolint:wrapcheck // the connection's own error, for the transport.
}

// UnusableSender is a destination whose address could not be had. It stays in the
// set so a rule naming it records why nothing arrived, rather than its notices
// vanishing: a missing secret must not stop a control plane, and must not be silent.
// reason says why, and must quote nothing of the value.
func UnusableSender(name, reason string) *Sender {
	return &Sender{name: name, reason: reason}
}

// Name is the destination's name in the config.
func (s Sender) Name() string { return s.name }

// String names the destination and shows nothing of its address.
func (s Sender) String() string { return "alerts.Sender{" + s.name + "}" }

// GoString covers %#v, which does not consult String.
func (s Sender) GoString() string { return s.String() }

// Format makes every verb safe, %+v above all, which walks an unexported field.
func (s Sender) Format(f fmt.State, _ rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(f, s.String())
}

// MarshalJSON renders the destination's name and nothing else.
func (s Sender) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(s.String())
	if err != nil {
		return nil, fmt.Errorf("alerts: marshal a sender: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (s Sender) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Send delivers one rendered notice. nil is an answer of 2xx. An error wrapping
// ErrPermanent is not worth another try; any other is.
//
// EVERY ERROR IS BUILT HERE FROM THE CLASS OF THE FAILURE, and none quotes what the
// client returned. A *url.Error names the URL, and a Slack URL's path is the secret,
// so scrubbing a query is not enough; and a response body is the destination's to
// write, so only its status is kept.
func (s Sender) Send(ctx context.Context, body []byte) error {
	if s.reason != "" {
		return fmt.Errorf("%w: %s", ErrPermanent, s.reason)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(s.url), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: the request could not be built", ErrPermanent)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "billet")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("alerts: the request failed: %s", sendFailureClass(err))
	}
	defer resp.Body.Close()

	// DRAINED SO THE CONNECTION IS REUSED, and bounded so a destination cannot make
	// this read forever. What it says is not kept.
	//nolint:errcheck // what the destination says is not used, and a failed read says nothing about the status it already gave.
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSkimmed))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("alerts: the destination answered %d", resp.StatusCode)
	default:
		return fmt.Errorf("%w: the destination answered %d", ErrPermanent, resp.StatusCode)
	}
}

// sendFailureClass names what stopped a request, from fixed words only.
func sendFailureClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "it was cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "it timed out"
	}
	if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
		return "it timed out"
	}
	if dns, ok := errors.AsType[*net.DNSError](err); ok && dns != nil {
		return "the host name did not resolve"
	}
	if op, ok := errors.AsType[*net.OpError](err); ok {
		// op.Op IS A FIXED WORD (dial, read, write); op.Addr is the destination's.
		return "the connection failed (" + op.Op + ")"
	}

	return "the connection broke"
}
