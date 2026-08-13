package ec2

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// apiVersion is the EC2 query API this client speaks. Pinned, because the
// parameter and response shapes below are that version's.
const apiVersion = "2016-11-15"

// maxAttempts bounds how many times one call is issued.
//
// THREE, NOT MORE, and the reason is that the caller is a launch path a job is
// waiting on. Retrying is right for a throttle or a 500 — those are the API
// saying "not now" rather than "no" — but a long retry ladder in front of
// RunInstances turns a busy region into a launch that outlives the plane's
// command timeout, and the node then has custody of compute nobody asked for.
const maxAttempts = 3

// client talks the EC2 query API over signed HTTPS.
//
//nolint:recvcheck // The redaction methods MUST take a value receiver: a pointer-receiver String is not consulted when a VALUE is formatted, so %+v on a dereferenced client would print the secret out of its unexported creds field. Every other method needs the pointer. The mix is deliberate and the safety property is the reason — github.App carries the identical exception for the identical reason.
type client struct {
	http     *http.Client
	endpoint string
	region   string
	creds    CredentialSource

	// now is time.Now, replaceable so a test can pin a signature.
	now func() time.Time
	// sleep waits between attempts, replaceable so a test does not.
	sleep func(ctx context.Context, d time.Duration) error
}

// REDACTED, BECAUSE IT HOLDS A CREDENTIAL SOURCE IN AN UNEXPORTED FIELD.
//
// fmt cannot invoke methods through an unexported field — reflect refuses — so a
// source's own redaction is never consulted when the struct AROUND it is printed
// structurally. `%+v` on a client holding a value-typed source printed the secret
// access key in full, past three layers of redaction that all worked in
// isolation.
//
// This is the fourth type in this package to need its own methods for that
// reason, which is the tell that method-based redaction cannot be made absolute:
// see the note in the billet-security skill, where the same residual is recorded
// for the GitHub App key.
// ON A VALUE RECEIVER, which is the rule this package's own skill states and
// which the first version of these methods broke: a pointer receiver is not
// consulted when a VALUE is formatted, so `%+v` on a dereferenced client
// structurally rendered the unexported creds field and printed the secret. A
// value receiver serves both forms.
//
// `Credentials` and `StaticCredentials` take value receivers for the same reason.
// `IMDSCredentials` cannot — it holds a sync.Mutex, so a value copy is a vet
// error and the pointer receiver is the only correct choice there.
func (c client) String() string { return "ec2.client{endpoint=" + c.endpoint + "}" }

// GoString covers %#v.
func (c client) GoString() string { return c.String() }

// Format catches every verb.
func (c client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a client out of anything that serializes it structurally.
func (c client) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults.
func (c client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// apiError is a refusal the EC2 API described.
//
// THE CODE IS THE PART THAT MATTERS, and it is kept separate from the message
// because callers branch on it: an already-terminated instance is success for an
// idempotent destroy, and telling that from a real failure by matching prose is
// how a teardown failure gets swallowed.
type apiError struct {
	Code    string
	Message string
	Status  int
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("ec2: the api returned http %d", e.Status)
	}

	return fmt.Sprintf("ec2: %s: %s", e.Code, e.Message)
}

// codeOf reports the API error code in a chain, and whether there was one.
func codeOf(err error) (string, bool) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Code, true
	}

	return "", false
}

// retryable reports whether an attempt is worth repeating.
//
// A THROTTLE AND A 500 ARE "NOT NOW"; EVERYTHING ELSE IS "NO". Retrying a
// rejected parameter just spends the caller's deadline arriving at the same
// answer, and retrying RunInstances after an ambiguous failure is how one job
// becomes two instances — which is why the launch also carries a client token,
// so that even a retry AWS itself performs cannot double-launch.
func retryable(err error) bool {
	// A REFUSED REDIRECT IS A VERDICT, NOT A BLIP. An endpoint that answers with a
	// redirect will answer with one again, so the retries cannot change the
	// outcome — and each one is another signed request handed to whatever is
	// answering. It has to be named here because it is not an apiError, and the
	// default for those is the opposite.
	if errors.Is(err, errRedirected) {
		return false
	}

	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		// Anything else that is not the API answering: a connection that dropped, a
		// body that would not parse. The request may never have arrived, so it is
		// worth making again — unlike the refusal above, which is billet's own
		// verdict and will not change.
		return true
	}

	switch apiErr.Code {
	case "RequestLimitExceeded", "Throttling", "ThrottlingException",
		"Unavailable", "ServiceUnavailable", "InternalError", "InternalFailure":
		return true
	}

	return apiErr.Status >= http.StatusInternalServerError
}

