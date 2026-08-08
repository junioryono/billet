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

// LeaseStore is the part of the capacity ledger the runner uses.
//
// An interface rather than *alloc.Allocator because the runner's hardest paths
// are the ones where the ledger REFUSES, and a concrete allocator gives a test
// no way to produce that. Custody exists to keep a lease held when compute is
// unaccounted for; the branch that keeps holding it when the release itself
// fails was untestable and therefore untested, which in this codebase has
// reliably meant wrong.
//
// It is deliberately the whole set the runner calls and nothing more, so the
// dependency is legible at a glance.
type LeaseStore interface {
	Bind(ctx context.Context, leaseID string, epoch int64, node string) error
	Advance(ctx context.Context, leaseID string, epoch int64, to alloc.Phase) error
	Heartbeat(ctx context.Context, leaseID string, epoch int64) error
	Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error
	Lease(ctx context.Context, leaseID string) (*alloc.Lease, error)
	LaunchedLeaseIDs(ctx context.Context, node string) (map[string]bool, error)
}

// Runner starts and stops the compute for assigned leases.
type Runner struct {
	jit      JITSource
	provider provider.Provider
	alloc    LeaseStore
	log      *slog.Logger

	// tending serializes Tend, which mutates custody entries in place and issues
	// backend calls between reads. Separate from mu, which guards the map itself
	// for the brief moments it is read or written.
	tending sync.Mutex

	// custody holds leases whose compute is unaccounted for, keyed by lease id.
	// See custody.go — it is what stops a launch failure or a restart from
	// handing back capacity that something may still be using.
	custody map[string]*custody

	// now is time.Now, replaceable so a test can age a custody entry without
	// sleeping.
	now func() time.Time

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
	a LeaseStore, node string, jit JITSource, p provider.Provider,
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
		custody:  make(map[string]*custody),
		now:      time.Now,
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
		// If the stray could not be confirmed gone, the LEASE STAYS HELD. Returning
		// an ordinary error here made the listener release the capacity while a
		// container might still be running on it, which is the over-commitment this
		// whole subsystem exists to prevent — arrived at by treating "the launch
		// failed" as "nothing is using the host".
		// CUSTODY UNLESS THE CLEANUP WAS CAUSAL. A successful Destroy proves the
		// compute is gone; anything else — an error, or simply not finding it —
		// does not, because the daemon may still be acting on a create whose
		// response was lost.
		if confirmed, cleanupErr := r.destroyStray(ctx, name); !confirmed {
			r.hold(lease, name)

			return errCustody(name, errors.Join(err, cleanupErr))
		}

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
		// Not something this incarnation started — but it may be something an
		// earlier one did, which this one adopted. Without this an adopted
		// container whose job GitHub later reports finished is held forever: the
		// restarted listener has no record of the request either, so nothing else
		// connects the completion to the compute.
		// Idempotent either way: a request nothing was started for is success,
		// because this runs on redelivered completions, on shutdown, and after a
		// failure. The return value says whether custody handled it, which nothing
		// above needs — the error is what matters.
		if _, err := r.releaseRequest(ctx, requestID); err != nil {
			return err
		}

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
func (r *Runner) destroyStray(ctx context.Context, name string) (bool, error) {
	// A fresh context, because the usual reason a launch failed is that the
	// caller's was cancelled — and asking a cancelled context to clean up
	// guarantees the cleanup fails too.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), strayCleanupTimeout)
	defer cancel()

	inst, found, err := r.provider.Find(ctx, name)
	if err != nil {
		return false, fmt.Errorf("could not tell whether a failed launch left %s behind: %w", name, err)
	}

	if !found {
		// NOT CONFIRMED. Absence in one observation is not proof that nothing will
		// appear: the create may be in flight inside the daemon. The caller keeps
		// the capacity and looks again.
		return false, fmt.Errorf("no instance named %s is visible yet, which does not "+
			"prove the daemon is not still starting one", name)
	}

	r.log.Warn("a failed launch had in fact started something; removing it",
		"name", name, "instance", inst.ID)

	if err := r.provider.Destroy(ctx, inst.ID); err != nil {
		return false, fmt.Errorf("could not remove %s, which a failed launch had started: %w", name, err)
	}

	return true, nil
}

// Recover decides what to do with compute an earlier run left behind, once, at
// startup and before any listener opens a session.
//
// IT DOES NOT DESTROY EVERYTHING, and the previous version's argument for doing
// so was wrong on a point of fact. I claimed a killed job would simply be
// requeued by GitHub. The scale-set documentation says reassignment happens when
// a job is assigned to a scale set "but not acquired by a runner in time" — it
// says nothing about a job a runner has already started, and there is no
// evidence GitHub transparently retries that. Force-killing a container running
// a twenty-minute job is therefore a deliberate job failure, not a recovery, and
// the fact that a graceful shutdown also kills jobs does not make doing it after
// an unrelated controller crash acceptable.
//
// So a surviving container whose lease is still open is ADOPTED. Billet cannot
// manage it — the request-id mapping and completion handling died with the last
// process — but the runner inside is talking to GitHub on its own and may well
// finish. What billet can do is keep the lease alive so the capacity is not
// resold underneath it, and clean up once it stops. That is what custody is.
//
// A container whose lease is NOT open is a genuine orphan: nothing is waiting
// for its result and its capacity has already gone back to the budget. Those are
// destroyed here rather than left for the first sweep, because until they are
// gone the host is over-committed by exactly their size.
func (r *Runner) Recover(ctx context.Context) error {
	instances, err := r.provider.List(ctx)
	if err != nil {
		return fmt.Errorf("node: list what is already running: %w", err)
	}

	if len(instances) == 0 {
		return nil
	}

	open, err := r.alloc.LaunchedLeaseIDs(ctx, r.node)
	if err != nil {
		return fmt.Errorf("node: list leases still open on %s: %w", r.node, err)
	}

	var adopted, failed int

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

		if open[leaseID] && inst.Running {
			lease, err := r.alloc.Lease(ctx, leaseID)

			switch {
			case errors.Is(err, alloc.ErrLeaseNotFound):
				// The lease went terminal between the two reads. It really is an
				// orphan, and the destroy below is the right answer.
				r.log.Warn("a surviving instance's lease was terminalized while adopting it",
					"name", inst.Name, "lease", leaseID)

			case err != nil:
				// ANY OTHER ERROR MEANS "COULD NOT VERIFY", NOT "SAFE TO DESTROY".
				// Falling through here turned a transient database read failure into
				// a force-killed job, which is the single worst thing this function
				// can do. Recovery aborts instead; the caller treats that as fatal
				// and nothing has been destroyed.
				return fmt.Errorf("node: read the lease of surviving instance %s: %w", inst.Name, err)

			default:
				if err := r.takeCustody(ctx, lease, inst); err != nil {
					return err
				}

				adopted++

				r.log.Info("adopted a running job from an earlier run; it will be left to "+
					"finish and its capacity stays held",
					"name", inst.Name, "instance", inst.ID, "lease", leaseID)

				continue
			}
		}

		r.log.Warn("destroying an instance nothing is waiting for",
			"name", inst.Name, "instance", inst.ID, "lease", leaseID, "running", inst.Running)

		if err := r.provider.Destroy(ctx, inst.ID); err != nil {
			failed++

			r.log.Error("could not destroy an instance left by an earlier run",
				"name", inst.Name, "instance", inst.ID, "error", err)

			continue
		}

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

	r.log.Info("reconciled compute left by an earlier run",
		"found", len(instances), "adopted", adopted)

	return nil
}

// takeCustody adopts an instance and immediately renews its lease.
//
// THE RENEWAL IS THE POINT, and leaving it to the first periodic tick was a real
// hole. Billet may have been down for longer than a lease TTL, so the lease it
// is adopting can already be expired — and the control plane runs a reap BEFORE
// its first tend. The reaper would terminalize the lease it had just adopted,
// hand the capacity back, and let a listener advertise it while the container
// carried on running.
func (r *Runner) takeCustody(ctx context.Context, lease *alloc.Lease, inst *provider.Instance) error {
	if err := r.alloc.Heartbeat(ctx, lease.ID, lease.Epoch); err != nil {
		return fmt.Errorf("node: renew the lease of adopted instance %s: %w", inst.Name, err)
	}

	r.adopt(lease, inst)

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

	// Anything in custody is Tend's business, not the sweep's. Both would
	// otherwise act on the same instance in the same tick — the sweep destroying
	// an adopted container the moment its lease went terminal, before Tend had
	// the chance to release the capacity in the right order.
	held := r.heldLeases()

	var orphans, failed int

	for _, inst := range instances {
		leaseID, ours := provider.LeaseOf(inst.Name)
		if !ours {
			r.log.Warn("ignoring an instance whose name billet did not assign",
				"name", inst.Name, "instance", inst.ID)

			continue
		}

		if open[leaseID] || held[leaseID] {
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
