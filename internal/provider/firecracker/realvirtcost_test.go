package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// guest's RAM, and measured by the node's own sampler through its cgroup. A
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
// WHAT EACH MODE SHARES WITH THE GUEST: its cpus, the guest's RAM as its memory
// limit, and its /tmp, where the compile keeps its caches, on a clone of the golden
// on Ceph as the guest's root is. A process runs the guest's very binaries,
// chrooted into that clone in a mount namespace of its own; a container runs them
// from an image of the golden's filesystem. What differs is what is measured: the
// virtualization.
//
// HOW AN INSTANCE ENDS, so its counters are read while its cgroup still exists:
// its body runs, its exit status is written to a file of its own, and a sleep holds
// the cgroup until the final reading is taken and the instance is stopped. Every
// clone, mount, unit and container is torn down by a cleanup registered before the
// step that makes it, which runs whichever way the test ends.
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
	if strings.Contains(body, "reboot") || strings.Contains(body, "halt") || strings.Contains(body, "poweroff") {
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

	// THE WHOLE RUN ENDS WELL BEFORE THE TEST DOES, so every cleanup has time to
	// run before the test binary's own timeout kills it.
	deadline := time.Now().Add(45 * time.Minute)
	if d, ok := t.Deadline(); ok && d.Add(-10*time.Minute).Before(deadline) {
		deadline = d.Add(-10 * time.Minute)
	}
	runCtx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()

	monitor := usage.NewMonitor("/", usage.Options{Interval: time.Second, RAPL: true, IdleWatts: idle})
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(t.Context())
	}()
	t.Cleanup(func() { <-done })

	type instance struct {
		name, cpus, root, rc string
		started, exited      time.Time
		summary              usage.Summary
		final                bool
		err                  error
	}
	all := make([]*instance, instances)
	for i := range all {
		// ONE THREAD OF EACH OF CCD i's FIRST CORES, as spread places a guest of
		// this shape on an empty host: cpu N's SMT sibling is N+64.
		name := "virtcost-" + freshLease(t)[:12]
		all[i] = &instance{name: name, cpus: fmt.Sprintf("%d-%d", 8*i, 8*i+vcpu-1),
			root: filepath.Join(virtcostRoots, name), rc: filepath.Join(virtcostRoots, name+".rc")}
	}

	// EVERY INSTANCE'S ROOT IS A CLONE OF THE GOLDEN, mounted before any starts so
	// a clone's time is never a run's. The discard and the unmount are registered
	// before the clone and the mount, so a step that half succeeded is undone too;
	// cleanups run last first, so the unmount comes before the discard.
	for _, in := range all {
		t.Cleanup(func() {
			if err := env.disk.DiscardRoot(cleanupContext(t), in.name); err != nil {
				t.Errorf("discard %s's clone: %v", in.name, err)
			}
		})
		dev, err := env.disk.CloneRoot(runCtx, env.image, in.name, 0)
		if err != nil {
			t.Fatalf("clone the golden for %s: %v", in.name, err)
		}
		if err := os.MkdirAll(in.root, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := cleanupContext(t)
			if mounted(in.root) {
				if out, err := exec.CommandContext(ctx, "umount", in.root).CombinedOutput(); err != nil {
					t.Errorf("unmount %s's clone: %v: %s", in.name, err, out)
				}
			}
			for _, leftover := range []string{in.root, in.rc, in.rc + ".d/rc", in.rc + ".d"} {
				if err := os.Remove(leftover); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Logf("remove %s: %v", leftover, err)
				}
			}
		})
		if out, err := exec.CommandContext(runCtx, "mount", dev, in.root).CombinedOutput(); err != nil {
			t.Fatalf("mount %s's clone: %v: %s", in.name, err, out)
		}
	}

	hostBusyBefore := hostBusyJiffies(t)
	started := time.Now()

	var wg sync.WaitGroup
	for _, in := range all {
		// THE INSTANCE IS STOPPED BEFORE ITS ROOT IS UNMOUNTED: this cleanup is
		// registered after the mount's, so it runs first.
		t.Cleanup(func() {
			if err := stopInstance(cleanupContext(t), mode, in.name); err != nil {
				t.Errorf("stop %s: %v", in.name, err)
			}
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.err = runInstance(runCtx, monitor, mode, image, in.name, in.root, in.rc, in.cpus, memory, vcpu,
				body, &in.started, &in.exited, &in.summary, &in.final)
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
			"memory_limit_bytes": int64(memory), "tmp": "golden clone (ext4 on rbd)",
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
			gaps := map[string]bool{}
			for _, point := range s.Points {
				if point.Gap != usage.GapNone {
					gaps[point.Gap.String()] = true
				}
			}
			line["gaps"] = slices.Sorted(maps.Keys(gaps))
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

// virtcostRoots is where an instance's clone of the golden is mounted and its exit
// status written.
const virtcostRoots = "/run/billet-virtcost"

// cleanupContext bounds one cleanup step, apart from the test's own context, which
// has ended by the time cleanups run.
func cleanupContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	t.Cleanup(cancel)

	return ctx
}

// runInstance starts one instance, measures it from the moment it is running
// until its body has written its exit status, and takes the final reading while
// its cgroup still holds it. Started is when the unit or container was running;
// the guest's equivalent is when Launch returned.
func runInstance(ctx context.Context, monitor *usage.Monitor, mode, image, name, root, rc, cpus string,
	memory config.ByteSize, vcpu int, body string, started, exited *time.Time, summary *usage.Summary,
	final *bool,
) error {
	var dir string
	switch mode {
	case "process":
		// A TRANSIENT SERVICE, so stopping it kills everything in its cgroup, and
		// a mount namespace of its own, so proc, the devices and /dev/shm go with
		// it and nothing is mounted on the host's own tree. /tmp is the clone's.
		inner := `set -u
R=$1
mount -t proc proc "$R/proc" && mount --rbind /dev "$R/dev" && mount -t tmpfs tmpfs "$R/dev/shm" || exit 1
chroot "$R" /bin/bash -c "$2"
echo $? >"$3"
exec sleep infinity`
		unit := name + ".service"
		if out, err := exec.CommandContext(ctx, "systemd-run", "--quiet", "--unit="+unit, "--collect",
			"-p", "AllowedCPUs="+cpus, "-p", fmt.Sprintf("MemoryMax=%d", int64(memory)), "-p", "MemorySwapMax=0",
			"--", "unshare", "--mount", "--propagation", "private", "--", "/bin/sh", "-c", inner, "sh",
			root, body, rc).CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run: %w: %s", err, out)
		}
		*started = time.Now()
		cgroupRoot, err := cgroup2Mount(defaultProcMountsPath)
		if err != nil {
			return err
		}
		dir = filepath.Join(cgroupRoot, "system.slice", unit)
	default:
		mem := strconv.FormatInt(int64(memory), 10)
		wrapper := `bash -c "$0"; echo $? >/billet-rc/rc; exec sleep infinity`
		rcDir := rc + ".d"
		if err := os.MkdirAll(rcDir, 0o700); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--label", "billet-virtcost=1",
			"--cpuset-cpus", cpus, "--memory", mem, "--memory-swap", mem,
			"-v", filepath.Join(root, "tmp")+":/tmp", "-v", rcDir+":/billet-rc",
			image, "/bin/bash", "-c", wrapper, body).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker run: %w: %s", err, out)
		}
		*started = time.Now()
		id := strings.TrimSpace(string(out))
		if dir, err = containerCgroup(ctx, name, id); err != nil {
			return err
		}
		rc = filepath.Join(rcDir, "rc")
	}

	// THE CGROUP IS THE INSTANCE'S OWN, read back with the limits it was given,
	// so a lookup that found another cgroup is never measured as this one.
	if err := cgroupHolds(ctx, dir, cpus, memory); err != nil {
		return err
	}
	monitor.Start(name, usage.Target{CgroupDir: dir}, vcpu)
	defer monitor.Forget(name)

	for {
		raw, err := os.ReadFile(rc)
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			*exited = time.Now()
			*summary, *final = monitor.Final(name)
			if code := strings.TrimSpace(string(raw)); code != "0" {
				return fmt.Errorf("the workload exited %s", code)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the workload had not finished by the run's deadline: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stopInstance stops whatever of an instance is still there, and reports it gone
// only once nothing of it runs. Stopping one that never started is not an error.
func stopInstance(ctx context.Context, mode, name string) error {
	if mode == "process" {
		unit := name + ".service"
		out, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "not loaded") {
			return fmt.Errorf("systemctl stop: %w: %s", err, out)
		}
		state, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "--value", unit).Output()
		if err == nil && strings.TrimSpace(string(state)) == "active" {
			return fmt.Errorf("%s is still active after its stop", unit)
		}

		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %w: %s", err, out)
	}

	return nil
}

