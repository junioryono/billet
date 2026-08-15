// Package firecracker runs each job in its own microVM on bare metal.
//
// THE ISOLATION BOUNDARY IS A KERNEL, which is the whole reason this backend
// exists and the reason the caching plane sits on top of it: a guest with its own
// kernel is something a block device can be attached to, and a container is not.
// Every cache billet will ever mount — a golden image, a per-job root disk, a
// sticky disk — is a block device, so nothing in that plane can start until this
// does.
//
// EVERY GUEST RUNS UNDER THE JAILER, never under bare firecracker. The jailer
// chroots the VMM, drops it to an unprivileged uid, puts it in a cgroup and gives
// it a pid namespace of its own before the VMM has parsed anything an operator
// wrote. Running firecracker directly would leave a process with the whole host
// filesystem in front of a guest whose job is running somebody's CI. (The seccomp
// filter is not the jailer's: it is compiled into the firecracker binary and on by
// default, which is worth knowing because it means it is present either way and
// absent from a debug build either way.)
//
// FOUR THINGS HERE WERE MEASURED ON THE REFERENCE HOST rather than read, and each
// of them is a way this package could have looked correct and not been:
//
//   - The jailer names its chroot after the RESOLVED --exec-file, so the versioned
//     binary behind a stable symlink — which is how the reference host installs
//     firecracker — decides the directory List enumerates. See jail.
//   - `jailer --daemonize` exits 0 for a VM that died during startup. Its exit code
//     is not evidence, and Launch confirms through the VMM's own API instead.
//   - The jailer creates a per-VM cgroup only when it is given at least one
//     --cgroup, and MIXING the two forms on one host wedges it outright. See
//     jailerArgs.
//   - The runner registration travels in the metadata service, placed before the
//     guest's first instruction, so it is never on a disk and never in argv.
package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// Instance is provider.Instance, aliased so this file does not repeat the package
// name on every line.
type Instance = provider.Instance

// RootDisk supplies the block device a guest boots from.
//
// AN INTERFACE BECAUSE STORAGE AND COMPUTE ARE SIBLINGS. A provider may not import
// the store and the store may not import a provider — depguard enforces it, and the
// reason is that either direction makes one of them unsubstitutable. So this
// package declares the two operations it needs and the wiring hands it something
// that performs them; internal/store/ceph satisfies this today by having the
// methods, with no adapter and no shared type.
type RootDisk interface {
	// ResolveGeneration turns a tier's image reference into one exact generation.
	//
	// SEPARATE FROM THE CLONE so that the concrete generation is known BEFORE
	// anything is done with it: it is what the launch logs, and "which image did
	// this job actually boot" has to be answerable from that line afterwards. A
	// reference that already names a generation comes back unchanged.
	ResolveGeneration(ctx context.Context, image string) (string, error)
	// CloneRoot makes a per-job copy-on-write clone of a golden image and maps it,
	// returning the host device path.
	CloneRoot(ctx context.Context, image, name string) (string, error)
	// DiscardRoot unmaps and removes a clone. It must be idempotent: teardown runs
	// on paths that have already failed once.
	DiscardRoot(ctx context.Context, name string) error
	// KernelFor reports which kernel file a generation was paired with, if any.
	//
	// THE PAIRING IS AN INVARIANT, NOT BOOKKEEPING. A guest booted with a different
	// kernel than the one its filesystem was verified against does not fail to
	// start -- it fails in the middle of somebody's job, which is why the two are
	// published together. Recording which kernel a generation needs means nothing
	// unless the launch asks.
	//
	// NOT FOUND IS NORMAL AND NOT AN ERROR: generations published by
	// build-guest-image.sh record no kernel, because that script installs none and
	// genuinely does not know which will be used.
	KernelFor(ctx context.Context, image, generation string) (string, bool, error)
}

// Provider launches one Firecracker microVM per job.
type Provider struct {
	log   *slog.Logger
	owner string
	cfg   config.FirecrackerConfig
	disk  RootDisk

	// execPath and execName are the firecracker binary with its symlinks already
	// followed, and the directory name the jailer derives from it.
	//
	// RESOLVED ONCE, AND THE RESOLVED PATH IS WHAT THE JAILER IS GIVEN. Passing the
	// configured symlink instead would let the two disagree the moment an operator
	// retargeted it: billet would build, enumerate and clean up under the name it
	// pinned at startup while the jailer chrooted into the new one — so the VMM
	// would boot into an empty directory, and a launch that failed that way would
	// leave a jail nothing lists. That is not hypothetical; it is what the first
	// manual smoke test did, and firecracker aborted with no message at all.
	execPath string
	execName string

	run    runner
	mknod  mknodFunc
	chown  chownFunc
	apiFor func(socket string) *vmmAPI
	// pidOwner reports whether a pid is still a jail's VMM. A seam because it
	// reads /proc, and because the rule it enforces — never signal a pid billet
	// could not tie to this microVM — is only testable if "could not tell" can be
	// staged.
	pidOwner func(pid int, jailID string) (bool, error)

	// bootWait bounds how long Launch waits for a VMM to answer its own API.
	bootWait time.Duration
}

// runner executes one command. A seam, so a test can assert the ARGUMENTS billet
// builds — which is where the mistakes are — without a hypervisor.
type runner func(ctx context.Context, bin string, args []string) ([]byte, error)

// mknodFunc creates a block device node in the jail mirroring a host device.
type mknodFunc func(path, hostDevice string, uid, gid int) error

// chownFunc gives a tree to the account the jailer will drop to.
type chownFunc func(root string, uid, gid int) error

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// withRunner replaces process execution. Unexported because its parameter is.
func withRunner(r runner) Option {
	return func(p *Provider) {
		if r != nil {
			p.run = r
		}
	}
}

// withPrivileged replaces the two operations that need root, for a test that has
// neither root nor a device to mirror.
func withPrivileged(m mknodFunc, c chownFunc) Option {
	return func(p *Provider) {
		if m != nil {
			p.mknod = m
		}

		if c != nil {
			p.chown = c
		}
	}
}

// withPidOwner replaces the pid-to-microVM check, so a test can stage the answer
// billet must refuse to act on: "could not tell".
func withPidOwner(f func(pid int, jailID string) (bool, error)) Option {
	return func(p *Provider) {
		if f != nil {
			p.pidOwner = f
		}
	}
}

// WithBootWait bounds how long Launch waits for a new VMM to answer.
func WithBootWait(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.bootWait = d
		}
	}
}

// DefaultBootWait bounds the wait for a VMM to come up and answer its API.
//
// A microVM boots in tens of milliseconds and the API socket appears before the
// guest does, so this is a bound on a wedged process rather than a budget for
// booting. It is far inside the node command timeout, so a VMM that never answers
// surfaces as a launch failure the listener can hand capacity back for, rather
// than as a command the control plane gives up on and calls custody.
const DefaultBootWait = 30 * time.Second

