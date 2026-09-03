// Package tart runs macOS and Linux arm64 guests on Apple Silicon through the
// tart CLI (github.com/openai/tart).
//
// The CLI rather than Apple's Virtualization.framework on purpose: the
// framework's Go bindings are cgo, a distributed binary using them must be
// codesigned with the virtualization entitlement, and tart ships the OCI image
// distribution and copy-on-write cloning billet would otherwise rebuild. Tart is
// an external dependency an operator installs, exactly as Docker, Ceph and
// Firecracker are.
//
// Three facts about tart shape this file, each pinned to the tool's source
// rather than its documentation (the firecracker backend's history is why):
//
//   - `tart run` has no daemon mode: the process IS the VM. It is therefore
//     started detached, in its own session with its output in the VM directory,
//     so a billet restart or upgrade does not kill every guest on the host —
//     billet's promise is that running jobs keep running — MEASURED against a
//     real GitHub Actions job on this backend, both ways: a node SIGKILLed
//     mid-job leaves the guest running and the next node adopts that exact VM
//     ("found=1 adopted=1") while the job finishes green, and a node sent
//     SIGTERM drains instead, holding the job for its full grace rather than
//     killing it. Nothing believes that
//     process's exit status; the VM's own state, read back through `tart list`,
//     is what is believed, for the same reason the firecracker backend believes
//     the VMM's API and not the jailer's exit code.
//
//   - The runner registration travels through `tart exec -i` on STDIN, to the
//     guest agent over a local unix socket in the VM directory. Not argv, where
//     every process on the host reads it; not SSH, which would put a guest
//     password in billet's config and the credential on a bridge another guest
//     can ARP-spoof; not the clone, where it would persist. The guest consumes
//     it into process environment and it never touches a disk.
//
//   - Tart has no labels, so ownership is a marker FILE billet writes into the
//     VM directory — and the clone is created under a staging name, the marker
//     written and synced, and only then `tart rename`d to the lease name. A VM
//     carrying a billet lease name therefore ALWAYS carries an ownership
//     marker: List can refuse to guess, and two billet deployments on one Mac
//     cannot adopt or destroy each other's guests. The window a crash can leak
//     is a stopped, unmarked STAGING clone, which never matches a lease and is
//     reported rather than adopted.
//
// UNTRUSTED WORK IS REFUSED. Tart's default NAT lets guests reach the host and,
// via ARP spoofing, each other's traffic; the isolation this backend will
// eventually stand on is a billet-managed softnet policy, and until that exists
// refusing is the only honest answer. This backend also does not attach cache
// volumes yet, and it refuses a spec that asks rather than launching a guest
// that silently has no cache.
package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/provider"
)

// Instance is provider.Instance, aliased so this file does not repeat the
// package name on every line.
type Instance = provider.Instance

// ownerMarker is the file inside a VM directory that carries the DEPLOYMENT
// identity of the billet that created it — not the node name, which defaults to
// the hostname and is shared by every billet installation on one machine.
//
// It is what List filters on, and List feeds a loop that destroys, so a marker
// two installations could both claim is a way for one to destroy the other's
// live jobs. See state.DeploymentID.
const ownerMarker = "billet-owner"

// runLog is where the detached `tart run` process writes its output, inside the
// VM directory so it is removed with the VM and cannot collide across guests.
const runLog = "billet-run.log"

// stagingSuffix marks a clone that has not yet been renamed to its lease name.
// A staging name never equals a lease's instance name, so a crash between clone
// and rename cannot leave an unmarked VM under a name reconciliation acts on.
const stagingSuffix = ".staging"

// jitEnvVar is what the GitHub runner reads its single-use registration from.
const jitEnvVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// launchClaim is the atomic single-winner record for one VM's runner spawn.
//
// `tart exec` can fail after the guest has already acted — the transport is a
// socket, and a lost response is not a lost command — so delivery retries, and
// something has to make a retry unable to start a SECOND runner. An ordinary
// "did we already do this" flag written after the spawn cannot: an attempt that
// detaches and then loses its response leaves no flag, and the retry spawns
// again, so one lease ends up with two runners racing to consume a single-use
// registration.
//
// So the claim is created with the shell's noclobber (`set -C`), which is
// O_EXCL: exactly one delivery attempt in the VM's whole life wins it and may
// spawn. Every later attempt loses, spawns nothing, and instead waits for the
// winner's announcement — and if the winner died without announcing, the
// delivery fails loudly rather than starting a rival.
//
// The name is unchanged from when this was a post-spawn sentinel, deliberately:
// a VM created by an older billet carries that file, so it loses the claim and
// cannot be given a second runner. It has no pid file, so the launch fails
// where it would otherwise silently adopt a VM whose runner cannot be proved.
const launchClaim = ".billet-runner-launched"

// runnerPIDFile holds the pid of the process the guest launcher exec'd into.
//
// Written by the launcher itself rather than captured from `$!` in the
// delivering shell, because `$!` names whatever wrapper did the detaching and
// the pid that matters is the RUNNER's.
const runnerPIDFile = ".billet-runner-pid"

// runnerBirthFile holds that pid's BIRTH TOKEN, and it is what makes the pid
// mean anything.
//
// A pid on its own proves a number is in use, not that it is still this
// runner: guest kernels reuse pids, so `kill -0` on a recycled number reports a
// live runner where there is none. Matching the process's command line instead
// was considered and is wrong — the GitHub runner's `./run.sh` execs
// `Runner.Listener`, so the argv billet launched is not the argv it would find.
// A birth time cannot be reused, because a recycled pid is by definition a
// process that started later.
const runnerBirthFile = ".billet-runner-birth"

// runnerLog is where the guest keeps the runner's own output. It stays in the
// guest: everything in it is written by the runner and by the job, so nothing
// read out of it may reach a billet error or a node log. See launchStatus for
// what billet quotes instead, and why.
const runnerLog = "billet-runner.log"

// launchStatusFile is where billet's own launcher records its verdict, and the
// status* constants are the entire vocabulary it may use.
//
// A GUEST CAN WRITE THIS FILE TOO, which is exactly why the vocabulary is
// closed: a value that is not one of these is reported as unrecognised rather
// than echoed, so the worst a job can do by writing here is make billet print
// one of billet's own sentences. That is the difference between this and
// reading the runner's log, which would let a job choose the bytes.
const (
	launchStatusFile = ".billet-launch-status"
	// statusCommandMissing is the tier command not being resolvable in the
	// guest — the failure that actually happened, when the runner default
	// pointed at a path the published images do not have.
	statusCommandMissing = "command-missing"
	// statusStarted means the launcher got as far as exec. A runner that is
	// dead despite this died in its own code, not in billet's delivery.
	statusStarted = "started"
	// statusLaunching is the launcher past the command check and not yet
	// announced. It exists so a launcher that gives up in between — failing its
	// identity check, say — is distinguishable from a delivery that never ran
	// at all, WITHOUT a second process writing a correction: billet reads the
	// pid in the same query and derives the verdict.
	statusLaunching = "launching"
	// statusStartedNoPID is `started` observed without a pid. It is a SAMPLING
	// artefact the first time — the two files are written in the opposite order
	// to the one they are read in — and a real failure if it persists, so the
	// caller decides rather than this reader.
	statusStartedNoPID = "started, pid pending"
	// statusNeverAnnounced is the delivery waiting out its handshake without
	// the launcher recording a pid. It replaces a message the script used to
	// print to stderr, which billet no longer reads: the registration travels
	// on the same call's STDIN, so a guest that copies stdin to stderr would
	// reflect a live credential into a node log.
	statusNeverAnnounced = "never-announced"
	// statusUnreadable and statusForeign are billet's OWN verdicts about the
	// file rather than values written into it. They are spelled so no guest can
	// produce them: a token with a space is not a token the launcher writes.
	statusUnreadable = "could not read"
	statusForeign    = "not billet's"
	// statusNotAsked is billet declining to look, because the caller's context
	// had already ended. It is NOT the same as an empty status file, and
	// reporting it as one said "the launcher died before its first instruction"
	// about a launcher that had recorded `started`.
	statusNotAsked = "not asked"
)

// launchStatusLimit bounds the status read. A token plus a newline; anything
// longer is not a token billet wrote.
const launchStatusLimit = 64

// commandWaitDelay is how long a cancelled tart command has to let go of its
// output pipes before billet stops waiting for it.
//
// Without one a deadline is advisory: exec.CommandContext kills the process it
// started, and Wait blocks until the pipes close — which anything that process
// spawned keeps open. Short, because by this point the caller has already given
// up and the node's single command slot is what is being held.
const commandWaitDelay = 2 * time.Second

// launchStatusTimeout bounds the one read of it. Deliberately a constant rather
// than a multiple of the retry cadence: how often to re-ask a question is not
// how long a single answer may take.
const launchStatusTimeout = 10 * time.Second

// markerLimit bounds an ownership-marker read. A marker is one deployment id;
// anything larger is not a marker and must not be read into memory whole.
const markerLimit = 4096

// runLogTailLimit bounds how much of a failed VM's run log is quoted into an
// error. Enough for the reason, not enough to paste a boot log into a lease.
const runLogTailLimit = 512

// proveSamples is how many consecutive live answers, a beat apart, make a
// runner proved. One is an instant rather than a state: the pid is announced
// just before the exec, so a single reading can land on a runner that is about
// to exit — the very failure being checked for, a moment early.
const proveSamples = 2

// Provider launches tart VMs through the tart CLI.
type Provider struct {
	log *slog.Logger
	// cfg is the node's tart block: what isolates an untrusted guest, and what
	// that guest resolves through once isolated.
	cfg config.TartConfig
	// tart is the binary to invoke, overridable so tests can substitute a stub.
	tart string
	// softnet is the isolation helper tart supervises for an untrusted guest.
	// A seam so the preflight's ownership rules are assertable without touching
	// a real setuid binary.
	softnet string
	// softnetStat reports a file's mode and owning uid. A seam because the
	// grant's POSITIVE case — setuid AND owned by root — cannot otherwise be
	// asserted without running the suite as root, which would leave the
	// success path tested only by a log line.
	softnetStat func(string) (os.FileMode, uint32, error)
	// owner is the deployment identity written into every VM billet creates.
	owner string
	// home is TART_HOME — where tart keeps vms/<name>/ directories. Resolved
	// once at construction so every read of an ownership marker and every write
	// of a run log agrees with the tart binary about which store they share.
	home string
	// execRetry is how long Launch waits between guest-agent attempts while the
	// guest boots. A seam so tests do not wait wall-clock seconds.
	execRetry time.Duration
	// proveWindow is how long proveRunning keeps asking before it calls a runner
	// dead, and proveRetry is the gap between asks. Both are seams for the same
	// reason execRetry is.
	proveWindow time.Duration
	proveRetry  time.Duration
	// startWindow is how long Launch waits for a detached `tart run` to make the
	// VM actually running before it reads the run log and gives up.
	startWindow time.Duration
	// stopWindow is how long Destroy waits for a stopped VM's state to settle.
	// `tart stop` requests a stop rather than completing one, so a single read
	// catches a VM mid-transition. A seam for the reason the others are.
	stopWindow time.Duration
	// storeLockWindow is how long a name mutation waits for the host-wide store
	// lock, and storeLockRetry is the gap between attempts.
	//
	// CHOSEN AGAINST THE LONGEST THING THE LOCK COVERS, which is a `tart delete`
	// of a macOS VM directory rather than the rename it is usually protecting: a
	// bound shorter than the work underneath it causes the failure it exists to
	// prevent, and here that failure is a teardown refused because a PEER
	// teardown was doing its job. Still far inside the node's ten-minute command
	// timeout, which is what a genuinely stuck holder would otherwise consume.
	// Seams for the reason the others are.
	storeLockWindow time.Duration
	storeLockRetry  time.Duration
	// resolverWaits is how many one-second checks the guest's resolver script
	// makes before calling the configuration failed. A seam for the reason
	// execRetry is one: the failure paths are the interesting ones to test, and
	// each costs this many wall-clock seconds.
	resolverWaits int
	// goos is the host platform CheckHost judges. A seam because the refusal it
	// gates must be assertable from the platform CI actually runs tests on.
	goos string
	// beforeStoreLock runs immediately before CheckHost attempts the store lock,
	// and nothing sets it outside tests.
	//
	// A SEAM BECAUSE THE ALTERNATIVE COULD NOT BE MADE DETERMINISTIC. What has to
	// be exercised is a caller cancelling WHILE the preflight waits on a held
	// lock — lockStore then wraps ctx.Err() and errStoreLockBusy in one error,
	// and CheckHost has to report the cancellation rather than a healthy host.
	// Racing that from a goroutine means guessing when the wait began: keying off
	// the stub's argv log cancels during the `tart list` instead, one run in
	// three, and the test then passes on an error from somewhere else entirely.
	// Measured, not feared — that is how the first version of this behaved.
	beforeStoreLock func()

	// names serializes Launch and Destroy per VM name. Destroy's stop→prove→
	// delete is not atomic, and a Launch admitted into that window could have
	// its fresh VM deleted on the strength of the corpse's proof. The node runs
	// one command at a time, so this costs nothing in the ordinary path; it
	// exists for the paths that are not ordinary.
	namesMu sync.Mutex
	names   map[string]*nameLock
}

