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
	"fmt"
	"log/slog"
	"sync"

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
	log      *slog.Logger

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

// New builds a runner over a provider.
func New(jit JITSource, p provider.Provider, tiers []config.Tier, log *slog.Logger) *Runner {
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
	name := "billet-" + lease.ID

	reg, err := r.jit.JITConfig(ctx, setID, name, "_work")
	if err != nil {
		return fmt.Errorf("node: mint a registration for %s: %w", name, err)
	}

	inst, err := r.provider.Launch(ctx, provider.Spec{
		Name:  reg.RunnerName(),
		Image: tier.Image,
		VCPU:  tier.VCPU,

		Memory: tier.Memory,
		Disk:   tier.Disk,
		SHM:    tier.SHM,

		// Classified from the event that queued the job. The zero value is
		// unknown and backends refuse it, so a job whose event billet does not
		// recognise cannot run anywhere weak by default.
		Trust: provider.Classify(job.Event),

		JITConfig: reg.Config(),
	})
	if err != nil {
		return fmt.Errorf("node: start %s: %w", name, err)
	}

	r.mu.Lock()
	r.running[job.RequestID] = inst
	r.mu.Unlock()

	r.log.Info("started a runner",
		"tier", lease.Tier, "request", job.RequestID, "runner", inst.Name,
		"instance", inst.ID, "trust", provider.Classify(job.Event))

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