// New builds a firecracker provider. owner names this billet deployment and is
// written into every jail it creates.
func New(owner string, cfg config.FirecrackerConfig, disk RootDisk, opts ...Option) (*Provider, error) {
	// NORMALIZED BEFORE IT IS STORED, not merely to be checked. ownerOf trims what
	// it reads back, so an identity kept with its padding would never equal the
	// marker it wrote — and a provider that fails to recognise its OWN jails
	// reports an empty inventory, which frees the capacity of every lease it is
	// running. The ec2 and ceph blocks each shipped this defect once, in the other
	// direction.
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("firecracker: a provider needs the deployment identity that marks " +
			"its jails, or it cannot tell its own microVMs from another billet's")
	}

	if disk == nil {
		return nil, errors.New("firecracker: a provider needs a root disk source: every guest " +
			"boots from a clone of a golden image, so there is nothing to launch without one")
	}

	// NORMALIZED AND RE-CHECKED HERE, because this constructor is exported and
	// cannot assume its configuration came through config.Load — the same reason
	// alloc.New and ec2.New re-apply their own rules. CheckFirecracker holds every
	// rule, so there is one place to get them right rather than two that drift.
	cfg.Normalize()

	if errs := config.CheckFirecracker(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("firecracker: %w", errors.Join(errs...))
	}

	p := &Provider{
		log:      slog.Default(),
		owner:    owner,
		cfg:      cfg,
		disk:     disk,
		run:      execRunner,
		mknod:    mknodBlock,
		chown:    chownTree,
		apiFor:   newVMMAPI,
		pidOwner: pidIsVMM,
		bootWait: DefaultBootWait,
	}

	for _, opt := range opts {
		opt(p)
	}

	// AN OPTION MUST NOT BE ABLE TO PRODUCE A PANIC, which is the rule ec2.New
	// arrived at after guarding one option while claiming the invariant.
	if p.log == nil {
		return nil, errors.New("firecracker: WithLogger was given no logger")
	}

	var err error

	if p.execPath, p.execName, err = resolveExec(cfg.BinaryPath); err != nil {
		return nil, err
	}

	// AT CONSTRUCTION, because the alternative is per launch, after the root disk
	// has already been cloned, in an error that names no field.
	if err := checkSocketPath(cfg.ChrootBase, p.execName); err != nil {
		return nil, err
	}

	return p, nil
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderFirecracker }

// Accepts reports whether this backend may run work of that trust class.
//
// A MICROVM IS A REAL ISOLATION BOUNDARY, so unlike the container backend this one
// CAN run code billet cannot vouch for: a fork's pull request gets its own kernel,
// and the machine is destroyed with the job.
//
// BUT THE BOUNDARY IS THE KERNEL, NOT THE NETWORK — the same distinction the ec2
// backend draws, and it is sharper here. That backend's guests are in a VPC
// somebody built; these are on a bridge on a machine that also holds the Ceph
// cluster, the control-plane database and, on the reference host, an overlay
// network that reaches production. A guest on the ordinary bridge reaches all of
// it. So untrusted work runs only once a SEPARATE bridge has been described for
// it, and its absence is what refuses the job — rather than defaulting onto the
// trusted one, which is the direction that cannot be undone once a job has run.
//
// UNKNOWN is refused outright, and that is a different judgement: untrusted is a
// classification billet made, while unknown means it could not classify the job at
// all, so there is no basis for choosing either network.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	switch trust {
	case provider.TrustTrusted:
		return nil

	case provider.TrustUntrusted:
		if p.cfg.UntrustedBridge != "" {
			return nil
		}

		return errors.New("firecracker: refusing to run untrusted work until it has a network of " +
			"its own: a microVM isolates the kernel but not the bridge it is attached to, so set " +
			"node.firecracker.untrusted_bridge to one that reaches only what a fork's pull " +
			"request should be able to reach")

	case provider.TrustUnknown:
		return errors.New("firecracker: refusing to run work billet could not classify: an " +
			"unrecognised event establishes no provenance, so there is no basis for choosing " +
			"which network to place it on")

	default:
		return fmt.Errorf("firecracker: refusing to run %s work", trust)
	}
}

// bridgeFor reports which bridge a workload of that trust class attaches to.
func (p *Provider) bridgeFor(trust provider.TrustClass) string {
	if trust == provider.TrustUntrusted {
		return p.cfg.UntrustedBridge
	}

	return p.cfg.Bridge
}

// Launch starts one microVM running the job its JIT config names.
//
// IT RETURNS WHEN THE VMM SAYS IT IS RUNNING, not when the jailer returns. That is
// not belt and braces: `jailer --daemonize` exits 0 for a VM that died during
// startup — measured, with a pid file and an API socket both present beside a VMM
// that had exited 1 — which is exactly the shape of the docker default-command bug
// this repository was bitten by, where every signal reported success and no runner
// ever started.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*Instance, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}

	// Checked again here, not only via Accepts. A caller is expected to ask first so
	// a refusal costs no runner registration, but a backend that only refuses when
	// asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	j := p.jailFor(spec.Name)

	// THE JAIL IS CLAIMED BEFORE ANYTHING IS CLONED, and the order is not tidiness.
	// A clone exists in the cluster and a mapping exists in the kernel the instant
	// CloneRoot returns; if billet died between that and creating the jail, nothing
	// on this host would carry the lease's name — List would report nothing, Find
	// would say "not found", the caller would conclude nothing started and free the
	// lease, and the clone would hold pool space with no way left to attribute it.
	// A jail that exists first is a jail List reports and whose Destroy discards
	// the disk.
	//
	// IT IS ALSO THE ONLY STEP WHOSE FAILURE MUST NOT UNWIND. Refusing a jail that
	// already exists is a refusal to touch a previous microVM's state, so tearing
	// it down would destroy exactly what the error says was left alone.
	if err := p.claim(j); err != nil {
		return nil, err
	}

	// THE HOST'S SCARCE THINGS, TAKEN AND WRITTEN DOWN BEFORE ANYTHING USES THEM.
	// A uid and a device name are allocated rather than derived, so teardown can only
	// find them again by reading what this recorded — and it has to be readable
	// before the first thing that would need releasing exists.
	res, err := p.claimResources(j)
	if err != nil {
		return nil, errors.Join(err, j.remove())
	}

	// RESOLVED HERE, SO EVERYTHING AFTER IT NAMES ONE GENERATION. A tier may say
	// `@verified`, which means "the newest one proved to boot" — a moving target by
	// design. Passing that through would leave the launch log, and therefore any
	// later question about which image a job actually ran, pointing at a word rather
	// than at an artifact somebody can go and look at.
	image, err := p.disk.ResolveGeneration(ctx, spec.Image)
	if err != nil {
		return nil, errors.Join(err, j.remove())
	}

	spec.Image = image

	// WHICH KERNEL THIS GENERATION WAS VERIFIED AGAINST, resolved before anything
	// is placed in the jail. A node-wide kernel is a default, not a decision: an
	// operator who points one config at two generations that need different kernels
	// has no way to be right, and the resulting failure lands inside a job rather
	// than at launch.
	kernel, err := p.kernelFor(ctx, spec.Image)
	if err != nil {
		return nil, errors.Join(err, j.remove())
	}

	device, err := p.disk.CloneRoot(ctx, spec.Image, spec.Name)
	if err != nil {
		// A CLONE ERROR DOES NOT PROVE NO CLONE EXISTS. `rbd clone` can be killed by
		// a cancelled context after the cluster has already created the image, and
		// the map-failure path says outright that it may leave one behind. Removing
		// the jail without discarding would then leave a cache-pool image with
		// nothing on the host carrying its name — which is the unattributable orphan
		// claiming the jail first exists to prevent, reached through a caller's
		// timeout instead of a crash.
		return nil, errors.Join(
			fmt.Errorf("firecracker: root disk for %s: %w", spec.Name, err),
			p.unwind(ctx, j, spec, res, err))
	}

	// FROM HERE EVERY FAILURE UNWINDS WHAT IT MADE, in reverse order, and says so if
	// it cannot. The caller treats a launch error as "billet does not know whether
	// something started" and asks Find — so this is not the safety net, it is the
	// ordinary case being tidy. What it must never do is replace the reason the
	// launch failed with the reason a cleanup failed.
	inst, err := p.launch(ctx, j, spec, device, kernel, res)
	if err != nil {
		return nil, errors.Join(err, p.unwind(ctx, j, spec, res, err))
	}

	p.log.Info("launched a microVM", "runner", spec.Name, "vcpu", spec.VCPU,
		"memory", spec.Memory, "image", spec.Image, "trust", spec.Trust)

	return inst, nil
}

