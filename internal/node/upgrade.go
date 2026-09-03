package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// StartUpgrade launches the updater and returns as soon as it is running.
//
// IT DOES NOT WAIT, AND THAT IS THE WHOLE CONTRACT. A node executes commands one
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
func upgradeArgs(spec nodeapi.UpgradeSpec, configPath string) []string {
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

	// FD 3 IS WHERE THE UPDATER ANSWERS; see AckFD.
	args = append(args, "--ack-fd", strconv.Itoa(AckFD))

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
// NO TEST THAT SWAPS THIS MAY CALL t.Parallel, and three of them swap it. They
// are safe today by construction rather than by argument: a parallel test parks
// at its t.Parallel() call until every serial test has finished, so a serial
// test's mutation window cannot overlap a parallel test's read. EVERY OTHER TEST
// IN upgradeack_test.go IS PARALLEL, which makes adding t.Parallel() to one of
// the three the most natural-looking edit available and a data race the moment
// it lands.
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
// THE DEADLINE IS NOT DECORATION. A descriptor passed through ExtraFiles is NOT
// close-on-exec in the child, so anything the updater execs before it answers
// inherits this pipe and holds it open past the updater's own exit — at which
// point EOF never comes. Nothing in the updater spawns a process before it
// answers, and this is what bounds the damage if something ever does.
func awaitAck(r *os.File, version string) error {
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
			return fmt.Errorf("%w: it did not say within %s whether it had taken the job; "+
				"nothing here can tell whether it is installing %s or stuck, so look at "+
				"/var/lib/billet/upgrades on this machine", ErrUpgradeRefused, ackWait, version)
		}

		if line != "" {
			return fmt.Errorf("%w: it stopped partway through its answer (%q)",
				ErrUpgradeRefused, line)
		}

		return fmt.Errorf("%w: it stopped without saying why", ErrUpgradeRefused)

	default:
		// QUOTED, AND SAFE TO QUOTE. This descriptor is inherited by a program this
		// node execs, not by anything a job can reach, and the updater bounds and
		// flattens what it writes.
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
	// AckFD is the child descriptor the answer travels on: the first after the
	// standard three, which is what ExtraFiles[0] becomes in the child.
	AckFD = 3
)

// ExecUpgrader runs billet's own updater as a detached process.
type ExecUpgrader struct {
	// Binary is the billet to exec. Empty means the running executable.
	Binary string
	// ConfigPath is the config the updater reads.
	ConfigPath string
}

// StartUpgrade execs `billet host-upgrade` and does not wait for it.
//
// DETACHED FROM THIS PROCESS'S CONTEXT, ON PURPOSE. The updater's whole job is to
// stop the very service that started it, so inheriting a context this node
// cancels on shutdown would kill the updater at the exact moment it succeeded —
// leaving a machine with both services stopped and nothing running that knows
// what to do about it. context.WithoutCancel is what breaks that link.
//
// Setpgid puts it in its own process group for the same reason: a signal sent to
// the node's group must not reach a transaction midway through replacing a
// binary.
func (e ExecUpgrader) StartUpgrade(ctx context.Context, spec nodeapi.UpgradeSpec) error {
	binary := e.Binary
	if binary == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("node: find this billet to run the updater: %w", err)
		}

		binary = self
	}

	args := upgradeArgs(spec, e.ConfigPath)

	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("node: open the updater's answer channel: %w", err)
	}

	defer func() { _ = writer.Close() }()

	// NOT CommandContext. The updater must outlive this process, and
	// CommandContext kills the child when the context ends — which for a node is
	// the moment the updater successfully stops it.
	cmd := exec.Command(binary, args...) //nolint:noctx // see the comment above: the updater must outlive this process
	cmd.SysProcAttr = detachedAttr()
	cmd.ExtraFiles = []*os.File{writer}

	if err := cmd.Start(); err != nil {
		_ = reader.Close()

		return fmt.Errorf("node: start the updater for %s: %w", spec.Version, err)
	}

	// REAPED IN THE BACKGROUND, AND Release WAS WRONG HERE.
	//
	// Release drops Go's handle on the process; it does not reparent it. The node
	// is still the parent, so an updater that exits leaves a zombie until the node
	// itself exits — which the original reasoning assumed was imminent, because a
	// SUCCESSFUL updater stops this very service. A REFUSED one does not: the node
	// keeps running, the rollout retries every few minutes, and the process table
	// fills with the corpses of updaters that declined.
	//
	// Waiting neither signals the child nor ties it to this process's lifetime, so
	// the detachment is unaffected; the goroutine simply sits until the updater
	// ends, which for a successful upgrade is after this node has been stopped.
	// THE STATUS IS NOT READ BECAUSE REAPING IS THE WHOLE PURPOSE. Whether the
	// updater succeeded is answered by the acknowledgement below and then by the
	// host's next registration; this only stops a refused one becoming a zombie.
	go func() { _ = cmd.Wait() }() //nolint:errcheck // reaping is the purpose; the outcome arrives on the answer channel

	// THIS END IS CLOSED BEFORE READING, or the read never sees EOF: the parent's
	// own copy of the write end would hold the pipe open forever and an updater
	// that died silently would look like one still thinking.
	_ = writer.Close()

	_ = ctx

	return awaitAck(reader, spec.Version)
}
