package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// hostProcessStart reads a pid's start time from the real /proc.
func hostProcessStart(pid int) (uint64, error) { return usage.Reader{Root: "/"}.ProcessStart(pid) }

// vcpuThreadPrefix is how Firecracker names the threads that run guest code:
// "fc_vcpu 0" through "fc_vcpu N". The event loop is "firecracker-v1." (the
// executable's name cut to 15 bytes) and the API thread "fc_api"; measured on
// v1.16.1 (the reference host, 2026-09-25).
const vcpuThreadPrefix = "fc_vcpu"

// UsageTarget says where a running microVM's counters are: the cgroup the
// jailer made for it, its VMM's pid and its tap.
//
// THE PID IS THE ONE vmmPID PROVES, so a number the kernel has reused for
// another process is never read as this guest's threads.
func (p *Provider) UsageTarget(_ context.Context, instanceID string) (provider.UsageTarget, error) {
	j := p.jailFor(instanceID)

	root, err := cgroup2Mount(p.procMountsPath)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	pid, err := p.vmmPID(j)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	if pid == 0 {
		return provider.UsageTarget{}, fmt.Errorf("firecracker: %s has no running vmm to measure", instanceID)
	}
	// READ AFTER vmmPID PROVED THE PID IS THIS JAIL'S, and carried with it, so
	// every later read can prove it still is.
	start, err := p.processStart(pid)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("firecracker: read the start time of %s's vmm: %w",
			instanceID, err)
	}
	// AND PROVED AGAIN AFTER, so the start time read is the VMM's and not that of
	// a process that took the pid between the two reads.
	if owns, err := p.pidOwner(pid, j.id); err != nil || !owns {
		return provider.UsageTarget{}, fmt.Errorf("firecracker: %s's vmm pid %d changed hands while it "+
			"was being read", instanceID, pid)
	}
	res, err := resourcesOf(j)
	if err != nil {
		return provider.UsageTarget{}, err
	}

	return provider.UsageTarget{
		CgroupDir:        filepath.Join(root, j.execName, j.id),
		PID:              pid,
		PIDStart:         start,
		VCPUThreadPrefix: vcpuThreadPrefix,
		NetDevice:        res.Tap,
		NetHostView:      true,
	}, nil
}