// launch does the work Launch unwinds on failure.
func (p *Provider) launch(
	ctx context.Context, j jail, spec provider.Spec, device, kernel string, res resources,
) (*Instance, error) {
	if err := p.build(j, device, kernel, res); err != nil {
		return nil, err
	}

	if err := p.addTap(ctx, res.Tap, p.bridgeFor(spec.Trust), res.UID); err != nil {
		return nil, err
	}

	if err := p.startVMM(ctx, j, res); err != nil {
		return nil, err
	}

	api, err := p.awaitAPI(ctx, j)
	if err != nil {
		return nil, err
	}

	if err := p.configure(ctx, api, spec, res.Tap); err != nil {
		return nil, err
	}

	if err := api.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"}); err != nil {
		return nil, fmt.Errorf("firecracker: start the guest for %s: %w", spec.Name, err)
	}

	// THE CONFIRMATION, and the reason this function exists in this shape. A VMM
	// that accepted every configuration call can still fail to start the guest, and
	// nothing before this line would have noticed.
	info, err := api.info(ctx)
	if err != nil {
		return nil, fmt.Errorf("firecracker: %s did not report its state after being started: %w",
			spec.Name, err)
	}

	if info.State != stateRunning {
		return nil, fmt.Errorf("firecracker: %s was started and reports state %s rather than %s",
			spec.Name, bounded(info.State), stateRunning)
	}

	// AND THAT IT IS THE RIGHT ONE. The socket lives at a path derived from the
	// name, so a stale socket left by an earlier VM with the same name would answer
	// happily — and billet would report a launch for a guest running somebody
	// else's job.
	if info.ID != spec.Name {
		return nil, fmt.Errorf("firecracker: the vmm answering for %s calls itself %s, so that "+
			"socket belongs to a different microVM", spec.Name, bounded(info.ID))
	}

	return &Instance{ID: spec.Name, Name: spec.Name, Running: true}, nil
}

// checkSpec refuses a spec that would produce a microVM which cannot run the job.
func checkSpec(spec provider.Spec) error {
	if spec.Name == "" {
		return errors.New("firecracker: a spec needs a name")
	}

	if _, ours := provider.LeaseOf(spec.Name); !ours {
		// THE NAME IS THE JAIL ID AND THE CLONE NAME, so it is not decoration here
		// the way it is for a container that also carries a label. Everything this
		// backend can find again — the chroot, the socket, the root disk, the tap —
		// is derived from it, and a name that does not encode a lease produces
		// compute that reconciliation cannot attribute to anything.
		return fmt.Errorf("firecracker: %s does not name a lease, and this backend derives a "+
			"jail id, a root disk and a network device from it", bounded(spec.Name))
	}

	if spec.Image == "" {
		return fmt.Errorf("firecracker: %s has no image; this backend reads the tier's image as "+
			"a golden image in the ceph image pool, written image@snapshot", spec.Name)
	}

	if spec.JITConfig == "" {
		return fmt.Errorf("firecracker: %s has no JIT config, so nothing would register", spec.Name)
	}

	// REFUSED, not defaulted, for the reason every backend refuses it: a guest that
	// boots without being told what to run is a microVM that starts, reports
	// success, and never registers a runner, while the job sits queued until GitHub
	// gives up on it.
	if len(spec.Command) == 0 {
		return fmt.Errorf("firecracker: %s has no command, so the guest would boot without ever "+
			"starting a runner and the job would stay queued", spec.Name)
	}

	// AND EVERY ARGUMENT IS SOMETHING THAT CAN ARRIVE UNCHANGED, because the one
	// promise this path makes about a command is that the guest runs the argv it was
	// given. The command travels as JSON, and `json.Marshal` does not fail on a Go
	// string holding invalid UTF-8 — it substitutes U+FFFD and returns success. So a
	// byte billet cannot carry would otherwise be REWRITTEN in flight and the guest
	// would run a subtly different command, silently, with nothing anywhere saying so.
	//
	// A NUL cannot be in an argv at all: execve's arguments are NUL-terminated, so an
	// argument containing one is a request the kernel has no way to honour.
	//
	// Refusing is the whole point. Neither of these is a command somebody meant to
	// write, and a launch that stops here names the tier that has to be fixed.
	// AND THE FIRST ARGUMENT NAMES SOMETHING. An empty argv[0] passes every check
	// above — it is a non-empty command, valid UTF-8, no NUL — and then costs a jail,
	// a uid, a tap and a cloned disk before the guest fails to exec it. Refusing here
	// spends nothing, and says which tier to fix instead of leaving a launch that
	// failed inside a machine nobody can see into.
	if spec.Command[0] == "" {
		return fmt.Errorf("firecracker: %s has an empty program in its command, so there is "+
			"nothing for the guest to exec", spec.Name)
	}

	// AND THE WHOLE PAYLOAD FITS IN THE SERVICE THAT HAS TO HOLD IT, checked here
	// rather than discovered at the end of a launch.
	//
	// Without this a command large enough to overflow the data store passes every
	// check, and billet then claims a jail, a uid, a tap and a cloned disk, starts a
	// VMM, and fails on the PUT that fills the metadata. It unwinds correctly — but a
	// tier configured that way pays for a full launch on every job to reach the same
	// refusal, and the refusal names an HTTP request rather than the tier.
	payload, err := metadata(spec)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("firecracker: encode the metadata for %s: %w", spec.Name, err)
	}

	if len(encoded) > mmdsSizeLimit {
		return fmt.Errorf("firecracker: %s needs %d bytes of metadata and the service holds "+
			"%d, so the guest could never be told what to run", spec.Name, len(encoded),
			mmdsSizeLimit)
	}

	for i, arg := range spec.Command {
		if !utf8.ValidString(arg) {
			return fmt.Errorf("firecracker: %s has argument %d with bytes that are not valid "+
				"UTF-8, and they cannot reach the guest unchanged", spec.Name, i)
		}

		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("firecracker: %s has argument %d containing a NUL, which no "+
				"argv can carry", spec.Name, i)
		}
	}

	if spec.VCPU <= 0 {
		return fmt.Errorf("firecracker: %s asks for %d vCPU", spec.Name, spec.VCPU)
	}

	if spec.Memory <= 0 {
		return fmt.Errorf("firecracker: %s asks for %s of memory", spec.Name, spec.Memory)
	}

	return nil
}

