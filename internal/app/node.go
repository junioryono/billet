package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/provider/docker"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/wirecert"
)

// NodeOptions are how the node was invoked.
type NodeOptions struct {
	// Upgrader is how this process replaces itself when a rollout reaches it: a
	// property of how the process was INSTALLED, which only the command knows.
	Upgrader node.ExecUpgrader
}

// Node is a compute host that holds this deployment's identity and has built
// its provider, and has not yet dialed its control plane.
type Node struct {
	cfg        *config.Config
	deployment string
	lock       *state.DeploymentLock
	identity   *wirecert.Rotating
	client     *nodeclient.Client
	provider   provider.Provider
	upgrader   node.ExecUpgrader
}

// OpenNode loads the certificate this node presents, claims the deployment
// identity it names and the host-wide lock on it, and builds the client and
// the provider. Close the Node to release the lock.
//
// THE ORDER IS THE SAFETY. The certificate decides the identity, so it is read
// first: a node JOINS a deployment, it does not found one. And the provider is
// built only after the lock is held, because nothing may touch a container
// before the right to manage containers under this identity is established.
func OpenNode(cfg *config.Config, opts NodeOptions) (*Node, error) {
	bundle, err := NodeBundle(cfg)
	if err != nil {
		return nil, err
	}

	// THE NODE'S LOCK MUST LAND WHERE THE SERVER'S DOES. Both roles can run on
	// one host, and a server honouring server.lock_dir while this used the
	// per-user default would take two different locks for one identity — after
	// which both manage the same containers and either can adopt or destroy the
	// other's live work. Sharing the claim is what keeps that true.
	deployment, lock, err := ClaimNodeDeployment(cfg, bundle)
	if err != nil {
		return nil, err
	}

	n := &Node{cfg: cfg, deployment: deployment, lock: lock, upgrader: opts.Upgrader}

	if err := n.open(bundle); err != nil {
		if rerr := lock.Release(); rerr != nil {
			slog.Default().Warn("could not release the deployment lock", "error", rerr)
		}

		return nil, err
	}

	return n, nil
}

// open builds what the node needs once it holds the identity.
func (n *Node) open(bundle *wirecert.Bundle) error {
	cfg := n.cfg

	// A ROTATING IDENTITY, so a renewal takes effect without a restart. The
	// callback is answered per handshake, which is what makes it safe to replace
	// the certificate under a node holding long-lived connections.
	var tlsConf *tls.Config

	if bundle != nil {
		identity, err := wirecert.NewRotating(cfg.Node.TLS.CertPath, cfg.Node.TLS.KeyPath, cfg.Node.TLS.CAPath)
		if err != nil {
			return err
		}

		// SAID OUT LOUD, because nothing is broken and something did go wrong. A
		// renewal was interrupted partway through installing itself and this node
		// came back on the generation it was replacing — which still works, and
		// which is closer to expiry than the one that did not land.
		if stale := identity.StaleCopies(); stale != nil {
			slog.Default().Warn("a superseded certificate generation could not be removed, so a "+
				"second copy of this node's private key is still on disk; delete it",
				"error", stale)
		}

		if identity.RolledBack() {
			slog.Default().Warn("a certificate renewal was interrupted before it finished "+
				"installing; this node is running on the one it replaced and will try again",
				"expires", identity.Leaf().NotAfter.Format(time.DateOnly))
		}

		host, err := serverHostname(cfg.Node.ServerAddr)
		if err != nil {
			return err
		}

		n.identity = identity
		tlsConf = identity.ClientTLS(host)
	}

	client, err := NewNodeClient(cfg, tlsConf)
	if err != nil {
		return err
	}

	p, err := NewProvider(cfg, n.deployment)
	if err != nil {
		return err
	}

	n.client, n.provider = client, p

	return nil
}

// Name is the name this node registers under, which the certificate decides
// when it has one.
func (n *Node) Name() string { return n.cfg.Node.Name }

// Deployment is the deployment identity this node claimed.
func (n *Node) Deployment() string { return n.deployment }

