package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// Upgrader starts the transactional updater that replaces this node's billet.
//
// A SEAM RATHER THAN A DIRECT exec, for two reasons that are both about honesty.
// A node with no updater configured must REFUSE the command rather than silently
// do nothing — a rollout that recorded it as instructed would wait forever for a
// convergence that cannot come — and the argv this produces is the only part of
// the mechanism a test can see, since everything downstream stops services and
// replaces binaries.
type Upgrader interface {
	StartUpgrade(ctx context.Context, spec nodeapi.UpgradeSpec) error
}

// WithUpgrader gives a node the ability to replace its own billet.
//
// ABSENT BY DEFAULT, and deliberately. A node running out of a working directory,
// or under a test harness, has no packaged binary to replace and no units to
// restart; making the capability opt-in means those refuse the command with a
// sentence an operator can act on instead of attempting a transaction against a
// machine that is not shaped for one.
func WithUpgrader(u Upgrader) Option {
	return func(r *Runner) { r.upgrader = u }
}

// ErrNoUpgrader means this node cannot replace its own billet.
var ErrNoUpgrader = errors.New("node: this node has no transactional updater")

// StartUpgrade launches the updater and waits only for it to accept responsibility.
//
// IT DOES NOT WAIT FOR THE TRANSACTION. A node executes commands one
// at a time and each command's timeout starts when it is QUEUED, so an upgrade
// carried out inline would hold the node's single slot for as long as the drain
// takes — which is as long as the longest job, with no bound on it. Every other
// command to this host would expire in the queue behind it, including the
// destroys that let the drain finish.
func (r *Runner) StartUpgrade(ctx context.Context, spec nodeapi.UpgradeSpec) error {
	if r.upgrader == nil {
		return fmt.Errorf("%w, so it cannot install %s; upgrade it out of band",
			ErrNoUpgrader, spec.Version)
	}

	r.log.Info("starting a transactional upgrade of this node",
		"version", spec.Version, "rollout", spec.RolloutID, "generation", spec.Generation)

	return r.upgrader.StartUpgrade(ctx, spec)
}

// upgradeArgs renders the instruction as the updater's command line.
//
// A FUNCTION SO A TEST CAN SEE IT. Everything past this point stops services and
// replaces binaries, so the argv is the last observable thing about a dispatched
// upgrade — and a review found that three of the spec's four fields were being
// dropped here while a test asserting the spec reached the Upgrader INTERFACE
// whole passed the whole time.
//
// EVERY FIELD IS CARRIED. The digest is what makes the node install the manifest
// the ROLLOUT resolved rather than whatever the channel says by the time this
// runs; the rollout id and generation are what let an operator who finds a
// machine mid-upgrade know which fleet decision it belongs to. Sending only the
// version turned a fenced instruction into a bare request to install a tag.
func upgradeArgs(spec nodeapi.UpgradeSpec, configPath, ackPath string) []string {
	args := []string{"host-upgrade", "--version", spec.Version}

	if spec.ManifestSHA256 != "" {
		args = append(args, "--manifest-sha256", spec.ManifestSHA256)
	}

	if spec.RolloutID != "" {
		args = append(args, "--rollout", spec.RolloutID)
	}

	if spec.Generation > 0 {
		args = append(args, "--generation", strconv.FormatInt(spec.Generation, 10))
	}

	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	args = append(args, "--ack-path", ackPath)

	return args
}

// ackWait bounds how long the node waits to hear whether the updater took the
// job.
//
// GENEROUS, BUT FINITE. What happens before the answer is a manifest fetch, a
// signature verification, a compatibility check and a claim — seconds on a
// healthy host, longer against a slow mirror. What happens AFTER it is an archive
// download and an unbounded drain, so this must never be sized against those. A
// node executes commands one at a time under a ten-minute timeout, and this sits
// comfortably inside it: an updater that has not answered by now has something
// wrong with it, and saying so lets the rollout back off and retry rather than
// recording a host as draining that never heard anything.
// A VAR RATHER THAN A CONST, so a test can prove the bound actually applies.
// Without that this is an untested branch in the one mechanism standing between a
// silent updater and a node that waits forever.
// Tests that replace this must stay serial. Parallel reader tests park until all
// serial tests finish, so their reads cannot overlap a mutation window.
var ackWait = 90 * time.Second

// ErrUpgradeRefused means the updater started, decided against the instruction,
// and changed nothing.
var ErrUpgradeRefused = errors.New("node: the updater refused this upgrade")

