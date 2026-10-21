package app

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// namedWaiter is an admission order with one waiter ahead of every tier, which
// records the tier it was asked about and decides nothing itself.
type namedWaiter struct{ asked []string }

func (o *namedWaiter) ResizeAdmission(_ context.Context, tier string) []string {
	o.asked = append(o.asked, tier)

	return []string{"waiter"}
}

// THE GATE ASKS THE ORDER FOR THE LEASE'S OWN TIER AND HANDS ITS ANSWER TO THE
// ALLOCATOR (#172). A cloud lease escrowed at 4 vCPU asks to grow to 8 under a
// 12 vCPU ceiling while an 8 vCPU tier waits on a host of its own: the growth
// would leave the waiter 4 of the 8 it needs. Bound to an order naming that
// waiter, the gate refuses it and the lease keeps its charge; unbound, before
// the scheduler exists, the store's own budget grants it.
func TestTheResizeGateAsksTheOrderForTheLeasesTier(t *testing.T) {
	t.Parallel()

	cloud := config.Tier{Label: "cloud", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-test"}
	waiter := config.Tier{Label: "waiter", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux,
		VCPU: 8, Memory: 16 * config.GiB, Image: "ubuntu-2404-x64", Node: "home"}

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 12, MaxMemory: 256 * config.GiB}, []config.Tier{cloud, waiter})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	for _, reg := range []alloc.NodeRegistration{
		{Name: "cloud-1", Provider: config.ProviderEC2, VCPU: 64, Memory: 128 * config.GiB,
			EC2Shapes: []config.EC2InstanceType{
				{Type: "four", VCPU: 4, Memory: 8 * config.GiB},
				{Type: "eight", VCPU: 8, Memory: 16 * config.GiB},
			}},
		{Name: "home", Provider: config.ProviderFirecracker, VCPU: 8, Memory: 16 * config.GiB},
	} {
		if _, err := a.RegisterNode(t.Context(), reg); err != nil {
			t.Fatalf("RegisterNode(%s): %v", reg.Name, err)
		}
	}

	lease, err := a.Reserve(t.Context(), "cloud")
	if err != nil || lease.VCPU != 4 {
		t.Fatalf("Reserve = %+v, %v; want the 4 vCPU shape", lease, err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 1, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "cloud-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	charged := func() int {
		t.Helper()

		got, err := a.Lease(t.Context(), lease.ID)
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}

		return got.VCPU
	}

	// BOUND, through the route's own entry point, as Schedule leaves it.
	bound := &resizeGate{alloc: a}
	order := &namedWaiter{}
	bound.bind(order)

	err = bound.resize(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB)
	if !errors.Is(err, alloc.ErrNoCapacity) || !strings.Contains(err.Error(), "admission order holds for waiter") {
		t.Fatalf("a growth into the waiter's room answered %v, want the order's no-capacity", err)
	}

	if len(order.asked) != 1 || order.asked[0] != "cloud" {
		t.Errorf("the order was asked about %v, want the lease's own tier once", order.asked)
	}

	if got := charged(); got != 4 {
		t.Errorf("the refused growth left the lease charged %d vCPU, want 4", got)
	}

	// UNBOUND, the store's budget alone answers, and it allows this growth.
	unbound := &resizeGate{alloc: a}
	if err := unbound.resize(t.Context(), lease.ID, lease.Epoch, "eight", 8, 16*config.GiB); err != nil {
		t.Fatalf("the unbound gate refused a growth the budget allows: %v", err)
	}

	if got := charged(); got != 8 {
		t.Errorf("the unbound gate's growth left the lease charged %d vCPU, want 8", got)
	}
}

// leaseOnlyStore answers the one ledger read the resize route's guard makes,
// placing every lease on node, and answers a resize itself with a fence, so a
// route that asked the store rather than the resizer fails as fenced. Every
// other method is the nil interface's, and a route that reached one panics.
type leaseOnlyStore struct {
	nodeplane.LeaseStore

	node string
}

func (s leaseOnlyStore) Lease(_ context.Context, id string) (*alloc.Lease, error) {
	return &alloc.Lease{ID: id, Node: s.node}, nil
}

func (leaseOnlyStore) Resize(context.Context, string, int64, string, int, config.ByteSize) error {
	return alloc.ErrFenced
}

// LaunchedLeaseIDs and QuarantinedLeaseIDs are what registration reads: a node
// holding nothing.
func (leaseOnlyStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (leaseOnlyStore) QuarantinedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// THE SERVED WIRE'S RESIZE ROUTE IS THE RESIZER ServeNodeWire WAS GIVEN (#172):
// a node's resize, sent over the wire ServeNodeWire serves, reaches it through
// BuildNodeWire and the handler, and its refusal comes back as the no-capacity
// a fallback understands. Dropping the resizer anywhere on that path answers
// from the store, which says fenced.
func TestTheServedWireAnswersAResizeWithItsResizer(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{Listen: "127.0.0.1:0", IdentityDir: stateDir}}

	var asked []string

	resizer := func(_ context.Context, leaseID string, _ int64, instanceType string, _ int, _ config.ByteSize) error {
		asked = append(asked, leaseID+" "+instanceType)

		return fmt.Errorf("%w: the admission order holds this room for org-16", alloc.ErrNoCapacity)
	}

	served, err := ServeNodeWire(t.Context(), cfg, noIdentityAccess,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		leaseOnlyStore{node: "n1"}, nil, nil, nil, nil, withResizer(resizer))
	if err != nil {
		t.Fatalf("serving the node wire: %v", err)
	}

	t.Cleanup(served.Stop)

	c, err := nodeclient.New(nodeclient.Options{Base: "http://" + served.Addr, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider: config.ProviderEC2, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: deploymentID, VCPU: 16, Memory: 64 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	err = c.Resize(t.Context(), "lease-1", 1, "large", 16, 32*config.GiB)
	if !errors.Is(err, alloc.ErrNoCapacity) {
		t.Fatalf("the served wire answered the resize with %v, want the resizer's no-capacity", err)
	}

	if len(asked) != 1 || asked[0] != "lease-1 large" {
		t.Errorf("the resizer was asked %v, want once for lease-1's large shape", asked)
	}
}

// THE ROUTE ASKS THE GATE THE WIRE CARRIES (#172). Schedule binds the gate the
// ServingWire holds, and TestTheControlPlaneAssemblesStepByStep proves it is
// bound to the scheduler it built; this is the other half, that ServeWire hands
// the node wire's resize route that same gate's resize. Asserted on the source
// because reaching the route for real needs a registered remote node holding a
// launching lease, and a route served with no resizer answers from the store,
// which every other test would still accept.
func TestServeWireHandsTheResizeRouteTheGateItKeeps(t *testing.T) {
	t.Parallel()

	serve := findMethod(t, "Controller", "ServeWire")

	var passed, kept string

	ast.Inspect(serve, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if calleeName(n) != "withResizer" || len(n.Args) != 1 {
				return true
			}

			if sel, ok := n.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "resize" {
				if id, ok := sel.X.(*ast.Ident); ok {
					passed = id.Name
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && key.Name == "resizes" {
				if id, ok := n.Value.(*ast.Ident); ok {
					kept = id.Name
				}
			}
		}

		return true
	})

	if passed == "" {
		t.Fatal("ServeWire does not hand the node wire a gate's resize through withResizer")
	}

	if kept != passed {
		t.Errorf("ServeWire hands the route %q's resize and keeps %q on the wire, so Schedule binds a gate "+
			"the route never asks", passed, kept)
	}
}