// Close releases the host-wide deployment lock.
func (n *Node) Close() error { return n.lock.Release() }

// NodeHost is what the node takes from this process and machine.
type NodeHost struct {
	// Ready tells the service manager this process is serving.
	Ready func() error
	// DrainRequested reports whether the stop under way must drain although
	// node.stop says handoff.
	DrainRequested func() bool
	// Hurry is closed by the second signal, reaching the wait that honours it.
	Hurry <-chan struct{}
	// Out is where the lines an operator reads go.
	Out io.Writer
	// RegistrationRecordPath is where the node publishes its registration
	// record after every accepted registration, empty where none is kept.
	RegistrationRecordPath string
}

// Run starts the guest cache, the job sampler and the runner, tells the
// service manager this process is serving, and runs the node loop against the
// control plane until ctx ends and the node has drained or handed over.
func (n *Node) Run(ctx context.Context, host NodeHost) error {
	cfg, p := n.cfg, n.provider

	handOver, err := cfg.Node.HandsOverOnStop()
	if err != nil {
		return err
	}

	cacheService, serveCache, stopCache, err := startNodeCache(ctx, cfg, p, n.deployment, n.client,
		nodeCacheStopGrace(handOver))
	if err != nil {
		return err
	}
	defer stopCache()

	maxCustody, err := cfg.Node.MaxCustodyDuration()
	if err != nil {
		return err
	}

	// THE CLIENT IS BOTH THE LEDGER AND THE MINT.
	//
	// It satisfies node.LeaseStore and node.JITSource, which is the whole reason
	// the runner needs no idea it is remote: the interfaces it already took are
	// the seam the network went through.
	runnerOpts := []node.Option{node.WithMaxCustody(maxCustody)}
	if cacheService != nil {
		runnerOpts = append(runnerOpts, node.WithCacheService(cacheService))
	}
	if cfg.Node.RegistryMirrors != nil {
		runnerOpts = append(runnerOpts, node.WithRegistryMirrors(*cfg.Node.RegistryMirrors))
	}

	// AND THE ABILITY TO REPLACE ITSELF, which is what makes a rollout reach a
	// node at all. It is given here rather than defaulted inside the runner
	// because it is a property of how this process was INSTALLED — a node started
	// out of a working directory has no packaged binary to replace, and the
	// runner refusing the command with a sentence an operator can act on is better
	// than one attempting a transaction against a machine that is not shaped for
	// it.
	runnerOpts = append(runnerOpts, node.WithUpgrader(n.upgrader))

	// THE SAMPLER OUTLIVES THE SHUTDOWN SIGNAL, as the drain does: jobs keep
	// running and being measured until this command returns.
	monitorCtx, stopMonitor := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMonitor()
	monitorOpts, err := nodeMonitorOptions(monitorCtx, cfg, p)
	if err != nil {
		return err
	}
	runnerOpts = append(runnerOpts, monitorOpts...)

	runner := node.New(n.client, cfg.Node.Name, n.client, p, slog.Default(), runnerOpts...)

	fmt.Fprintf(host.Out, "billet node %s: dialing %s\n", cfg.Node.Name, cfg.Node.ServerAddr)

	// NO GUEST-OS CLAIM. A host's allowlist lives in the SERVER's fleet
	// configuration, not here, and Bind is what enforces it. Sending one from the
	// node would be a second authority for a fact the operator already stated in
	// one place — and the node's copy is the one nobody would think to update.
	// Parsed rather than trusted: Validate rejected a bad value when the file was
	// read, so this cannot normally fail — but reading it is the only thing that
	// makes node.drain_timeout take effect.
	drainTimeout, err := cfg.Node.DrainTimeoutDuration()
	if err != nil {
		return err
	}
	// RESOLVED ONCE, HERE, rather than on each re-registration. A drain
	// re-registers, and a node that came back reporting a different contribution
	// would move the fleet's arithmetic underneath work it is still holding.
	contribution, err := nodeContribution(cfg)
	if err != nil {
		return err
	}

	for _, w := range contribution.Warnings {
		slog.Default().Warn(w, "node", cfg.Node.Name)
	}
	if err := host.Ready(); err != nil {
		return fmt.Errorf("node readiness: %w", err)
	}

	return nodeclient.Run(ctx, n.client, runner, nodeclient.LoopOptions{
		Provider:       cfg.Node.Provider,
		Deployment:     n.deployment,
		Site:           cfg.Node.Site,
		VCPU:           contribution.VCPU,
		Memory:         contribution.Memory,
		GuestOS:        nodeGuestOS(cfg),
		EC2Shapes:      RemoteShapes(cfg),
		CodeBuildFleet: codeBuildFleet(cfg),

		CodeBuildJITParameterPath: codeBuildJITPath(cfg),
		CodeBuildRegion:           codeBuildRegion(cfg),
		Log:                       slog.Default(),
		Identity:                  n.identity,
		SweepEvery:                5 * time.Minute,
		DrainTimeout:              drainTimeout,
		HandOverOnStop:            handOver,
		// THE GUEST CACHE ANSWERS ONCE THIS PROCESS IS REGISTERED AND RECOVERED;
		// until then its connections wait in the listener's queue.
		Ready:          serveCache,
		DrainRequested: host.DrainRequested,
		// The second signal, reaching the wait that honours it.
		Hurry: host.Hurry,
		// OVERLAPPING LAUNCHES ONLY WHERE THE PROVIDER WAS BUILT FOR THEM: Firecracker
		// locks each lease separately and allocates by atomic link. The others keep
		// one command at a time until each is shown to be safe the same way.
		LaunchConcurrency: nodeLaunchConcurrency(cfg.Node.Provider),
		// Where the node publishes its registration record after every accepted
		// registration, for the inspector to read: the one spelling, empty on a
		// Mac.
		RegistrationRecordPath: host.RegistrationRecordPath,
	})
}

