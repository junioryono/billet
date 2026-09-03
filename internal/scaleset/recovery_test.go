package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	billetgithub "github.com/junioryono/billet/internal/github"
)

type recoveryPolicy struct {
	recovery billetgithub.RunnerRecovery
	err      error
	calls    int
	name     string
	id       int64
}

func TestRunnerRemovalRefusesAStaticReplacementBeforeDelete(t *testing.T) {
	var deletes atomic.Int32
	fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, `{"count":1,"value":[{"id":79,"name":"billet-l1","runnerScaleSetId":0}]}`)
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected runner request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	client, err := New(fake.config(t), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := client.RemoveRunner(t.Context(), 79, "billet-l1"); err == nil {
		t.Fatal("RemoveRunner accepted a static replacement")
	}
	if got := deletes.Load(); got != 0 {
		t.Fatalf("runner DELETE calls = %d, want 0", got)
	}
}

// The recovery fake does not exercise group lookup; it exists to satisfy the
// interface, and returns a refusal rather than a plausible id so a test that
// starts depending on it fails loudly instead of silently believing a group.
func (*recoveryPolicy) FindRunnerGroupID(context.Context, string) (int, bool, error) {
	return 0, false, errors.New("recoveryPolicy does not resolve runner groups")
}

func (*recoveryPolicy) ValidateTrustedRunnerGroup(context.Context, int, []string) error {
	return nil
}

func (p *recoveryPolicy) InspectScaleSetRunner(
	_ context.Context, name string, id int64,
) (billetgithub.RunnerRecovery, error) {
	p.calls++
	p.name = name
	p.id = id

	return p.recovery, p.err
}

func TestRunnerRecoveryRequiresScaleSetAndOrganizationIdentityToAgree(t *testing.T) {
	tests := []struct {
		name       string
		actions    string
		policy     billetgithub.RunnerRecovery
		policyErr  error
		want       billetgithub.RunnerRecovery
		wantErr    bool
		wantPolicy bool
		wantID     int64
	}{
		{name: "absent from scale set service", actions: `{"count":0,"value":[]}`},
		{name: "null envelope", actions: `null`, wantErr: true},
		{name: "missing envelope", actions: `{}`, wantErr: true},
		{name: "null values", actions: `{"count":0,"value":null}`, wantErr: true},
		{name: "false absence", actions: `{"count":0,"value":[{"id":70,"name":"billet-l1","runnerScaleSetId":9}]}`, wantErr: true},
		{name: "missing value", actions: `{"count":1,"value":[]}`, wantErr: true},
		{name: "negative count", actions: `{"count":-1,"value":[]}`, wantErr: true},
		{name: "incomplete runner", actions: `{"count":1,"value":[{"id":70,"name":"billet-l1"}]}`, wantErr: true},
		{name: "busy exact scale set runner", actions: `{"count":1,"value":[{"id":71,"name":"billet-l1","runnerScaleSetId":9}]}`, policy: billetgithub.RunnerRecovery{RunnerID: 71, Present: true, Busy: true}, want: billetgithub.RunnerRecovery{RunnerID: 71, Present: true, Busy: true}, wantPolicy: true, wantID: 71},
		{name: "idle exact scale set runner", actions: `{"count":1,"value":[{"id":72,"name":"billet-l1","runnerScaleSetId":9}]}`, policy: billetgithub.RunnerRecovery{RunnerID: 72, Present: true}, want: billetgithub.RunnerRecovery{RunnerID: 72, Present: true}, wantPolicy: true, wantID: 72},
		{name: "organization disappearance", actions: `{"count":1,"value":[{"id":73,"name":"billet-l1","runnerScaleSetId":9}]}`, wantErr: true, wantPolicy: true, wantID: 73},
		{name: "static runner", actions: `{"count":1,"value":[{"id":74,"name":"billet-l1","runnerScaleSetId":0}]}`, wantErr: true},
		{name: "wrong name", actions: `{"count":1,"value":[{"id":75,"name":"replacement","runnerScaleSetId":9}]}`, wantErr: true},
		{name: "missing id", actions: `{"count":1,"value":[{"name":"billet-l1","runnerScaleSetId":9}]}`, wantErr: true},
		{name: "organization replacement", actions: `{"count":1,"value":[{"id":76,"name":"billet-l1","runnerScaleSetId":9}]}`, policy: billetgithub.RunnerRecovery{RunnerID: 77, Present: true}, wantErr: true, wantPolicy: true, wantID: 76},
		{name: "organization failure", actions: `{"count":1,"value":[{"id":78,"name":"billet-l1","runnerScaleSetId":9}]}`, policyErr: fmt.Errorf("unavailable"), wantErr: true, wantPolicy: true, wantID: 78},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tenant/123/_apis/distributedtask/pools/0/agents" {
					t.Errorf("runner lookup path = %q", r.URL.Path)
				}
				if got := r.URL.Query().Get("agentName"); got != "billet-l1" {
					t.Errorf("runner lookup name = %q", got)
				}
				fmt.Fprint(w, tc.actions)
			})
			client, err := New(fake.config(t), slog.Default())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			policy := &recoveryPolicy{recovery: tc.policy, err: tc.policyErr}
			client.policy = policy

			got, err := client.RecoverRunner(t.Context(), "billet-l1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RecoverRunner succeeded: %+v", got)
				}
			} else {
				if err != nil {
					t.Fatalf("RecoverRunner: %v", err)
				}
				if got != tc.want {
					t.Errorf("recovery = %+v, want %+v", got, tc.want)
				}
			}
			if tc.wantPolicy != (policy.calls == 1) {
				t.Errorf("policy calls = %d, want called %v", policy.calls, tc.wantPolicy)
			}
			if policy.calls == 1 && (policy.name != "billet-l1" || policy.id != tc.wantID) {
				t.Errorf("policy identity = %q/%d, want billet-l1/%d", policy.name, policy.id, tc.wantID)
			}
		})
	}
}
