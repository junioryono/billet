// Package awsjson is billet's client for AWS services that speak JSON 1.1.
//
// EXTRACTED FROM internal/provider/codebuild RATHER THAN WRITTEN AGAIN, for the
// reason internal/awscreds was: a second package needed the same machinery, and a
// compute backend cannot be a library for the rest of billet. The server role now
// reaches Parameter Store for the deployment's identity material, and copying four
// hundred lines of retry ladder, redirect refusal, error classification and
// endpoint derivation would be the two-pins problem in the code that signs
// requests against somebody else's API.
//
// WHAT IT DOES NOT OWN is what each service means. An action's request and
// response shapes, which errors are verdicts, and which calls may be retried at
// all belong to the caller; this is the transport, the signature, and the
// classification of outcomes that are the same for every service.
//
// IT IS A LEAF. It imports billet's signer, its credential chain and the config
// package below both, and nothing else, so a provider and the control plane can
// both use it without either importing the other. config is there for one rule:
// which DNS suffix a region's partition uses. That question is asked on both sides
// of the wire — here to build a host, and in config to refuse a spot queue URL that
// names the other partition's — and config may import nothing of billet's, so the
// rule is declared there and called from here rather than written twice.
package awsjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

// ContentType is what AWS JSON 1.1 requires. It is SIGNED, so a mismatch between
// the header sent and the header signed is a 403 naming nothing.
const ContentType = "application/x-amz-json-1.1"

// MaxAttempts bounds how many times one call is issued.
//
// THREE, the same as the ec2 client and for the same reason: a caller is often a
// launch path a job is waiting on, and a long retry ladder turns a throttled
// region into a launch that outlives the plane's command timeout.
const MaxAttempts = 3

// APITimeout bounds one API call. Generous next to the calls themselves, because
// what is being bounded is a stall rather than the work.
const APITimeout = 30 * time.Second

// maxResponse bounds a response read. An unbounded read is an allocation sized by
// whatever answered.
const maxResponse = 8 << 20

// ErrRedirected is billet's own refusal of a redirect from a signed endpoint.
//
// A SENTINEL RATHER THAN PROSE, for the reason the ec2 client documents: net/http
// wraps whatever CheckRedirect returns in a *url.Error, and THAT type renders the
// whole redirect target — including a query string chosen by whatever answered.
// The call boundary recognises this and replaces the wrapper rather than wrapping
// it.
//
// ONE SENTINEL FOR EVERY SERVICE, because errors.Is has to match across the
// package boundary: a per-client value would make every caller's own check false.
var ErrRedirected = errors.New("awsjson: the api endpoint answered with a redirect")

// APIError is a refusal one of the JSON APIs described.
//
// THE TYPE IS THE PART THAT MATTERS and it is kept separate from the message,
// because callers branch on it: a build that is already complete is success for
// an idempotent teardown, and telling that from a real failure by matching prose
// is how a teardown failure gets swallowed. The wire's `__type` is often
// qualified (`com.amazonaws...#ResourceNotFoundException`), so Code strips the
// qualifier.
type APIError struct {
	// Service names the caller in the message, so an operator reading a log sees
	// which of billet's clients was refused rather than a bare code.
	Service string
	Code    string
	Message string
	Status  int
}

func (e *APIError) Error() string {
	who := e.Service
	if who == "" {
		who = "awsjson"
	}

	if e.Code == "" {
		return fmt.Sprintf("%s: the api returned http %d", who, e.Status)
	}

	return fmt.Sprintf("%s: %s: %s", who, e.Code, e.Message)
}

// CodeOf reports the API error code in a chain, and whether there was one.
func CodeOf(err error) (string, bool) {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Code, true
	}

	return "", false
}

// StatusOf reports the HTTP status in a chain, and whether there was one.
func StatusOf(err error) (int, bool) {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.Status, true
	}

	return 0, false
}

// Unqualified strips the namespace AWS prefixes onto a JSON error type.
//
// `__type` arrives either bare (`ResourceNotFoundException`) or fully qualified
// (`com.amazonaws.codebuild#ResourceNotFoundException`), and both forms are the
// same verdict. Matching the raw string would make every branch depend on which
// form a given endpoint happens to send.
func Unqualified(t string) string {
	if i := strings.LastIndexAny(t, "#:"); i >= 0 {
		return t[i+1:]
	}

	return t
}