// ClaimNodeDeployment reads this host's identity and takes the host-wide lock on
// it.
//
// THE LOCK IS THE NODE'S ALONE, because the node is the role that manages
// containers. It stops two processes carrying one deployment identity from
// managing the same compute, and a control plane manages none — so the server
// takes no lock, and `server.lock_dir` is gone.
//
// That is not tidying, it is a requirement. The lock is EXCLUSIVE per identity,
// so a server that took it would keep a node on the same machine from ever
// starting — and one machine running both is the single-machine deployment,
// which is now precisely `billet server` and `billet node` side by side.
//
// MUST BE CALLED BEFORE NewProvider: nothing may touch a container before the
// right to manage containers under this identity is established.
func ClaimNodeDeployment(cfg *config.Config, bundle *wirecert.Bundle) (string, *state.DeploymentLock, error) {
	// A NODE JOINS A DEPLOYMENT, IT DOES NOT FOUND ONE, and the whole question is what
	// tells it which one. A certificate answers directly. Without one, the node can
	// only reach a control plane inside this machine — validation guarantees that,
	// because a certless node may dial nothing but loopback — and if this file also
	// describes that control plane, its state directory holds the answer.
	//
	// Falling back to the node's OWN directory is not an option, and fails invisibly:
	// a state directory with no identity MINTS a fresh random one, so the node invents
	// a deployment nobody has heard of, the plane refuses it for belonging elsewhere,
	// and that refusal is ErrRefused — which the node loop reads as a verdict rather
	// than an outage, so the process exits and nothing repairs it.
	deployment, err := NodeDeploymentID(cfg, bundle)
	if err != nil {
		return "", nil, err
	}

	if deployment != "" {
		if _, err := state.AdoptDeploymentID(cfg.Node.StateDir, deployment); err != nil {
			return "", nil, err
		}
	}

	return claimIdentity(cfg.Node.StateDir, cfg.Node.LockDir, cfg.Node.AllowUnlockedDeployment)
}

