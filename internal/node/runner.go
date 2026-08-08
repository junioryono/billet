// Package node turns assigned leases into running compute.
//
// It is the in-process half of what will become `billet node`: the control plane
// hands it a lease, it mints a single-use runner registration, and it asks a
// provider to start something that consumes it. When the node splits out over
// mTLS, the remote side implements the same two methods and this becomes the
// local case rather than the only one.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// JITSource mints single-use runner registrations and finds the scale set to
// mint them against.
//
// An interface rather than the concrete client so this package does not depend
// on the preview scale-set API, and so a test can drive the whole launch path
// without a GitHub organization.
type JITSource interface {
	// Describe finds a tier's scale set. Returns a nil set when there is none.
	Describe(ctx context.Context, name, group string) (*Set, []string, error)
	// JITConfig mints a registration for one runner against one scale set.
	JITConfig(ctx context.Context, scaleSetID int, runnerName, workFolder string) (Registration, error)
}

// Set is the part of a scale set this package needs.
type Set struct {
	ID   int
	Name string
}

// Registration is a minted runner registration. The config inside is a
// CREDENTIAL until the runner consumes it.
type Registration interface {
	// Config returns the encoded JIT configuration.
	Config() string
	// RunnerName is what GitHub registered, which is what teardown needs.
	RunnerName() string
}

// Runner starts and stops the compute for assigned leases.
type Runner struct {
	jit      JITSource
	provider provider.Provider
	alloc    *alloc.Allocator
	log      *slog.Logger

	// node is this host's name, which is what leases are bound to. Placement is
	// enforced against the node REGISTERED under this name, so it has to match a
	// row in the nodes table rather than being decorative.
	node string

	// tiers is the catalog, so a lease's tier can be turned into a machine shape.
	tiers map[string]config.Tier

	mu sync.Mutex
	// running maps a request to what was started for it, which is the only way
	// Destroy knows what to remove.
	running map[int64]*provider.Instance
	// sets caches tier to scale-set id. Looked up once per tier rather than per
	// launch, because it does not change while the process runs and a lookup on
	// the launch path is a round trip in front of every job.
	sets map[string]int
}

// strayCleanupTimeout bounds the check after a failed launch. Short: it runs on
// an error path the caller is waiting on, and a provider that cannot answer
// quickly is better logged than waited for.
const strayCleanupTimeout = 30 * time.Second

// New builds a runner over a provider.
func New(
	a *alloc.Allocator, node string, jit JITSource, p provider.Provider,
	tiers []config.Tier, log *slog.Logger,
) *Runner {
	if log == nil {
		log = slog.Default()
	}

	byLabel := make(map[string]config.Tier, len(tiers))
	for i := range tiers {
		byLabel[tiers[i].Label] = tiers[i]
	}

	return &Runner{
		jit:      jit,
		provider: p,
		alloc:    a,
		node:     node,
		log:      log,
		tiers:    byLabel,
		running:  make(map[int64]*provider.Instance),
		sets:     make(map[string]int),
	}
}

var _ server.Runner = (*Runner)(nil)

