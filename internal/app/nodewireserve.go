package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// IdentityAccess takes the exclusion a process holds while it reads, and on
// first use creates, the identity directory's authority, and returns its
// release. It is the host's, not the assembly's: cmd/billet supplies it until
// the host authority moves out of cmd/billet (#356 Phase 4b).
type IdentityAccess func(ctx context.Context, dir string) (release func() error, err error)

// nodeTLSHosts is what a node will type to reach this control plane.
//
// A WILDCARD LISTEN ADDRESS ANSWERS A DIFFERENT QUESTION than this one. It says
// which interfaces to accept on; it says nothing about the name a node dials,
// and a certificate minted for "0.0.0.0" matches nothing. The failure would land
// on the node as a name mismatch, on the far side of the deployment from the
// file that caused it — so it is refused here, where the file is.
//
// BOTH LISTENERS PRESENT ONE CERTIFICATE, so a bootstrap address bound to a
// different concrete host than the node wire contributes its own name. An
// enrolling node verifies this certificate by the hostname it dialled, right
// after the fingerprint check, so a missing subject name is a handshake failure
// during enrollment and nothing else. An explicit node_tls_hosts is taken as
// written: an operator naming the addresses their fleet uses is answering this
// question themselves, and the bootstrap name has to be among them.
func nodeTLSHosts(cfg *config.Config) ([]string, error) {
	if len(cfg.Server.NodeTLSHosts) > 0 {
		return cfg.Server.NodeTLSHosts, nil
	}

	host, err := dialledHost("server.listen", cfg.Server.Listen)
	if err != nil {
		return nil, err
	}

	hosts := []string{host}

	// A WILDCARD BOOTSTRAP ADDRESS IS NOT AN ERROR HERE. It says which interfaces
	// to accept enrollments on, and the node wire's own concrete host above is
	// already a name that reaches this machine — so there is nothing missing,
	// unlike the wildcard node wire, which leaves nothing at all.
	if bootstrap := strings.TrimSpace(cfg.Server.BootstrapListen); bootstrap != "" {
		if bootstrapHost, err := dialledHost("server.bootstrap_listen", bootstrap); err == nil &&
			bootstrapHost != host {
			hosts = append(hosts, bootstrapHost)
		}
	}

	return hosts, nil
}

// dialledHost is the concrete name in a listen address, or an error naming what
// to set instead.
func dialledHost(key, addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%s %q must be host:port: %w", key, addr, err)
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf(
			"%s is %q, which accepts on every interface and so does not say what a "+
				"node will dial. Set server.node_tls_hosts to the names and addresses nodes use "+
				"for this control plane; they become the subject names of the certificate it "+
				"serves", key, addr)
	}

	return host, nil
}

// wireOption adjusts how serveNodeWire builds its listeners.
type wireOption func(*wireLimits)

// wireLimits are the two connection budgets, which are separate on purpose — an
// anonymous caller occupies the enrollment listener's and can never occupy the
// node wire's — plus how long a connection may take to prove itself.
type wireLimits struct {
	operational, bootstrap int
	handshake              time.Duration
}

// withConnectionLimits is for tests, which need both budgets SMALL AND EQUAL.
// At the production sizes a test cannot tell one shared budget from two: filling
// the enrollment listener's 64 leaves 448 of a shared 512, so the node wire is
// served either way and the assertion proves only that 64 is a bound.
func withConnectionLimits(operational, bootstrap int) wireOption {
	return func(l *wireLimits) { l.operational, l.bootstrap = operational, bootstrap }
}

// withHandshakeTimeout is for tests, which otherwise have to SLEEP past the
// production five seconds to observe anything about the bound. Several of them
// do, and measured, that is enough added wall clock to make an already
// load-sensitive package fail under a full -race run.
func withHandshakeTimeout(d time.Duration) wireOption {
	return func(l *wireLimits) { l.handshake = d }
}