// NodeDeploymentID is the identity this host must claim, or "" when only its own
// state directory can say.
//
// The certificate outranks the config file: a bundle is proof issued BY the
// control plane, while a `server:` section is merely a description sitting next
// to the node's own. They agree in every sane deployment, and where they do not,
// the one the plane will actually check is the certificate.
func NodeDeploymentID(cfg *config.Config, bundle *wirecert.Bundle) (string, error) {
	if bundle != nil {
		return bundle.Deployment()
	}

	if cfg.Server != nil {
		// Founding it here is correct if the server has not started yet: whichever
		// role runs first mints the identity, and the other reads that same file.
		return state.DeploymentID(cfg.Server.IdentityDir)
	}

	// A node whose file says nothing about the control plane it dials. Its own
	// directory is the only answer available, so it must already hold the right
	// one — see the node.state_dir note in billet.example.yaml.
	return "", nil
}

// claimIdentity reads an installation identity and takes the host-wide lock.
func claimIdentity(
	stateDir, lockDir string, allowUnplaceable bool,
) (string, *state.DeploymentLock, error) {
	deployment, err := state.DeploymentID(stateDir)
	if err != nil {
		return "", nil, err
	}

	// The state directory's own lock guards a PATH, so a copied directory is a
	// different inode and both copies lock happily — while both carry the same
	// deployment identity and therefore manage the same containers against the
	// same daemon. This lock is keyed by the identity, so the copy collides.
	lock, err := state.LockDeployment(deployment, state.LockOptions{
		Dir:              lockDir,
		AllowUnplaceable: allowUnplaceable,
	})
	if err != nil {
		return "", nil, err
	}

	if why := lock.Degraded(); why != "" {
		// Reached only because the operator asked for it. Still said out loud every
		// boot: billet is back to the directory lock alone, which is what it had
		// before this existed and still lets two copies of a state directory run at
		// once.
		slog.Default().Warn("starting WITHOUT a host-wide deployment lock because "+
			"allow_unlocked_deployment is set, so nothing stops a COPY of this state "+
			"directory from running alongside it and managing the same containers",
			"reason", why)

		return deployment, lock, nil
	}

	// LOGGED SO A MISMATCH IS VISIBLE. The default location is per-user, so two
	// billets that ought to collide can quietly pick different directories and
	// both start. The path is the only evidence of which collision domain this
	// process actually joined.
	slog.Default().Info("holding the host-wide deployment lock",
		"identity", deployment, "path", lock.Path())

	return deployment, lock, nil
}

// nodeContribution is what this host offers: what it detected, unless its own
// config said otherwise.
//
// ONE DEFINITION, ONE CALLER, because `billet node` is the only way a host joins.
// Two paths resolving it independently would let the same file describe a
// different machine depending on which process read it.
func nodeContribution(cfg *config.Config) (config.Contribution, error) {
	// NOT MEASURED WHEN THE WORK RUNS SOMEWHERE ELSE. An ec2 node is an
	// orchestrator: it calls an API and the compute appears in a region, so this
	// machine's cores are a default for nothing — config validation requires the
	// numbers outright. Detecting anyway would spend a syscall on an answer whose
	// only use would be comparing it against a declaration it has no relationship
	// with.
	if !cfg.Node.Provider.RunsOnHost() {
		return cfg.Node.Contribution(0, 0), nil
	}

	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		return config.Contribution{}, err
	}

	return cfg.Node.Contribution(vcpu, memory), nil
}

// serverHostname is the name a node checks its control plane's certificate
// against.
//
// Taken from node.server_addr, which is the address the node actually dials, so
// the certificate is verified against the thing that was reached rather than
// against whatever the certificate happens to claim.
func serverHostname(addr string) (string, error) {
	s := addr
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("node.server_addr %q is not an address billet can dial: %w", addr, err)
	}

	if u.Hostname() == "" {
		return "", fmt.Errorf("node.server_addr %q names no host, so there is nothing to verify "+
			"the control plane's certificate against", addr)
	}

	return u.Hostname(), nil
}