// awaitAck reads the updater's one-line answer.
//
// A SPAWN IS NOT AN ANSWER, which is the whole reason this exists. The updater is
// detached on purpose — it has to outlive the node it is about to stop — so
// returning as soon as it started meant every refusal it makes before touching
// anything was invisible: a digest that disagreed with the fleet's decision, a
// candidate this build cannot run, a claim another upgrade already held. The
// control plane recorded the host as draining and waited forever, because the
// host keeps running, stays live, reports the same release, and nothing ever
// contradicts it.
//
// EOF WITH NOTHING WRITTEN IS A REFUSAL TOO. It means the updater died before it
// could say anything, which is exactly the case a silent spawn hid.
//
// A connected updater may stall before finishing its answer; the read deadline
// bounds that separately from the listener's wait for a connection.
func awaitAck(r ackConn, version string) error {
	defer func() { _ = r.Close() }()

	if err := r.SetReadDeadline(time.Now().Add(ackWait)); err != nil {
		return fmt.Errorf("node: bound the wait for the updater's answer: %w", err)
	}

	// ONE LINE, NOT EVERYTHING UNTIL EOF. Reading to EOF makes the node wait for
	// the updater to CLOSE the descriptor rather than to answer on it — so an
	// updater that accepted and then took its time closing would be reported as
	// refusing, ninety seconds after it had already said yes, while it went on to
	// install. The answer is one line by construction; read exactly that.
	//
	// THE LIMIT LEAVES ROOM FOR THE TERMINATOR. The writer bounds its payload at
	// MaxAckBytes and then appends a newline, so a reader capped at MaxAckBytes
	// would cut the newline off a maximal answer and, by the rule below, refuse it.
	answer, err := bufio.NewReader(io.LimitReader(r, MaxAckBytes+1)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("node: read the updater's answer: %w", err)
	}

	// A LINE THAT NEVER ENDED IS NOT AN ANSWER, WHATEVER IT SAYS SO FAR.
	//
	// ReadString returns what it has TOGETHER WITH the error, so bytes that happen
	// to spell an acceptance arrive identically whether the updater finished
	// writing or died halfway through the word. Taking them at face value records
	// the host as draining on the strength of a sentence nobody finished — and the
	// rollout then waits forever for a machine that never took the job. Only a read
	// that found the newline proves the updater completed its answer.
	incomplete := err != nil

	line := strings.TrimSpace(answer)

	switch {
	case line == AckAccepted && !incomplete:
		return nil

	case line == "" || incomplete:
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return ackTimeout(version)
		}

		if line != "" {
			return fmt.Errorf("%w: it stopped partway through its answer (%q)",
				ErrUpgradeRefused, line)
		}

		return fmt.Errorf("%w: it stopped without saying why", ErrUpgradeRefused)

	default:
		// The socket is in the node's private state directory; the updater bounds
		// and flattens the reason it writes.
		return fmt.Errorf("%w: %s", ErrUpgradeRefused, strings.TrimPrefix(line, AckRefused))
	}
}

// The updater's vocabulary, defined ONCE, here.
//
// IN THE PACKAGE THAT READS IT, and used by the command that writes it — which is
// the way round that works, since cmd/billet imports this package and nothing
// imports cmd/billet. Two copies of a protocol whose whole job is to be
// recognised is the two-pins problem: the writer would go on writing a word the
// reader had stopped accepting, and the failure would be a rollout waiting
// forever on an updater that had answered.
const (
	// AckAccepted is what an updater writes once it has taken responsibility.
	AckAccepted = "accepted"
	// AckRefused prefixes the reason when it has not.
	AckRefused = "refused: "
	// MaxAckBytes bounds what either end will write or read. An acknowledgement is
	// one short line.
	MaxAckBytes = 4 << 10
)

// ackConn lets the bounded reader use a socket or a test pipe.
type ackConn interface {
	io.Reader
	SetReadDeadline(deadline time.Time) error
	Close() error
}

func ackTimeout(version string) error {
	return fmt.Errorf("%w: it did not say within %s whether it had taken the job; "+
		"nothing here can tell whether it is installing %s or stuck, so look at "+
		"the upgrade journal on this machine", ErrUpgradeRefused, ackWait, version)
}

// ExecUpgrader runs billet's own updater outside the node's service lifetime.
type ExecUpgrader struct {
	// Binary is the billet to exec. Empty means the running executable.
	Binary string
	// ConfigPath is the config the updater reads.
	ConfigPath string
	// AckDir is the node's private, writable state directory.
	AckDir string
	// DSNEnv names the configured PostgreSQL variable; its value never enters argv.
	DSNEnv string
}

