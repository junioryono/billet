package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/usage"
)

// THE VIRTUALIZATION COST EXPERIMENT's other two thirds: the placement
// experiment's workloads (experimentBodies) run in a Docker container and as a
// plain process on the host, each instance confined to the cpus `spread` gives a
// guest of the same shape (one thread of each of its CCD's first cores) and to the
// guest's memory, and measured by the node's own sampler through its cgroup. A
// microVM of the same shape is TestRealPlacementExperiment under spread; the
// three write the same lines to one results file, which
// docs/reference/records/virtualization-cost.md compares.
//
// It skips unless BILLET_VIRT_EXPERIMENT_OUT names a results file, on top of
// everything requireRealHost needs, and it is meant for a drained node: its
// instances run beside nothing billet placed, on cpus nothing else is pinned to.
//
// Settings, all from the environment:
//   - BILLET_VIRT_EXPERIMENT_MODE: container or process.
//   - BILLET_VIRT_EXPERIMENT_INSTANCES and _VCPU: how many, of how many cpus,
//     at most 8 of at most 8, so each has a CCD of its own as spread gives.
//   - BILLET_VIRT_EXPERIMENT_WORKLOAD: gostd or memory.
//   - BILLET_VIRT_EXPERIMENT_IMAGE: for container, the Docker image to run, one
//     imported from the golden's own filesystem so the workload's binaries are
//     the guest's.
//   - BILLET_VIRT_EXPERIMENT_RUN: a label written on every line.
//   - BILLET_VIRT_EXPERIMENT_IDLE_WATTS: the package's idle power, so each
//     instance's share of the energy above it is what the node would attribute.
//
// A PROCESS RUNS IN A CHROOT OF A CLONE OF THE GOLDEN, in a mount namespace of its
// own with proc, the host's devices and a tmpfs /tmp and /dev/shm, so it runs the
// guest's very binaries with nothing of the host's but its kernel. Its cgroup is a
// transient systemd scope. The workload's body never carries workloadEnd, which on
// the host would reboot the machine.
func TestRealVirtualizationCost(t *testing.T) {
	out := os.Getenv("BILLET_VIRT_EXPERIMENT_OUT")
	if out == "" {
		t.Skip("set BILLET_VIRT_EXPERIMENT_OUT to a results file to run the virtualization cost experiment")
	}
	env := requireRealHost(t)

	mode := os.Getenv("BILLET_VIRT_EXPERIMENT_MODE")
	if mode != "container" && mode != "process" {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_MODE is %q; expected container or process", mode)
	}
	instances := experimentInt(t, "BILLET_VIRT_EXPERIMENT_INSTANCES", 4)
	vcpu := experimentInt(t, "BILLET_VIRT_EXPERIMENT_VCPU", 8)
	if instances < 1 || instances > 8 || vcpu < 1 || vcpu > 8 {
		t.Fatalf("%d instances of %d cpus: at most 8 of at most 8, so each has a CCD of its own", instances, vcpu)
	}
	workload := os.Getenv("BILLET_VIRT_EXPERIMENT_WORKLOAD")
	body, ok := experimentBodies[workload]
	if !ok || workload == "noop" {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_WORKLOAD is %q; expected gostd or memory", workload)
	}
	if strings.Contains(body, "reboot") || strings.Contains(body, "halt") {
		t.Fatalf("the %s body would power off the host it runs on", workload)
	}
	image := os.Getenv("BILLET_VIRT_EXPERIMENT_IMAGE")
	if mode == "container" && image == "" {
		t.Fatal("BILLET_VIRT_EXPERIMENT_IMAGE names no image for the container")
	}
	idle, err := strconv.ParseFloat(os.Getenv("BILLET_VIRT_EXPERIMENT_IDLE_WATTS"), 64)
	if err != nil {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_IDLE_WATTS: %v", err)
	}
	memory := config.ByteSize(vcpu) * config.GiB

	monitor := usage.NewMonitor("/", usage.Options{Interval: time.Second, RAPL: true, IdleWatts: idle})
	done := make(chan struct{})
	ctx := t.Context()
	go func() {
		defer close(done)
		monitor.Run(ctx)
	}()
	t.Cleanup(func() { <-done })

	type instance struct {
		name, cpus, root string
		started, exited  time.Time
		summary          usage.Summary
		final            bool
		err              error
	}
	all := make([]*instance, instances)
	for i := range all {
		// ONE THREAD OF EACH OF CCD i's FIRST CORES, as spread places a guest of
		// this shape on an empty host: cpu N's SMT sibling is N+64.
		all[i] = &instance{name: "virtcost-" + freshLease(t)[:12],
			cpus: fmt.Sprintf("%d-%d", 8*i, 8*i+vcpu-1)}
	}

	// A PROCESS'S ROOT IS A CLONE OF THE GOLDEN, mounted before any instance
	// starts so a clone's time is never a run's.
	if mode == "process" {
		for _, in := range all {
			dev, err := env.disk.CloneRoot(ctx, env.image, in.name, 0)
			if err != nil {
				t.Fatalf("clone the golden for %s: %v", in.name, err)
			}
			t.Cleanup(func() {
				if err := env.disk.DiscardRoot(context.WithoutCancel(t.Context()), in.name); err != nil {
					t.Errorf("discard %s's clone: %v", in.name, err)
				}
			})
			in.root = filepath.Join(virtcostRoots, in.name)
			if err := os.MkdirAll(in.root, 0o700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(ctx, "mount", dev, in.root).CombinedOutput(); err != nil {
				t.Fatalf("mount %s's clone: %v: %s", in.name, err, out)
			}
			t.Cleanup(func() {
				if out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "umount", in.root).
					CombinedOutput(); err != nil {
					t.Errorf("unmount %s's clone: %v: %s", in.name, err, out)
				}
				_ = os.Remove(in.root)
			})
		}
	}

	hostBusyBefore := hostBusyJiffies(t)
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, 40*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	for _, in := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.err = func() error {
				switch mode {
				case "process":
					return runProcess(runCtx, monitor, in.name, in.root, in.cpus, memory, vcpu, body,
						&in.started, &in.exited, &in.summary, &in.final)
				default:
					return runContainer(runCtx, monitor, in.name, image, in.cpus, memory, vcpu, body,
						&in.started, &in.exited, &in.summary, &in.final)
				}
			}()
		}()
	}
	wg.Wait()
	finished := time.Now()
	hostBusySeconds := float64(hostBusyJiffies(t)-hostBusyBefore) / userHZ

	file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for i, in := range all {
		line := map[string]any{
			"run": os.Getenv("BILLET_VIRT_EXPERIMENT_RUN"), "mode": mode, "placement": "spread",
			"workload": workload, "vms": instances, "vcpu": vcpu, "guest": i, "cpus": in.cpus,
			"run_started_epoch": started.Unix(), "run_finished_epoch": finished.Unix(),
			"host_busy_cpu_seconds": hostBusySeconds,
		}
		if in.err != nil {
			line["error"] = in.err.Error()
		} else {
			s := in.summary
			line["launched_epoch_ms"] = in.started.UnixMilli()
			line["exited_epoch_ms"] = in.exited.UnixMilli()
			line["run_seconds"] = in.exited.Sub(in.started).Seconds()
			line["sampler_known"] = in.final
			if s.Measured.CPU {
				line["cgroup_cpu_seconds"] = float64(s.Latest.CPUUsage) / 1e6
			}
			if s.Measured.Memory {
				line["memory_peak_bytes"] = s.MemoryPeak
			}
			if s.Measured.Energy {
				line["energy_active_joules"] = float64(s.EnergyActive) / 1e6
				line["energy_idle_joules"] = float64(s.EnergyIdle) / 1e6
			}
		}
		if err := encoder.Encode(line); err != nil {
			t.Fatalf("write the results: %v", err)
		}
		if in.err != nil {
			t.Errorf("instance %d: %v", i, in.err)
		}
	}
	t.Logf("%d %s instances ran %s in all; results in %s", instances, mode,
		finished.Sub(started).Round(time.Second), filepath.Clean(out))
}

