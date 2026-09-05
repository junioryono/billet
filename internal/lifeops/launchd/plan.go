package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/junioryono/billet/internal/lifeops"

	"golang.org/x/sys/unix"
)

// runningStates are the launchd states in which a job is up and staying up.
//
// AN ALLOWLIST, and a short one. `SIGTERMed` is a job whose process is alive and
// STOPPING — the launchd twin of systemd's `deactivating` — and `spawn
// scheduled` is one launchd intends to start and has not. Neither is a service
// that is running, and treating either as one lets `up` decide there is nothing
// to do about a node that is on its way out. Anything not listed is a state this
// build has no rule for, which refuses rather than being guessed at.
var runningStates = map[string]bool{"running": true}

// Plan is what `up` would do to this host.
func (c *Converger) Plan(ctx context.Context, req lifeops.UpRequest) (lifeops.UpPlan, error) {
	var plan lifeops.UpPlan

	plan.Refusals = append(plan.Refusals, c.hostRefusals(req)...)

	// THE SERVER FIRST, ALWAYS: a node that came up first has nothing to
	// register with.
	server, node := c.Services()

	for _, want := range []struct {
		wanted bool
		label  string
	}{{req.WantServer, server}, {req.WantNode, node}} {
		if !want.wanted {
			continue
		}

		unit, refusals, err := c.planOne(ctx, want.label)
		if err != nil {
			return lifeops.UpPlan{}, err
		}

		plan.Refusals = append(plan.Refusals, refusals...)

		if len(refusals) == 0 {
			plan.Units = append(plan.Units, unit)
		}
	}

	// launchd declares no StateDirectory, so what the server keeps its ledger in
	// is what the CONFIG says and nothing else.
	if req.WantServer {
		plan.ServerState = req.ServerStateDir
	}

	return plan, nil
}

// hostRefusals are the things wrong with this Mac rather than with a service.
func (c *Converger) hostRefusals(req lifeops.UpRequest) []lifeops.Refusal {
	var refusals []lifeops.Refusal

	if os.Getuid() == 0 {
		refusals = append(refusals, lifeops.Refusal{
			What: "this is running as root, and billet's macOS services are launch AGENTS that " +
				"live in a logged-in user's GUI domain",
			Remedy: "run it as the account that will run the node, without sudo — root's domain " +
				"has no unlocked login keychain for Virtualization.framework and none of the " +
				"tart images you pulled, so everything would start and then fail for reasons " +
				"naming none of that",
		})
	}

	// THE DIRECTORIES launchd WILL NOT MAKE. A launch agent declares no
	// StateDirectory the way a systemd unit does and creates nothing: an absent
	// log directory makes the agent fail to spawn with no log to say so, which
	// is the most confusing failure available here. /usr/local is root-owned on
	// a stock Mac and these commands refuse root, so each of these is a refusal
	// naming the command rather than something `up` does.
	for _, dir := range c.requiredDirs(req) {
		if err := c.writableDir(dir); err != nil {
			refusals = append(refusals, lifeops.Refusal{
				What:   fmt.Sprintf("%s is not a directory this account can write (%v)", dir, err),
				Remedy: c.makeDirCommand(dir),
			})
		}
	}

	return refusals
}

// makeDirCommand renders how to create a directory, asking for root ONLY where
// root is what is missing.
//
// A REMEDY THAT ASKS FOR MORE THAN IT NEEDS IS A BAD REMEDY. `/usr/local` is
// root-owned on a stock Mac, so those directories genuinely need `sudo` — but a
// path inside the operator's own home does not, and telling somebody to
// `sudo chown` their own home directory to themselves is advice that is at best
// a no-op and teaches them to paste sudo at anything billet prints.
// IT ASKS RATHER THAN PROBES. writableDir answers by creating and removing a
// file, which is the accurate way — mode bits alone miss group membership and
// ACLs. But this runs while RENDERING A STRING, including under `--dry-run`,
// whose promise is that nothing changed; a temporary file is a small change and
// still a change. unix.Access is approximate in the permissive direction, and
// being wrong here costs a slightly wrong suggestion rather than a wrong verdict.
func (c *Converger) makeDirCommand(dir string) string {
	if unix.Access(filepath.Dir(dir), unix.W_OK) == nil {
		return fmt.Sprintf("mkdir -p %s", dir)
	}

	return fmt.Sprintf("sudo mkdir -p %s && sudo chown %s %s",
		dir, accountName(os.Getuid()), dir)
}

