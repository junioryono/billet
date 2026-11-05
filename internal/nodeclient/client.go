// Package nodeclient is a compute host's half of the node wire.
//
// It dials the control plane, registers, and then does two things forever: long-
// polls for commands, and answers the ledger questions the runner asks. The
// LeaseStore it implements is the same interface internal/node.Runner already
// takes, which is why the runner needs no knowledge that its ledger is now on
// the other side of a network.
package nodeclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/usage"
	"github.com/junioryono/billet/internal/version"
	"github.com/junioryono/billet/internal/wirecert"
)

// ErrUnregistered means the control plane does not know this node.
//
// The node's answer is to register again, not to retry the same call: this is
// what a restarted control plane looks like from here, and retrying a lease
// write it will keep refusing is an infinite loop that fixes nothing.
var ErrUnregistered = errors.New("nodeclient: the control plane does not know this node")

// ErrRefused means the control plane understood the request and rejected it.
//
// RETRYING CANNOT HELP, which is the whole reason it is a distinct error. A
// protocol version mismatch or a foreign deployment identity will be refused
// identically forever, and a node that retried every five seconds would sit
// there looking alive while never being able to work — the failure nobody
// notices because nothing is crashing.
var ErrRefused = errors.New("nodeclient: the control plane refused this node")

// ErrUnauthenticated means the connection proved nothing about who it is.
//
// NOT A VERDICT, and that distinction is the point. ErrRefused is permanent and
// the node stops on it. This is an expired, missing, or replaced certificate —
// something an operator fixes on this host, after which the same node connects
// fine. A node that gave up here would have to be restarted by hand once they
// had.
var ErrUnauthenticated = errors.New("nodeclient: this node did not prove who it is")

// ErrSuperseded means another process has registered under this node's name.
//
// The node STOPS on this, and stopping is the point. Re-registering would take
// the name back, the other host would take it back in turn, and the control
// plane's accounting would follow neither while both ran containers against the
// same leases. It is a configuration mistake — one certificate bundle copied to
// two machines, or one name written into two config files — and an operator has
// to fix it.
var ErrSuperseded = errors.New("nodeclient: another process is registered as this node")

// Client talks to a control plane.
type Client struct {
	// targets are every controller this node may register with, in the order
	// it tries them, and current the one its registration was accepted by
	// (the first until one is): every request but a registration goes there.
	targets []*target
	current atomic.Int64
	node    string
	// installedDigest is the release manifest that produced this binary, proved
	// against the bytes running, or empty when nothing on this machine can say.
	//
	// HELD FOR THE LIFE OF THE CLIENT, and read when it is built. See the function
	// of the same name for why the answer must describe the bytes this process
	// started with rather than whatever is at the path when a registration happens.
	installedDigest string
	// incarnation identifies THIS node process, for the whole of its life.
	//
	// Minted once, here, rather than per registration: a node that re-registers
	// after the plane forgot it is the same process holding the same compute, and
	// giving it a new identity each time would make every reconnect look like a
	// second host.
	incarnation string
	reqTimeout  time.Duration

	// WRITTEN BY EVERY REGISTRATION, READ BY THE JANITOR AND THE POLL LOOP, which
	// are different goroutines with no lock between them. Plain fields here were a
	// data race that -race would only catch on the re-registration a test has to
	// go out of its way to produce: the first registration happens-before the
	// janitor starts, and every one after it does not.
	ttl  atomic.Int64 // nanoseconds
	poll atomic.Int64 // nanoseconds
	// wire is the protocol version registration settled on, and it governs this
	// incarnation for as long as that registration stands.
	wire atomic.Int64

	// generation counts successful registrations, and regMu orders it against
	// the wire it publishes.
	//
	// A RESPONSE IS ABOUT THE REGISTRATION ITS REQUEST WAS SENT UNDER. Every
	// request captures the generation before it goes out, and an "unregistered"
	// answer clears the wire only while that generation is still current — a
	// heartbeat that left before a re-registration and came back after it is
	// telling the truth about a registration that no longer exists, and acting
	// on it would erase the one that does.
	regMu      sync.Mutex
	generation int64
	// imagesAccept is what the last accepted registration's answer said the
	// deployment accepts, and imagesAcceptCarried whether its wire carries that
	// at all (nodeapi.VersionImageAcceptance); both under regMu.
	imagesAccept        []string
	imagesAcceptCarried bool
	// macOSLimit is the node's macOS limit as the last accepted registration's answer
	// said it, and nil when its wire does not carry one; under regMu.
	macOSLimit *int

	// guestUnsent is set the first time a guest report is not sent because the
	// negotiated wire does not carry one, so that is said once and not once a
	// job.
	guestUnsent atomic.Bool
}

// Options configures a Client.
type Options struct {
	// Base is the control plane's address, as a URL or a bare host:port.
	//
	// A bare address takes its scheme from whether TLS is configured, which is the
	// only place that answer is known. Config carries host:port because that is
	// what an operator writes and what the address can be validated as; a URL
	// there would let someone write http:// beside a certificate and get a node
	// that quietly never used it.
	Base string
	// Node is this host's name, which must match its entry in the server's
	// fleet configuration.
	Node string
	// HTTP is the transport. A caller supplies one so timeouts are decided once
	// rather than here.
	HTTP *http.Client
	// TLS is the node's side of the wire: its certificate, and the deployment
	// authority it verifies the control plane against.
	//
	// Nil serves loopback, and nothing else — a control plane bound to a network
	// address refuses to start without a certificate, so a node reaching one over
	// the network without this fails at the handshake rather than half-connecting.
	TLS *tls.Config
	// RequestTimeout bounds an ordinary request. Zero uses requestTimeout.
	//
	// Configuration rather than a test hook: a node on a slow or distant link
	// legitimately needs longer, and the alternative is every such deployment
	// discovering the constant by hitting it.
	RequestTimeout time.Duration
	// Targets are every controller of an active-passive pair, in the order the
	// node tries them, each with its own TLS (or HTTP client), in place of Base,
	// TLS and HTTP above, which are one target's shorthand.
	//
	// ONE TLS CONFIG A TARGET, because a node verifies each controller's
	// certificate against the name it dialled (wirecert's ClientTLS fixes it),
	// and two controllers have two names.
	Targets []Target
}

// Target is one controller a node may register with.
type Target struct {
	// Base, TLS and HTTP mean what Options' fields of the same names mean.
	Base string
	TLS  *tls.Config
	HTTP *http.Client
}

// target is one controller as the client reaches it.
type target struct {
	base string
	// endpoint is the request base as the one representation, derived from
	// base at construction and fixed for the client's life.
	endpoint endpoint.Endpoint
	http     *http.Client
	// dialer is the dialer the client's own transport connects with; nil for a
	// caller's HTTP client, whose transport is the caller's.
	dialer *net.Dialer
	// resolving is how the transport reaches one of several controllers, nil
	// for a client of one.
	resolving *controllerDialer
}

