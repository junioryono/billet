package ec2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

const (
	sqsService     = "sqs"
	sqsContentType = "application/x-amz-json-1.0"
)

// sqsAPIError is a refusal the SQS API described, keeping the wire's own
// error type so callers classify by it rather than by prose.
type sqsAPIError struct {
	Status  int
	Type    string
	Message string
}

func (e *sqsAPIError) Error() string {
	return fmt.Sprintf("ec2: sqs returned http %d (%s: %s)", e.Status, e.Type, e.Message)
}

type sqsClient struct {
	api      *client
	queueURL string
}

type sqsMessage struct {
	Body          string `json:"Body"`
	ReceiptHandle string `json:"ReceiptHandle"`
}

func (s *sqsClient) receive(ctx context.Context) (*provider.InterruptionNotice, error) {
	for {
		var response struct {
			Messages []sqsMessage `json:"Messages"`
		}
		if err := s.call(ctx, "AmazonSQS.ReceiveMessage", map[string]any{
			"QueueUrl":            s.queueURL,
			"MaxNumberOfMessages": 1,
			"WaitTimeSeconds":     20,
			"VisibilityTimeout":   120,
		}, &response); err != nil {
			return nil, err
		}
		if len(response.Messages) == 0 {
			continue
		}

		return interruptionFromMessage(&response.Messages[0]), nil
	}
}

func interruptionFromMessage(message *sqsMessage) *provider.InterruptionNotice {
	var event struct {
		DetailType string `json:"detail-type"`
		Source     string `json:"source"`
		Detail     struct {
			InstanceID string `json:"instance-id"`
			Action     string `json:"instance-action"`
		} `json:"detail"`
	}
	notice := &provider.InterruptionNotice{Receipt: message.ReceiptHandle}
	if err := json.Unmarshal([]byte(message.Body), &event); err != nil {
		notice.Problem = "the SQS message body is not JSON"

		return notice
	}
	if event.DetailType != "EC2 Spot Instance Interruption Warning" || event.Source != "aws.ec2" {
		notice.Problem = "the SQS message is not an EC2 Spot interruption warning"

		return notice
	}
	if event.Detail.InstanceID == "" || event.Detail.Action == "" {
		notice.Problem = "the EC2 Spot interruption warning has no instance id or action"

		return notice
	}

	notice.InstanceID = event.Detail.InstanceID
	notice.Action = event.Detail.Action

	return notice
}

func (s *sqsClient) delete(ctx context.Context, receipt string) error {
	if receipt == "" {
		return errors.New("ec2: an interruption message has no receipt handle")
	}

	return s.call(ctx, "AmazonSQS.DeleteMessage", map[string]any{
		"QueueUrl":      s.queueURL,
		"ReceiptHandle": receipt,
	}, nil)
}

func (s *sqsClient) call(ctx context.Context, target string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("ec2: encode %s: %w", target, err)
	}

	queue, err := url.Parse(s.queueURL)
	if err != nil {
		return errors.New("ec2: interruption queue url is invalid")
	}
	endpoint := &url.URL{Scheme: queue.Scheme, Host: queue.Host, Path: "/"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ec2: build %s request: %w", target, err)
	}
	req.Header.Set("Content-Type", sqsContentType)
	req.Header.Set("X-Amz-Target", target)
	req.ContentLength = int64(len(body))

	creds, err := s.api.creds.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("ec2: resolve aws credentials for sqs: %w", err)
	}
	now := time.Now
	if s.api.now != nil {
		now = s.api.now
	}
	if err := signService(req, body, creds, s.api.region, sqsService, now()); err != nil {
		return err
	}

	resp, err := s.api.http.Do(req)
	if err != nil {
		// A refused redirect must not render net/http's *url.Error, which
		// carries the attacker-chosen target — the same strip client.attempt
		// applies.
		if uerr, ok := errors.AsType[*url.Error](err); ok && errors.Is(err, errRedirected) {
			return uerr.Err
		}

		return fmt.Errorf("ec2: call sqs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("ec2: read sqs response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The wire carries a JSON error type; keeping it TYPED is the
		// difference between "the queue does not exist" and "this identity may
		// not read it" — and classification by substring over the rendered
		// error would match a queue whose NAME contains the word.
		var detail struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &detail); err == nil && detail.Type != "" {
			return &sqsAPIError{Status: resp.StatusCode, Type: detail.Type, Message: detail.Message}
		}

		return fmt.Errorf("ec2: sqs returned http %d", resp.StatusCode)
	}
	if output == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return fmt.Errorf("ec2: parse sqs response: %w", err)
	}

	return nil
}