// requiredDirs are the directories the agents and the config need.
func (c *Converger) requiredDirs(req lifeops.UpRequest) []string {
	dirs := []string{c.agentsDir, c.logDir}

	for _, path := range append([]string{req.ConfigPath}, req.KeyPaths...) {
		if path != "" {
			dirs = append(dirs, filepath.Dir(path))
		}
	}

	// AND WHERE THE BINARY LIVES. The upgrade agent runs as this account and
	// replaces /usr/local/bin/billet by renaming into its directory, which is
	// root-owned on a stock Mac; a refusal here, with the chown that fixes it,
	// beats a transaction that drains the node and then cannot land.
	for _, dir := range []string{req.ServerStateDir, req.NodeStateDir, req.NodeLockDir,
		req.BinaryDir} {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}

	return uniqueDirs(dirs)
}

// uniqueDirs sorts and de-duplicates, DROPPING any empty name.
//
// THE EMPTY NAME IS THE POINT. A field nobody filled in reads as "" and reaches
// here as a directory to check, and on a real Mac that produced a refusal about
// a directory named "" whose remedy was `sudo mkdir -p  && sudo chown
// junioryono ` — a command that is either nothing or something terrible
// depending on how it is pasted. The field in question was this package's own
// logDir, left unset by New(); the callers all guarded their inputs and the one
// value nobody thought to guard was the one that was wrong.
//
// So the guard is here, at the point every directory passes through, rather than
// at each caller — which is where it already was, and where it did not help.
func uniqueDirs(in []string) []string {
	sorted := make([]string, 0, len(in))

	for _, dir := range in {
		if dir != "" {
			sorted = append(sorted, dir)
		}
	}

	sort.Strings(sorted)

	out := make([]string, 0, len(sorted))

	for i, dir := range sorted {
		if i == 0 || dir != sorted[i-1] {
			out = append(out, dir)
		}
	}

	return out
}

// writableDir reports why a directory cannot hold what billet must put there.
func (c *Converger) writableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return errors.New("not a directory")
	}

	// THE ANSWER IS WHETHER THIS ACCOUNT CAN WRITE, which mode bits alone do not
	// give: group membership and ACLs both decide it. Asking the filesystem is
	// the only reliable way, so billet asks by creating and removing a file it
	// owns.
	probe, err := os.CreateTemp(dir, ".billet-writable-*")
	if err != nil {
		return err
	}

	name := probe.Name()

	if err := probe.Close(); err != nil {
		return err
	}

	return os.Remove(name)
}

// planOne decides what to do with one label.
func (c *Converger) planOne(ctx context.Context, label string) (lifeops.UnitPlan,
	[]lifeops.Refusal, error,
) {
	shipped, ok := agentOf(label)
	if !ok {
		return lifeops.UnitPlan{}, []lifeops.Refusal{{
			What:   fmt.Sprintf("%s is not a service billet ships", label),
			Remedy: "this is a bug in billet rather than in the host",
		}}, nil
	}

	unit := lifeops.UnitPlan{
		Name: label,
		// launchd cannot bootstrap a label carrying a disabled override, so on
		// this manager the commitment cannot follow the proof.
		EnableBeforeStart: true,
		Detail:            "will start at login (its agent is installed and not disabled)",
	}

	var refusals []lifeops.Refusal

	// AN INSTALLED AGENT THAT IS NOT THE ONE THIS BUILD SHIPS IS NOT REPLACED.
	// An operator edit is how somebody adds a PATH entry or a resource limit
	// this build does not know about, and clobbering it is a change they cannot
	// see. Nothing else on a Mac renders these files, so a difference is either
	// that edit or a stale billet — and both want a person.
	installed, present, err := c.installedAgent(label)
	if err != nil {
		return lifeops.UnitPlan{}, nil, err
	}

	if present && !sameAgent(installed, shipped) {
		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("%s is not the agent this billet ships", c.agentPath(label)),
			Remedy: fmt.Sprintf("compare them with `diff %s <(billet show-agent %s)`; keep yours, "+
				"or move it aside and run this again to install the shipped one",
				c.agentPath(label), label),
		})
	}

	job, loaded, err := c.job(ctx, label)
	if err != nil {
		return lifeops.UnitPlan{}, nil, err
	}

	if loaded {
		refusals = append(refusals, c.loadedRefusals(label, job, shipped)...)
	}

	// STARTING IS FOR A SERVICE THAT IS NOT RUNNING, and only a state billet
	// recognises as running counts. A job that is SIGTERMed has a live process
	// on its way out; deciding there is nothing to do would leave `up` reporting
	// a host is up while its node finishes draining and stops.
	unit.Start = !loaded || !runningStates[job.State] || !job.Running()

	if loaded && job.Running() && !runningStates[job.State] {
		refusals = append(refusals, lifeops.Refusal{
			What: fmt.Sprintf("%s is %s, which billet does not treat as running or as stopped",
				label, orNone(job.State)),
			Remedy: fmt.Sprintf("wait for it to settle and run this again, or take the host "+
				"down first with `billet local down`; check it with `launchctl print %s`",
				c.target(label)),
		})
	}

	enablement, err := c.EnabledNow(ctx, label)
	if err != nil {
		return lifeops.UnitPlan{}, nil, err
	}

	unit.Enable = enablement.Enabled == lifeops.No

	if enablement.Enabled == lifeops.Unknown {
		refusals = append(refusals, lifeops.Refusal{
			What:   fmt.Sprintf("billet cannot tell whether %s starts at login: %s", label, enablement.How),
			Remedy: "resolve the above and run this again",
		})
	}

	return unit, refusals, nil
}