// Dead-peer detection on the node's connections.
//
// A CONTROLLER CUT OFF THE NETWORK SENDS NOTHING, not even a reset, so a long
// poll to it waited out the whole poll window and its slack before the node
// tried anything else: measured 2026-09-04 (scripts/promotion-rehearsal.sh),
// the node re-registered 41 seconds after its standby was promoted. Probes
// that go unanswered end the connection in about Idle + Interval x Count
// seconds; a live controller's kernel answers them, so they never cut a healthy
// poll short. The dial is bounded more tightly than the handshake, because an
// address that does not answer a SYN in three seconds is not the one to wait
// on while another controller may be serving. Measured 2026-10-25 on a
// partition that drops packets: nodes said they had lost their controller 13.9
// to 14.3 seconds after it, and every node was registered with the promoted
// one 0.3 to 2.7 seconds after its promotion.
//
// THE NAME IS LOOKED UP OUTSIDE THE DIAL'S BOUND, under its own. A dialer's
// timeout covers its lookup too, and this static build's resolver waits five
// seconds on each nameserver it tries, so a three-second dial behind a dead
// first nameserver would never connect to anything.
//
// ONLY FOR SEVERAL CONTROLLERS. A node that names one has nowhere else to go,
// and dials as it always did (singleDialTimeout, Go's default keepalive).
const (
	keepAliveIdle     = 5 * time.Second
	keepAliveInterval = 2 * time.Second
	keepAliveCount    = 3
	dialTimeout       = 3 * time.Second
	lookupTimeout     = 10 * time.Second
	singleDialTimeout = 10 * time.Second
)

// New builds a Client. It does not dial; Register does.
func New(opts Options) (*Client, error) {
	if opts.Node == "" {
		return nil, errors.New("nodeclient: a node name is required")
	}

	targets := opts.Targets
	if targets == nil {
		if opts.Base == "" {
			return nil, errors.New("nodeclient: a control plane address is required")
		}

		targets = []Target{{Base: opts.Base, TLS: opts.TLS, HTTP: opts.HTTP}}
	} else if opts.Base != "" || opts.TLS != nil || opts.HTTP != nil {
		return nil, errors.New("nodeclient: both Targets and Base, TLS or HTTP were supplied; name every " +
			"controller in Targets")
	}

	if len(targets) == 0 {
		return nil, errors.New("nodeclient: a control plane address is required")
	}

	incarnation, err := mintIncarnation()
	if err != nil {
		return nil, err
	}

	c := &Client{
		node:        opts.Node,
		incarnation: incarnation,
		reqTimeout:  opts.RequestTimeout,
		// READ HERE, ONCE, FOR THE LIFE OF THIS NODE. See installedDigest.
		installedDigest: installedDigest(),
	}

	if c.reqTimeout <= 0 {
		c.reqTimeout = requestTimeout
	}

	for i, spec := range targets {
		t, err := newTarget(spec, len(targets) > 1)
		if err != nil {
			return nil, err
		}

		// THE SAME CONTROLLER TWICE is compared as the one representation, so
		// two spellings of one endpoint are one target.
		for _, earlier := range c.targets[:i] {
			if earlier.endpoint.Equal(t.endpoint) {
				return nil, fmt.Errorf("nodeclient: the control plane %s is named twice", t.base)
			}
		}

		c.targets = append(c.targets, t)
	}

	return c, nil
}

// newTarget builds one controller's request base and transport, as one of
// several when several is true.
func newTarget(spec Target, several bool) (*target, error) {
	if spec.Base == "" {
		return nil, errors.New("nodeclient: a control plane address is required")
	}

	base, err := normaliseBase(spec.Base, spec.TLS != nil)
	if err != nil {
		return nil, err
	}

	// THE ACCESSOR IS DERIVED FROM THE BASE, so the endpoint the record names and
	// the request base every call is built from cannot diverge: one is the
	// other's canonical text, read back.
	ep, err := endpoint.ParseCanonical(base)
	if err != nil {
		return nil, fmt.Errorf("nodeclient: the request base %q is not an endpoint's canonical text: %w", base, err)
	}

	t := &target{base: base, endpoint: ep, http: spec.HTTP}

	// A SUPPLIED CLIENT KEEPS ITS OWN TRANSPORT. Reaching into someone else's
	// http.Client to install a TLS config would silently change every other
	// request that client makes, and the caller that passed it in is the one who
	// knows whether that is wanted.
	if t.http != nil && spec.TLS != nil {
		return nil, errors.New(
			"nodeclient: both an HTTP client and a TLS config were supplied; the TLS config " +
				"would have to be installed into a transport this package does not own")
	}

	if t.http == nil {
		// NO CLIENT-WIDE TIMEOUT, deliberately: a command poll is a long poll and
		// a blanket Timeout would cut it every cycle. What replaces it is a
		// per-request deadline, applied below — the first version of this comment
		// promised that and no call actually had one, so every operation could
		// hang forever against a control plane that accepted the connection and
		// then said nothing.
		//
		// The transport still bounds the phases a long poll does not need to keep
		// open. A dial or a TLS handshake that never completes is never a healthy
		// poll, and leaving those unbounded means a wedged network holds a
		// goroutine and a socket indefinitely.
		t.dialer = &net.Dialer{Timeout: singleDialTimeout}
		dial := t.dialer.DialContext

		if several {
			t.dialer = newDialer()
			t.resolving = &controllerDialer{connect: t.dialer.DialContext, lookup: lookupZoned}
			dial = t.resolving.DialContext
		}

		t.http = &http.Client{
			Transport: &http.Transport{
				DialContext:           dial,
				TLSClientConfig:       spec.TLS,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 0, // a long poll withholds headers on purpose
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}

	return t, nil
}

// newDialer is the node's dialer: bounded, and probing its connections for a
// controller that has gone silent.
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout: dialTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount,
		},
	}
}

// controllerDialer reaches one of several controllers: the name looked up under
// lookupTimeout, then its addresses connected to under the connecting dialer's
// own bound. The addresses of the family the lookup answered first are tried
// in turn, and those of the other family beside them from fallbackDelay on,
// as the standard dialer does, so a family that does not route costs a
// fraction of a second rather than the dial's bound for every address in it.
type controllerDialer struct {
	connect func(ctx context.Context, network, address string) (net.Conn, error)
	lookup  func(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// fallbackDelay is how long the first family's addresses have to themselves
// before the other family's are tried beside them: the standard dialer's.
const fallbackDelay = 300 * time.Millisecond

func (d *controllerDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	if _, err := netip.ParseAddr(host); err == nil {
		return d.connect(ctx, network, address)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	addrs, err := d.lookup(lookupCtx, lookupNetwork(network), host)

	cancel()

	if err != nil {
		return nil, err
	}

	if len(addrs) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}

	// ONE BOUND FOR EVERY ADDRESS, from here, as the standard dialer shares its
	// timeout among the addresses a name answers: five addresses that do not
	// answer cost the dial's three seconds, not fifteen.
	ctx, cancel = context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var primaries, fallbacks []netip.Addr

	for _, addr := range addrs {
		if addr.Unmap().Is4() == addrs[0].Unmap().Is4() {
			primaries = append(primaries, addr)
		} else {
			fallbacks = append(fallbacks, addr)
		}
	}

	if len(fallbacks) == 0 {
		return d.inTurn(ctx, network, primaries, port)
	}

	return d.race(ctx, network, primaries, fallbacks, port)
}

// inTurn connects to each address in turn until one answers, each given its
// share of what is left of the context's deadline.
func (d *controllerDialer) inTurn(ctx context.Context, network string, addrs []netip.Addr, port string) (net.Conn, error) {
	var errs []error

	for i, addr := range addrs {
		attemptCtx, cancel := context.WithDeadline(ctx, partialDeadline(ctx, len(addrs)-i))
		conn, err := d.connect(attemptCtx, network, net.JoinHostPort(addr.Unmap().String(), port))

		cancel()

		if err == nil {
			return conn, nil
		}

		errs = append(errs, err)

		if ctx.Err() != nil {
			break
		}
	}

	return nil, errors.Join(errs...)
}

// race tries the primaries, and the fallbacks beside them once fallbackDelay
// has passed or the primaries have all failed, and answers the first
// connection either makes. The other attempt is cancelled as it returns, and a
// connection it makes after all is closed.
func (d *controllerDialer) race(
	ctx context.Context, network string, primaries, fallbacks []netip.Addr, port string,
) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type attempt struct {
		conn net.Conn
		err  error
	}

	results := make(chan attempt, 2)
	try := func(addrs []netip.Addr) {
		go func() {
			conn, err := d.inTurn(ctx, network, addrs, port)
			results <- attempt{conn, err}
		}()
	}

	try(primaries)

	fallback := time.NewTimer(fallbackDelay)
	defer fallback.Stop()

	started, finished := 1, 0

	var errs []error

	for {
		select {
		case <-fallback.C:
			if started == 1 {
				try(fallbacks)
				started++
			}
		case r := <-results:
			finished++

			if r.err == nil {
				if pending := started - finished; pending > 0 {
					go func() {
						for range pending {
							if late := <-results; late.conn != nil {
								_ = late.conn.Close()
							}
						}
					}()
				}

				return r.conn, nil
			}

			errs = append(errs, r.err)

			if started == 1 {
				try(fallbacks)
				started++

				continue
			}

			if finished == started {
				return nil, errors.Join(errs...)
			}
		}
	}
}

// minimumAttempt is the least an address is given while the deadline allows it,
// the standard dialer's own floor.
const minimumAttempt = 2 * time.Second

// partialDeadline is the deadline of one of remaining addresses still to try
// before ctx's: an equal share of the time left, at least minimumAttempt while
// that much is left, as the standard dialer gives each address.
func partialDeadline(ctx context.Context, remaining int) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(dialTimeout)
	}

	left := time.Until(deadline)
	share := left / time.Duration(remaining)

	if share < minimumAttempt {
		share = min(minimumAttempt, left)
	}

	return time.Now().Add(share)
}

