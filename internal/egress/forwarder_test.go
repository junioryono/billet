package egress

import (
	"context"
	"encoding/binary"
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

// A LISTED GUEST'S QUERY GOES UPSTREAM AS WHAT THE POLICY READ, through the forwarder and not
// only the function that makes it.
func TestAListedGuestsQueryIsForwardedPlain(t *testing.T) {
	t.Parallel()

	q := aQuery(t, "allowed.example.", dnsmessage.TypeA)

	smuggling := slices.Clone(q)
	binary.BigEndian.PutUint16(smuggling[len(smuggling)-2:], 8)
	smuggling = append(smuggling, "SMUGGLED"...)

	var heard []byte

	f := aForwarder(listed(t, "allowed.example"), &journal{}, nil)

	resp := f.answer(t.Context(), smuggling, func(_ context.Context, query []byte) ([]byte, error) {
		heard = slices.Clone(query)

		return anAnswer(t, q, nil, anAddr), nil
	})

	if got := rcode(t, resp); got != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", got)
	}

	if strings.Contains(string(heard), "SMUGGLED") {
		t.Fatalf("the upstream heard the guest's bytes: %q", heard)
	}
}

// AN ANSWER AN OPEN GUEST'S RECORD CANNOT READ IS STILL ITS ANSWER: only the record goes
// without, since nothing was going to be admitted from it.
func TestAnOpenGuestStillGetsAnAnswerThatCannotBeRead(t *testing.T) {
	t.Parallel()

	f := aForwarder(Observe(), &journal{}, nil)
	q := aQuery(t, "asked.example.", dnsmessage.TypeA)
	other := anAnswer(t, aQuery(t, "other.example.", dnsmessage.TypeA), nil, anAddr)

	got := f.answer(t.Context(), q, func(context.Context, []byte) ([]byte, error) { return other, nil })
	if !slices.Equal(got, other) {
		t.Fatalf("answered %x, want the upstream's answer unchanged", got)
	}

	if rec := f.rec.freeze(counts{}); !rec.Incomplete || len(rec.Names) != 0 {
		t.Fatalf("record = %+v", rec)
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
	t         *testing.T
	port      uint16
	mu        sync.Mutex
	asked     []string
	protocols []string
	answer    func(q []byte) []byte
}

func startResolver(t *testing.T) *fakeResolver {
	t.Helper()

	udp, tcp, port, err := listenPair(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}

	r := &fakeResolver{t: t, port: port}
	r.answer = func(q []byte) []byte {
		answer, err := answerTo(q, nil, anAddr)
		if err != nil {
			t.Errorf("the resolver was asked %x: %v", q, err)
		}

		return answer
	}

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

			r.heard("udp", buf[:n])

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
				r.heard("tcp", query)
				_ = writeFrame(conn, r.answer(query)) //nolint:errcheck // a client that left fails for itself
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

func (r *fakeResolver) heard(protocol string, q []byte) {
	read, _ := readQuestion(q)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.asked, r.protocols = append(r.asked, read.name), append(r.protocols, protocol)
}

func (r *fakeResolver) protocolsHeard() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.protocols)
}

func (r *fakeResolver) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.asked)
}