// nameLock is one name's mutex plus the count of holders and waiters, which is
// what lets the entry be removed when it reaches zero — lease names are unique
// per job, so a map that only grows is a slow leak measured in jobs.
type nameLock struct {
	mu   sync.Mutex
	refs int
}

// lockName serializes operations on one VM name. The returned func unlocks.
func (p *Provider) lockName(name string) func() {
	p.namesMu.Lock()

	if p.names == nil {
		p.names = make(map[string]*nameLock)
	}

	l, ok := p.names[name]
	if !ok {
		l = &nameLock{}
		p.names[name] = l
	}

	l.refs++
	p.namesMu.Unlock()
	l.mu.Lock()

	return func() {
		l.mu.Unlock()
		p.namesMu.Lock()

		l.refs--
		if l.refs == 0 {
			delete(p.names, name)
		}

		p.namesMu.Unlock()
	}
}

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// WithBinary sets the tart CLI to invoke.
func WithBinary(path string) Option {
	return func(p *Provider) { p.tart = path }
}

// WithConfig sets the node's tart block, which is what decides whether this
// backend will run untrusted work at all.
//
// NORMALIZED HERE, not trusted as given: New is exported, so a caller that
// built the struct in code never passed through config.Load, and a rule applied
// in only one of the two entry points is not applied.
func WithConfig(cfg config.TartConfig) Option {
	return func(p *Provider) {
		cfg.Normalize()

		// CLONED, because a struct copy shares the slice's backing array: a
		// caller that kept the slice it passed could rewrite a resolver AFTER
		// New validated it, and the value that reaches the guest's shell would
		// be one nothing checked.
		cfg.UntrustedDNS = slices.Clone(cfg.UntrustedDNS)

		p.cfg = cfg
	}
}

// WithHome sets TART_HOME. The default honours the TART_HOME environment
// variable and falls back to ~/.tart, which is the tart binary's own default —
// the two processes must agree on the store or ownership markers are written
// beside VMs that live somewhere else.
func WithHome(dir string) Option {
	return func(p *Provider) { p.home = dir }
}

// New builds a tart provider. owner names this billet deployment and is written
// into every VM directory it creates.
func New(owner string, opts ...Option) (*Provider, error) {
	if err := deploymentid.Validate(owner); err != nil {
		return nil, fmt.Errorf("tart: %w", err)
	}

	p := &Provider{
		log:           slog.Default(),
		tart:          "tart",
		softnet:       "softnet",
		softnetStat:   statOwner,
		owner:         owner,
		execRetry:     2 * time.Second,
		proveWindow:   10 * time.Second,
		proveRetry:    time.Second,
		startWindow:   90 * time.Second,
		stopWindow:    30 * time.Second,
		resolverWaits: 10,
		goos:          runtime.GOOS,

		storeLockWindow: 2 * time.Minute,
		storeLockRetry:  25 * time.Millisecond,
	}

	for _, opt := range opts {
		opt(p)
	}

	// RE-APPLIED HERE, exactly as alloc.New re-applies its catalogue's rules.
	// New is exported, so a caller that built a TartConfig in code never passed
	// through config.Load — and these values are not decorative: the resolvers
	// are interpolated into a shell script that runs inside the guest, so
	// "every one of them parses as an IP address" is what makes that
	// interpolation safe rather than a place to quote defensively and hope.
	if errs := config.CheckTart(p.cfg); len(errs) > 0 {
		return nil, fmt.Errorf("tart: %w", errors.Join(errs...))
	}

	if p.home == "" {
		if env := os.Getenv("TART_HOME"); env != "" {
			p.home = env
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("tart: resolve TART_HOME: %w", err)
			}

			p.home = filepath.Join(home, ".tart")
		}
	}

	return p, nil
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderTart }

// Pull fetches an OCI image into tart's local store, writing tart's own
// progress to the given writer.
//
// SEPARATE FROM Launch ON PURPOSE, and that separation is the point of this
// method existing at all. A launch REFUSES an image that is not present rather
// than fetching it, because a macOS image is tens of gigabytes and the node
// executes one command at a time — a pull inside a launch would occupy that
// node's only command slot for as long as the download takes, timing out the
// job that triggered it and every job behind it. So the fetch is an operator
// action, and this is what makes it one billet can perform instead of a command
// the operator has to know.
//
// Progress is streamed rather than captured: this is minutes to hours of
// download, and a silent command that long is indistinguishable from a hung one.
func (p *Provider) Pull(ctx context.Context, ref string, progress io.Writer) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("tart: pull: no image reference given")
	}

	// #nosec G204 -- the binary is operator configuration and the reference comes
	// from this deployment's own tier catalogue. There is no shell.
	cmd := exec.CommandContext(ctx, p.tart, "pull", ref)
	cmd.WaitDelay = commandWaitDelay
	cmd.Stdout = progress
	cmd.Stderr = progress

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tart: pull %s: %w", ref, err)
	}

	// AND THE EXIT CODE IS NOT THE ANSWER, which is this backend's recurring
	// lesson arriving in a new place. tart reclaims space from its own OCI cache
	// to make an operation fit — documented on `tart pull --help` as automatic
	// pruning — so a pull can exit 0 having evicted something else, and a later
	// CLONE can evict what this pull just fetched. Measured on a full disk: this
	// command reported the image pulled, and the first job that wanted it was
	// refused because it was no longer there.
	//
	// Asking again turns that into a failure the operator sees NOW, next to the
	// download they just waited for, rather than as a launch error later.
	if !p.Pulled(ctx, ref) {
		return fmt.Errorf("tart: pulled %s and it is not in the store afterwards; tart "+
			"reclaims its own cache to make room, so this disk is too full to hold it — "+
			"free space, or set TART_NO_AUTO_PRUNE and watch the disk yourself", ref)
	}

	return nil
}

// Pulled reports whether an image is already in tart's local store.
//
// Asked through resolveImage so it answers the question a LAUNCH will ask —
// whether this reference resolves to a digest on this machine — rather than
// whether some row bearing that name exists. The two differ for a moving tag.
func (p *Provider) Pulled(ctx context.Context, ref string) bool {
	_, err := p.resolveImage(ctx, ref)

	return err == nil
}

// Accepts refuses anything that is not established as trusted, and untrusted
// work until an isolation mechanism has been named for it.
//
// A tart VM is a real kernel boundary; the NETWORK is not one. Tart's default
// is shared NAT, where a guest reaches the host and can ARP-spoof the vmnet
// bridge to read another guest's traffic — so untrusted work runs only once
// node.tart.untrusted_isolation describes its confinement, rather than landing
// on the default because nobody said otherwise. Exactly the rule
// node.firecracker.untrusted_bridge states, for the same reason.
//
// UNKNOWN IS REFUSED EVEN WITH ISOLATION CONFIGURED, and that is a different
// judgement: untrusted is a classification billet made, so it can be placed
// under a policy chosen for it, while unknown means billet could not classify
// the job at all — there is no basis for deciding what it may reach.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	switch trust {
	case provider.TrustTrusted:
		return nil

	case provider.TrustUntrusted:
		_, err := p.isolationFlags()

		return err

	case provider.TrustUnknown:
		return errors.New("tart: refusing to run work billet could not classify: an " +
			"unrecognised event establishes no provenance, so there is no basis for choosing " +
			"what the guest may reach")

	default:
		return fmt.Errorf("tart: refusing to run %s work", trust)
	}
}

// isolationOf names what confined a guest, for the launch log. Said out loud
// because "which network did this job run on" is the question nobody can answer
// afterwards, and a guest that quietly took the default NAT looks identical.
func isolationOf(netFlags []string) string {
	if len(netFlags) == 0 {
		return "default NAT (trusted)"
	}

	return strings.Join(netFlags, " ")
}

// isolationFlags maps the configured mechanism to the `tart run` arguments that
// implement it, or says why untrusted work cannot run here.
//
// ONE FUNCTION DECIDES BOTH, deliberately. Written as two — an Accepts that
// admitted any non-empty mechanism and a flag builder that emitted arguments
// only for softnet — a mechanism added to the config and not to the launch path
// would have been admitted and then run with tart's DEFAULT NAT: untrusted work
// on the trusted network, reported as isolated. Whether billet will run the job
// and what confines it are the same question, so they are the same answer.
func (p *Provider) isolationFlags() ([]string, error) {
	switch p.cfg.UntrustedIsolation {
	case config.IsolationSoftnet:
		return []string{"--net-softnet"}, nil

	case "":
		return nil, errors.New("tart: refusing to run untrusted work until it has isolation of " +
			"its own: a VM isolates the kernel but not the network, and tart's default NAT lets " +
			"a guest reach the host and spoof other guests' traffic — set " +
			"node.tart.untrusted_isolation to `softnet`")

	default:
		return nil, fmt.Errorf("tart: refusing to run untrusted work: "+
			"node.tart.untrusted_isolation is %q, which this build does not know how to "+
			"enforce", p.cfg.UntrustedIsolation)
	}
}

// netFlags are the `tart run` networking arguments for a workload of that trust
// class. Trusted work keeps tart's default NAT.
//
// The error is unreachable for a spec Launch admitted — Accepts asks the same
// function first — and is returned rather than dropped because "unreachable"
// stops being true the moment someone calls Launch without it.
func (p *Provider) netFlags(trust provider.TrustClass) ([]string, error) {
	if trust == provider.TrustTrusted {
		return nil, nil
	}

	return p.isolationFlags()
}

