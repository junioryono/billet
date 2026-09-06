package nodeplane_test

import (
	"errors"
	"strings"
	"testing"

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
