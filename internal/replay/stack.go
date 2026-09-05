package replay

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider/simulated"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wiring"
)

// stack is billet, assembled the way cmd/billet assembles it: a real control
// plane and ledger, a real node wire on loopback, and one real node runtime per
// simulated host, all over the scripted Actions service.
type stack struct {
	db     *state.DB
	alloc  *alloc.Allocator
	plane  *nodeplane.Plane
	server *server.Server

	closeDB   func()
	stopNodes func()
	hurry     chan struct{}
	hurryOnce sync.Once
}

// Real-time bounds on the harness's own waits. None of them is a simulated
// duration; they are how long a real goroutine may take to do something the
// harness asked for before the replay is declared stuck.
const (
	registrationWait = 30 * time.Second
	shutdownWait     = 60 * time.Second
	wireShutdownWait = 30 * time.Second
	// planePoll is the long-poll window nodes are told to wait out. Short, so a
	// stopping node loop returns quickly; it also sets how long a silent node
	// survives (four windows), which no simulated host approaches while the
	// plane stays on wall time.
	planePoll = 2 * time.Second
)

// buildStack stands billet up over the fleet.
//
// WHICH CLOCKS MOVE. The allocator and every simulated provider read the
// harness's clock: leases are dated, expired and archived in simulated time and
// an instance runs for a simulated duration. The node plane, the listener's
// heartbeat and the node loops stay on wall time, because their timings are
// facts about a real machine talking to a real one: a plane on the simulated
// clock would expire a node parked in a poll across a jump of hours. The reaper
// never ticks for the same reason the multi-day end-to-end scenario turns it
// off: a jumped clock manufactures an expiry continuous time cannot produce.
func buildStack(t *testing.T, log *slog.Logger, fleet Fleet, tiers []config.Tier, clock *Clock,
	actions *plane,
) *stack {
	t.Helper()

	client, err := scaleset.New(scaleset.Config{
		Target:         billetgithub.OrganizationTarget(DefaultOwner),
		GitHubURL:      actions.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     actions.PrivateKeyPEM(),
		AppID:          12345,
		APIURL:         actions.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	dir := t.TempDir()

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	closeDB := sync.OnceFunc(func() { _ = db.Close() })
	t.Cleanup(closeDB)

	a, err := alloc.New(db, fleet.limits(), tiers,
		alloc.WithClock(clock.Now), alloc.WithPlacement(fleet.Placement))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	plane := nodeplane.New(log, deployment, a.LeaseTTL(),
		nodeplane.WithRegistrar(a), nodeplane.WithTierCatalog(tiers),
		nodeplane.WithBarrierStore(a), nodeplane.WithPollTimeout(planePoll))

	wire, err := wiring.BuildNodeWire(wiring.NodeWireRequest{
		Loopback:    true,
		Log:         log,
		Plane:       plane,
		Leases:      a,
		JIT:         wiring.NodeJIT{Client: client},
		CachePolicy: db,
	})
	if err != nil {
		t.Fatalf("BuildNodeWire: %v", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	wireServer := &http.Server{Handler: wire.Handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := wireServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the node wire stopped unexpectedly: %v", err)
		}
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), wireShutdownWait)
		defer cancel()

		if err := wireServer.Shutdown(ctx); err != nil {
			t.Errorf("the node wire did not shut down cleanly: %v", err)
		}
	})

	// THE OPERATOR'S SECOND SIGNAL, wired as `billet node` and `billet server`
	// wire it. A replay ends with nothing running, so a drain ends by itself;
	// closing this at stop is what keeps a replay that failed half way from
	// hanging in cleanup instead of in the assertion that named the failure.
	hurry := make(chan struct{})

	loopCtx, cancelLoops := context.WithCancel(t.Context())

	var loops sync.WaitGroup

	for _, h := range fleet.Hosts {
		nc, err := nodeclient.New(nodeclient.Options{Base: "http://" + ln.Addr().String(), Node: h.Name})
		if err != nil {
			t.Fatalf("nodeclient.New(%s): %v", h.Name, err)
		}

		prov, err := simulated.New(deployment, simulated.WithClock(clock.Now), simulated.WithLogger(log))
		if err != nil {
			t.Fatalf("simulated.New(%s): %v", h.Name, err)
		}

		// THE CLIENT IS BOTH LEDGER AND MINT, exactly as `billet node` wires it.
		runner := node.New(nc, h.Name, nc, prov, log)

		loops.Go(func() {
			err := nodeclient.Run(loopCtx, nc, runner, nodeclient.LoopOptions{
				Provider:   config.ProviderSimulated,
				Deployment: deployment,
				Site:       h.Site,
				VCPU:       h.VCPU,
				Memory:     h.Memory,
				Log:        log,
				Backoff:    50 * time.Millisecond,
				Hurry:      hurry,
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("node %s stopped for a reason other than shutdown: %v", h.Name, err)
			}
		})
	}

	stopNodes := sync.OnceFunc(func() {
		cancelLoops()
		loops.Wait()
	})
	t.Cleanup(stopNodes)

	// REGISTERED BEFORE THE CONTROL PLANE HAS ANYTHING TO GIVE THEM, or the first
	// escrow finds a partial fleet and the replay measures startup order.
	deadline := time.Now().Add(registrationWait)

	for len(plane.Nodes()) < len(fleet.Hosts) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d hosts registered over the wire", len(plane.Nodes()), len(fleet.Hosts))
		}

		time.Sleep(20 * time.Millisecond)
	}

	prov := orderedSessions{Provisioner: wiring.Provisioner{Client: client}, actions: actions}

	// The one target, assembled the way the CLI assembles one, so the scale-set
	// record carries the owner's path and every tier resolves through it.
	ctl := server.New(a, nil, tiers, owner, log,
		server.WithNodeRunner(plane.NewRunner()),
		server.WithCompletionLedger(db),
		server.WithTargets(server.Target{
			Config:      config.GitHubTarget{Name: config.DefaultTargetName, Org: DefaultOwner},
			Provisioner: prov,
		}),
		// NEVER, for a replay: a jumped clock would expire every lease between two
		// heartbeats. The startup reap still runs, on an empty ledger.
		server.WithReapInterval(24*time.Hour),
		server.WithDrainTimeout(time.Hour),
		server.WithHurry(hurry))

	return &stack{
		db: db, alloc: a, plane: plane, server: ctl,
		closeDB: closeDB, stopNodes: stopNodes, hurry: hurry,
	}
}