// NewProvider builds the compute backend this host runs.
//
// docker, ec2 and firecracker exist. tart needs Apple Silicon and a licence
// carve-out — each is a separate implementation of the same interface, and it is
// refused explicitly rather than falling through to something that happens to
// compile.
func NewProvider(cfg *config.Config, deployment string) (provider.Provider, error) {
	switch cfg.Node.Provider {
	case config.ProviderDocker:
		// Labelled with the DEPLOYMENT id, not the node name. Two billets on one
		// machine share a hostname — and therefore a default node name — while
		// keeping separate state directories, so a node-name label would let each
		// enumerate the other's containers and destroy them as orphans.
		return docker.New(deployment, docker.WithLogger(slog.Default())), nil

	case config.ProviderEC2:
		// Config validation is what guarantees this is non-nil, and the constructor
		// refuses an empty deployment identity for the same reason docker is
		// labelled with one: instances are TAGGED with it, and List feeds a loop
		// that terminates, so two installations sharing a tag is a way for one to
		// destroy the other's live jobs.
		if cfg.Node.EC2 == nil {
			return nil, errors.New("billet: node.ec2 is missing; the provider is ec2")
		}

		ec2Config := *cfg.Node.EC2
		ec2Config.NodeName = cfg.Node.Name

		return ec2.New(deployment, ec2Config, ec2.WithLogger(slog.Default()))

	case config.ProviderCodeBuild:
		if cfg.Node.CodeBuild == nil {
			return nil, errors.New("billet: node.codebuild is missing; the provider is codebuild")
		}

		// THE DEPLOYMENT IDENTITY IS EVEN MORE LOAD-BEARING HERE THAN ON EC2, because
		// a CodeBuild build CANNOT BE TAGGED: `StartBuild` has no field that becomes
		// one, so the per-instance owner tag the ec2 backend filters List on does not
		// exist. What replaces it is a dedicated project plus this identity carried as
		// an environment-variable marker — and List feeds a loop that STOPS builds, so
		// two installations sharing a project and an identity is a way for one to stop
		// the other's live jobs.
		//
		// THE CREDENTIAL CHAIN IS ADAPTED RATHER THAN DUPLICATED. It lives in
		// internal/provider/ec2 and a compute backend must not import a sibling
		// compute backend, so this is the one place that knows about both — exactly
		// what openArchiveStore already does for internal/archivestore. Extracting the
		// chain into a shared package is a separate change; moving a four-types-deep
		// redaction table is a security refactor that wants its own review and its
		// own mutation run rather than riding along here.
		return codebuild.New(deployment, *cfg.Node.CodeBuild,
			codebuild.WithLogger(slog.Default()),
			codebuild.WithCredentials(AWSCredentials()))

	case config.ProviderFirecracker:
		// BOTH BLOCKS ARE GUARANTEED BY VALIDATION — node.ceph is required for this
		// backend and refused for every other, and node.firecracker likewise — so
		// these are the guards that keep that true rather than cases that happen.
		if cfg.Node.Firecracker == nil {
			return nil, errors.New("billet: node.firecracker is missing; the provider is firecracker")
		}

		if cfg.Node.Ceph == nil {
			return nil, errors.New("billet: node.ceph is missing; every guest boots from a clone " +
				"of a golden image in the site's cluster")
		}

		// THE STORAGE IS BUILT HERE AND HANDED IN, because a provider and a store
		// are siblings that may not import each other. This is the one place that
		// knows about both.
		store, err := ceph.New(*cfg.Node.Ceph)
		if err != nil {
			return nil, err
		}

		return firecracker.New(deployment, *cfg.Node.Firecracker, store, firecrackerOptions(cfg)...)

	case config.ProviderTart:
		// Labelled with the DEPLOYMENT id for the docker backend's reason: two
		// billets on one Mac share a hostname, and the ownership marker is what
		// keeps each from destroying the other's guests as orphans.
		var tartCfg config.TartConfig
		if cfg.Node.Tart != nil {
			tartCfg = *cfg.Node.Tart
		}

		return tart.New(deployment,
			tart.WithLogger(slog.Default()),
			tart.WithConfig(tartCfg))

	case config.ProviderSimulated:
		// UNREACHABLE THROUGH config.Load, WHICH REFUSES THE KIND, and refused again
		// here because this switch is the one place that turns a kind into compute.
		// The simulated backend fabricates completions; a host running it would
		// report every job finished and run none.
		return nil, errors.New("billet: the simulated backend exists for billet's own test " +
			"harness and is never constructed by the CLI")

	default:
		return nil, fmt.Errorf("billet: unknown provider %q", cfg.Node.Provider)
	}
}