// lookupZoned looks a name up as net.DefaultResolver does, keeping each IPv6
// address's zone, which LookupNetIP drops: a link-local controller named in
// the hosts file is reached only through its interface.
func lookupZoned(ctx context.Context, network, host string) ([]netip.Addr, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	return addrsIn(ips, network), nil
}

// addrsIn is the addresses of a lookup's answer in network ("ip", "ip4" or
// "ip6"), zones kept. The transport dials "tcp", so the lookup asks for both
// families and nothing here is filtered in practice; a dial for one family
// would still wait on the other's query, which is why the filter is no more
// than a filter.
func addrsIn(ips []net.IPAddr, network string) []netip.Addr {
	addrs := make([]netip.Addr, 0, len(ips))

	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip.IP)
		if !ok {
			continue
		}

		addr = addr.Unmap()

		switch {
		case network == "ip4" && !addr.Is4(), network == "ip6" && !addr.Is6():
			continue
		case addr.Is6() && ip.Zone != "":
			addr = addr.WithZone(ip.Zone)
		}

		addrs = append(addrs, addr)
	}

	return addrs
}

// lookupNetwork is the network a name is looked up in for a dial in network.
func lookupNetwork(network string) string {
	switch network {
	case "tcp4":
		return "ip4"
	case "tcp6":
		return "ip6"
	default:
		return "ip"
	}
}

// normaliseBase turns what an operator wrote into a URL requests can be built
// from.
//
// THIS IS WHERE `billet node` WAS BROKEN, and nothing caught it because the
// check that stood here could not fail: url.Parse accepts "127.0.0.1:7717"
// happily — it reads the host as a scheme — so a config-supplied host:port
// passed validation, became the base, and then every single request died at
// construction with "first path segment in URL cannot contain colon". The node
// command could not make one call. Every test dialled an httptest server, whose
// URL already carries a scheme, so the whole suite was green.
func normaliseBase(raw string, secure bool) (string, error) {
	e, err := endpoint.Parse(raw, secure)
	if err != nil {
		return "", fmt.Errorf("nodeclient: control plane address: %w", err)
	}

	return e.String(), nil
}

// Endpoint is the control-plane endpoint this client dials now: the request
// base of the controller its registration was accepted by (the first it names
// until one is), as the one representation. It is the process's request base
// and not an observed peer, so name resolution, translation and redirects are
// outside what it says.
func (c *Client) Endpoint() endpoint.Endpoint { return c.at().endpoint }

// Controllers is how many controllers this client may register with.
func (c *Client) Controllers() int { return len(c.targets) }

// at is the controller every request but a registration goes to.
func (c *Client) at() *target { return c.targets[c.current.Load()] }

// ReplaceTransportForTest installs a transport on every controller of a built
// client and changes nothing else, so a fixture can observe the requests a
// production-built client sends without building its own.
func ReplaceTransportForTest(c *Client, rt http.RoundTripper) {
	for _, t := range c.targets {
		t.http = &http.Client{Transport: rt, Timeout: t.http.Timeout}
	}
}

// BaseForTest reports the URL requests are built from now.
//
// Exported for tests because the failure it guards is invisible from outside:
// a base without a scheme builds a request that never leaves the process.
func (c *Client) BaseForTest() string { return c.at().base }

// Registration is what this node says about itself when it introduces itself.
//
// A STRUCT RATHER THAN A GROWING PARAMETER LIST. Two of these are strings that
// mean unrelated things — a deployment identity and a place — and transposing
// them compiles, runs, and produces a node in the wrong deployment or the wrong
// site rather than an error.
type Registration struct {
	Provider   config.ProviderKind
	GuestOS    []config.GuestOS
	Deployment string
	Site       string
	// VCPU and Memory are what this host contributes.
	VCPU      int
	Memory    config.ByteSize
	EC2Shapes []config.RemoteShape
	// Architectures are what this node's provider can launch. See
	// nodeapi.RegisterRequest.
	Architectures []config.Arch
	// CodeBuildFleet is the reserved-capacity fleet this node's builds run on, or
	// empty for on-demand compute. The control plane refuses a second live node
	// naming the same one, because its capacity is shared.
	CodeBuildFleet string
	// CodeBuildJITParameterPath and CodeBuildRegion are where a codebuild node
	// stages runner registrations, so the control plane can sweep the ones a dead
	// node left behind. Empty for every other backend.
	CodeBuildJITParameterPath string
	CodeBuildRegion           string
	// Instances are the lease ids this host is actually running, and
	// InventoryKnown says the list is complete rather than absent. See
	// nodeapi.RegisterRequest.
	Instances      []string
	InventoryKnown bool
}

// installedDigest reports which manifest produced this binary, or nothing.
//
// AN ABSENCE IS NOT A FAILURE, AND A STALE RECORD IS NOT AN ABSENCE. A host with
// no record is the ordinary case — every package install, every source build,
// every host upgraded before this existed — and it registers exactly as before.
// A record that no longer describes the running binary is a different fact: it
// means something replaced billet without updating what says where billet came
// from, and it is worth one line in the node's log even though the registration
// carries on regardless.
//
// NOTHING HERE REFUSES. What this field decides is how a rollout READS this host,
// and taking a working node out of the fleet over a diagnostic is the trade
// VersionNodeRelease already refused to make.
// READ ONCE, WHEN THE CLIENT IS BUILT, AND HELD. Hashing the executable is a
// ~22MB read and a node re-registers on every reconnect, so asking per
// registration would pay for one answer repeatedly. The answer must also describe
// the bytes THIS PROCESS STARTED WITH: os.Executable resolves to a path on macOS,
// so a binary replaced while the node runs would otherwise be hashed in place of
// the one actually executing — and an updater replacing billet underneath a node
// that is draining is not hypothetical, it is the ordinary upgrade.
func installedDigest() string {
	digest, err := provenance.Installed()

	switch {
	case err == nil:
		return digest

	case errors.Is(err, provenance.ErrNoRecord):
		return ""

	default:
		slog.Default().Warn("this machine's record of which release produced it cannot be used, so "+
			"a rollout cannot prove which bytes this node is running", "error", err)

		return ""
	}
}

