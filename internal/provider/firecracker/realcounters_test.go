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
// a reading; this asserts that the readings are the guest's. A guest spinning in
// a shell loop keeps one vCPU thread on a core in guest mode, so the cycles
// counted, divided by the time the vCPU threads ran (from /proc, independently
// of perf), is the core's clock. Counting without guest mode, or the wrong
// thread, leaves only the exits: a small fraction of those cycles, and a clock
// far below any CPU billet runs on. The IPC is then checked against a broad
// band a core can have.
//
// It skips unless the machine can run a real microVM (requireRealHost) and the
// node could count on this platform.
func TestRealVCPUThreadsCountTheGuestsInstructions(t *testing.T) {
	env := requireRealHost(t)
	counters, err := usage.HardwareCounters()
	if err == nil {
		err = usage.ProveCounting(counters)
	}
	if err != nil {
		t.Skipf("this host cannot count, and a node here refuses perf: %v", err)
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
	if !sum.Measured.Threads || len(sum.Points) == 0 {
		t.Fatalf("the vCPU threads' CPU time was not measured (%+v), so the counts cannot be checked "+
			"against it", sum.Measured)
	}
	// THE LOOP RAN: of ten seconds, the vCPU threads spent at least five on a core.
	ran := time.Duration(sum.Latest.GuestCPU-sum.Points[0].GuestCPU) * time.Microsecond
	if ran < 5*time.Second {
		t.Fatalf("the vCPU threads ran %s while the guest was meant to spin; the loop did not start", ran)
	}
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
	// THE CYCLES ARE THE GUEST'S: per second the vCPU threads ran, a core's clock.
	if ghz := float64(c.Values[usage.Cycles]) / ran.Seconds() / 1e9; ghz < 0.5 || ghz > 6 {
		t.Errorf("%.2f GHz of cycles counted over the %s the vCPU threads ran, want 0.5 to 6: "+
			"guest mode is not being counted", ghz, ran)
	}
	// A BROAD BAND, because it is a property of the CPU and the loop rather than
	// of billet: below it the count is mostly not the guest's, above it no core
	// billet runs on retires that many.
	if ipc := float64(c.Values[usage.Instructions]) / float64(c.Values[usage.Cycles]); ipc < 0.2 || ipc > 6 {
		t.Errorf("IPC %.2f is outside 0.2 to 6", ipc)
	}
}