// loadedRefusals compares the job launchd LOADED against the agent billet ships.
//
// THE LOADED JOB IS NOT ITS PLIST, and this is the measurement the whole backend
// turns on. launchd reads a plist ONCE, at bootstrap, and keeps what it read: a
// node can be running a stale ExitTimeOut while the file on disk is byte-equal
// to the one billet ships, and comparing the FILE would certify it. At the next
// logout launchd SIGKILLs that node five seconds into a drain it was allowed
// 88200 for, leaving guests running with their leases renewed by nobody.
//
// THE WHOLE ENVIRONMENT IS COMPARED, not a chosen field of it. The node resolves
// tart and softnet out of PATH, and tart's VM store is per-user and selected by
// TART_HOME — so a loaded job carrying a redirected HOME or TART_HOME inspects a
// DIFFERENT store, reports live guests absent, and the control plane frees their
// leases and sells the capacity again. Picking which variables matter is picking
// which of those a future agent may quietly add.
func (c *Converger) loadedRefusals(label string, job Job, shipped string) []lifeops.Refusal {
	want, err := declaredAgent(shipped)
	if err != nil {
		return []lifeops.Refusal{{
			What:   fmt.Sprintf("billet cannot read the agent it ships for %s: %v", label, err),
			Remedy: "this is a bug in billet rather than in the host",
		}}
	}

	var differ []string

	if job.Program != "" && len(want.Arguments) > 0 && job.Program != want.Arguments[0] {
		differ = append(differ, fmt.Sprintf("it runs %s rather than %s",
			job.Program, want.Arguments[0]))
	}

	if strings.Join(job.Arguments, "\x00") != strings.Join(want.Arguments, "\x00") {
		differ = append(differ, fmt.Sprintf("its arguments are %v rather than %v",
			job.Arguments, want.Arguments))
	}

	if job.ExitTimeoutKnown && want.ExitTimeoutKnown && job.ExitTimeout != want.ExitTimeout {
		differ = append(differ, fmt.Sprintf("launchd will SIGKILL it %ds after asking it to "+
			"stop, rather than the %ds this build's agent declares — a node draining a job "+
			"would be killed through the middle of it", job.ExitTimeout, want.ExitTimeout))
	}

	if diff := environmentDiff(job.Environment, want.Environment); diff != "" {
		differ = append(differ, "its environment differs: "+diff)
	}

	if len(differ) == 0 {
		return nil
	}

	return []lifeops.Refusal{{
		What: fmt.Sprintf("%s is loaded from a definition that is not the one this billet "+
			"ships (%s)", label, strings.Join(differ, "; ")),
		Remedy: "launchd keeps what it read when the service was bootstrapped, so replacing " +
			"the file changes nothing about the running job. Take the host down first — " +
			"`billet local down` drains it and proves the process gone — and then run this " +
			"again, which bootstraps the current agent",
	}}
}

// environmentDiff renders how two environments differ, ignoring the variables
// launchd adds to every job.
func environmentDiff(loaded, want map[string]string) string {
	var out []string

	for _, name := range sortedKeys(want) {
		if got := loaded[name]; got != want[name] {
			out = append(out, fmt.Sprintf("%s=%q, want %q", name, got, want[name]))
		}
	}

	// EXTRA VARIABLES COUNT. A loaded job carrying a TART_HOME the shipped agent
	// does not declare points the node at another VM store, and an inventory
	// taken there reports live guests absent.
	for _, name := range sortedKeys(loaded) {
		if launchdOwns[name] {
			continue
		}

		if _, ok := want[name]; !ok {
			out = append(out, fmt.Sprintf("%s=%q, which this build's agent does not set",
				name, loaded[name]))
		}
	}

	return strings.Join(out, ", ")
}

// launchdOwns are the variables launchd puts in every job's environment, which
// are not the agent's and are not evidence of anything.
var launchdOwns = map[string]bool{
	"XPC_SERVICE_NAME": true,
	"OSLogRateLimit":   true,
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
