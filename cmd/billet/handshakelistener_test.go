package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

// testHandshakeTimeout is the bound these tests give a handshake.
//
// SHORT ON PURPOSE. Several of them have to outwait it to observe anything, and
// at the production five seconds that is tens of seconds of sleeping added to a
// package whose ceph preflight tests already fail under a loaded -race run. Long
// enough that a local TLS handshake is never the thing that misses it.
const testHandshakeTimeout = 300 * time.Millisecond

// nodeClient is a client holding a certificate this deployment issued.
//
// ONE NAME FOR EVERY CALLER, because none of these tests is about WHICH node is
// connecting — they are about how many connections the wire will hold and who
// may spend that budget. What the common name decides has its own tests.
func nodeClient(t *testing.T, ca *wirecert.CA, timeout time.Duration) *http.Client {
	t.Helper()

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: conf, MaxIdleConnsPerHost: 1},
	}
}

// reachesTheWire reports whether a client can get an answer out of the node wire.
//
// ANY ANSWER COUNTS, and 401 is excluded on purpose. The plane in these tests
// declares no tiers, so a registration is refused on its merits — what is being
// asked is whether the connection was ADMITTED and the handler ran, not whether
// it liked the request. A 401 would mean the certificate did not arrive, which is
// a different failure and never the one under test.
func reachesTheWire(t *testing.T, client *http.Client, addr string) bool {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+addr+"/v1/register", strings.NewReader(`{"node":"epyc-1"}`))
	if err != nil {
		t.Errorf("build: %v", err)

		return false
	}

	res, err := client.Do(req)
	if err != nil {
		return false
	}

	defer res.Body.Close()

	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return false
	}

	if res.StatusCode == http.StatusUnauthorized {
		t.Errorf("the node wire told a certificate holder it had not authenticated: %s", res.Status)
	}

	return true
}

// silentConnections opens raw TCP connections and sends nothing on them.
//
// THE CHEAPEST ATTACK THERE IS: no TLS, no HTTP, no certificate — just a socket.
// Before the handshaking acceptor these took the fleet's own connection budget,
// because the permit was charged in Accept, before anybody had presented
// anything.
func silentConnections(t *testing.T, addr string, n int) {
	t.Helper()

	var dialer net.Dialer

	for range n {
		conn, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}

		t.Cleanup(func() { _ = conn.Close() })
	}
}

// SILENT SOCKETS CANNOT KEEP A NODE OFF THE WIRE. This is the half of the
// shared-budget attack that splitting the listeners did not fix, and the reason
// this acceptor exists.
//
// The connection permit used to be taken in Accept, BEFORE the underlying accept
// and so before the handshake — which cannot tell an enrolled node from a
// stranger, because at that point nobody has presented anything. Opening sockets
// and sending no TLS bytes therefore spent the fleet's budget, and while it was
// full Accept blocked ahead of the kernel accept, so a node's connection sat in
// the backlog until its own dial timeout. The fleet was offline and every process
// involved was behaving correctly.
//
// MORE SILENT SOCKETS THAN THE BUDGET, deliberately: under the old shape this
// exact setup wedges the listener for the whole handshake timeout.
func TestSilentConnectionsCannotKeepANodeOffTheWire(t *testing.T) {
	t.Parallel()

	const budget = 4

	wire, _, ca := splitWire(t, "", withConnectionLimits(budget, budget),
		withHandshakeTimeout(testHandshakeTimeout))

	silentConnections(t, wire, budget*2)

	// SHORTER THAN THE HANDSHAKE BOUND, AND THE ELAPSED TIME IS ASSERTED. A
	// generous budget here would let a broken implementation WAIT OUT the silent
	// connections — they age out after the handshake bound — and then be served,
	// which passes while proving the opposite of what the name claims. The
	// question is whether a node gets in WHILE they are there.
	const patience = testHandshakeTimeout / 2

	start := time.Now()

	if !reachesTheWire(t, nodeClient(t, ca, patience), wire) {
		t.Fatal("a node could not reach the wire while sockets that had proved nothing were " +
			"open against it, which is the failure this acceptor exists to prevent")
	}

	if waited := time.Since(start); waited >= testHandshakeTimeout {
		t.Errorf("the node was served only after %s, which is past the point where the silent "+
			"sockets expire — it waited them out rather than being unaffected by them", waited)
	}
}

