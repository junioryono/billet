package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// startPoll is how often billet asks whether launchd has given a service a
// process yet, and DefaultStartWindow bounds that wait.
//
// The window is generous because what it waits for is launchd's own scheduling
// rather than billet's: an agent is bootstrapped, launchd accepts it, and the
// process appears afterwards. Too short reports a healthy service as broken —
// and on this manager that unwinds, which removes the agent just installed.
const (
	startPoll          = 250 * time.Millisecond
	DefaultStartWindow = 30 * time.Second
)

// DefaultStabilityWait is how long `up` watches a service after starting it.
//
// The same reason as the systemd side's: a process that starts and then dies is
// restarted by KeepAlive, and at any single instant that looks like a service
// which is running. It has to be longer than ThrottleInterval (5s in both
// shipped agents) or the sample lands during launchd's own back-off, when there
// is no process to see and nothing has gone wrong.
const DefaultStabilityWait = 8 * time.Second

// agentOf maps a label to the agent billet ships for it.
func agentOf(label string) (string, bool) {
	switch label {
	case deploy.ServerAgentLabel:
		return deploy.ServerAgent, true
	case deploy.NodeAgentLabel:
		return deploy.NodeAgent, true
	case deploy.UpgradeAgentLabel:
		return deploy.UpgradeAgent, true
	case deploy.ImagesAgentLabel:
		return deploy.ImagesAgent, true
	}

	return "", false
}

// Scheduled names billet's two oneshot agents, the upgrade first.
//
// NOT SERVICES. Neither holds a process between runs, so nothing here starts,
// proves or drains them; they are installed and enabled so launchd runs them on
// their schedule, and removed with the rest at uninstall.
func (c *Converger) Scheduled() (string, string) {
	return deploy.UpgradeAgentLabel, deploy.ImagesAgentLabel
}

// AgentPath is where a label's plist lives, for a caller that preserves or
// restores it.
func (c *Converger) AgentPath(label string) string { return c.agentPath(label) }

// Identity is the account the launch agents run as: the one invoking this.
//
// THERE IS NO SERVICE ACCOUNT ON macOS, and that is the whole shape of this
// backend. billet ships launch AGENTS rather than daemons because
// Virtualization.framework needs an unlocked login keychain and tart's image
// store is per-user, so the services run inside a person's GUI session as that
// person. Nothing is chowned to a `billet` account, because none exists.
//
// ROOT IS REFUSED. `sudo billet local up` would target root's domain — no
// unlocked keychain, no tart images — and everything would then fail for reasons
// naming none of that.
func (c *Converger) Identity(lifeops.UpRequest) (int, int, error) {
	uid, gid := os.Getuid(), os.Getgid()

	if uid == 0 {
		return 0, 0, errors.New("launchd: billet's macOS services are launch AGENTS, which live " +
			"in a logged-in user's GUI domain — running this as root would target root's domain, " +
			"where there is no unlocked login keychain for Virtualization.framework and none of " +
			"the tart images you pulled. Run it as the account that will run the node, without sudo")
	}

	return uid, gid, nil
}

// Running reports what each wanted service is executing right now.
func (c *Converger) Running(ctx context.Context,
	req lifeops.UpRequest,
) ([]lifeops.RunningFacts, error) {
	server, node := c.Services()

	var facts []lifeops.RunningFacts

	for _, s := range []struct {
		want  bool
		label string
	}{{req.WantServer, server}, {req.WantNode, node}} {
		if !s.want {
			continue
		}

		fact, err := c.runningFacts(ctx, s.label)
		if err != nil {
			return nil, err
		}

		facts = append(facts, fact)
	}

	return facts, nil
}

// RunningOne answers what one label is executing.
func (c *Converger) RunningOne(ctx context.Context, label string) (lifeops.RunningFacts, error) {
	return c.runningFacts(ctx, label)
}

