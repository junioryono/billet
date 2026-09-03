package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// DefaultTimeout bounds one launchctl call. It is not a bound on a STOP: the
// process being stopped is a node that drains for as long as its jobs take, and
// the poll that waits for it is bounded by the caller's context alone, exactly
// as the systemd side is.
const DefaultTimeout = 30 * time.Second

// stopPoll is how often the stop proof re-asks. The barrier has already been
// waited on by the time anything is stopped, so a node still draining here is
// the exception rather than the rule.
const stopPoll = 500 * time.Millisecond

// The exit statuses launchctl uses to say "there is no such service", which are
// NOT the same number for the two verbs that can say it.
//
// Measured on macOS 26 against a label that had never existed: `print` answers
// 113 and `bootout` answers 3. Neither is documented anywhere billet could cite,
// and assuming one number covered both made a cleanup report that an agent "may
// still be loaded" every time it had already gone.
const (
	notLoaded     = 113
	nothingToBoot = 3
)

// defaultLogDir is where the shipped agents send stdout and stderr.
//
// It is a CONSTANT rather than a value derived from the config, because launchd
// performs no variable substitution: the path in the plist is a literal, and
// this has to be the same literal or billet checks a directory the agents do not
// use.
const defaultLogDir = "/usr/local/var/log/billet"

// Converger drives launchd for the shared lifecycle commands.
type Converger struct {
	launchctl string
	uid       int

	// run is a seam. Every launchctl call goes through it, so the command tests
	// can stage a host without one — and, more to the point, so the stop proof
	// can be exercised against a launchd that answers slowly, which is the case
	// that matters and which no real machine will reproduce on demand.
	run func(ctx context.Context, args []string) (string, int, error)
	// alive answers whether a pid is still a live process. A seam for the same
	// reason: the stop proof's whole job is to keep asking this after launchd
	// has stopped answering.
	alive func(pid int) bool

	// agentsDir is where a launch agent's plist has to be for launchd to load
	// it at login. A field so a test has one it can write to.
	agentsDir string
	// created is the labels whose agents THIS RUN installed. Disable consults it
	// because undoing an enablement means different things depending on how it
	// was made: removing a file this run wrote, or writing back an override it
	// cleared.
	created []string
	// cleared is the labels whose DISABLED OVERRIDE this run removed. Undoing
	// that means writing it back, which is a different act from removing a file
	// -- and an operator's deliberate `launchctl disable` is theirs, not
	// billet's to spend.
	cleared []string

	// logDir is where the shipped agents send stdout and stderr. launchd creates
	// neither the directory nor the files: an agent whose log directory is
	// missing fails to spawn, with no log to say so.
	logDir string

	// self is os.Executable behind a seam, so the "is the running service this
	// build" judgment can be exercised against a binary a test controls.
	self func() (string, error)

	timeout       time.Duration
	stabilityWait time.Duration
	startWindow   time.Duration
	sleep         func(context.Context, time.Duration) bool
}

// WithAgentsDir points at a different LaunchAgents directory, for a test.
func WithAgentsDir(dir string) Option {
	return func(c *Converger) { c.agentsDir = dir }
}

// WithLogDir points at a different log directory, for a test.
func WithLogDir(dir string) Option {
	return func(c *Converger) { c.logDir = dir }
}

// homeOf resolves an account's home from the ACCOUNT DATABASE rather than from
// $HOME.
//
// $HOME IS A VARIABLE ANYBODY CAN SET, and what depends on it here is which
// directory launchd scans at login. A shell with a redirected HOME would have
// billet install an agent into a directory nothing loads, bootstrap it by path
// so the run looks entirely successful, and leave a Mac that starts nothing at
// the next login -- with every surface reporting the service installed.
//
// Falling back to $HOME when the database cannot be read is deliberate: it is
// the only other answer available, and a wrong directory the operator can see
// beats refusing to run at all.
func homeOf(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}

	return os.Getenv("HOME")
}

// Option configures a Converger.
type Option func(*Converger)

// WithLaunchctl points at a different launchctl, for a test.
func WithLaunchctl(path string) Option {
	return func(c *Converger) { c.launchctl = path }
}