// THE BUDGET IS REAL, AND IT HOLDS ONLY WHAT AUTHENTICATED.
//
// Without this the test above passes against an acceptor that has no budget at
// all — which would bound nothing and leak a goroutine and a socket per caller.
// The other direction matters too: a permit has to come BACK, or a control plane
// stops admitting nodes after its first few connections have come and gone.
func TestTheNodeWireBudgetHoldsOnlyAuthenticatedConnections(t *testing.T) {
	t.Parallel()

	const budget = 2

	wire, _, ca := splitWire(t, "", withConnectionLimits(budget, budget))

	// HELD BY SEPARATE CLIENTS, because one transport reuses a single connection
	// and would occupy one slot however many requests it made.
	holders := make([]*http.Client, budget)

	for i := range holders {
		holders[i] = nodeClient(t, ca, 10*time.Second)

		if !reachesTheWire(t, holders[i], wire) {
			t.Fatalf("authenticated connection %d of %d was refused", i+1, budget)
		}
	}

	// ONE PAST THE BOUND. Its handshake succeeds — it is a real node certificate —
	// and it is refused after that, which is the budget doing its job.
	if reachesTheWire(t, nodeClient(t, ca, 5*time.Second), wire) {
		t.Error("a connection beyond the node wire's budget was admitted, so the budget " +
			"bounds nothing")
	}

	// AND THE PERMIT COMES BACK. Closing a holder's idle connection releases it,
	// so the caller that was just refused now fits.
	holders[0].CloseIdleConnections()

	// The release happens on Close, which the transport performs as it drops the
	// connection; retry briefly rather than assuming it has already landed.
	admitted := false

	for range 50 {
		if reachesTheWire(t, nodeClient(t, ca, 5*time.Second), wire) {
			admitted = true

			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if !admitted {
		t.Error("a closed connection did not return its place to the budget, so a control " +
			"plane stops admitting nodes once enough have come and gone")
	}
}

// A HANDSHAKE THAT FAILED NEVER HELD A PLACE, AND MUST NOT RETURN ONE.
//
// The permit is taken after the handshake, so a refused connection holds none.
// If its wrapper releases anyway it takes back somebody ELSE's place — the
// budget's count drifts down while the connections it was counting are still
// live, and the wire ends up admitting more than its limit.
// `admittedConn.charged` is what distinguishes the two.
//
// THE ORDER HERE IS THE WHOLE TEST, and the first version of it got that wrong.
// Running the failed handshakes first proves nothing: the budget is empty at
// that point, so a spurious release finds nothing to take, and the mutation that
// deletes `charged` SURVIVED. A permit has to be HELD while the failures happen
// for the theft to be observable at all.
func TestAFailedHandshakeReturnsNoPlaceToTheBudget(t *testing.T) {
	t.Parallel()

	const budget = 2

	wire, _, ca := splitWire(t, "", withConnectionLimits(budget, budget))

	// ONE PLACE TAKEN AND HELD, so there is something for a wrongly-released
	// permit to steal.
	holder := nodeClient(t, ca, 10*time.Second)
	if !reachesTheWire(t, holder, wire) {
		t.Fatal("the first authenticated connection was refused")
	}

	// Refused in the handshake: the node wire requires a client certificate and
	// these have none. Under the defect each of these takes the holder's place.
	for range budget * 4 {
		if readAuthority(t, anonymousClient(5*time.Second), wire) {
			t.Fatal("a caller with no certificate was served by the node wire")
		}
	}

	// The holder still occupies one place, so exactly one more fits.
	second := nodeClient(t, ca, 10*time.Second)
	if !reachesTheWire(t, second, wire) {
		t.Fatal("the second authenticated connection was refused, so a failed handshake " +
			"consumed a place it never held")
	}

	// AND NO MORE. If the failures above handed the budget back places they never
	// took, this one is admitted and the limit means nothing.
	if reachesTheWire(t, nodeClient(t, ca, 5*time.Second), wire) {
		t.Error("the wire admitted more than its budget after some handshakes failed, so a " +
			"refused connection returned a permit it never took")
	}
}

// readWithin reads n bytes, giving up after d — WITHOUT touching the
// connection's own deadlines, which are what its callers are testing.
func readWithin(conn net.Conn, n int, d time.Duration) ([]byte, error) {
	type result struct {
		buf []byte
		err error
	}

	done := make(chan result, 1)

	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(conn, buf)
		done <- result{buf: buf, err: err}
	}()

	select {
	case r := <-done:
		return r.buf, r.err
	case <-time.After(d):
		return nil, errors.New("nothing arrived")
	}
}

// THE HANDSHAKE DEADLINE MUST NOT SURVIVE ONTO THE CONNECTION.
//
// The handshake is bounded by a deadline set on the raw connection, and a
// command poll on this wire is a LONG poll — the node wire has no WriteTimeout
// precisely so a response may be held for as long as GitHub keeps billet
// waiting. A deadline left set would cut a healthy poll five seconds in, and the
// node would see a broken connection rather than a slow one. Every job billet
// schedules goes through that path.
//
// ASSERTED ON THE CONNECTION ITSELF, with no http.Server in the way, and that is
// what makes it a test of this listener. MEASURED: http.Server restores
// conn-level deadlines after its own (already-completed) handshake whenever its
// derived tlsHandshakeTimeout is positive — so putting any server with a
// ReadHeaderTimeout in front clears the deadline as a side effect, deleting the
// clear in handshake() survives, and the test proves nothing about this code.
func TestAnAdmittedConnectionKeepsNoHandshakeDeadline(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	// NoClientCert, because what is under test is the deadline rather than who
	// may connect.
	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatalf("bootstrap tls: %v", err)
	}

	var lc net.ListenConfig

	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ln := newHandshakingListener(t.Context(), inner, conf, 4,
		handshakeBounds{handshakeFor: testHandshakeTimeout}, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = ln.Close() })

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	dialed := make(chan net.Conn, 1)

	go func() {
		dialer := tls.Dialer{
			Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
		}

		conn, err := dialer.DialContext(t.Context(), "tcp", ln.Addr().String())
		if err != nil {
			close(dialed)

			return
		}

		dialed <- conn
	}()

	// BOUNDED, because a regression in the handover would otherwise hang this
	// test rather than fail it, and a hang is not a diagnosis.
	accepted := make(chan net.Conn, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)

			return
		}

		accepted <- conn
	}()

	var server net.Conn

	select {
	case conn, ok := <-accepted:
		if !ok {
			t.Fatal("the listener refused the connection")
		}

		server = conn
	case <-time.After(10 * time.Second):
		t.Fatal("the listener never handed the connection over")
	}

	defer server.Close()

	var client net.Conn

	select {
	case conn, ok := <-dialed:
		if !ok {
			t.Fatal("the client could not complete a handshake")
		}

		client = conn
	case <-time.After(10 * time.Second):
		t.Fatal("the client never finished its handshake")
	}

	defer client.Close()

	// PAST THE POINT THE HANDSHAKE DEADLINE WOULD HAVE FIRED. A connection that
	// kept it is now unusable.
	time.Sleep(2 * testHandshakeTimeout)

	// BOTH DIRECTIONS, because SetDeadline sets two and clearing only one would
	// pass a test that checks only the other. The read deadline is the one that
	// matters most here: it is what a request arriving after the bound would hit.
	for _, d := range []struct {
		name     string
		from, to net.Conn
		payload  string
	}{
		{name: "server to client", from: server, to: client, payload: "poll answer\n"},
		{name: "client to server", from: client, to: server, payload: "poll request\n"},
	} {
		if _, err := d.from.Write([]byte(d.payload)); err != nil {
			t.Fatalf("%s: writing %s after the connection was accepted failed, so a deadline "+
				"outlived the handshake and every long poll on this wire would break: %v",
				d.name, 2*testHandshakeTimeout, err)
		}

		// THE READ IS BOUNDED BY A TIMER, NOT BY A DEADLINE ON THE CONNECTION.
		// Setting one here was the first version and it clobbered the very thing
		// under test: a stale read deadline left over from the handshake is erased
		// by SetReadDeadline, so the mutation that clears only the WRITE deadline
		// passed. Measured — it survived until this was fixed.
		got, err := readWithin(d.to, len(d.payload), 10*time.Second)
		if err != nil {
			t.Fatalf("%s: the far side did not receive what was sent, so its read deadline "+
				"outlived the handshake: %v", d.name, err)
		}

		if string(got) != d.payload {
			t.Errorf("%s: read %q, want %q", d.name, got, d.payload)
		}
	}
}