// Launch mints a registration and starts something that will consume it.
func (r *Runner) Launch(ctx context.Context, lease *alloc.Lease, job Job) error {
	tier, ok := r.tiers[lease.Tier]
	if !ok {
		return fmt.Errorf("node: no tier named %q in the catalog", lease.Tier)
	}

	if tier.Provider != r.provider.Kind() {
		// Placement should have caught this, so reaching it means the catalog and
		// the host disagree. Refusing is the only safe answer: the alternative is
		// running a job on a backend its tier was never sized or trusted for.
		return fmt.Errorf("node: tier %s wants provider %q but this host runs %q",
			lease.Tier, tier.Provider, r.provider.Kind())
	}

	// ASKED BEFORE ANYTHING IRREVERSIBLE HAPPENS.
	//
	// Minting the registration first and being refused afterwards leaves a runner
	// registered on GitHub with nothing to consume it — and since every pull
	// request is refused by a container backend, that is one orphan per PR,
	// accumulating quietly until somebody notices the runner list.
	trust := provider.Classify(job.Event)

	if err := r.provider.Accepts(trust); err != nil {
		return fmt.Errorf("node: %w", err)
	}

	// BOUND TO THIS HOST BEFORE IT RUNS ANYWHERE.
	//
	// Bind is where the allocator enforces placement: the node's guest-OS
	// allowlist, its registered provider, the macOS licence cap, and the tier's
	// own pin. Launching without it meant `leases.node` stayed NULL and every one
	// of those checks was skipped — a tier pinned to another host would have run
	// here purely because the provider kinds happened to match.
	if err := r.alloc.Bind(ctx, lease.ID, lease.Epoch, r.node); err != nil {
		return fmt.Errorf("node: place lease %s on %s: %w", lease.ID, r.node, err)
	}

	if err := r.alloc.Advance(ctx, lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
		return fmt.Errorf("node: mark lease %s launching: %w", lease.ID, err)
	}

	setID, err := r.scaleSetID(ctx, tier)
	if err != nil {
		return err
	}

	// NAMED AFTER THE LEASE, which is unique and already exists.
	//
	// Reusing a name across launches is what made docker refuse to start a
	// container after a crash left one behind, and GitHub has the same problem
	// from the other side — GenerateJitRunnerConfig fails with RunnerExistsError
	// on a name that is already registered. A lease id is unique by construction,
	// so neither collision is reachable.
	//
	// It also carries the only durable link between an instance and its lease.
	// Nothing writes "this container belongs to that lease" anywhere, so after a
	// crash the name is what reconciliation reads to decide whether a surviving
	// instance is still wanted. See provider.InstanceName.
	name := provider.InstanceName(lease.ID)

	reg, err := r.jit.JITConfig(ctx, setID, name, "_work")
	if err != nil {
		// The cached scale-set id is dropped, not reused. If the set was deleted
		// and recreated — a teardown plus another control plane — every later
		// launch would keep targeting the id that no longer exists. Clearing it
		// makes the NEXT job re-resolve; this one is not retried, because JIT
		// creation has an ambiguous-success case and a blind retry is how one job
		// becomes two runners.
		r.forgetScaleSet(tier.Label)

		return fmt.Errorf("node: mint a registration for %s: %w", name, err)
	}

	inst, err := r.provider.Launch(ctx, provider.Spec{
		// BILLET's name, not GitHub's. reg.RunnerName() is what GitHub called the
		// runner, and using it here would have left the instance carrying a name
		// with no lease in it — unattributable to reconciliation, which is the one
		// reader that has nothing else to go on. The JIT config carries GitHub's
		// identity into the guest; the instance carries billet's.
		Name:  name,
		Image: tier.Image,
		VCPU:  tier.VCPU,

		Memory: tier.Memory,
		Disk:   tier.Disk,
		SHM:    tier.SHM,

		// Classified from the event that queued the job. The zero value is
		// unknown and backends refuse it, so a job whose event billet does not
		// recognise cannot run anywhere weak by default.
		Trust: trust,

		JITConfig: reg.Config(),
	})
	if err != nil {
		// A LAUNCH ERROR IS NOT PROOF NOTHING STARTED.
		//
		// A cancelled context can kill the CLI after the daemon accepted the create;
		// a remote API can commit and lose the response. The error says the caller
		// does not KNOW, which is not the same as knowing there is nothing there —
		// and the difference is a container running a job nobody will ever collect,
		// holding a runner registration nobody will ever delete.
		//
		// So ask. Whatever is found is destroyed rather than adopted: the lease is
		// about to be failed, so an instance for it has no future, and an adopted
		// half-started instance is a worse thing to own than a clean failure.
		r.destroyStray(ctx, name)

		return fmt.Errorf("node: start %s: %w", name, err)
	}

	r.mu.Lock()
	r.running[job.RequestID] = inst
	r.mu.Unlock()

	r.log.Info("started a runner",
		"tier", lease.Tier, "request", job.RequestID, "runner", inst.Name,
		"instance", inst.ID, "trust", trust)

	return nil
}

// Destroy removes whatever Launch started for a request.
//
// Idempotent: a request nothing was started for is success, because this runs on
// redelivered completions, on shutdown, and after a failure.
func (r *Runner) Destroy(ctx context.Context, requestID int64) error {
	r.mu.Lock()
	inst, ok := r.running[requestID]
	r.mu.Unlock()

	if !ok {
		return nil
	}

	if err := r.provider.Destroy(ctx, inst.ID); err != nil {
		// KEPT in the map. The instance may still be running, and forgetting it
		// here is how it becomes an orphan nobody can find by request id.
		return fmt.Errorf("node: stop %s for request %d: %w", inst.Name, requestID, err)
	}

	r.mu.Lock()
	delete(r.running, requestID)
	r.mu.Unlock()

	return nil
}