// StartUpgrade waits only for the updater's acceptance or refusal, never its drain.
func (e ExecUpgrader) StartUpgrade(_ context.Context, spec nodeapi.UpgradeSpec) error {
	binary := e.Binary
	if binary == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("node: find this billet to run the updater: %w", err)
		}

		binary = self
	}

	ackPath, err := e.ackPath()
	if err != nil {
		return err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: ackPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("node: open the updater's answer channel: %w", err)
	}

	// Close unlinks the socket on every exit, including a failed launch.
	defer func() { _ = listener.Close() }()

	if err := e.launch(binary, upgradeArgs(spec, e.ConfigPath, ackPath), spec); err != nil {
		return err
	}

	if err := listener.SetDeadline(time.Now().Add(ackWait)); err != nil {
		return fmt.Errorf("node: bound the wait for the updater to connect: %w", err)
	}

	conn, err := listener.AcceptUnix()
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return ackTimeout(spec.Version)
		}

		return fmt.Errorf("node: accept the updater's answer: %w", err)
	}

	return awaitAck(conn, spec.Version)
}

// Check refuses a state-directory path that cannot carry an upgrade answer.
// The CLI calls this before starting the node or its candidate probe.
func (e ExecUpgrader) Check() error {
	_, err := e.ackPath()

	return err
}

// ackPath bounds the absolute pathname, including the random name and separator.
func (e ExecUpgrader) ackPath() (string, error) {
	if e.AckDir == "" {
		return "", errors.New("node: the updater's answer needs the node's state directory")
	}

	path, err := filepath.Abs(filepath.Join(e.AckDir, "upgrade-ack-"+rand.Text()))
	if err != nil {
		return "", fmt.Errorf("node: resolve the updater's answer path: %w", err)
	}

	// Measured with ListenUnix on macOS and Linux, 2026-09-06. The pathname
	// needs one byte for its terminator in sockaddr_un.sun_path.
	limit := 107
	if runtime.GOOS == "darwin" {
		limit = 103
	}

	if len(path) > limit {
		return "", fmt.Errorf("node.state_dir makes the upgrade answer socket path %d bytes; "+
			"the maximum on %s is %d; use a shorter node.state_dir path", len(path), runtime.GOOS, limit)
	}

	return path, nil
}

// underSystemd and systemdRun are seams for exercising both launch shapes.
// Tests replacing either must stay serial, as must tests replacing ackWait.
var (
	underSystemd = func() bool { return runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != "" }
	systemdRun   = "systemd-run"
)

// launch escapes systemd's mount namespace and cgroup through the system manager.
// A setsid child escapes a launch agent, but remains inside a systemd service.
func (e ExecUpgrader) launch(binary string, args []string, spec nodeapi.UpgradeSpec) error {
	if underSystemd() {
		// No CommandContext: the transaction must survive the node's shutdown.
		//nolint:noctx,gosec // the updater must outlive the node; systemdRun is a fixed binary name, variable only for tests
		cmd := exec.Command(systemdRun, e.systemdRunArgs(binary, args, spec)...)

		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("node: start the updater for %s through systemd-run: %w: %s",
				spec.Version, err, strings.TrimSpace(string(out)))
		}

		return nil
	}

	cmd := exec.Command(binary, args...) //nolint:noctx // the updater must outlive the node
	cmd.SysProcAttr = detachedAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("node: start the updater for %s: %w", spec.Version, err)
	}

	// Release does not reparent a child. Reap refusals while the node keeps serving.
	go func() { _ = cmd.Wait() }() //nolint:errcheck // reaping is the purpose; the outcome arrives on the answer channel

	return nil
}

// systemdRunArgs names one unit per instruction, so a redelivery cannot run beside it.
func (e ExecUpgrader) systemdRunArgs(binary string, args []string, spec nodeapi.UpgradeSpec) []string {
	unit := fmt.Sprintf("billet-host-upgrade-%s-g%d", spec.RolloutID, spec.Generation)
	prefix := make([]string, 0, 13+len(args))
	prefix = append(prefix,
		"--unit="+unit,
		"--description=billet host upgrade to "+spec.Version,
		"--service-type=oneshot", "--collect", "--quiet", "--no-block",
		// Match the scheduled root updater's largest drain plus teardown and margin.
		"--property=TimeoutStartSec=88200",
	)

	if e.DSNEnv != "" {
		// With no '=value', systemd-run copies its own environment variable over
		// the bus. Only the configured name is visible in argv, never the DSN.
		prefix = append(prefix, "--setenv="+e.DSNEnv)
	}

	// Candidate probes on a combined-role host may read the App key from SSM.
	// Preserve explicit credentials so losing them cannot select an instance role.
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, present := os.LookupEnv(name); present {
			prefix = append(prefix, "--setenv="+name)
		}
	}

	prefix = append(prefix, "--", binary)

	return append(prefix, args...)
}