// jailFor is the chroot this instance name maps to.
func (p *Provider) jailFor(name string) jail {
	return jail{base: p.cfg.ChrootBase, execName: p.execName, id: name}
}

// configure places every resource the guest needs, while it is still stopped.
//
// THE ORDER IS NOT ARBITRARY. The metadata service has to be configured against a
// network interface that already exists, and the credential has to be in it before
// the guest can ask — which is the whole reason this backend drives the API instead
// of handing firecracker a config file, since a config file starts the machine as
// it is read.
func (p *Provider) configure(
	ctx context.Context, api *vmmAPI, spec provider.Spec, tap string,
) error {
	memMiB := int64(spec.Memory) / int64(config.MiB)
	if memMiB <= 0 {
		return fmt.Errorf("firecracker: %s asks for %s of memory, which is less than the 1MiB "+
			"the vmm counts in", spec.Name, spec.Memory)
	}

	md, err := metadata(spec)
	if err != nil {
		return err
	}

	for _, step := range []struct {
		path string
		body any
	}{
		{"/machine-config", map[string]any{
			"vcpu_count":   spec.VCPU,
			"mem_size_mib": memMiB,
			// SMT OFF. A vCPU billet charged a lease for is a core's worth of
			// capacity, and handing a guest two hyperthreads of one core while the
			// ledger recorded two vCPU would over-commit the machine by exactly the
			// amount nobody can see.
			"smt": false,
		}},
		{"/boot-source", map[string]any{
			"kernel_image_path": "/" + guestKernel,
			"boot_args":         bootArgs,
		}},
		{"/drives/rootfs", map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   "/" + guestRootDisk,
			"is_root_device": true,
			"is_read_only":   false,
		}},
		{"/network-interfaces/" + guestInterface, map[string]any{
			"iface_id":      guestInterface,
			"host_dev_name": tap,
		}},
		// V2, WHICH IS A SECURITY PROPERTY RATHER THAN A VERSION, AND V1 IS THE
		// DEFAULT. Under V1 any process in the guest reads the metadata with a bare
		// GET, so a workflow step could take the runner registration; V2 requires a
		// PUT to mint a session token first. Since the service defaults to V1,
		// naming the version here is what makes the credential safe rather than a
		// belt-and-braces restatement — the same reason billet pins IMDSv2 on the
		// instances its ec2 backend launches.
		{"/mmds/config", map[string]any{
			"version":            "V2",
			"network_interfaces": []string{guestInterface},
			"ipv4_address":       mmdsAddress,
		}},
		{"/mmds", md},
	} {
		if err := api.put(ctx, step.path, step.body); err != nil {
			return fmt.Errorf("firecracker: configure %s for %s: %w", step.path, spec.Name, err)
		}
	}

	return nil
}

// metadata is what the guest agent reads once and consumes.
//
// THE CREDENTIAL LIVES HERE AND NOWHERE ELSE. It is not in argv, where every
// process on the host could read it out of /proc — the mistake the container
// backend documents at length — and it is not on a disk, where it would survive
// the read and outlive the job. The metadata service holds it in the VMM's memory
// and it dies with the machine.
//
// It is written BEFORE InstanceStart, so there is no window in which the guest is
// running and the answer is not there yet.
// EVERY LEAF IS A STRING, AND THAT IS A CONSTRAINT OF THE SERVICE RATHER THAN A
// STYLE. Firecracker answers a guest's plain GET in IMDS format, which can render a
// string and can list the keys of an object — and nothing else. A leaf that is an
// array, a number or a boolean is not served: the guest's request fails, and it fails
// per-key, so the rest of the tree keeps working and only the one field is missing.
//
// MEASURED, because it is the failure this cost the most time to see. `command` was
// a []string here. The guest booted, got its address, minted its session token, read
// the contract and read the registration — and then its fetch of `command` failed, so
// the agent stopped with "no command in the metadata" and the microVM ran nothing. A
// job that could not start looked exactly like a guest image that would not boot.
//
// So a command is carried as its JSON encoding, and the agent parses it back. That
// keeps the one property worth having — billet never word-splits somebody's argv —
// while staying inside what the service can actually hand over. `metadataLeaves` in
// the tests walks this tree and fails on any leaf that is not a string, so a field
// added later cannot reintroduce it.
func metadata(spec provider.Spec) (map[string]any, error) {
	command, err := json.Marshal(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("firecracker: encode command for %s: %w", spec.Name, err)
	}

	return map[string]any{
		"latest": map[string]any{
			"meta-data": map[string]any{
				"billet": map[string]any{
					"contract":    GuestContract,
					"runner-name": spec.Name,
					"jit-config":  spec.JITConfig,
					"command":     string(command),
				},
			},
		},
	}, nil
}

// GuestContract is the version of this layout the guest agent must understand.
//
// THE AGENT LIVES IN THE IMAGE, NOT IN THIS BINARY, AND THAT IS THE PROBLEM IT
// SOLVES. A guest image is published once and booted for months; billet is upgraded
// independently. So the two can drift, and the drift is silent in the worst
// direction: a billet that renamed a key here would hand an older agent metadata it
// does not recognise, and the guest would boot, find nothing it could use, and
// register no runner — a microVM that starts perfectly and runs nothing, which is
// the failure this whole backend is built to make impossible.
//
// billet cannot inspect an image to find out what it understands. What it CAN do is
// say what it is speaking, so the guest can refuse loudly instead of half-working.
// The agent compares this against what it was built for and stops with a message
// naming both if they differ.
//
// BUMP IT WHEN THE SHAPE CHANGES — a renamed key, a new required field, a different
// encoding — and republish the image in the same change. Adding an OPTIONAL key that
// an older agent can ignore is not a change to the shape and does not need one.
//
// Republishing is safe precisely because generations are immutable: a job holding a
// clone of the old generation keeps the agent it booted with, and clone v2 lets the
// old generation be removed once nothing holds it.
const GuestContract = "2"

// mmdsSizeLimit is how much the metadata service will hold.
//
// MEASURED FROM THE BINARY, not assumed: `firecracker --help` documents
// `--http-api-max-payload-size` as `[default: "51200"]` and `--mmds-size-limit` as
// taking that same default when it is not given. billet passes NEITHER flag, so both
// are in force at 51200 bytes — which is why this is a constant here rather than
// something read from the configuration.
//
// It bounds a runner registration plus a tier's command, and a JIT registration is
// hundreds of bytes, so nothing ordinary comes close. It exists for the case that is
// not ordinary, where the alternative is a launch that spends everything and then
// fails on an HTTP request.
const mmdsSizeLimit = 51200

