package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var (
	anAddr      = netip.MustParseAddr("192.0.2.10")
	anotherAddr = netip.MustParseAddr("192.0.2.11")
)

func listed(t *testing.T, allow ...string) *Policy {
	t.Helper()

	p, err := Compile(allow)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// journal is the order things happened in, which is what several of these prove.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.events = append(j.events, fmt.Sprintf(format, args...))
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()

	return slices.Clone(j.events)
}

func aForwarder(p *Policy, j *journal, admitErr error) *forwarder {
	return &forwarder{
		policy: p, rec: newRecorder(p.Listed(), false), log: slog.Default(),
		admit: func(_ context.Context, addrs []netip.Addr) error {
			j.add("admit %v", addrs)

			return admitErr
		},
		ctx: context.Background(),
	}
}

// rcode is an answer's response code, failing on no answer.
func rcode(t *testing.T, resp []byte) dnsmessage.RCode {
	t.Helper()

	var p dnsmessage.Parser

	header, err := p.Start(resp)
	if err != nil {
		t.Fatalf("answer %x does not parse: %v", resp, err)
	}

	return header.RCode
}

// A NAME OFF THE LIST NEVER LEAVES THE HOST. A query for <data>.attacker.example
// reaches that domain's nameserver whatever comes back, so a stopped answer is not
// enough: the upstream must not hear it.
func TestAListedGuestsOffListNameIsRefusedAndNeverForwarded(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "allowed.example"), j, nil)

	resp := f.answer(t.Context(), aQuery(t, "secret-data.attacker.example.", dnsmessage.TypeA),
		func(context.Context, []byte) ([]byte, error) {
			j.add("forwarded")

			return nil, errors.New("not reached")
		})

	if got := rcode(t, resp); got != dnsmessage.RCodeRefused {
		t.Fatalf("rcode = %v, want refused", got)
	}

	if events := j.all(); len(events) != 0 {
		t.Fatalf("a refused name reached %v", events)
	}

	rec := f.rec.freeze(counts{})
	if rec.Queries != 1 || rec.RefusedQueries != 1 || len(rec.Refused) != 1 ||
		rec.Refused[0] != (Refused{Name: "secret-data.attacker.example", Count: 1}) {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAListedGuestsUnreadableQueryIsRefusedAndNeverForwarded(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "allowed.example"), j, nil)

	// ONE LABEL SPELLING github.com, WHICH EVERY LIST ALLOWS.
	resp := f.answer(t.Context(), rawQuery("github.com"), func(context.Context, []byte) ([]byte, error) {
		j.add("forwarded")

		return nil, errors.New("not reached")
	})

	if got := rcode(t, resp); got != dnsmessage.RCodeFormatError {
		t.Fatalf("rcode = %v, want a format error", got)
	}

	if events := j.all(); len(events) != 0 {
		t.Fatalf("an unreadable query reached %v", events)
	}

	rec := f.rec.freeze(counts{})
	if rec.RefusedQueries != 1 || len(rec.Refused) != 0 {
		t.Fatalf("record = %+v: it should count, and name nothing it could not read", rec)
	}
}

// THE ADDRESS IS ADMITTED BEFORE THE GUEST HEARS IT. The answer returning is what
// lets the guest connect, so an admit that came after could be beaten by the connect.
func TestAnAddressIsAdmittedBeforeTheAnswerReturns(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "allowed.example"), j, nil)
	q := aQuery(t, "allowed.example.", dnsmessage.TypeA)

	resp := f.answer(t.Context(), q, func(context.Context, []byte) ([]byte, error) {
		j.add("forwarded")

		return anAnswer(t, q, []string{"edge.cdn.example."}, anAddr, netip.MustParseAddr("2001:db8::1")), nil
	})
	j.add("returned")

	if got := rcode(t, resp); got != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", got)
	}

	want := []string{"forwarded", "admit [192.0.2.10]", "returned"}
	if got := j.all(); !slices.Equal(got, want) {
		t.Fatalf("order = %q, want %q (the IPv6 address is not admitted: a listed guest has none)", got, want)
	}

	rec := f.rec.freeze(counts{})
	if !slices.Equal(rec.Names[anAddr], []string{"allowed.example", "edge.cdn.example"}) {
		t.Fatalf("names = %+v", rec.Names)
	}
}

// AN ANSWER WHOSE ADDRESSES WERE NOT ADMITTED IS NOT GIVEN: the guest would connect,
// be refused, and nothing would say why.
func TestAnAnswerThatCouldNotBeAdmittedFails(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "allowed.example"), j, errors.New("the set is full"))
	q := aQuery(t, "allowed.example.", dnsmessage.TypeA)

	resp := f.answer(t.Context(), q, func(context.Context, []byte) ([]byte, error) {
		return anAnswer(t, q, nil, anAddr), nil
	})

	if got := rcode(t, resp); got != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want a server failure", got)
	}

	rec := f.rec.freeze(counts{})
	if len(rec.Names) != 0 || !rec.Incomplete {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAListedGuestIsNotGivenAnAnswerThatCannotBeRead(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "allowed.example"), j, nil)
	q := aQuery(t, "allowed.example.", dnsmessage.TypeA)

	resp := f.answer(t.Context(), q, func(context.Context, []byte) ([]byte, error) {
		return anAnswer(t, aQuery(t, "other.example.", dnsmessage.TypeA), nil, anAddr), nil
	})

	if got := rcode(t, resp); got != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want a server failure", got)
	}

	if events := j.all(); len(events) != 0 {
		t.Fatalf("an unread answer admitted %v", events)
	}
}