// stopServing drains a listener, and stops it outright if draining runs out of
// time.
//
// SHUTDOWN ALONE IS NOT A STOP. It returns ctx.Err() on expiry and leaves every
// active connection and handler RUNNING, with their request contexts uncancelled
// — so an enrollment blocked in the ledger outlives ServeNodeWire and is still
// using the database when the control plane closes the ledger after the wire.
// Close is what ends them.
//
// A FRESH CONTEXT, deliberately. This runs while the caller's context is already
// cancelled — that cancellation is what brought us here — so deriving from it
// would abort the drain instantly and cut the very connections this exists to
// let finish.
//
// THE DEADLINE CAN EXPIRE WITH NOTHING IN FLIGHT, and Close is what ends that
// case. A connection the server has ACCEPTED but that has sent no request
// header is StateNew, and Shutdown counts it as idle only once it has been so
// for more than five seconds (net/http's closeIdleConns, "Issue 22682"), so a
// five-second deadline is a photo finish the deadline wins. Measured with a
// standalone probe: one accepted-but-silent connection returned `context
// deadline exceeded` after exactly 5s under a 5s deadline. A node makes one
// whenever its loop is cancelled between dialling and writing a request.
func stopServing(ctx context.Context, srv *http.Server, what string) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Default().Warn("the "+what+" did not drain in time; closing it, which ends any "+
			"request still in flight", "error", err)

		if err := srv.Close(); err != nil {
			slog.Default().Warn("the "+what+" did not close cleanly", "error", err)
		}
	}
}

// ServedWire is what a running node wire reports about itself.
//
// THE ADDRESSES ARE RESOLVED, NOT CONFIGURED. A wildcard or a zero port in the
// config says what to bind, not what was bound, and the two listeners' real
// addresses are the only way to say anything about them afterwards.
type ServedWire struct {
	Stop func()
	// Addr is the operational wire. Bootstrap is the enrollment listener, empty
	// when this deployment does not enroll over the network.
	Addr      string
	Bootstrap string
}