// New builds a Converger for the GUI domain of the calling user.
//
// THE CALLING USER, NEVER AN INFERRED ONE. A launch agent lives in a per-user
// GUI domain, and billet's agents must be in the domain of the person whose
// login keychain is unlocked and whose tart image store holds the images —
// which is why running these commands as root is refused elsewhere rather than
// silently retargeted here.
func New(opts ...Option) *Converger {
	c := &Converger{
		launchctl:     "launchctl",
		uid:           os.Getuid(),
		timeout:       DefaultTimeout,
		stabilityWait: DefaultStabilityWait,
		startWindow:   DefaultStartWindow,
		alive:         processAlive,
		sleep:         sleep,
		agentsDir:     filepath.Join(homeOf(os.Getuid()), "Library", "LaunchAgents"),
		logDir:        defaultLogDir,
	}

	c.run = c.exec

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Services names billet's two launch agents, server first.
func (c *Converger) Services() (string, string) {
	return deploy.ServerAgentLabel, deploy.NodeAgentLabel
}

// EnablementCmd renders what an operator runs to see a label's enablement.
//
// IT IS print-disabled RATHER THAN print, because enablement on launchd is not
// a property of the loaded job at all: it lives in a durable override database
// keyed by LABEL, which survives both a bootout and the removal of the plist.
// An operator sent to `launchctl print` would be told the service does not
// exist and learn nothing about why it will not start.
func (c *Converger) EnablementCmd(...string) string {
	return fmt.Sprintf("launchctl print-disabled gui/%d", c.uid)
}

// target renders the service target launchctl addresses a label by.
func (c *Converger) target(label string) string {
	return fmt.Sprintf("gui/%d/%s", c.uid, label)
}

// domain renders this user's GUI domain target.
func (c *Converger) domain() string { return fmt.Sprintf("gui/%d", c.uid) }

// exec runs launchctl and returns its output and exit status.
//
// A NON-ZERO EXIT IS NOT AN ERROR HERE, because launchctl uses exit status to
// answer questions — 113 means "no such service", which is a fact rather than a
// failure. Only a launchctl that could not be run at all is an error.
func (c *Converger) exec(ctx context.Context, args []string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// #nosec G204 -- launchctl is the configured path and every argument is
	// built here; nothing from a config file reaches argv.
	cmd := exec.CommandContext(ctx, c.launchctl, args...)

	// SEPARATE STREAMS, never combined — the same rule the systemd side follows.
	// `launchctl print` writes the job description to stdout and its narration
	// to stderr, so a combined buffer puts a sentence like "Try re-running the
	// command as root for richer errors" into the middle of what this package
	// then parses as a job.
	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// THE SAME LESSON THE TART BACKEND PAID FOR: CommandContext kills the
	// process it started and then Wait blocks until the output pipes close,
	// which anything that process spawned holds open. Without this, the timeout
	// above is advisory.
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), exit.ExitCode(), nil
	}

	if err != nil {
		return stdout.String(), 0, fmt.Errorf("launchd: %s %s: %w: %s",
			c.launchctl, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), 0, nil
}

// job asks launchd what it has LOADED for a label.
//
// Three answers, and they are deliberately not two: the job as launchd holds it,
// a definite absence, or an error. A caller that cannot tell absence from "could
// not ask" will eventually treat one as the other, and in this package both
// directions are harmful — a false absence lets a second node start on top of a
// live one, and a false presence blocks a host from ever coming up.
func (c *Converger) job(ctx context.Context, label string) (Job, bool, error) {
	out, code, err := c.run(ctx, []string{"print", c.target(label)})
	if err != nil {
		return Job{}, false, err
	}

	if code == notLoaded {
		return Job{}, false, nil
	}

	if code != 0 {
		return Job{}, false, fmt.Errorf("launchd: `launchctl print %s` exited %d: %s",
			c.target(label), code, firstLine(out))
	}

	job, err := parsePrint(c.target(label), out)
	if err != nil {
		return Job{}, false, err
	}

	return job, true, nil
}

