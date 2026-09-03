package tart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// checkStoreLockWait bounds the preflight's own attempt on the store lock.
//
// Short on purpose, and unrelated to the window a NAME MUTATION waits: a check
// is asking whether the lock can be placed, and a node holding it answers that
// question without waiting at all.
const checkStoreLockWait = 2 * time.Second

// HostReport is what CheckHost proved about this machine.
type HostReport struct {
	// Version is the tart binary's own version string.
	Version string
	// VMs is how many local VMs tart's store holds right now, all owners.
	VMs int
	// StoreLock is the file every billet sharing this store takes around a
	// mutation of a lease name. REPORTED rather than assumed, for the reason the
	// deployment lock logs its path every boot: which collision domain a process
	// joined is decided by TART_HOME, and two billets that disagree about that
	// serialize against nothing while looking correct.
	StoreLock string
	// StoreLockProved says billet actually took and released it. StoreLockWhy
	// says why it could not, when it could not.
	//
	// THREE STATES, NOT TWO, because contention and placeability are different
	// facts and the first version collapsed them: a lock that is held proves a
	// file exists and something holds it, NOT that this host can ever acquire
	// one. Reporting a wedged store as a clean preflight is the failure that
	// shape produces.
	StoreLockProved bool
	StoreLockWhy    string
	// Softnet is what this host can do about guest network isolation.
	Softnet SoftnetReport
}

// SoftnetReport says what this host could do about guest network isolation.
//
// Reported rather than fatal, because a node running only trusted tiers needs
// none of it — but reported LOUDLY, because the alternative is discovering on
// the first untrusted job that the isolation billet promised does not exist.
type SoftnetReport struct {
	// Path is the resolved binary, empty when softnet is not installed.
	Path string
	// GrantConfigured reports that the setuid-root grant is in place.
	//
	// DELIBERATELY NOT NAMED "isolation works". It is two metadata fields —
	// the setuid bit and an owner of root — and it says softnet could start,
	// nothing about what its policy then permits. Only a network probe from
	// inside a guest can say that.
	GrantConfigured bool
	// Trusted reports that the binary sits where tart's own install puts it,
	// which is what makes it safe to print a command granting it root.
	Trusted bool
	// Why explains the state, with the command that fixes it when there is one.
	Why string
}

// statOwner reports a file's mode and owning uid.
func statOwner(path string) (os.FileMode, uint32, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}

	// os.Stat().Sys() holds a *syscall.Stat_t, NOT the structurally identical
	// *unix.Stat_t — the firecracker backend lost a launch to that exact
	// confusion, so this asserts the type it actually gets.
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("ownership cannot be read on this platform")
	}

	return info.Mode(), st.Uid, nil
}

// checkSoftnet reports whether untrusted work could be isolated on this host.
//
// THE SETUID BIT ALONE IS NOT THE GRANT, and that is why this exists rather
// than a line in a README. Setting the bit on a Homebrew-installed softnet
// leaves it owned by the INSTALLING USER, and setuid confers the owner's
// privileges — so `ls` shows the `s`, everything looks configured, and softnet
// still exits with "root privileges are required to run and passwordless sudo
// was not available" on the first untrusted launch. Measured on a real Mac
// after making exactly that mistake.
//
// AND THE REMEDIATION IS NOT PRINTED FOR JUST ANY BINARY. `softnet` is taken
// from PATH, so a writable directory earlier in it would otherwise turn this
// check's own output into a local privilege-escalation recipe: billet would be
// telling an operator to make a stranger's executable setuid-root. A candidate
// that does not sit beside the tart binary is reported with the caution
// attached, never as a plain instruction to run.
func (p *Provider) checkSoftnet() SoftnetReport {
	var report SoftnetReport

	path, err := exec.LookPath(p.softnet)
	if err != nil {
		report.Why = "not installed; it ships with tart (`brew install openai/tools/tart`)"

		return report
	}

	// The symlink is not the file: Homebrew puts `bin/softnet` in front of a
	// Cellar path, and the ownership that decides is the target's.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		report.Why = "cannot be resolved: " + err.Error()

		return report
	}

	report.Path = resolved
	report.Trusted = p.sameInstallAsTart(path)

	mode, uid, err := p.softnetStat(resolved)
	if err != nil {
		report.Why = "cannot be inspected: " + err.Error()

		return report
	}

	// BOTH COMMANDS, IN THIS ORDER, ALWAYS. chown CLEARS the setuid bit —
	// measured on macOS — so advising chown alone for a wrong-owner binary
	// leaves it root-owned and still unusable, which looks like the fix did
	// nothing.
	fix := "sudo chown root " + shellQuote(resolved) +
		" && sudo chmod u+s " + shellQuote(resolved)

	switch {
	case mode&os.ModeSetuid != 0 && uid == 0:
		report.GrantConfigured = true

		if !report.Trusted {
			report.Why = "is setuid-root but does not sit beside the tart binary; " +
				"confirm it is the softnet tart installed before trusting it"
		}
	case mode&os.ModeSetuid == 0:
		report.Why = "is not setuid-root, so it cannot create a vmnet interface: " +
			fix
	default:
		report.Why = "is setuid but owned by uid " + strconv.FormatUint(uint64(uid), 10) +
			" rather than root, and setuid confers the OWNER's privileges — so the bit " +
			"currently grants nothing: " + fix
	}

	if !report.Trusted && report.Why != "" && !report.GrantConfigured {
		report.Why = "was found on PATH rather than beside the tart binary, so verify it " +
			"is tart's own softnet BEFORE granting it root — it " + report.Why
	}

	return report
}