// Launch clones the image, marks the clone as billet's, boots it detached, and
// hands the runner its registration through the guest agent.
//
// Idempotent by lease identity: a VM already carrying this spec's name and this
// deployment's marker is returned rather than duplicated, because an error from
// an earlier Launch does not prove nothing started and retrying blindly is how
// one job becomes two runners.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*Instance, error) {
	if err := checkSpec(spec); err != nil {
		return nil, err
	}

	// Checked again here, not only via Accepts: a backend that only refuses when
	// asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	unlock := p.lockName(spec.Name)
	defer unlock()

	if existing, ok, err := p.Find(ctx, spec.Name); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	} else if ok {
		if existing.Running {
			// The earlier attempt proved the VM boots; it did NOT prove the
			// registration arrived — it can have failed between `tart run` and the
			// guest agent accepting the credential, and adopting without asking
			// records a successful launch for a runner that never registers.
			// Redelivery is idempotent: the in-guest sentinel makes a second
			// delivery a no-op when the first one landed.
			if err := p.deliverRegistration(ctx, spec); err != nil {
				return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
			}

			p.log.Info("adopting an instance this lease already launched", "runner", spec.Name)

			return existing, nil
		}

		// A stopped VM under this lease's name is an earlier attempt that got
		// partway: the registration may or may not have been consumed, and
		// booting the corpse would hand a possibly-spent credential to a guest
		// billet believes is fresh. The caller's custody path owns this.
		return nil, fmt.Errorf("tart: launch %s: a stopped VM already carries this lease's name; "+
			"destroy it before relaunching", spec.Name)
	}

	image, err := p.resolveImage(ctx, spec.Image)
	if err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	staging := spec.Name + stagingSuffix
	if err := p.prepareStaging(ctx, staging); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	if _, err := p.run(ctx, "clone", image, staging); err != nil {
		return nil, fmt.Errorf("tart: clone %s for %s: %w", image, spec.Name, err)
	}

	// THE MARKER IS WRITTEN AND SYNCED BEFORE THE VM CAN CARRY A LEASE NAME.
	// After the rename below, a billet-named VM with no readable marker is not a
	// crash artifact — it is evidence of interference, and List refuses to guess
	// about it.
	if err := p.writeOwner(staging); err != nil {
		p.discardStaging(ctx, staging)

		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	if err := p.applyShape(ctx, staging, spec); err != nil {
		p.discardStaging(ctx, staging)

		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	// THE ONE MOMENT A LEASE NAME COMES INTO EXISTENCE, and it happens under the
	// host-wide store lock — the other half of the pair Destroy's delete is. See
	// storelock.go.
	if err := p.publishStaging(ctx, staging, spec.Name); err != nil {
		p.discardStaging(ctx, staging)

		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	// From here a failure LEAVES THE VM IN PLACE. It carries the lease name and
	// the marker, so Find answers for it and the caller's custody machinery owns
	// its teardown — destroying it here would race the very reconciliation that
	// makes an ambiguous launch answerable.
	// Resolved AFTER the rename rather than at admission, because these flags
	// are what confines the guest and the confinement must be decided by the
	// same code path that boots it. Accepts has already asked the same
	// question, so an error here is a caller that skipped it.
	netFlags, err := p.netFlags(spec.Trust)
	if err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	if err := p.startDetached(spec.Name, netFlags...); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	if err := p.proveStarted(ctx, spec.Name); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	// BEFORE THE REGISTRATION, because the registration is single-use: a runner
	// that starts and then cannot resolve github.com has consumed it, and the
	// retry that would fix the resolver has nothing left to register with.
	if err := p.configureResolver(ctx, spec); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	if err := p.deliverRegistration(ctx, spec); err != nil {
		return nil, fmt.Errorf("tart: launch %s: %w", spec.Name, err)
	}

	p.log.Info("launched VM", "runner", spec.Name, "image", image,
		"isolation", isolationOf(netFlags))

	return &Instance{ID: spec.Name, Name: spec.Name, Running: true}, nil
}

// resolveImage pins a moving reference to the exact digest this launch boots.
//
// `tart fqn` answers from the LOCAL OCI cache: a pulled tag resolves to the
// digest actually on this machine — which is the right semantics, because a
// registry re-tag mid-fleet must not change what a lease already placed here
// boots — and a local VM name echoes back unchanged. The clone below is then
// made from the pinned reference, so what launched is recorded rather than
// whatever the tag means by the time someone reads the log.
//
// AN UNPULLED REMOTE REFERENCE IS REFUSED, not pulled. A macOS image is tens of
// gigabytes, the node executes one command at a time, and a pull inside a
// launch starts every queued command's ten-minute clock against a download that
// can outlast all of them — the EC2 serial-queue lesson. Pre-pulling is host
// preparation, and the refusal says the exact command.
func (p *Provider) resolveImage(ctx context.Context, image string) (string, error) {
	out, err := p.run(ctx, "fqn", image)
	if err != nil {
		// Pinned phrasing, matched WHOLE: VMStorageOCI answers "<ref> is not a
		// digest and doesn't point to a digest" for a reference its cache does not
		// hold, and matching a fragment would let an unrelated error that happens
		// to echo those words send the operator to pull an image that is not the
		// problem.
		if strings.Contains(err.Error(), "is not a digest and doesn't point to a digest") {
			// AND IT MAY WELL HAVE BEEN PULLED. tart reclaims storage from its own
			// OCI cache whenever an operation needs room — a pull, and also a
			// CLONE — so on a full disk an image billet fetched an hour ago is
			// deleted by the next guest that starts. Measured here: `billet images
			// pull` succeeded, and the image was gone by the time a job wanted it.
			// Saying only "pre-pull it" sends an operator to repeat the thing they
			// already did.
			return "", fmt.Errorf("image %s is not pulled on this host; fetch it with "+
				"`billet images pull` (billet does not pull inside a launch, because a pull "+
				"of a macOS image starves every queued command's timeout). If it WAS pulled, "+
				"tart reclaims its own cache when the disk is tight — including for a clone — "+
				"so check free space and consider TART_NO_AUTO_PRUNE", image)
		}

		return "", fmt.Errorf("resolve image %s: %w", image, err)
	}

	resolved := strings.TrimSpace(out)

	// FAIL CLOSED ON AN UNPINNED ANSWER. Accepting whatever fqn prints makes the
	// pin decorative: a tart release or a wrapper that echoes the tag back would
	// silently clone the moving reference. What is accepted is exactly two
	// shapes — a digest-qualified reference, or a local VM name echoed back
	// unchanged. A remote-shaped input (it names a registry path) that comes
	// back without a digest is refused, because that is the one answer that
	// LOOKS resolved and is not.
	switch {
	case resolved == "":
		return "", fmt.Errorf("resolve image %s: tart fqn answered nothing", image)
	case strings.Contains(resolved, "@sha256:"):
		return resolved, nil
	case resolved == image && !strings.Contains(image, "/"):
		// A local VM name passes through unchanged; a local name cannot contain a
		// path separator, so a slash means the reference was remote-shaped.
		return resolved, nil
	default:
		return "", fmt.Errorf("resolve image %s: tart fqn answered %q, which is neither a "+
			"digest-qualified reference nor the local name echoed back; refusing to clone a "+
			"reference that could move", image, resolved)
	}
}

// checkSpec refuses what this backend cannot honour, loudly, before anything is
// created. Every branch here is a spec that would otherwise produce a guest
// that LOOKS launched and silently is not what was asked for.
func checkSpec(spec provider.Spec) error {
	// THE STAGING SUFFIX IS RESERVED, because List treats it as proof of
	// history: a directory still carrying it was never renamed, so it was never
	// launched, so an unreadable one is a corpse rather than a guest whose
	// config got damaged. That inference is only sound if nothing can be
	// LAUNCHED under such a name. billet's own names are "billet-" + a hex
	// lease id and never end this way, but the constructor is exported and this
	// is the one place that can make the invariant true rather than customary.
	if strings.HasSuffix(spec.Name, stagingSuffix) {
		return fmt.Errorf("tart: %q ends in %q, which billet reserves for clones that have "+
			"not been marked yet; List reads that name as proof a VM was never launched, so "+
			"one launched under it would be classified as a corpse and have its capacity "+
			"freed while it ran", spec.Name, stagingSuffix)
	}

	if spec.Name == "" {
		return errors.New("tart: a spec needs a name")
	}

	if _, ok := provider.LeaseOf(spec.Name); !ok {
		return fmt.Errorf("tart: %s is not a billet instance name, so nothing could ever "+
			"reconcile it back to a lease", spec.Name)
	}

	if spec.Image == "" {
		return fmt.Errorf("tart: %s has no image", spec.Name)
	}

	// The Ceph positional-spec rule, one backend over: billet hands the image to
	// tart as a POSITIONAL argument, and tart's parser reads a leading dash as a
	// flag — so `--net-softnet` as an image is not an image, it is an option
	// injection with a working-looking config.
	if strings.HasPrefix(spec.Image, "-") {
		return fmt.Errorf("tart: %s image %q begins with %q, which tart reads as a flag "+
			"rather than an image name", spec.Name, spec.Image, "-")
	}

	if spec.JITConfig == "" {
		return fmt.Errorf("tart: %s has no JIT config, so nothing would register", spec.Name)
	}

	// REFUSED, not defaulted, for the docker backend's reason: a wrong guess
	// starts a guest that exits at once while billet logs a started runner.
	if len(spec.Command) == 0 {
		return fmt.Errorf("tart: %s has no command, so no runner would start in the guest", spec.Name)
	}

	// A cache the backend cannot attach must fail the launch, not vanish. A
	// deployment that believes it has a cache and does not is the exact failure
	// the storage rules in CLAUDE.md exist to prevent.
	if len(spec.Volumes) > 0 || spec.CacheEndpoint != "" || spec.CacheToken != "" {
		return fmt.Errorf("tart: %s asks for cache volumes, which this backend does not "+
			"attach yet; launching without them would be a guest that silently has "+
			"no cache", spec.Name)
	}

	if spec.ActionsProxy != "" || spec.ActionsCAPEM != "" {
		return fmt.Errorf("tart: %s asks for the actions results proxy, which this backend "+
			"does not wire into a guest yet", spec.Name)
	}

	if spec.InstanceType != "" {
		return fmt.Errorf("tart: %s names EC2 instance type %q, which a host-backed "+
			"provider cannot honour", spec.Name, spec.InstanceType)
	}

	if spec.SHM > 0 {
		return fmt.Errorf("tart: %s sizes /dev/shm, which tart cannot configure from the "+
			"host; a tier for this backend must not set shm", spec.Name)
	}

	return nil
}

// prepareStaging clears a leftover staging clone from an earlier crashed
// attempt, and refuses one it cannot prove is billet's.
//
// NOT UNDER THE STORE LOCK, and the first version of it was — for uniformity,
// on the rule that every name mutation should take the lock. That rule was the
// wrong shape and it cost something: the lock exists to make a check and an act
// atomic for a name ANOTHER BILLET COULD PUT A VM UNDER, and no other billet can
// put one under this name. It carries a lease id unique to one control plane,
// and checkSpec RESERVES the staging suffix so nothing can be launched under it
// at all. So the lock bought nothing here and made cleanup wait out a window it
// could lose — see discardStaging, where losing it leaks tens of gigabytes.
func (p *Provider) prepareStaging(ctx context.Context, staging string) error {
	dir := p.vmDir(staging)

	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect staging %s: %w", dir, err)
	}

	owner, err := p.ownerOf(staging)
	if err != nil {
		// An unreadable or missing marker is never authority to delete. A staging
		// clone with no marker is most likely this deployment's own crash between
		// clone and marker — but "most likely" does not destroy compute.
		return fmt.Errorf("a staging clone already exists at %s and its ownership cannot be "+
			"proved (%w); remove it by hand with `tart delete %s` if it is this deployment's",
			dir, err, staging)
	}

	if owner != p.owner {
		return fmt.Errorf("a staging clone at %s belongs to another billet deployment; "+
			"refusing to touch it", dir)
	}

	// Ours, never renamed, therefore never launched: a stopped clone with no
	// lease attached. Deleting it is cleanup, not teardown.
	if _, err := p.run(ctx, "delete", staging); err != nil && !isMissingVM(err) {
		return fmt.Errorf("remove leftover staging clone %s: %w", staging, err)
	}

	return nil
}

// applyShape sets the clone's CPU, memory and disk to the spec's.
func (p *Provider) applyShape(ctx context.Context, name string, spec provider.Spec) error {
	args := []string{"set", name}

	if spec.VCPU > 0 {
		args = append(args, "--cpu", strconv.Itoa(spec.VCPU))
	}

	if spec.Memory > 0 {
		// tart takes megabytes. Integer division is deliberate and safe: a tier
		// memory below 1MiB is not expressible in config.
		args = append(args, "--memory", strconv.FormatInt(int64(spec.Memory)/(1<<20), 10))
	}

	if spec.Disk > 0 {
		// tart takes gigabytes and can only grow a disk. Round UP: a disk even one
		// byte short of what the tier promised is a broken promise, while one
		// rounded up wastes at most a gigabyte of thin-provisioned space.
		gb := (int64(spec.Disk) + (1 << 30) - 1) / (1 << 30)
		args = append(args, "--disk-size", strconv.FormatInt(gb, 10))
	}

	if len(args) == 2 {
		return nil
	}

	if _, err := p.run(ctx, args...); err != nil {
		return fmt.Errorf("set shape: %w", err)
	}

	return nil
}

// startDetached boots the VM under a `tart run` that survives billet.
//
// NOT CommandContext, NOT waited on, and in its own session: the tart run
// process is the VM, and tying its lifetime to this process's context — or this
// process — makes every billet restart and upgrade kill every guest on the
// host. Its output goes to a file in the VM directory, because a dropped stderr
// is how "the VM never booted" becomes undiagnosable.
func (p *Provider) startDetached(name string, netFlags ...string) error {
	logPath := filepath.Join(p.vmDir(name), runLog)

	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}

	defer func() {
		// The parent's descriptor only; the child holds its own.
		if closeErr := out.Close(); closeErr != nil {
			p.log.Error("could not close the run log descriptor", "path", logPath, "error", closeErr)
		}
	}()

	args := append([]string{"run", name, "--no-graphics"}, netFlags...)

	// #nosec G204 -- the binary is operator configuration, the name was validated
	// as billet's own instance-name shape, and the network flags are literals
	// this package chose from the configured mechanism. There is no shell.
	//
	//nolint:noctx // deliberate: this process IS the VM, and a context would tie
	// every guest's lifetime to one billet invocation — the exact coupling the
	// detachment above exists to remove.
	cmd := exec.Command(p.tart, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tart run: %w", err)
	}

	// Reaped, never believed: Setsid detaches the session, not the parenthood,
	// so an exited child stays a zombie until someone waits on it — one per
	// finished VM, for as long as this node runs. The Wait exists solely to
	// reap; its result is logged and nothing more, because the exit status of
	// this process proves nothing about the VM (the jailer lesson) and List
	// reads tart's own state instead.
	go func() {
		if err := cmd.Wait(); err != nil {
			p.log.Debug("the detached tart run process exited with an error; the VM's "+
				"state is read from tart, not from this", "runner", name, "error", err)
		}
	}()

	return nil
}