// StopResult is lifeops's, so the command layer stays one piece of code.
//
// StopAndProve stops a launch agent and PROVES its process is gone.
//
// launchd's OWN ANSWER IS NOT THE PROOF, and this is measured rather than
// cautious. A plain `launchctl bootout` returns in ZERO seconds against an agent
// that is still draining: the service stays in the domain reporting `state =
// SIGTERMed` with its pid, the process keeps running, and the record only
// disappears when the process finally exits. So the command's return says
// nothing at all, and `state` is launchd's intent as much as its observation.
//
// What billet does instead is capture the pid FIRST and then wait for two facts
// together: launchd no longer has the service, and that pid is no longer a live
// process. The second is what makes this independent of launchd's own
// bookkeeping — the same discipline the provider inventories follow, where an
// answer that shrank is never taken as proof on its own. Pid reuse can only make
// this answer "still running" about a pid that is somebody else's, which is the
// safe direction: it refuses to report a host down, rather than reporting one
// down while a job runs on it.
//
// bootout RATHER THAN `launchctl kill TERM`, and the difference is not stylistic.
// Both agents carry KeepAlive{SuccessfulExit: false}, and measured, `kill TERM`
// on an agent that exits non-zero makes launchd START IT AGAIN — the service
// billet was asked to stop, restarted by the stop. bootout removes it from the
// domain, so KeepAlive has nothing left to act on.
func (c *Converger) StopAndProve(ctx context.Context, label string) (lifeops.StopResult, error) {
	before, loaded, err := c.job(ctx, label)
	if err != nil {
		return lifeops.StopResult{Gone: lifeops.Unknown, How: "could not be asked about"}, err
	}

	if !loaded {
		// NOTHING IN THE DOMAIN TO STOP, and that is the honest scope of this
		// answer: launchd is not running this SERVICE. It is not a claim that no
		// billet process exists on the host — a node started by hand from a
		// terminal is not a launch agent and was never in scope here, exactly as
		// it is not in scope for `systemctl stop` on Linux. What protects a job
		// held by such a process is upstream: `down` waits on the quiescence
		// barrier before it stops anything, and says out loud what that barrier
		// cannot see.
		return lifeops.StopResult{Gone: lifeops.Yes, How: "is not loaded"}, nil
	}

	// EVERY PID THIS LABEL IS EVER SEEN WITH, not just the one before the
	// bootout. If launchd starts the job again while this polls — a bootout that
	// failed leaves KeepAlive in place — the pid changes, and following only the
	// first one would prove a process gone that has already been replaced. All
	// of them must be gone.
	watched := map[int]bool{}
	watch := func(j Job) {
		if j.PIDKnown && j.PID > 0 {
			watched[j.PID] = true
		}
	}

	watch(before)

	// A BOOTOUT THAT DID NOT SUCCEED IS NOT A STOP. `run` reports a non-zero
	// exit separately from an error precisely so it can be acted on, and
	// ignoring it here meant a refused bootout — a permission problem, a domain
	// that does not exist — fell through into a poll that would then wait for a
	// process nobody had asked to stop.
	out, code, err := c.run(ctx, []string{"bootout", c.target(label)})
	if err != nil {
		return lifeops.StopResult{Gone: lifeops.Unknown, How: "could not be booted out"}, err
	}

	if code != 0 {
		return lifeops.StopResult{
				Gone: lifeops.Unknown,
				How:  fmt.Sprintf("could not be booted out (launchctl exited %d)", code),
			}, fmt.Errorf("launchd: `launchctl bootout %s` exited %d: %s",
				c.target(label), code, firstLine(out))
	}

	for {
		now, stillLoaded, err := c.job(ctx, label)
		if err != nil {
			return lifeops.StopResult{
				Gone: lifeops.Unknown,
				How:  "could not be asked about after being booted out",
			}, err
		}

		watch(now)

		alive := c.anyAlive(watched)

		switch {
		case stillLoaded:
			// Still in the domain: draining, restarting, or launchd has not
			// finished. Either way a process may be alive and this is not down.

		case alive > 0:
			// launchd has let go and a process has NOT. This is the window the
			// independent proof exists for.

		case len(watched) > 0:
			return lifeops.StopResult{
				Gone: lifeops.Yes,
				How:  fmt.Sprintf("is out of its domain and pid %s is gone", pids(watched)),
			}, nil

		default:
			// It was loaded but launchd never named a pid, so there is no
			// process to follow. Its absence from the domain is all the evidence
			// there is, and billet says exactly that rather than more.
			return lifeops.StopResult{
				Gone: lifeops.Yes,
				How:  "is out of its domain (launchd named no process for it)",
			}, nil
		}

		if !c.sleep(ctx, stopPoll) {
			// THE WAIT ENDED WITHOUT A STOP. Reporting anything but uncertainty
			// here would tell an operator walking away that a host holding a
			// running job is down.
			//
			// The cause is READ rather than assumed: a caller's cancellation is
			// the reason in production, and wrapping ctx.Err() unconditionally
			// rendered `%!w(<nil>)` on any other path — an error message about
			// a live process that says nothing at all.
			result := lifeops.StopResult{
				Gone: lifeops.Unknown,
				How:  c.stillThere(label, watched, alive),
			}

			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("launchd: stopped waiting for %s before its process "+
					"was proved gone: %w", label, err)
			}

			return result, fmt.Errorf("launchd: stopped waiting for %s before its process was "+
				"proved gone; it %s", label, result.How)
		}
	}
}