// runProcess runs body as a plain process chrooted into root, in a transient
// systemd scope confined to cpus and memory, and measures it until it exits.
func runProcess(ctx context.Context, monitor *usage.Monitor, name, root, cpus string,
	memory config.ByteSize, vcpu int, body string, started, exited *time.Time, summary *usage.Summary,
	final *bool,
) error {
	// A MOUNT NAMESPACE OF ITS OWN, so proc, the devices and the tmpfs mounts go
	// with the process and nothing is mounted on the host's own tree.
	inner := `set -e
R=$1
mount -t proc proc "$R/proc"
mount --rbind /dev "$R/dev"
mount -t tmpfs tmpfs "$R/dev/shm"
mount -t tmpfs tmpfs "$R/tmp"
exec chroot "$R" /bin/bash -c "$2"`
	cmd := exec.CommandContext(ctx, "systemd-run", "--scope", "--quiet", "--unit="+name,
		"-p", "AllowedCPUs="+cpus, "-p", fmt.Sprintf("MemoryMax=%d", int64(memory)), "-p", "MemorySwapMax=0",
		"--", "unshare", "--mount", "--propagation", "private", "--", "/bin/sh", "-c", inner, "sh", root, body)
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	*started = time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	// systemd-run --scope execs the command in its own process, so this pid is
	// the scope's first process, whose cgroup is the scope.
	dir, err := cgroupOf(ctx, cmd.Process.Pid)
	if err != nil {
		// THE SCOPE IS STOPPED WHATEVER ELSE FAILED: a workload left running would
		// hold its cpus through the next run.
		return errors.Join(err, cmd.Process.Kill(), cmd.Wait())
	}
	monitor.Start(name, usage.Target{CgroupDir: dir}, vcpu)
	defer monitor.Forget(name)
	waitErr := cmd.Wait()
	*exited = time.Now()
	*summary, *final = monitor.Final(name)
	if waitErr != nil {
		return fmt.Errorf("the workload failed: %w: %s", waitErr, tail(output.String()))
	}

	return nil
}

