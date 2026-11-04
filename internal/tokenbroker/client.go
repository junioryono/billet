package tokenbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/junioryono/billet/internal/github"
)

const (
	// clientRequestFor bounds one question to the broker, end to end.
	clientRequestFor = 30 * time.Second

	// clientRefreshAt is when a source asks for a new token. A failure then is no
	// failure yet: the token it holds stays good until tokenLastUsable, which rides a
	// broker's restart out with nothing else noticing.
	clientRefreshAt = 5 * time.Minute
	tokenLastUsable = time.Minute

	// maxAnswer bounds what is read of the broker's answer.
	maxAnswer = 1 << 20
)

// Client is one control plane's connection to a broker: its key, the broker's
// roots, and whose deployment it believes this is.
type Client struct {
	url  string
	http *http.Client
	// identity is this host's deployment, read when a token arrives, so an identity a
	// first start mints later is still compared. Not found is not a mismatch.
	identity func() (string, bool, error)
}

// ClientOptions builds a Client.
type ClientOptions struct {
	URL string
	// Certificate is what ClientCertificate made of this host's key.
	Certificate tls.Certificate
	// Roots verifies the broker, nil meaning the system's.
	Roots    *x509.CertPool
	Identity func() (string, bool, error)
}

// NewClient builds the connection. Nothing is dialled until a token is asked for.
func NewClient(opts ClientOptions) *Client {
	return &Client{
		url: opts.URL,
		http: &http.Client{
			Timeout: clientRequestFor,
			// A REDIRECT WOULD CARRY THE QUESTION SOMEWHERE THE OPERATOR DID NOT NAME, so
			// it is an answer to report rather than one to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 25 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
				TLSClientConfig: &tls.Config{
					Certificates: []tls.Certificate{opts.Certificate}, RootCAs: opts.Roots, MinVersion: tls.VersionTLS13,
				},
			},
		},
		identity: opts.Identity,
	}
}

// Tokens is the token source for one target of this deployment.
func (c *Client) Tokens(target github.Target, appID, installationID int64) github.TokenSource {
	return brokerTokens{&brokerState{
		client: c, target: target, appID: appID, installationID: installationID, slot: make(chan struct{}, 1),
	}}
}

// brokerTokens is one target's tokens from a broker: a handle on one shared state, so
// its rendering methods sit on a value receiver with nothing to copy.
type brokerTokens struct{ *brokerState }

type brokerState struct {
	client         *Client
	target         github.Target
	appID          int64
	installationID int64

	// slot admits one question at a time. A channel rather than a mutex so a caller
	// behind a stalled broker gives up when its own context does.
	slot  chan struct{}
	token github.Token
}

// InstallationToken answers the token this source holds or asks the broker for a new
// one. A broker that cannot answer costs nothing while the held token is still good.
func (b brokerTokens) InstallationToken(ctx context.Context) (github.Token, error) {
	select {
	case b.slot <- struct{}{}:
	case <-ctx.Done():
		return github.Token{}, fmt.Errorf("%w: waiting for another question to the broker: %w",
			github.ErrTokenUnavailable, ctx.Err())
	}
	defer func() { <-b.slot }()

	if b.token.Bearer() != "" && time.Until(b.token.ExpiresAt()) > clientRefreshAt {
		return b.token, nil
	}

	token, err := b.ask(ctx)
	if err == nil {
		b.token = token

		return token, nil
	}

	// A DEFINITE REFUSAL IS REPORTED however long the old token has left: a deployment
	// the broker no longer knows should learn so now. Silence is what gets ridden out.
	if errors.Is(err, github.ErrTokenUnavailable) && b.token.Bearer() != "" &&
		time.Until(b.token.ExpiresAt()) > tokenLastUsable {
		slog.Default().Warn("the token broker did not answer; keeping the installation token held until it does",
			"target", b.target.Path(), "expires", b.token.ExpiresAt().Format(time.RFC3339), "error", err)

		return b.token, nil
	}

	return github.Token{}, err
}