// destroyStray removes an instance a failed launch may have left behind.
//
// Best-effort by construction, and the errors are logged rather than returned:
// the caller is already failing, and replacing its error with this one would
// hide why the launch failed in the first place. What must not happen is
// silence — an operator who has to find an orphan by hand needs to know it
// might exist.
func (r *Runner) destroyStray(ctx context.Context, name string) {
	// A fresh context, because the usual reason a launch failed is that the
	// caller's was cancelled — and asking a cancelled context to clean up
	// guarantees the cleanup fails too.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), strayCleanupTimeout)
	defer cancel()

	inst, found, err := r.provider.Find(ctx, name)
	if err != nil {
		r.log.Error("could not tell whether a failed launch left an instance behind",
			"name", name, "error", err)

		return
	}

	if !found {
		return
	}

	r.log.Warn("a failed launch had in fact started something; removing it",
		"name", name, "instance", inst.ID)

	if err := r.provider.Destroy(ctx, inst.ID); err != nil {
		r.log.Error("could not remove the instance a failed launch left behind",
			"name", name, "instance", inst.ID, "error", err)
	}
}

// Recover destroys everything this node is running, because nothing here is
// ours yet. Called once at startup, before any listener opens a session.
//
// The predicate is NOT "does a lease still want this". An open lease says the
// job was wanted; it does not say the process that has just started can manage
// the container. It cannot: the maps that map a request id to an instance, and a
// lease to its heartbeat, are in memory, and this process has empty ones. A
// container spared here would run with nothing to heartbeat its lease, nothing
// to notice its completion, and nothing to destroy it afterwards — the lease
// would expire, the reaper would hand its capacity back, and the container would
// keep running forever on capacity billet had already re-sold.
//
// So a restart is destructive, and the honest description of the cost is that a
// job which might have finished is killed and GitHub requeues it. That is what
// already happens on a graceful shutdown, so it is not a new behaviour — only a
// crash now behaves like a stop instead of leaking.
//
// The alternative is adoption: reconstructing request-id ownership, heartbeat
// responsibility, session semantics and completion teardown for compute this
// process never started. That is a real feature, not a branch in this function,
// and it is worth building only once the node runs in its own process — at which
// point a server restart MUST not destroy a live node's work.
func (r *Runner) Recover(ctx context.Context) error {
	instances, err := r.provider.List(ctx)
	if err != nil {
		return fmt.Errorf("node: list what is already running: %w", err)
	}

	var failed int

	for _, inst := range instances {
		leaseID, ours := provider.LeaseOf(inst.Name)

		// NOT OURS, NOT TOUCHED. The provider filters by this deployment's own
		// label, so this should be unreachable — but the action here is
		// destruction, and "should be unreachable" is a poor argument for a loop
		// that destroys. Adopting a name billet did not choose is the one mistake
		// with no undo.
		if !ours {
			r.log.Warn("ignoring an instance whose name billet did not assign",
				"name", inst.Name, "instance", inst.ID)

			continue
		}

		r.log.Warn("destroying an instance left by an earlier run; this process cannot manage it",
			"name", inst.Name, "instance", inst.ID, "lease", leaseID)

		if err := r.provider.Destroy(ctx, inst.ID); err != nil {
			failed++

			r.log.Error("could not destroy an instance left by an earlier run",
				"name", inst.Name, "instance", inst.ID, "error", err)

			continue
		}

		// The lease goes with it. Waiting for the reaper would hold its vCPU and
		// memory for a full lease TTL after the compute they were paying for is
		// already gone, which is a self-inflicted capacity shortfall on every
		// restart.
		if err := r.releaseOrphanedLease(ctx, leaseID); err != nil {
			r.log.Warn("destroyed an instance but could not release its lease",
				"lease", leaseID, "error", err)
		}
	}

	if failed > 0 {
		// REPORTED, not swallowed. An instance that resists destruction is holding
		// capacity the ledger believes is free, and a caller that starts admitting
		// work anyway over-commits the host by exactly that much.
		return fmt.Errorf("node: %d instance(s) left by an earlier run could not be destroyed", failed)
	}

	if len(instances) > 0 {
		r.log.Info("removed instances left behind by an earlier run", "count", len(instances))
	}

	return nil
}