// flakyListener fails its first n accepts the way a host out of descriptors
// does, then behaves.
type flakyListener struct {
	net.Listener

	err       error
	remaining atomic.Int64
}

// temporaryError is what a resource failure looks like to net/http: a net.Error
// that reports itself as temporary, which is the class Serve retries.
type temporaryError struct{ error }

func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.remaining.Add(-1) >= 0 {
		return nil, l.err
	}

	return l.Listener.Accept()
}

// A TRANSIENT ACCEPT FAILURE MUST NOT END THE WIRE.
//
// This is the shape of regression that a fix for a denial of service most easily
// introduces. The listener this replaced delegated to the inner Accept on every
// call, so http.Server.Serve retrying a temporary error — which it does, with
// backoff — re-entered the accept path and recovered. A single accept goroutine
// that returns on the first error does not: Serve retries, receives the same
// stored error forever, and the wire never admits another connection while the
// process sits there looking perfectly healthy. Running out of file descriptors
// is the ordinary way in, and it is exactly the moment a fleet must not vanish.
func TestATransientAcceptFailureDoesNotEndTheWire(t *testing.T) {
	t.Parallel()

	// BOTH KINDS, because the contract is broader than the old one. http.Server
	// retried only errors reporting themselves TEMPORARY, and a regression that
	// kept just that test would pass against the first case alone — while an
	// ordinary error, which is most of what a listener can return, still ended the
	// wire forever.
	for name, failure := range map[string]error{
		"temporary": temporaryError{errors.New("accept: too many open files")},
		"ordinary":  errors.New("accept: something else entirely"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transientAcceptFailure(t, failure)
		})
	}
}

