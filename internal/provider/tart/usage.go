package tart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// vmService is the executable Virtualization.framework runs each VM in: an XPC
// service launchd starts on tart's behalf, one per VM, parented to launchd
// rather than to `tart run`. The guest's CPU, memory and disk are charged to
// it, and tart's own process holds almost none (macOS 27, tart 2.37.0,
// measured on the reference Mac 2026-09-30).
const vmService = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/" +
	"com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"

// hostProcessStart reads a process's start from the running kernel.
func hostProcessStart(pid int) (uint64, error) {
	c, err := usage.Reader{Root: "/"}.ProcessCounters(pid)
	if err != nil {
		return 0, err
	}

	return c.Start, nil
}

// UsageTarget says which process is a running VM: the one Virtualization.framework
// process that holds the VM's disk open.
//
// OWNERSHIP IS THE OPEN DISK, which names the VM; nothing about the process
// itself does. So the disk must be this deployment's own (the VM directory's
// ownership marker, with no symlink at either level, since lsof follows one to
// whatever it names), and the same file before and after. The holder is proved
// twice around reading its start time, so the start that every later read is
// checked against is the VM's and not that of a process that took the pid in
// between.
func (p *Provider) UsageTarget(ctx context.Context, instanceID string) (provider.UsageTarget, error) {
	if _, ok := provider.LeaseOf(instanceID); !ok || filepath.Base(instanceID) != instanceID {
		return provider.UsageTarget{}, fmt.Errorf("tart: %q is not an instance billet names", instanceID)
	}
	disk, err := p.ownDisk(instanceID)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	path := filepath.Join(p.vmDir(instanceID), "disk.img")
	pid, err := p.diskHolder(ctx, path)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("tart: find %s's VM process: %w", instanceID, err)
	}
	start, err := p.processStart(pid)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("tart: read the start of %s's VM process: %w", instanceID, err)
	}
	if again, err := p.diskHolder(ctx, path); err != nil || again != pid {
		return provider.UsageTarget{}, fmt.Errorf("tart: %s's VM process changed while it was being read",
			instanceID)
	}
	if after, err := p.ownDisk(instanceID); err != nil || !os.SameFile(disk, after) {
		return provider.UsageTarget{}, fmt.Errorf("tart: %s's disk changed while its VM was being found",
			instanceID)
	}

	return provider.UsageTarget{PID: pid, PIDStart: start, Process: true}, nil
}

// ownDisk is the VM's disk, when the VM is this deployment's and neither its
// directory nor its disk is a symlink.
//
// THESE CATCH ACCIDENTS, NOT AN ADVERSARY: a process running as the node's user
// can write TART_HOME, and so can forge the marker as easily as swap a path or
// hard-link a disk. That user is trusted; a guest never reaches these files.
func (p *Provider) ownDisk(name string) (os.FileInfo, error) {
	dir, err := os.Lstat(p.vmDir(name))
	if err != nil {
		return nil, fmt.Errorf("tart: %s has no VM directory: %w", name, err)
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("tart: %s's VM directory is not a directory", name)
	}
	owner, err := p.ownerOf(name)
	if err != nil {
		return nil, fmt.Errorf("tart: %s is not proved to be this deployment's VM: %w", name, err)
	}
	if owner != p.owner {
		return nil, fmt.Errorf("tart: %s belongs to deployment %s, not this one", name, owner)
	}
	disk, err := os.Lstat(filepath.Join(p.vmDir(name), "disk.img"))
	if err != nil {
		return nil, fmt.Errorf("tart: %s has no disk to find its VM by: %w", name, err)
	}
	if !disk.Mode().IsRegular() {
		return nil, fmt.Errorf("tart: %s's disk is not a regular file", name)
	}

	return disk, nil
}

// diskHolder is the one VM process that has disk open. None, several, or one
// that is not a Virtualization.framework VM is an error, never a guess.
func (p *Provider) diskHolder(ctx context.Context, disk string) (int, error) {
	pids, err := p.openers(ctx, disk)
	if err != nil {
		return 0, err
	}
	var vms []int
	for _, pid := range pids {
		path, err := p.processPath(pid)
		// ONLY A HOLDER PROVED GONE IS SKIPPED. One that could not be read is
		// could-not-tell, and skipping it could make another VM the sole one.
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("could not tell what process %d holding %s is: %w", pid, disk, err)
		}
		if path == vmService {
			vms = append(vms, pid)
		}
	}
	switch len(vms) {
	case 0:
		return 0, fmt.Errorf("no VM process has %s open", disk)
	case 1:
		return vms[0], nil
	default:
		return 0, fmt.Errorf("%d VM processes have %s open", len(vms), disk)
	}
}

// openers lists the processes this user can see that have path open.
//
// lsof EXITS 1 BOTH FOR NO OPENER AND FOR AN ERROR, so an empty answer is
// believed only when lsof also said nothing on stderr. Its warnings are turned
// back on (-t turns them off) and any of them refuses, on either exit status.
//
// IT SEES ONLY THIS USER'S PROCESSES, and skips the rest without a word. The disk
// lives in this user's tart store, so another user's process holding it would
// have to be root's; that is outside what billet defends against.
func (p *Provider) openers(ctx context.Context, path string) ([]int, error) {
	// #nosec G204 -- the binary is fixed and the one argument is a path built
	// from TART_HOME and a lease-shaped name. No shell.
	cmd := exec.CommandContext(ctx, p.lsof, "-t", "+w", "--", path)
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	complaint := strings.TrimSpace(stderr.String())
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 &&
		stdout.Len() == 0 && complaint == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", p.lsof, err, complaint)
	}
	if complaint != "" {
		return nil, fmt.Errorf("%s warned, so its answer may be short: %s", p.lsof, complaint)
	}
	var pids []int
	for field := range strings.FieldsSeq(stdout.String()) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("%s answered %q, which is not a pid", p.lsof, field)
		}
		if !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}