// resolverScript configures the guest's resolvers and proves the result, or
// fails saying which step could not be done.
//
// BILLET OWNS THIS BECAUSE BILLET BROKE IT. Under softnet a guest's
// DHCP-assigned resolver is the vmnet gateway, which sits in the private
// address space softnet blocks — measured: egress to public addresses keeps
// working and so does TCP/443, so nothing looks wrong and every job simply
// fails to resolve github.com. Leaving that to the guest image would make an
// isolated tier's correctness depend on an image billet does not build.
//
// THE PROOF IS THE POINT, not the configuration. Three mechanisms are attempted
// because guests differ (systemd-resolved, a plain resolv.conf, macOS's
// SystemConfiguration), and any of them can stop working when an image changes.
// What follows is a resolution the guest actually performs, so a mechanism that
// silently stops applying becomes a launch error rather than a job that dies
// cloning.
//
// macOS is the reason resolv.conf is not simply written everywhere: there it is
// generated by configd and the system resolver reads SystemConfiguration
// instead, so the file can be perfectly correct and consulted by nothing.
func resolverScript(resolvers []string, waits int) string {
	// EVERY RESOLVER IS SHELL-QUOTED, ONCE, HERE — and then referred to only as
	// positional parameters below.
	//
	// The first version interpolated them as a bare space-separated list and
	// argued that validation made that safe, because each one had parsed as an
	// IP address. That argument was FALSE: `netip.ParseAddr` accepts an IPv6
	// zone and puts no restriction on it, so
	// `2001:4860:4860::8888%x;touch${IFS}/tmp/pwn` parses, and reached a shell
	// running as root in the guest. Zones are refused now, but the refusal is
	// the second line of defence rather than the first — quoting holds whatever
	// the value turns out to be, including the next thing a parser tolerates.
	quoted := make([]string, 0, len(resolvers))
	for _, r := range resolvers {
		quoted = append(quoted, shellQuote(r))
	}

	return `set -u
set -- ` + strings.Join(quoted, " ") + `
resolves() {
  if command -v getent >/dev/null 2>&1; then
    getent hosts github.com >/dev/null 2>&1
  elif command -v dscacheutil >/dev/null 2>&1; then
    dscacheutil -q host -a name github.com 2>/dev/null | grep -q ip_address
  else
    return 1
  fi
}

# ALREADY WORKING IS A VALID ANSWER. An image that ships its own public
# resolver needs nothing, and changing a working configuration is a way to
# break one.
if resolves; then echo ok; exit 0; fi

if command -v networksetup >/dev/null 2>&1; then
  # Every service, because which one the guest's single NIC is called varies by
  # image; the leading "*" marks a disabled service and is not part of the name.
  networksetup -listallnetworkservices 2>/dev/null | tail -n +2 | sed 's/^\*//' |
    while IFS= read -r svc; do
      [ -n "$svc" ] || continue
      sudo -n networksetup -setdnsservers "$svc" "$@" >/dev/null 2>&1 || true
    done
elif command -v resolvectl >/dev/null 2>&1 && sudo -n test -d /etc/systemd; then
  sudo -n mkdir -p /etc/systemd/resolved.conf.d 2>/dev/null || true
  # "$*" rather than an interpolated list: the addresses reach printf as ONE
  # argument it substitutes, never as part of its format string.
  printf '[Resolve]\nDNS=%s\nFallbackDNS=\n' "$*" |
    sudo -n tee /etc/systemd/resolved.conf.d/billet.conf >/dev/null 2>&1 || true
  sudo -n systemctl restart systemd-resolved >/dev/null 2>&1 || true
else
  # A resolv.conf that is a symlink into a resolved stub must be replaced, not
  # written through, or the stub rewrites it back.
  sudo -n rm -f /etc/resolv.conf >/dev/null 2>&1 || true
  for ns in "$@"; do
    printf 'nameserver %s\n' "$ns" | sudo -n tee -a /etc/resolv.conf >/dev/null 2>&1 || true
  done
fi

# The caches these mechanisms feed are not always cleared by the change.
sudo -n dscacheutil -flushcache >/dev/null 2>&1 || true

n=0
while [ "$n" -lt ` + strconv.Itoa(waits) + ` ]; do
  if resolves; then echo ok; exit 0; fi
  sleep 1
  n=$((n + 1))
done

echo failed
exit 1`
}

// configureResolver gives an isolated guest a resolver it can actually reach.
// A guest that is not isolated keeps the DHCP one, which works.
func (p *Provider) configureResolver(ctx context.Context, spec provider.Spec) error {
	if spec.Trust == provider.TrustTrusted || len(p.cfg.UntrustedDNS) == 0 {
		return nil
	}

	out, err := p.run(ctx, "exec", spec.Name, "/bin/sh", "-c",
		resolverScript(p.cfg.UntrustedDNS, p.resolverWaits))
	if err != nil {
		return fmt.Errorf("configure the isolated guest's resolver: %w", err)
	}

	if !strings.Contains(out, "ok") {
		return fmt.Errorf("the isolated guest cannot resolve github.com through %s: softnet "+
			"blocks the DHCP-assigned resolver because it is a private address, and billet "+
			"could not put a reachable one in its place — every job in this guest would fail "+
			"to clone (node.tart.untrusted_dns)", strings.Join(p.cfg.UntrustedDNS, ", "))
	}

	return nil
}