// Register introduces this node and learns the timings it must respect.
//
// TO EVERY CONTROLLER IN TURN, from the one it is registered with: the first
// that accepts is the one every later request goes to. Only the controller
// holding the deployment's claim serves the node wire, so a standby refuses the
// connection, and a controller that has lost its claim stops serving it; one
// that answers is the controller. A verdict stops the round where it is given
// (ErrRefused, ErrSuperseded): it is the controller's answer about this node,
// and asking a standby next could only fail to connect. Anything else, an
// address that does not answer included, moves to the next controller.
func (c *Client) Register(ctx context.Context, reg Registration) error {
	start := int(c.current.Load())

	var errs []error

	for i := range c.targets {
		at := (start + i) % len(c.targets)

		err := c.registerWith(ctx, at, reg)
		if err == nil {
			return nil
		}

		if len(c.targets) == 1 || errors.Is(err, ErrRefused) || errors.Is(err, ErrSuperseded) || ctx.Err() != nil {
			return err
		}

		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// registerWith registers with the controller at index at, and on its
// acceptance makes it the one every later request goes to.
func (c *Client) registerWith(ctx context.Context, at int, reg Registration) error {
	var res nodeapi.RegisterResponse

	self := nodeapi.Self()

	err := c.doAt(ctx, c.targets[at], http.MethodPost, "/v1/register", nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		MinVersion: nodeapi.MinVersion,
		// TAKEN FROM THE BINARY, NOT FROM Registration. It is a fact about this
		// build rather than about the host's configuration, and a caller-supplied
		// copy is a second source of it that can drift from the one `billet
		// version` prints.
		Release: version.Version(),
		// FROM THE INSTALLATION, NOT FROM THE BUILD, which is the distinction that
		// makes it worth sending beside Release. version.Version() says what this
		// binary was built as; this says which signed manifest put it here, proved
		// against the bytes that are running. Empty when nothing can say, which a
		// rollout reads as "cannot tell" rather than as a disagreement.
		InstalledDigest: c.installedDigest,
		Incarnation:     c.incarnation,
		Node:            c.node,
		Provider:        reg.Provider,
		GuestOS:         reg.GuestOS,
		Deployment:      reg.Deployment,
		Instances:       reg.Instances,
		InventoryKnown:  reg.InventoryKnown,
		Site:            reg.Site,
		VCPU:            reg.VCPU,
		Memory:          reg.Memory,
		EC2Shapes:       reg.EC2Shapes,
		Architectures:   reg.Architectures,
		CodeBuildFleet:  reg.CodeBuildFleet,

		CodeBuildJITParameterPath: reg.CodeBuildJITParameterPath,
		CodeBuildRegion:           reg.CodeBuildRegion,
	}, &res)
	if err != nil {
		// THE RANGE IS ADDED TO EVERY FAILURE, unconditionally, rather than
		// matched out of the control plane's message. A control plane older than
		// the negotiated wire rejects this body in its STRICT DECODER, before any
		// version check it has can run, so what comes back is
		// `json: unknown field "min_version"` — true, permanent, and unactionable
		// on its own. Beside this node's range it says which side to upgrade.
		return fmt.Errorf("nodeclient: registering with %s at protocol %d (this node speaks %s): %w",
			c.targets[at].base, nodeapi.Version, self, err)
	}

	// THE NEGOTIATED VERSION, CHECKED AGAINST WHAT THIS NODE CAN ACTUALLY SPEAK.
	// A control plane answering outside this range is broken rather than merely
	// old, and no amount of retrying changes what it answers — so this is
	// ErrRefused, which stops the node, and not an error the loop treats as an
	// outage and retries against forever.
	if !self.Speaks(res.Version) {
		return fmt.Errorf(
			"%w: it chose protocol version %d, which this node does not speak (%s)",
			ErrRefused, res.Version, self)
	}

	// TAKEN FROM THE SERVER, not chosen here. The reaper on the other side is what
	// enforces the TTL, so a node that picked its own renewal cadence would be
	// guessing at someone else's deadline.
	ttl := time.Duration(res.LeaseTTLSeconds) * time.Second

	// VALIDATED BEFORE IT IS PUBLISHED. Storing an unusable TTL and then reporting
	// the error would leave a janitor already running on the bad value.
	if ttl <= 0 {
		return fmt.Errorf("nodeclient: control plane reported a lease TTL of %ds, which cannot "+
			"be renewed against", res.LeaseTTLSeconds)
	}

	c.ttl.Store(int64(ttl))
	c.poll.Store(int64(time.Duration(res.PollSeconds) * time.Second))

	// THE GENERATION MOVES WITH THE WIRE, under one lock, so a stale answer
	// decoded in between cannot clear a wire this registration is about to
	// publish or has just published.
	//
	// AND THE CONTROLLER WITH THEM: every request after this one goes to the
	// controller that accepted it, and an answer to one sent before belongs to
	// the generation it was sent under.
	c.regMu.Lock()
	c.generation++
	c.current.Store(int64(at))
	c.wire.Store(int64(res.Version))
	c.imagesAccept = slices.Clone(res.ImagesAccept)
	c.imagesAcceptCarried = res.Version >= nodeapi.VersionImageAcceptance
	c.macOSLimit = nil

	if res.Version >= nodeapi.VersionTartVerified {
		c.macOSLimit = res.MacOSVMLimit
	}

	c.regMu.Unlock()

	return nil
}

// ImageAcceptance is what the last accepted registration said the deployment's
// images.accept names, and false when its wire does not carry it: an answer below
// nodeapi.VersionImageAcceptance is silent, which is not "accepts none".
func (c *Client) ImageAcceptance() ([]string, bool) {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	if !c.imagesAcceptCarried {
		return nil, false
	}

	return slices.Clone(c.imagesAccept), true
}

// MacOSLimit is how many macOS guests the last accepted registration said this node
// may run, and false when its control plane does not say, which is not a limit of zero.
func (c *Client) MacOSLimit() (int, bool) {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	if c.macOSLimit == nil {
		return 0, false
	}

	return *c.macOSLimit, true
}

// WireVersion is the protocol version this node's registration settled on.
//
// Zero until the first registration answers, for the same reason LeaseTTL is:
// the control plane is what chooses it — and zero again once the control plane
// has said it does not know this node, see forgetRegistration.
func (c *Client) WireVersion() int { return int(c.wire.Load()) }

// EverRegistered reports whether any registration of this client was accepted.
// Unlike WireVersion it is not cleared when the plane forgets one, because what
// a forgotten registration was given may still be this process's to account for.
func (c *Client) EverRegistered() bool {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	return c.generation > 0
}

// forgetRegistration records that the control plane no longer knows the
// registration a request was sent under, so nothing that registration decided
// is acted on until a new one answers.
//
// CALLED WHERE THE CODE IS DECODED, on every route, rather than by the loop
// where it happens to branch on the error: the loop checks cancellation before
// it looks at the verdict, so a stop landing beside an "unregistered" answer
// would have kept a wire version negotiated with a control plane that is gone
// — and sent a withdrawal to a replacement that may have no route for it.
//
// ONLY WHILE THAT REGISTRATION IS STILL THE CURRENT ONE. A request that left
// before a re-registration and was answered after it is a fact about the old
// registration; clearing the new one's wire on it would suppress the very
// withdrawal this exists to keep, and the node would go back to being
// forgotten by silence.
//
// THE GENERATION IS THIS CLIENT'S VIEW, NOT THE SERVER'S. A request that left
// while a registration was in flight is stamped with the generation before that
// registration completed here, whatever the server had already decided; so an
// "unregistered" answer to it is ignored, and if the plane has since forgotten
// the node again the client keeps a wire it no longer holds. What that costs is
// one refused withdrawal at the next stop — answered "unregistered" by a
// current plane, or a bare 404 by an older one, which the node reports and
// gives up on — and never anything about capacity. Closing it would mean
// serialising every request against registration, which is the janitor
// waiting on the poll loop; the residual is cheaper than the lock.
//
// THE WIRE VERSION ONLY. The lease TTL and the poll window stay: the janitor is
// renewing on the first, and a registration that fails keeps the node holding
// what it holds.
func (c *Client) forgetRegistration(sentUnder int64) {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	if c.generation == sentUnder {
		c.wire.Store(0)
	}
}

// currentTarget is the controller a request about to be sent goes to, and the
// registration it belongs to, read together under the lock a registration
// publishes both under.
func (c *Client) currentTarget() (*target, int64) {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	return c.at(), c.generation
}

// errNoAnswer is what an error wraps when the controller gave no answer at
// all: the connection failed, was cut, or the request's deadline passed with
// nothing heard. A refusal is an answer, and never wraps it, and neither does
// the caller's own cancellation. A TLS handshake the node cannot complete (a
// certificate it does not trust, or an alert) is counted as no answer: the
// node cannot use that controller, and with another to try it tries it, as a
// node with one controller retries the one it has.
var errNoAnswer = errors.New("nodeclient: the control plane did not answer")

// noAnswer carries a transport's error, and says it is errNoAnswer.
type noAnswer struct{ err error }

func (e noAnswer) Error() string        { return e.err.Error() }
func (e noAnswer) Unwrap() error        { return e.err }
func (e noAnswer) Is(target error) bool { return target == errNoAnswer }

// Incarnation identifies this node process.
func (c *Client) Incarnation() string { return c.incarnation }

// mintIncarnation picks a value no other node process will hold.
//
// RANDOM, NOT A COUNTER OR A TIMESTAMP. A counter starts again at one after a
// restart, and two hosts booted by the same automation at the same second share
// a timestamp — both of which produce collisions in exactly the situation this
// exists to detect.
func mintIncarnation() (string, error) {
	var raw [16]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("nodeclient: mint an incarnation: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// requestTimeout bounds an ordinary request.
//
// Generous enough for a slow control plane doing a database write, short enough
// that a wedged one does not stall the node's whole loop. A heartbeat that takes
// this long has already missed its purpose.
const requestTimeout = 30 * time.Second

// pollSlack is added to the negotiated poll window before the node gives up.
//
// The server closes an idle poll at the window; the node must allow for that
// answer to travel. Cutting at exactly the window would abort healthy polls at
// the moment they were about to return 204.
const pollSlack = 15 * time.Second

// LeaseTTL is how long a lease survives without a heartbeat.
//
// Zero until the first registration answers, because the server is what decides
// it. A caller that renews on this must not start before then.
func (c *Client) LeaseTTL() time.Duration { return time.Duration(c.ttl.Load()) }

// PollWindow is how long a command poll may block.
func (c *Client) PollWindow() time.Duration {
	if poll := time.Duration(c.poll.Load()); poll > 0 {
		return poll
	}

	return 50 * time.Second
}

// Poll waits for one command.
//
// Returns ok=false when the window closed with nothing to do, which is the
// ordinary outcome on an idle fleet and not an error — the same shape the plane
// uses on the other side, for the same reason.
func (c *Client) Poll(ctx context.Context) (nodeapi.Command, bool, error) {
	var cmd nodeapi.Command

	// BOUNDED BY THE NEGOTIATED WINDOW, not by the request timeout. This is the
	// one call that is meant to block, and the only call whose deadline comes
	// from what the server said rather than from a constant here.
	ctx, cancel := context.WithTimeout(ctx, c.PollWindow()+pollSlack)
	defer cancel()

	status, err := c.doStatus(ctx, nil, http.MethodPost, c.nodePath("/poll"), nil, &cmd)
	if err != nil {
		return nodeapi.Command{}, false, err
	}

	if status == http.StatusNoContent {
		return nodeapi.Command{}, false, nil
	}

	return cmd, true, nil
}

// ActionsCacheAllowed reads the control plane's current interception kill switch.
func (c *Client) ActionsCacheAllowed(
	ctx context.Context,
	owner, repository string,
) (bool, error) {
	return c.CacheAllowed(ctx, config.CacheActions, owner, repository)
}

// CacheAllowed reads the control plane's kill switch for one cache.
//
// Below the version that knows kinds, only the Actions cache has a switch to
// ask about, and every other cache is answered allowed without asking: that is
// exactly what the older control plane would enforce.
func (c *Client) CacheAllowed(
	ctx context.Context,
	kind config.CacheKind,
	owner, repository string,
) (bool, error) {
	query := url.Values{"owner": {owner}, "repository": {repository}}
	if kind != config.CacheActions {
		if c.WireVersion() == 0 {
			return false, errUnregistered
		}
		if c.WireVersion() < nodeapi.VersionCacheAuthority {
			return true, nil
		}
		query.Set("kind", string(kind))
	}
	var response nodeapi.CachePolicyResponse
	if err := c.do(ctx, http.MethodGet, c.nodePath("/cache-policy")+"?"+query.Encode(),
		nil, &response); err != nil {
		return false, err
	}

	return response.Allowed, nil
}

// Report tells the control plane what happened to a command.
func (c *Client) Report(ctx context.Context, res nodeapi.CommandResult) error {
	return c.do(ctx, http.MethodPost, c.nodePath("/result"), res, nil)
}

// Withdraw tells the control plane this node will not poll again, so nothing
// more is placed on it.
//
// SENT ONLY BY THE LOOP, once the node holds nothing: a withdrawal removes the
// host from placement and nothing else, and the plane accepts it only from the
// process currently registered under the name. ErrSuperseded and
// ErrUnregistered both mean there is nothing of this process's to withdraw.
func (c *Client) Withdraw(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, c.nodePath("/withdraw"), nodeapi.WithdrawRequest{}, nil)
}

// Bind claims a lease for this node.
func (c *Client) Bind(ctx context.Context, leaseID string, epoch int64, nodeName string) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/bind"),
		nodeapi.BindRequest{Epoch: epoch, Node: nodeName}, nil)
}