func transientAcceptFailure(t *testing.T, failure error) {
	t.Helper()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatalf("bootstrap tls: %v", err)
	}

	var lc net.ListenConfig

	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	flaky := &flakyListener{Listener: inner, err: failure}
	flaky.remaining.Store(3)

	ln := newHandshakingListener(t.Context(), flaky, conf, 4,
		handshakeBounds{handshakeFor: testHandshakeTimeout}, slog.New(slog.DiscardHandler))

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	t.Cleanup(func() {
		_ = srv.Close()
		// JOINED, so the serving goroutine does not outlive its test — and its
		// error is read rather than discarded, which is what says the server
		// stopped because this test closed it.
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, net.ErrClosed) {
			t.Errorf("the server stopped for an unexpected reason: %v", err)
		}
	})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
		},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://"+ln.Addr().String(), http.NoBody)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("the wire never served another connection after a transient accept "+
			"failure, so a host briefly out of descriptors loses its fleet until it is "+
			"restarted: %v", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Errorf("answered %s, want 204", res.Status)
	}

	// AND THE FAILURES WERE REAL. Without this the test passes against a fake
	// listener that never failed at all.
	if remaining := flaky.remaining.Load(); remaining >= 0 {
		t.Errorf("the listener refused %d accepts, so nothing transient was exercised",
			3-remaining)
	}
}