// deliverRegistration hands the runner its JIT config through the guest agent
// and starts the tier's command, retrying while the guest boots.
//
// The credential goes over STDIN on a local unix socket. The script that
// receives it is argv — visible in ps on the host — so the script carries no
// secret: it reads the registration from its own stdin into a shell variable,
// exports it to the runner's environment, and backgrounds the runner under
// nohup, in a subshell so it is orphaned away from the agent session, whose end
// must not take a six-hour job with it.
//
// The sentinel is what makes redelivery safe, and WHERE it is written is a
// decided trade. Checked before spawning, it makes a retry a no-op once any
// earlier delivery got that far; written AFTER the spawn statement, a crash in
// between can let a retry spawn twice. Twice is the bounded direction: the
// registration is single-use, so the losing runner fails to register and
// exits. The other order — sentinel before spawn — bounds nothing: a crash in
// between leaves a sentinel with no runner behind it, every retry then exits
// successfully doing nothing, and billet reports a launched runner for a job
// that sits queued until GitHub gives up. That is the docker default-command
// failure class, and it is the one this backend must not reproduce.
//
// THE DETACHMENT IS A NEW SESSION, AND THAT WAS MEASURED RATHER THAN CHOSEN.
// The first version used `nohup` inside a subshell, which reads like ample
// orphaning and is not: against a real guest the agent tears down its exec
// session's process group when the call returns, and the runner was gone three
// seconds later while `tart exec` had exited 0 and `tart list` still reported
// the VM running. Measured on ghcr.io/cirruslabs/ubuntu, nohup — bare or in a
// subshell — a double fork, and `systemd-run --user` (there is no user bus
// under the agent) all die; only a genuine `setsid` survives. A macOS guest has
// no `setsid` binary, so the fallback is perl's POSIX::setsid, which macOS
// still ships.
//
// AND THE DELIVERY'S EXIT CODE IS NOT BELIEVED, which is the durable half.
// Every mechanism above reported success while starting nothing, so a launch is
// not finished until the guest has been asked whether the runner is alive — see
// proveRunning. A future agent change, or a macOS that finally drops perl, then
// produces a LAUNCH FAILURE the caller already handles instead of a job queued
// until GitHub gives up. It is the rule the firecracker backend learned from
// `jailer --daemonize` exiting 0 for a VM that had died on startup.
func (p *Provider) deliverRegistration(ctx context.Context, spec provider.Spec) error {
	// The launcher announces WHICH process it is — pid plus birth token — and
	// then BECOMES the runner, so what proveRunning checks is the runner itself
	// rather than a wrapper, and a recycled pid cannot impersonate it. Both
	// files are written before the exec, and the pid file is written LAST so
	// its presence implies the birth token is already there.
	// AN IDENTITY OR NOTHING: the launcher refuses to announce a pid it cannot
	// pair with a birth token, because a pid alone is a number a later process
	// can inherit and the proof would then accept a stranger.
	// THE ONE THING BILLET MAY QUOTE OUT OF A GUEST. Everything the runner
	// writes is guest-controlled — see launchStatus — so the launcher records
	// billet's OWN verdict on the two failures it can see before any job code
	// runs, and does it BEFORE announcing a pid so a pre-flight refusal is
	// distinguishable from a runner that started and died.
	// EVERY RECORD CARRIES THE VM IT IS ABOUT. The status path is fixed and the
	// guest's home is cloned from an image, so a stale record can arrive with
	// the image — and a delivery attempt that fails before the script even runs
	// would then read someone else's `command-missing` and abandon a launch
	// that was fine. A lease name is unique, so a record naming a different one
	// is discarded rather than believed.
	status := "s() { printf '%s %s\\n' \"$1\" " + shellQuote(spec.Name) +
		" > \"$HOME/" + launchStatusFile + "\"; }; " +
		"command -v " + shellQuote(spec.Command[0]) + " >/dev/null 2>&1 || " +
		"{ s " + statusCommandMissing + "; exit 1; }; " +
		"s " + statusLaunching + "; "

	launcher := birthFunc + status +
		"b=$(billet_birth \"$$\" || true); " +
		"[ -n \"$b\" ] || exit 1; " +
		"printf '%s\\n' \"$b\" > \"$HOME/" + runnerBirthFile + "\"; " +
		// The pid is written LAST, so its presence implies the token is already
		// there and the waiter below cannot see half an announcement.
		// THE STATUS IS WRITTEN BEFORE THE PID, and the pid is the LAST thing
		// this launcher does before exec. That ordering is the invariant: the
		// pid is what proveRunning samples, so publishing it while any fallible
		// write remains means a shell that then BLOCKS — on a pre-existing FIFO
		// at the status path, say — is sampled twice, matches its own birth
		// token, and reports a healthy runner for a tier command that was never
		// exec'd.
		"s " + statusStarted + "; " +
		"printf '%s\\n' \"$$\" > \"$HOME/" + runnerPIDFile + "\"; " +
		"exec " + shellJoin(spec.Command)
	redirect := " > \"$HOME/" + runnerLog + "\" 2>&1 &\n"

	// The credential rides as a COMMAND PREFIX rather than an exported variable,
	// so it enters the spawned runner's environment and never the delivering
	// shell's — a losing attempt waits fifteen seconds, and it must not hold a
	// live registration in its environment while it does.
	prefix := jitEnvVar + "=\"$jit\" "

	script := "set -eu\n" +
		"cd \"$HOME\"\n" +
		"IFS= read -r jit\n" +
		// THE ATOMIC CLAIM. `set -C` makes this O_EXCL, so exactly one attempt in
		// the VM's life may spawn and a retry can never start a rival runner.
		// Nothing is unlinked here: removing a pid file would be removing a live
		// attempt's announcement.
		"if (set -C; : > \"$HOME/" + launchClaim + "\") 2>/dev/null; then\n" +
		// AND EVEN THE WINNER DOES NOT SPAWN BESIDE AN EXISTING ANNOUNCEMENT.
		// A billet older than the claim wrote its pid BEFORE its post-spawn
		// sentinel, so an interrupted delivery from that version leaves a live
		// runner and no claim — and winning a fresh claim over it would be the
		// second runner this whole mechanism exists to prevent. Leaving it alone
		// makes the proof below judge that runner, which for a legacy one has no
		// birth token and is therefore unprovable: the launch fails and custody
		// destroys the VM, rather than two runners racing one registration.
		"  if [ ! -s \"$HOME/" + runnerPIDFile + "\" ]; then\n" +
		"    if command -v setsid >/dev/null 2>&1; then\n" +
		"      " + prefix + "setsid /bin/sh -c " + shellQuote(launcher) + redirect +
		"    else\n" +
		"      " + prefix + "perl -e 'use POSIX qw(setsid); setsid(); exec @ARGV or die $!' " +
		"/bin/sh -c " + shellQuote(launcher) + redirect +
		"    fi\n" +
		"  fi\n" +
		"fi\n" +
		// Whether this attempt won or lost, it has no further use for the
		// credential.
		"unset jit\n" +
		// THE HANDSHAKE, AND IT IS THE WHOLE FIX for the measured race. Spawning
		// and returning immediately loses it: the delivering shell exits, the
		// agent tears its session down, and the child is killed in the window
		// BEFORE it has called setsid() — so it never escapes and never
		// announces. Waiting for the announcement proves setsid() has already
		// returned, which is what puts the runner in a session the teardown
		// cannot reach. A LOSER of the claim waits here too, for the winner.
		//
		// Whole seconds because `sleep 0.1` is a GNU/BSD extension: on a guest
		// whose sleep rejects it, `set -e` would kill this script after the spawn
		// and before the claim could help. The first check happens before any
		// sleep, so the ordinary case still costs nothing.
		"n=0\n" +
		"while [ ! -s \"$HOME/" + runnerPIDFile + "\" ] && [ \"$n\" -lt 15 ]; do\n" +
		"  sleep 1\n" +
		"  n=$((n + 1))\n" +
		"done\n" +
		// RECORDED, NOT PRINTED. The verdict goes to the status file, whose
		// vocabulary billet owns, because stderr from this call is discarded —
		// see execStdin for why.
		// THE HANDSHAKE WRITES NOTHING, and that is deliberate. Correcting the
		// launcher's `started` from here made this a SECOND writer of one file,
		// and the interleaving is unavoidable: the handshake finds no pid, the
		// launcher then writes `started` and its pid, and the handshake
		// overwrites a verdict that had just become true. Checking again before
		// writing only moves the race one instruction.
		//
		// So there is one writer — the launcher — and billet DERIVES the rest,
		// because it reads the pid and the status in the same query. A
		// `launching` with no pid is conclusive (that marker precedes the
		// identity check by a long way); a `started` with no pid is transient
		// once and conclusive if it repeats, because the two files are written
		// in the opposite order to the one they are read in. See
		// launchStatusToken and the loop in deliverRegistration.
		"[ -s \"$HOME/" + runnerPIDFile + "\" ] || exit 1\n"

	retry := time.NewTimer(p.execRetry)
	defer retry.Stop()

	// The last attempt that COMPLETED BEFORE CANCELLATION WAS OBSERVED. Once the
	// deadline lands inside an attempt, that attempt fails because of it, and
	// reporting the clock beside the wrapper's identical clock says only that
	// time ran out — what the guest said is the diagnosis. The discriminator is
	// when the attempt returned rather than what caused it, so a substantive
	// failure landing in the same instant as the deadline is discarded with the
	// rest; the older diagnosis it keeps is the better of the two answers
	// available without reporting both.
	var lastErr error

	// AND THE GUEST'S OWN VERDICT IS KEPT AS IT IS SEEN, not fetched at the end.
	// By the time this loop exits the caller's context is done, so asking then
	// means either declining — and reporting "no status recorded" about a
	// launcher that recorded one — or spending more of a deadline the caller
	// has already given up on. The read below happens once per failed attempt
	// anyway, so remembering it is free.
	lastStatus := ""
	// Whether any query REACHED the guest, and whether any came back READABLE.
	// Three different things get reported here and conflating them says the
	// launcher recorded nothing when billet never asked, or when it asked and
	// could not read the answer.
	asked, readable := false, false

	for {
		err := p.execStdin(ctx, spec.Name, spec.JITConfig+"\n", "/bin/sh", "-c", script)
		if err == nil {
			return p.proveRunning(ctx, spec.Name)
		}

		// ASKED OF THE CONTEXT, not of the error: measured, a cut-short attempt
		// does not reliably wrap the context's error — a killed child comes back
		// as "signal: killed" and a cancelled wait as "context deadline
		// exceeded", depending on which lost the race.
		if lastErr == nil || ctx.Err() == nil {
			lastErr = err
		}

		// EXCEPT THE ONE FAILURE RETRYING CANNOT FIX. A tier command the guest
		// does not have never announces a pid, so every attempt fails
		// identically until the node's command timeout — ten minutes of that
		// node's single command slot, per job, for a typo. It is also the
		// failure that actually happened on this branch, so it is the one worth
		// telling apart. Any other status keeps retrying: the ordinary case is
		// the agent not yet listening while the guest boots, which looks the
		// same from outside and does resolve on its own.
		switch token := p.launchStatusToken(ctx, spec.Name); token {
		case statusCommandMissing:
			return fmt.Errorf("deliver runner registration: %s",
				launchStatusText(statusCommandMissing))

		case statusNotAsked:
			// Cancelled before the question was put; says nothing either way.

		case statusUnreadable:
			asked = true

		case statusStartedNoPID:
			// SEEN TWICE IS NOT A RACE. The first observation can fall in the
			// microseconds between the launcher's status write and its pid
			// write; a second one means the pid is not coming.
			asked, readable = true, true

			if lastStatus == statusStartedNoPID {
				lastStatus = statusNeverAnnounced
			} else {
				lastStatus = statusStartedNoPID
			}

		default:
			asked, readable = true, true
			lastStatus = token
		}

		// Everything else retries until the caller's deadline. The usual one is
		// the agent not listening yet while macOS boots; a genuine script failure
		// retries too, harmlessly, because the claim makes a spawn happen at most
		// once. The deadline is the node's command timeout, which is the bound
		// chosen against the longest legitimate boot.
		retry.Reset(p.execRetry)

		select {
		case <-ctx.Done():
			// THE GUEST'S VERDICT, IN BILLET'S WORDS. The attempt errors say
			// only that `tart exec` failed — the guest's own stderr is
			// deliberately discarded there, because the registration travels
			// on that same call's stdin. What the delivery script recorded is
			// the diagnosis, and it comes from a closed vocabulary, so it can
			// be reported without handing a guest a channel into a node log.
			// "Recorded nothing" is only reportable if billet looked AND could
			// read what it found.
			verdict := launchStatusText(lastStatus)

			switch {
			case !asked:
				verdict = launchStatusText(statusNotAsked)
			case !readable:
				verdict = launchStatusText(statusUnreadable)
			}

			return fmt.Errorf("deliver runner registration: %w (%s; last attempt: %w)",
				ctx.Err(), verdict, lastErr)
		case <-retry.C:
		}
	}
}

// proveStarted waits for the VM to actually be running, and when it is not,
// says WHY in the operator's own terms.
//
// `tart run` is detached and its exit status is never believed, so a VM that
// refuses to start is otherwise invisible until the registration delivery times
// out against a guest agent that was never there — an operator reads a timeout
// and learns nothing. The commonest cause is not obscure: MEASURED on a real
// Mac, a THIRD concurrent macOS guest is refused with "The number of VMs
// exceeds the system limit (other running VMs: …)", which is Apple's per-host
// limit and exactly what config.DefaultMacOSVMLimit exists to keep billet
// inside. Quoting the run log turns a mystery into that sentence.
func (p *Provider) proveStarted(ctx context.Context, name string) error {
	deadline := time.Now().Add(p.startWindow)

	retry := time.NewTimer(p.proveRetry)
	defer retry.Stop()

	for {
		vms, err := p.list(ctx)
		if err == nil {
			for _, vm := range vms {
				if vm.Name == name && vm.Running {
					return nil
				}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the VM never started: %s", p.runLogTail(name))
		}

		retry.Reset(p.proveRetry)

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to start: %w", name, ctx.Err())
		case <-retry.C:
		}
	}
}

// runLogTail returns the last of what the detached `tart run` wrote, which is
// where the reason a VM refused to start actually appears.
func (p *Provider) runLogTail(name string) string {
	b, err := os.ReadFile(filepath.Join(p.vmDir(name), runLog))
	if err != nil {
		return "no reason recorded (" + err.Error() + ")"
	}

	lines := strings.Fields(strings.TrimSpace(string(b)))
	if len(lines) == 0 {
		return "the run log is empty"
	}

	tail := strings.TrimSpace(string(b))
	if len(tail) > runLogTailLimit {
		tail = tail[len(tail)-runLogTailLimit:]
	}

	return tail
}

// launchStatus reports what BILLET's launcher recorded about this launch, in
// billet's own words.
//
// THE RUNNER'S OUTPUT IS NOT AVAILABLE FOR THIS, and an earlier version of this
// function quoted it. The argument for that was that a launch failure means the
// runner never survived startup, so its first 512 bytes predate any job. Both
// halves are wrong. GitHub can dispatch a job the moment the runner registers,
// which is while proveRunning is still taking its second sample — so a job can
// print a secret and exit inside the window. And the file is a PATHNAME in a
// filesystem the guest controls: a job can unlink it and leave a symlink to
// anything readable, keeping its own descriptor, so what billet reads is
// whatever the job chose. Bounding the read bounds the volume, not the
// sensitivity, and the node logs whatever comes back.
//
// So billet quotes only what billet wrote: a token from a closed set, validated
// against that set here. A guest can overwrite the file, and the worst that
// achieves is billet printing one of its own sentences.
//
// WHAT IT COSTS is the case where the runner started and died of its own
// accord: billet can say that it started and not why. That output stays in the
// guest, where a job's secrets belong.
func (p *Provider) launchStatus(ctx context.Context, name string) string {
	return launchStatusText(p.launchStatusToken(ctx, name))
}

// launchStatusToken reads the status and answers with one of the constants
// above, or "" when there is nothing billet can vouch for.
//
// SEPARATE FROM ITS PROSE because one caller acts on it rather than printing
// it: a command the guest does not have is CONCLUSIVE, and delivery retrying it
// until the node's command timeout leaves a misconfigured tier occupying that
// node's one command slot for ten minutes per job.
func (p *Provider) launchStatusToken(ctx context.Context, name string) string {
	// A BUDGET OF ITS OWN, BUT NEVER A FRESH ONE. This runs on the failure path
	// while the per-name lock is held, and the node serves one command at a
	// time — so spending twenty seconds here after the caller has already given
	// up delays the cleanup and every command behind it. A caller that is
	// already done gets billet's silence rather than more guest I/O.
	if ctx.Err() != nil {
		return statusNotAsked
	}

	ctx, cancel := context.WithTimeout(ctx, launchStatusTimeout)
	defer cancel()

	// BOTH FACTS IN ONE QUESTION, and the test is an `if` rather than an `&&`:
	// a trailing `[ -s pid ] && printf pid` makes the SCRIPT exit non-zero
	// whenever there is no pid, which billet reads as "could not ask" — so
	// every status came back unreadable and the conclusive command-missing exit
	// never fired. The pid is what makes `started` true, so
	// reading them separately would let them disagree; asking together is also
	// what lets the launcher stay the file's only writer.
	out, err := p.run(ctx, "exec", name, "/bin/sh", "-c",
		`printf '%s|' "$(head -c `+strconv.Itoa(launchStatusLimit)+` "$HOME/`+launchStatusFile+
			`" 2>/dev/null)"; if [ -s "$HOME/`+runnerPIDFile+`" ]; then printf pid; fi`)
	if err != nil {
		return statusUnreadable
	}

	record, pid, _ := strings.Cut(out, "|")
	announced := strings.TrimSpace(pid) == "pid"

	out = record

	// "<token> <vm>", and BOTH halves are checked. The name binds the record to
	// this launch: a guest's home is cloned from an image, so a stale record
	// can arrive with the image, and believing one would abandon a launch that
	// was fine.
	token, named, ok := strings.Cut(strings.TrimSpace(out), " ")
	switch {
	case strings.TrimSpace(out) == "":
		return ""

	case !ok || strings.TrimSpace(named) != name:
		// Either a record with no name (a billet older than this one) or one
		// about a different VM. Neither says anything about this launch.
		return statusForeign
	}

	switch token {
	case statusLaunching:
		// `launching` is written before the identity check and long before any
		// pid, so its absence here is a launcher that gave up in between —
		// which is what failing that check looks like.
		if !announced {
			return statusNeverAnnounced
		}

		return statusLaunching

	case statusStarted:
		// TRANSIENT, NOT PERMANENT, when there is no pid. The two files are
		// read in one order and written in the other, so a query can read
		// `started` and miss a pid written an instant later — deriving "never
		// announced" from that would be a false diagnosis produced by the
		// sampling. But it is only transient ONCE: a launcher that died or
		// blocked while writing its pid stays in this state forever, and
		// reporting "in flight" for the caller's whole deadline hides a
		// conclusive failure and burns the node's command slot. The caller
		// resolves it — see deliverRegistration — which is why this returns a
		// state of its own rather than guessing here.
		if !announced {
			return statusStartedNoPID
		}

		return statusStarted

	case statusCommandMissing, statusNeverAnnounced:
		return token

	default:
		// NEVER THE VALUE ITSELF. It is either a billet the guest does not match
		// or a value the guest wrote, and neither is something to print.
		return statusForeign
	}
}