// Advance moves a lease to a new phase.
func (c *Client) Advance(ctx context.Context, leaseID string, epoch int64, to alloc.Phase) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/advance"),
		nodeapi.AdvanceRequest{Epoch: epoch, Phase: string(to)}, nil)
}

// Heartbeat renews a lease.
func (c *Client) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/heartbeat"),
		nodeapi.HeartbeatRequest{Epoch: epoch}, nil)
}

// RecordLeaseUsage tells the control plane what the host measured a lease's
// job do.
//
// CHECKED WHERE IT IS EMITTED, for RecordCacheObservation's reason: an older
// plane answers the route with a bare 404, and what the pairing loses is the
// measurement, never a job, so the report is dropped as success.
func (c *Client) RecordLeaseUsage(
	ctx context.Context, leaseID string, epoch int64, measured alloc.JobUsage, series *alloc.UsageSeries,
) error {
	wire := c.WireVersion()
	if wire < nodeapi.VersionJobUsage {
		return nil
	}
	if wire < nodeapi.VersionProcessUsage {
		measured = beforeProcessUsage(measured)
	}
	if wire < nodeapi.VersionMeterEnergy {
		measured = measured.WithoutEnergyFrom(alloc.EnergyMeter)
	}
	switch {
	case wire < nodeapi.VersionSeriesClock:
		series = beforeSeriesClock(series)
	case wire < nodeapi.VersionSeriesGaps:
		series = beforeSeriesGaps(series)
	}
	// AN OLDER PLANE GETS THE REST OF THE REPORT, whose strict decoder would
	// refuse all of it for the counters or the destinations alone.
	if wire < nodeapi.VersionJobCounters {
		measured.Counters = nil
	}
	if wire < nodeapi.VersionJobDestinations {
		measured.Destinations = nil
	}
	if wire < nodeapi.VersionUnansweredFlows && measured.Destinations != nil {
		without := measured.Destinations.WithoutUnanswered()
		measured.Destinations = &without
	}
	if wire < nodeapi.VersionReplyTimes && measured.Destinations != nil {
		without := measured.Destinations.WithoutReplyTimes()
		measured.Destinations = &without
	}

	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/usage"),
		nodeapi.UsageRequest{Epoch: epoch, Usage: measured, Series: series}, nil)
}

