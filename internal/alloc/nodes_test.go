package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestRegisteredNodesReportSiteAndLivenessInNameOrder(t *testing.T) {
	t.Parallel()

	a, err := New(openState(t), Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	registrations := []NodeRegistration{
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "legacy", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB},
	}
	var edgeEpoch int64
	for _, registration := range registrations {
		epoch, err := a.RegisterNode(t.Context(), registration)
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", registration.Name, err)
		}
		if registration.Name == "edge-1" {
			edgeEpoch = epoch
		}
	}
	if err := a.NodeGone(t.Context(), "edge-1", edgeEpoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	got, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}
	want := []RegisteredNode{
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", Live: false},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "legacy", Provider: config.ProviderDocker, Site: "", Live: true},
	}
	if len(got) != len(want) {
		t.Fatalf("RegisteredNodes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RegisteredNodes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRegisteredNodesRefuseCorruptPlacementIdentity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *Allocator)
		wantErr string
	}{
		{
			name: "unknown provider",
			corrupt: func(t *testing.T, a *Allocator) {
				t.Helper()
				if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(),
						`UPDATE nodes SET provider = 'bogus' WHERE name = 'z-corrupt'`)

					return err
				}); err != nil {
					t.Fatalf("corrupt provider: %v", err)
				}
			},
			wantErr: `unknown provider "bogus"`,
		},
		{
			name: "invalid liveness",
			corrupt: func(t *testing.T, a *Allocator) {
				t.Helper()
				if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(),
						`PRAGMA ignore_check_constraints = ON`); err != nil {
						return err
					}
					_, updateErr := tx.ExecContext(t.Context(),
						`UPDATE nodes SET live = 2 WHERE name = 'z-corrupt'`)
					_, resetErr := tx.ExecContext(t.Context(),
						`PRAGMA ignore_check_constraints = OFF`)

					return errors.Join(updateErr, resetErr)
				}); err != nil {
					t.Fatalf("corrupt liveness: %v", err)
				}
			},
			wantErr: "invalid liveness 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := New(openState(t), Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for _, name := range []string{"a-valid", "z-corrupt"} {
				if _, err := a.RegisterNode(t.Context(), NodeRegistration{
					Name: name, Provider: config.ProviderDocker, Site: "home",
					VCPU: 8, Memory: 32 * config.GiB,
				}); err != nil {
					t.Fatalf("RegisterNode(%s): %v", name, err)
				}
			}
			tc.corrupt(t, a)

			got, err := a.RegisteredNodes(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RegisteredNodes error = %v, want one containing %q", err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("RegisteredNodes returned partial results after corruption: %+v", got)
			}
		})
	}
}