// launchStatusText renders a token as the sentence that goes in a launch error.
func launchStatusText(token string) string {
	switch token {
	case statusCommandMissing:
		return "billet's launcher could not find the tier's command in the guest, so nothing " +
			"was ever started (check the tier's `command` against the image's layout)"

	case statusNeverAnnounced:
		return "billet's launcher never recorded a pid, so the delivery ran and nothing it " +
			"started identified itself — the guest may be tearing down the exec session, or " +
			"the launcher could not read a process identity there"

	case statusLaunching:
		return "billet's launcher announced itself and has not reached exec yet"

	case statusStarted:
		return "billet's launcher reached exec, so the runner started and then stopped on its " +
			"own; its output is in $HOME/" + runnerLog + " inside the guest, which billet does " +
			"not read because a job's output can be in it"

	case statusUnreadable:
		return "billet's own launch status could not be read from the guest"

	case statusForeign:
		return "billet's launch status file holds a value billet did not write"

	case statusNotAsked:
		return "billet did not ask the guest for its launch status, because the launch had " +
			"already been abandoned"

	case statusStartedNoPID:
		// INCONCLUSIVE ON PURPOSE. One sighting can fall in the microseconds
		// between the launcher's status write and its pid write, so stating
		// that nothing announced would be a false verdict about a launch that
		// was fine. Two sightings become statusNeverAnnounced, which does say
		// it — see the loop in deliverRegistration.
		return "billet saw its launcher past the command check with no pid published yet, " +
			"and the launch ended before that could be confirmed either way"

	default:
		return "billet's launcher recorded no status at all, so it died before its first " +
			"instruction"
	}
}

// birthFunc defines billet_birth, a /bin/sh function printing a token that
// identifies WHICH process holds a pid. It is embedded in both the guest
// launcher and the proof so the two cannot drift.
//
// Linux keeps a process's start time in /proc/<pid>/stat and macOS answers
// `ps -o lstart=`; both are fixed for the life of a process and cannot be
// shared with a later one that inherits the number, which is the whole point.
//
// THE /proc PARSE IS NOT `awk '{print $22}'`, AND THAT MATTERS. Field 2 of stat
// is the executable name in parentheses, and it may contain spaces or its own
// parentheses — a runner named `Runner.Listener (x)` shifts every later field
// and a healthy process reads as a stranger. Everything through the LAST `) `
// is removed first, which is the documented way to parse this file, leaving
// start time at field 20 of the remainder.
//
// The Linux token carries the BOOT ID as well, because start ticks are counted
// from boot and are only unique within one: after a guest reboot the same ticks
// can name a different process. macOS `lstart` is an absolute wall-clock time
// and needs no such qualifier.
//
// A failure prints nothing and returns non-zero. Callers must treat an empty
// answer as "cannot tell", never as a match.
const birthFunc = `billet_birth() {
  if [ -r "/proc/$1/stat" ]; then
    _s=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
    _rest=${_s##*') '}
    # shellcheck disable=SC2086
    set -- $_rest
    _st=${20:-}
    case "$_st" in ''|*[!0-9]*) return 1 ;; esac
    _boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
    printf '%s\n' "$_boot:$_st"
  else
    _v=$(ps -p "$1" -o lstart= 2>/dev/null || true)
    [ -n "$_v" ] || return 1
    printf '%s\n' "$_v"
  fi
}
`

// proveRunning asks the guest whether the runner it was told to start is still
// alive, and fails the launch when it is not.
//
// THE POINT IS THE FAILURE MODE IT CONVERTS. A delivery that exits 0 having
// started nothing — the session teardown measured above, a tier command that
// exits immediately, an image missing the binary the tier names — is otherwise
// indistinguishable from a launch that worked: billet logs a started runner,
// the VM keeps running, and the job sits queued until GitHub gives up hours
// later. Asking makes each of those a launch error the caller already knows how
// to handle.
//
// IDENTITY, NOT EXISTENCE. `kill -0` alone proves a number is in use, and guest
// kernels reuse numbers, so a recycled pid would report a healthy runner where
// there is none. The launcher records a birth token with its pid and this
// compares it, which a later process holding the same number cannot match.
//
// STABILITY, NOT A SINGLE INSTANT. The answer must hold across two samples a
// beat apart: the pid file is written just before the exec, so one lucky
// reading can catch a runner that is about to die — which is exactly the
// failure being tested for, arriving a moment late.
func (p *Provider) proveRunning(ctx context.Context, name string) error {
	// FAIL CLOSED ON A MISSING IDENTITY. An earlier version rejected only a
	// mismatch, so an announcement with no birth token — which is exactly what a
	// billet older than this one leaves — read as `alive` on nothing more than a
	// pid being in use, and a recycled number then adopted a stranger. Absent,
	// unreadable and mismatched are all "cannot prove this is our runner".
	script := birthFunc + `p=$(cat "$HOME/` + runnerPIDFile + `" 2>/dev/null || true)
b=$(cat "$HOME/` + runnerBirthFile + `" 2>/dev/null || true)
if [ -z "$p" ]; then echo no-pid; exit 0; fi
if [ -z "$b" ]; then echo no-identity; exit 0; fi
if ! kill -0 "$p" 2>/dev/null; then echo dead; exit 0; fi
now=$(billet_birth "$p" || true)
if [ -z "$now" ]; then echo unreadable; exit 0; fi
if [ "$now" != "$b" ]; then echo recycled; exit 0; fi
echo alive`

	deadline := time.Now().Add(p.proveWindow)

	retry := time.NewTimer(p.proveRetry)
	defer retry.Stop()

	var (
		last    string
		lastErr error
		streak  int
	)

	for {
		out, err := p.run(ctx, "exec", name, "/bin/sh", "-c", script)

		lastErr = err
		if err == nil {
			last = strings.TrimSpace(out)

			switch last {
			case "alive":
				streak++
				if streak >= proveSamples {
					return nil
				}
			case "dead", "recycled", "no-identity":
				// CONCLUSIVE, and waiting cannot improve any of them: a dead
				// process does not come back, a recycled pid only drifts further,
				// and an announcement with no identity will never grow one.
				// Failing now also stops handing pid reuse more chances to look
				// alive. ("unreadable" is deliberately NOT here — that is a
				// transient failure to ask, and it retries.)
				return fmt.Errorf("the runner in %s is not running after its registration was "+
					"delivered (%s); the delivery itself reported success, so this is the "+
					"guest killing it or the tier's command exiting immediately — %s",
					name, provedAnswer(last), p.launchStatus(ctx, name))
			default:
				streak = 0
			}
		} else {
			streak = 0
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				// The error is NOT rendered: it comes from `tart exec`, whose
				// stderr is the guest's, and a job dispatched during the proof
				// window can choose those bytes.
				return fmt.Errorf("could not ask %s whether its runner is alive before the "+
					"launch deadline; the guest agent did not answer", name)
			}

			return fmt.Errorf("the runner in %s never stayed running after its registration "+
				"was delivered (%s) — %s",
				name, provedAnswer(last), p.launchStatus(ctx, name))
		}

		retry.Reset(p.proveRetry)

		select {
		case <-ctx.Done():
			return fmt.Errorf("prove the runner started in %s: %w", name, ctx.Err())
		case <-retry.C:
		}
	}
}

// provedAnswer names what the liveness script said, from a CLOSED set.
//
// Its stdout is the guest's, and a guest can print anything — a job dispatched
// the moment the runner registers is running while proveRunning is still
// sampling, so "whatever it said" is a channel a job can choose bytes on. The
// recognised answers are billet's own vocabulary; anything else is reported as
// unrecognised rather than quoted.
func provedAnswer(last string) string {
	switch last {
	case "alive", "dead", "recycled", "unreadable", "no-pid", "no-identity":
		return "the guest says " + last

	case "":
		return "the guest said nothing"

	default:
		return "the guest answered something billet does not recognise"
	}
}

// execStdin runs `tart exec -i` with the given stdin. The stdin is a credential
// on the delivery path, so it is never logged and never in argv.
func (p *Provider) execStdin(ctx context.Context, name, stdin string, argv ...string) error {
	args := append([]string{"exec", "-i", name}, argv...)

	// #nosec G204 -- operator-configured binary, billet-built arguments, no shell
	// on the host side.
	cmd := exec.CommandContext(ctx, p.tart, args...)
	cmd.WaitDelay = commandWaitDelay
	cmd.Stdin = strings.NewReader(stdin)

	// STREAMED TO NOWHERE, not buffered and then dropped. An unset Stderr is
	// INHERITED, which would print the guest's output — credential included —
	// on billet's own stderr; a strings.Builder avoided that and replaced it
	// with a guest that writes stderr forever growing the node process until it
	// is OOM-killed. io.Discard has neither property.
	cmd.Stderr = io.Discard

	if err := cmd.Run(); err != nil {
		// THE GUEST'S STDERR IS NOT REPORTED, and this is the one call where
		// that matters most: the runner's registration goes in on STDIN, so a
		// guest whose shell copies its stdin to stderr — three characters,
		// `cat >&2` — would reflect a live credential into an error the node
		// writes to its log. Nothing downstream can filter that, because a
		// secret out of its field is an opaque string.
		//
		// What is lost is the guest's own words on a failed delivery. They are
		// not lost from BILLET: the script records its verdict in a status file
		// with a closed vocabulary, which launchStatus reads and which no guest
		// can use to make billet print bytes of its choosing.
		return fmt.Errorf("%s exec in %s failed: %w", p.tart, name, err)
	}

	return nil
}