// ServeNodeWire opens the listener nodes dial, holding access while it reads
// the deployment's authority.
//
// LOOPBACK IS THE ONLY ADDRESS SERVED WITHOUT mTLS. The wire identifies a node
// by the name in its certificate; without one, the only thing left is the name
// in the request path, which is a claim rather than a proof. Anything that could
// reach such a listener could bind another node's leases, take its commands,
// and — worst — ask for a JIT registration, a credential that registers a runner
// against the organisation.
//
// So a network address mints a certificate from the deployment's own authority
// and requires one back — in the HANDSHAKE, so a caller with nothing to present
// never reaches billet's HTTP server and cannot occupy a connection an enrolled
// node needs. There is nothing to configure and nothing to install: the CA lives
// beside the state directory, and `billet ca issue <node>` produces the bundle a
// node is given.
//
// THAT IS WHY THERE ARE TWO LISTENERS. The two routes a machine with no
// certificate needs cannot require one, so they are served by serveBootstrapWire
// on server.bootstrap_listen, with a budget of its own; serveBootstrapWire says
// what sharing one budget cost.
// THE DEPLOYMENT IS READ HERE RATHER THAN PASSED IN, because a parameter of
// that type is one a caller can fill with the wrong string and the compiler
// cannot tell. It was filled with the hostname, and both boot orders failed
// silently: minted against a hostname, the authority produces node certificates
// carrying a deployment no node can parse and the plane refuses forever; minted
// by `billet ca issue` against the real id, this function refuses to start at
// all. Neither shows up on loopback, which is every local run.
//
// state.DeploymentID reads the file the rest of the process already read, so
// there is nothing to keep in step.
func ServeNodeWire(
	ctx context.Context,
	cfg *config.Config,
	access IdentityAccess,
	nodes *nodeplane.Plane, store nodeplane.LeaseStore, jit map[string]nodeplane.JITSource,
	revocations nodeplane.Revocations, enrollments nodeplane.Enrollments,
	cachePolicy nodeplane.CachePolicy,
	opts ...wireOption,
) (*ServedWire, error) {
	limits := wireLimits{
		operational: nodeWireConnectionLimit,
		bootstrap:   bootstrapConnectionLimit,
	}
	for _, opt := range opts {
		opt(&limits)
	}

	addr := cfg.Server.Listen
	loopback := nodeplane.LoopbackOnly(addr)

	// THE ACCESS AGAIN, AFTER PROMOTION: the authority load below reads (and
	// on first use creates) the CA, and a retirement of this host cannot be
	// running while the server it stops first is here, but a rotation can.
	release, err := access(ctx, cfg.Server.IdentityDir)
	if err != nil {
		return nil, err
	}

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return nil, errors.Join(err, release())
	}

	var (
		hosts         []string
		stopBootstrap = func() {}
		bootstrapAddr string
		// startBootstrap opens the enrollment listener once the node wire holds
		// its own socket. Nil on a loopback wire, which has no certificates and
		// so nothing to enroll into.
		startBootstrap func() error
	)

	if !loopback {
		if hosts, err = nodeTLSHosts(cfg); err != nil {
			return nil, errors.Join(err, release())
		}
	}

	// ASSEMBLED BY BuildNodeWire, NOT HERE, AND IT HANDS BACK THE HANDLER
	// RATHER THAN THE PIECES. What the server presents, what it accepts and what
	// renewal signs with are three answers that must describe one moment and one
	// authority, so BuildNodeWire assembles them from one read and this function
	// only installs what it returns; the serving tests here prove it is installed
	// (TestTheServedWireCarriesTheFleetOntoTheNewAuthority). Returning the options
	// and installing them from the caller was the first attempt and had
	// the same shape as the bug it fixed: deleting the line that installed them
	// left every test green.
	wire, err := BuildNodeWire(NodeWireRequest{
		StateDir:    cfg.Server.IdentityDir,
		Deployment:  deployment,
		Hosts:       hosts,
		Loopback:    loopback,
		Log:         slog.Default(),
		Plane:       nodes,
		Leases:      store,
		TargetJIT:   jit,
		Revocations: revocations,
		Enrollments: enrollments,
		CachePolicy: cachePolicy,
	})
	if err := errors.Join(err, release()); err != nil {
		return nil, err
	}

	if wire.Rotating {
		slog.Default().Warn("a certificate authority rotation is running; nodes adopt the "+
			"new one as they renew, and `billet ca retire` finishes it once they all have",
			"started", wire.RotationAge.Round(time.Hour))
	}

	if !loopback {
		slog.Default().Info("the node wire requires client certificates",
			"hosts", hosts, "ca_expires", wire.IssuingExpiry.Format(time.DateOnly))

		// THE ENROLLMENT SURFACE IS SOMEWHERE ELSE OR NOWHERE. The two routes a
		// machine with no certificate needs cannot live on the wire above, because
		// they would drag its connection budget down to whatever an anonymous
		// caller feels like taking.
		//
		// IT TAKES THE SAME ONE READ OF THE AUTHORITY, and that matters for the
		// same reason the read exists: what this listener PRESENTS and what it
		// hands an enrolling machine to TRUST have to describe one moment, or a
		// `billet ca retire` landing between them admits a node against an
		// authority the control plane has stopped presenting. Presents signs the
		// certificate both listeners serve; Issuing is what approvals are signed
		// by and what `billet ca show` reports; Trust is every authority a node
		// should accept while an overlap runs. BuildNodeWire hands all three back
		// from that one read, which is what makes "the same moment" a fact rather
		// than a convention two call sites have to keep.
		//
		// DEFERRED UNTIL THE NODE WIRE HAS ITS SOCKET, and that ordering is the
		// whole reason this is a closure rather than a call. Config validation
		// refuses two addresses that name one socket, but it cannot see every
		// overlap a host can produce — and whichever listener binds SECOND is the
		// one that reports the collision. Binding enrollment first therefore
		// reported an operator's mistake against server.listen, which was fine.
		startBootstrap = func() error {
			var err error

			stopBootstrap, bootstrapAddr, err = serveBootstrapWire(
				ctx, cfg, wire.Bootstrap, wire.Serving, limits.bootstrap, limits.handshake)

			return err
		}
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for nodes on %s: %w", addr, err)
	}

	if startBootstrap != nil {
		if err := startBootstrap(); err != nil {
			// The node wire's socket is open and nothing is serving it yet, so it
			// is this function's to close.
			_ = ln.Close()

			return nil, err
		}
	}

	// BOUNDED, AND CHARGED ONLY TO CALLERS THAT PROVED WHO THEY ARE. This budget
	// used to be taken in Accept, before the underlying accept and therefore
	// before the handshake — which cannot tell an enrolled node from a stranger,
	// because at that point nobody has presented anything. So the fleet's capacity
	// was spendable by whoever could open a socket, and while it was full Accept
	// blocked before the kernel accept and a node's connection waited in the
	// backlog until its own dial timeout. Moving the unauthenticated routes to
	// their own listener did not fix that half; this does.
	//
	// handshakingListener accepts unconditionally, bounds handshakes separately,
	// and charges this budget only for a connection that verified. What is
	// guaranteed is that anonymous traffic can never displace an admitted node;
	// what is best effort is a handshake slot, which no server can reserve for a
	// caller it has not yet identified.
	//
	// NOT CANCELLED BY THE SIGNAL. ctx is cancelled by SIGTERM, when a drain begins,
	// and the wire must keep serving until stop runs after the drain: handshaking
	// under it refused every node connection for the whole drain (measured
	// 2026-10-03, over forty minutes), so running jobs lost their cache and the node
	// could not renew a lease. Close ends the listener; the per-handshake deadline
	// bounds each handshake.
	if wire.TLS != nil {
		ln = newHandshakingListener(context.WithoutCancel(ctx), ln, wire.TLS, limits.operational,
			handshakeBounds{handshakeFor: limits.handshake}, slog.Default())
	} else {
		// A LOOPBACK WIRE HAS NO HANDSHAKE TO WAIT FOR. There are no certificates
		// here at all — the trust boundary is the machine — so there is nothing to
		// charge after, and the simple bound is the honest one.
		ln = newLimitedListener(ln, limits.operational)
	}

	srv := &http.Server{
		Handler: wire.Handler,
		// A command poll is a LONG poll, so there is deliberately NO WriteTimeout:
		// one would cut every cycle. The rest are safe and are what stop a stalled
		// or hostile connection holding a slot.
		//
		// ReadTimeout bounds the REQUEST, never the response, so a long poll is
		// untouched -- it is what stops a body dribbled a byte at a time from
		// holding a connection for days, which ReadHeaderTimeout does not cover.
		//
		// IdleTimeout is 120s BECAUSE THE CLIENT'S OWN IdleConnTimeout IS 90s
		// (internal/nodeclient/client.go). Strictly longer, so it can only ever
		// reap a connection the client has already abandoned or that was never a
		// node; a shorter value would cut healthy keep-alives.
		//
		// The 90s is what the ONLY production construction gets: cmd/billet builds
		// its nodeclient with no HTTP client of its own, so it takes the package's
		// transport. That is the whole claim -- a caller that supplies its own
		// client can choose any idle it likes, and this timeout is sized against
		// the one billet ships rather than against every possible caller.
		//
		// ReadTimeout does not cut a long poll: measured, a handler holding the
		// response for 5s completes under a 2s ReadTimeout, and the connection is
		// still reusable afterwards. Go applies it to reading the request, not to
		// the handler.
		//
		// THE HANDSHAKE IS NOT BOUNDED HERE, and that is a change worth knowing
		// about. Go's Server.tlsHandshakeTimeout is the minimum of the POSITIVE
		// ReadHeaderTimeout, ReadTimeout and WriteTimeout — an emergent number,
		// which on this listener happened to be 10s and would have become
		// unlimited if the other two were ever zeroed. handshakingListener does
		// the handshake itself under an explicit handshakeTimeout before this
		// server ever sees the connection, so by the time http.Server re-checks,
		// the handshake is already complete and its own bound is a no-op.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the node listener stopped", "error", err)
		}
	}()

	slog.Default().Info("serving the node wire", "addr", ln.Addr().String())

	return &ServedWire{
		Addr:      ln.Addr().String(),
		Bootstrap: bootstrapAddr,
		Stop: func() {
			// ENROLLMENT STOPS FIRST. It is the surface that admits callers with
			// nothing to present, so leaving it open while the node wire spends up
			// to five seconds draining would keep admitting machines to a
			// deployment that is going away — and its handlers are the ones
			// holding the ledger this process is about to close.
			stopBootstrap()
			stopServing(ctx, srv, "node listener")
		},
	}, nil
}