// orderedSessions is the CLI's provisioner with one addition: a tier's session
// is asked for only once every tier before it in the fleet's order has its
// listener parked.
//
// THE SECOND THING THE HARNESS STEERS, BESIDE THE CLOCK, and it steers an order
// GitHub itself leaves to chance. Listeners start together and each escrows its
// discovery slot as soon as its session opens; escrow is placement, so with two
// tiers of different sizes the host each slot lands on depends on which
// goroutine ran first, and every later job on that tier inherits it. Waiting
// HERE rather than in the scripted service's handler, because the scale-set
// client serialises its calls under one mutex and a request held open in the
// handler would hold every other tier's request behind it.
type orderedSessions struct {
	wiring.Provisioner

	actions *plane
}

// Session waits for this set's turn, then opens the session the real adapter
// opens.
func (o orderedSessions) Session(ctx context.Context, scaleSetID int, owner string) (server.Session, error) {
	if err := o.actions.awaitTurn(ctx, scaleSetID); err != nil {
		return nil, err
	}

	return o.Provisioner.Session(ctx, scaleSetID, owner)
}

// run starts the control plane and returns its stop.
//
// A Run that ends before it is asked to is reported at once rather than at
// stop, so a control plane that died at startup names itself instead of
// whichever wait timed out first.
func (s *stack) run(t *testing.T) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))

	var runErr error

	finished := make(chan struct{})

	go func() {
		runErr = s.server.Run(ctx)

		close(finished)
	}()

	stopped := make(chan struct{})

	go func() {
		select {
		case <-finished:
			select {
			case <-stopped:
			default:
				t.Errorf("the control plane stopped on its own: %v", runErr)
			}
		case <-stopped:
		}
	}()

	stop := sync.OnceFunc(func() {
		close(stopped)
		cancel()
		s.hurryOnce.Do(func() { close(s.hurry) })

		limit := time.NewTimer(shutdownWait)
		defer limit.Stop()

		select {
		case <-finished:
		case <-limit.C:
			t.Error("the control plane did not stop")
		}
	})

	t.Cleanup(stop)

	return stop
}