// runContainer runs body in a Docker container of image confined to cpus and
// memory, and measures it until it exits.
func runContainer(ctx context.Context, monitor *usage.Monitor, name, image, cpus string,
	memory config.ByteSize, vcpu int, body string, started, exited *time.Time, summary *usage.Summary,
	final *bool,
) (err error) {
	mem := strconv.FormatInt(int64(memory), 10)
	*started = time.Now()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--cpuset-cpus", cpus,
		"--memory", mem, "--memory-swap", mem, "--tmpfs", "/tmp", image, "/bin/bash", "-c", body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run: %w: %s", err, out)
	}
	// THE CONTAINER IS REMOVED HOWEVER THIS RETURNS, so a run that failed half way
	// leaves nothing holding its cpus. A context of its own, since the run's may be
	// the reason it is returning.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if out, rmErr := exec.CommandContext(rmCtx, "docker", "rm", "-f", name).CombinedOutput(); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the container: %w: %s", rmErr, out))
		}
	}()
	pidOut, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Pid}}", name).Output()
	if err != nil {
		return fmt.Errorf("docker inspect: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		return fmt.Errorf("the container's pid is %q: %w", pidOut, err)
	}
	if pid <= 0 {
		return fmt.Errorf("the container's pid is %d: it is not running", pid)
	}
	dir, err := cgroupOf(ctx, pid)
	if err != nil {
		return err
	}
	monitor.Start(name, usage.Target{CgroupDir: dir}, vcpu)
	defer monitor.Forget(name)
	code, err := exec.CommandContext(ctx, "docker", "wait", name).Output()
	*exited = time.Now()
	*summary, *final = monitor.Final(name)
	if err != nil {
		return fmt.Errorf("docker wait: %w", err)
	}
	if c := strings.TrimSpace(string(code)); c != "0" {
		logs, err := exec.CommandContext(context.WithoutCancel(ctx), "docker", "logs", "--tail", "20", name).
			CombinedOutput()
		if err != nil {
			logs = []byte("its log could not be read: " + err.Error())
		}
		return fmt.Errorf("the workload exited %s: %s", c, tail(string(logs)))
	}

	return nil
}

// cgroupOf is the cgroup-v2 directory a process is in, read from its own
// /proc/<pid>/cgroup, waited for briefly while a scope or a container's cgroup is
// still being set up around it.
func cgroupOf(ctx context.Context, pid int) (string, error) {
	root, err := cgroup2Mount(defaultProcMountsPath)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			return "", fmt.Errorf("read the cgroup of %d: %w", pid, err)
		}
		var path string
		scan := bufio.NewScanner(f)
		for scan.Scan() {
			if rest, ok := strings.CutPrefix(scan.Text(), "0::"); ok {
				path = rest
			}
		}
		f.Close()
		// A SCOPE OR A CONTAINER, never the slice its launcher ran in.
		if strings.HasSuffix(path, ".scope") || strings.Contains(path, "docker") {
			return filepath.Join(root, path), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("process %d is in %q, not a scope or container of its own", pid, path)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// virtcostRoots is where a process instance's clone of the golden is mounted.
const virtcostRoots = "/run/billet-virtcost"

// tail is the last lines of a command's output, for an error.
func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}

	return strings.Join(lines, " | ")
}