// Retryable reports whether an attempt is worth repeating.
//
// A THROTTLE AND A 5XX ARE "NOT NOW"; EVERYTHING ELSE IS "NO". Retrying a
// rejected parameter spends the caller's deadline arriving at the same answer.
func Retryable(err error) bool {
	// A REFUSED REDIRECT IS A VERDICT, NOT A BLIP. An endpoint that answers with a
	// redirect will answer with one again, so retrying cannot change the outcome —
	// and each attempt is another signed request handed to whatever is answering.
	if errors.Is(err, ErrRedirected) {
		return false
	}

	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		// A connection that dropped, or a body that would not parse. The request
		// may never have arrived, so it is worth making again — unlike the refusal
		// above, which is billet's own verdict and will not change.
		return true
	}

	switch apiErr.Code {
	case "ThrottlingException", "Throttling", "TooManyRequestsException",
		"RequestThrottledException", "ServiceUnavailableException",
		"InternalServerError", "InternalFailure", "ServiceUnavailable":
		return true
	}

	// AccountLimitExceededException IS NOT A BLIP and is named here because the
	// line below reads the status alone. It is AWS saying an account quota is
	// full — which retrying milliseconds later cannot change.
	if apiErr.Code == "AccountLimitExceededException" {
		return false
	}

	return apiErr.Status >= http.StatusInternalServerError
}

// RetryableRefusal is Retryable narrowed to outcomes AWS ANSWERED WITH.
//
// The distinction is whether billet knows the request was not acted on. A
// throttle says so; a dropped connection, a body that would not parse and a 5xx
// do not — and for an action that creates something, "may have been processed"
// and "was processed" have to be treated the same way.
func RetryableRefusal(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		return false
	}

	switch apiErr.Code {
	case "ThrottlingException", "Throttling", "TooManyRequestsException",
		"RequestThrottledException":
		return true
	}

	return false
}

// Client talks a JSON 1.1 API over signed HTTPS.
//
//nolint:recvcheck // The redaction methods MUST take a value receiver: a pointer-receiver String is not consulted when a VALUE is formatted, so %+v on a dereferenced client would print the secret out of its unexported creds field. Every other method needs the pointer. The ec2 and codebuild clients carry the identical exception for the identical reason.
type Client struct {
	// name prefixes this client's own errors, so a log says which of billet's
	// clients was talking rather than naming this package.
	name string

	http   *http.Client
	region string
	creds  awscreds.Source

	// Now is time.Now, replaceable so a test can pin a signature.
	Now func() time.Time
	// Sleep waits between attempts, replaceable so a test does not.
	Sleep func(ctx context.Context, d time.Duration) error
}

// REDACTED, BECAUSE IT HOLDS A CREDENTIAL SOURCE IN AN UNEXPORTED FIELD.
//
// fmt cannot invoke methods through an unexported field — reflect refuses — so a
// source's own redaction is never consulted when the struct AROUND it is printed
// structurally. On the ec2 side `%+v` on a client holding a value-typed source
// printed the secret access key in full, past three layers of redaction that all
// worked in isolation.
//
// ON A VALUE RECEIVER, which is the rule and which the ec2 client broke on its
// first attempt: a pointer receiver is not consulted when a VALUE is formatted.
func (c Client) String() string { return "awsjson.Client{name=" + c.name + "}" }

// GoString covers %#v.
func (c Client) GoString() string { return c.String() }