// sameInstallAsTart reports whether a softnet candidate sits in the directory
// tart itself was installed into, which is what tart's own packaging does.
func (p *Provider) sameInstallAsTart(softnetPath string) bool {
	tartPath, err := exec.LookPath(p.tart)
	if err != nil {
		return false
	}

	return filepath.Dir(softnetPath) == filepath.Dir(tartPath)
}

// CheckHost proves this machine can act as a tart node.
//
// The same distinction the firecracker and ec2 preflights make: config
// validation proves a block is coherent, and it cannot prove tart is installed,
// that it speaks the JSON billet parses, that it carries the `exec` subcommand
// the registration delivery depends on, or that billet and the binary agree
// which store holds the VMs. A node wrong about any of those validates
// perfectly and fails on the first job of the day.
func (p *Provider) CheckHost(ctx context.Context) (HostReport, error) {
	var report HostReport

	// The binary only exists for Apple Silicon macOS, so on any other platform
	// the useful diagnostic is the platform, not "executable file not found".
	if p.goos != "darwin" {
		return report, fmt.Errorf("tart: this node's provider is tart, which runs macOS and "+
			"Linux guests through Apple's Virtualization.framework — it needs a macOS host, "+
			"and this is %s", p.goos)
	}

	if _, err := exec.LookPath(p.tart); err != nil {
		return report, fmt.Errorf("tart: the tart binary is not installed (%w); install it "+
			"with `brew install openai/tools/tart`", err)
	}

	version, err := p.run(ctx, "--version")
	if err != nil {
		return report, fmt.Errorf("tart: the binary exists but cannot report its version: %w", err)
	}

	report.Version = strings.TrimSpace(version)

	// CAPABILITY, not a version floor: billet delivers the runner registration
	// through `tart exec`, and a version list would go stale the way the Ceph
	// release list did — while asking the binary whether it HAS the subcommand
	// can never refuse a correct install. A tart old enough to lack it fails
	// its own argument parser here.
	if _, err := p.run(ctx, "exec", "--help"); err != nil {
		return report, fmt.Errorf("tart: this tart has no `exec` subcommand, which billet "+
			"needs to hand the runner its registration through the guest agent; upgrade tart "+
			"(%w)", err)
	}

	// The real list, parsed by the real reader: proves --format json still has
	// the shape billet reads, and that billet's TART_HOME and the binary's are
	// one store — the cross-check inside refuses a listing that omits a VM
	// billet's own store holds.
	vms, err := p.list(ctx)
	if err != nil {
		return report, err
	}

	report.VMs = len(vms)

	// TAKEN AND RELEASED, because a lock that cannot be placed is a node that
	// cannot destroy a guest — and discovering that on the first teardown of the
	// day means a lease whose capacity is held while an operator works out why.
	// This CREATES the lock directory and file if they are not there, which is a
	// preflight writing into TART_HOME and worth knowing about; it is a 0700
	// directory holding one empty file.
	//
	// ON ITS OWN SHORT CLOCK, AND CONTENTION IS NOT A FAILURE. A check runs
	// beside a live node, so the lock being held is the ordinary state, and
	// waiting the mutation window for it would stall an operator's preflight for
	// minutes.
	//
	// IT IS NOT A PASS EITHER, AND SAYING SO WAS THIS FUNCTION'S OWN DEFECT.
	// EWOULDBLOCK proves some file description holds the lock. It does not prove
	// the holder is billet, that the holder is making progress, or that a real
	// mutation would ever acquire it — so a permanently wedged store would have
	// passed `billet check` and then failed every launch and teardown. The
	// report carries three states and the operator is told which one this is.
	if p.beforeStoreLock != nil {
		p.beforeStoreLock()
	}

	lockCtx, cancelLock := context.WithTimeout(ctx, checkStoreLockWait)
	defer cancelLock()

	unlock, err := p.lockStore(lockCtx, "prove billet can serialize name mutations here")

	report.StoreLock = p.storeLockPath()

	switch {
	// THE CALLER'S CANCELLATION IS ASKED ABOUT FIRST, and putting it second was
	// a real bug: lockStore wraps ctx.Err() AND errStoreLockBusy on that branch,
	// so an operator who interrupts a waiting `billet check` produced an error
	// matching both — and a busy-first switch read it as a host that is fine.
	// "Could not tell" is not "no", one more time.
	case ctx.Err() != nil:
		return report, fmt.Errorf("tart: could not finish proving the store lock at %s: %w",
			report.StoreLock, ctx.Err())

	case errors.Is(err, errStoreLockBusy):
		// Something holds it. Almost always a node on this host doing its job,
		// and NOT established as such — hence Proved staying false.
		report.StoreLockWhy = "held by another process for the whole of this check, so billet " +
			"could not prove it can take it; that is what a busy node looks like, and also " +
			"what a wedged store looks like"

	case err != nil:
		return report, fmt.Errorf("tart: %w; every rename of a clone into a lease name and "+
			"every delete of one happens under that lock, so a host that cannot place it "+
			"cannot launch or tear down a guest", err)

	default:
		unlock()

		report.StoreLockProved = true
	}
	report.Softnet = p.checkSoftnet()

	return report, nil
}
