package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Enable makes a label start at the next login, and is what this backend has
// instead of `systemctl enable`.
//
// TWO THINGS ARE ENABLEMENT HERE, and both are done together on purpose. A
// launch agent starts at login when its plist is in ~/Library/LaunchAgents AND
// its label is not disabled in launchd's durable override database. Splitting
// them across two steps of `up` would make the command's own "is it enabled
// yet" reading flip to yes half way through its transaction — so `up` would
// refuse the second half of the work it had already begun.
//
// IT ALSO HAS TO HAPPEN BEFORE THE START, which no other backend needs: a label
// carrying a disabled override refuses to bootstrap at all. So on macOS the
// commitment precedes the proof, and what protects the host is Disable undoing
// exactly this — see UnitPlan.EnableBeforeStart.
func (c *Converger) Enable(ctx context.Context, label string) error {
	shipped, ok := agentOf(label)
	if !ok {
		return fmt.Errorf("launchd: %s is not a service billet ships", label)
	}

	// WHAT WAS TRUE BEFORE, recorded before anything changes it. Undoing this
	// means putting back what was there, and the two halves are undone
	// differently: an agent this run installed is removed, while an override this
	// run cleared has to be written again.
	disabled, err := c.disabledLabels(ctx)
	if err != nil {
		return err
	}

	if err := c.installAgent(label, shipped); err != nil {
		return err
	}

	if _, code, err := c.run(ctx, []string{"enable", c.target(label)}); err != nil {
		// THIS RUN'S INSTALL IS TAKEN BACK BY THIS RUN. The label is still
		// disabled, so nothing starts either way — but leaving the agent behind
		// with no record that this run put it there is the "installed, no undo"
		// state, and the caller cannot undo what it was never told about.
		return errors.Join(err, c.undoInstall(label))
	} else if code != 0 {
		return errors.Join(
			fmt.Errorf("launchd: `launchctl enable %s` exited %d", c.target(label), code),
			c.undoInstall(label))
	}

	if disabled[label] {
		c.cleared = append(c.cleared, label)
	}

	return nil
}

// undoInstall takes back an agent installed moments ago by the caller.
func (c *Converger) undoInstall(label string) error {
	if !c.installedThisRun(label) {
		return nil
	}

	if err := c.removeAgent(label); err != nil {
		return err
	}

	c.forgetInstall(label)

	return nil
}

// forgetInstall drops a label from what this run installed.
func (c *Converger) forgetInstall(label string) {
	kept := c.created[:0]

	for _, l := range c.created {
		if l != label {
			kept = append(kept, l)
		}
	}

	c.created = kept
}

// installAgent puts the shipped plist where launchd looks for it at login.
//
// WRITTEN COMPLETE, ELSEWHERE, AND PUBLISHED BY A LINK THAT CANNOT REPLACE. The
// destination is scanned by launchd, so a partially written file there is one it
// may read; and `O_EXCL` on the destination is race-safe but not crash-safe,
// because an interruption leaves the empty file it created. So the bytes go to a
// sibling temporary file, are fsynced, and are published with os.Link — which
// FAILS rather than replaces if something appeared meanwhile. That is the same
// shape billet uses for the App private key, and for the same reason: a
// pathname is not a promise.
//
// What this run created is remembered, so a failure later in the run can undo
// exactly what it did and nothing else.
func (c *Converger) installAgent(label, shipped string) error {
	path := c.agentPath(label)

	switch installed, present, err := c.installedAgent(label); {
	case err != nil:
		return err

	case present && sameAgent(installed, shipped):
		// Already the agent this build ships. Nothing to do, and nothing this
		// run may later undo.
		return nil

	case present:
		// The plan refuses this, so reaching it means the world changed since —
		// and replacing an operator's edited agent is not something to do on the
		// way past.
		return fmt.Errorf("launchd: %s is not the agent this billet ships, and billet will not "+
			"replace it. Move it aside and run this again", path)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".billet-agent-*")
	if err != nil {
		return fmt.Errorf("launchd: stage %s: %w", path, err)
	}

	staged := tmp.Name()

	defer func() { _ = os.Remove(staged) }()

	if err := writeAndSync(tmp, shipped); err != nil {
		return fmt.Errorf("launchd: stage %s: %w", path, err)
	}

	// PUBLISHED WITHOUT REPLACING. os.Link fails if the destination exists, so
	// an agent that appeared while this ran is reported rather than clobbered.
	if err := os.Link(staged, path); err != nil {
		return fmt.Errorf("launchd: install %s: %w", path, err)
	}

	// AND THE DIRECTORY ENTRY IS FLUSHED, because what makes this survive a
	// crash is the entry rather than the bytes — and the very next thing billet
	// does is clear an override that assumes this file is there.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("launchd: install %s: %w", path, err)
	}

	c.created = append(c.created, label)

	return nil
}

