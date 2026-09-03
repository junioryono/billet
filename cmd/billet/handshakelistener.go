package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// handshakeTimeout bounds how long one connection may take to prove itself.
//
// SET HERE RATHER THAN DERIVED. On a plain limitedListener the bound comes from
// Go's Server.tlsHandshakeTimeout, which is the minimum of the POSITIVE
// ReadHeaderTimeout, ReadTimeout and WriteTimeout — so the node wire's
// deliberately absent WriteTimeout leaves it at ten seconds and zeroing the other
// two would make it unlimited. A number that decides how expensive an attack is
// should not be an emergent property of three unrelated settings.
//
// Five seconds is many times what a TLS 1.3 handshake takes across the internet,
// and it is the divisor in the cost of the attack below: an attacker must sustain
// maxPendingHandshakes / handshakeTimeout connections a second to keep the
// pre-authentication capacity full.
const handshakeTimeout = 5 * time.Second

// maxPendingHandshakes bounds connections that have not completed a handshake.
//
// SEPARATE FROM THE ADMITTED BUDGET, AND THAT SEPARATION IS THE POINT. Anyone
// who can open a socket occupies these; only a completed handshake occupies the
// other. What that is worth depends on the listener, and the distinction is
// worth stating rather than blurring: on the node wire the handshake requires a
// certificate this deployment signed, so the admitted budget really is the
// fleet's alone — while the enrollment listener asks for no certificate, so its
// admitted budget is anonymous by design and the separation buys graceful
// refusal rather than exclusion.
//
// Generous, because a handshake is short and the whole cost of holding one of
// these is a goroutine and a socket for at most handshakeTimeout — where the
// admitted budget is held for as long as a caller keeps its connection.
const maxPendingHandshakes = 1024

// handshakingListener admits a connection to the server's budget only once its
// TLS handshake has succeeded.
//
// THE BUDGET USED TO BE CHARGED BEFORE THE HANDSHAKE, and that is what moving
// routes to a second listener could not fix. A permit taken in Accept, before
// the underlying accept, cannot distinguish an enrolled node from a stranger —
// nobody has presented anything yet — so the fleet's capacity was spendable by
// anyone who could open a socket. Sending no TLS bytes at all held a permit for
// the handshake timeout, and while the budget was full Accept blocked BEFORE the
// kernel accept, so a healthy node's connection sat in the backlog until its own
// dial timeout fired. The fleet was down with nothing in the process misbehaving.
//
// So the accept loop here NEVER BLOCKS ON A PERMIT. It accepts unconditionally,
// hands each connection to a bounded set of handshake workers, and charges the
// server's budget only for what verifies. Two consequences worth being precise
// about, because the difference between them is the whole guarantee:
//
//   - An admitted connection is UNTOUCHABLE by traffic that has not completed a
//     handshake. Nothing pre-handshake holds any of that budget, so on the node
//     wire — where completing a handshake means presenting a certificate this
//     deployment signed — a connected node is never displaced by callers who
//     cannot authenticate. On the enrollment listener, which asks for no
//     certificate, the same mechanism buys immediate refusal rather than
//     exclusion, and that is the whole reason the two do not share a listener.
//   - A handshake SLOT is best effort. Pre-authentication the two are
//     indistinguishable, so an attacker sustaining enough connections a second can
//     still make a node's handshake be refused. It is refused IMMEDIATELY and the
//     node redials, rather than waiting in a backlog nothing will ever read — which
//     is the difference between a service under load and a fleet that is offline.
//
// That residual is not closable in userspace: every TLS server shares it, and it
// is what a rate limiter or connection-tracking firewall in front is for.
//
// ONE MORE RESIDUAL, RECORDED RATHER THAN CLAIMED AWAY. Nothing this listener
// starts outlives Close except a reporting goroutine whose log handler never
// returns. A slog.Handler is supplied from outside and writes synchronously, so
// a sink that blocks forever cannot be cancelled — only bounded, which the
// single-flight flag does at one goroutine per listener. Closing over that would
// mean a process-owned reporting facility with a lifecycle of its own, which is
// a larger thing than the diagnostic it protects. The cost is one goroutine
// holding this listener and its logger, for a condition that means the machine
// is already in trouble.
type handshakingListener struct {
	inner   net.Listener
	tlsConf *tls.Config
	log     *slog.Logger

	// handshakeFor is how long one connection may take to prove itself. A field
	// rather than the constant so a test can choose a short one: the alternative
	// is tests that sleep past the production five seconds, which is both slow
	// and — measured — load-sensitive enough to fail under a full -race suite.
	handshakeFor time.Duration

	// pending bounds handshakes in flight; admitted bounds connections that have
	// completed one. A permit moves from neither to admitted, never between them.
	pending  chan struct{}
	admitted chan struct{}

	// out carries connections the server may have. Unbuffered: a handshake worker
	// hands its connection over or gives it up on close, and nothing queues.
	out chan net.Conn

	// acceptErr is the inner listener's own failure, delivered to Accept once so
	// http.Server.Serve stops for the reason it really stopped.
	acceptErr atomic.Pointer[error]
	stopped   chan struct{}

	closed    chan struct{}
	closeOnce sync.Once

	// closing is set BEFORE `closed` is, and is what Accept re-checks. A select
	// with two ready cases picks at random, so `closed` alone cannot express
	// "this listener has ended" to a receive that also had a connection waiting.
	closing atomic.Bool

	// born is the monotonic origin the report throttles measure from.
	born time.Time

	// refusedPending counts connections turned away before a handshake and
	// refusedAdmitted those turned away after one; acceptFailures counts accepts
	// that failed and were retried. Each is reported on its own throttle.
	refusedPending  atomic.Int64
	refusedAdmitted atomic.Int64
	acceptFailures  atomic.Int64

	refusalReports throttle
	acceptReports  throttle

	// reporting admits one logging goroutine at a time. The throttle bounds how
	// OFTEN a report starts; it does not bound how many are alive, and a slog
	// handler that blocks permanently would otherwise leave one more stuck
	// goroutine every interval, for as long as somebody keeps the condition true
	// — which on the enrollment listener is anybody who can open a socket.
	reporting atomic.Bool
}