// anyAlive counts how many of the watched pids are still live processes.
func (c *Converger) anyAlive(watched map[int]bool) int {
	n := 0

	for pid := range watched {
		if c.alive(pid) {
			n++
		}
	}

	return n
}

// pids renders a watched set in a stable order, for a diagnostic.
func pids(watched map[int]bool) string {
	out := make([]int, 0, len(watched))
	for pid := range watched {
		out = append(out, pid)
	}

	sort.Ints(out)

	parts := make([]string, len(out))
	for i, pid := range out {
		parts[i] = strconv.Itoa(pid)
	}

	return strings.Join(parts, ", ")
}

// stillThere describes what was true when a wait was abandoned.
func (c *Converger) stillThere(label string, watched map[int]bool, alive int) string {
	if alive > 0 {
		return fmt.Sprintf("was still stopping, with %d of its processes alive (%s)",
			alive, pids(watched))
	}

	return fmt.Sprintf("was still in its domain (%s)", label)
}

// processAlive reports whether a pid names a live process.
//
// SIGNAL 0 IS THE QUESTION, not an action: it performs the permission and
// existence checks and delivers nothing. EPERM means the process exists and
// belongs to somebody else, which for this package's purposes is "alive" — a
// launch agent's node runs as the same user, so EPERM here means the pid was
// reused by another account's process, and answering "alive" refuses to declare
// the host down rather than declaring it down wrongly.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// sleep waits, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// disabledLabels reads launchd's durable override database.
//
// THE DATABASE IS THE SECOND HALF OF ENABLEMENT, and it is invisible from
// everything else. It is keyed by LABEL, and measured, it survives a bootout AND
// the removal of the plist entirely — `launchctl print-disabled` on a real Mac
// lists labels belonging to software uninstalled long ago. It also accepts a
// label that has never existed. So a `disable` left behind by an uninstall is a
// landmine: the next install bootstraps a service launchd refuses to run, with
// the same useless `Bootstrap failed: 5: Input/output error` it gives for every
// other reason.
func (c *Converger) disabledLabels(ctx context.Context) (map[string]bool, error) {
	out, code, err := c.run(ctx, []string{"print-disabled", c.domain()})
	if err != nil {
		return nil, err
	}

	if code != 0 {
		return nil, fmt.Errorf("launchd: `launchctl print-disabled %s` exited %d: %s",
			c.domain(), code, firstLine(out))
	}

	return parseDisabled(out)
}