// NodeBundle loads the certificate this node presents, if it has one.
//
// Config validation is what refuses a network address without a bundle, so a nil
// return here means loopback — the one case where the control plane is inside
// this machine and there is nothing between the two to authenticate against.
func NodeBundle(cfg *config.Config) (*wirecert.Bundle, error) {
	if cfg.Node.TLS == nil {
		return nil, nil //nolint:nilnil // no bundle is a state, not an error
	}

	bundle, err := wirecert.LoadBundle(cfg.Node.TLS.CertPath, cfg.Node.TLS.KeyPath, cfg.Node.TLS.CAPath)
	if err != nil {
		return nil, err
	}

	// VALIDATED BEFORE ANYTHING IS WRITTEN FROM IT, because the deployment
	// identity this bundle names is about to be recorded permanently. A malformed
	// or mixed bundle would otherwise write deployment A into the state directory
	// and only then fail on the key pair — after which the CORRECT bundle for
	// deployment B is refused as an identity conflict, and an operator has to
	// clear state by hand for an enrollment that never succeeded.
	if _, err := wirecert.ClientTLS(bundle); err != nil {
		return nil, err
	}

	// CHECKED HERE, WHERE THE FILES ARE NAMED. The control plane refuses a
	// mismatch too, but it can only say "you are not who you claim" — this can say
	// which file on this host holds the wrong certificate, which is the sentence
	// an operator can act on.
	name, err := bundle.NodeName()
	if err != nil {
		return nil, err
	}

	// THE CERTIFICATE IS THE NAME, and an absent node.name is filled in from it
	// rather than defaulted from the hostname. The control plane authorises by
	// this name, so a machine whose hostname differs from it would otherwise be
	// refused for a value the operator never chose.
	if cfg.Node.Name == "" {
		cfg.Node.Name = name

		return &bundle, nil
	}

	if name != cfg.Node.Name {
		return nil, fmt.Errorf(
			"node.name is %q but %s was issued for %q; the control plane authorises by the "+
				"name in the certificate, so this node could only ever act as %q. Remove "+
				"node.name to take it from the certificate",
			cfg.Node.Name, cfg.Node.TLS.CertPath, name, name)
	}

	return &bundle, nil
}

// NewNodeClient is THE ONE CONSTRUCTION of the node's client from its
// configuration: the address, the name and the TLS state as the command
// resolves them, so a fixture that builds the client the way the command does
// and the command itself cannot disagree about the request base.
func NewNodeClient(cfg *config.Config, tlsConf *tls.Config) (*nodeclient.Client, error) {
	return nodeclient.New(nodeclient.Options{
		Base: cfg.Node.ServerAddr,
		Node: cfg.Node.Name,
		TLS:  tlsConf,
	})
}

// nodeLaunchConcurrency is how many launches a node of this provider runs at once.
// Four keeps a host starting guests while no single burst outruns its disk and
// network setup; one is a node as it always was.
func nodeLaunchConcurrency(kind config.ProviderKind) int {
	if kind == config.ProviderFirecracker {
		return 4
	}

	return 1
}

// RemoteShapes is the ordered shape catalogue this node registers, whichever
// remote backend it runs.
//
// CLONED, because the registration is carried across every reconnect and the
// caller keeps the config: a slice shared with cfg would let anything holding the
// config widen what this host claims it may buy after the numbers were validated.
//
// A HOST-BACKED PROVIDER REGISTERS NONE, and the allocator refuses one that does —
// its capacity is the machine, so a shape catalogue there is a purchase decision
// about compute nobody buys.
func RemoteShapes(cfg *config.Config) []config.RemoteShape {
	switch cfg.Node.Provider {
	case config.ProviderEC2:
		if cfg.Node.EC2 != nil {
			return slices.Clone(cfg.Node.EC2.InstanceTypes)
		}

	case config.ProviderCodeBuild:
		if cfg.Node.CodeBuild != nil {
			return slices.Clone(cfg.Node.CodeBuild.ComputeTypes)
		}

	case config.ProviderDocker, config.ProviderFirecracker, config.ProviderTart:
		return nil
	}

	return nil
}

