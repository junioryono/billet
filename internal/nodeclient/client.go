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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
)

// ErrUnregistered means the control plane does not know this node.
//
// The node's answer is to register again, not to retry the same call: this is
// what a restarted control plane looks like from here, and retrying a lease
// write it will keep refusing is an infinite loop that fixes nothing.
var ErrUnregistered = errors.New("nodeclient: the control plane does not know this node")

// Client talks to a control plane.
type Client struct {
	base       string
	node       string
	http       *http.Client
	ttl        time.Duration
	poll       time.Duration
	reqTimeout time.Duration
}

// Options configures a Client.
type Options struct {
	// Base is the control plane's address, e.g. http://127.0.0.1:7717.
	Base string
	// Node is this host's name, which must match its entry in the server's
	// fleet configuration.
	Node string
	// HTTP is the transport. A caller supplies one so timeouts and, later, mTLS
	// are decided once rather than here.
	HTTP *http.Client
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

	if _, err := url.Parse(opts.Base); err != nil {
		return nil, fmt.Errorf("nodeclient: control plane address %q: %w", opts.Base, err)
	}

	c := &Client{
		base:       strings.TrimSuffix(opts.Base, "/"),
		node:       opts.Node,
		http:       opts.HTTP,
		reqTimeout: opts.RequestTimeout,
	}

	if c.reqTimeout <= 0 {
		c.reqTimeout = requestTimeout
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
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 0, // a long poll withholds headers on purpose
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}

	return c, nil
}

// Register introduces this node and learns the timings it must respect.
func (c *Client) Register(
	ctx context.Context,
	provider config.ProviderKind,
	guestOS []config.GuestOS,
	deployment string,
) error {
	var res nodeapi.RegisterResponse

	err := c.do(ctx, http.MethodPost, "/v1/register", nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		Node:       c.node,
		Provider:   provider,
		GuestOS:    guestOS,
		Deployment: deployment,
	}, &res)
	if err != nil {
		return err
	}

	if res.Version != nodeapi.Version {
		return fmt.Errorf(
			"nodeclient: control plane answered with protocol version %d, this node speaks %d",
			res.Version, nodeapi.Version)
	}

	// TAKEN FROM THE SERVER, not chosen here. The reaper on the other side is what
	// enforces the TTL, so a node that picked its own renewal cadence would be
	// guessing at someone else's deadline.
	c.ttl = time.Duration(res.LeaseTTLSeconds) * time.Second
	c.poll = time.Duration(res.PollSeconds) * time.Second

	if c.ttl <= 0 {
		return fmt.Errorf("nodeclient: control plane reported a lease TTL of %ds, which cannot "+
			"be renewed against", res.LeaseTTLSeconds)
	}

	return nil
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
func (c *Client) LeaseTTL() time.Duration { return c.ttl }

// PollWindow is how long a command poll may block.
func (c *Client) PollWindow() time.Duration {
	if c.poll <= 0 {
		return 50 * time.Second
	}

	return c.poll
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

// Report tells the control plane what happened to a command.
func (c *Client) Report(ctx context.Context, res nodeapi.CommandResult) error {
	return c.do(ctx, http.MethodPost, c.nodePath("/result"), res, nil)
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
	var res nodeapi.LaunchedResponse

	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(nodeName)+"/launched", nil, &res); err != nil {
		return nil, err
	}

	if res.LeaseIDs == nil {
		return map[string]bool{}, nil
	}

	return res.LeaseIDs, nil
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

	return &registration{config: res.Config, name: res.RunnerName}, nil
}

// registration is a minted registration whose config is a CREDENTIAL.
//
// The field is unexported and reachable only through Config(), so printing the
// value with %v or %+v yields a struct with no visible secret. That is a small
// thing until somebody logs the registration while debugging a launch, which is
// exactly when it would happen.
type registration struct {
	config string
	name   string
}

func (r *registration) Config() string     { return r.config }
func (r *registration) RunnerName() string { return r.name }

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
		return resp.StatusCode, c.decodeErr(resp)
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
func (c *Client) decodeErr(resp *http.Response) error {
	var body nodeapi.ErrorResponse

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return fmt.Errorf("nodeclient: %s (and its error body was unreadable: %w)",
			resp.Status, err)
	}

	switch body.Code {
	case nodeapi.CodeFenced:
		return fmt.Errorf("%w: %s", alloc.ErrFenced, body.Message)
	case nodeapi.CodeNotFound:
		return fmt.Errorf("%w: %s", alloc.ErrLeaseNotFound, body.Message)
	case nodeapi.CodeUnregistered:
		return fmt.Errorf("%w: %s", ErrUnregistered, body.Message)
	default:
		return fmt.Errorf("nodeclient: control plane refused: %s (%s)", body.Message, resp.Status)
	}
}
