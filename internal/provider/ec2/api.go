package ec2

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
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		// A transport error: the request may never have arrived.
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
		err := c.attempt(ctx, body, out)
		if err == nil {
			return nil
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

func (c *client) attempt(ctx context.Context, body string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("ec2: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	// SET EXPLICITLY because it is SIGNED. NewRequestWithContext derives it from a
	// strings.Reader already, and stating it here keeps the signed value and the
	// sent value from being two separate derivations.
	req.ContentLength = int64(len(body))

	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("ec2: resolve aws credentials: %w", err)
	}

	now := time.Now
	if c.now != nil {
		now = c.now
	}

	if err := sign(req, []byte(body), creds, c.region, now()); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ec2: call the api: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	// BOUNDED. A DescribeInstances page is large but not unbounded, and an
	// unbounded read is an allocation sized by whatever answered.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("ec2: read the api response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return parseAPIError(payload, resp.StatusCode)
	}

	if out == nil {
		return nil
	}

	if err := xml.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("ec2: parse the api response: %w", err)
	}

	return nil
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

// describeImagesResponse carries the one field billet needs from an AMI.
type describeImagesResponse struct {
	Images []struct {
		ImageID        string `xml:"imageId"`
		RootDeviceName string `xml:"rootDeviceName"`
	} `xml:"imagesSet>item"`
}