// Destroy stops a VM, proves it stopped, and deletes it.
//
// Idempotent: a VM that is already gone is success. CONFIRMING, like docker and
// unlike EC2: tart is authoritative about its own local store, `tart stop`
// escalates to SIGKILL after its timeout, and the state is read back before
// anything is deleted — TeardownStopped is returned only after that proof,
// because deleting the directory of a running VM removes the evidence that it
// exists while the VMM keeps executing someone's job.
func (p *Provider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	if id == "" {
		return provider.TeardownRequested, errors.New("tart: destroy needs a VM name")
	}

	if _, ok := provider.LeaseOf(strings.TrimSuffix(id, stagingSuffix)); !ok {
		return provider.TeardownRequested, fmt.Errorf(
			"tart: refusing to destroy %q, which is not a billet instance name", id)
	}

	unlock := p.lockName(id)
	defer unlock()

	if _, err := os.Stat(p.vmDir(id)); errors.Is(err, os.ErrNotExist) {
		// No directory is no VM — tart's local store is a directory — but only
		// once tart AGREES. Billet's TART_HOME and the binary's must be the same
		// store, and if they ever are not, every stat here misses while every VM
		// keeps running; the cross-check turns that configuration drift into an
		// error instead of a fleet-wide TeardownStopped. What this cannot catch
		// is an operator removing a running VM's directory by hand underneath
		// tart — that deletes the evidence outside billet entirely, and it is a
		// documented residual rather than a handled case.
		vms, listErr := p.list(ctx)
		if listErr != nil {
			return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: %w", id, listErr)
		}

		for _, vm := range vms {
			if vm.Name == id {
				return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: billet sees no "+
					"VM directory but `tart list` still reports one; billet's TART_HOME and "+
					"tart's disagree about where VMs live", id)
			}
		}

		return provider.TeardownStopped, nil
	} else if err != nil {
		return provider.TeardownRequested, fmt.Errorf("tart: inspect %s: %w", id, err)
	}

	owner, err := p.ownerOf(id)
	if err != nil {
		return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: ownership cannot be "+
			"proved (%w); an unreadable marker is not authority to destroy", id, err)
	}

	if owner != p.owner {
		return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: it belongs to another "+
			"billet deployment", id)
	}

	if _, err := p.run(ctx, "stop", id); err != nil && !isNotRunning(err) && !isMissingVM(err) {
		return provider.TeardownRequested, fmt.Errorf("tart: stop %s: %w", id, err)
	}

	// PROOF, not inference from the stop's exit status: the state is read back,
	// and only a VM tart itself REPORTS AS STOPPED may have its directory
	// removed. The row must be PRESENT — a loop that merely finds no running
	// row treats an omitted row as proof, and an inventory's omission is the
	// one answer this package never trusts.
	//
	// AND IT WAITS FOR THE STATE TO SETTLE, because `tart stop` REQUESTS a stop
	// rather than completing one. Reading the state once, immediately after,
	// caught a VM mid-transition and returned "still reports state running" —
	// on a loaded machine often enough to fail the suite, and in production it
	// makes every teardown a retry the caller did not need. Waiting does not
	// weaken the proof: what is accepted is still only a row that says stopped.
	// BOUNDED BY THE EARLIER OF THE TWO. Computing a deadline and then handing
	// each `tart list` the CALLER's context bounds nothing: a hung list holds
	// the node's one command slot for the caller's whole timeout, which is ten
	// minutes, while this function claims thirty seconds.
	stopCtx, cancelStop := context.WithTimeout(ctx, p.stopWindow)
	defer cancelStop()

	retry := time.NewTimer(p.proveRetry)
	defer retry.Stop()

	for {
		vms, err := p.list(stopCtx)
		if err != nil {
			// OUR OWN BOUND, reported as what it is. Without this the operator
			// sees the list being killed rather than "it never stopped", which
			// describes the mechanism instead of the situation.
			if stopCtx.Err() != nil && ctx.Err() == nil {
				return provider.TeardownRequested, fmt.Errorf(
					"tart: %s did not report itself stopped within %s of being asked",
					id, p.stopWindow)
			}

			return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: %w", id, err)
		}

		var (
			proved bool
			state  string
			listed bool
		)

		for _, vm := range vms {
			if vm.Name != id {
				continue
			}

			listed, state = true, vm.State
			proved = !vm.Running && strings.EqualFold(vm.State, "stopped")
		}

		switch {
		case proved:
			// Settled, and settled STOPPED.

		case !listed:
			// Mostly redundant with list()'s directory cross-check — a
			// billet-named directory without a row already fails the list — and
			// kept deliberately: the one path that reaches here is the directory
			// vanishing between the stat above and the list, and that race
			// window is exactly what this function exists to refuse to guess
			// about.
			return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: the VM directory "+
				"exists but `tart list` does not report it, so its state cannot be proved", id)

		case ctx.Err() != nil:
			// The CALLER ran out, which is a different fact from billet's own
			// settle window expiring and must not be reported as one.
			return provider.TeardownRequested, fmt.Errorf(
				"tart: waiting for %s to stop: %w", id, ctx.Err())

		case stopCtx.Err() != nil:
			return provider.TeardownRequested, fmt.Errorf(
				"tart: %s still reports state %q %s after it was asked to stop",
				id, state, p.stopWindow)

		default:
			retry.Reset(p.proveRetry)

			select {
			case <-stopCtx.Done():
				// The SAME attribution the observation path makes: a caller
				// that ran out is a different fact from billet's own settle
				// window expiring.
				if ctx.Err() != nil {
					return provider.TeardownRequested, fmt.Errorf(
						"tart: waiting for %s to stop: %w", id, ctx.Err())
				}

				return provider.TeardownRequested, fmt.Errorf(
					"tart: %s did not report itself stopped within %s of being asked",
					id, p.stopWindow)
			case <-retry.C:
			}

			continue
		}

		break
	}

	// RE-PROVED AND DELETED AS ONE ACT, under the host-wide store lock.
	//
	// The ownership check above happened before a poll that can last thirty
	// seconds, and `tart delete` resolves the NAME again — so a VM appearing
	// under this name between the proof and the delete is what would be
	// destroyed. Both halves therefore live in deleteProvenOurs, which holds the
	// lock across them, and the only operation that can put a different VM under
	// a lease name — Launch's rename — takes the same lock. That is what makes
	// the marker re-read conclusive rather than merely narrowing.
	//
	// THE MECHANISM THAT WAS TRIED FIRST AND REVERTED, kept here because it is
	// genuinely tempting and will otherwise be written again: renaming the VM
	// aside and deleting the new name, so the delete acts on an object rather
	// than a name. A quarantine name is a new durable state on disk that List,
	// the inventory cross-check, the next Launch that wants the name and a
	// Destroy resuming an interrupted one must all reason about — and one review
	// round of it produced three defects of exactly that shape, each of which
	// either freed a running guest's capacity or deleted another deployment's
	// VM. A lock is the smaller mechanism.
	//
	// WHAT REMAINS OPEN, because a lock only binds those who take it: an
	// operator running `tart delete` or `tart rename` by hand. tart has no lock
	// of its own and billet does not own the store, so there is nothing billet
	// can do about that beyond saying so.
	if err := p.deleteProvenOurs(ctx, id); err != nil {
		return provider.TeardownRequested, fmt.Errorf("tart: destroy %s: %w", id, err)
	}

	p.log.Info("destroyed VM", "runner", id)

	return provider.TeardownStopped, nil
}

// stillOurs re-reads the ownership marker, because the poll above put a whole
// stop window between deciding whose VM this is and acting on it.
//
// THE MARKER IS THE CHECK, AND IT USED TO HAVE AN os.SameFile BESIDE IT. The
// argument for that was that a directory replaced wholesale keeps its name and
// changes its identity, so comparing the inode catches a replacement the marker
// cannot. It does not: CI demonstrated the case on Linux, where removing a
// directory and recreating it under the same name REUSES the inode, so
// os.SameFile answered "the same file" about a directory that had been replaced
// — on a filesystem where the test for it therefore could not pass. A check
// that cannot be tested, and that is defeated by an ordinary property of the
// platform it runs on, is worse than its absence: it reads as proof.
//
// What the marker does cover is the case that matters. Another deployment
// taking this name must write its own marker, and a same-deployment
// replacement cannot race this: `lockName` serializes one process and the
// host-wide deployment lock stops two.
//
// CALLED ONLY FROM deleteProvenOurs, WHICH HOLDS THE STORE LOCK, and that is
// what turns this from a narrowing into a proof. On its own it answers about an
// instant; under the lock no billet can put a different VM under the name
// between the answer and the delete, because publishing a lease name takes the
// same lock. Calling it anywhere else re-opens the window it was written for.
func (p *Provider) stillOurs(id string) error {
	owner, err := p.ownerOf(id)
	if err != nil {
		return fmt.Errorf("re-read the ownership marker for %s before deleting it: %w", id, err)
	}

	if owner != p.owner {
		return fmt.Errorf("the ownership marker for %s changed while billet waited for it to "+
			"stop; it now belongs to another billet deployment", id)
	}

	return nil
}

// Find reports the VM with that name, and whether there was one. Only a VM this
// deployment owns is reported — adoption is the dangerous direction, because
// the caller may go on to destroy what it adopts.
func (p *Provider) Find(ctx context.Context, name string) (*Instance, bool, error) {
	instances, err := p.List(ctx)
	if err != nil {
		return nil, false, err
	}

	for _, inst := range instances {
		if inst.Name == name {
			return inst, true, nil
		}
	}

	return nil, false, nil
}

// List reports every VM this billet deployment created here, running or not.
//
// The input to reconciliation, which frees the capacity of every lease ABSENT
// from it — so the dangerous failure here is an answer that is short, empty and
// successful, and every ambiguity below resolves toward an error rather than an
// omission.
//
// A SNAPSHOT, deliberately unserialized with Launch: a VM whose launch is in
// flight may be absent, exactly as a docker container is absent from a `docker
// ps` taken before its run returns. What makes that safe is the lease contract,
// not this package — the node renews a lease from the moment it commits to
// launching, so reconciliation cannot free capacity whose launch is mid-flight.
// Serializing List against Launch instead would block inventory for the minutes
// a registration delivery legitimately takes.
func (p *Provider) List(ctx context.Context) ([]*Instance, error) {
	vms, err := p.list(ctx)
	if err != nil {
		return nil, err
	}

	var instances []*Instance

	for _, vm := range vms {
		lease, ok := provider.LeaseOf(vm.Name)
		if !ok {
			// Not billet's naming at all: an operator's own VM, another tool's.
			continue
		}

		owner, err := p.ownerOf(vm.Name)

		switch {
		case err != nil && strings.HasSuffix(vm.Name, stagingSuffix):
			// A staging clone with no readable marker is the one crash window the
			// clone-mark-rename ordering leaves: a clone abandoned between creation
			// and marking. It never matches a lease, nothing adopts it, and it is
			// REPORTED rather than silently skipped so the leak has a name.
			p.log.Warn("a staging clone has no readable ownership marker; "+
				"if it is this deployment's, remove it with `tart delete`",
				"vm", vm.Name, "error", err)

			continue
		case err != nil:
			// A LEASE-NAMED VM always carries a marker — the rename happens after
			// the marker is synced — so an unreadable one is not a crash artifact,
			// and guessing in either direction is wrong: omitting it lets its
			// capacity be resold while it runs, adopting it may destroy another
			// deployment's guest. Stop reconciliation and say so.
			return nil, fmt.Errorf("tart: %s carries a billet lease name (%s) but its ownership "+
				"marker cannot be read (%w); refusing to reconcile against an inventory billet "+
				"cannot vouch for", vm.Name, lease, err)
		case owner != p.owner:
			// Another billet deployment's guest, healthy and none of our business.
			continue
		}

		if strings.HasSuffix(vm.Name, stagingSuffix) {
			// Ours, never renamed, never launched: cleanup for the next Launch that
			// wants the name, not inventory a lease is charged for.
			continue
		}

		instances = append(instances, &Instance{
			ID:   vm.Name,
			Name: vm.Name,
			// A suspended VM is not executing but can return to executing, so for
			// custody it counts as running: treating it as finished destroys the
			// frozen job. An unrecognised state counts as running for the same
			// reason — the caller destroys what is not.
			Running: vm.Running || !strings.EqualFold(vm.State, "stopped"),
		})
	}

	return instances, nil
}

// listedVM is one entry of `tart list --format json`, pinned to the tool's own
// VMInfo encoder: Source, Name, Running and State are the fields billet reads.
type listedVM struct {
	Source  string `json:"Source"`
	Name    string `json:"Name"`
	Running bool   `json:"Running"`
	State   string `json:"State"`
}

// list runs `tart list` for local VMs and refuses output it cannot vouch for.
//
// The output is CROSS-CHECKED against the VM directories, because the caller
// frees the capacity of every lease absent from it — so the dangerous failure
// is not an error but an answer that is short, empty and successful. `[]`, JSON
// `null`, a wrapper that drops a row, a tart that reads a different TART_HOME:
// each would be a silent shrink without the check, and each is an error with
// it. The directories are enumerated AFTER the list runs, so a VM created in
// between appears as a directory without a row and fails toward the error side;
// reconciliation retries, and a transient refusal is the safe direction.
func (p *Provider) list(ctx context.Context) ([]listedVM, error) {
	out, err := p.run(ctx, "list", "--format", "json", "--source", "local")
	if err != nil {
		return nil, fmt.Errorf("tart: list VMs: %w", err)
	}

	var vms []listedVM

	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return nil, fmt.Errorf("tart: cannot read `tart list` output as JSON: %w; is `tart` "+
			"a wrapper that prints extra output?", err)
	}

	seen := make(map[string]bool, len(vms))

	for _, vm := range vms {
		// REPORTED, not skipped: an entry billet cannot read is an inventory it
		// cannot vouch for, and the caller acts on what is missing.
		if vm.Name == "" || vm.State == "" {
			return nil, fmt.Errorf("tart: a `tart list` entry is missing its name or state: %+v", vm)
		}

		// Two rows for one name means billet cannot know which state is the VM's;
		// acting on either could act on the wrong one.
		if seen[vm.Name] {
			return nil, fmt.Errorf("tart: `tart list` reports %q twice", vm.Name)
		}

		seen[vm.Name] = true
	}

	if err := p.crossCheckStore(ctx, seen); err != nil {
		return nil, err
	}

	return vms, nil
}

