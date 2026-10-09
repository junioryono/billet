package firecracker

import (
	"context"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// A REAL MICROVM'S vCPU THREADS, COUNTED THROUGH perf_event_open.
//
// The fake counter source in internal/usage asserts what the sampler does with
// a reading; this asserts that the readings exist: that a vCPU thread opened by
// tid, with guest mode included, counts the guest's instructions at all, and that
// their ratio to cycles is an IPC a CPU can have. A guest spinning in a shell
// loop retires instructions on every cycle it runs, so excluding guest mode, or
// counting the wrong thread, reads as no instructions or as a ratio outside any
// CPU's range.
//
// It skips unless the machine can run a real microVM (requireRealHost) and the
// node could count on this platform.
func TestRealVCPUThreadsCountTheGuestsInstructions(t *testing.T) {
	env := requireRealHost(t)
	counters, err := usage.HardwareCounters()
	if err != nil {
		t.Skipf("this platform cannot count: %v", err)
	}

	p, err := New(selftestDeployment, env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	name := provider.InstanceName(env.lease)
	t.Cleanup(func() {
		if _, err := p.Destroy(context.WithoutCancel(t.Context()), name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	inst, err := p.Launch(t.Context(), provider.Spec{
		Name: name, Image: env.image, VCPU: 2, Memory: 1 * config.GiB,
		Disk:      env.disk.growthCapacity(t, env.image),
		Command:   []string{"/bin/sh", "-c", "i=0; while :; do i=$((i+1)); done"},
		Trust:     provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	target, err := p.UsageTarget(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("UsageTarget: %v", err)
	}

	monitor := usage.NewMonitor("/", usage.Options{Interval: time.Second, Counters: counters})
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	monitor.Start("vm", usage.Target{CgroupDir: target.CgroupDir, PID: target.PID,
		PIDStart: target.PIDStart, VCPUThreadPrefix: target.VCPUThreadPrefix}, 2)

	select {
	case <-time.After(10 * time.Second):
	case <-t.Context().Done():
		t.Fatal("the test ended while the guest was spinning")
	}

	sum, ok := monitor.Final("vm")
	if !ok || sum.Counters == nil {
		t.Fatalf("Final = %+v, %v; want a counted job", sum.Counters, ok)
	}
	c := sum.Counters
	t.Logf("cycles %d, instructions %d, cache references %d, cache misses %d, branch misses %d, "+
		"frontend stalls %d; measured %v", c.Values[usage.Cycles], c.Values[usage.Instructions],
		c.Values[usage.CacheReferences], c.Values[usage.CacheMisses], c.Values[usage.BranchMisses],
		c.Values[usage.FrontendStalls], c.Measured)

	if !c.Measured[usage.Cycles] || !c.Measured[usage.Instructions] {
		t.Fatalf("cycles and instructions were not both counted (%v): a group of six may not fit "+
			"beside the NMI watchdog's counter", c.Measured)
	}
	if c.Values[usage.Instructions] <= 0 || c.Values[usage.Cycles] <= 0 {
		t.Fatalf("a spinning guest counted %d instructions in %d cycles", c.Values[usage.Instructions],
			c.Values[usage.Cycles])
	}
	// A BROAD BAND, because it is a property of the CPU and the loop rather than
	// of billet: below it the count is mostly not the guest's, above it no core
	// billet runs on retires that many.
	if ipc := float64(c.Values[usage.Instructions]) / float64(c.Values[usage.Cycles]); ipc < 0.2 || ipc > 6 {
		t.Errorf("IPC %.2f is outside 0.2 to 6", ipc)
	}
}
