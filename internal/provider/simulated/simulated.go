// Package simulated is a compute backend that starts no compute.
//
// It exists so a workload can be run through the real listener, allocator and
// placer at a scale no real backend can afford: a scenario with a thousand
// concurrent leases costs a thousand guests on every other backend, and nothing
// about a placement decision needs a guest to exist. An instance here is a record
// that reports itself running until a modelled duration has elapsed and stopped
// afterwards, and the duration comes from the spec rather than from anything
// this package decides, so the provider stays a mechanism and the workload stays
// the harness's business.
//
// NONE OF THE CONTRACT IS WEAKENED FOR BEING A STAND-IN. List errors rather than
// answering short, because reconciliation frees the capacity of every lease
// absent from an inventory and a harness whose inventory may lie proves nothing
// about a scheduler that trusts it. Destroy reports TeardownStopped only for an
// instance this store held and removed. Untrusted work is refused, since there
// is no boundary here at all. Instances carry the deployment identity and
// destruction is scoped by it.
//
// IT IS REFUSED IN A CONFIGURATION AND HAS NO PRODUCTION CALLER. config.Load
// rejects the kind wherever a file could name it and cmd/billet never
// constructs it; a test in this package proves nothing outside a test imports
// it. A backend that fabricates completions reachable from a real billet.yaml
// would be a fleet that reports every job finished and runs none.
package simulated

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// runnerWord is the closed vocabulary a spec's command must use. The command is
// what starts the runner inside an instance on every other backend, and here
// the runner is a modelled occupation of the compute, so the command says how
// long it lasts and nothing else.
const (
	runnerWord = "billet-simulated-runner"
	runForFlag = "--run-for"
)

// RunFor renders the command a tier gives this backend: a runner that occupies
// its instance for d. It is the only shape Launch accepts.
func RunFor(d time.Duration) []string {
	return []string{runnerWord, runForFlag, d.String()}
}

// durationOf reads the modelled duration back out of a command RunFor built.
//
// EXACT, not lenient. A command from a tier written for another backend, or a
// hand-typed one, is refused rather than read as some duration, for the reason
// docker refuses an empty command: the alternative is a success that models the
// wrong thing while every signal says the launch worked.
func durationOf(cmd []string) (time.Duration, error) {
	if len(cmd) != 3 || cmd[0] != runnerWord || cmd[1] != runForFlag {
		return 0, fmt.Errorf("simulated: the command must be exactly what RunFor renders "+
			"(%s %s <duration>), got %q", runnerWord, runForFlag, cmd)
	}

	d, err := time.ParseDuration(cmd[2])
	if err != nil {
		return 0, fmt.Errorf("simulated: the command's duration %q is not one: %w", cmd[2], err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("simulated: a modelled runner must occupy its instance for a "+
			"positive duration, got %s", d)
	}

	// THE CANONICAL SPELLING, not merely a parseable one. A closed vocabulary that
	// admits `60s` beside `1m0s` is two spellings of one command, and the second
	// spelling is the one nothing documents.
	if cmd[2] != d.String() {
		return 0, fmt.Errorf("simulated: the duration must be spelled as RunFor renders it "+
			"(%s), got %q", d, cmd[2])
	}

	return d, nil
}

// Host is the store instances live in: the machine, as far as this backend is
// concerned.
//
// SHARED ON PURPOSE. Two deployments on one machine share a docker daemon, a
// tart store or a jailer root, and every backend has to keep them from seeing
// or destroying each other's compute. Two providers over one Host model that,
// and a provider built without one gets a Host of its own.
type Host struct {
	mu        sync.Mutex
	next      int
	instances map[string]*record
}

// NewHost builds an empty host.
func NewHost() *Host {
	return &Host{instances: make(map[string]*record)}
}

// record is one instance. It never holds the registration the spec carried.
type record struct {
	id, name, owner string
	startedAt       time.Time
	duration        time.Duration
	vcpu            int
	memory          config.ByteSize
}

// Provider is the simulated backend for one deployment.
type Provider struct {
	log   *slog.Logger
	owner string
	host  *Host
	now   func() time.Time

	mu sync.Mutex
	// inventoryFault, when set, is what List and Find answer instead of the
	// inventory. It is the harness's seam for a host whose inventory cannot be
	// read, which every real backend can suffer and an in-memory one otherwise
	// never would.
	inventoryFault error
}

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// WithClock replaces the clock instances are timed against. The default is
// time.Now; a harness replaces it so a modelled hour takes no wall time.
func WithClock(now func() time.Time) Option {
	return func(p *Provider) { p.now = now }
}

// OnHost places this provider's instances on a host it may share.
func OnHost(h *Host) Option {
	return func(p *Provider) { p.host = h }
}

// New builds a simulated provider. owner names the deployment and is written
// onto every instance it starts.
func New(owner string, opts ...Option) (*Provider, error) {
	// Refused for ec2's reason: List filters on it and feeds a loop that
	// destroys, so two deployments sharing an empty identity would destroy each
	// other's instances.
	if owner == "" {
		return nil, errors.New("simulated: a provider needs a deployment identity to own its instances")
	}

	p := &Provider{log: slog.Default(), owner: owner, now: time.Now}

	for _, opt := range opts {
		opt(p)
	}

	if p.host == nil {
		p.host = NewHost()
	}

	return p, nil
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderSimulated }

// Accepts refuses anything that is not established as trusted.
//
// There is no boundary here at all: nothing runs, so nothing isolates. Unknown
// is refused alongside untrusted, because a caller that has not classified a
// job has not established it is safe anywhere.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	if trust == provider.TrustTrusted {
		return nil
	}

	return fmt.Errorf("simulated: refusing to model %s work: this backend starts no compute "+
		"and has no boundary at all, so it may stand in only for work billet already vouches for",
		trust)
}

