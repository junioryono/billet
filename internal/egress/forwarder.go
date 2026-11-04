package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// upstreamLimit bounds one query's wait for the host's resolver. A guest's own
	// stub retries sooner than this, and a query past it gets no answer, exactly
	// as one the upstream lost.
	upstreamLimit = 5 * time.Second
	// tcpIdleLimit bounds how long a TCP client may hold a connection between
	// queries, since each one holds a goroutine.
	tcpIdleLimit = 10 * time.Second
	// maxInFlight bounds what one guest has outstanding, across both protocols.
	maxInFlight = 128
	// maxFollowed bounds the names an allowed name's answers may lead a guest to.
	maxFollowed = 1024
	// maxResponse is the largest DNS message there is.
	maxResponse = 65535
)

// exchange asks the upstream one query, over whichever protocol the guest used.
type exchange func(ctx context.Context, query []byte) ([]byte, error)

// forwarder is one guest's resolver: a UDP socket and a TCP listener on one port
// of its gateway's address, which only its own chain's redirect reaches, so
// whatever arrives here is that guest's whatever source address it carries.
type forwarder struct {
	policy   *Policy
	rec      *recorder
	upstream netip.AddrPort
	log      *slog.Logger
	// admit puts addresses a listed name answered in the guest's set, returning
	// once a packet to them would pass. An error means they will not.
	admit func(ctx context.Context, addrs []netip.Addr) error

	udp      *net.UDPConn
	tcp      *net.TCPListener
	port     uint16
	inFlight chan struct{}
	// clients is the guest's subnet. A query from elsewhere goes unanswered, since an
	// answer would be the host sending its bytes wherever the query said it was from.
	clients netip.Prefix
	// followed is the names an allowed name's answers passed through. A real guest's stub
	// asks for them (measured 2026-11-04), and refusing that keeps out nothing: the chain's addresses are admitted already, and a guest does not
	// choose what the chain says. Past its bound a name is refused, which fails closed.
	followMu sync.Mutex
	followed map[string]struct{}
	// warnedAdmit says the failure to admit an answer has been logged, once: a guest can
	// ask as fast as it likes, and each is a line otherwise.
	warnedAdmit atomic.Bool

	// cancel ends what serve started, nil until then.
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// listenPair binds one kernel-chosen port on addr for both protocols. The TCP
// half can find a port the UDP half's choice has taken, which is why it retries.
func listenPair(addr netip.Addr) (*net.UDPConn, *net.TCPListener, uint16, error) {
	var lastErr error

	for range 16 {
		udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 0)))
		if err != nil {
			return nil, nil, 0, fmt.Errorf("egress: bind a resolver on %s: %w", addr, err)
		}

		local, ok := udp.LocalAddr().(*net.UDPAddr)
		if !ok {
			_ = udp.Close()

			return nil, nil, 0, fmt.Errorf("egress: a resolver on %s is bound to %s, which is not a UDP address", addr, udp.LocalAddr())
		}

		port := local.AddrPort().Port()

		tcp, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(addr, port)))
		if err == nil {
			return udp, tcp, port, nil
		}

		_ = udp.Close()
		lastErr = err
	}

	return nil, nil, 0, fmt.Errorf("egress: bind a resolver on %s: %w", addr, lastErr)
}

// newForwarder returns a forwarder bound and not yet serving, so its port can be
// put in the guest's rules before anything can reach it.
func newForwarder(policy *Policy, rec *recorder, gateway netip.Prefix, upstreamPort uint16, log *slog.Logger,
	admit func(context.Context, []netip.Addr) error,
) (*forwarder, error) {
	udp, tcp, port, err := listenPair(gateway.Addr())
	if err != nil {
		return nil, err
	}

	return &forwarder{
		policy: policy, rec: rec, upstream: netip.AddrPortFrom(gateway.Addr(), upstreamPort), log: log, admit: admit,
		udp: udp, tcp: tcp, port: port, inFlight: make(chan struct{}, maxInFlight), clients: gateway.Masked(),
	}, nil
}

// serve starts answering, until stop. WITHOUT THE CALLER'S CANCELLATION: a guest's
// resolver lives as long as the guest, and the launch that attached it returns long
// before that.
func (f *forwarder) serve(ctx context.Context) {
	ctx, f.cancel = context.WithCancel(context.WithoutCancel(ctx))

	f.wg.Add(2)

	go func() {
		defer f.wg.Done()
		f.serveUDP(ctx)
	}()

	go func() {
		defer f.wg.Done()
		f.serveTCP(ctx)
	}()
}

// stop closes the sockets and waits for every query in flight. A forwarder that
// was never served has nothing to wait for.
func (f *forwarder) stop() {
	if f.cancel != nil {
		f.cancel()
	}

	_ = f.udp.Close()
	_ = f.tcp.Close()
	f.wg.Wait()
}

func (f *forwarder) serveUDP(ctx context.Context) {
	for {
		buf := make([]byte, maxQuery+1)

		n, client, err := f.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.log.Warn("the guest resolver stopped reading", "protocol", "udp", "error", err)
				f.rec.incomplete()
			}

			return
		}

		if !f.clients.Contains(client.Addr().Unmap()) {
			f.rec.incomplete()

			continue
		}

		// OVER THE BOUND IS DROPPED, as a guest's stub retries a lost datagram: a
		// query is held for as long as its upstream takes, and a guest flooding
		// its own resolver is left waiting on itself.
		select {
		case f.inFlight <- struct{}{}:
		default:
			f.rec.incomplete()

			continue
		}

		f.wg.Add(1)

		go func() {
			defer f.wg.Done()
			defer func() { <-f.inFlight }()

			resp := f.answer(ctx, buf[:n], f.exchangeUDP)
			if resp == nil {
				return
			}

			if _, err := f.udp.WriteToUDPAddrPort(resp, client); err != nil && !errors.Is(err, net.ErrClosed) {
				f.log.Debug("could not answer a guest's query", "error", err)
			}
		}()
	}
}

