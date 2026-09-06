package nodeplane_test

import (
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

func TestInventoryCannotGrantOwnershipAgainstLedgerPlacement(t *testing.T) {
	t.Parallel()

	for _, route := range []string{"register", "register-without-registrar", "reconcile"} {
		for _, placement := range []string{"bound", "escrow", "unreadable", "own"} {
			t.Run(route+"/"+placement, func(t *testing.T) {
				t.Parallel()

				lease := &alloc.Lease{ID: "l1", Epoch: 1}
				var readErr error
				switch placement {
				case "bound":
					lease.Node = "other"
				case "escrow":
					lease.TargetNode = "other"
				case "unreadable":
					readErr = errors.New("ledger unavailable")
				case "own":
					lease.Node = "n1"
				}
				registrar := &fakeRegistrar{lease: lease, leaseErr: readErr}
				store := &fakeStore{lease: lease, leaseErr: readErr}
				var options []nodeplane.Option
				if route != "register-without-registrar" {
					options = append(options, nodeplane.WithRegistrar(registrar))
				}
				plane, base := serve(t, store, options...)
				client := dial(t, base)
				var err error
				if strings.HasPrefix(route, "register") {
					err = client.Register(t.Context(), nodeclient.Registration{
						Provider: config.ProviderDocker, Deployment: deployment,
						VCPU: testNodeVCPU, Memory: testNodeMemory,
						InventoryKnown: true, Instances: []string{"l1"},
					})
				} else {
					_, err = client.Reconcile(t.Context(), "n1", []string{"l1"})
				}
				owner, owned := plane.OwnerOfLease("l1")
				if placement == "own" {
					if err != nil || !owned || owner.Node != "n1" {
						t.Fatalf("legitimate inventory: err=%v owner=%+v present=%v", err, owner, owned)
					}
					return
				}
				if err == nil || owned {
					t.Fatalf("inventory granted unproved ownership: err=%v owner=%+v present=%v", err, owner, owned)
				}
				if placement == "unreadable" {
					if !strings.Contains(err.Error(), "ledger unavailable") {
						t.Fatalf("inventory did not report the failed ledger read: %v", err)
					}
				} else if !errors.Is(err, nodeclient.ErrRefused) {
					t.Fatalf("foreign inventory did not receive a placement refusal: %v", err)
				}
				if called, _ := registrar.reconciliation(); called {
					t.Fatal("unverified inventory reached capacity reconciliation")
				}
			})
		}
	}
}

func TestAdoptedEndedInventoryDoesNotAuthorizeForeignRunnerRemoval(t *testing.T) {
	t.Parallel()

	for _, history := range []string{"n1", "n2", ""} {
		t.Run("history="+history, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{
				leaseErr:    alloc.ErrLeaseNotFound,
				historyNode: map[string]string{"l1": history},
				pool: map[string]alloc.PoolRunner{"runner": {
					LeaseID: "l1", Tier: "billet-2vcpu", RunnerID: 91,
					RunnerName: "runner", Status: alloc.PoolRunnerBusy,
				}},
			}
			jit := &returnedJIT{}
			log := slog.New(slog.DiscardHandler)
			p := nodeplane.New(log, deployment, time.Minute)
			srv := httptest.NewServer(nodeplane.Handler(log, p, store, jit))
			t.Cleanup(srv.Close)
			c := dial(t, srv.URL)
			if err := c.Register(t.Context(), nodeclient.Registration{
				Provider: config.ProviderDocker, Deployment: deployment,
				VCPU: testNodeVCPU, Memory: testNodeMemory,
				InventoryKnown: true, Instances: []string{"l1"},
			}); err != nil {
				t.Fatal(err)
			}
			if owner, ok := p.OwnerOfLease("l1"); !ok || owner.Node != "n1" {
				t.Fatalf("ended inventory was not adopted: owner=%+v present=%v", owner, ok)
			}
			err := c.EnsureRunnerRemoved(t.Context(), "l1")
			if history == "n1" {
				if err != nil || len(jit.removed) != 1 || len(store.retired) != 1 {
					t.Fatalf("own ended runner removal: err=%v removed=%v retired=%v", err, jit.removed, store.retired)
				}
			} else if !errors.Is(err, nodeclient.ErrRefused) || len(jit.removed) != 0 || len(store.retired) != 0 {
				t.Fatalf("unproved ended runner removal: err=%v removed=%v retired=%v", err, jit.removed, store.retired)
			}
		})
	}
}
