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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/server"
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
	base string
	node string
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
	http        *http.Client
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
}

// New builds a Client. It does not dial; Register does.
func New(opts Options) (*Client, error) {
	if opts.Base == "" {
		return nil, errors.New("nodeclient: a control plane address is required")
	}

	if opts.Node == "" {
		return nil, errors.New("nodeclient: a node name is required")
	}

	base, err := normaliseBase(opts.Base, opts.TLS != nil)
	if err != nil {
		return nil, err
	}

	incarnation, err := mintIncarnation()
	if err != nil {
		return nil, err
	}

	c := &Client{
		base:        base,
		node:        opts.Node,
		incarnation: incarnation,
		http:        opts.HTTP,
		reqTimeout:  opts.RequestTimeout,
		// READ HERE, ONCE, FOR THE LIFE OF THIS NODE. See installedDigest.
		installedDigest: installedDigest(),
	}

	if c.reqTimeout <= 0 {
		c.reqTimeout = requestTimeout
	}

	// A SUPPLIED CLIENT KEEPS ITS OWN TRANSPORT. Reaching into someone else's
	// http.Client to install a TLS config would silently change every other
	// request that client makes, and the caller that passed it in is the one who
	// knows whether that is wanted.
	if c.http != nil && opts.TLS != nil {
		return nil, errors.New(
			"nodeclient: both an HTTP client and a TLS config were supplied; the TLS config " +
				"would have to be installed into a transport this package does not own")
	}

	if c.http == nil {
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
		c.http = &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSClientConfig:       opts.TLS,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 0, // a long poll withholds headers on purpose
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}

	return c, nil
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
	raw = strings.TrimSuffix(raw, "/")

	scheme := "http"
	if secure {
		scheme = "https"
	}

	if !strings.Contains(raw, "://") {
		raw = scheme + "://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("nodeclient: control plane address %q: %w", raw, err)
	}

	if u.Host == "" {
		return "", fmt.Errorf(
			"nodeclient: control plane address %q names no host", raw)
	}

	switch u.Scheme {
	case "http":
		// A CERTIFICATE THAT WOULD NEVER BE PRESENTED IS A CONFIGURATION ERROR, not
		// a fallback. The handshake would fail anyway against a plane that requires
		// one; refusing here says why, instead of leaving an operator reading TLS
		// errors from a node they believed was configured for TLS.
		if secure {
			return "", fmt.Errorf(
				"nodeclient: control plane address %q is http, but this node has a certificate "+
					"to present; drop the scheme or write https", raw)
		}
	case "https":
		if !secure {
			return "", fmt.Errorf(
				"nodeclient: control plane address %q is https, but this node has no certificate "+
					"to present, so the control plane will reject the handshake", raw)
		}
	default:
		return "", fmt.Errorf(
			"nodeclient: control plane address %q must be http or https", raw)
	}

	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/"), nil
}

// BaseForTest reports the URL requests are built from.
//
// Exported for tests because the failure it guards is invisible from outside:
// a base without a scheme builds a request that never leaves the process.
func (c *Client) BaseForTest() string { return c.base }

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
func (c *Client) Register(ctx context.Context, reg Registration) error {
	var res nodeapi.RegisterResponse

	self := nodeapi.Self()

	err := c.do(ctx, http.MethodPost, "/v1/register", nodeapi.RegisterRequest{
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
		return fmt.Errorf("nodeclient: registering at protocol %d (this node speaks %s): %w",
			nodeapi.Version, self, err)
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
	c.regMu.Lock()
	c.generation++
	c.wire.Store(int64(res.Version))
	c.regMu.Unlock()

	return nil
}

// WireVersion is the protocol version this node's registration settled on.
//
// Zero until the first registration answers, for the same reason LeaseTTL is:
// the control plane is what chooses it — and zero again once the control plane
// has said it does not know this node, see forgetRegistration.
func (c *Client) WireVersion() int { return int(c.wire.Load()) }

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

// currentGeneration is the registration a request about to be sent belongs to.
func (c *Client) currentGeneration() int64 {
	c.regMu.Lock()
	defer c.regMu.Unlock()

	return c.generation
}

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

	status, err := c.doStatus(ctx, http.MethodPost, c.nodePath("/poll"), nil, &cmd)
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
	query := url.Values{"owner": {owner}, "repository": {repository}}
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

// MarkFailure records why a running lease is destined to fail before teardown.
func (c *Client) MarkFailure(ctx context.Context, leaseID string, epoch int64, reason string) error {
	return c.do(ctx, http.MethodPost, c.leasePath(leaseID, "/failure"),
		nodeapi.MarkFailureRequest{Epoch: epoch, Reason: reason}, nil)
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
	ctx context.Context, group string, workflows []string,
) error {
	return c.do(ctx, http.MethodPost, c.nodePath("/trusted-runner-group"),
		nodeapi.TrustedRunnerGroupRequest{Group: group, Workflows: workflows}, nil)
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
	ctx, cancel := context.WithTimeout(ctx, c.reqTimeout)
	defer cancel()

	_, err := c.doStatus(ctx, method, path, body, into)

	return err
}

// doStatus performs one request and translates the answer.
func (c *Client) doStatus(ctx context.Context, method, path string, body, into any) (int, error) {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("nodeclient: encode %s: %w", path, err)
		}

		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
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

	// CAPTURED BEFORE THE REQUEST LEAVES, so the answer can be attributed to the
	// registration it is actually about. See forgetRegistration.
	sentUnder := c.currentGeneration()

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("nodeclient: %s %s: %w", method, path, err)
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
		return fmt.Errorf("%w: %s", server.ErrCustody, body.Message)
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