// handshakeBounds is the pre-authentication tuning, which both listeners share.
//
// A STRUCT BECAUSE BOTH HAVE TO BE REACHABLE FROM A TEST. At the production 1024
// pending slots and a five second bound, filling the pre-handshake capacity means
// opening a thousand sockets faster than they expire, which is a test that
// measures the machine rather than the code. Zero means the production value, so
// a caller states only what it is changing.
//
// THE ADMITTED BUDGET IS DELIBERATELY NOT IN HERE. It is per-listener policy —
// hundreds for the fleet, dozens for enrollment — so a shared default for it
// would be wrong for one of the two, silently: a caller that omitted it on the
// enrollment listener would have got the node wire's 512.
type handshakeBounds struct {
	// pending is how many connections may be proving themselves at once, and
	// handshakeFor how long one of them gets.
	pending      int
	handshakeFor time.Duration
}

func (b handshakeBounds) orDefaults() handshakeBounds {
	if b.pending <= 0 {
		b.pending = maxPendingHandshakes
	}

	if b.handshakeFor <= 0 {
		b.handshakeFor = handshakeTimeout
	}

	return b
}

// newHandshakingListener wraps inner, admitting at most admitted verified
// connections at a time.
func newHandshakingListener(
	ctx context.Context, inner net.Listener, conf *tls.Config, admitted int,
	bounds handshakeBounds, log *slog.Logger,
) *handshakingListener {
	bounds = bounds.orDefaults()

	l := &handshakingListener{
		inner:        inner,
		tlsConf:      conf,
		log:          log,
		handshakeFor: bounds.handshakeFor,
		pending:      make(chan struct{}, bounds.pending),
		admitted:     make(chan struct{}, admitted),
		out:          make(chan net.Conn),
		stopped:      make(chan struct{}),
		closed:       make(chan struct{}),
		born:         time.Now(),
	}

	go l.accept(ctx)

	return l
}

