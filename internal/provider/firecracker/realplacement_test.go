package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// THE PLACEMENT EXPERIMENT: a set of real microVMs, each running a workload
// that needs no network, launched together under one cpu placement, measured
// by the node's own sampler from launch to the moment the guest powers itself
// off. One run writes one JSON line per guest to the results file; a series of
// runs, interleaved across placements and loads with the wall meter logging
// beside them, is the comparison docs/reference/records/cpu-placement.md
// records.
//
// It skips unless BILLET_PLACEMENT_EXPERIMENT_OUT names a results file, on top
// of everything requireRealHost needs, and it is meant for a drained node: its
// guests are this test's own deployment's, which the node's sweeps leave alone,
// but the host's cpus are shared with whatever else runs.
//
// Settings, all from the environment:
//   - BILLET_PLACEMENT_EXPERIMENT_PLACEMENT: none, compact or spread.
//   - BILLET_PLACEMENT_EXPERIMENT_VMS and _VCPU: how many guests, of how many vCPUs.
//   - BILLET_PLACEMENT_EXPERIMENT_WORKLOAD: gostd (`go build -a std`, a parallel
//     compile) or memory (every vCPU copying a buffer far larger than an L3).
//   - BILLET_PLACEMENT_EXPERIMENT_RUN: a label written on every line.
//   - BILLET_PLACEMENT_EXPERIMENT_IDLE_WATTS: the package's idle power, so each
//     guest's share of the energy above it is what the node would attribute.
func TestRealPlacementExperiment(t *testing.T) {
	out := os.Getenv("BILLET_PLACEMENT_EXPERIMENT_OUT")
	if out == "" {
		t.Skip("set BILLET_PLACEMENT_EXPERIMENT_OUT to a results file to run the placement experiment")
	}
	env := requireRealHost(t)

	placement := os.Getenv("BILLET_PLACEMENT_EXPERIMENT_PLACEMENT")
	if placement == "none" {
		placement = ""
	}
	vms := experimentInt(t, "BILLET_PLACEMENT_EXPERIMENT_VMS", 4)
	vcpu := experimentInt(t, "BILLET_PLACEMENT_EXPERIMENT_VCPU", 8)
	workload := os.Getenv("BILLET_PLACEMENT_EXPERIMENT_WORKLOAD")
	command, ok := experimentWorkloads[workload]
	if !ok {
		t.Fatalf("BILLET_PLACEMENT_EXPERIMENT_WORKLOAD is %q; expected gostd or memory", workload)
	}
	idle, err := strconv.ParseFloat(os.Getenv("BILLET_PLACEMENT_EXPERIMENT_IDLE_WATTS"), 64)
	if err != nil {
		t.Fatalf("BILLET_PLACEMENT_EXPERIMENT_IDLE_WATTS: %v", err)
	}

	cfg := env.cfg
	cfg.CPUPlacement = placement
	p, err := New(selftestDeployment, cfg, env.disk, WithBootWait(60*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// WHAT AN EARLIER RUN LEFT is this test's own deployment's, and goes first, so
	// its claims hold no cpu this run's placement needs.
	left, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, inst := range left {
		if _, err := p.Destroy(context.WithoutCancel(t.Context()), inst.ID); err != nil {
			t.Fatalf("destroy %s, left by an earlier run: %v", inst.ID, err)
		}
		t.Logf("destroyed %s, left by an earlier run", inst.ID)
	}
	counters, err := usage.HardwareCounters()
	if err == nil {
		err = usage.ProveCounting(counters)
	}
	if err != nil {
		counters = nil
		t.Logf("counting no hardware events on this host: %v", err)
	}
	monitor := usage.NewMonitor("/", usage.Options{Interval: time.Second, RAPL: true, IdleWatts: idle,
		Counters: counters})
	// THE SAMPLER STOPS WITH THE TEST'S CONTEXT, which ends before the cleanups
	// run, so the wait below joins it.
	done := make(chan struct{})
	ctx := t.Context()
	go func() {
		defer close(done)
		monitor.Run(ctx)
	}()
	t.Cleanup(func() { <-done })

	type guest struct {
		name       string
		launchedAt time.Time
		launched   time.Time
		exited     time.Time
		cpus       string
		summary    usage.Summary
		err        error
	}
	guests := make([]*guest, vms)
	disk := env.disk.growthCapacity(t, env.image)
	hostBusyBefore := hostBusyJiffies(t)
	started := time.Now()
	// THE WAIT ENDS WELL BEFORE THE TEST DOES, so the cleanups that destroy the
	// guests run before the test binary's own timeout kills it and leaves them.
	// The launches are held to it too, so a launch stuck in its disk clone cannot
	// keep the test running past it.
	waitUntil := started.Add(30 * time.Minute)
	if deadline, ok := t.Deadline(); ok && deadline.Add(-5*time.Minute).Before(waitUntil) {
		waitUntil = deadline.Add(-5 * time.Minute)
	}
	runCtx, cancelRun := context.WithDeadline(ctx, waitUntil)
	defer cancelRun()

	// EVERY GUEST IS DESTROYED AT ONCE, each within its own bound, so one slow
	// disk teardown cannot leave another guest's VMM running.
	t.Cleanup(func() {
		var teardown sync.WaitGroup
		for _, g := range guests {
			if g == nil {
				continue
			}
			teardown.Add(1)
			go func() {
				defer teardown.Done()
				destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Minute)
				defer cancel()
				if _, err := p.Destroy(destroyCtx, g.name); err != nil {
					t.Errorf("destroy %s: %v", g.name, err)
				}
			}()
		}
		teardown.Wait()
	})

	// EVERY NAME FIRST, since minting one can skip the test, and a skip after
	// the first worker started would leave it running.
	for i := range vms {
		guests[i] = &guest{name: provider.InstanceName(freshLease(t))}
	}

	var wg sync.WaitGroup
	for _, g := range guests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.launchedAt = time.Now()
			inst, err := p.Launch(runCtx, provider.Spec{
				Name: g.name, Image: env.image, VCPU: vcpu, Memory: config.ByteSize(vcpu) * config.GiB,
				Disk: disk, Command: command, Trust: provider.TrustTrusted,
				JITConfig: "not-a-real-registration",
			})
			if err != nil {
				g.err = fmt.Errorf("launch: %w", err)

				return
			}
			g.launched = time.Now()
			target, err := p.UsageTarget(runCtx, inst.ID)
			if err != nil {
				g.err = fmt.Errorf("usage target: %w", err)

				return
			}
			monitor.Start(g.name, usage.Target{CgroupDir: target.CgroupDir, PID: target.PID,
				PIDStart: target.PIDStart, VCPUThreadPrefix: target.VCPUThreadPrefix,
				NetDevice: target.NetDevice, NetHostView: target.NetHostView}, vcpu)
			if res, err := resourcesOf(p.jailFor(g.name)); err == nil {
				g.cpus = res.CPUs
			}

			if placement != "" && g.cpus == "" {
				g.err = fmt.Errorf("the guest was placed on no cpus under %s", placement)

				return
			}

			// THE GUEST REBOOTS ONLY WHEN ITS WORKLOAD SUCCEEDED, and the VMM exits
			// with it: that moment is the run's end. A workload that failed halts the
			// guest instead, which leaves the VMM running, so it is never read as a
			// finished run.
			for {
				pid, err := p.vmmPID(p.jailFor(g.name))
				if err == nil && pid == 0 {
					g.exited = time.Now()
					g.summary, _ = monitor.Final(g.name)

					return
				}
				select {
				case <-runCtx.Done():
					g.err = fmt.Errorf("the guest did not power off by %s: its workload failed or "+
						"never finished", waitUntil.Format(time.TimeOnly))

					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
	}
	wg.Wait()
	finished := time.Now()
	// THE WHOLE HOST'S BUSY CPU OVER THE RUN, so what no guest's cgroup was
	// charged with is a number beside the energy no guest was charged with.
	hostBusySeconds := float64(hostBusyJiffies(t)-hostBusyBefore) / userHZ

	file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for i, g := range guests {
		line := map[string]any{
			"run": os.Getenv("BILLET_PLACEMENT_EXPERIMENT_RUN"), "placement": placementName(placement),
			"workload": workload, "vms": vms, "vcpu": vcpu, "guest": i, "cpus": g.cpus,
			"run_started_epoch": started.Unix(), "run_finished_epoch": finished.Unix(),
			"host_busy_cpu_seconds": hostBusySeconds,
		}
		if g.err != nil {
			line["error"] = g.err.Error()
		} else {
			s := g.summary
			line["launch_seconds"] = g.launched.Sub(g.launchedAt).Seconds()
			line["launched_epoch_ms"] = g.launched.UnixMilli()
			line["exited_epoch_ms"] = g.exited.UnixMilli()
			line["run_seconds"] = g.exited.Sub(g.launched).Seconds()
			line["guest_cpu_seconds"] = float64(s.Latest.GuestCPU) / 1e6
			line["vmm_cpu_seconds"] = float64(s.Latest.VMMCPU) / 1e6
			line["cgroup_cpu_seconds"] = float64(s.Latest.CPUUsage) / 1e6
			line["memory_peak_bytes"] = s.Latest.MemoryPeak
			line["energy_active_joules"] = float64(s.EnergyActive) / 1e6
			line["energy_idle_joules"] = float64(s.EnergyIdle) / 1e6
			if c := s.Counters; c != nil {
				line["cycles"] = c.Values[usage.Cycles]
				line["instructions"] = c.Values[usage.Instructions]
				line["cache_references"] = c.Values[usage.CacheReferences]
				line["cache_misses"] = c.Values[usage.CacheMisses]
				line["frontend_stalls"] = c.Values[usage.FrontendStalls]
			}
		}
		if err := encoder.Encode(line); err != nil {
			t.Fatalf("write the results: %v", err)
		}
		if g.err != nil {
			t.Errorf("guest %d: %v", i, g.err)
		}
	}
	t.Logf("%d guests under %s ran %s in all; results in %s", vms, placementName(placement),
		finished.Sub(started).Round(time.Second), filepath.Clean(out))
}

// experimentWorkloads are commands the runner image can run with no network.
// Each reboots the guest when it succeeds, which is what ends a Firecracker
// VMM, and halts it when it fails, which leaves the VMM running for the host to
// time out.
//
// EVERY FALLIBLE STEP IS ITS OWN ASSIGNMENT, under pipefail: `export X=$(...)`
// answers with export's status and hides the substitution's, which set -e would
// then never see.
const workloadEnd = `trap 'if [ $? -eq 0 ]; then sudo -n reboot -f; else sudo -n halt -f; fi' EXIT
set -eo pipefail
`

var experimentWorkloads = map[string][]string{
	"gostd": {"/bin/bash", "-c", workloadEnd + `gobin=$(ls -d /opt/hostedtoolcache/go/1.26*/x64/bin | tail -1)
test -x "$gobin/go"
procs=$(nproc)
export HOME=/tmp GOTOOLCHAIN=local GOFLAGS=-p=$procs PATH=$gobin:$PATH
for build in 1 2; do
	cache=$(mktemp -d)
	GOCACHE=$cache "$gobin/go" build -a std
done`},
	"memory": {"/bin/bash", "-c", workloadEnd + `python3 -c '
import multiprocessing as m
def copy(_):
    a = bytearray(256 << 20)
    b = bytearray(256 << 20)
    for _ in range(600):
        b[:] = a
n = m.cpu_count()
m.Pool(n).map(copy, range(n))
'`},
}

func placementName(placement string) string {
	if placement == "" {
		return "none"
	}

	return placement
}

func experimentInt(t *testing.T, name string, fallback int) int {
	t.Helper()

	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Fatalf("%s is %q; expected a positive number", name, raw)
	}

	return n
}

// userHZ is the kernel's clock tick for /proc/stat, 100 on every Linux billet
// runs on.
const userHZ = 100

// hostBusyJiffies is /proc/stat's cpu line less idle and iowait: everything
// the whole host's cpus did.
func hostBusyJiffies(t *testing.T) int64 {
	t.Helper()

	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		t.Fatalf("read /proc/stat: %v", err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(line)
	if len(fields) < 8 || fields[0] != "cpu" {
		t.Fatalf("/proc/stat begins %q", line)
	}
	var busy int64
	for i, field := range fields[1:] {
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			t.Fatalf("/proc/stat cpu field %d is %q", i, field)
		}
		// idle and iowait are the fourth and fifth; guest time is already in user.
		if i == 3 || i == 4 || i >= 8 {
			continue
		}
		busy += n
	}

	return busy
}
