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

func (p *recoveryPool) PreserveRecoveredBusyPoolRunner(_ context.Context, runner alloc.PoolRunner) error {
	if p.err != nil && !errors.Is(p.err, alloc.ErrLeaseNotFound) {
		return p.err
	}
	if p.binding.LeaseID == "" {
		p.binding = runner
	}
	p.binding.RunnerID = runner.RunnerID
	p.binding.RunnerName = runner.RunnerName
	p.binding.Status = alloc.PoolRunnerBusy
	p.err = nil
	return nil
}

func (p *recoveryPool) RetireRecoveredPoolRunner(
	_ context.Context, recovered alloc.PoolRunner,
) (alloc.PoolRunner, error) {
	if p.binding.LeaseID == "" {
		recovered.Status = alloc.PoolRunnerRetiring
		p.binding = recovered
		p.err = nil
		p.retired = append(p.retired, recovered.LeaseID)
		return p.binding, nil
	}
	if p.binding.Status == alloc.PoolRunnerBusy &&
		(p.binding.ActualRequestID != 0 || p.binding.RunID != 0 || p.binding.JobID != "") {
		return p.binding, nil
	}
	p.retired = append(p.retired, recovered.LeaseID)
	if p.retireErr != nil {
		return alloc.PoolRunner{}, p.retireErr
	}
	p.binding.Status = alloc.PoolRunnerRetiring
	return p.binding, nil
}

type recoveryClient struct {
	recovery   billetgithub.RunnerRecovery
	recoverErr error
	recovered  []string
	onRecover  func()
	removed    []string
	removeID   int64
	removeErr  error
	onRemove   func()
}

func (c *recoveryClient) RecoverRunner(
	_ context.Context, name string,
) (billetgithub.RunnerRecovery, error) {
	c.recovered = append(c.recovered, name)
	if c.onRecover != nil {
		c.onRecover()
	}
	return c.recovery, c.recoverErr
}

func (c *recoveryClient) RemoveRunner(_ context.Context, id int64, name string) error {
	c.removeID = id
	c.removed = append(c.removed, name)
	if c.onRemove != nil {
		c.onRemove()
	}
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
		{name: "idle durable removal failure", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 69, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 69, Present: true}, removeErr: errors.New("github unavailable")}, wantRecover: true, wantRemove: true, wantErr: true},
		{name: "busy durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, Status: alloc.PoolRunnerBusy, ActualRequestID: 8}}, client: &recoveryClient{}, want: node.RunnerRecoveryTracked},
		{name: "retiring durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, RunnerID: 70, RunnerName: "billet-l1", Status: alloc.PoolRunnerRetiring}}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired, wantRemove: true},
		{name: "retired durable", pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7, Status: alloc.PoolRunnerRetired}}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired},
		{name: "absent", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{}, want: node.RunnerRecoveryRetired, wantRecover: true, wantRetire: true},
		{name: "busy", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 71, Present: true, Busy: true}}, want: node.RunnerRecoveryBusy, wantRecover: true},
		{name: "idle", pool: &recoveryPool{err: alloc.ErrLeaseNotFound}, client: &recoveryClient{recovery: billetgithub.RunnerRecovery{RunnerID: 72, Present: true}}, want: node.RunnerRecoveryRetired, wantRecover: true, wantRemove: true, wantRetire: true},
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

func TestLocalRecoveryLetsJobStartedWinEveryRemoteRace(t *testing.T) {
	started := alloc.PoolRunner{LeaseID: "l1", Tier: "linux", LaunchRequestID: 7,
		RunnerID: 71, RunnerName: "billet-l1", Status: alloc.PoolRunnerBusy,
		ActualRequestID: 8, RunID: 9, JobID: "job"}
	tests := []struct {
		name   string
		pool   *recoveryPool
		client *recoveryClient
	}{
		{
			name:   "started while GitHub lookup was in flight",
			pool:   &recoveryPool{err: alloc.ErrLeaseNotFound},
			client: &recoveryClient{onRecover: func() {}},
		},
		{
			name: "started after successful remote deletion",
			pool: &recoveryPool{binding: alloc.PoolRunner{LeaseID: "l1", Tier: "linux",
				LaunchRequestID: 7, RunnerID: 71, RunnerName: "billet-l1", Status: alloc.PoolRunnerIdle}},
			client: &recoveryClient{recovery: billetgithub.RunnerRecovery{
				RunnerID: 71, Present: true,
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.client.recovery.Present {
				tc.client.onRemove = func() {
					tc.pool.binding = started
					tc.pool.err = nil
				}
			} else {
				tc.client.onRecover = func() {
					tc.pool.binding = started
					tc.pool.err = nil
				}
			}
			got, err := recoverRunner(t.Context(), tc.pool, tc.client,
				"l1", "linux", 7, "billet-l1")
			if err != nil {
				t.Fatalf("recoverRunner: %v", err)
			}
			if got != node.RunnerRecoveryTracked {
				t.Fatalf("recovery disposition = %q, want tracked", got)
			}
			if tc.pool.binding.Status != alloc.PoolRunnerBusy || tc.pool.binding.JobID != "job" {
				t.Fatalf("started binding was changed: %+v", tc.pool.binding)
			}
		})
	}
}

func TestBusyLegacyRecoveryJournalsIdentityForBoundedTeardown(t *testing.T) {
	pool := &recoveryPool{err: alloc.ErrLeaseNotFound}
	client := &recoveryClient{recovery: billetgithub.RunnerRecovery{
		RunnerID: 71, Present: true, Busy: true,
	}}
	got, err := recoverRunner(t.Context(), pool, client, "l1", "linux", 7, "billet-l1")
	if err != nil {
		t.Fatalf("recoverRunner: %v", err)
	}
	if got != node.RunnerRecoveryBusy {
		t.Fatalf("recovery disposition = %q, want busy", got)
	}
	if pool.binding.LeaseID != "l1" || pool.binding.RunnerID != 71 ||
		pool.binding.RunnerName != "billet-l1" || pool.binding.Status != alloc.PoolRunnerBusy {
		t.Fatalf("durable recovered identity = %+v", pool.binding)
	}
}