// accept takes every connection the kernel has and never waits for capacity.
//
// REFUSING IS NOT THE SAME AS NOT ACCEPTING. When the handshake workers are full
// this closes the connection immediately, which the caller sees as a reset it can
// retry. Declining to call the underlying Accept instead leaves the connection in
// the kernel's backlog, where the caller learns nothing until its own timeout —
// and where a node cannot tell a busy control plane from an absent one.
func (l *handshakingListener) accept(ctx context.Context) {
	defer close(l.stopped)

	// ENDING THE ACCEPT LOOP HAS TO RELEASE THE WORKERS. When the loop stops for
	// any reason, `closed` must be set or a handshake still in flight completes
	// and then blocks forever handing over a connection nobody will take, holding
	// a goroutine, a socket and a place in the admitted budget. Closing here gives
	// every one of them the exit it already selects on.
	defer l.end()

	// A TRANSIENT ACCEPT FAILURE MUST NOT END THE WIRE, and getting this wrong is
	// how a fix for one denial of service becomes another. The listener this
	// replaced delegated to the inner Accept on every call, so http.Server.Serve
	// retrying a temporary error — which it does, with backoff, for anything
	// reporting net.Error.Temporary — re-entered the accept path and recovered.
	// One goroutine that returns on the first error does not: Serve retries, gets
	// the same stored error forever, and the wire never admits another connection
	// while the process sits there looking healthy. Descriptor exhaustion is the
	// ordinary way in.
	//
	// SO ONLY A CLOSED LISTENER IS TERMINAL. Everything else backs off and tries
	// again, which fails in the direction of staying up: a permanently broken
	// listener retries once a second with a line saying so, where an operator can
	// see it, rather than going quiet.
	backoff := time.Duration(0)

	for {
		conn, err := l.inner.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.acceptErr.Store(&err)

				return
			}

			backoff = nextBackoff(backoff)
			n := l.acceptFailures.Add(1)

			// THROTTLED FOR THE SAME REASON THE REFUSALS ARE. The backoff resets
			// after every successful accept, so a listener alternating between
			// failing and succeeding — which is what descriptor pressure looks
			// like — returns to a five millisecond retry and would write a line
			// each time. Whoever is causing the pressure would be choosing how
			// much billet writes to disk.
			// COPIED BEFORE THE GOROUTINE SEES THEM. `backoff` lives across
			// iterations of this loop, so a closure reading it later reads whatever
			// the loop has since written — a genuine data race, caught by -race
			// once the retry path and a report overlapped. It arrived with the move
			// to a closure: the previous form evaluated its arguments at the `go`
			// statement, where the loop had not yet moved on. Anything handed to a
			// reporter from inside a loop has to be a copy.
			failed, retryIn, total := err, backoff, n

			// NOT ON THIS GOROUTINE. This is the only one that accepts connections,
			// and a log sink that blocks would stop it accepting AND stop it
			// noticing Close — the listener would be wedged by its own diagnostics,
			// which is the failure this whole file exists to remove.
			l.report(&l.acceptReports, func() {
				l.log.Warn("could not accept a connection; retrying rather than "+
					"giving up, because a listener that stops accepting takes the "+
					"fleet offline silently",
					"error", failed, "retry_in", retryIn, "failures_total", total)
			})

			select {
			case <-time.After(backoff):
			case <-l.closed:
				return
			}

			continue
		}

		backoff = 0

		select {
		case l.pending <- struct{}{}:
		case <-l.closed:
			_ = conn.Close()

			return
		default:
			// Bounded, so an attacker cannot make this process hold unbounded
			// goroutines and sockets — and instant, so a node that loses the race
			// redials rather than waiting on a backlog.
			//
			// AND COUNTED, because this is the branch that fires under the exact
			// flood this listener exists to survive. It said nothing at all at
			// first: an operator whose nodes were being turned away pre-handshake
			// would have seen a healthy control plane and no evidence, which is the
			// same silence the shared budget was reported for.
			_ = conn.Close()
			l.refusedPending.Add(1)
			l.reportRefusals()

			continue
		}

		go l.handshake(ctx, conn)
	}
}