// bootArgs is the guest kernel command line.
//
// `console=` IS ABSENT ON PURPOSE. A serial console costs boot time on every job,
// and under --daemonize the VMM's stdout is /dev/null, so it would be written
// nowhere — measured: the guest's output does not reach the VMM's own log file,
// which carries VMM-level lines only.
//
// `pci=off` and the i8042 options are Firecracker's own documented arguments for a
// machine with no PCI bus and no keyboard controller; without them the guest spends
// its first seconds probing hardware that is not there.
const bootArgs = "reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd " +
	"root=/dev/vda rw"

// guestInterface is the one network device a guest gets.
const guestInterface = "eth0"

// mmdsAddress is the link-local address the metadata service answers on. The same
// one every cloud uses, so a stock guest agent needs no special case.
const mmdsAddress = "169.254.169.254"

// startVMM runs the jailer, which chroots, drops privileges and execs the VMM.
func (p *Provider) startVMM(ctx context.Context, j jail, res resources) error {
	if _, err := p.run(ctx, p.cfg.JailerPath, p.jailerArgs(j, res)); err != nil {
		return fmt.Errorf("firecracker: start the jailer for %s: %w", j.id, err)
	}

	return nil
}

// jailerArgs is one jailer invocation.
//
// `--cgroup` IS PASSED EVEN THOUGH BILLET SETS NO LIMIT THROUGH IT, and that is the
// least obvious line here. The jailer creates a per-VM cgroup only when it is given
// at least one, and — measured — once any VM on the host has been started WITH one,
// a later VM started WITHOUT one fails outright:
// `CgroupMove("/sys/fs/cgroup/firecracker-v1.16.1") Resource busy`. So the two forms
// cannot coexist on a machine, and the choice is which one every launch uses. A
// per-VM cgroup is worth having on its own terms: it is what makes a runaway VMM
// killable as a group rather than as a pid.
//
// The value is cpu.weight at its default of 100, which changes nothing. The vCPU
// and memory a guest gets are set on the VMM itself, where the ledger's numbers
// belong; this is about the cgroup EXISTING.
func (p *Provider) jailerArgs(j jail, res resources) []string {
	return []string{
		"--id", j.id,
		"--exec-file", p.execPath,
		"--uid", strconv.Itoa(res.UID),
		"--gid", strconv.Itoa(res.GID),
		"--chroot-base-dir", p.cfg.ChrootBase,
		"--cgroup-version", "2",
		"--cgroup", "cpu.weight=100",
		// A PID NAMESPACE OF ITS OWN, and the reason is a pid FILE rather than the
		// isolation — though the isolation is worth having, since a VMM that cannot
		// see a host process cannot signal one.
		//
		// The jailer's documentation says it writes `<exec-name>.pid` only when
		// this is passed. Measured against v1.16.1 it writes one either way, and
		// billet's teardown reads that file to find the process it must stop — so
		// depending on the undocumented behaviour means a future version could stop
		// writing it and billet would read "no pid file" as "nothing is running",
		// then unmap the block device of a guest in the middle of a job. Asking for
		// the documented contract costs nothing and removes that.
		"--new-pid-ns",
		// DETACHED, so billet is not the VMM's parent. A node that restarts must
		// leave running jobs running — that is what restart recovery adopts — and a
		// VMM that died with its launcher would fail every build on the host every
		// time billet was upgraded.
		"--daemonize",
		"--",
		"--api-sock", "/" + filepath.Join("run", "firecracker.socket"),
		"--log-path", "/" + vmmLog,
		"--level", "Info",
	}
}

// vmmLog is the VMM's own log inside the chroot. It carries VMM-level lines only —
// the guest's console is not in it.
const vmmLog = "fc.log"

// awaitAPI waits for a new VMM to answer, and reports honestly when it never does.
//
// THE SOCKET EXISTING IS NOT THE VMM ANSWERING — measured, a VMM that exited during
// startup leaves its socket file behind — so this polls the API itself rather than
// stat-ing a path. What it is waiting out is the millisecond or two between the
// jailer returning and the VMM binding its socket.
func (p *Provider) awaitAPI(ctx context.Context, j jail) (*vmmAPI, error) {
	api := p.apiFor(j.socket())

	ctx, cancel := context.WithTimeout(ctx, p.bootWait)
	defer cancel()

	ticker := time.NewTicker(bootPoll)
	defer ticker.Stop()

	var last error

	for {
		info, err := api.info(ctx)
		if err == nil {
			if info.ID != j.id {
				return nil, fmt.Errorf("firecracker: the vmm at %s calls itself %s rather than "+
					"%s", j.socket(), bounded(info.ID), j.id)
			}

			return api, nil
		}

		last = err

		select {
		case <-ctx.Done():
			// THE VMM'S OWN LOG, because the reason is in it and nowhere else. The
			// jailer said nothing (it exited 0), the socket may well exist, and the
			// sentence that explains the failure — a bad drive, a refused tap — was
			// written inside the jail.
			return nil, fmt.Errorf("firecracker: %s never answered its api socket: %w%s",
				j.id, last, p.vmmLogTail(j))
		case <-ticker.C:
		}
	}
}

// bootPoll is how often a starting VMM is asked whether it is up yet.
const bootPoll = 20 * time.Millisecond

