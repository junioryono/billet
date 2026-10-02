//go:build darwin

package node

import (
	"context"
	"os"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// selfProvider says every instance is this test's own process, as tart says a
// VM is its Virtualization.framework process.
type selfProvider struct{ *fakeProvider }

func (selfProvider) UsageTarget(context.Context, string) (provider.UsageTarget, error) {
	c, err := usage.Reader{}.ProcessCounters(os.Getpid())
	if err != nil {
		return provider.UsageTarget{}, err
	}

	return provider.UsageTarget{PID: os.Getpid(), PIDStart: c.Start, Process: true}, nil
}

// A VM THAT IS ONE PROCESS IS MEASURED BY THAT PROCESS'S OWN ACCOUNTING, from
// launch to the report in the ledger, by the real kernel.
func TestAProcessVMIsMeasuredByTheKernelsAccounting(t *testing.T) {
	p := selfProvider{&fakeProvider{kind: config.ProviderDocker}}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithMonitor(runningMonitor(t, "/")))

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := a.LeaseUsage(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsage: %v", err)
	}
	if !got.Measured(alloc.UsageCPU) || !got.Measured(alloc.UsageMemory) || !got.Measured(alloc.UsageIO) {
		t.Errorf("unmeasured = %v, want cpu, memory and io read from the process", got.Unmeasured)
	}
	if got.Measured(alloc.UsageOOM) || got.Measured(alloc.UsageNet) || got.Measured(alloc.UsageThreads) {
		t.Errorf("unmeasured = %v, want oom, net and threads named", got.Unmeasured)
	}
	if got.CPUUserMicros <= 0 || got.MemoryPeakBytes <= 0 {
		t.Errorf("cpu %dµs, memory peak %d bytes: this process has used both", got.CPUUserMicros,
			got.MemoryPeakBytes)
	}
	if !got.Measured(alloc.UsageEnergy) || got.EnergySource != alloc.EnergyProcess ||
		got.EnergyActiveMicrojoules <= 0 {
		t.Errorf("energy %d µJ from %q, want the process's own", got.EnergyActiveMicrojoules, got.EnergySource)
	}
}