// AN OPEN GUEST IS ONLY WATCHED: anything it asks goes through unchanged, what cannot
// be read costs the record, and nothing is ever admitted because nothing is judged.
func TestAnOpenGuestIsOnlyWatched(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(Observe(), j, nil)
	q := aQuery(t, "anything.example.", dnsmessage.TypeA)
	answer := anAnswer(t, q, nil, anAddr)

	got := f.answer(t.Context(), q, func(context.Context, []byte) ([]byte, error) { return answer, nil })
	if !slices.Equal(got, answer) {
		t.Fatal("an open guest's answer was changed")
	}

	if events := j.all(); len(events) != 0 {
		t.Fatalf("an open guest's answer did %v", events)
	}

	rec := f.rec.freeze(counts{})
	if !slices.Equal(rec.Names[anAddr], []string{"anything.example"}) || rec.Incomplete {
		t.Fatalf("record = %+v", rec)
	}

	// WHAT IT CANNOT READ IS FORWARDED AS IT WOULD HAVE BEEN, AND THE RECORD SAYS SO.
	weird := rawQuery("a b", "example")
	echoed := f.answer(t.Context(), weird, func(_ context.Context, query []byte) ([]byte, error) { return query, nil })

	if !slices.Equal(echoed, weird) {
		t.Fatal("an open guest's unreadable query was not forwarded")
	}

	if rec := f.rec.freeze(counts{}); !rec.Incomplete {
		t.Fatal("a query the record could not read did not mark it incomplete")
	}
}

// AN UPSTREAM THAT DID NOT ANSWER IS NO ANSWER, as one that lost the query would be:
// the guest's stub retries, which an invented failure would stop it doing.
func TestNoAnswerFromUpstreamIsNoAnswer(t *testing.T) {
	t.Parallel()

	for _, p := range []*Policy{listed(t, "allowed.example"), Observe()} {
		f := aForwarder(p, &journal{}, nil)

		got := f.answer(t.Context(), aQuery(t, "allowed.example.", dnsmessage.TypeA),
			func(context.Context, []byte) ([]byte, error) { return nil, errors.New("timed out") })
		if got != nil {
			t.Errorf("listed %v: answered %x", p.Listed(), got)
		}
	}
}

// fakeResolver answers on one port for both protocols, counting what it heard.
type fakeResolver struct {
	t      *testing.T
	port   uint16
	mu     sync.Mutex
	asked  []string
	answer func(q []byte) []byte
}

func startResolver(t *testing.T) *fakeResolver {
	t.Helper()

	udp, tcp, port, err := listenPair(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}

	r := &fakeResolver{t: t, port: port}
	r.answer = func(q []byte) []byte { return anAnswer(t, q, nil, anAddr) }

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for {
			buf := make([]byte, maxResponse)

			n, from, err := udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			r.heard(buf[:n])

			if _, err := udp.WriteToUDPAddrPort(r.answer(buf[:n]), from); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()

		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}

			query, err := readFrame(conn, maxQuery)
			if err == nil {
				r.heard(query)
				_ = writeFrame(conn, r.answer(query))
			}

			_ = conn.Close()
		}
	}()

	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()

		wg.Wait()
	})

	return r
}

func (r *fakeResolver) heard(q []byte) {
	read, _ := readQuestion(q)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.asked = append(r.asked, read.name)
}

func (r *fakeResolver) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.asked)
}