// servedForwarder is a forwarder on real sockets, in front of r.
func servedForwarder(t *testing.T, r *fakeResolver, p *Policy, j *journal) *forwarder {
	t.Helper()

	f, err := newForwarder(p, newRecorder(p.Listed(), false), netip.MustParsePrefix("127.0.0.1/8"), r.port, slog.Default(),
		func(_ context.Context, addrs []netip.Addr) error {
			j.add("admit %v", addrs)

			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	f.serve(t.Context())
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

	// EACH ASKED OVER ITS OWN PROTOCOL: a large answer needs TCP all the way up.
	if got := r.protocolsHeard(); !slices.Contains(got, "udp") || !slices.Contains(got, "tcp") {
		t.Fatalf("the upstream was asked over %q", got)
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

	frame := "\xff\xff" + strings.Repeat("x", 65535)

	_, err := readFrame(strings.NewReader(frame), maxQuery)
	if err == nil || !strings.Contains(err.Error(), "is over the") {
		t.Fatalf("readFrame = %v, want the size refused", err)
	}
}

// A QUERY FROM OUTSIDE THE GUEST'S SUBNET GOES UNANSWERED: an answer would be the host sending
// the query's bytes wherever the query said it was from.
func TestAQueryFromOutsideTheSubnetIsNotAnswered(t *testing.T) {
	t.Parallel()

	r := startResolver(t)

	outsideOf := func(clients string) *forwarder {
		f, err := newForwarder(Observe(), newRecorder(false, false), netip.MustParsePrefix("127.0.0.1/8"), r.port,
			slog.Default(), func(context.Context, []netip.Addr) error { return nil })
		if err != nil {
			t.Fatal(err)
		}

		// BEFORE IT SERVES: nothing else reads this.
		f.clients = netip.MustParsePrefix(clients)
		f.serve(t.Context())
		t.Cleanup(f.stop)

		return f
	}

	// THE SAME QUERY FROM INSIDE IS ANSWERED, so what follows has something to be the absence of.
	inside := outsideOf("127.0.0.0/8")
	if got := rcode(t, askUDP(t, inside.port, aQuery(t, "inside.example.", dnsmessage.TypeA))); got != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", got)
	}

	outside := outsideOf("10.0.0.0/8")

	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(
		netip.MustParseAddr("127.0.0.1"), outside.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write(aQuery(t, "outside.example.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if n, err := conn.Read(make([]byte, 512)); err == nil {
		t.Fatalf("answered %d bytes to a source outside the subnet", n)
	}

	if slices.Contains(r.names(), "outside.example") {
		t.Fatalf("the upstream heard %q", r.names())
	}
}

// OVER ITS BOUND A RESOLVER DROPS RATHER THAN QUEUES: a query is held for as long as its
// upstream takes, and a guest flooding its own resolver waits on itself, not on the host.
func TestAResolverOverItsBoundDropsAQuery(t *testing.T) {
	t.Parallel()

	r := startResolver(t)
	f := servedForwarder(t, r, Observe(), &journal{})

	for range maxInFlight {
		f.inFlight <- struct{}{}
	}

	t.Cleanup(func() {
		for range maxInFlight {
			<-f.inFlight
		}
	})

	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(
		netip.MustParseAddr("127.0.0.1"), f.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write(aQuery(t, "dropped.example.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if n, err := conn.Read(make([]byte, 512)); err == nil {
		t.Fatalf("answered %d bytes over the bound", n)
	}

	if len(r.names()) != 0 {
		t.Fatalf("the upstream heard %q", r.names())
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

	// BELOW THE LIMIT IT WOULD OTHERWISE FALL BACK TO, or a stop that waits it out passes.
	select {
	case <-stopped:
	case <-time.After(tcpIdleLimit / 2):
		t.Fatal("stop waited for an idle client's own limit")
	}
}

// NOR ON AN UPSTREAM THAT WILL NEVER ANSWER, over either protocol: stop cancels the question.
func TestStoppingAResolverEndsAQuestionInFlight(t *testing.T) {
	t.Parallel()

	for name, ask := range map[string]func(*forwarder) exchange{
		"udp": func(f *forwarder) exchange { return f.exchangeUDP },
		"tcp": func(f *forwarder) exchange { return f.exchangeTCP },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			udp, tcp, port, err := listenPair(netip.MustParseAddr("127.0.0.1"))
			if err != nil {
				t.Fatal(err)
			}

			// BOUND AND SILENT: a TCP connect succeeds, and nothing ever comes back. WHAT ARRIVES
			// IS SIGNALLED, so the cancel lands on a question in flight, not on a dial it stops.
			t.Cleanup(func() {
				_ = udp.Close()
				_ = tcp.Close()
			})

			heard := make(chan struct{}, 2)

			go func() {
				if _, _, err := udp.ReadFromUDPAddrPort(make([]byte, 512)); err == nil {
					heard <- struct{}{}
				}
			}()

			go func() {
				conn, err := tcp.Accept()
				if err != nil {
					return
				}

				t.Cleanup(func() { _ = conn.Close() })

				if _, err := readFrame(conn, maxQuery); err == nil {
					heard <- struct{}{}
				}
			}()

			f, err := newForwarder(Observe(), newRecorder(false, false), netip.MustParsePrefix("127.0.0.1/8"), port,
				slog.Default(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer f.stop()

			ctx, cancel := context.WithCancel(t.Context())
			returned := make(chan error, 1)

			go func() {
				_, err := ask(f)(ctx, aQuery(t, "silent.example.", dnsmessage.TypeA))
				returned <- err
			}()

			select {
			case <-heard:
			case <-time.After(5 * time.Second):
				t.Fatal("the question never reached the upstream")
			}

			select {
			case err := <-returned:
				t.Fatalf("a question nobody answered returned before it was cancelled: %v", err)
			default:
			}

			cancel()

			select {
			case err := <-returned:
				if err == nil {
					t.Fatal("a question nobody answered returned an answer")
				}
			case <-time.After(upstreamLimit / 2):
				t.Fatal("a cancelled question waited out its upstream")
			}
		})
	}
}

// A STUB ASKS FOR WHERE AN ALLOWED NAME'S CHAIN LED, measured from a real guest on 2026-11-04. Refusing it keeps out nothing, since the chain's addresses are
// admitted already, and shows an operator a refusal of a name their list allowed. Only a name an
// allowed answer named follows: asked first, or asked of another chain, it is off the list.
func TestAStubMayFollowWhereAnAllowedNameLed(t *testing.T) {
	t.Parallel()

	j := &journal{}
	f := aForwarder(listed(t, "example.com"), j, nil)

	ask := func(_ context.Context, q []byte) ([]byte, error) {
		return anAnswer(t, q, []string{"glb.cdn.test."}, anAddr), nil
	}
	target := aQuery(t, "glb.cdn.test.", dnsmessage.TypeAAAA)

	if got := rcode(t, f.answer(t.Context(), target, ask)); got != dnsmessage.RCodeRefused {
		t.Fatalf("before any answer named it: rcode = %v", got)
	}

	if got := rcode(t, f.answer(t.Context(), aQuery(t, "example.com.", dnsmessage.TypeA), ask)); got != dnsmessage.RCodeSuccess {
		t.Fatalf("the allowed name: rcode = %v", got)
	}

	if got := rcode(t, f.answer(t.Context(), target, ask)); got != dnsmessage.RCodeSuccess {
		t.Fatalf("where it led: rcode = %v", got)
	}

	if got := rcode(t, f.answer(t.Context(), aQuery(t, "other.test.", dnsmessage.TypeA), ask)); got != dnsmessage.RCodeRefused {
		t.Fatalf("a name no answer named: rcode = %v", got)
	}

	// AN ANSWER TO A NAME OFF THE LIST NAMES NOTHING: it was never asked.
	refused := aQuery(t, "off.test.", dnsmessage.TypeA)
	f.answer(t.Context(), refused, func(_ context.Context, q []byte) ([]byte, error) {
		return anAnswer(t, q, []string{"beyond.test."}, anAddr), nil
	})

	if got := rcode(t, f.answer(t.Context(), aQuery(t, "beyond.test.", dnsmessage.TypeA), ask)); got != dnsmessage.RCodeRefused {
		t.Fatalf("a chain from a refused name was followed: rcode = %v", got)
	}
}

// AN OPEN GUEST'S ANSWERS FOLLOW NOTHING, AND PAST ITS BOUND A LIST'S STOP: an allowed name's owner
// must not be able to grow what the node holds for a guest without limit.
func TestWhatAGuestMayFollowIsBounded(t *testing.T) {
	t.Parallel()

	f := aForwarder(listed(t, "example.com"), &journal{}, nil)

	names := make([]string, 0, maxFollowed+1)
	for i := range maxFollowed + 1 {
		names = append(names, fmt.Sprintf("n%d.cdn.test", i))
	}

	f.follow(names)

	if len(f.followed) != maxFollowed {
		t.Fatalf("followed %d, want the bound %d", len(f.followed), maxFollowed)
	}

	if f.isFollowed(names[maxFollowed]) {
		t.Fatal("a name past the bound was followed")
	}

	f.follow([]string{"example.com"})

	if f.isFollowed("example.com") {
		t.Fatal("an allowed name took up a place")
	}

	open := aForwarder(Observe(), &journal{}, nil)
	open.answer(t.Context(), aQuery(t, "example.com.", dnsmessage.TypeA), func(_ context.Context, q []byte) ([]byte, error) {
		return anAnswer(t, q, []string{"glb.cdn.test."}, anAddr), nil
	})

	if open.isFollowed("glb.cdn.test") {
		t.Fatal("an open guest's answer was kept for nothing")
	}
}

// A QUESTION HAS A LIMIT OF ITS OWN, whatever the resolver's life is: past it the guest gets
// what a lost query gets.
func TestAQuestionsWaitIsBounded(t *testing.T) {
	t.Parallel()

	f := aForwarder(Observe(), &journal{}, nil)

	var deadline time.Time

	got := f.answer(t.Context(), aQuery(t, "slow.example.", dnsmessage.TypeA),
		func(ctx context.Context, _ []byte) ([]byte, error) {
			deadline, _ = ctx.Deadline()

			return nil, errors.New("timed out")
		})
	if got != nil {
		t.Fatalf("answered %x", got)
	}

	if deadline.IsZero() || time.Until(deadline) > upstreamLimit {
		t.Fatalf("the question's deadline is %v", deadline)
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

	if rec.Refused[0].Name != "n0.example" {
		t.Fatalf("refused = %+v", rec.Refused[:2])
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

// WHAT IS REFUSED MOST COMES FIRST, so the report's sixty-four are the ones that matter.
func TestARecordListsWhatWasRefusedMostFirst(t *testing.T) {
	t.Parallel()

	r := newRecorder(true, false)

	for _, name := range []string{"once.example", "thrice.example", "thrice.example", "thrice.example", "a.example"} {
		r.refuse(name)
	}

	got := r.freeze(counts{}).Refused
	want := []Refused{{"thrice.example", 3}, {"a.example", 1}, {"once.example", 1}}

	if !slices.Equal(got, want) {
		t.Fatalf("refused = %+v, want %+v", got, want)
	}
}