// serveBootstrapWire opens the listener a machine with no certificate dials.
//
// SEPARATE FROM THE NODE WIRE BECAUSE IT CANNOT AUTHENTICATE. Reading this
// deployment's authority and asking to join are the two things a machine must be
// able to do before it has anything to prove, so the listener in front of them
// admits strangers — and a listener that admits strangers must not share a
// connection budget with the fleet, or a few requests a second take every node
// offline. Saturating this one delays an enrollment instead.
//
// ABSENT IS THE ORDINARY ANSWER. Without server.bootstrap_listen this control
// plane simply does not enroll over the network, and `billet ca issue <node>`
// remains the way in: an operator mints the bundle here and copies it out of
// band, which is the right shape for a machine being provisioned anyway.
//
// The port is meant to be closable between enrollments — nothing that keeps a
// running fleet working goes through it.
func serveBootstrapWire(
	ctx context.Context,
	cfg *config.Config,
	handler http.Handler,
	bundle wirecert.Bundle,
	limit int,
	handshakeFor time.Duration,
) (func(), string, error) {
	addr := strings.TrimSpace(cfg.Server.BootstrapListen)
	if addr == "" {
		slog.Default().Info("this control plane does not enroll nodes over the network; "+
			"issue a bundle with `billet ca issue <node>` and copy it to the host, or set "+
			"server.bootstrap_listen to serve enrollment on an address of its own",
			"listen", cfg.Server.Listen)

		return func() {}, "", nil
	}

	// NO CLIENT CERTIFICATE IS ASKED FOR. Nobody reaching here has one, and not
	// asking means this listener never parses or verifies a chain a stranger
	// chose. What secures these routes is the fingerprint the operator compared,
	// the join token, and the approval that waits for a human.
	tlsConf, err := wirecert.BootstrapTLS(bundle)
	if err != nil {
		return nil, "", err
	}

	var lc net.ListenConfig

	// FATAL, AND THE ESCAPE HATCH IS NAMED. A control plane that cannot bind this
	// is almost always a config error — the node wire's own port, or a second
	// billet — and starting anyway would leave an operator debugging a handshake
	// failure on the far machine with nothing here saying why. But this is an
	// optional surface on the one process whose loss stops every job, so the
	// error has to say how to start without it.
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf(
			"listen for enrollments on %s: %w\n(this is server.bootstrap_listen; remove it to "+
				"start the control plane without network enrollment and admit machines with "+
				"`billet ca issue <node>` instead)", addr, err)
	}

	// THE SAME ACCEPTOR, for the half of its value that applies here. Nobody
	// reaching this listener has a certificate, so "admitted" is not a trust
	// statement and the budget it guards is an anonymous one by design — but the
	// accept loop still never blocks on a permit, so a saturated enrollment port
	// refuses immediately instead of leaving a machine waiting on a backlog it
	// cannot tell from an absent control plane.
	bounded := newHandshakingListener(ctx, ln, tlsConf, limit,
		handshakeBounds{handshakeFor: handshakeFor}, slog.Default())

	srv := &http.Server{
		// ASSEMBLED BY BuildNodeWire FROM THE SAME ONE READ the operational wire
		// used, rather than here from pieces handed across: two call sites reading
		// the authority separately is the defect wiring exists to remove, and this
		// listener is its second consumer.
		Handler: handler,
		// TIGHTER THAN THE NODE WIRE'S, ALL OF IT, because nothing here is a long
		// poll: both routes are one small request and one small response. That is
		// why WriteTimeout can be set at all, which the node wire cannot do. The
		// handshake itself is bounded by handshakingListener rather than by any of
		// these, so a connection cannot be held for long whatever it does.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	go func() {
		if err := srv.Serve(bounded); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("the enrollment listener stopped; enrolled nodes are unaffected",
				"error", err)
		}
	}()

	slog.Default().Info("serving enrollment on its own listener; it can be closed between "+
		"enrollments without affecting the fleet", "addr", ln.Addr().String())

	return func() { stopServing(ctx, srv, "enrollment listener") }, ln.Addr().String(), nil
}