// crossCheckStore refuses a listing that does not account for billet's store.
//
// THE CALLER FREES THE CAPACITY OF EVERY LEASE ABSENT FROM THE LISTING, so a row
// that goes missing while its guest keeps running is capacity resold underneath
// live work. That is what this exists to make impossible.
//
// IT ASKS TART ABOUT EACH ONE RATHER THAN INFERRING, and the two versions before
// this each got that wrong in opposite directions. The first refused every
// operation whenever any billet-named directory had no row — and one directory
// left behind by a killed node then wedged every launch, teardown and check on
// that host, permanently, until a human removed it. The second tolerated single
// absences, reasoning that a directory tart does not list is one tart cannot
// start; that is exactly the inference the first version existed to prevent, and
// it silently frees a live guest's capacity if a row is dropped for any other
// reason.
//
// So neither infers. `tart get <name>` answers about that one VM, and its
// answers are MEASURED rather than read from the documentation: a real VM
// returns its configuration, a name with no directory exits 2 with `does not
// exist`, and a directory tart cannot read exits 1 with `missing files for a
// supported layout`. Only those last two are corpses. A VM that answers is real
// and its absence from the listing is an inconsistency worth refusing over;
// anything else is "cannot tell", which fails closed.
func (p *Provider) crossCheckStore(ctx context.Context, seen map[string]bool) error {
	entries, err := os.ReadDir(filepath.Join(p.home, "vms"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tart: enumerate VM directories: %w", err)
	}

	var missing []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Only billet's own names are considered: another tool's VM missing from
		// a filtered view is not billet's inventory.
		if _, ok := provider.LeaseOf(strings.TrimSuffix(entry.Name(), stagingSuffix)); !ok {
			continue
		}

		if !seen[entry.Name()] {
			missing = append(missing, entry.Name())
		}
	}

	// CLASSIFIED FIRST, DECIDED AFTER, and the ORDER is the whole correction.
	// An earlier version returned the store-mismatch error before asking about
	// any single VM — which meant a host whose store held exactly ONE
	// interrupted clone and nothing else was wedged permanently, because
	// `ours == missing == 1` looked identical to "tart can see none of our
	// store". That is the same failure the per-VM query was added to remove,
	// reached from the other side.
	//
	// THE DISCRIMINATOR IS WHICH REFUSAL TART GIVES, and both were measured:
	//
	//   - "missing files for a supported layout" means tart LOOKED at our
	//     directory and could not make a VM of it. That is a corpse: a clone
	//     killed before it was finished. It is not a running guest, because
	//     tart cannot start what it cannot read.
	//   - "does not exist" means tart did not find the directory AT ALL — and
	//     billet has just enumerated it, so tart is reading a different store.
	//     Classifying that as a corpse is what would free the capacity of every
	//     lease on the host, one confident answer at a time.
	var absent []string

	for _, name := range missing {
		_, err := p.run(ctx, "get", name)

		switch {
		case err == nil:
			return fmt.Errorf("tart: %s is a VM tart can describe but did not list; an "+
				"inventory that omits a live VM has its lease's capacity freed and resold "+
				"while it runs, so this listing is refused rather than acted on", name)

		case isUnreadableVM(err) && strings.HasSuffix(name, stagingSuffix):
			// ONLY A STAGING NAME. `tart get` failing on the layout proves tart
			// cannot RECONSTRUCT the VM from its store — not that a `tart run`
			// already executing has stopped. Remove or damage a running guest's
			// config.json and this is exactly the state: the VMM keeps going
			// while tart can no longer describe it, and dropping the row would
			// free that lease's capacity underneath a live job.
			//
			// A staging name is different, and it is the clone/mark/rename
			// ordering that makes it so: a directory still carrying the staging
			// suffix was never renamed, so it was never launched, so there is
			// no VMM to be wrong about.
			p.reportCorpse(name)

		case isMissingVM(err):
			absent = append(absent, name)

		case isUnreadableVM(err):
			// A LEASE-NAMED DIRECTORY TART CANNOT READ, which is ambiguous and
			// therefore refused. It wedges this host until a person looks, and
			// that is the direction to fail in: the alternative is freeing a
			// lease whose guest may still be running and placing a second job
			// on top of it. The message names the directory and what to check.
			return fmt.Errorf("tart: %s carries a billet lease name and `tart list` does not "+
				"report it, while `tart get` cannot read it as a VM (%w). That does NOT prove "+
				"its guest stopped — a running VM whose config is damaged looks exactly like "+
				"this — so billet refuses rather than freeing capacity that may still be in "+
				"use. Check for a `tart run` process holding it; once you have established "+
				"there is none, remove the directory", name, err)

		default:
			return fmt.Errorf("tart: %s has a VM directory and no `tart list` row, and tart "+
				"could not say whether it exists (%w); billet will not guess about a VM whose "+
				"capacity it may be about to free", name, err)
		}
	}

	// A directory billet just enumerated that tart says does not exist is the
	// two of them disagreeing about where the store IS. Reported once, naming
	// the whole set rather than the first, because the count is the evidence.
	if len(absent) > 0 {
		return fmt.Errorf("tart: billet's store holds %d VM director%s that `tart list` does "+
			"not report and `tart get` says do not exist (%s…); billet's TART_HOME and tart's "+
			"are not the same store, and reconciling against this listing would free capacity "+
			"for guests that are still running",
			len(absent), plural(len(absent), "y", "ies"), absent[0])
	}

	return nil
}

// plural picks a suffix so a diagnostic reads as a sentence.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// reportCorpse names a directory tart cannot make a VM out of, and offers to
// remove it only when it can prove whose it is.
//
// THE REMOVAL COMMAND IS THE DANGEROUS PART. Every billet deployment names its
// VMs the same way, so a lease-named directory is not necessarily this
// deployment's — and printing an exact `rm -rf` for one is telling an operator
// to delete another deployment's data. The marker is the only thing that says
// whose it is, so nothing is suggested without reading it.
func (p *Provider) reportCorpse(name string) {
	const msg = "a VM directory has no `tart list` row and tart cannot read it as a VM, " +
		"so it holds disk and is excluded from inventory"

	owner, err := p.ownerOf(name)

	switch {
	case err != nil:
		p.log.Warn(msg+"; its ownership marker cannot be read, so billet will not say "+
			"whether it is safe to remove", "vm", name, "error", err)

	case owner != p.owner:
		p.log.Warn(msg+"; it belongs to another billet deployment, which is the one that "+
			"can remove it", "vm", name, "owner", owner)

	default:
		p.log.Warn(msg, "vm", name,
			"remove_with", "rm -rf "+shellQuote(filepath.Join(p.home, "vms", name)))
	}
}

// writeOwner writes and syncs the ownership marker into a VM directory.
//
// Synced — file then directory — because the marker is the fact List trusts
// when it decides whose compute a VM is, and the rename that exposes the VM
// under its lease name must not become durable before the marker does.
func (p *Provider) writeOwner(name string) error {
	path := filepath.Join(p.vmDir(name), ownerMarker)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create ownership marker: %w", err)
	}

	if _, err := f.WriteString(p.owner + "\n"); err != nil {
		_ = f.Close()

		return fmt.Errorf("write ownership marker: %w", err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("sync ownership marker: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close ownership marker: %w", err)
	}

	if err := syncDir(p.vmDir(name)); err != nil {
		return fmt.Errorf("sync VM directory: %w", err)
	}

	return nil
}

// ownerOf reads the deployment identity out of a VM directory's marker.
//
// The content is VALIDATED as a deployment id, not merely read: an empty or
// corrupt marker that comes back as an arbitrary string reads as "some other
// deployment" at every call site, and "someone else's" is the silent answer —
// List omits the VM and its capacity is resold while it runs. Malformed is an
// error, and only a VALID id that differs is foreign.
func (p *Provider) ownerOf(name string) (string, error) {
	f, err := os.Open(filepath.Join(p.vmDir(name), ownerMarker))
	if err != nil {
		return "", err
	}

	defer func() { _ = f.Close() }()

	buf := make([]byte, markerLimit)

	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}

	if n == markerLimit {
		return "", fmt.Errorf("marker is larger than %d bytes, which no deployment id is", markerLimit)
	}

	owner := strings.TrimSpace(string(buf[:n]))
	if err := deploymentid.Validate(owner); err != nil {
		return "", fmt.Errorf("marker does not hold a deployment id: %w", err)
	}

	return owner, nil
}

// vmDir is where tart keeps one VM.
func (p *Provider) vmDir(name string) string {
	return filepath.Join(p.home, "vms", name)
}

// discardStaging removes a staging clone a failed launch no longer needs. Best
// effort with a loud failure: what is left is a stopped clone holding disk, not
// a credential — the registration never touches the clone.
//
// DELIBERATELY NOT UNDER THE STORE LOCK, and it was, which was a defect a review
// caught. The reasoning for taking it was uniformity; the cost was permanent.
// The one path that reaches here after a CONTENDED publish would have waited out
// a second full window and then failed too — and what it abandons is a marked
// staging clone that nothing ever reclaims: List skips it because it is not
// inventory a lease is charged for, and prepareStaging only ever revisits THIS
// lease's name, which no later job will use. Tens of gigabytes, once per
// occurrence, reported in one log line and never again.
//
// Taking the lock was not buying anything for that price. A staging name cannot
// be raced: it carries a lease id unique to one control plane, and checkSpec
// reserves the suffix so nothing can be launched under it. See prepareStaging.
func (p *Provider) discardStaging(ctx context.Context, staging string) {
	if _, err := p.run(ctx, "delete", staging); err != nil && !isMissingVM(err) {
		p.log.Error("could not remove a staging clone after a failed launch; "+
			"it holds disk until removed", "vm", staging, "error", err)
	}
}

// run invokes the tart CLI and returns its STDOUT, keeping stderr for the
// error. The streams are kept apart for the docker backend's reason: values
// come back on stdout, narration on stderr, and a combined buffer corrupts any
// value the first time narration appears.
func (p *Provider) run(ctx context.Context, args ...string) (string, error) {
	// #nosec G204 -- the binary is operator configuration and every argument is
	// built here from typed config, never from job or workflow input. No shell.
	cmd := exec.CommandContext(ctx, p.tart, args...)

	// A CANCELLED COMMAND MUST ACTUALLY RETURN. CommandContext kills the process
	// it started, and Wait then blocks until the output pipes close — which a
	// surviving grandchild holds open. Measured: a `tart list` that hangs took
	// the full sixty seconds of its child's sleep despite a 300ms deadline, so
	// every timeout in this package was advisory. WaitDelay bounds the wait for
	// the pipes after cancellation, which is what makes the deadline real.
	cmd.WaitDelay = commandWaitDelay

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			p.tart, args[0], err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// isMissingVM reports whether a command failed because the VM does not exist.
// Pinned to tart's own RuntimeError.VMDoesNotExist rendering. Narrow on
// purpose: matching loosely would swallow real failures, and a swallowed
// teardown failure is a VM nobody knows is running.
func isMissingVM(err error) bool {
	return strings.Contains(err.Error(), "does not exist")
}

// isUnreadableVM reports whether a command failed because the directory is not
// a VM tart can read.
//
// MEASURED against a real tart, not read: a directory under vms/ that has no
// config.json makes `tart get` exit 1 with "VM is missing files for a supported
// layout". That is the shape a killed clone leaves behind, and telling it apart
// from a VM tart CAN describe is what lets billet exclude a corpse from
// inventory without ever excluding a live guest.
func isUnreadableVM(err error) bool {
	return strings.Contains(err.Error(), "missing files for a supported layout")
}

// isNotRunning reports whether a stop failed because the VM is already
// stopped. Pinned to RuntimeError.VMNotRunning: `VM "name" is not running`.
func isNotRunning(err error) bool {
	return strings.Contains(err.Error(), "is not running")
}

// syncDir fsyncs a directory so a rename or create inside it is durable.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	return d.Sync()
}

// shellJoin renders argv as a /bin/sh command line with every word
// single-quoted, so an argument containing spaces or metacharacters reaches the
// runner as one word rather than being re-split by the guest shell.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))

	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}

	return strings.Join(quoted, " ")
}

// shellQuote renders one string as a single /bin/sh word.
//
// Single quotes, because inside them the shell interprets nothing at all; the
// only character needing care is the quote itself, which is closed, escaped and
// reopened. This is what lets a whole script be passed as one argument to
// `sh -c` without the outer shell touching it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