// A FLOOD IS COUNTED AND SAID OUT LOUD.
//
// Refusing before a handshake is the branch that fires under exactly the attack
// this listener exists to survive, and it began life silent: an operator whose
// nodes were being turned away would have seen a healthy control plane and no
// evidence at all, which is the same silence the shared budget was reported for.
// The two counts are reported together because they mean opposite things — load
// in front of the port, versus a fleet larger than its budget.
func TestRefusedConnectionsAreCountedAndReported(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatalf("bootstrap tls: %v", err)
	}

	var lc net.ListenConfig

	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	logged := &countingHandler{wrote: make(chan struct{}, 8)}

	// A TINY PRE-HANDSHAKE BOUND, so a flood is a handful of sockets rather than a
	// thousand opened faster than they expire.
	const pending = 4

	ln := newHandshakingListener(t.Context(), inner, conf, 4,
		handshakeBounds{pending: pending, handshakeFor: testHandshakeTimeout}, slog.New(logged))
	t.Cleanup(func() { _ = ln.Close() })

	// FILL THE PRE-HANDSHAKE CAPACITY, which is what a flood does. Nothing here
	// completes a handshake, so nothing reaches the admitted budget.
	var dialer net.Dialer

	for range pending * 8 {
		conn, err := dialer.DialContext(t.Context(), "tcp", ln.Addr().String())
		if err != nil {
			// The kernel backlog is finite and this is deliberately past it; what
			// matters is that enough got through to fill the workers.
			break
		}

		t.Cleanup(func() { _ = conn.Close() })
	}

	// The accept loop and the refusals happen on their own goroutine, so wait for
	// the count rather than assuming it has landed.
	refused := false

	for range 100 {
		if ln.refusedPending.Load() > 0 {
			refused = true

			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if !refused {
		t.Fatalf("no connection was refused before a handshake after %d dials, so either the "+
			"pre-handshake bound is not being reached or it is not being counted",
			pending*8)
	}

	// AND IT REACHED A LOG. A counter nobody reports is not evidence an operator
	// can act on, which is the whole point of this test.
	//
	// WAITED FOR RATHER THAN READ. The report is written on its own goroutine so
	// that a blocked log sink cannot hold up the listener, which means checking a
	// counter the instant the refusal lands is a race that fails under load.
	select {
	case <-logged.wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("connections were refused and nothing was logged, so an operator sees a " +
			"healthy control plane while its nodes are being turned away")
	}

	// AND EXACTLY ONE LINE FOR THE BURST. The throttle allows one per interval,
	// and this test does not run for one — so a second line means the throttle is
	// not throttling, and the report is itself the amplification it was added to
	// prevent.
	select {
	case <-logged.wrote:
		t.Errorf("a second warning was written for one burst of refusals; the report is "+
			"throttled to one per %s", reportEvery)
	case <-time.After(500 * time.Millisecond):
	}
}

// countingHandler reports each warning on a channel, without writing anywhere.
//
// A CHANNEL RATHER THAN A COUNTER, because the thing it observes is written on a
// goroutine of its own: a test that reads a count immediately after causing the
// condition is asserting against a schedule rather than a behaviour.
type countingHandler struct {
	slog.Handler

	wrote chan struct{}
}

func (h *countingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}

	// Non-blocking, so a handler that nobody is reading cannot become the very
	// stuck log sink these tests are about.
	select {
	case h.wrote <- struct{}{}:
	default:
	}

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// THE THROTTLE IS WHAT STOPS A CALLER CHOOSING HOW MUCH BILLET WRITES TO DISK.
//
// Both things it guards — refusals and failed accepts — are conditions somebody
// else causes at whatever rate they like, so getting this wrong turns a
// diagnostic into the amplification it was added to prevent. Its CAS is the
// subtle part: a burst of goroutines all read the same stale timestamp, and
// exactly one of them must win.
func TestTheReportThrottleAllowsOnePerInterval(t *testing.T) {
	t.Parallel()

	var tr throttle

	// THE FIRST ONE ALWAYS PASSES, because the stored value is a SENTINEL rather
	// than an age: zero means nobody has reported. Elapsed time starts at
	// approximately zero, so an age comparison alone would suppress exactly the
	// report that matters most. The first refusal should be visible at once, and
	// only the repetition is worth suppressing.
	if !tr.allow(0) {
		t.Fatal("the first report was suppressed, so a condition that happens once is never " +
			"seen at all")
	}

	if tr.allow(reportEvery - time.Millisecond) {
		t.Error("a second report inside the interval was allowed")
	}

	if !tr.allow(reportEvery) {
		t.Error("a report after the interval was suppressed")
	}

	// AND A BURST ON A FRESH THROTTLE YIELDS EXACTLY ONE. This one IS decidable,
	// because the first report is claimed by a single compare-and-swap against
	// the sentinel rather than by two separate operations on two fields.
	var (
		fresh   throttle
		allowed atomic.Int64
		racing  sync.WaitGroup
	)

	for range 64 {
		racing.Add(1)

		go func() {
			defer racing.Done()

			if fresh.allow(0) {
				allowed.Add(1)
			}
		}()
	}

	racing.Wait()

	if n := allowed.Load(); n != 1 {
		t.Errorf("%d of 64 concurrent first reports were allowed, want exactly 1", n)
	}

	// AND AT A NON-ZERO ELAPSED TIME, which exercises the other branch: with
	// since==0 every losing caller fails the interval check for an unrelated
	// reason, so that case says nothing about elapsed times a listener actually
	// sees. The first refusal usually happens well after the listener started.
	//
	// WHAT NEITHER CASE DISTINGUISHES is the compare-and-swap from a plain store
	// — MEASURED, replacing it leaves both green, because a caller that reads
	// after somebody else has stored is denied by the interval check anyway and
	// the window between load and store is too narrow to hit on demand. They do
	// discriminate: breaking the interval check itself fails them both. What the
	// single-word encoding buys is structural rather than tested — "never
	// reported" and "reported at T" used to be two fields, and a caller that had
	// claimed the first report without yet writing its timestamp left the rest
	// reading "already reported, at time zero", which passes any interval check.
	// One word cannot be read half-written.
	var (
		later    throttle
		allowed2 atomic.Int64
		racing2  sync.WaitGroup
	)

	for range 64 {
		racing2.Add(1)

		go func() {
			defer racing2.Done()

			if later.allow(reportEvery * 3) {
				allowed2.Add(1)
			}
		}()
	}

	racing2.Wait()

	if n := allowed2.Load(); n != 1 {
		t.Errorf("%d of 64 concurrent first reports at elapsed %s were allowed, want exactly 1",
			n, reportEvery*3)
	}

	// WHAT THIS DOES NOT PROVE, said out loud rather than left to be assumed. For
	// reports AFTER the first, allow compares an elapsed time and swaps it, and a
	// burst of goroutines that all read the same stale value should yield one
	// report rather than one each. That part is not deterministically testable:
	// MEASURED, replacing that second CAS with a plain Store leaves this package
	// green, because goroutines starting after an earlier store observe a fresh
	// elapsed time and are denied by the interval check anyway. So it narrows a
	// window rather than closing a behaviour a test can pin, and the assertions
	// here are what is actually decidable.
}

// A LISTENER THAT HAS ENDED HANDS NOTHING OVER, and returns what it was holding.
//
// The reliable half of the property: after the listener ends, Accept reports an
// error rather than a connection, and a connection that was mid-handover has its
// place in the admitted budget returned rather than stranded. A shutdown that
// leaked a permit per lost race would shrink the fleet's capacity every restart.
//
// WHAT THIS DOES NOT REACH, and it is the reason the `closing` flag exists.
// A worker whose handshake finished at the moment of shutdown is parked offering
// its connection, and Go picks at RANDOM between two ready cases — so if Accept
// enters its select in the window after close() has marked that worker runnable
// but before it has actually run and withdrawn its offer, Accept can select the
// CONNECTION rather than the closure. http.Server.Serve then starts a handler,
// because it re-checks shutdown only when Accept returns an error. `closing` is
// what Accept re-asks, since which case fired cannot answer it.
//
// MEASURED: that window could not be forced. Deleting the flag leaves this test
// green over sixty iterations, using end() rather than Close() so no syscall
// widens the gap — the worker wakes and takes the closure every time. So the
// flag guards a real interleaving that a test cannot produce on demand, and the
// assertions below are what is actually decidable. Saying so is the point: an
// iteration count that reads as a probabilistic proof would be claiming coverage
// this does not have.
func TestNoConnectionIsHandedOverAfterClose(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatalf("bootstrap tls: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	for i := range 8 {
		var lc net.ListenConfig

		inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}

		ln := newHandshakingListener(t.Context(), inner, conf, 4,
			handshakeBounds{handshakeFor: testHandshakeTimeout}, slog.New(slog.DiscardHandler))

		// CLOSED FOR REAL, WHATEVER THE TEST DOES BELOW. end() only ends the
		// wrapper: it does not touch `inner`, so the accept goroutine stays parked
		// in inner.Accept() and the socket stays open. Without this every
		// iteration leaked a listening socket and a goroutine, which is a test
		// that measures nothing and costs the suite descriptors.
		t.Cleanup(func() { _ = ln.Close() })

		// NOBODY CALLS Accept YET, so a connection that verifies is left offering
		// itself — which is the state this test needs and the only one in which
		// the race exists.
		dialed := make(chan net.Conn, 1)

		go func() {
			dialer := tls.Dialer{
				Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
			}

			conn, err := dialer.DialContext(t.Context(), "tcp", ln.Addr().String())
			if err != nil {
				close(dialed)

				return
			}

			dialed <- conn
		}()

		// THE PERMIT IS THE CLOSEST SIGNAL THERE IS, and it is not proof: it is
		// taken immediately BEFORE the handover, so seeing it means a worker is
		// about to offer a connection rather than that it already is. Nothing
		// observable sits between the two, which is part of why the window below
		// cannot be forced.
		charged := false

		for range 200 {
			if len(ln.admitted) == 1 {
				charged = true

				break
			}

			time.Sleep(10 * time.Millisecond)
		}

		if !charged {
			t.Fatalf("iteration %d: no connection reached the handover", i)
		}

		// end() RATHER THAN Close(), with nothing between it and Accept: Close also
		// shuts the underlying socket, and that syscall widens the gap further
		// still. It does not make the race reachable — see above — but it is the
		// closest this can get to it.
		ln.end()

		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
			t.Fatalf("iteration %d: Accept returned a connection after Close, so a handler "+
				"starts inside the window meant to be finishing the old ones", i)
		}

		// AND ITS PLACE CAME BACK. Closing a connection Accept declined must
		// release the permit it was charged, or a shutdown leaks one every time
		// this race is lost.
		returned := false

		for range 200 {
			if len(ln.admitted) == 0 {
				returned = true

				break
			}

			time.Sleep(10 * time.Millisecond)
		}

		if !returned {
			t.Fatalf("iteration %d: the declined connection kept its place in the budget", i)
		}

		// BOUNDED, so a regression in the handover fails this test rather than
		// hanging it.
		select {
		case client, ok := <-dialed:
			if ok {
				_ = client.Close()
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: the client never finished its handshake", i)
		}
	}
}