// handshake proves a connection and charges the admitted budget for it.
func (l *handshakingListener) handshake(ctx context.Context, raw net.Conn) {
	defer func() { <-l.pending }()

	// WRAPPED BENEATH THE TLS CONN, not around it, because http.Server type-asserts
	// its connection to *tls.Conn to populate Request.TLS — and every route on the
	// node wire refuses a request whose TLS state is missing. A wrapper on the
	// outside would authenticate nobody and reject the entire fleet.
	tracked := &admittedConn{Conn: raw, release: l.release}
	conn := tls.Server(tracked, l.tlsConf)

	// BOTH DEADLINES, and cleared on success. A write deadline left set would cut
	// a command long poll hours later, on a listener that deliberately has no
	// WriteTimeout precisely so it cannot.
	if err := conn.SetDeadline(time.Now().Add(l.handshakeFor)); err != nil {
		_ = conn.Close()

		return
	}

	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()

		return
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()

		return
	}

	// CHARGED ONLY NOW, once the handshake has actually happened. What that is
	// worth depends on the listener: on the node wire a completed handshake means
	// a certificate this deployment signed, so nothing below here is spendable by
	// a stranger; on the enrollment listener, which asks for none, it means only
	// that somebody completed TLS. Both get the same graceful refusal when the
	// budget is full, and only one of them gets exclusion.
	select {
	case l.admitted <- struct{}{}:
		tracked.charged.Store(true)
	case <-l.closed:
		_ = conn.Close()

		return
	default:
		// CLOSED BEFORE IT IS REPORTED, and reported at most once an interval.
		// Logging first held the socket and its pending permit for the duration of
		// a synchronous write to the log sink — and on the enrollment listener,
		// which asks for no certificate, a stranger reaches this branch at
		// handshake rate. That is a line of log per connection they choose to
		// make, and a sink that blocks would pin every worker's socket with it.
		_ = conn.Close()
		l.refusedAdmitted.Add(1)
		l.reportRefusals()

		return
	}

	select {
	case l.out <- conn:
	case <-l.closed:
		_ = conn.Close()
	}
}

// reportEvery bounds how often a repeating condition is said out loud.
//
// Both users of it are things a caller can cause at whatever rate they like: a
// refusal, and an accept that failed. The first of each is reported immediately
// — a zero timestamp is always older than the interval — and the rest are
// counted, because the alternative is that whoever is causing the condition also
// chooses how much billet writes to disk.
const reportEvery = 30 * time.Second

// throttle allows one report per interval, whoever asks.
//
// IT TAKES ELAPSED TIME RATHER THAN A CLOCK READING. time.Now().UnixNano()
// discards the monotonic component, so a wall clock that steps BACKWARD — an
// operator correcting a drifted host, NTP settling after a long outage — makes
// every later comparison negative and suppresses reporting until real time
// catches up. time.Since keeps the monotonic reading, so nothing about this
// depends on what the wall clock says.
type throttle struct {
	// last is the elapsed time of the last report, PLUS ONE, so that zero can
	// mean "never" — and both facts live in one word.
	//
	// TWO FIELDS WOULD PUBLISH IN TWO STEPS, which is a duplicate report rather
	// than a suppressed one: a caller that claimed the first report and had not
	// yet written its timestamp left a second caller reading "already reported,
	// at time zero", which passes any interval check. The window is small and the
	// common case walks straight into it, since the first refusal usually happens
	// well after the listener started.
	//
	// The sentinel is needed at all because this measures ELAPSED time: a
	// wall-clock reading is always far past any interval, but elapsed time starts
	// at approximately zero, so an age comparison alone would suppress the FIRST
	// report — the one that matters most, in the window where a control plane
	// that has just started is being flooded. It is the sentinel that admits it,
	// not its age.
	last atomic.Int64
}

func (t *throttle) allow(since time.Duration) bool {
	for {
		last := t.last.Load()
		if last != 0 && int64(since)-(last-1) < int64(reportEvery) {
			return false
		}

		// EXACTLY ONE CALLER WINS, because one compare-and-swap decides both
		// whether anybody has reported and when. Losing means somebody else just
		// reported: go round, and the interval check above will say so.
		if t.last.CompareAndSwap(last, int64(since)+1) {
			return true
		}
	}
}