// call issues one action and unmarshals the response into out.
func (c *client) call(ctx context.Context, params url.Values, out any) error {
	params.Set("Version", apiVersion)

	body := params.Encode()

	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			// EXPONENTIAL, AND CANCELLABLE. A fixed pause is either useless
			// against a throttle or wasteful against a blip, and a sleep that
			// ignores the context outlives the job it is launching for.
			delay := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond

			if err := c.wait(ctx, delay); err != nil {
				return err
			}
		}

		// REBUILT EVERY ATTEMPT. A signature covers a timestamp and the
		// credentials, both of which can change between attempts, and the body
		// reader is consumed by the one before.
		payload, err := c.attempt(ctx, body)
		if err == nil {
			if out == nil {
				return nil
			}

			// ZEROED BEFORE EVERY DECODE, because encoding/xml APPENDS to slices
			// and this target is shared across attempts.
			//
			// A truncated body is worth retrying — it is a transfer that failed
			// rather than an answer billet disagrees with — but the decoder fills
			// in what it managed to read before it fails. Without this the retry
			// appended a full set of rows to the partial ones, and
			// DescribeInstances reported an instance twice into a list that feeds a
			// loop that destroys. Measured: two instances came back as four.
			if v := reflect.ValueOf(out); v.Kind() == reflect.Pointer && !v.IsNil() {
				v.Elem().SetZero()
			}

			if err = xml.Unmarshal(payload, out); err == nil {
				return nil
			}

			err = fmt.Errorf("ec2: parse the api response: %w", err)
		}

		lastErr = err

		if !retryable(err) {
			return err
		}
	}

	return fmt.Errorf("ec2: %s failed after %d attempts: %w",
		params.Get("Action"), maxAttempts, lastErr)
}

func (c *client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
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
// caller so that a failed attempt cannot contaminate the next one's target.
func (c *client) attempt(ctx context.Context, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ec2: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	// SET EXPLICITLY because it is SIGNED. NewRequestWithContext derives it from a
	// strings.Reader already, and stating it here keeps the signed value and the
	// sent value from being two separate derivations.
	req.ContentLength = int64(len(body))

	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("ec2: resolve aws credentials: %w", err)
	}

	now := time.Now
	if c.now != nil {
		now = c.now
	}

	if err := sign(req, []byte(body), creds, c.region, now()); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// THE WRAPPER IS DISCARDED, NOT WRAPPED AGAIN. net/http returns a
		// *url.Error whose message contains the whole URL it was working on — for
		// a refused redirect that is the TARGET, chosen by whatever answered, with
		// its query string intact. The sentinel already says everything billet is
		// willing to say about it.
		// THE SENTINEL'S OWN MESSAGE, not the bare sentinel. The CheckRedirect
		// closure names the host it refused — safe by the same rule that governs
		// everything else here — and returning errRedirected alone threw that away,
		// so an operator whose VPC endpoint sits behind a redirecting proxy was told
		// only that a redirect happened. What must not survive is net/http's
		// *url.Error wrapper, which renders the whole target including its query.
		var uerr *url.Error
		if errors.As(err, &uerr) && errors.Is(err, errRedirected) {
			return nil, uerr.Err
		}

		if errors.Is(err, errRedirected) {
			return nil, err
		}

		return nil, fmt.Errorf("ec2: call the api: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	// BOUNDED. A DescribeInstances page is large but not unbounded, and an
	// unbounded read is an allocation sized by whatever answered.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("ec2: read the api response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(payload, resp.StatusCode)
	}

	return payload, nil
}

// errorResponse is the shape the query API uses for a refusal.
type errorResponse struct {
	Errors []struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Errors>Error"`
	RequestID string `xml:"RequestID"`
}