// RecordGuestReport tells the control plane what the agent inside a lease's
// guest told this node, the guest's own unverified view.
//
// CHECKED WHERE IT IS EMITTED, for RecordLeaseUsage's reason: below
// VersionGuestReport the plane has no route for it and answers a bare 404, and
// what the pairing loses is the report, never a job, so it is dropped as
// success and said once in the node's log. With no wire negotiated at all the
// version cannot be told, so that is ErrUnregistered for the caller to retry
// after registering, never a drop.
func (c *Client) RecordGuestReport(ctx context.Context, leaseID string, epoch int64,
	report alloc.GuestReport,
) error {
	wire := c.WireVersion()
	if wire == 0 {
		return fmt.Errorf("%w: no wire is negotiated, so whether it carries a guest report "+
			"cannot be told", ErrUnregistered)
	}
	if wire < nodeapi.VersionGuestReport {
		if !c.guestUnsent.Swap(true) {
			slog.Default().Info("the control plane's wire does not carry guest reports, so none is sent",
				"negotiated", wire, "needs", nodeapi.VersionGuestReport)
		}

		return nil
	}

	// IN UTC ON THE WIRE: JSON spells a time in its own zone, so an instant a
	// zone carries past the year 9999 would not encode, and an offset with
	// seconds in it would lose them. A zero time in another zone is spelled
	// as a time that is not zero, so a zero is normalised too, and stays zero.
	// The caller's report is not changed.
	report.FirstReceived, report.LastReceived = report.FirstReceived.UTC(), report.LastReceived.UTC()

	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/guest"),
		nodeapi.GuestReportRequest{Epoch: epoch, Report: report}, nil)
}

// beforeSeriesClock is the series a plane older than VersionSeriesClock can
// keep: a clocked one re-encoded without its clock and gap marks, which that
// plane's validation refuses by codec and would take the whole report down
// with it. A series that cannot be re-encoded is not sent; the summary still
// is. The caller's series is not changed.
func beforeSeriesClock(series *alloc.UsageSeries) *alloc.UsageSeries {
	if series == nil || series.Codec != usage.SeriesCodecClocked && series.Codec != usage.SeriesCodecGaps {
		return series
	}
	data, err := usage.WithoutClock(series.Codec, series.Data, alloc.MaxUsageSeriesBytes)
	if err != nil {
		return nil
	}

	return &alloc.UsageSeries{Codec: usage.SeriesCodec, Data: data}
}

// beforeSeriesGaps is the series a plane older than VersionSeriesGaps can
// keep: one with causes re-encoded with marks alone, which that plane's
// validation would refuse by codec, taking the whole report down with it. A
// series that cannot be re-encoded is not sent; the summary still is. The
// caller's series is not changed.
func beforeSeriesGaps(series *alloc.UsageSeries) *alloc.UsageSeries {
	if series == nil || series.Codec != usage.SeriesCodecGaps {
		return series
	}
	data, err := usage.WithoutCauses(series.Data, alloc.MaxUsageSeriesBytes)
	if err != nil {
		return nil
	}

	return &alloc.UsageSeries{Codec: usage.SeriesCodecClocked, Data: data}
}

// beforeProcessUsage says what a report can say to a plane older than
// VersionProcessUsage, whose strict validation knows neither the oom group nor
// the process energy source. It says less, never something else: process energy
// becomes unmeasured, and memory without an OOM count becomes unmeasured whole,
// because that plane reads measured memory as a measured count.
func beforeProcessUsage(u alloc.JobUsage) alloc.JobUsage {
	unmeasured := slices.Clone(u.Unmeasured)
	mark := func(group string) {
		if !slices.Contains(unmeasured, group) {
			unmeasured = append(unmeasured, group)
		}
	}
	if u.EnergySource == alloc.EnergyProcess {
		u.EnergyActiveMicrojoules, u.EnergyIdleMicrojoules, u.EnergySource = 0, 0, ""
		mark(alloc.UsageEnergy)
	}
	if slices.Contains(unmeasured, alloc.UsageOOM) {
		unmeasured = slices.DeleteFunc(unmeasured, func(g string) bool { return g == alloc.UsageOOM })
		u.MemoryPeakBytes, u.OOMKills = 0, 0
		mark(alloc.UsageMemory)
	}
	slices.Sort(unmeasured)
	u.Unmeasured = unmeasured

	return u
}

// MarkFailure records why a running lease is destined to fail before teardown.
func (c *Client) MarkFailure(ctx context.Context, leaseID string, epoch int64, reason string) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/failure"),
		nodeapi.MarkFailureRequest{Epoch: epoch, Reason: reason}, nil)
}

// RecordCacheObservation tells the control plane what the cache did for a
// lease's job.
//
// CHECKED WHERE IT IS EMITTED. An older control plane has no route for this and
// answers a bare 404, which would read here as a decode failure on every cache
// request of every job on the node. A pairing below the version loses one
// diagnostic column and nothing else, so the observation is dropped as success
// rather than reported as an error the caller would resend at teardown; the
// registration log line already names the negotiated version.
func (c *Client) RecordCacheObservation(
	ctx context.Context, leaseID string, epoch int64, obs alloc.CacheObservation,
) error {
	if c.WireVersion() < nodeapi.VersionCacheObservation {
		return nil
	}
	request := nodeapi.CacheObservationRequest{
		Epoch:           epoch,
		ImageCache:      string(obs.ImageCache),
		CacheGeneration: obs.CacheGeneration,
		ActionsCache:    string(obs.ActionsCache),
	}
	// THE BUILD CACHES GO ONLY TO A PLANE THAT KNOWS THEM, whose decoder would
	// otherwise refuse the image and Actions halves along with them.
	if c.WireVersion() >= nodeapi.VersionCacheAuthority {
		request.StickyCache, request.GitCache = string(obs.Sticky), string(obs.Git)
		request.BazelCache, request.GoCache = string(obs.Bazel), string(obs.Go)
	}
	if c.WireVersion() >= nodeapi.VersionNpmObservation {
		request.NpmCache = string(obs.Npm)
	}
	if request == (nodeapi.CacheObservationRequest{Epoch: epoch}) {
		return nil
	}

	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/cache"), request, nil)
}

// errUnregistered says the client has not negotiated a wire with the control
// plane yet, so a version-dependent question cannot be answered, only retried.
var errUnregistered = errors.New("nodeclient: not registered with the control plane yet")