// nextBackoff is http.Server.Serve's own retry schedule, which this listener now
// owns because it no longer lets Serve do the retrying.
func nextBackoff(current time.Duration) time.Duration {
	const (
		first = 5 * time.Millisecond
		most  = time.Second
	)

	switch {
	case current == 0:
		return first
	case current*2 > most:
		return most
	default:
		return current * 2
	}
}

// reportRefusals says how much this listener is turning away, occasionally.
//
// BOTH COUNTS TOGETHER, because they mean different things and the difference is
// what an operator acts on. Refusals before a handshake are load — somebody is
// opening connections faster than they can be proved, which is the flood this
// listener is built to survive and a reason to look at what is in front of the
// port. Refusals after one are more completed handshakes than the budget holds:
// on the node wire that is a fleet larger than its limit, and a number to raise;
// on the enrollment listener it is anybody at all, since that one asks for no
// certificate, and it means the same as the first count with an extra handshake
// spent.
func (l *handshakingListener) reportRefusals() {
	l.report(&l.refusalReports, func() {
		l.log.Warn("this listener is refusing connections",
			"before_handshake", l.refusedPending.Load(),
			"after_handshake", l.refusedAdmitted.Load(),
			"handshake_limit", cap(l.pending),
			"admitted_limit", cap(l.admitted))
	})
}

// report writes a log line without letting the log sink hold up this listener.
//
// ON ANOTHER GOROUTINE, because a slog handler writes synchronously to whatever
// is behind it and a blocked sink — a full pipe, a journal under backpressure —
// would otherwise hold its caller: a handshake worker with a pending permit, or
// the ONE goroutine that accepts connections.
//
// AND AT MOST ONE AT A TIME, which the throttle does not give. A throttle bounds
// how often a report STARTS; against a handler that never returns, that is one
// more stuck goroutine every interval, forever, driven by whoever is keeping the
// condition true. With this flag a stuck reporter means later reports are
// dropped, which is the right way to lose a diagnostic.
//
// THE SLOT IS TAKEN BEFORE THE INTERVAL IS SPENT, and that order matters because
// the two report kinds share one slot. Consulting the throttle first meant a
// report the other kind had crowded out still counted as having been made, so it
// stayed suppressed for a full interval afterwards as well.
//
// WHAT IS STILL LOST, stated because the obvious reading is more generous than
// the code. A crowded-out report is DROPPED, not queued: nothing retries it. For
// a repeating condition that costs nothing, since the counters keep climbing and
// the next report carries the totals. For a condition that happens once and
// never again — a single refusal overlapping a slow accept-error report — the
// line is simply never written. Queueing it would mean a pending report per kind
// and a way to flush them, which is more machinery than a diagnostic for a
// one-off event is worth.
func (l *handshakingListener) report(t *throttle, write func()) {
	if !l.reporting.CompareAndSwap(false, true) {
		return
	}

	if !t.allow(time.Since(l.born)) {
		l.reporting.Store(false)

		return
	}

	go func() {
		defer l.reporting.Store(false)

		write()
	}()
}

// release returns one place to the admitted budget.
//
// NON-BLOCKING, AS A SECOND LINE RATHER THAN AN OPTIMISATION. A receive is how a
// permit comes back, so a release for a connection that never took one would
// block forever on an empty channel and hang the goroutine closing it — an
// http.Server connection goroutine, or a handshake worker. `charged` is what
// stops that happening; this is what stops it being a DEADLOCK if `charged` is
// ever got wrong, because an accounting error a test can see beats a control
// plane that accumulates stuck goroutines every time somebody fails to connect.
func (l *handshakingListener) release() {
	select {
	case <-l.admitted:
	default:
	}
}