// runningFacts answers what one label is executing.
//
// WHETHER IT IS THIS BUILD IS ANSWERED BY THE PROGRAM launchd LOADED, and only
// when billet can be sure. `launchctl print` reports the program of the job as
// LOADED, which is what an agent bootstrapped before an upgrade is still
// running — precisely the case worth catching. But a path is not an identity: a
// replacement at the same path is a different build under the same name, so a
// matching path is Unknown rather than Yes unless it is the same FILE as the
// running billet.
func (c *Converger) runningFacts(ctx context.Context, label string) (lifeops.RunningFacts, error) {
	facts := lifeops.RunningFacts{Name: label, Active: lifeops.Unknown}

	job, loaded, err := c.job(ctx, label)
	if err != nil {
		return facts, err
	}

	if !loaded {
		facts.Active = lifeops.No
		facts.Why = "launchd does not have this service"

		return facts, nil
	}

	// A PID IS THE EVIDENCE. `state` is launchd's intent as much as its
	// observation — `spawn scheduled` means it means to start one — so a job
	// with no pid is not running whatever the state says, and one WITH a pid is,
	// including while it drains.
	if job.Running() {
		facts.Active = lifeops.Yes
	} else {
		facts.Active = lifeops.No
	}

	facts.IsThisBuild, facts.Why = c.sameBuild(job)

	return facts, nil
}

// sameBuild compares the program launchd loaded against the running billet.
func (c *Converger) sameBuild(job Job) (lifeops.Tristate, string) {
	if job.Program == "" {
		return lifeops.Unknown, "launchd did not say which program this service runs"
	}

	self, err := c.selfPath()
	if err != nil {
		return lifeops.Unknown, fmt.Sprintf("billet cannot tell which file it is running from (%v)", err)
	}

	selfInfo, err := os.Stat(self)
	if err != nil {
		return lifeops.Unknown, fmt.Sprintf("billet cannot stat its own binary (%v)", err)
	}

	loadedInfo, err := os.Stat(job.Program)
	if err != nil {
		// The program a running service was started from can be gone — replaced
		// by an upgrade — which is not a mismatch. It is the case where billet
		// genuinely cannot tell, and the one this must not call a match.
		return lifeops.Unknown, fmt.Sprintf("the service runs %s, which billet cannot stat (%v)",
			job.Program, err)
	}

	// SAME FILE, not same path. A binary replaced at the same path is a
	// different build wearing the same name, and it is the ordinary way a host
	// ends up running something nobody installed there.
	if os.SameFile(selfInfo, loadedInfo) {
		return lifeops.Yes, "the service runs this same billet binary"
	}

	return lifeops.No, fmt.Sprintf("the service runs %s, which is a different file from this "+
		"billet (%s)", job.Program, self)
}

// selfPath is the executable this process was started from.
func (c *Converger) selfPath() (string, error) {
	if c.self != nil {
		return c.self()
	}

	return os.Executable()
}

// Snapshot renders what a service looks like now, for the before-and-after
// comparison that catches billet disturbing a service it did not start.
//
// WHAT IT COMPARES IS OPAQUE ON PURPOSE. The caller only asks whether the string
// changed, so this can carry whatever this manager makes observable without the
// shared command learning any of it.
func (c *Converger) Snapshot(ctx context.Context, label string) (string, error) {
	job, loaded, err := c.job(ctx, label)
	if err != nil {
		return "", err
	}

	if !loaded {
		return "not loaded", nil
	}

	return fmt.Sprintf("%s (pid %s, %s runs)",
			orNone(job.State), orNoNumber(job.PID, job.PIDKnown), orNoNumber(job.Runs, job.RunsKnown)),
		nil
}