// writeAndSync writes a complete file and flushes it before it is published.
func writeAndSync(f *os.File, body string) error {
	defer func() { _ = f.Close() }()

	if err := f.Chmod(0o600); err != nil {
		return err
	}

	if _, err := f.WriteString(body); err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		return err
	}

	return f.Close()
}

// syncDir flushes a directory entry.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return err
	}

	return d.Close()
}

// Disable stops a label starting at the next login, and undoes an Enable this
// run performed.
//
// IT UNDOES WHAT THIS RUN DID, in the shape this run did it. If this run
// INSTALLED the agent, undoing means removing that file — and removing it is
// enough, because a label with no plist starts nothing. If the agent was already
// there, this run only cleared an override, so undoing means writing it back.
//
// THE TWO ARE NOT INTERCHANGEABLE, and getting it wrong leaves the landmine this
// backend exists to avoid: a label disabled in the durable database with no
// plist to explain it, which makes a LATER install bootstrap a service launchd
// silently refuses to run.
func (c *Converger) Disable(ctx context.Context, label string) error {
	if c.installedThisRun(label) {
		if err := c.removeAgent(label); err != nil {
			return err
		}

		c.forgetInstall(label)

		// AND THE OVERRIDE THIS RUN CLEARED GOES BACK. Removing the file makes
		// the service not start, which is most of the undo — but an operator who
		// had DELIBERATELY disabled this label is entitled to find it disabled
		// afterwards, and a run that quietly enabled it has changed something it
		// was never asked to change.
		if !c.clearedThisRun(label) {
			return nil
		}
	}

	if _, code, err := c.run(ctx, []string{"disable", c.target(label)}); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("launchd: `launchctl disable %s` exited %d", c.target(label), code)
	}

	return nil
}

// clearedThisRun reports whether this run cleared a disabled override that was
// already there.
func (c *Converger) clearedThisRun(label string) bool {
	for _, l := range c.cleared {
		if l == label {
			return true
		}
	}

	return false
}

// installedThisRun reports whether this run is what put the agent there.
func (c *Converger) installedThisRun(label string) bool {
	for _, l := range c.created {
		if l == label {
			return true
		}
	}

	return false
}

// removeAgent takes back an agent this run installed.
//
// ONLY IF IT IS STILL THE FILE THIS RUN WROTE. A pathname is not a promise: in
// between, an operator or another tool can have replaced it, and deleting that
// would lose somebody's work while reporting a clean rollback. The bytes are
// compared rather than a timestamp, because the question is whether the content
// is still billet's.
func (c *Converger) removeAgent(label string) error {
	shipped, ok := agentOf(label)
	if !ok {
		return fmt.Errorf("launchd: %s is not a service billet ships", label)
	}

	path := c.agentPath(label)

	installed, present, err := c.installedAgent(label)

	switch {
	case err != nil:
		return err

	case !present:
		// Somebody else removed it. The outcome billet wanted, by another hand.
		return nil

	case !sameAgent(installed, shipped):
		return fmt.Errorf("launchd: %s was installed by this run but is no longer the agent "+
			"billet wrote there, so it has been left alone. Undoing this run did NOT remove it — "+
			"look at it and remove it yourself if it is not wanted", path)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("launchd: remove %s: %w", path, err)
	}

	return syncDir(filepath.Dir(path))
}

