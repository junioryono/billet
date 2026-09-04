// Package awsquota reads an AWS account's Service Quotas.
//
// A CEILING BILLET DOES NOT OWN IS STILL A CEILING, and until this nothing in
// billet asked about one. `billet check` proves a config is coherent and that
// this machine can act on it; what it could not say is whether the ACCOUNT will
// let the fleet do what the config promises. On CodeBuild that is not a corner
// case — the concurrently-running-builds quota defaults to ONE per compute type,
// so a fresh account cannot run two concurrent builds of one shape however much
// capacity billet has escrowed, and the second build queues until CodeBuild
// fails it. On EC2 the equivalent is a running-instance vCPU limit a new account
// gets at a few dozen.
//
// ITS OWN PACKAGE, BESIDE internal/awssig, FOR THE REASON depguard STATES: a
// provider is a leaf and providers are siblings, so `ec2` and `codebuild` cannot
// share a helper by one importing the other. Both need this.
//
// WHAT IT REPORTS IS ADVISORY, ALWAYS, and every caller says so. A quota is
// raised by a support request rather than a config change, a read can fail for
// reasons billet did not cause, and refusing a working deployment over a stale
// or unreadable answer is the failure ADR-005 names — after which the next thing
// anybody does is delete the check.
package awsquota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsjson"
	"github.com/junioryono/billet/internal/awssig"
)

const (
	service     = "servicequotas"
	contentType = "application/x-amz-json-1.1"
	getTarget   = "ServiceQuotasV20190624.GetServiceQuota"
	listTarget  = "ServiceQuotasV20190624.ListServiceQuotas"
	// apiTimeout bounds one call. A preflight makes several and a person is
	// waiting on the command, so this is short: a quota billet could not read in
	// ten seconds is reported as unread rather than waited for.
	apiTimeout = 10 * time.Second
	// maxBody bounds a response, so a redirected or hostile endpoint cannot
	// stream indefinitely into a command an operator is watching.
	maxBody = 4 << 20
	// maxPages bounds a listing. Service Quotas paginates, and a token that
	// cycles would otherwise loop forever inside a diagnostic.
	maxPages = 20
)

// errRedirected is a response that tried to send a signed request elsewhere.
var errRedirected = errors.New("awsquota: the endpoint redirected")

// ErrUnavailable means the account's answer could not be obtained.
//
// A SEPARATE SENTINEL BECAUSE "COULD NOT ASK" IS NOT "NO LIMIT". Every caller
// here reports rather than gates, but they report DIFFERENTLY: a quota read and
// found generous is a fact, and a quota billet could not read is billet saying
// it does not know. Collapsing the two is the could-not-tell/no collapse this
// codebase has already removed from the credential paths, the custody paths and
// the compute barrier.
var ErrUnavailable = errors.New("awsquota: the account's quota could not be read")

// Quota is one account limit, as Service Quotas reports it.
type Quota struct {
	// Service and Code are AWS's own identifiers, carried so a report is
	// actionable in the console or a support request without translating
	// billet's wording back into theirs.
	Service string
	Code    string
	// Name is AWS's human name for the limit.
	Name string
	// Value is the limit and Unit is what it counts — "None" for a plain count,
	// "vCPU" for EC2's running-instance limits. AWS reports it as a double,
	// which is why this is not an int.
	Value float64
	Unit  string
	// Adjustable says whether a support request can raise it, which changes what
	// an operator should do about it entirely.
	Adjustable bool
}

// Client reads quotas for one region.
type Client struct {
	http     *http.Client
	endpoint string
	region   string
	creds    awscreds.Source
}

// New builds a client for a region.
//
// THE REGION IS INTERPOLATED INTO THE ENDPOINT HOST when no endpoint is given,
// so a caller must have validated it — every caller here is a provider whose
// config validation already applies its own region rule, which is the argument
// ec2's discovery client makes for the same shape.
func New(region, endpoint string, creds awscreds.Source) *Client {
	if creds == nil {
		creds = awscreds.Default()
	}

	if endpoint == "" {
		// THE SUFFIX IS THE PARTITION'S, asked of the one place that holds the
		// rule rather than restated as the commercial one, which names no host in
		// cn-north-1.
		endpoint = awsjson.EndpointFor("servicequotas", region)
	}

	httpClient := &http.Client{Timeout: apiTimeout}
	httpClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("%w to host %q", errRedirected, req.URL.Hostname())
	}

	return &Client{http: httpClient, endpoint: endpoint, region: region, creds: creds}
}

