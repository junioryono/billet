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
// chroots the VMM, drops it to an unprivileged uid, puts it in a cgroup and
// installs a seccomp filter before the VMM has parsed anything an operator wrote.
// Running firecracker directly would leave a process with the whole host
// filesystem and every syscall available, in front of a guest whose job is running
// somebody's CI.
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
	// CloneRoot makes a per-job copy-on-write clone of a golden image and maps it,
	// returning the host device path.
	CloneRoot(ctx context.Context, image, name string) (string, error)
	// DiscardRoot unmaps and removes a clone. It must be idempotent: teardown runs
	// on paths that have already failed once.
	DiscardRoot(ctx context.Context, name string) error
}

// Provider launches one Firecracker microVM per job.
type Provider struct {
	log   *slog.Logger
	owner string
	cfg   config.FirecrackerConfig
	disk  RootDisk

	// execName is the directory name the jailer will use, resolved once at
	// construction because it cannot change while the process runs and because
	// getting it wrong must fail loudly at startup rather than quietly per launch.
	execName string

	// uid and gid are what the jailer drops the VMM to.
	uid, gid int

	run      runner
	mknod    mknodFunc
	chown    chownFunc
	apiFor   func(socket string) *vmmAPI
	jailUser func(name string) (int, int, error)

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

// withJailUser replaces the account lookup, for a test that has no such account
// and must not need root to run.
func withJailUser(uid, gid int) Option {
	return func(p *Provider) {
		p.jailUser = func(string) (int, int, error) { return uid, gid, nil }
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
	if strings.TrimSpace(owner) == "" {
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
		jailUser: lookupJailUser,
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

	uid, gid, err := p.jailUser(cfg.JailUser)
	if err != nil {
		return nil, err
	}

	p.uid, p.gid = uid, gid

	if p.execName, err = resolveExecName(cfg.BinaryPath); err != nil {
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

	device, err := p.disk.CloneRoot(ctx, spec.Image, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("firecracker: root disk for %s: %w", spec.Name, err)
	}

	// FROM HERE EVERY FAILURE UNWINDS WHAT IT MADE, in reverse order, and says so if
	// it cannot. The caller treats a launch error as "billet does not know whether
	// something started" and asks Find — so this is not the safety net, it is the
	// ordinary case being tidy. What it must never do is replace the reason the
	// launch failed with the reason a cleanup failed.
	inst, err := p.launch(ctx, j, spec, device)
	if err != nil {
		return nil, errors.Join(err, p.unwind(ctx, j, spec))
	}

	p.log.Info("launched a microVM", "runner", spec.Name, "vcpu", spec.VCPU,
		"memory", spec.Memory, "image", spec.Image, "trust", spec.Trust)

	return inst, nil
}

// launch does the work Launch unwinds on failure.
func (p *Provider) launch(
	ctx context.Context, j jail, spec provider.Spec, device string,
) (*Instance, error) {
	if err := p.build(j, device); err != nil {
		return nil, err
	}

	tap := tapName(spec.Name)

	if err := p.addTap(ctx, tap, p.bridgeFor(spec.Trust)); err != nil {
		return nil, err
	}

	if err := p.startVMM(ctx, j); err != nil {
		return nil, err
	}

	api, err := p.awaitAPI(ctx, j)
	if err != nil {
		return nil, err
	}

	if err := p.configure(ctx, api, spec, tap); err != nil {
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
		// V2, WHICH IS A SECURITY PROPERTY RATHER THAN A VERSION. Under V1 any
		// process in the guest can read the metadata with a bare GET, so a workflow
		// step could take the runner registration; V2 requires a PUT to mint a
		// session token first, which is the same reason billet insists on IMDSv2 for
		// the instances the ec2 backend launches.
		{"/mmds/config", map[string]any{
			"version":            "V2",
			"network_interfaces": []string{guestInterface},
			"ipv4_address":       mmdsAddress,
		}},
		{"/mmds", p.metadata(spec)},
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
func (p *Provider) metadata(spec provider.Spec) map[string]any {
	return map[string]any{
		"latest": map[string]any{
			"meta-data": map[string]any{
				"billet": map[string]any{
					"runner-name": spec.Name,
					"jit-config":  spec.JITConfig,
					"command":     spec.Command,
				},
			},
		},
	}
}

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
func (p *Provider) startVMM(ctx context.Context, j jail) error {
	if _, err := p.run(ctx, p.cfg.JailerPath, p.jailerArgs(j)); err != nil {
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
func (p *Provider) jailerArgs(j jail) []string {
	return []string{
		"--id", j.id,
		"--exec-file", p.cfg.BinaryPath,
		"--uid", strconv.Itoa(p.uid),
		"--gid", strconv.Itoa(p.gid),
		"--chroot-base-dir", p.cfg.ChrootBase,
		"--cgroup-version", "2",
		"--cgroup", "cpu.weight=100",
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

	j := p.jailFor(id)

	var failures []error

	if err := p.stopVMM(ctx, j); err != nil {
		failures = append(failures, err)
	}

	if err := p.deleteTap(ctx, tapName(id)); err != nil {
		failures = append(failures, err)
	}

	if err := j.remove(); err != nil {
		failures = append(failures, err)
	}

	// LAST, BECAUSE THE VMM HOLDS IT OPEN. Unmapping a device a running VMM has
	// open fails, so the root disk goes only once the process that was reading it
	// is gone.
	if err := p.disk.DiscardRoot(ctx, id); err != nil {
		failures = append(failures, fmt.Errorf("firecracker: discard the root disk of %s: %w", id, err))
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

	// SIGTERM FIRST, because firecracker exits cleanly on it — measured — and a
	// clean exit closes the guest's disk rather than leaving the mapped device to
	// be torn out from under it.
	if err := signalVMM(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("firecracker: stop the microVM %s: %w", j.id, err)
	}

	if err := p.awaitExit(ctx, j, pid); err == nil {
		return nil
	}

	// AND SIGKILL IF IT WILL NOT, because the alternative is a microVM holding a
	// mapped block device open forever while its capacity stays charged.
	if err := signalVMM(pid, syscall.SIGKILL); err != nil {
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
	owns, err := pidIsVMM(pid, j.id)
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
		owns, err := pidIsVMM(pid, j.id)
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
func (p *Provider) unwind(ctx context.Context, j jail, spec provider.Spec) error {
	// A FRESH CONTEXT, because the usual reason a launch failed is that the
	// caller's was cancelled — and asking a cancelled context to clean up
	// guarantees the cleanup fails too.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.bootWait)
	defer cancel()

	var failures []error

	if err := p.stopVMM(ctx, j); err != nil {
		failures = append(failures, err)
	}

	if err := p.deleteTap(ctx, tapName(spec.Name)); err != nil {
		failures = append(failures, err)
	}

	if err := j.remove(); err != nil {
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

	j := p.jailFor(name)

	owner, err := ownerOf(j)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
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

	return &Instance{ID: name, Name: name, Running: p.running(ctx, j)}, true, nil
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
	dir := filepath.Join(p.cfg.ChrootBase, p.execName)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("firecracker: list the microVMs under %s: %w", dir, err)
	}

	var instances []*Instance

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()

		if _, ours := provider.LeaseOf(name); !ours {
			// A jail billet did not name. Nothing else writes into this directory
			// today, but the action this list feeds is destruction, so a name that
			// does not encode a lease is left alone rather than reported.
			continue
		}

		j := p.jailFor(name)

		owner, err := ownerOf(j)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A JAIL WITH NO MARKER IS REPORTED, NOT SKIPPED, and it is the case
				// this distinction exists for: build writes the marker before anything
				// else, so a jail without one is a launch that was interrupted between
				// the mkdir and the first write. It is billet's, it may have a mapped
				// root disk behind it, and skipping it would leave that unreclaimable.
				instances = append(instances, &Instance{ID: name, Name: name, Running: false})

				continue
			}

			return nil, fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
		}

		if owner != p.owner {
			continue
		}

		instances = append(instances, &Instance{ID: name, Name: name, Running: p.running(ctx, j)})
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
func (p *Provider) running(ctx context.Context, j jail) bool {
	info, err := p.apiFor(j.socket()).info(ctx)
	if err != nil {
		return !gone(err)
	}

	// A DIFFERENT VMM ON THIS SOCKET IS NOT THIS ONE RUNNING. Reporting true would
	// hold a lease open for a guest that belongs to another lease entirely.
	if info.ID != j.id {
		return false
	}

	return info.State == stateRunning
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