// StartAndProve bootstraps a launch agent and establishes it stayed up.
//
// A FAILED bootstrap IS NEVER INTERPRETED. Measured: `Bootstrap failed: 5:
// Input/output error` is what launchd returns for a label that is already
// loaded, one that is still draining, AND one carrying a disabled override —
// three different situations and one useless sentence. So a failure is diagnosed
// by RE-READING what launchd and the override database say, which is the only
// way to tell an operator something true.
//
// WHAT IT PROVES IS RETURNED, because it is weaker here than on Linux and must
// not be reported in the same words. systemd's units are Type=notify, so a
// successful start means billet's own process reached READY=1. launchd has no
// such thing: all that can be established is that one process survived a window.
func (c *Converger) StartAndProve(ctx context.Context, label string) (string, error) {
	plist := c.agentPath(label)

	out, code, err := c.run(ctx, []string{"bootstrap", c.domain(), plist})
	if err != nil {
		return "", err
	}

	if code != 0 {
		return "", c.diagnoseBootstrap(ctx, label, out, code)
	}

	if err := c.ProveStable(ctx, label); err != nil {
		return "", err
	}

	return "the same process is still running after the settle window — launchd has no " +
		"readiness notification, so nothing here says it finished starting up", nil
}

// diagnoseBootstrap works out why a bootstrap failed, by asking.
func (c *Converger) diagnoseBootstrap(ctx context.Context, label, out string, code int) error {
	said := strings.TrimSpace(firstLine(out))
	if said == "" {
		said = fmt.Sprintf("exit %d", code)
	}

	if disabled, err := c.disabledLabels(ctx); err == nil && disabled[label] {
		return fmt.Errorf("launchd: %s is disabled, so launchd refused to load it (%s). "+
			"Clear it with `launchctl enable %s`", label, said, c.target(label))
	}

	if job, loaded, err := c.job(ctx, label); err == nil && loaded {
		if job.Running() {
			return fmt.Errorf("launchd: %s is already loaded and running as pid %d (%s)",
				label, job.PID, said)
		}

		return fmt.Errorf("launchd: %s is already loaded and is %s, which usually means it is "+
			"still stopping (%s). Wait for it to finish and run this again",
			label, orNone(job.State), said)
	}

	return fmt.Errorf("launchd: `launchctl bootstrap %s %s` failed (%s), and billet could not "+
		"establish why — launchd returns the same error for a service that is already loaded, "+
		"one that is still stopping, and one that is disabled", c.domain(), c.agentPath(label), said)
}