// vmmLogTail reads the end of a VMM's own log, for a diagnostic.
//
// Best effort and bounded: it is another program's output on its way into an error
// string, and it is the only place the reason for a failed boot is written.
func (p *Provider) vmmLogTail(j jail) string {
	raw, err := os.ReadFile(filepath.Join(j.root(), vmmLog))
	if err != nil {
		return ""
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	keep := lines
	if len(keep) > vmmLogLines {
		keep = keep[len(keep)-vmmLogLines:]
	}

	return " (its log ends: " + bounded(strings.Join(keep, "; ")) + ")"
}

// vmmLogLines is how much of a failed VMM's log is quoted.
const vmmLogLines = 3

// Destroy removes a microVM and everything it owns.
//
// FOUR THINGS OUTLIVE A GUEST and none of them is collected by anything else: the
// VMM process, its jail, the tap device on the host bridge, and the root disk —
// which is a mapped kernel block device AND an image holding pool space. Measured:
// SIGTERM stops the VMM and leaves the other three exactly where they were.
//
// Idempotent, because teardown runs on paths that have already failed once. Every
// step tolerates its subject being absent, and the errors are joined rather than
// returned at the first one — stopping early would leave a mapped device behind
// because a directory was already gone.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("firecracker: destroy needs a microVM id")
	}

	if _, ours := provider.LeaseOf(id); !ours {
		return fmt.Errorf("firecracker: %s does not name a lease, so it is not a microVM billet "+
			"started", bounded(id))
	}

	j, found, err := p.findJail(id)
	if err != nil {
		return err
	}

	if !found {
		// NO JAIL, BUT NOT NECESSARILY NOTHING. The root disk is named after the
		// lease rather than after the jail, and a device name is recorded in a claim
		// that outlives the jail it named — so a Destroy that removed the jail and
		// then failed its discard would otherwise converge to "success" with a tap
		// still attached to the bridge and a uid still held. Nothing enumerates
		// network devices looking for orphans, so it would stay there.
		var failures []error

		if orphan, err := p.claimedBy(id); err != nil {
			failures = append(failures, err)
		} else if err := p.releaseOrphaned(ctx, orphan, id); err != nil {
			failures = append(failures, err)
		}

		return p.discardWith(ctx, id, failures)
	}

	// NOT THIS DEPLOYMENT'S IS NOT THIS DEPLOYMENT'S TO DESTROY. Find refuses to
	// even report another billet's microVM; this is the same rule at the layer that
	// ACTS rather than the one that reports, which is where it actually protects
	// anything.
	owner, err := ownerOf(j)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
	}

	if err == nil && owner != p.owner {
		return fmt.Errorf("firecracker: %s belongs to deployment %s, not to this one",
			j.dir(), bounded(owner))
	}

	// STOPPING COMES FIRST AND NOTHING PROCEEDS WITHOUT IT.
	//
	// Everything below destroys evidence: the jail holds the pid file, which is the
	// only handle for ever stopping this VMM, and the tap is the guest's network. A
	// teardown that removed them while the VMM was still running — or while billet
	// merely could not TELL whether it was — would leave a guest executing a job
	// that nothing can find, nothing can kill, and that List no longer reports, so
	// its capacity is handed back and sold to another job. That is the exact failure
	// this backend's inventory exists to prevent, reached through its teardown.
	if err := p.stopVMM(ctx, j); err != nil {
		return err
	}

	res, err := resourcesOf(j)
	if err != nil {
		return err
	}

	var failures []error

	// THE NAME IS ONLY GIVEN BACK IF THE DEVICE ACTUALLY WENT. A device that would
	// not delete is still attached to the bridge, and nothing enumerates orphan
	// devices — so handing the name back wedges the host: the next launch draws the
	// same lowest-free name, `ip tuntap add` refuses it because the kernel already
	// has it, that launch fails and releases the name, and every launch after it
	// fails identically until an operator intervenes. Keeping the claim is what lets
	// a later teardown find this device and finish removing it.
	tapGone := true

	if err := p.deleteTap(ctx, res.Tap); err != nil {
		failures = append(failures, err)
		tapGone = false
	}

	if err := p.removeCgroup(j); err != nil {
		failures = append(failures, err)
	}

	// PAST A FAILED TAP REMOVAL, deliberately, unlike the stop above. The VMM is
	// already gone by here, so the remaining steps are reclaiming things rather than
	// acting on live compute — and stopping at the first would leave a mapped block
	// device behind because a network device would not go.
	if err := j.remove(); err != nil {
		failures = append(failures, err)
	}

	// AFTER THE JAIL. Releasing a claim while the jail still names it would let
	// another launch take the uid a VMM may still be running as.
	//
	// Each release checks that the claim STILL names this jail: the jail is gone by
	// now, which is precisely the reaper's condition, so a concurrent launch may have
	// legitimately taken this uid or device name in the gap. Deleting it by name
	// alone would then free a live claim and hand a running microVM's uid to the
	// launch after it.
	held := res
	if !tapGone {
		held.Tap = ""
	}

	if err := p.releaseResources(held, j.id); err != nil {
		failures = append(failures, err)
	}

	return p.discardWith(ctx, id, failures)
}

// discardWith releases the root disk and reports it alongside whatever else failed.
//
// LAST, BECAUSE THE VMM HOLDS IT OPEN. Unmapping a device a running VMM has open
// fails, so the root disk goes only once the process that was reading it is gone.
func (p *Provider) discardWith(ctx context.Context, id string, failures []error) error {
	if err := p.disk.DiscardRoot(ctx, id); err != nil {
		failures = append(failures,
			fmt.Errorf("firecracker: discard the root disk of %s: %w", id, err))
	}

	if len(failures) > 0 {
		return errors.Join(failures...)
	}

	p.log.Info("destroyed a microVM", "runner", id)

	return nil
}

// stopVMM stops a microVM, and waits until it has stopped.
//
// BY SIGNALLING THE VMM, BECAUSE THE API HAS NO WAY TO KILL ONE. Its only shutdown
// action is SendCtrlAltDel, which is a keyboard event the GUEST has to choose to
// act on — measured against a real guest, the VMM was still answering twenty
// seconds later. billet destroys a microVM because the job is over or its lease is
// gone, and neither of those is something the guest gets a say in. The container
// backend's `docker rm --force` is the same judgement.
//
// THE PID IS PROVEN TO BE THIS MICROVM'S BEFORE ANYTHING IS SIGNALLED. A pid file
// is a number, and a stale one holds a number the kernel has since given to
// something else — while this backend runs as root, so a signal sent on that
// evidence lands wherever the number now points. /proc/<pid>/cmdline carries the
// jailer's `--id`, measured, so it is checked first and a pid that cannot be
// verified is not signalled at all. Failing closed there costs a jail that has to
// be cleaned up by hand; failing open costs an arbitrary process on the host.
func (p *Provider) stopVMM(ctx context.Context, j jail) error {
	pid, err := p.vmmPID(j)
	if err != nil {
		return err
	}

	if pid == 0 {
		// No pid file, or no process answering to it. Either way there is nothing
		// running — the idempotent case this runs into constantly.
		return nil
	}

	// A REFERENCE TO THE PROCESS, TAKEN BEFORE IT IS CHECKED AND USED FOR EVERY
	// SIGNAL BELOW.
	//
	// vmmPID has already confirmed this pid is the VMM, but confirming and signalling
	// are two operations: in between, the VMM can exit and the kernel can hand its
	// number to something else, and this backend signals as root. The handle refers
	// to the process rather than to the number, so a recycled pid gets ESRCH instead
	// of a signal meant for a microVM.
	handle, alive, err := openVMM(pid)
	if err != nil {
		return fmt.Errorf("firecracker: stop the microVM %s: %w", j.id, err)
	}

	if !alive {
		return nil
	}

	defer handle.close()

	// SIGTERM FIRST, because firecracker exits cleanly on it — measured — and a
	// clean exit closes the guest's disk rather than leaving the mapped device to
	// be torn out from under it.
	if err := handle.signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("firecracker: stop the microVM %s: %w", j.id, err)
	}

	err = p.awaitExit(ctx, j, pid)
	if err == nil {
		return nil
	}

	// ESCALATION NEEDS A CONFIRMED PID, NOT MERELY A FAILED WAIT.
	//
	// awaitExit fails for two unlike reasons: the VMM outlived its grace, or billet
	// could not TELL whether the pid is still the VMM. Killing on the second is the
	// act process.go refuses by name — signalling, as root, a number nothing ties to
	// this jail. So the pid is checked once more, and a check that cannot answer
	// propagates rather than escalating.
	owns, checkErr := p.pidOwner(pid, j.id)
	if checkErr != nil {
		return errors.Join(err, checkErr)
	}

	if !owns {
		// It exited while billet was deciding, which is the outcome that was wanted.
		return nil
	}

	// AND SIGKILL IF IT WILL NOT GO, because the alternative is a microVM holding a
	// mapped block device open forever while its capacity stays charged.
	if err := handle.signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("firecracker: kill the microVM %s, which did not stop when asked: %w",
			j.id, err)
	}

	return p.awaitExit(ctx, j, pid)
}