// CacheAuthority asks what a lease's running job may do with a cache.
//
// Below the version that serves it, the answer is the zero authority, which
// writes nothing, without asking: an older control plane cannot decide one.
func (c *Client) CacheAuthority(ctx context.Context, leaseID string) (dispatch.CacheAuthority, error) {
	if c.WireVersion() == 0 {
		return dispatch.CacheAuthority{}, errUnregistered
	}
	if c.WireVersion() < nodeapi.VersionCacheAuthority {
		return dispatch.CacheAuthority{}, nil
	}

	var response nodeapi.CacheAuthorityResponse
	if err := c.do(ctx, http.MethodGet, c.leasePath(leaseID, "/cache-authority"), nil,
		&response); err != nil {
		return dispatch.CacheAuthority{}, err
	}

	return ServerCacheAuthority(&response.Authority), nil
}

// Resize changes an EC2 lease's charged shape before the provider attempts it.
func (c *Client) Resize(
	ctx context.Context, leaseID string, epoch int64, instanceType string,
	vcpu int, memory config.ByteSize,
) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/resize"),
		nodeapi.ResizeRequest{
			Epoch: epoch, InstanceType: instanceType, VCPU: vcpu, Memory: memory,
		}, nil)
}

// Release ends a lease with a terminal outcome.
func (c *Client) Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/release"),
		nodeapi.ReleaseRequest{Epoch: epoch, Outcome: string(outcome)}, nil)
}

// Lease reads a lease.
func (c *Client) Lease(ctx context.Context, leaseID string) (*alloc.Lease, error) {
	var res nodeapi.LeaseResponse

	if err := c.do(ctx, http.MethodGet, c.leasePath(leaseID, ""), nil, &res); err != nil {
		return nil, err
	}

	if res.Lease == nil {
		return nil, fmt.Errorf("%w: %s", alloc.ErrLeaseNotFound, leaseID)
	}

	return res.Lease, nil
}

// LaunchedLeaseIDs reports which leases this node is believed to have launched.
func (c *Client) LaunchedLeaseIDs(ctx context.Context, nodeName string) (map[string]bool, error) {
	res, err := c.launched(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	if res.LeaseIDs == nil {
		return map[string]bool{}, nil
	}

	return res.LeaseIDs, nil
}

// QuarantinedLeaseIDs reports the leases holding capacity for compute the
// control plane cannot account for on this node.
func (c *Client) QuarantinedLeaseIDs(ctx context.Context, nodeName string) (map[string]bool, error) {
	res, err := c.launched(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	if res.Quarantined == nil {
		return map[string]bool{}, nil
	}

	return res.Quarantined, nil
}

// Reconcile tells the control plane what this host is running, so it can free
// capacity held for compute that is gone.
func (c *Client) Reconcile(ctx context.Context, nodeName string, running []string) (int, error) {
	var res nodeapi.ReconcileResponse

	err := c.do(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(nodeName)+"/reconcile",
		nodeapi.ReconcileRequest{Instances: running}, &res)

	return res.Freed, err
}

func (c *Client) launched(ctx context.Context, nodeName string) (nodeapi.LaunchedResponse, error) {
	var res nodeapi.LaunchedResponse

	err := c.do(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(nodeName)+"/launched", nil, &res)

	return res, err
}

// Describe finds a tier's scale set, via the control plane.
//
// The node cannot ask GitHub itself: it holds no App key, by design. See the JIT
// half of internal/nodeapi for why that is a security property rather than an
// inconvenience.
func (c *Client) Describe(ctx context.Context, name, group string) (*node.Set, []string, error) {
	var res nodeapi.DescribeResponse

	err := c.do(ctx, http.MethodPost, c.nodePath("/describe"),
		nodeapi.DescribeRequest{Name: name, Group: group}, &res)
	if err != nil {
		return nil, nil, err
	}

	if !res.Found {
		// A NIL SET IS THE CONTRACT for "there is no such scale set", and the
		// runner treats it as a reason to stop. Returning a zero-valued set would
		// have it launch against scale set 0.
		return nil, res.Names, nil
	}

	return &node.Set{ID: res.ID, Name: res.Name}, res.Names, nil
}

// ValidateTrustedRunnerGroup asks the credential-holding control plane to
// revalidate the workflow boundary immediately before this node requests a JIT
// registration. The control plane repeats the check against the entitled tier
// while minting, so a compromised node cannot substitute this request.
func (c *Client) ValidateTrustedRunnerGroup(
	ctx context.Context, tier, group string, workflows []string,
) error {
	return c.do(ctx, http.MethodPost, c.nodePath("/trusted-runner-group"),
		trustedRunnerGroupRequest(c.WireVersion(), tier, group, workflows), nil)
}

// trustedRunnerGroupRequest carries the tier only on a wire that knows the
// field. The plane decodes with DisallowUnknownFields, so a node that
// negotiated an older version with an older plane (a rollback, or a node
// upgraded ahead of its controller) would otherwise have every trusted launch
// refused as a malformed body; on that wire the plane serves one target and
// resolves the tier itself.
func trustedRunnerGroupRequest(wire int, tier, group string, workflows []string,
) nodeapi.TrustedRunnerGroupRequest {
	req := nodeapi.TrustedRunnerGroupRequest{Group: group, Workflows: workflows}
	if wire >= nodeapi.VersionTargetedRunnerGroup {
		req.Tier = tier
	}

	return req
}

// JITConfig asks the control plane to mint one runner registration.
func (c *Client) JITConfig(
	ctx context.Context, scaleSetID int, runnerName, workFolder string,
) (node.Registration, error) {
	var res nodeapi.JITResponse

	err := c.do(ctx, http.MethodPost, c.nodePath("/jit"), nodeapi.JITRequest{
		ScaleSetID: scaleSetID,
		RunnerName: runnerName,
		WorkFolder: workFolder,
	}, &res)
	if err != nil {
		return nil, err
	}

	if res.Config == "" {
		return nil, fmt.Errorf(
			"nodeclient: the control plane returned an empty registration for %q, which would "+
				"start an instance that can never register", runnerName)
	}

	return &registration{config: res.Config, id: res.RunnerID, name: res.RunnerName}, nil
}

// RemoveRunner asks the credential-holding control plane to withdraw routing.
func (c *Client) RemoveRunner(
	ctx context.Context, leaseID string, runnerID int64, runnerName string,
) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/runner/remove"), nodeapi.RemoveRunnerRequest{
		RunnerID: runnerID, RunnerName: runnerName,
	}, nil)
}

// EnsureRunnerRemoved resolves a restart-surviving registration by its lease
// and withdraws it before recovered custody touches compute.
func (c *Client) EnsureRunnerRemoved(ctx context.Context, leaseID string) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/runner/remove"),
		nodeapi.RemoveRunnerRequest{}, nil)
}

// RecoverRunner asks the control plane to preserve a proven-busy legacy
// registration or retire it before quarantined compute is touched.
func (c *Client) RecoverRunner(ctx context.Context, leaseID, _ string, _ int64,
	runnerName string,
) (node.RunnerRecovery, error) {
	var res nodeapi.RecoverRunnerResponse
	err := c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/runner/recover"),
		nodeapi.RecoverRunnerRequest{RunnerName: runnerName}, &res)
	if err != nil {
		return "", err
	}
	switch node.RunnerRecovery(res.State) {
	case node.RunnerRecoveryTracked, node.RunnerRecoveryBusy, node.RunnerRecoveryRetired:
		return node.RunnerRecovery(res.State), nil
	default:
		return "", fmt.Errorf("nodeclient: the control plane returned unknown runner recovery state %q",
			res.State)
	}
}