// Get reads one quota by its code.
//
// FOR A CODE BILLET IS CONFIDENT OF, which in practice means one AWS documents
// and that is stable across shapes — EC2's running-on-demand-instances limit is
// the case. Where the codes are per-shape and billet would be inventing them,
// use List and match on what AWS itself returns.
//
// THE APPLIED VALUE IS WHAT COMES BACK: GetServiceQuota returns the account's
// applied value where one exists and the default otherwise, which is exactly the
// question — what will this account let us run.
func (c *Client) Get(ctx context.Context, serviceCode, quotaCode string) (Quota, error) {
	body, err := json.Marshal(map[string]string{
		"ServiceCode": serviceCode,
		"QuotaCode":   quotaCode,
	})
	if err != nil {
		return Quota{}, fmt.Errorf("awsquota: encode the request: %w", err)
	}

	raw, err := c.call(ctx, getTarget, body)
	if err != nil {
		return Quota{}, err
	}

	var out struct {
		Quota wireQuota `json:"Quota"`
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		return Quota{}, fmt.Errorf("%w: %s/%s: the response did not parse: %w",
			ErrUnavailable, serviceCode, quotaCode, err)
	}

	// A RESPONSE WITH NO QUOTA IN IT IS UNAVAILABLE, NOT A LIMIT OF ZERO. AWS
	// answers NoSuchResourceException for a code it does not know, which the
	// error path catches — but a body that parsed and carried nothing is the same
	// "could not tell", and reading it as zero would report every fleet as over
	// its limit.
	if out.Quota.QuotaCode == "" {
		return Quota{}, fmt.Errorf("%w: %s/%s: the response carried no quota",
			ErrUnavailable, serviceCode, quotaCode)
	}

	return out.Quota.quota(), nil
}

// List reports every quota AWS publishes for one service.
//
// USED WHERE THE CODES ARE PER-SHAPE. CodeBuild has one concurrency limit per
// environment and compute type, and their codes are not derivable from anything
// billet knows — so billet asks AWS what they are and matches on the names AWS
// returns, rather than shipping a table of identifiers it would be guessing at.
// A shape whose limit is not in the answer is reported as one billet could not
// find, which is the honest third state.
func (c *Client) List(ctx context.Context, serviceCode string) ([]Quota, error) {
	var (
		out   []Quota
		token string
		seen  = map[string]bool{}
	)

	for page := 0; ; page++ {
		if page >= maxPages {
			return out, fmt.Errorf("%w: %s: the listing did not end after %d pages",
				ErrUnavailable, serviceCode, maxPages)
		}

		input := map[string]any{"ServiceCode": serviceCode, "MaxResults": 100}
		if token != "" {
			input["NextToken"] = token
		}

		body, err := json.Marshal(input)
		if err != nil {
			return out, fmt.Errorf("awsquota: encode the request: %w", err)
		}

		raw, err := c.call(ctx, listTarget, body)
		if err != nil {
			return out, err
		}

		var page struct {
			Quotas    []wireQuota `json:"Quotas"`
			NextToken string      `json:"NextToken"`
		}

		if err := json.Unmarshal(raw, &page); err != nil {
			return out, fmt.Errorf("%w: %s: the listing did not parse: %w",
				ErrUnavailable, serviceCode, err)
		}

		for i := range page.Quotas {
			out = append(out, page.Quotas[i].quota())
		}

		if page.NextToken == "" {
			return out, nil
		}

		// A CYCLING TOKEN IS AN ERROR RATHER THAN A LOOP, the rule ec2's price
		// listing already follows.
		if seen[page.NextToken] {
			return out, fmt.Errorf("%w: %s: the listing cycled its pagination token",
				ErrUnavailable, serviceCode)
		}

		seen[page.NextToken] = true
		token = page.NextToken
	}
}

// wireQuota is the shape both calls return.
type wireQuota struct {
	QuotaCode   string  `json:"QuotaCode"`
	QuotaName   string  `json:"QuotaName"`
	ServiceCode string  `json:"ServiceCode"`
	Value       float64 `json:"Value"`
	Unit        string  `json:"Unit"`
	Adjustable  bool    `json:"Adjustable"`
}

func (w wireQuota) quota() Quota {
	return Quota{
		Service:    w.ServiceCode,
		Code:       w.QuotaCode,
		Name:       w.QuotaName,
		Value:      w.Value,
		Unit:       w.Unit,
		Adjustable: w.Adjustable,
	}
}

// call signs and sends one request.
func (c *Client) call(ctx context.Context, target string, body []byte) ([]byte, error) {
	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: no aws credentials: %w", ErrUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint,
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("awsquota: build the request: %w", err)
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Target", target)

	if err := awssig.Sign(req, body, creds, c.region, service, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("awsquota: sign the request: %w", err)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: read the response: %w", ErrUnavailable, err)
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: %s", ErrUnavailable, res.Status, apiMessage(raw))
	}

	return raw, nil
}

// apiMessage pulls AWS's own error type out of a failed response, so a refusal
// says which one it was rather than printing a JSON document at an operator.
//
// THE TYPE AS WELL AS THE MESSAGE, because they send a reader to different
// places: `AccessDeniedException` is a policy statement to add,
// `NoSuchResourceException` is billet naming something AWS does not have, and
// only the first is the operator's to fix.
func apiMessage(raw []byte) string {
	var out struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
		Alt     string `json:"Message"`
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		return "the response did not parse"
	}

	// The type is namespaced (`com.amazon.coral...#AccessDeniedException`), and
	// only the tail is useful.
	kind := out.Type
	if i := strings.LastIndex(kind, "#"); i >= 0 {
		kind = kind[i+1:]
	}

	message := out.Message
	if message == "" {
		message = out.Alt
	}

	switch {
	case kind != "" && message != "":
		return kind + ": " + message
	case kind != "":
		return kind
	case message != "":
		return message
	}

	return "no reason given"
}
