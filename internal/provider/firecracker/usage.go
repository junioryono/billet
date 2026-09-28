package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/junioryono/billet/internal/provider"
)

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
	res, err := resourcesOf(j)
	if err != nil {
		return provider.UsageTarget{}, err
	}

	return provider.UsageTarget{
		CgroupDir:        filepath.Join(root, j.execName, j.id),
		PID:              pid,
		VCPUThreadPrefix: vcpuThreadPrefix,
		NetDevice:        res.Tap,
		NetHostView:      true,
	}, nil
}
