package nodeclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/wirecert"
)

// ErrNotApproved means the control plane has the request and nobody has decided
// yet. The caller waits and asks again.
var ErrNotApproved = errors.New("nodeclient: this node is waiting to be approved")

// ErrDenied means an operator refused this machine. Terminal: asking again
// cannot change it, and the node should stop rather than poll forever.
var ErrDenied = errors.New("nodeclient: an operator denied this node")

// FetchCA reads a control plane's authority and checks it against a fingerprint
// the operator supplied.
//
// THE HANDSHAKE IS DELIBERATELY UNVERIFIED HERE, and the fingerprint is what
// replaces it. A node enrolling for the first time has no authority to verify
// against — that is what it is fetching — so the connection cannot be trusted
// and nothing from it is believed until the fingerprint matches a value that
// travelled by a channel the network does not control.
//
// WITHOUT A FINGERPRINT THIS REFUSES. Accepting whatever answered would be
// trust-on-first-use with no verification step, which on a network an attacker
// can reach is just trust: they answer first, the node enrolls with them, and
// every job it runs afterwards is theirs. The operator reads the value off the
// control plane with `billet ca show`.
func FetchCA(ctx context.Context, base, wantFingerprint string) ([]byte, string, error) {
	if strings.TrimSpace(wantFingerprint) == "" {
		return nil, "", errors.New(
			"nodeclient: enrolling needs the control plane's CA fingerprint, so this node can " +
				"tell it from anything else that answers. Run `billet ca show` on the control " +
				"plane and pass it as --ca-fingerprint")
	}

	endpoint, err := url.JoinPath(base, "/v1/ca")
	if err != nil {
		return nil, "", fmt.Errorf("nodeclient: build the ca url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("nodeclient: build the ca request: %w", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			//nolint:gosec // The fingerprint below is the verification; see the doc comment.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		},
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("nodeclient: read the control plane's authority: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("nodeclient: the control plane returned %s for its authority",
			res.Status)
	}

	var body nodeapi.CAResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("nodeclient: decode the authority: %w", err)
	}

	got, err := wirecert.FingerprintOfCAPEM([]byte(body.CAPEM))
	if err != nil {
		return nil, "", err
	}

	// COMPUTED HERE rather than trusted from the response. The server reports its
	// own fingerprint too, and believing that would be asking the thing being
	// verified to grade itself.
	if !wirecert.SameFingerprint(got, wantFingerprint) {
		return nil, "", fmt.Errorf(
			"nodeclient: the control plane at %s presented authority %s, not %s. Either this is "+
				"not the control plane you meant, or something is answering in its place — do "+
				"not enroll until you know which",
			base, got, wantFingerprint)
	}

	return []byte(body.CAPEM), body.Deployment, nil
}

// Enroll asks a control plane to admit this node, and reports what it said.
//
// Returns ErrNotApproved while an operator has not decided, which is the
// ordinary case on the first call: the caller prints the fingerprint, waits, and
// asks again.
func Enroll(ctx context.Context, base, name string, caPEM, csrPEM []byte) ([]byte, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("nodeclient: the authority could not be parsed for verification")
	}

	// VERIFIED FROM HERE ON. The fingerprint check has established which
	// authority this is, so every later exchange is an ordinary TLS connection
	// pinned to it.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
		},
	}

	endpoint, err := url.JoinPath(base, "/v1/enroll")
	if err != nil {
		return nil, fmt.Errorf("nodeclient: build the enroll url: %w", err)
	}

	payload, err := json.Marshal(nodeapi.EnrollRequest{Node: name, CSRPEM: string(csrPEM)})
	if err != nil {
		return nil, fmt.Errorf("nodeclient: encode the enrollment request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("nodeclient: build the enrollment request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nodeclient: ask to enroll: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodeclient: the control plane refused this enrollment: %s", res.Status)
	}

	var body nodeapi.EnrollResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("nodeclient: decode the enrollment response: %w", err)
	}

	switch body.State {
	case "approved":
		return []byte(body.CertPEM), nil
	case "denied":
		return nil, ErrDenied
	default:
		return nil, fmt.Errorf("%w (fingerprint %s)", ErrNotApproved, body.Fingerprint)
	}
}