// containerCgroup is the cgroup of the container docker just started, found by its
// full id in its init process's own cgroup path.
func containerCgroup(ctx context.Context, name, id string) (string, error) {
	if len(id) != 64 {
		return "", fmt.Errorf("docker answered %q, not a container id", id)
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Id}} {{.State.Pid}}", name).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != id {
		return "", fmt.Errorf("docker inspect of %s answered %q, not container %s", name, out, id)
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 0 {
		return "", fmt.Errorf("container %s has no running process (%q)", name, fields[1])
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", fmt.Errorf("read the container's cgroup: %w", err)
	}
	defer f.Close()
	var path string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		if rest, ok := strings.CutPrefix(scan.Text(), "0::"); ok {
			path = rest
		}
	}
	if err := scan.Err(); err != nil {
		return "", fmt.Errorf("read the container's cgroup: %w", err)
	}
	if !strings.Contains(path, id) {
		return "", fmt.Errorf("the container's process is in %q, which does not name container %s", path, id)
	}
	root, err := cgroup2Mount(defaultProcMountsPath)
	if err != nil {
		return "", err
	}

	return filepath.Join(root, path), nil
}

// cgroupHolds refuses a cgroup that does not confine its instance to exactly the
// cpus and memory it was given, waiting briefly while it is still being set up.
func cgroupHolds(ctx context.Context, dir, cpus string, memory config.ByteSize) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		gotCPUs, errCPUs := os.ReadFile(filepath.Join(dir, "cpuset.cpus"))
		gotMem, errMem := os.ReadFile(filepath.Join(dir, "memory.max"))
		if errCPUs == nil && errMem == nil {
			if strings.TrimSpace(string(gotCPUs)) != cpus {
				return fmt.Errorf("%s confines its instance to cpus %q, not %s", dir, gotCPUs, cpus)
			}
			if strings.TrimSpace(string(gotMem)) != strconv.FormatInt(int64(memory), 10) {
				return fmt.Errorf("%s limits its instance's memory to %q, not %d", dir, gotMem, int64(memory))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.Join(fmt.Errorf("%s's limits could not be read", dir), errCPUs, errMem)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// mounted reports whether dir is a mount point, by its own line in /proc/mounts.
func mounted(dir string) bool {
	raw, err := os.ReadFile(defaultProcMountsPath)
	if err != nil {
		return true // could not tell: try the unmount, which says if there was none
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == dir {
			return true
		}
	}

	return false
}