// Format catches every verb. Implementing it means fmt never consults String or
// GoString, which is why they are also called directly by the redaction test.
func (c Client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a client out of anything that serializes it structurally.
func (c Client) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults; its JSON handler ignores fmt entirely.
func (c Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// New builds a signed client.
//
// THERE IS NO DEFAULT CREDENTIAL SOURCE. A caller passes one rather than having
// this reach for the ambient chain, so nothing can end up signing with
// credentials it did not choose.
func New(name, region string, creds awscreds.Source) *Client {
	return &Client{
		name:   name,
		http:   &http.Client{Timeout: APITimeout},
		region: region,
		creds:  creds,
	}
}

// HTTPClient exposes the transport so a caller can install a redirect refusal or
// a bounded timeout of its own.
func (c *Client) HTTPClient() *http.Client { return c.http }

// SetHTTPClient replaces the transport. A caller that supplies one owns its
// timeout and its redirect policy.
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// Region is the signing region this client was built for.
func (c *Client) Region() string { return c.region }

// CredentialSource is where this client resolves credentials, or nil.
//
// EXPOSED SO A CONSTRUCTOR CAN REFUSE A MISSING OR TYPED-NIL SOURCE, which is the
// one thing a caller has to check before the first signed call: a typed nil
// satisfies the interface, passes a plain `== nil`, and dereferences on a path
// that is already holding leases.
func (c *Client) CredentialSource() awscreds.Source { return c.creds }

// SetCredentials replaces the credential source.
func (c *Client) SetCredentials(src awscreds.Source) { c.creds = src }

// Invoke issues one action against one endpoint and unmarshals the response.
//
// `again` DECIDES WHAT MAY BE REPEATED, and it is the caller's rather than this
// package's: a read may be retried on anything transient, while an action that
// CREATES something may be retried only on an outcome AWS itself refused to act
// on. Passing the wrong one is how a launch becomes two.
func (c *Client) Invoke(
	ctx context.Context, endpoint, service, target, action string, in, out any,
	again func(error) bool,
) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%s: encode %s: %w", c.name, action, err)
	}

	var lastErr error

	for attempt := range MaxAttempts {
		if attempt > 0 {
			// EXPONENTIAL, AND CANCELLABLE. A fixed pause is either useless against
			// a throttle or wasteful against a blip, and a sleep that ignores the
			// context outlives the job it is launching for.
			delay := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond

			if err := c.wait(ctx, delay); err != nil {
				return err
			}
		}

		// REBUILT EVERY ATTEMPT. A signature covers a timestamp and the
		// credentials, both of which can change between attempts, and the body
		// reader is consumed by the one before.
		payload, err := c.attempt(ctx, endpoint, service, target, body)
		if err == nil {
			if out == nil || len(payload) == 0 {
				return nil
			}

			if err = json.Unmarshal(payload, out); err == nil {
				return nil
			}

			// A TRUNCATED BODY IS WORTH RETRYING — it is a transfer that failed
			// rather than an answer billet disagrees with. Unlike encoding/xml,
			// encoding/json REPLACES a slice rather than appending to it, so a
			// partial decode into a shared target cannot leave rows from the
			// previous attempt behind.
			err = fmt.Errorf("%s: parse the %s response: %w", c.name, action, err)
		}

		lastErr = err

		if !again(err) {
			return err
		}
	}

	return fmt.Errorf("%s: %s failed after %d attempts: %w", c.name, action, MaxAttempts, lastErr)
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}

	// A TIMER RATHER THAN time.After, which billet bans outright: After leaks its
	// timer until it fires, and this one is abandoned on every cancellation.
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// attempt issues one request and returns its body, leaving the decode to the
// caller so a failed attempt cannot contaminate the next one's target.
func (c *Client) attempt(
	ctx context.Context, endpoint, service, target string, body []byte,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", c.name, err)
	}

	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("X-Amz-Target", target)
	// SET EXPLICITLY BECAUSE IT IS SIGNED. NewRequestWithContext derives it from a
	// bytes.Reader already, and stating it here keeps the signed value and the sent
	// value from being two separate derivations.
	req.ContentLength = int64(len(body))

	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: resolve aws credentials: %w", c.name, err)
	}

	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, awssig.ErrNoCredentials
	}

	if err := awssig.Sign(req, body, creds, c.region, service, now()); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// THE WRAPPER IS DISCARDED, NOT WRAPPED AGAIN. net/http returns a
		// *url.Error whose message contains the whole URL it was working on — for
		// a refused redirect that is the TARGET, chosen by whatever answered, with
		// its query string intact. The sentinel's own message already says
		// everything billet is willing to say.
		if uerr, ok := errors.AsType[*url.Error](err); ok && errors.Is(err, ErrRedirected) {
			return nil, uerr.Err
		}

		if errors.Is(err, ErrRedirected) {
			return nil, err
		}

		return nil, fmt.Errorf("%s: call the api: %w", c.name, err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("%s: read the api response: %w", c.name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(c.name, payload, resp.StatusCode)
	}

	return payload, nil
}

// ParseAPIError turns a non-200 into an APIError, keeping the status when the
// body is not the shape it should be.
//
// A GATEWAY, A PROXY OR A LOAD BALANCER can answer instead of AWS, and its body
// is not this shape. The status is all there is, and it is enough to decide
// whether to retry.
func ParseAPIError(service string, payload []byte, status int) error {
	var detail struct {
		Type      string `json:"__type"`
		Message   string `json:"message"`
		AltMessag string `json:"Message"`
	}

	if err := json.Unmarshal(payload, &detail); err != nil {
		return &APIError{Service: service, Status: status}
	}

	message := detail.Message
	if message == "" {
		message = detail.AltMessag
	}

	if detail.Type == "" {
		return &APIError{Service: service, Status: status, Message: message}
	}

	return &APIError{
		Service: service,
		Code:    Unqualified(detail.Type),
		Message: message,
		Status:  status,
	}
}

// EndpointFor derives the regional endpoint for one service.
//
// THE SUFFIX IS NOT THE SAME IN EVERY PARTITION, the rule the ec2 client already
// states: a region check deliberately admits partitions billet has never run in,
// so the commercial suffix would derive a host that does not exist for
// `cn-north-1`. AWS China is reached at amazonaws.com.cn; GovCloud uses the
// commercial suffix.
func EndpointFor(service, region string) string {
	return "https://" + service + "." + region + "." + DNSSuffixFor(region) + "/"
}

// DNSSuffixFor is the partition's DNS suffix for a region.
//
// THE RULE ITSELF LIVES IN internal/config, and this is one call rather than a
// second copy. config is the leaf everything reads and may import nothing of
// billet's, so its SQS host validator cannot ask this package the question — and it
// has to select exactly this suffix or it admits a queue host in the other
// partition. Keeping the name here means nothing above changes. config.TapPrefix is
// the same arrangement for the firecracker provider, for the same reason.
func DNSSuffixFor(region string) string {
	return config.AWSDNSSuffix(region)
}