// EnableScheduled installs a oneshot agent, clears any disabled override, and
// loads it so its schedule is live now rather than at the next login.
//
// NO PROOF, BECAUSE THERE IS NO PROCESS TO PROVE. StartAndProve's whole test is
// that one pid survived a settle window, and a oneshot that ran at load and
// exited is a success that test would call a crash. What is established instead
// is that launchd holds the job: `print` answers for it afterwards.
//
// A LOADED JOB WITH A PROCESS IS LEFT ALONE. That process may be the upgrade
// transaction itself, midway through draining the node; booting it out to load
// a fresh copy of its own plist would kill the updater. A loaded job with no
// process is booted out and loaded again, so what launchd holds is what the
// plist on disk says — launchd reads a plist once, at bootstrap. A plist that
// differs from the one this build ships is refused by Enable before any of
// that, as the service agents' are: a changed schedule wants a person to move
// the old file aside, and `up` says so.
func (c *Converger) EnableScheduled(ctx context.Context, label string) error {
	if err := c.Enable(ctx, label); err != nil {
		return err
	}

	job, loaded, err := c.job(ctx, label)
	if err != nil {
		return err
	}

	if loaded {
		if job.Running() {
			return nil
		}

		if _, code, err := c.run(ctx, []string{"bootout", c.target(label)}); err != nil {
			return err
		} else if code != 0 && code != nothingToBoot {
			return fmt.Errorf("launchd: `launchctl bootout %s` exited %d", c.target(label), code)
		}
	}

	out, code, err := c.run(ctx, []string{"bootstrap", c.domain(), c.agentPath(label)})
	if err != nil {
		return err
	}

	if code != 0 {
		return c.diagnoseBootstrap(ctx, label, out, code)
	}

	if _, loaded, err := c.job(ctx, label); err != nil {
		return err
	} else if !loaded {
		return fmt.Errorf("launchd: %s was bootstrapped and launchd does not hold it", label)
	}

	return nil
}

// Uninstall removes a label's agent and leaves nothing behind that would stop a
// later install.
//
// THE ORDER IS THE WHOLE THING, and it is the opposite of what reads naturally.
// The plist is removed FIRST and that removal is flushed to disk BEFORE the
// override is cleared. Clearing first opens a window in which the label is
// enabled and its plist is still there — a login or a reboot inside it starts
// the node this command is uninstalling, and nothing is left watching it.
//
// AND THE OVERRIDE MUST BE CLEARED AT ALL, which is the landmine this exists to
// remove. launchd's disabled-override database is durable, keyed by LABEL, and
// measured, it survives both the bootout and the removal of the plist entirely —
// `launchctl print-disabled` on a real Mac lists labels belonging to software
// uninstalled years ago. A label left disabled with no plist to explain it means
// a LATER install bootstraps a service launchd silently refuses to run, with the
// same `Bootstrap failed: 5: Input/output error` it gives for three other
// reasons. Nothing on the machine would say why.
//
// A PLIST THAT IS NOT THE ONE BILLET SHIPS IS LEFT, for the same reason `up`
// refuses to replace one: it is somebody's edit, and this command was asked to
// remove billet rather than to remove their work.
func (c *Converger) Uninstall(ctx context.Context, label string) error {
	shipped, ok := agentOf(label)
	if !ok {
		return fmt.Errorf("launchd: %s is not a service billet ships", label)
	}

	path := c.agentPath(label)

	installed, present, err := c.installedAgent(label)
	if err != nil {
		return err
	}

	switch {
	case !present:
		// Nothing to remove. The override still has to go: it outlives the file,
		// and that is exactly the state that breaks the next install.

	case !sameAgent(installed, shipped):
		return fmt.Errorf("launchd: %s is not the agent this billet ships, so it has been left "+
			"alone — remove it yourself if it is not wanted. Nothing else was undone for %s, "+
			"including its disabled override, because a half-uninstalled service is worse than "+
			"one still installed", path, label)

	default:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("launchd: remove %s: %w", path, err)
		}

		// FLUSHED BEFORE THE OVERRIDE IS TOUCHED. What survives a crash is the
		// directory entry rather than the unlink call returning, and the next
		// thing this does is make the label startable again.
		if err := syncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("launchd: remove %s: %w", path, err)
		}
	}

	if _, code, err := c.run(ctx, []string{"enable", c.target(label)}); err != nil {
		return fmt.Errorf("launchd: %s was removed but billet could not clear its disabled "+
			"override (%w). Clear it with `launchctl enable %s`, or a later install will "+
			"bootstrap a service launchd refuses to run", label, err, c.target(label))
	} else if code != 0 {
		return fmt.Errorf("launchd: %s was removed but `launchctl enable %s` exited %d, so its "+
			"disabled override may remain; clear it before installing again", label,
			c.target(label), code)
	}

	return nil
}