// servedForwarder is a forwarder on real sockets, in front of r.
func servedForwarder(t *testing.T, r *fakeResolver, p *Policy, j *journal) *forwarder {
	t.Helper()

	f, err := newForwarder(p, newRecorder(p.Listed(), false), netip.MustParseAddr("127.0.0.1"), r.port, slog.Default(),
		func(_ context.Context, addrs []netip.Addr) error {
			j.add("admit %v", addrs)

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	f.serve()
	t.Cleanup(f.stop)

	return f
}

func askUDP(t *testing.T, port uint16, q []byte) []byte {
	t.Helper()

	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(
		netip.MustParseAddr("127.0.0.1"), port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write(q); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, maxResponse)

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no answer over UDP: %v", err)
	}

	return buf[:n]
}

func askTCP(t *testing.T, port uint16, q []byte) []byte {
	t.Helper()

	conn, err := net.DialTCP("tcp4", nil, net.TCPAddrFromAddrPort(netip.AddrPortFrom(
		netip.MustParseAddr("127.0.0.1"), port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := writeFrame(conn, q); err != nil {
		t.Fatal(err)
	}

	resp, err := readFrame(conn, maxResponse)
	if err != nil {
		t.Fatalf("no answer over TCP: %v", err)
	}

	return resp
}

// OVER REAL SOCKETS, BOTH PROTOCOLS: a guest uses TCP for what does not fit UDP, and a
// TCP path that went unwatched would be a way past the list.
func TestAForwarderJudgesBothProtocolsAlike(t *testing.T) {
	t.Parallel()

	r := startResolver(t)
	j := &journal{}
	f := servedForwarder(t, r, listed(t, "allowed.example"), j)

	for name, ask := range map[string]func(*testing.T, uint16, []byte) []byte{"udp": askUDP, "tcp": askTCP} {
		if got := rcode(t, ask(t, f.port, aQuery(t, "allowed.example.", dnsmessage.TypeA))); got != dnsmessage.RCodeSuccess {
			t.Errorf("%s: an allowed name was answered %v", name, got)
		}

		if got := rcode(t, ask(t, f.port, aQuery(t, "off-list.example.", dnsmessage.TypeA))); got != dnsmessage.RCodeRefused {
			t.Errorf("%s: an off-list name was answered %v", name, got)
		}
	}

	if got := r.names(); !slices.Equal(got, []string{"allowed.example", "allowed.example"}) {
		t.Fatalf("the upstream heard %q, and should have heard only the allowed name, once a protocol", got)
	}

	if events := j.all(); len(events) != 2 || events[0] != "admit [192.0.2.10]" {
		t.Fatalf("admits = %q", events)
	}

	rec := f.rec.freeze(counts{})
	if rec.Queries != 4 || rec.RefusedQueries != 2 || rec.Refused[0].Count != 2 {
		t.Fatalf("record = %+v", rec)
	}
}

// A TCP FRAME OVER THE BOUND IS NOT READ, and the connection goes.
func TestAnOversizedTCPQueryIsNotRead(t *testing.T) {
	t.Parallel()

	if _, err := readFrame(strings.NewReader("\xff\xff"), maxQuery); err == nil {
		t.Fatal("a 65535 byte query was read")
	}
}

// STOPPING DOES NOT WAIT ON WHAT THE GUEST HOLDS OPEN: an idle TCP client, and a query
// its upstream will never answer, both end when the resolver does, since a detach
// waits for this on the destroy path.
func TestStoppingAResolverEndsWhatItHolds(t *testing.T) {
	t.Parallel()

	r := startResolver(t)
	f := servedForwarder(t, r, Observe(), &journal{})

	idle, err := net.DialTCP("tcp4", nil, net.TCPAddrFromAddrPort(netip.AddrPortFrom(
		netip.MustParseAddr("127.0.0.1"), f.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	// THE CLIENT IS ACCEPTED BEFORE THE STOP, proved by an answer on the connection.
	if err := writeFrame(idle, aQuery(t, "first.example.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}

	if err := idle.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := readFrame(idle, maxResponse); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		f.stop()
	}()

	select {
	case <-stopped:
	case <-time.After(tcpIdleLimit):
		t.Fatal("stop waited for an idle client's own limit")
	}
}

func TestARecordKeepsWithinItsBounds(t *testing.T) {
	t.Parallel()

	r := newRecorder(true, false)

	for i := range MaxRefusedNames + 5 {
		r.query()
		r.refuse(fmt.Sprintf("n%d.example", i))
	}

	r.answer(answered{names: []string{"a.example", "b.example", "c.example", "d.example", "e.example"},
		addrs: []netip.Addr{anAddr, anAddr}})
	r.answer(answered{names: []string{"a.example"}, addrs: []netip.Addr{anAddr, anotherAddr}})

	rec := r.freeze(counts{})

	if rec.RefusedQueries != MaxRefusedNames+5 || len(rec.Refused) != MaxRefusedNames || !rec.Incomplete {
		t.Fatalf("refused %d, kept %d, incomplete %v", rec.RefusedQueries, len(rec.Refused), rec.Incomplete)
	}

	if got := rec.Names[anAddr]; len(got) != MaxNamesPerAddr || got[0] != "a.example" {
		t.Fatalf("names = %q", got)
	}

	if got := rec.Names[anotherAddr]; !slices.Equal(got, []string{"a.example"}) {
		t.Fatalf("names = %q", got)
	}

	// A KEPT RECORD IS NOT THE LIVE ONE.
	rec.Names[anAddr][0] = "changed"
	if r.freeze(counts{}).Names[anAddr][0] != "a.example" {
		t.Fatal("a frozen record shares its names with the recorder")
	}

	full := newRecorder(false, false)

	for i := range MaxNamedAddrs + 1 {
		full.answer(answered{names: []string{"a.example"}, addrs: []netip.Addr{
			netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}),
		}})
	}

	if rec := full.freeze(counts{}); len(rec.Names) != MaxNamedAddrs || !rec.Incomplete {
		t.Fatalf("kept %d addresses, incomplete %v", len(rec.Names), rec.Incomplete)
	}
}