// Sweep destroys instances whose lease is no longer open on this node.
//
// The steady-state counterpart to Recover, and the reason a failed cleanup is
// survivable rather than permanent. Three things leak compute while the process
// is alive and none of them is reachable by a startup-only pass: a stray that
// Find could not confirm, a Destroy the daemon refused, and a lease reaped out
// from under a container that is still running.
//
// Safe to run concurrently with live launches, because of an ordering the launch
// path guarantees: Bind and Advance(launching) both commit BEFORE the provider
// is asked to create anything. So an instance that appears in the list already
// has a lease at launching or beyond, and a sweep cannot see compute whose lease
// has not yet been written. (I had this backwards when Recover was written, and
// argued a sweep would race a starting job. It cannot — the list is taken first,
// so anything in it predates the query that judges it.)
func (r *Runner) Sweep(ctx context.Context) error {
	instances, err := r.provider.List(ctx)
	if err != nil {
		return fmt.Errorf("node: list running instances: %w", err)
	}

	if len(instances) == 0 {
		return nil
	}

	open, err := r.alloc.LaunchedLeaseIDs(ctx, r.node)
	if err != nil {
		return fmt.Errorf("node: list leases still open on %s: %w", r.node, err)
	}

	var orphans, failed int

	for _, inst := range instances {
		leaseID, ours := provider.LeaseOf(inst.Name)
		if !ours {
			r.log.Warn("ignoring an instance whose name billet did not assign",
				"name", inst.Name, "instance", inst.ID)

			continue
		}

		if open[leaseID] {
			continue
		}

		orphans++

		r.log.Warn("destroying an instance whose lease is gone",
			"name", inst.Name, "instance", inst.ID, "lease", leaseID)

		if err := r.provider.Destroy(ctx, inst.ID); err != nil {
			failed++

			r.log.Error("could not destroy an orphaned instance",
				"name", inst.Name, "instance", inst.ID, "error", err)
		}
	}

	if failed > 0 {
		return fmt.Errorf("node: %d of %d orphaned instances could not be destroyed", failed, orphans)
	}

	if orphans > 0 {
		r.log.Info("removed orphaned instances", "count", orphans)
	}

	return nil
}

// releaseOrphanedLease terminalizes a lease whose compute has just been
// destroyed, tolerating one that is already gone.
func (r *Runner) releaseOrphanedLease(ctx context.Context, leaseID string) error {
	lease, err := r.alloc.Lease(ctx, leaseID)
	if err != nil {
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			// Already terminal, or never existed. Either way there is nothing
			// holding capacity, which is the outcome this was after.
			return nil
		}

		return err
	}

	return r.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseFailed)
}

// forgetScaleSet drops a cached id so the next launch re-resolves it.
func (r *Runner) forgetScaleSet(tier string) {
	r.mu.Lock()
	delete(r.sets, tier)
	r.mu.Unlock()
}

// scaleSetID resolves a tier's scale set, once.
func (r *Runner) scaleSetID(ctx context.Context, tier config.Tier) (int, error) {
	r.mu.Lock()
	id, cached := r.sets[tier.Label]
	r.mu.Unlock()

	if cached {
		return id, nil
	}

	set, _, err := r.jit.Describe(ctx, tier.Label, tier.RunnerGroup)
	if err != nil {
		return 0, fmt.Errorf("node: find the scale set for tier %s: %w", tier.Label, err)
	}

	if set == nil {
		// Reconciliation creates these before any listener starts, so an absent
		// one means somebody removed it underneath a running control plane.
		return 0, fmt.Errorf("node: tier %s has no scale set on github; it was created at "+
			"startup, so something removed it since", tier.Label)
	}

	r.mu.Lock()
	r.sets[tier.Label] = set.ID
	r.mu.Unlock()

	return set.ID, nil
}

// Job re-exports the listener's job identity so this package's signature matches
// server.Runner without importing anything else from it at call sites.
type Job = server.Job
