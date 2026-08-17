package ec2

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestSpotInterruptionIsReceivedAndAcknowledged(t *testing.T) {
	var (
		mu      sync.Mutex
		targets []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "/us-west-2/sqs/aws4_request") {
			t.Errorf("request was not signed for SQS: %s", got)
		}

		target := r.Header.Get("X-Amz-Target")
		mu.Lock()
		targets = append(targets, target)
		mu.Unlock()

		switch target {
		case "AmazonSQS.ReceiveMessage":
			event, err := json.Marshal(map[string]any{
				"detail-type": "EC2 Spot Instance Interruption Warning",
				"source":      "aws.ec2",
				"detail": map[string]string{
					"instance-id":     "i-0123456789",
					"instance-action": "terminate",
				},
			})
			if err != nil {
				t.Errorf("marshal event: %v", err)
				w.WriteHeader(http.StatusInternalServerError)

				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"Messages": []map[string]string{
				{"Body": string(event), "ReceiptHandle": "opaque-receipt"},
			}}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		case "AmazonSQS.DeleteMessage":
			if !strings.Contains(string(body), `"ReceiptHandle":"opaque-receipt"`) {
				t.Errorf("delete did not carry the receipt handle: %s", body)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected target %q", target)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region:               "us-west-2",
		SubnetID:             "subnet-1",
		SecurityGroupIDs:     []string{"sg-1"},
		Spot:                 true,
		InterruptionQueueURL: srv.URL + "/123456789012/aws-1",
		NodeName:             "aws-1",
		InstanceTypes:        []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(StaticCredentials{
		AccessKeyID: "AKID", SecretAccessKey: "secret",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	notice, err := p.NextInterruption(t.Context())
	if err != nil {
		t.Fatalf("NextInterruption: %v", err)
	}
	if notice.InstanceID != "i-0123456789" || notice.Action != "terminate" {
		t.Fatalf("notice = %+v", notice)
	}
	if err := p.AcknowledgeInterruption(t.Context(), notice); err != nil {
		t.Fatalf("AcknowledgeInterruption: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(targets, ","); got != "AmazonSQS.ReceiveMessage,AmazonSQS.DeleteMessage" {
		t.Errorf("targets = %s", got)
	}
}
