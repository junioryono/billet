package ec2

import (
	"bytes"
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
	"github.com/junioryono/billet/internal/config"
)

// The preflight probes `billet check` runs beyond the describes in
// preflight.go. Each one asks the narrowest question that proves the
// deployment can do its job, chosen so the probe itself cannot cost anything:
// the queue probe reads attributes because a ReceiveMessage could CONSUME a
// real two-minute interruption warning, and the instance-profile probe is
// three-valued because the checking identity often may not read IAM at all —
// and "could not tell" collapsed into either verdict misleads the operator.

// RegionMayBeDisabled reports whether an error is the shape an AWS region
// that is NOT ENABLED on the account returns. Measured, twice, with VALID
// credentials (an IAM user and an SSO role that both work in us-east-1):
// af-south-1 answers
//
//	<Code>AuthFailure</Code>
//	<Message>AWS was not able to validate the provided access credentials</Message>
//
// — indistinguishable from a bad key by content. An operator whose key works
// elsewhere must be told the region is the suspect, or they will rotate
// credentials that were never wrong.
func RegionMayBeDisabled(err error) bool {
	apiErr, ok := errors.AsType[*apiError](err)
	if !ok {
		return false
	}

	return apiErr.Code == "AuthFailure" &&
		strings.Contains(apiErr.Message, "not able to validate the provided access credentials")
}

// CheckInterruptionQueue proves the spot interruption queue exists and this
// identity may read it — via GetQueueAttributes, NEVER ReceiveMessage, which
// would consume (and hide for the visibility timeout) a real interruption
// warning some node needed. Returns the queue's ARN.
//
// THE QUEUE URL IS VALIDATED HERE, at the exported boundary, the way ec2.New
// re-validates it: the request below is signed with the operator's
// credentials and posted to the URL's own host, so a caller that skipped
// config.Load must not be able to aim it anywhere. One attempt, no retry
// ladder: check is interactive and rerunning is cheaper than masking a flap.
func CheckInterruptionQueue(
	ctx context.Context, region string, creds awscreds.Source, queueURL string,
) (string, error) {
	if err := config.CheckSQSQueueURL(queueURL, region); err != nil {
		return "", err
	}

	s := &sqsClient{api: discoveryClient(region, "", creds), queueURL: queueURL}

	var out struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := s.call(ctx, "AmazonSQS.GetQueueAttributes", map[string]any{
		"QueueUrl":       queueURL,
		"AttributeNames": []string{"QueueArn"},
	}, &out); err != nil {
		return "", fmt.Errorf("interruption queue %s: %w", queueURL, err)
	}

	arn := out.Attributes["QueueArn"]
	if arn == "" {
		return "", fmt.Errorf("interruption queue %s answered without an ARN", queueURL)
	}

	return arn, nil
}

// QueueProbeInconclusive reports whether a queue-probe failure is a fact
// about the CHECKING identity rather than the queue: a role provisioned
// before sqs:GetQueueAttributes joined SpotIAMActions refuses the probe while
// consuming warnings perfectly well, so AccessDenied must stay advisory or
// the probe fails every pre-upgrade node.
func QueueProbeInconclusive(err error) bool {
	api, ok := errors.AsType[*sqsAPIError](err)

	return ok && strings.Contains(api.Type, "AccessDenied")
}

// ProfileCheck is the three-valued answer about an instance profile. The
// values are distinct on purpose: Missing is a misconfiguration a launch will
// fail on; Unknown means the CHECKING identity may not read IAM, which says
// nothing about the profile — advisory, never fatal.
type ProfileCheck int

const (
	// Unknown is the ZERO VALUE on purpose: an uninitialized verdict must be
	// the least confident answer, not the most.
	ProfileUnknown ProfileCheck = iota
	ProfileFound
	ProfileMissing
)

// CheckInstanceProfile asks IAM whether the named instance profile exists.
// region is the deployment's EC2 region, from which the partition-global IAM
// endpoint AND its signing region are derived — IAM is global per partition,
// and signing a China or GovCloud request as us-east-1 is a
// SignatureDoesNotMatch that would read as a permanently Unknown verdict.
// endpoint overrides the host for tests only. reason carries the API's own
// words for the Unknown verdict.
func CheckInstanceProfile(
	ctx context.Context, region, endpoint string, creds awscreds.Source, name string,
) (ProfileCheck, string, error) {
	signRegion := "us-east-1"
	defaultEndpoint := "https://iam.amazonaws.com/"
	switch {
	case strings.HasPrefix(region, "cn-"):
		signRegion, defaultEndpoint = "cn-north-1", "https://iam.cn-north-1.amazonaws.com.cn/"
	case strings.HasPrefix(region, "us-gov-"):
		signRegion, defaultEndpoint = "us-gov-west-1", "https://iam.us-gov.amazonaws.com/"
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if creds == nil {
		creds = awscreds.Default()
	}

	form := url.Values{}
	form.Set("Action", "GetInstanceProfile")
	form.Set("Version", "2010-05-08")
	form.Set("InstanceProfileName", name)
	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ProfileUnknown, "", fmt.Errorf("ec2: build iam request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.ContentLength = int64(len(body))

	resolved, err := creds.Credentials(ctx)
	if err != nil {
		return ProfileUnknown, "", fmt.Errorf("ec2: resolve aws credentials for iam: %w", err)
	}

	if err := signService(req, body, resolved, signRegion, "iam", time.Now()); err != nil {
		return ProfileUnknown, "", err
	}

	httpClient := &http.Client{Timeout: apiTimeout}
	httpClient.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		return fmt.Errorf("%w to host %q", errRedirected, r.URL.Hostname())
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		// A refused redirect must not render net/http's *url.Error, which
		// carries the attacker-chosen target — the same strip client.attempt
		// applies.
		if uerr, ok := errors.AsType[*url.Error](err); ok && errors.Is(err, errRedirected) {
			return ProfileUnknown, "", uerr.Err
		}

		return ProfileUnknown, "", fmt.Errorf("ec2: call iam: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProfileUnknown, "", fmt.Errorf("ec2: read iam response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		// The BODY must be IAM's answer, not a captive portal's 200: a Found
		// verdict on a proxy page would be the most confident wrong answer.
		if !strings.Contains(string(payload), "GetInstanceProfileResult") {
			return ProfileUnknown, "the 200 response was not an IAM answer", nil
		}

		return ProfileFound, "", nil
	}

	var apiResp struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	if err := xml.Unmarshal(payload, &apiResp); err != nil || apiResp.Error.Code == "" {
		return ProfileUnknown, fmt.Sprintf("iam answered http %d", resp.StatusCode), nil
	}

	if apiResp.Error.Code == "NoSuchEntity" {
		return ProfileMissing, apiResp.Error.Message, nil
	}

	// AccessDenied and everything else: a verdict about the CHECKER, not the
	// profile.
	return ProfileUnknown, fmt.Sprintf("%s: %s", apiResp.Error.Code, apiResp.Error.Message), nil
}