// vmmPID is the process id of this jail's VMM, or zero when there is none.
//
// ZERO IS "NOTHING IS RUNNING" AND AN ERROR IS "BILLET COULD NOT TELL", which the
// caller must not confuse: the first permits teardown to continue and the second
// must stop it, because the next steps unmap a block device the VMM may still hold.
func (p *Provider) vmmPID(j jail) (int, error) {
	raw, err := os.ReadFile(j.pidFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// NO PID FILE IS ONLY "NOTHING IS RUNNING" IF NO VMM EVER STARTED, and
			// the API socket is what distinguishes the two. billet creates the jail
			// before it runs the jailer, so a jail with neither is a launch that
			// failed early and has nothing to stop. A jail with a SOCKET and no pid
			// file is a VMM that got far enough to bind one — reading that as
			// "nothing is running" would take the block device out from under a
			// guest in the middle of a job.
			//
			// Not reachable on the measured version, which writes the file either
			// way; reachable if a future one honours its own documentation, where
			// the file appears only with --new-pid-ns.
			if _, statErr := os.Stat(j.socket()); statErr == nil {
				return 0, fmt.Errorf("firecracker: %s has an api socket and no pid file, so "+
					"billet cannot tell whether its vmm is running", j.dir())
			}

			return 0, nil
		}

		return 0, fmt.Errorf("firecracker: read the pid of the microVM %s: %w", j.id, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("firecracker: %s holds %s, which is not a process id",
			j.pidFile(), bounded(strings.TrimSpace(string(raw))))
	}

	// THE DISCRIMINATOR AGAINST PID REUSE. The jailer execs the VMM with
	// `--id <jail id>`, so the command line is proof that this number is still the
	// process the file was written for.
	owns, err := p.pidOwner(pid, j.id)
	if err != nil {
		return 0, err
	}

	if !owns {
		return 0, nil
	}

	return pid, nil
}

// awaitExit waits for a signalled VMM to actually be gone.
//
// IT WATCHES THE PROCESS, not the API socket. A VMM that is mid-exit can stop
// answering while its file descriptors are still open — and what the next step
// needs is not "it stopped serving" but "it has released the block device", which
// only the process ending establishes.
func (p *Provider) awaitExit(ctx context.Context, j jail, pid int) error {
	ctx, cancel := context.WithTimeout(ctx, exitWait)
	defer cancel()

	ticker := time.NewTicker(bootPoll)
	defer ticker.Stop()

	for {
		// A pid that no longer belongs to this microVM is one that has exited —
		// including the case where the number has already been reused, which is
		// equally proof that the VMM billet signalled is gone.
		owns, err := p.pidOwner(pid, j.id)
		if err != nil {
			return err
		}

		if !owns {
			return nil
		}

		select {
		case <-ctx.Done():
			// REPORTED, NOT SWALLOWED. A VMM that will not stop is holding a mapped
			// device open, so the discard after it is going to fail too — and the
			// caller reads any error here as "the compute may still exist", which is
			// exactly right.
			return fmt.Errorf("firecracker: the microVM %s (pid %d) was signalled and is still "+
				"running: %w", j.id, pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

// exitWait bounds how long a signalled VMM is given to go.
//
// SHORT, because SIGTERM to a firecracker process is not a graceful guest shutdown
// — the VMM exits, taking the guest with it, and measured that is immediate. What
// this is really bounding is the window before billet escalates to SIGKILL.
const exitWait = 5 * time.Second

// unwind removes what a failed launch made.
//
// Its errors are RETURNED to be joined with the launch failure rather than logged
// and dropped: a root disk that could not be discarded is pool space nothing will
// reclaim, and a caller deciding whether to hold a lease in custody needs to know
// the difference between "nothing started" and "something may still be here".
func (p *Provider) unwind(
	ctx context.Context, j jail, spec provider.Spec, res resources, cause error,
) error {
	// A FRESH CONTEXT, because the usual reason a launch failed is that the
	// caller's was cancelled — and asking a cancelled context to clean up
	// guarantees the cleanup fails too.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.bootWait)
	defer cancel()

	var failures []error

	if err := p.stopVMM(ctx, j); err != nil {
		// THE SAME GATE Destroy HAS, and for the same reason: everything below
		// destroys the evidence needed to ever stop this VMM. A launch that failed
		// after starting one is exactly the case where that matters.
		return err
	}

	// NOT A DEVICE THIS LAUNCH DID NOT CREATE. billet allocates the name, so a
	// collision means something outside billet holds it — and removing it would cut
	// the network out from under whatever that is, over a launch failure elsewhere.
	//
	// THE NAME GOES BACK ONLY IF THE DEVICE DID. Same reasoning as Destroy: a device
	// left attached while its name returns to the pool makes every later launch that
	// draws that name fail, permanently, because nothing enumerates orphan devices.
	// The errTapTaken case keeps the name for the opposite reason — the device is not
	// billet's to delete, so the name must stay out of the pool rather than be handed
	// to a launch that would fail on it too.
	tapGone := false

	if !errors.Is(cause, errTapTaken) {
		if err := p.deleteTap(ctx, res.Tap); err != nil {
			failures = append(failures, err)
		} else {
			tapGone = true
		}
	}

	if err := p.removeCgroup(j); err != nil {
		failures = append(failures, err)
	}

	if err := j.remove(); err != nil {
		failures = append(failures, err)
	}

	// AFTER THE JAIL, because releasing a claim while the jail still names it would
	// let another launch take the uid a live VMM is running as — and each release
	// checks the claim still names THIS jail, since the jail is gone by now and a
	// concurrent launch may have legitimately reaped and re-taken it.
	held := res
	if !tapGone {
		held.Tap = ""
	}

	if err := p.releaseResources(held, j.id); err != nil {
		failures = append(failures, err)
	}

	if err := p.disk.DiscardRoot(ctx, spec.Name); err != nil {
		failures = append(failures, fmt.Errorf("firecracker: discard the root disk of %s after a "+
			"failed launch: %w", spec.Name, err))
	}

	return errors.Join(failures...)
}

// Find reports the microVM with that name, and whether there was one.
func (p *Provider) Find(ctx context.Context, name string) (*Instance, bool, error) {
	if _, ours := provider.LeaseOf(name); !ours {
		return nil, false, nil
	}

	j, found, err := p.findJail(name)
	if err != nil {
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	owner, err := ownerOf(j)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A jail with no marker is a launch interrupted between claiming the
			// directory and writing the marker into it. It is billet's, and it may
			// have a mapped root disk behind it.
			running, runErr := p.running(ctx, j)
			if runErr != nil {
				return nil, false, runErr
			}

			return &Instance{ID: name, Name: name, Running: running}, true, nil
		}

		// NOT "NOT FOUND". The caller's next move on a miss is to conclude nothing
		// started, and a marker billet could not read is not evidence of that.
		return nil, false, fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
	}

	if owner != p.owner {
		// Another billet's microVM under a name this one also uses. Not ours to
		// report and emphatically not ours to destroy.
		return nil, false, nil
	}

	running, err := p.running(ctx, j)
	if err != nil {
		return nil, false, err
	}

	return &Instance{ID: name, Name: name, Running: running}, true, nil
}

// List reports every microVM this backend is running for billet.
//
// IT FAILS RATHER THAN REPORTING A SHORTER LIST. The control plane reconciles
// against this and frees the capacity of any lease ABSENT from it, so an entry
// silently dropped is capacity handed back for a guest that is still executing a
// job — the exact failure the inventory exists to prevent, caused by the report
// meant to prevent it. The ec2 backend fails its own List for the same reason.
//
// A chroot base that does not exist yet is EMPTY rather than an error: a node that
// has never launched anything has no such directory, and refusing there would make
// the first sweep on a fresh host a failure.
func (p *Provider) List(ctx context.Context) ([]*Instance, error) {
	dirs, err := p.execDirs()
	if err != nil {
		return nil, err
	}

	var instances []*Instance

	// EVERY DIRECTORY A JAILER HAS BUILT IN, not just this binary's — see execDirs.
	// A version bump behind the stable symlink the reference host uses would
	// otherwise make every guest started before it vanish from the inventory, and a
	// vanished lease is one whose capacity is handed back while its job runs.
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(p.cfg.ChrootBase, dir))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("firecracker: list the microVMs under %s: %w",
				filepath.Join(p.cfg.ChrootBase, dir), err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			name := entry.Name()

			if _, ours := provider.LeaseOf(name); !ours {
				// A jail billet did not name. Nothing else writes into this
				// directory today, but the action this list feeds is destruction,
				// so a name that does not encode a lease is left alone rather than
				// reported.
				continue
			}

			j := jail{base: p.cfg.ChrootBase, execName: dir, id: name}

			owner, err := ownerOf(j)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// A JAIL WITH NO MARKER IS REPORTED, NOT SKIPPED, and it is the
					// case this distinction exists for: claim writes the marker as
					// it creates the directory, so a jail without one is a launch
					// interrupted between the two. It is billet's, it may have a
					// mapped root disk behind it, and skipping it would leave that
					// unreclaimable.
					//
					// ITS LIVENESS IS ASKED FOR RATHER THAN ASSUMED. Hard-coding
					// false reads as "safe to destroy", and a partial teardown can
					// remove a marker while the VMM behind it is still running —
					// which is exactly the guest that must not be destroyed.
					running, runErr := p.running(ctx, j)
					if runErr != nil {
						return nil, runErr
					}

					instances = append(instances, &Instance{
						ID: name, Name: name, Running: running,
					})

					continue
				}

				return nil, fmt.Errorf("firecracker: read which deployment owns %s: %w",
					j.dir(), err)
			}

			if owner != p.owner {
				continue
			}

			running, err := p.running(ctx, j)
			if err != nil {
				return nil, err
			}

			instances = append(instances, &Instance{
				ID: name, Name: name, Running: running,
			})
		}
	}

	return instances, nil
}

