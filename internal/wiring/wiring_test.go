package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/node"
)

type recoveryPool struct {
	binding   alloc.PoolRunner
	err       error
	retired   []string
	retireErr error
}

func (p *recoveryPool) PoolRunnerByLease(context.Context, string) (alloc.PoolRunner, error) {
	return p.binding, p.err
}

func (p *recoveryPool) RetirePoolRunner(_ context.Context, leaseID string) error {
	p.retired = append(p.retired, leaseID)
	return p.retireErr
}

type recoveryClient struct {
	recovery   billetgithub.RunnerRecovery
	recoverErr error
	recovered  []string
	removed    []string
	removeID   int64
	removeErr  error
}

func (c *recoveryClient) RecoverRunner(
	_ context.Context, name string,
) (billetgithub.RunnerRecovery, error) {
	c.recovered = append(c.recovered, name)
	return c.recovery, c.recoverErr
}

func (c *recoveryClient) RemoveRunner(_ context.Context, id int64, name string) error {
	c.removeID = id
	c.removed = append(c.removed, name)
	return c.removeErr
}

func TestLocalLegacyRunnerRecoveryFailsClosedAndRemovesOnlyIdle(t *testing.T) {
	tests := []struct {
		name        string
		pool        *recoveryPool
		client      *recoveryClient
		want        node.RunnerRecovery
		wantRecover bool
		wantRemove  bool
		wantRetire  bool
		wantErr     bool
	}{
		{name: "idle durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 69, Present: true}}, want: node.RunnerRecoveryRetired, wantRecover: true, wantRemove: true, wantRetire: true},
		{name: "busy race on idle durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 69, Present: true, Busy: true}}, want: node.RunnerRecoveryBusy, wantRecover: true},
		{name: "replacement race on idle durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 70, Present: true}}, wantRecover: true, wantErr: true},
		{name: "absent idle durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired, wantRecover: true, wantRetire: true},
		{name: "idle durable removal failure", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 69, Present: true}, removeErr: errors.New("github unavailable")}, wantRecover: true, wantRemove: true, wantRetire: true, wantErr: true},
		{name: "busy durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, Status: alloc.PoolRunnerBusy}}, client: &recoveryClient{}, want: node.RunnerRecoveryTracked},
		{name: "retiring durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 70, RunnerName: "billet-l1", Status: alloc.PoolRunnerRetiring}}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired, wantRemove: true},
		{name: "retired durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, Status: alloc.PoolRunnerRetired}}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired},
		{name: "absent", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired, wantRecover: true},
		{name: "busy", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 71, Present: true, Busy: true}}, want: node.RunnerRecoveryBusy, wantRecover: true},
		{name: "idle", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 72, Present: true}}, want: node.RunnerRecoveryRetired, wantRecover: true, wantRemove: true},
		{name: "lookup failure", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recoverErr: errors.New("github unavailable")}, wantRecover: true, wantErr: true},
		{name: "removal failure", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 73, Present: true}, removeErr: errors.New("github unavailable")}, wantRecover: true, wantRemove: true, wantErr: true},
		{name: "durable mismatch", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "other", LaunchRequestID: 7, Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := recoverRunner(t.Context(), tc.pool, tc.client,
				"l1", "linux", 7, "billet-l1")
			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatalf("recoverRunner succeeded with disposition %q", got)
				}
			case err != nil:
				t.Fatalf("recoverRunner: %v", err)
			case got != tc.want:
				t.Errorf("disposition = %q, want %q", got, tc.want)
			}
			if tc.wantRecover != (len(tc.client.recovered) == 1) {
				t.Errorf("recovery calls = %v, want call %v", tc.client.recovered, tc.wantRecover)
			}
			if tc.wantRemove != (len(tc.client.removed) == 1) {
				t.Errorf("removal calls = %v, want call %v", tc.client.removed, tc.wantRemove)
			}
			if tc.wantRetire != (len(tc.pool.retired) == 1) {
				t.Errorf("retirement calls = %v, want call %v", tc.pool.retired, tc.wantRetire)
			}
			if tc.wantRemove && (tc.client.removeID == 0 || tc.client.removed[0] != "billet-l1") {
				t.Errorf("removed id %d names %v", tc.client.removeID, tc.client.removed)
			}
		})
	}
}
