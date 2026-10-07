package app

import (
	"errors"
	"io"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// EVERY ROLE ASSEMBLES, AND EVERY PIECE IT DECLARES IS THERE. The control plane
// is opened over a migrated SQLite ledger and walked to its scheduler; the node
// is opened over docker (whose constructor reaches no daemon) and closed; and
// each ledger mode opens, or refuses, the way the config it is given says. A
// role whose assembly stopped producing a piece it hands out fails here rather
// than at a host's first start.
func TestEveryRoleAssembles(t *testing.T) {
	t.Parallel()

	t.Run("control plane", func(t *testing.T) {
		t.Parallel()

		ctx := t.Context()
		dir := ledgertest.Dir(t)

		cfg := &config.Config{Server: &config.ServerConfig{
			Listen: "127.0.0.1:0", IdentityDir: dir, MaxVCPU: 8, MaxMemory: 16 * config.GiB,
		}}

		host := Host{
			ServerAccess: noIdentityAccess, AuthorityLock: noIdentityAccess,
			Ready: func() error { return nil }, Status: func(string) error { return nil }, Out: io.Discard,
		}

		cp, err := OpenControlPlane(ctx, cfg, host, nil, ControlPlaneOptions{})
		if err != nil {
			t.Fatalf("OpenControlPlane: %v", err)
		}

		t.Cleanup(func() { _ = cp.Close() })

		ctl, err := cp.BecomeController(ctx, func() {})
		if err != nil {
			t.Fatalf("BecomeController: %v", err)
		}

		defer ctl.Close()

		want, err := state.DeploymentID(dir)
		if err != nil {
			t.Fatalf("DeploymentID: %v", err)
		}

		if ctl.Allocator() == nil || ctl.Ledger() == nil || ctl.Deployment() != want {
			t.Errorf("the controller hands out allocator %v, ledger %v, deployment %q; want all of them, %q",
				ctl.Allocator(), ctl.Ledger(), ctl.Deployment(), want)
		}

		adopted, err := ctl.AdoptAuthority(ctx)
		if err != nil {
			t.Fatalf("AdoptAuthority: %v", err)
		}

		fleet, err := ctl.ForgetFleet(ctx)
		if err != nil {
			t.Fatalf("ForgetFleet: %v", err)
		}

		wire, err := ctl.ServeWire(ctx, fleet, adopted)
		if err != nil {
			t.Fatalf("ServeWire: %v", err)
		}

		defer wire.Stop()

		if wire.Plane() == nil || wire.Addr == "" {
			t.Errorf("the served wire has plane %v and address %q", wire.Plane(), wire.Addr)
		}

		published, err := ctl.PublishAuthority(ctx, wire)
		if err != nil {
			t.Fatalf("PublishAuthority: %v", err)
		}

		if scheduler, err := ctl.Schedule(wire, published, ScheduleOptions{}); err != nil || scheduler == nil {
			t.Fatalf("Schedule = (%v, %v), want a scheduler", scheduler, err)
		}
	})

	t.Run("node", func(t *testing.T) {
		t.Parallel()

		cfg := openNodeConfig(t, t.TempDir(), t.TempDir())
		cfg.Node.Provider = config.ProviderDocker

		n, err := OpenNode(cfg, NodeOptions{})
		if err != nil {
			t.Fatalf("OpenNode: %v", err)
		}

		if n.Name() != "host-1" || n.Deployment() == "" || n.provider == nil || n.client == nil {
			t.Errorf("the node is %q in %q with provider %v and client %v; want all of them",
				n.Name(), n.Deployment(), n.provider, n.client)
		}

		if err := n.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("ledger modes", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{Server: &config.ServerConfig{IdentityDir: ledgertest.Dir(t)}}

		for _, mode := range []LedgerMode{LedgerControlPlane, LedgerMaintenance} {
			db, err := OpenLedger(t.Context(), cfg, mode)
			if err != nil {
				t.Fatalf("OpenLedger(%d) on SQLite: %v", mode, err)
			}

			if err := db.Close(); err != nil {
				t.Errorf("close the ledger opened in mode %d: %v", mode, err)
			}
		}

		// A STANDBY IS POSTGRESQL'S, and a SQLite deployment asking for one is
		// refused rather than opened as a controller that cannot write.
		if db, err := OpenLedger(t.Context(), cfg, LedgerStandby); err == nil {
			_ = db.Close()

			t.Error("a SQLite ledger was opened as a standby")
		} else if errors.Is(err, state.ErrStandby) {
			t.Errorf("the standby open refused for the wrong reason: %v", err)
		}
	})
}

// WHAT A HARNESS STEERS IS APPLIED, EACH ONCE. The end-to-end suite and the
// replay harness reach the allocator, the node plane, the scheduler and the
// provisioner only through Steering, so a field the assembly stopped applying
// would leave a harness running on defaults while it believed it had steered
// them, and most of what they steer (pacing, an ordered session) changes no
// assertion when it is lost. Each option here records that it was applied.
func TestSteeringIsApplied(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	var allocations, planes, schedulers, provisioners int

	steering := &Steering{
		Allocator: []alloc.Option{func(*alloc.Allocator) { allocations++ }},
		Plane:     []nodeplane.Option{func(*nodeplane.Plane) { planes++ }},
		Server:    []server.ControlPlaneOption{func(*server.Server) { schedulers++ }},
		Provisioner: func(p server.Provisioner) server.Provisioner {
			provisioners++

			return p
		},
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen: "127.0.0.1:0", IdentityDir: ledgertest.Dir(t), MaxVCPU: 8, MaxMemory: 16 * config.GiB,
	}}

	host := Host{
		ServerAccess: noIdentityAccess, AuthorityLock: noIdentityAccess,
		Ready: func() error { return nil }, Status: func(string) error { return nil }, Out: io.Discard,
	}

	targets := []Target{{
		Config: config.GitHubTarget{Name: config.DefaultTargetName, Org: "acme"},
		Client: clientFor(t, billetgithub.OrganizationTarget("acme")),
	}}

	cp, err := OpenControlPlane(ctx, cfg, host, targets, ControlPlaneOptions{Steering: steering})
	if err != nil {
		t.Fatalf("OpenControlPlane: %v", err)
	}

	t.Cleanup(func() { _ = cp.Close() })

	ctl, err := cp.BecomeController(ctx, func() {})
	if err != nil {
		t.Fatalf("BecomeController: %v", err)
	}

	defer ctl.Close()

	adopted, err := ctl.AdoptAuthority(ctx)
	if err != nil {
		t.Fatalf("AdoptAuthority: %v", err)
	}

	fleet, err := ctl.ForgetFleet(ctx)
	if err != nil {
		t.Fatalf("ForgetFleet: %v", err)
	}

	wire, err := ctl.ServeWire(ctx, fleet, adopted)
	if err != nil {
		t.Fatalf("ServeWire: %v", err)
	}

	defer wire.Stop()

	published, err := ctl.PublishAuthority(ctx, wire)
	if err != nil {
		t.Fatalf("PublishAuthority: %v", err)
	}

	if _, err := ctl.Schedule(wire, published, ScheduleOptions{}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	for what, n := range map[string]int{
		"the allocator option": allocations, "the node plane option": planes,
		"the scheduler option": schedulers, "the provisioner wrapper": provisioners,
	} {
		if n != 1 {
			t.Errorf("%s was applied %d times, want once", what, n)
		}
	}
}