// running reports whether a microVM is still executing.
//
// THE ASYMMETRY IS DELIBERATE AND IT IS THE Instance CONTRACT'S. The caller
// destroys what is not running, so "billet could not tell" must answer TRUE: a
// timeout, a permission error or a half-read response would otherwise force-kill a
// job that is running perfectly well. Only an answer that PROVES the VMM is not
// there — a refused connection, a socket that is not there — makes it false.
//
// `Not started` is false, and it is the state that looks alive: a VMM which was
// configured and never started has a socket, a pid and a jail, and will never run
// anything, because whatever would have started it is gone. It is this backend's
// `created` container.
func (p *Provider) running(ctx context.Context, j jail) (bool, error) {
	info, err := p.apiFor(j.socket()).info(ctx)
	if err != nil {
		return !gone(err), nil
	}

	// A DIFFERENT VMM ON THIS SOCKET IS AN ERROR, NOT A "NO".
	//
	// Answering false would say this microVM has stopped, and the caller destroys
	// what has stopped — so billet would tear down a lease's jail and disk on the
	// strength of a socket it has just established belongs to something else. It
	// cannot say the guest is running either. Neither answer is available, which is
	// what an error is for.
	if info.ID != j.id {
		return false, fmt.Errorf("firecracker: the vmm answering for %s calls itself %s, so "+
			"billet cannot say whether %s is running", j.id, bounded(info.ID), j.id)
	}

	return info.State == stateRunning, nil
}

// execRunner runs the jailer or ip, and returns standard output.
//
// STDERR IS FOLDED INTO THE ERROR, never into the result. The jailer's own
// diagnostics go there — `Failed to create /dev/net/tun via mknod inside the jail`
// is the sentence that explains a whole class of failure — and a caller told only
// `exit status 1` has nowhere to go.
func execRunner(ctx context.Context, bin string, args []string) ([]byte, error) {
	// #nosec G204 -- the binary is operator configuration and every argument is
	// built here from typed config, never from job or workflow input. There is no
	// shell: exec.CommandContext passes argv directly, so nothing is interpreted.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(bin), err,
			bounded(lastLine(string(exitErr.Stderr))))
	}

	return nil, fmt.Errorf("%s: %w", filepath.Base(bin), err)
}

// waitDelay bounds how long the pipes may outlive the process.
//
// exec.CommandContext kills the direct child when the deadline passes, but Output
// waits for the pipes to reach EOF — and the jailer's whole job is to leave a
// DAEMONIZED grandchild behind. Without this the call would block until that
// grandchild, which is the microVM, exits.
const waitDelay = 2 * time.Second

// lastLine keeps the line a command ended on.
//
// The jailer prints its usage hint and its error on separate lines and the last
// one carries the reason. `Resource busy` at the end of a cgroup complaint is the
// difference between a fixable configuration and an unreadable exit code.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

// kernelFor resolves which kernel file a generation must boot.
//
// THE GENERATION'S OWN KERNEL WINS OVER THE NODE'S CONFIGURATION, because the two
// are published together and a mismatch fails inside somebody's job rather than at
// launch. A generation that records none falls back, since build-guest-image.sh
// installs no kernel and its generations arrive unpaired.
//
// A RECORDED KERNEL THAT IS NOT ON THIS HOST IS A HARD FAILURE, not a fallback.
// Quietly booting the node's default instead is exactly the mismatch this exists
// to prevent, and it would do it while reporting success.
func (p *Provider) kernelFor(ctx context.Context, image string) (string, error) {
	name, generation, found := strings.Cut(image, "@")
	if !found {
		return "", fmt.Errorf("firecracker: %q names no generation", image)
	}

	recorded, ok, err := p.disk.KernelFor(ctx, name, generation)
	if err != nil {
		return "", err
	}

	if !ok {
		recorded = ""
	}

	kernel, err := kernelForGeneration(recorded, p.cfg.KernelDir, p.cfg.KernelImage)
	if err != nil {
		return "", err
	}

	if recorded != "" {
		if _, err := os.Stat(kernel); err != nil {
			return "", fmt.Errorf("firecracker: %s was verified against kernel %s, which is "+
				"not on this host. Pull the image on this node so its kernel is installed; "+
				"booting the configured kernel instead is the mismatch that fails inside a "+
				"job rather than at launch: %w", image, recorded, err)
		}
	}

	return kernel, nil
}