// Launch records one instance that will report itself running for the duration
// its command names.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("simulated: launch %s: %w", spec.Name, err)
	}

	if spec.Name == "" {
		return nil, errors.New("simulated: a spec needs a name")
	}

	if spec.JITConfig == "" {
		return nil, fmt.Errorf("simulated: %s has no JIT config, so nothing would register", spec.Name)
	}

	// Checked again here, not only via Accepts: a backend that only refuses when
	// asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	duration, err := durationOf(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	p.host.mu.Lock()
	defer p.host.mu.Unlock()

	// A NAME IN USE IS REFUSED, whoever holds it. Docker refuses to start a
	// container under a name a crash left behind, and reconciliation reads the
	// name to find the lease, so two instances under one name would be two
	// claims on one lease.
	if existing, ok := p.host.instances[spec.Name]; ok {
		return nil, fmt.Errorf("simulated: an instance named %s already exists on this host "+
			"(id %s)", spec.Name, existing.id)
	}

	p.host.next++

	r := &record{
		id:        "sim-" + strconv.Itoa(p.host.next),
		name:      spec.Name,
		owner:     p.owner,
		startedAt: p.now(),
		duration:  duration,
		vcpu:      spec.VCPU,
		memory:    spec.Memory,
	}
	p.host.instances[spec.Name] = r

	p.log.Info("launched a simulated instance",
		"runner", spec.Name, "instance", r.id, "runs_for", duration)

	return &provider.Instance{ID: r.id, Name: r.name, Running: true}, nil
}

// Find reports this deployment's instance with that name, and whether there was
// one. The name is compared exactly.
func (p *Provider) Find(ctx context.Context, name string) (*provider.Instance, bool, error) {
	if err := p.inventoryError(ctx); err != nil {
		return nil, false, fmt.Errorf("simulated: find %s: %w", name, err)
	}

	p.host.mu.Lock()
	defer p.host.mu.Unlock()

	r, ok := p.host.instances[name]
	if !ok || r.owner != p.owner {
		return nil, false, nil
	}

	return p.render(r), true, nil
}

// List reports every instance this deployment has on the host, running or not.
//
// AN ERROR, NEVER A SHORT ANSWER. An inventory that cannot be read is reported
// as such, because the caller frees the capacity of every lease absent from
// what it is given.
func (p *Provider) List(ctx context.Context) ([]*provider.Instance, error) {
	if err := p.inventoryError(ctx); err != nil {
		return nil, fmt.Errorf("simulated: list instances: %w", err)
	}

	p.host.mu.Lock()
	defer p.host.mu.Unlock()

	var instances []*provider.Instance

	for _, r := range p.host.instances {
		if r.owner == p.owner {
			instances = append(instances, p.render(r))
		}
	}

	return instances, nil
}

// Destroy removes an instance this deployment owns.
//
// Idempotent: an id the host does not hold is success, and CONFIRMING, because
// this store is authoritative about its own contents the way the docker daemon
// is. An instance another deployment owns is refused rather than removed: the
// id came from somewhere, and destruction is scoped by identity.
func (p *Provider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	if id == "" {
		return provider.TeardownRequested, errors.New("simulated: destroy needs an instance id")
	}

	if err := ctx.Err(); err != nil {
		return provider.TeardownRequested, fmt.Errorf("simulated: destroy %s: %w", id, err)
	}

	p.host.mu.Lock()
	defer p.host.mu.Unlock()

	for name, r := range p.host.instances {
		if r.id != id {
			continue
		}

		if r.owner != p.owner {
			return provider.TeardownRequested, fmt.Errorf(
				"simulated: instance %s belongs to another deployment; refusing to destroy it", id)
		}

		delete(p.host.instances, name)

		p.log.Info("destroyed a simulated instance", "runner", name, "instance", id)

		return provider.TeardownStopped, nil
	}

	return provider.TeardownStopped, nil
}

// FailInventory makes List and Find answer err instead of the inventory until
// it is called again with nil.
//
// THE HARNESS'S SEAM, and the only way an in-memory inventory can fail. A real
// backend loses its daemon or its API; what billet does then is the question,
// and a stand-in that cannot pose it proves nothing about the answer.
func (p *Provider) FailInventory(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.inventoryFault = err
}

// inventoryError is what stops an inventory being read: a context that has
// ended, or a fault the harness injected. Nothing is read once it answers.
func (p *Provider) inventoryError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.inventoryFault
}

// render reports an instance's state on this provider's clock.
//
// Running until the modelled duration has elapsed and TERMINAL afterwards: the
// store is authoritative, so a stopped instance is positively known never to
// run again, which is the fact Terminal exists to carry. It stays in the
// inventory until Destroy removes it, as an exited container does.
func (p *Provider) render(r *record) *provider.Instance {
	running := p.now().Before(r.startedAt.Add(r.duration))

	return &provider.Instance{
		ID:       r.id,
		Name:     r.name,
		Running:  running,
		Terminal: !running,
	}
}