// AWSCredentials adapts billet's one AWS credential chain to a consumer that
// declares its own interface over awssig.Credentials.
//
// ONE CHAIN, ADAPTED AT THE EDGE. The chain — environment variables, then this
// instance's IAM role over IMDSv2, with v1 refused rather than used as a fallback —
// lives in internal/provider/ec2 and carries a redaction table that took several
// rounds to get right. A second copy would be a second security boundary, and a
// compute backend importing a sibling compute backend would make one of them a
// library for the other. So consumers declare a narrow interface and cmd/billet
// converts, which is what openArchiveStore already does for internal/archivestore.
//
// Extracting the chain into internal/awscreds is a separate change.
func AWSCredentials() codebuild.CredentialSource {
	resolver := awscreds.Default()

	return credentialSourceFunc(func(ctx context.Context) (awssig.Credentials, error) {
		resolved, err := resolver.Credentials(ctx)
		if err != nil {
			return awssig.Credentials{}, err
		}

		return awssig.Credentials{
			AccessKeyID:     resolved.AccessKeyID,
			SecretAccessKey: resolved.SecretAccessKey,
			SessionToken:    resolved.SessionToken,
		}, nil
	})
}

// credentialSourceFunc adapts a function to the interface.
type credentialSourceFunc func(context.Context) (awssig.Credentials, error)

func (f credentialSourceFunc) Credentials(ctx context.Context) (awssig.Credentials, error) {
	return f(ctx)
}

// nodeGuestOS is what this host tells the control plane it can boot, and it is
// derived ONLY for codebuild.
//
// A CODEBUILD NODE'S ENVIRONMENT TYPE DECIDES ITS GUEST OS OUTRIGHT: a MAC_ARM
// project boots macOS and every other permitted environment boots Linux, and there
// is no configuration that could make one serve the other. Reporting it lets the
// plane refuse an obviously wrong dispatch at pick time instead of at the first
// launch, and `alloc.Bind` remains the durable check a node cannot route around.
//
// EVERY OTHER BACKEND KEEPS REPORTING NOTHING, which the plane reads as
// unconstrained. That is today's behaviour and changing it is not this backend's
// business: a firecracker or tart host's real capability comes from its images and
// its `nodes:` policy, and having the node start asserting a narrower answer would
// silently stop dispatching leases that place correctly now.
func nodeGuestOS(cfg *config.Config) []config.GuestOS {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return nil
	}

	if !cfg.Node.CodeBuild.EnvironmentType.Valid() {
		return nil
	}

	return []config.GuestOS{cfg.Node.CodeBuild.EnvironmentType.GuestOS()}
}

// codeBuildFleet is the reserved-capacity fleet this node draws on, or empty.
//
// Read only for a codebuild node: the allocator refuses a fleet reported by any
// other backend, because a shared-pool claim from a host that draws on no pool
// would keep a legitimate second node out of a fleet its provider never reads.
func codeBuildFleet(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.FleetARN
}

// codeBuildJITPath is where this node stages runner registrations, or empty.
//
// Reported for the same reason the fleet is and read under the same guard: the
// control plane sweeps registrations a dead node left behind, and the path is a
// codebuild fact the allocator refuses from any other backend.
func codeBuildJITPath(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.JITParameterPath
}

// codeBuildRegion is the region those registrations live in, or empty.
func codeBuildRegion(cfg *config.Config) string {
	if cfg.Node.Provider != config.ProviderCodeBuild || cfg.Node.CodeBuild == nil {
		return ""
	}

	return cfg.Node.CodeBuild.Region
}