// ask puts the question to the broker and classifies what comes back. Every failure
// to get an answer is could-not-tell, and no answer of the broker's reads as the
// installation being gone but the one that says so.
func (b brokerTokens) ask(ctx context.Context) (github.Token, error) {
	request, err := json.Marshal(tokenRequest{AppID: b.appID, InstallationID: b.installationID, Target: b.target.Path()})
	if err != nil {
		return github.Token{}, fmt.Errorf("%w: encode the request: %w", github.ErrTokenUnavailable, err)
	}

	ctx, cancel := context.WithTimeout(ctx, clientRequestFor)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.client.url+tokenRoute, bytes.NewReader(request))
	if err != nil {
		return github.Token{}, fmt.Errorf("%w: build the request: %w", github.ErrTokenUnavailable, err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.http.Do(req)
	if err != nil {
		return github.Token{}, fmt.Errorf("%w: ask the broker: %w", github.ErrTokenUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return github.Token{}, fmt.Errorf("%w: read the broker's answer: %w", github.ErrTokenUnavailable, err)
	}

	if resp.StatusCode != http.StatusOK {
		return github.Token{}, b.refusal(resp.StatusCode, body)
	}

	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		// NEVER QUOTED: the body may hold the token this failed to read.
		return github.Token{}, fmt.Errorf("%w: the broker's answer was not one", github.ErrTokenUnavailable)
	}

	expiresAt, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if out.Token == "" || err != nil {
		return github.Token{}, fmt.Errorf("%w: the broker's answer was incomplete", github.ErrTokenUnavailable)
	}

	if time.Until(expiresAt) <= tokenLastUsable {
		return github.Token{}, fmt.Errorf("%w: the broker's token expires at %s", github.ErrTokenUnavailable, out.ExpiresAt)
	}

	if err := b.isOurs(out.Deployment); err != nil {
		return github.Token{}, err
	}

	return github.NewToken(out.Token, expiresAt, out.Permissions), nil
}

// isOurs refuses a token minted for another deployment: this host's key is registered
// under an identity that is not this host's, and its tokens are someone else's.
// Reading the identity failing is not a mismatch. A first start's identity may not
// exist yet, which is not one either.
func (b brokerTokens) isOurs(deployment string) error {
	if b.client.identity == nil {
		return nil
	}

	ours, found, err := b.client.identity()
	if err != nil {
		return fmt.Errorf("%w: read this host's deployment identity: %w", github.ErrTokenUnavailable, err)
	}

	if found && ours != deployment {
		return fmt.Errorf("%w: the broker knows this host's key as deployment %s, and this host is deployment %s; "+
			"fix the registration", github.ErrTokenRefused, deployment, ours)
	}

	return nil
}

// refusal reads a status the broker answered with.
func (b brokerTokens) refusal(status int, body []byte) error {
	var out errorResponse
	if err := json.Unmarshal(body, &out); err != nil {
		out = errorResponse{}
	}

	switch {
	case status == http.StatusNotFound && out.Code == codeNotInstalled:
		return fmt.Errorf("%w: %s: %s", github.ErrNotInstalled, b.target, out.Message)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: the broker answered %d %s: %s", github.ErrTokenRefused, status, out.Code, out.Message)
	default:
		return fmt.Errorf("%w: the broker answered %d %s", github.ErrTokenUnavailable, status, out.Code)
	}
}

const redactedBrokerTokens = "tokenbroker.TokenSource{credentials:[redacted]}"

// String redacts every credential the source holds.
func (brokerTokens) String() string { return redactedBrokerTokens }

// GoString covers %#v, which does not consult String.
func (b brokerTokens) GoString() string { return b.String() }

// Format makes every verb safe.
func (b brokerTokens) Format(f fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(f, b.String())

	_ = verb
}

// MarshalJSON keeps the source's credentials out of anything that serializes it.
func (b brokerTokens) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(b.String())
	if err != nil {
		return nil, fmt.Errorf("tokenbroker: marshal a redacted source: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (b brokerTokens) LogValue() slog.Value { return slog.StringValue(b.String()) }