// ProveStable establishes that a service is still the same process after a
// settle window.
//
// STABILITY IS NOT READINESS, and the difference is stated wherever this is
// reported. launchd has no sd_notify: nothing here says the process finished
// initialising, registered with the control plane, or can serve work. What it
// says is that one process survived the window — which rules out a crash loop
// and nothing else.
func (c *Converger) ProveStable(ctx context.Context, label string) error {
	before, err := c.awaitProcess(ctx, label)
	if err != nil {
		return err
	}

	if !c.sleep(ctx, c.stabilityWait) {
		return fmt.Errorf("launchd: watching %s: %w", label, ctx.Err())
	}

	after, err := c.sample(ctx, label)
	if err != nil {
		return err
	}

	switch {
	case !after.up():
		return fmt.Errorf("launchd: %s started and is %s %s later; it is not staying up",
			label, after, c.stabilityWait)

	// THE SAME PROCESS, not merely A process. KeepAlive restarts a service that
	// exits non-zero, so a crash loop presents a live pid at almost any instant
	// — a different one each time. The pid changing IS the crash loop.
	case after.pid != before.pid:
		return fmt.Errorf("launchd: %s was pid %d and is now pid %d, so it restarted within %s; "+
			"launchd's KeepAlive is restarting it and it is not staying up",
			label, before.pid, after.pid, c.stabilityWait)

	case after.runs != before.runs:
		return fmt.Errorf("launchd: %s has been started %d times, up from %d, within %s",
			label, after.runs, before.runs, c.stabilityWait)
	}

	return nil
}

// awaitProcess waits for launchd to name a process for a service it has been
// asked to start.
//
// A bootstrap IS NOT A SPAWN. `launchctl bootstrap` returns once launchd has
// accepted the job, and launchd starts the process afterwards — measured, a
// freshly bootstrapped agent reports `spawn scheduled` with no pid, and this
// package's own real-launchd helper polls for a pid before it will trust one.
// Production sampled once and concluded there was nothing to prove.
//
// THE CONSEQUENCE WAS NOT A BAD MESSAGE. launchd's plan sets EnableBeforeStart,
// so a failure here unwinds — and unwinding an install REMOVES the agent. A
// healthy Mac's first `up` would have failed and deleted the plist it had just
// written, every time, for no reason but arriving a moment early.
func (c *Converger) awaitProcess(ctx context.Context, label string) (stability, error) {
	// BOUNDED, and by billet's own window rather than the caller's. A service
	// that never gets a process is a failure to report now, not something to
	// wait on until an operator's patience or a shutdown ends it.
	ctx, cancel := context.WithTimeout(ctx, c.startWindow)
	defer cancel()

	var last stability

	for {
		got, err := c.sample(ctx, label)
		if err != nil {
			return stability{}, err
		}

		if got.up() {
			return got, nil
		}

		last = got

		// A SERVICE THAT HAS ALREADY GIVEN UP IS NOT WORTH WAITING FOR. launchd
		// records the previous run's exit, so a program that cannot start at all
		// — a mistyped path exits 78 — is known now rather than at the end of
		// the window.
		if !got.loaded {
			return stability{}, fmt.Errorf("launchd: %s left its domain instead of starting", label)
		}

		if !c.sleep(ctx, startPoll) {
			return stability{}, fmt.Errorf("launchd: %s is %s and launchd never gave it a "+
				"process: %w", label, last, ctx.Err())
		}
	}
}

// stability is one observation of a service.
type stability struct {
	loaded bool
	state  string
	pid    int
	runs   int
	// lastExit is what the previous run exited with, which is what says WHY a
	// service that is not up is not up.
	lastExit      int
	lastExitKnown bool
}

func (s stability) up() bool { return s.loaded && s.pid > 0 }

func (s stability) String() string {
	if !s.loaded {
		return "not loaded"
	}

	if s.pid > 0 {
		return fmt.Sprintf("running as pid %d", s.pid)
	}

	if s.lastExitKnown {
		return fmt.Sprintf("%s with no process (last exit %d)", orNone(s.state), s.lastExit)
	}

	return fmt.Sprintf("%s with no process", orNone(s.state))
}

