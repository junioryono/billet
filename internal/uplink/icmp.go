package uplink

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Pinger sends one ICMP echo to each reflector per round over a raw socket,
// which needs CAP_NET_RAW: the shaper runs as root anyway, to change qdiscs.
type Pinger struct {
	conn       net.PacketConn
	id         uint16
	seq        uint16
	reflectors []string
}

// NewPinger opens the socket. Every reflector must be a literal IPv4 address,
// so a round never waits on a resolver that the full queue is also delaying.
func NewPinger(reflectors []string) (*Pinger, error) {
	for _, reflector := range reflectors {
		if ip := net.ParseIP(reflector); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("reflector %q is not an IPv4 address", reflector)
		}
	}

	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open an ICMP socket (the shaper runs as root): %w", err)
	}

	// A RANDOM ECHO ID, so two shapers on one host, or a ping beside this one,
	// never take each other's answers.
	var id [2]byte
	if _, err := cryptorand.Read(id[:]); err != nil {
		conn.Close() //nolint:errcheck // the socket was never used; the read's error is the one to report

		return nil, fmt.Errorf("choose an echo id: %w", err)
	}

	return &Pinger{conn: conn, id: binary.BigEndian.Uint16(id[:]), reflectors: reflectors}, nil
}

// Close closes the socket.
func (p *Pinger) Close() error { return p.conn.Close() }

// Reflectors is who each round asks.
func (p *Pinger) Reflectors() []string { return p.reflectors }

// Round asks every reflector once and waits up to wait for the answers. An
// answer that arrives later belongs to no round and is ignored.
func (p *Pinger) Round(ctx context.Context, wait time.Duration) (map[string]time.Duration, error) {
	p.seq++
	sent := make(map[string]time.Time, len(p.reflectors))

	for _, reflector := range p.reflectors {
		addr := &net.IPAddr{IP: net.ParseIP(reflector)}
		at := time.Now()

		if _, err := p.conn.WriteTo(echo(p.id, p.seq), addr); err != nil {
			// ONE REFLECTOR UNREACHABLE IS AN UNANSWERED ONE, not a failed round:
			// the delay is a median, and a lost answer already counts against it.
			continue
		}

		sent[reflector] = at
	}

	answered := make(map[string]time.Duration, len(sent))
	deadline := time.Now().Add(wait)
	buf := make([]byte, 1500)

	for len(answered) < len(sent) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if err := p.conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("bound the ICMP read: %w", err)
		}

		n, from, err := p.conn.ReadFrom(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("read an ICMP answer: %w", err)
		}

		ip, ok := from.(*net.IPAddr)
		if !ok {
			continue
		}

		at, asked := sent[ip.IP.String()]
		if !asked || !isReply(buf[:n], p.id, p.seq) {
			continue
		}

		answered[ip.IP.String()] = time.Since(at)
	}

	return answered, nil
}

// echo is an ICMP echo request with this id and sequence and no payload.
func echo(id, seq uint16) []byte {
	msg := make([]byte, 8)
	msg[0] = 8 // echo request
	binary.BigEndian.PutUint16(msg[4:], id)
	binary.BigEndian.PutUint16(msg[6:], seq)
	binary.BigEndian.PutUint16(msg[2:], checksum(msg))

	return msg
}

// isReply reports whether msg is the echo reply to this id and sequence. A raw
// IPv4 socket on Linux hands back the ICMP message without its IP header.
func isReply(msg []byte, id, seq uint16) bool {
	return len(msg) >= 8 && msg[0] == 0 && msg[1] == 0 &&
		binary.BigEndian.Uint16(msg[4:]) == id && binary.BigEndian.Uint16(msg[6:]) == seq
}

func checksum(b []byte) uint16 {
	var sum uint32

	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}

	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}

	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}

	return ^uint16(sum)
}