// nodeWireConnectionLimit caps concurrent AUTHENTICATED connections to the
// control plane's node wire.
//
// Sized so a real fleet never reaches it -- a node holds one command poll plus
// the occasional short request, so this is hundreds of nodes.
//
// WHO CAN SPEND IT IS THE PART THAT CHANGED. It used to be charged in Accept,
// before the handshake, so anyone able to open a socket could spend the fleet's
// capacity; handshakingListener charges it only once a connection has verified
// against this deployment's authority. It still depends on the IdleTimeout above
// -- a connection that never speaks again holds its place until something reaps
// it -- but the callers who can reach that state are now hosts billet issued a
// certificate to.
const nodeWireConnectionLimit = 512

// bootstrapConnectionLimit caps concurrent connections to the enrollment
// listener.
//
// Small on purpose: enrolling is a human-paced operation on a handful of
// machines, and this is the one listener a stranger can complete a handshake
// against -- it asks for no certificate, so "admitted" here is not a trust
// statement and this budget is an anonymous one by design. Saturating it delays
// an enrollment; it cannot reach the node wire, which is the whole reason the two
// are separate.
const bootstrapConnectionLimit = 64

type limitedListener struct {
	net.Listener
	semaphore chan struct{}

	// CLOSING A SATURATED LISTENER HAS TO WORK. Accept blocks acquiring a permit,
	// and closing the underlying listener does NOT unblock a channel send -- so a
	// full semaphore left Shutdown waiting on the accept goroutine before its
	// context deadline could apply, and the process hung until a connection
	// happened to free a slot.
	//
	// THAT WAIT WAS UNBOUNDED, not "up to the idle timeout": IdleTimeout bounds
	// inactivity BETWEEN requests, so anything sending another request before each
	// deadline holds its permit indefinitely and the shutdown never completes.
	closed    chan struct{}
	closeOnce sync.Once
}

// LimitListener bounds inner to limit connections at once, for a listener with
// no handshake to charge after (a loopback node wire, the node's cache).
func LimitListener(inner net.Listener, limit int) net.Listener {
	return newLimitedListener(inner, limit)
}

func newLimitedListener(inner net.Listener, limit int) *limitedListener {
	return &limitedListener{
		Listener:  inner,
		semaphore: make(chan struct{}, limit),
		closed:    make(chan struct{}),
	}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	// SELECT, NOT A BARE SEND. Whichever is ready first: a permit, or the listener
	// being closed. Without the second case a saturated listener cannot be shut
	// down at all.
	select {
	case l.semaphore <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.semaphore

		return nil, err
	}

	return &limitedConn{Conn: conn, release: func() { <-l.semaphore }}, nil
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })

	return l.Listener.Close()
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)

	return err
}