func (f *forwarder) serveTCP(ctx context.Context) {
	for {
		conn, err := f.tcp.AcceptTCP()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.log.Warn("the guest resolver stopped reading", "protocol", "tcp", "error", err)
				f.rec.incomplete()
			}

			return
		}

		select {
		case f.inFlight <- struct{}{}:
		default:
			_ = conn.Close()
			f.rec.incomplete()

			continue
		}

		f.wg.Add(1)

		go func() {
			defer f.wg.Done()
			defer func() { <-f.inFlight }()

			f.serveConn(ctx, conn)
		}()
	}
}

// serveConn answers one TCP client's queries, one at a time.
func (f *forwarder) serveConn(ctx context.Context, conn *net.TCPConn) {
	defer conn.Close()

	// CLOSED ON STOP, which is what ends a read an idle client is holding.
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	for {
		if err := conn.SetDeadline(time.Now().Add(tcpIdleLimit)); err != nil {
			return
		}

		query, err := readFrame(conn, maxQuery)
		if err != nil {
			return
		}

		resp := f.answer(ctx, query, f.exchangeTCP)
		if resp == nil {
			return
		}

		if err := conn.SetDeadline(time.Now().Add(tcpIdleLimit)); err != nil {
			return
		}

		if err := writeFrame(conn, resp); err != nil {
			return
		}
	}
}

// answer is what a guest is told about one query, or nil for no answer at all.
//
// A LISTED GUEST'S QUERY IS FORWARDED ONLY WHEN THE POLICY ALLOWS WHAT IT PLAINLY
// ASKS, because a forwarded name is itself a way out: a query for
// <data>.attacker.example reaches attacker.example's nameserver whatever the
// answer is. So a name off the list, and a query this reader cannot read, stop
// here. An open guest's query is forwarded whatever it says, as dnsmasq would
// have taken it, and it is the record that goes without.
//
// THE ADDRESSES ARE IN THE SET BEFORE THE ANSWER LEAVES, so a guest that connects
// the moment it hears never finds them refused. An answer whose addresses could
// not be admitted is a SERVFAIL, the guest's to retry, rather than an address it
// would be refused.
func (f *forwarder) answer(ctx context.Context, query []byte, ask exchange) []byte {
	f.rec.query()

	read, readable := readQuestion(query)

	if f.policy.Listed() {
		if !readable {
			f.rec.refuse("")

			return refusal(query, read, false, dnsmessage.RCodeFormatError)
		}

		if !f.policy.AllowsName(read.name) && !f.isFollowed(read.name) {
			f.rec.refuse(read.name)

			return refusal(query, read, true, dnsmessage.RCodeRefused)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, upstreamLimit)
	defer cancel()

	forwarded := query
	if f.policy.Listed() {
		forwarded = plainQuery(query, read)
	}

	resp, err := ask(ctx, forwarded)
	if err != nil {
		// THE WHOLE ANSWER, and no more of it, is what a guest would have had from
		// an upstream that lost its query: nothing, and its own retry.
		f.log.Debug("the host's resolver did not answer a guest's query", "error", err)

		return nil
	}

	if !readable {
		f.rec.incomplete()

		return resp
	}

	got, err := readAnswer(resp, read)
	if err != nil {
		f.rec.incomplete()

		if f.policy.Listed() {
			f.log.Warn("could not read what the host's resolver answered a listed guest, so it is "+
				"refused", "error", err)

			return refusal(query, read, true, dnsmessage.RCodeServerFailure)
		}

		return resp
	}

	if f.policy.Listed() {
		var v4 []netip.Addr

		for _, addr := range got.addrs {
			if addr.Is4() {
				v4 = append(v4, addr)
			}
		}

		if err := f.admit(ctx, v4); err != nil {
			f.rec.incomplete()

			if !f.warnedAdmit.Swap(true) {
				f.log.Warn("could not admit what a listed name answered, so the guest's query fails", "error", err)
			}

			return refusal(query, read, true, dnsmessage.RCodeServerFailure)
		}

		f.follow(got.names)
	}

	f.rec.answer(got)

	return resp
}

func (f *forwarder) isFollowed(name string) bool {
	f.followMu.Lock()
	defer f.followMu.Unlock()

	_, ok := f.followed[name]

	return ok
}

func (f *forwarder) follow(names []string) {
	f.followMu.Lock()
	defer f.followMu.Unlock()

	if f.followed == nil {
		f.followed = map[string]struct{}{}
	}

	for _, name := range names {
		if len(f.followed) < maxFollowed && !f.policy.AllowsName(name) {
			f.followed[name] = struct{}{}
		}
	}
}

func (f *forwarder) exchangeUDP(ctx context.Context, query []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", f.upstream.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}

	buf := make([]byte, maxResponse)

	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}

func (f *forwarder) exchangeTCP(ctx context.Context, query []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", f.upstream.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := writeFrame(conn, query); err != nil {
		return nil, err
	}

	return readFrame(conn, maxResponse)
}

// readFrame reads one length-prefixed DNS message, refusing one over limit.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}

	n := int(binary.BigEndian.Uint16(size[:]))
	if n > limit {
		return nil, fmt.Errorf("egress: a %d byte DNS message is over the %d this reads", n, limit)
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	return buf, nil
}

// writeFrame writes one length-prefixed DNS message in one write, since a
// stub's first read may be its only one.
func writeFrame(w io.Writer, msg []byte) error {
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf, uint16(len(msg)))
	copy(buf[2:], msg)

	_, err := w.Write(buf)

	return err
}