// registration is a minted registration whose config is a CREDENTIAL.
//
// The field is unexported and reachable only through Config(), so printing the
// value with %v or %+v yields a struct with no visible secret. That is a small
// thing until somebody logs the registration while debugging a launch, which is
// exactly when it would happen.
type registration struct {
	config string
	id     int64
	name   string
}

func (r *registration) Config() string     { return r.config }
func (r *registration) RunnerName() string { return r.name }
func (r *registration) ID() int64          { return r.id }

// String keeps the credential out of anything that formats this value.
func (r *registration) String() string {
	return "nodeclient.registration{runner:" + r.name + ", config:REDACTED}"
}

func (c *Client) nodePath(suffix string) string {
	return "/v1/nodes/" + url.PathEscape(c.node) + suffix
}

func (c *Client) leasePath(leaseID, suffix string) string {
	return c.nodePath("/leases/" + url.PathEscape(leaseID) + suffix)
}

// do performs an ordinary request under the standard deadline.
//
// Every caller but Poll goes through here, which is what makes the bound
// universal rather than something each call site remembers.
func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	return c.doAt(ctx, nil, method, path, body, into)
}

// doAt is do against one controller, or the current one for nil.
func (c *Client) doAt(ctx context.Context, at *target, method, path string, body, into any) error {
	ctx, cancel := context.WithTimeout(ctx, c.reqTimeout)
	defer cancel()

	_, err := c.doStatus(ctx, at, method, path, body, into)

	return err
}

// doStatus performs one request, to the controller at or the current one for
// nil, and translates the answer.
func (c *Client) doStatus(ctx context.Context, at *target, method, path string, body, into any) (int, error) {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("nodeclient: encode %s: %w", path, err)
		}

		reader = bytes.NewReader(raw)
	}

	// THE CONTROLLER AND THE REGISTRATION THE REQUEST IS SENT UNDER, read
	// together, so an answer is attributed to the registration whose controller
	// gave it. See forgetRegistration.
	current, sentUnder := c.currentTarget()
	if at == nil {
		at = current
	}

	req, err := http.NewRequestWithContext(ctx, method, at.base+path, reader)
	if err != nil {
		return 0, fmt.Errorf("nodeclient: build %s: %w", path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// ON EVERY REQUEST, including the ones with no body. The registration claims
	// an incarnation; each request after it proves the claim is still held by the
	// process that made it, which is what lets the control plane tell a restart
	// from a second host wearing the same name.
	req.Header.Set(nodeapi.HeaderIncarnation, c.incarnation)

	resp, err := at.http.Do(req)
	if err != nil {
		// A CANCELLATION IS THE CALLER'S, and an error that came with a response
		// (a redirect refused, whose body the client has already closed) came
		// from a controller that answered: neither says the controller is gone.
		if resp != nil || errors.Is(err, context.Canceled) {
			return 0, fmt.Errorf("nodeclient: %s %s%s: %w", method, at.base, path, err)
		}

		// AND ITS IDLE CONNECTIONS ARE CLOSED, where the node knows another
		// controller: one opened before a partition is ended only by its own
		// keepalive probes, and a request that borrowed it would wait out its
		// whole deadline where a new dial gives up in seconds.
		if len(c.targets) > 1 {
			at.http.CloseIdleConnections()
		}

		// NO ANSWER, as distinct from a refusal: the controller was not reached,
		// or stopped answering, which is what a node with another controller to
		// try acts on (errNoAnswer).
		return 0, fmt.Errorf("nodeclient: %s %s%s: %w", method, at.base, path, noAnswer{err})
	}

	defer func() {
		// DRAINED BEFORE CLOSING so the connection can be reused. A node holds one
		// control plane for its whole life and makes a heartbeat-rate stream of
		// small requests; leaking a connection per call would eventually exhaust
		// the ports on a busy host.
		//
		// A drain that fails changes nothing the caller can act on — the response
		// has already been read or has already failed — so it is deliberately not
		// propagated. Closing still happens either way.
		if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); err != nil {
			_ = err
		}

		if err := resp.Body.Close(); err != nil {
			_ = err
		}
	}()

	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}

	if resp.StatusCode >= 300 {
		return resp.StatusCode, c.decodeErr(resp, sentUnder)
	}

	if into != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into); err != nil {
			return resp.StatusCode, fmt.Errorf("nodeclient: decode %s: %w", path, err)
		}
	}

	return resp.StatusCode, nil
}

// decodeErr turns a refusal into an error the node can branch on.
//
// THE CODE IS WHAT MATTERS. A fenced lease means stop — something else owns it —
// and must surface as alloc.ErrFenced so the runner's existing handling applies
// unchanged. That the failure arrived over a network is an implementation
// detail the runner should never have to know.
func (c *Client) decodeErr(resp *http.Response, sentUnder int64) error {
	var body nodeapi.ErrorResponse

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return fmt.Errorf("nodeclient: %s (and its error body was unreadable: %w)",
			resp.Status, err)
	}

	switch body.Code {
	case nodeapi.CodeRefused:
		return fmt.Errorf("%w: %s", ErrRefused, body.Message)
	case nodeapi.CodeSuperseded:
		return fmt.Errorf("%w: %s", ErrSuperseded, body.Message)
	case nodeapi.CodeUnauthenticated:
		return fmt.Errorf("%w: %s", ErrUnauthenticated, body.Message)
	case nodeapi.CodeCustody:
		return fmt.Errorf("%w: %s", dispatch.ErrCustody, body.Message)
	case nodeapi.CodeFenced:
		return fmt.Errorf("%w: %s", alloc.ErrFenced, body.Message)
	case nodeapi.CodeNotFound:
		return fmt.Errorf("%w: %s", alloc.ErrLeaseNotFound, body.Message)
	case nodeapi.CodeNoCapacity:
		return fmt.Errorf("%w: %s", alloc.ErrNoCapacity, body.Message)
	case nodeapi.CodeForceRelease:
		return fmt.Errorf("%w: %s", alloc.ErrForceRelease, body.Message)
	case nodeapi.CodeUnregistered:
		// THE CLIENT LEARNS IT FIRST, before any caller can branch on it — and
		// only about the registration this request was sent under.
		c.forgetRegistration(sentUnder)

		return fmt.Errorf("%w: %s", ErrUnregistered, body.Message)
	default:
		return fmt.Errorf("nodeclient: control plane refused: %s (%s)", body.Message, resp.Status)
	}
}

// Renew asks the control plane to sign a new certificate for this node.
//
// The key is generated HERE and stays here; only the request and the signature
// cross the wire. Authenticated by the certificate being replaced, so it grants
// nothing new — a host that can already act as this node asks to keep doing so.
func (c *Client) Renew(ctx context.Context, name string) ([]byte, []byte, []byte, error) {
	csrPEM, key, err := wirecert.NewNodeCSR(name)
	if err != nil {
		return nil, nil, nil, err
	}

	var res nodeapi.RenewResponse

	if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(c.node)+"/renew",
		nodeapi.RenewRequest{CSRPEM: string(csrPEM)}, &res); err != nil {
		return nil, nil, nil, err
	}

	if res.CertPEM == "" {
		return nil, nil, nil, errors.New("nodeclient: the control plane signed nothing")
	}

	// THE AUTHORITY TRAVELS WITH THE CERTIFICATE, which is what lets a CA
	// rotation reach a node: during an overlap this is a bundle holding both.
	return []byte(res.CertPEM), key, []byte(res.CAPEM), nil
}