func (c *Converger) sample(ctx context.Context, label string) (stability, error) {
	job, loaded, err := c.job(ctx, label)
	if err != nil {
		return stability{}, err
	}

	return stability{
		loaded:        loaded,
		state:         job.State,
		pid:           job.PID,
		runs:          job.Runs,
		lastExit:      job.LastExit,
		lastExitKnown: job.LastExitKnown,
	}, nil
}

// ApplyOwnership corrects the files `up` planned to correct.
//
// ON macOS THERE IS USUALLY NOTHING TO CORRECT: the agent runs as the operator,
// so the files they created are already readable by the service. What this must
// NOT do is chown — these commands refuse to run as root, so a file owned by
// somebody else is a refusal the plan already made rather than something to fix
// here.
func (c *Converger) ApplyOwnership(changes []lifeops.OwnershipChange, _, _ int) error {
	if len(changes) == 0 {
		return nil
	}

	return fmt.Errorf("launchd: billet planned to change the ownership of %d file(s), which it "+
		"cannot do without root — and these commands refuse to run as root because a launch "+
		"agent lives in a logged-in user's domain. This is a bug in the plan rather than in the "+
		"host", len(changes))
}

// RepairServerState has nothing to repair on macOS.
//
// The Linux one exists because `billet check` opens the ledger as the invoking
// process — root, under sudo — and leaves files the unprivileged server cannot
// write. Here the preflight and the agent are the same account, so the files it
// creates are already the agent's.
func (c *Converger) RepairServerState(string, int, int) ([]string, error) { return nil, nil }

// RepairPaths has nothing to repair either, for the same reason: a restore run
// here writes as the operator, and the agent runs as the operator.
func (c *Converger) RepairPaths(string, []lifeops.RepairTarget, int, int) ([]string, error) {
	return nil, nil
}

// Revalidate asks again, immediately before acting.
//
// THE PLAN IS OLD BY THE TIME IT IS USED. `billet check` spends as long on the
// network as the network takes, and in that time an agent can be booted out,
// disabled, or replaced. Acting on the older answer is how a converge starts a
// service it did not check.
func (c *Converger) Revalidate(ctx context.Context, req lifeops.UpRequest,
	want lifeops.UnitPlan,
) error {
	plan, err := c.Plan(ctx, req)
	if err != nil {
		return fmt.Errorf("look at %s again before acting on it: %w", want.Name, err)
	}

	if len(plan.Refusals) > 0 {
		return fmt.Errorf("%s is no longer a service billet will act on: %s",
			want.Name, plan.Refusals[0].Error())
	}

	for _, now := range plan.Units {
		if now.Name != want.Name {
			continue
		}

		if now != want {
			return fmt.Errorf("%s changed while billet was checking GitHub; nothing was done "+
				"to it. Run this again", want.Name)
		}

		return nil
	}

	return fmt.Errorf("%s is no longer in the plan billet made for this host; run this again",
		want.Name)
}

// accountName renders a uid for a diagnostic.
func accountName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}

	return "uid " + strconv.Itoa(uid)
}

// orNone renders an empty answer as what it is.
func orNone(v string) string {
	if v == "" {
		return "(no answer)"
	}

	return v
}

// orNoNumber renders a number launchd did not give as what it is, rather than
// as a confident zero.
func orNoNumber(v int, known bool) string {
	if !known {
		return "unknown"
	}

	return strconv.Itoa(v)
}

// agentPath is where this label's shipped agent belongs.
func (c *Converger) agentPath(label string) string {
	return filepath.Join(c.agentsDir, label+".plist")
}

// installedAgent reads the agent installed for a label, if there is one.
func (c *Converger) installedAgent(label string) (string, bool, error) {
	body, err := os.ReadFile(c.agentPath(label))

	switch {
	case err == nil:
		return string(body), true, nil
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("launchd: read the installed agent %s: %w",
			c.agentPath(label), err)
	}
}

// sameAgent reports whether an installed agent is the one this build ships.
func sameAgent(installed, shipped string) bool {
	return strings.TrimSpace(installed) == strings.TrimSpace(shipped)
}