// Accept hands over the next connection that verified.
//
// TWO PHASES, because one select over all three channels answers whichever is
// ready FIRST RATHER THAN WHICHEVER IS TRUE: on shutdown both `closed` and
// `stopped` are set, and Go picks at random, so a listener that failed for a real
// reason reported a plain close half the time. The second phase is where the
// error is chosen, once something has ended.
//
// NO PREFERENCE IS EXPRESSED OR NEEDED between a waiting connection and the end,
// and an earlier version of this comment claimed both. A worker still holding a
// connection selects on `closed` as well, and closes and releases when it fires
// — so nothing is stranded by declining to look. See the note further down.
func (l *handshakingListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.out:
		// RE-ASKED, because winning this case does not mean the listener is open:
		// Go picks at random between ready cases, so a worker handing over at the
		// moment of shutdown can be selected ahead of `closed`, and Serve starts a
		// handler after EVERY successful Accept without re-checking shutdown.
		//
		// WHAT THIS GUARANTEES, precisely, because a stronger claim was made here
		// for one commit and was not true. A listener that has ENDED hands nothing
		// over: once end() has run, every later Accept refuses. What it cannot
		// give is atomicity with Serve's decision to start a handler — that
		// happens after Accept returns, so a connection whose check ran a moment
		// before Close can still reach a handler. A mutex around this check was
		// added to close that and DOES NOT: the return is outside it either way,
		// and Serve is not ours to serialize. It was removed rather than left
		// implying a guarantee it could not keep.
		//
		// That residual is also the right behaviour. Such a connection completed
		// its handshake and took its place in the budget BEFORE shutdown began,
		// which is exactly what Shutdown exists to drain.
		if l.closing.Load() {
			// Closed here rather than returned, which releases its permit through
			// the same path every other connection uses.
			_ = conn.Close()

			break
		}

		return conn, nil
	case <-l.closed:
	case <-l.stopped:
	}

	// AND NOTHING IS DRAINED. An earlier version took whatever was left in `out`
	// on the theory that a connection in flight would leak its permit. It would
	// not: a worker holding one selects on `closed` too, and closes and releases
	// when it fires — so no preference for a waiting connection is needed here,
	// and asserting one would be describing an ordering select does not have.
	if err := l.acceptErr.Load(); err != nil {
		return nil, *err
	}

	return nil, net.ErrClosed
}

func (l *handshakingListener) Addr() net.Addr { return l.inner.Addr() }

// end marks this listener finished, exactly once.
//
// THE FLAG IS SET BEFORE THE CHANNEL, and that order is the whole point: a
// receiver that finds both a connection and `closed` ready picks between them at
// random, so it has to be able to ASK whether the listener ended rather than
// infer it from which case fired.
func (l *handshakingListener) end() {
	l.closeOnce.Do(func() {
		// THE FLAG BEFORE THE CHANNEL, so nothing can observe the channel closed
		// while the flag still says open.
		l.closing.Store(true)
		close(l.closed)
	})
}

func (l *handshakingListener) Close() error {
	l.end()

	// IN-FLIGHT HANDSHAKES ARE NOT WAITED FOR, and that is deliberate. Each ends
	// within handshakeTimeout and closes its own connection on the way out; making
	// Close block on them would put an attacker's five seconds inside a shutdown
	// budget of five seconds.
	return l.inner.Close()
}

// admittedConn releases the server's budget when the connection it carries closes.
//
// It sits UNDER the tls.Conn, so closing the tls.Conn closes this, which is the
// one path every connection takes on its way out.
type admittedConn struct {
	net.Conn

	// charged says whether a permit was ever taken for this connection. A
	// handshake that failed took none, and releasing one it never held STEALS
	// somebody else's: the budget's count drifts down while the connections it
	// was counting are still live, and the listener ends up admitting more than
	// its limit. A refused connection is the common case on a public port, so
	// that drift is continuous rather than occasional.
	//
	// It also used to be the difference between working and DEADLOCKING, when
	// release was a bare receive — a connection that never sent to the channel
	// blocked forever taking from it, hanging whichever goroutine was closing.
	// release is non-blocking now, so the failure is the accounting one above;
	// the note is kept because that is the version a reader is likely to write
	// if they simplify it back.
	charged atomic.Bool
	once    sync.Once
	release func()
}

func (c *admittedConn) Close() error {
	err := c.Conn.Close()

	if c.charged.Load() {
		c.once.Do(c.release)
	}

	return err
}
