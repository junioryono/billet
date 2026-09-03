// Package awssts answers one question: which AWS account is this credential in.
//
// ONE CALL, AND IT EXISTS FOR A REFUSAL. `billet acceptance` stands up a
// deployment that launches real compute and then destroys everything it finds
// belonging to itself, and the instruction an operator gives it is "do that in
// account N". Without asking, "account N" is a comment: the command would run
// against whatever credential happened to be in the environment, which on a
// developer's machine is routinely the wrong one.
//
// A LEAF BESIDE internal/awssig, for the reason internal/awsquota is one: the
// provider packages are siblings that may not import each other, and this is not
// a compute backend at all.
package awssts

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
)

const (
	service = "sts"
	// version is the STS API version GetCallerIdentity has always been under.
	version = "2011-06-15"
	// timeout bounds one call. It is a single unpaginated request against a
	// service with no work to do, and it runs in front of everything else an
	// acceptance run does — so a hang here is a run that never starts and never
	// says why.
	timeout = 15 * time.Second
	// maxBody bounds what is read back. The response is a few hundred bytes; the
	// bound is here because a signed request to a host an operator's region string
	// chose is not a host billet controls.
	maxBody = 64 << 10
)

// ErrNoAccount is what a caller gets when the response carried no account.
//
// SEPARATE FROM A TRANSPORT FAILURE, because the two mean opposite things to a
// refusal: "I could not ask" must never read as "the account does not match".
var ErrNoAccount = errors.New("awssts: the response carried no account id")

// Identity is who a credential belongs to.
type Identity struct {
	// Account is the twelve-digit account id — the only field billet compares.
	Account string
	// ARN and UserID are reported, never matched. An acceptance run prints them
	// so a person reading a log can see WHICH principal did the work, which is
	// the question that follows "was it the right account".
	ARN    string
	UserID string
}

// Endpoint returns the STS endpoint and signing region for an AWS region.
//
// REGIONAL RATHER THAN GLOBAL, deliberately. The global endpoint
// (sts.amazonaws.com) exists and works, and using it would mean a GovCloud or
// China credential signing against a host in the commercial partition — which
// fails, and fails with an authentication error rather than anything that names
// the partition. A regional endpoint is derived from the region billet was
// already told to use.
func Endpoint(region string) (string, string) {
	if strings.HasPrefix(region, "cn-") {
		return "https://sts." + region + ".amazonaws.com.cn/", region
	}

	return "https://sts." + region + ".amazonaws.com/", region
}

// Client calls GetCallerIdentity.
type Client struct {
	http     *http.Client
	endpoint string
	region   string
	creds    awscreds.Source

	// clock is the signing time, behind a seam so a test can pin a signature.
	// Nil means time.Now.
	clock func() time.Time
}

func (c *Client) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}

	return c.clock()
}

// New builds a client for one region. An empty endpoint takes the regional one.
func New(region, endpoint string, creds awscreds.Source, httpClient *http.Client) *Client {
	if creds == nil {
		creds = awscreds.Default()
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	signRegion := region
	if endpoint == "" {
		endpoint, signRegion = Endpoint(region)
	}

	return &Client{http: httpClient, endpoint: endpoint, region: signRegion, creds: creds}
}

// Whoami answers who this credential is.
//
// THREE ANSWERS, NOT TWO. A successful call with an account, a call that could
// not be made, and a call that answered without one — the last is ErrNoAccount,
// so a caller comparing accounts can refuse rather than compare against "".
func (c *Client) Whoami(ctx context.Context) (Identity, error) {
	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("awssts: aws credentials: %w", err)
	}

	form := url.Values{"Action": {"GetCallerIdentity"}, "Version": {version}}
	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return Identity{}, fmt.Errorf("awssts: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.ContentLength = int64(len(body))

	if err := awssig.Sign(req, body, creds, c.region, service, c.now()); err != nil {
		return Identity{}, fmt.Errorf("awssts: sign: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("awssts: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Identity{}, fmt.Errorf("awssts: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// THE STATUS AND THE SERVICE'S OWN WORDS, because the two most likely
		// failures — an expired credential and a credential with no permission —
		// are indistinguishable from the status alone and an operator needs to
		// know which. The body of an STS error carries no secret; the REQUEST
		// carries the signature, and that is not rendered.
		return Identity{}, fmt.Errorf("awssts: GetCallerIdentity: %s: %s",
			resp.Status, strings.TrimSpace(string(payload)))
	}

	var doc struct {
		Result struct {
			Account string `xml:"Account"`
			ARN     string `xml:"Arn"`
			UserID  string `xml:"UserId"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := xml.Unmarshal(payload, &doc); err != nil {
		return Identity{}, fmt.Errorf("awssts: parse response: %w", err)
	}

	if strings.TrimSpace(doc.Result.Account) == "" {
		return Identity{}, ErrNoAccount
	}

	return Identity{
		Account: doc.Result.Account,
		ARN:     doc.Result.ARN,
		UserID:  doc.Result.UserID,
	}, nil
}