// parseAPIError turns a non-200 into an apiError, keeping the status when the
// body is not the shape it should be.
func parseAPIError(payload []byte, status int) error {
	var parsed errorResponse

	if err := xml.Unmarshal(payload, &parsed); err != nil || len(parsed.Errors) == 0 {
		// A GATEWAY, A PROXY, OR A LOAD BALANCER answered instead of EC2, and its
		// body is not this shape. The status is all there is, and it is enough to
		// decide whether to retry.
		return &apiError{Status: status}
	}

	return &apiError{
		Code:    parsed.Errors[0].Code,
		Message: parsed.Errors[0].Message,
		Status:  status,
	}
}

// instanceItem is one instance as the API describes it.
type instanceItem struct {
	InstanceID string `xml:"instanceId"`
	State      struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	// StateReason says WHO stopped it, which "stopped" alone does not.
	// Client.InstanceInitiatedShutdown is the guest powering itself off;
	// Client.UserInitiatedShutdown is somebody calling StopInstances; Server.* is
	// the host. An image build treats only the first as a success signal.
	StateReason struct {
		Code string `xml:"code"`
	} `xml:"stateReason"`
	Tags []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
}

// tag reports a tag's value, and whether it was set.
func (i instanceItem) tag(key string) (string, bool) {
	for _, t := range i.Tags {
		if t.Key == key {
			return t.Value, true
		}
	}

	return "", false
}

// runInstancesResponse is what a launch answers with.
// createImageResponse carries the id of the AMI a build produced.
type createImageResponse struct {
	ImageID string `xml:"imageId"`
}

type runInstancesResponse struct {
	Instances []instanceItem `xml:"instancesSet>item"`
}

// describeInstancesResponse is one page of instances.
//
// NextToken is why this is a page rather than the answer. A fleet larger than one
// page that ignored it would report a subset — and the callers are reconciliation
// and teardown, so a missing instance reads as "that lease is not running here",
// which frees capacity for a machine that is still executing a job.
type describeInstancesResponse struct {
	Reservations []struct {
		Instances []instanceItem `xml:"instancesSet>item"`
	} `xml:"reservationSet>item"`
	NextToken string `xml:"nextToken"`
}

// describeImagesResponse carries what billet needs from an AMI: which device is
// root, and EVERY mapping the image declares — the root included, because whether
// billet says anything about overriding it depends on what that entry said.
type describeImagesResponse struct {
	Images []struct {
		ImageID        string `xml:"imageId"`
		RootDeviceName string `xml:"rootDeviceName"`
		// RootDeviceType is "ebs" or "instance-store". billet requires the first
		// and refuses the second up front (#54), because every root parameter it
		// sends is EBS-shaped.
		RootDeviceType string `xml:"rootDeviceType"`
		// State is "pending" until an image can be launched from, then "available".
		// A build that hands back an id before that produces a tier whose first
		// launch fails with an error about the image rather than about the config.
		State string `xml:"imageState"`
		// BlockDevices are the image's own mappings. billet states
		// DeleteOnTermination at launch for the EBS ones (#53), so these are read to
		// BUILD that request — and to warn about the ones the image asks to keep.
		// Mappings with no <ebs> child, and any with no device name, are not
		// restated; the root is stated by setBlockDevices rather than from here.
		BlockDevices []imageMapping `xml:"blockDeviceMapping>item"`
	} `xml:"imagesSet>item"`
}

// imageMapping is one entry of an image's block device mapping.
type imageMapping struct {
	DeviceName string `xml:"deviceName"`
	// A POINTER BECAUSE ABSENCE IS MEANINGFUL. A mapping with no <ebs> child is an
	// instance-store or suppressed device, and handing EC2 an
	// Ebs.DeleteOnTermination for one is a request about a volume that does not
	// exist. A value type cannot express that: an absent <ebs> and a
	// present-but-empty one would both decode to the zero struct.
	//
	// It is also how billet establishes that a root is EBS-backed when the image
	// does not report its root device type: see requireEBSRoot.
	EBS *struct {
		DeleteOnTermination string `xml:"deleteOnTermination"`
	} `xml:"ebs"`
}