// parseDisabled reads the `"label" => disabled` lines print-disabled emits.
//
// EVERY LINE IS ACCOUNTED FOR, and a reply this cannot fully read is an error.
// The first version skipped whatever it did not understand, so a truncated
// reply, a value launchd added later, or a label it could not unquote all came
// back as "not disabled" — and "not disabled" is what authorises `up` to start
// a service. A parser whose failure mode is a permission is the wrong way round.
func parseDisabled(out string) (map[string]bool, error) {
	seen := map[string]bool{}

	var opened, closed bool

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)

		switch {
		case line == "":
			continue

		case strings.HasSuffix(line, "{"):
			if opened {
				return nil, fmt.Errorf("launchd: `launchctl print-disabled` opened a second "+
					"block at %q, which billet does not understand", line)
			}

			opened = true

			continue

		case line == "}":
			closed = true

			continue
		}

		if !opened || closed {
			return nil, fmt.Errorf("launchd: `launchctl print-disabled` has %q outside the "+
				"list of services", line)
		}

		name, value, ok := strings.Cut(line, " => ")
		if !ok {
			return nil, fmt.Errorf("launchd: billet cannot read %q in `launchctl print-disabled`",
				line)
		}

		label, err := strconv.Unquote(strings.TrimSpace(name))
		if err != nil {
			return nil, fmt.Errorf("launchd: `launchctl print-disabled` named a service as %q, "+
				"which billet cannot read: %w", name, err)
		}

		// ONLY THESE TWO WORDS. The database lists labels in BOTH states, so a
		// reader that took every listed label for a disabled one would refuse to
		// start every service anybody had ever enabled by hand — and one that
		// took an unrecognised word for "enabled" would start a service launchd
		// will not run.
		switch strings.TrimSpace(value) {
		case "disabled":
			seen[label] = true
		case "enabled":
			seen[label] = false
		default:
			return nil, fmt.Errorf("launchd: `launchctl print-disabled` says %s is %q, which "+
				"billet does not understand", label, strings.TrimSpace(value))
		}
	}

	if !opened || !closed {
		return nil, errors.New("launchd: `launchctl print-disabled` was cut off before it " +
			"finished listing services, so a label missing from it proves nothing")
	}

	return seen, nil
}

// EnabledNow reports whether a label will start itself at the next login.
//
// TWO FACTS, AND THE SECOND IS INVISIBLE. A launch agent starts at login when
// its plist is in ~/Library/LaunchAgents AND its label is not disabled in
// launchd's override database. That database is durable, keyed by LABEL, and
// measured, it survives both a bootout and the removal of the plist entirely —
// `launchctl print-disabled` on a real Mac lists labels belonging to software
// uninstalled years ago, and it accepts a label that has never existed.
//
// So "the plist is there" is not enablement, and neither is "launchd knows the
// job". A disabled label with a perfectly good plist refuses to bootstrap at
// all, with the same `Bootstrap failed: 5: Input/output error` launchd gives for
// every other reason — which is why billet reads the database rather than
// interpreting that error.
func (c *Converger) EnabledNow(ctx context.Context, label string) (lifeops.Enablement, error) {
	disabled, err := c.disabledLabels(ctx)
	if err != nil {
		return lifeops.Enablement{}, err
	}

	// A DEFINITE NO. The operator, or a previous uninstall, has disabled this
	// label; the plist is irrelevant until that is cleared.
	if disabled[label] {
		return lifeops.Enablement{Enabled: lifeops.No, How: "disabled"}, nil
	}

	switch _, err := os.Stat(c.agentPath(label)); {
	case err == nil:
		return lifeops.Enablement{Enabled: lifeops.Yes, How: "enabled"}, nil

	case errors.Is(err, os.ErrNotExist):
		// Nothing to start at login. A definite no, and a different one from
		// `disabled` — the remedy is a plist rather than `launchctl enable`.
		return lifeops.Enablement{Enabled: lifeops.No, How: "not installed"}, nil

	default:
		// COULD NOT TELL, which is neither. A directory billet cannot read says
		// nothing about whether the agent is there, and answering "not
		// installed" would make `up` write over whatever is.
		return lifeops.Enablement{
			Enabled: lifeops.Unknown,
			How:     fmt.Sprintf("could not be read (%v)", err),
		}, nil
	}
}

// DisableCmd renders how an operator disables a label themselves.
func (c *Converger) DisableCmd(label string) string {
	return fmt.Sprintf("launchctl disable gui/%d/%s", c.uid, label)
}

// ManagerName is what billet calls this service manager in a sentence.
func (c *Converger) ManagerName() string { return "launchd" }

// CollateralNote explains how enabling one service could commit another.
//
// launchd HAS NO SUCH MECHANISM, which is worth saying rather than leaving the
// sentence systemd-shaped. There is no `[Install] Also=` here: a label is its
// own entry in the override database and its own file. The shared command still
// compares both services before and after, because a check that costs nothing
// and can only report a real change is worth keeping even where the mechanism it
// was written for does not exist.
func (c *Converger) CollateralNote() string {
	return "launchd has no mechanism that enables one service alongside another, so this is " +
		"something billet did not do and cannot explain"
}
