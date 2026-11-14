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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
// It skips unless BILLET_VIRT_EXPERIMENT_OUT names a results file, and it is
// meant for a drained node, as root: its instances run beside nothing billet
// placed, on cpus nothing else is pinned to.
//
// Settings, all from the environment:
//   - BILLET_VIRT_EXPERIMENT_MODE: container or process.
//   - BILLET_VIRT_EXPERIMENT_INSTANCES and _VCPU: how many, of how many cpus,
//     at most 8 of at most 8, so each has a CCD of its own as spread gives.
//   - BILLET_VIRT_EXPERIMENT_WORKLOAD: gostd or memory.
//   - BILLET_VIRT_EXPERIMENT_ROOT: for process, a read-only mount of the golden's
//     own filesystem, which the instances chroot into.
//   - BILLET_VIRT_EXPERIMENT_IMAGE: for container, a Docker image imported from
//     that same filesystem.
//   - BILLET_VIRT_EXPERIMENT_SCRATCH: a filesystem of its own, mounted for the run,
//     with at least scratchPerInstance free for each instance: their /tmp, go
//     files and statuses, so a compile that fills it fills nothing billet keeps.
//     It may not be the filesystem of /, billet's state or Docker's
//     (scratchProtected), however it is reached.
//   - BILLET_VIRT_EXPERIMENT_RUN: a label written on every line.
//   - BILLET_VIRT_EXPERIMENT_IDLE_WATTS: the package's idle power, so each
//     instance's share of the energy above it is what the node would attribute.
//
// IT TOUCHES NO CEPH. The golden reaches it once, out of band, as a read-only
// export of the generation's snapshot loop-mounted on the host's own disk
// (docs/reference/records/virtualization-cost.md), so nothing here maps, locks or
// removes an RBD image beside the node's own. Each instance's /tmp, where the
// compile keeps its caches, is a directory of its own on the scratch filesystem;
// the microVM keeps its root, /tmp included, on Ceph, as billet runs one.
//
// HOW AN INSTANCE RUNS, so it is measured from its first instruction to its last
// while its cgroup exists: it waits for a go file, which is written only once the
// sampler says it has the cgroup's baseline; its body runs; its exit status is
// written to a file of its own; and a sleep holds its cgroup until the final
// reading is taken. A setup that failed writes a status too, a status that could
// not be written ends the instance, and an instance whose unit or container died
// without one is a failure reported at once.
func TestRealVirtualizationCost(t *testing.T) {
	out := os.Getenv("BILLET_VIRT_EXPERIMENT_OUT")
	if out == "" {
		t.Skip("set BILLET_VIRT_EXPERIMENT_OUT to a results file to run the virtualization cost experiment")
	}
	if os.Geteuid() != 0 {
		t.Skip("not root: an instance runs as a transient service or a container with set cpus")
	}

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
	root, image := os.Getenv("BILLET_VIRT_EXPERIMENT_ROOT"), os.Getenv("BILLET_VIRT_EXPERIMENT_IMAGE")
	switch {
	case mode == "process" && !readOnlyMount(root):
		t.Fatalf("BILLET_VIRT_EXPERIMENT_ROOT %q is not a read-only mount", root)
	case mode == "container" && image == "":
		t.Fatal("BILLET_VIRT_EXPERIMENT_IMAGE names no image for the container")
	}
	scratch, err := scratchHolds(os.Getenv("BILLET_VIRT_EXPERIMENT_SCRATCH"), instances, scratchProtected)
	if err != nil {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_SCRATCH: %v", err)
	}
	// THE RESULTS FILE IS OPENED BEFORE ANYTHING STARTS, so a path that cannot take
	// them (a FIFO nobody reads, a device) is refused while there is nothing to clean
	// up, rather than blocking between the run and its cleanup.
	file, err := openResults(out)
	if err != nil {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_OUT: %v", err)
	}
	defer file.Close()
	idle, err := strconv.ParseFloat(os.Getenv("BILLET_VIRT_EXPERIMENT_IDLE_WATTS"), 64)
	if err != nil {
		t.Fatalf("BILLET_VIRT_EXPERIMENT_IDLE_WATTS: %v", err)
	}
	memory := config.ByteSize(vcpu) * config.GiB
	runID := freshLease(t)[:12]

	// THE WHOLE RUN ENDS WELL BEFORE THE TEST DOES: the one cleanup phase below is
	// bounded at five minutes, and this leaves it ten.
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

	all := make([]*virtInstance, instances)
	for i := range all {
		// ONE THREAD OF EACH OF CCD i's FIRST CORES, as spread places a guest of
		// this shape on an empty host: cpu N's SMT sibling is N+64.
		name := "virtcost-" + runID + "-" + strconv.Itoa(i)
		dir := filepath.Join(scratch, name)
		all[i] = &virtInstance{name: name, cpus: fmt.Sprintf("%d-%d", 8*i, 8*i+vcpu-1), dir: dir}
	}

	// ONE CLEANUP FOR EVERY INSTANCE, registered before any is made: stop them all
	// at once, then remove their directories, both phases within one five-minute
	// deadline of the ten minutes the run leaves. What the deadline cuts short is
	// reported, never removed after it.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Minute)
		defer cancel()
		var stop sync.WaitGroup
		errs := make([]error, len(all))
		for i, in := range all {
			stop.Add(1)
			go func() {
				defer stop.Done()
				var removed int
				removed, errs[i] = stopInstance(ctx, mode, in.name, runID)
				// A CREATE THAT FAILED is settled only by removing what it made: a failure
				// does not say whether the daemon made the container, and two empty reads
				// do not prove it will not register one.
				if errs[i] == nil && in.createUnsettled && removed == 0 {
					errs[i] = fmt.Errorf("its docker create failed and no container of it was found; "+
						"remove any container labelled billet-virtcost=%s if the daemon registers one", runID)
				}
			}()
		}
		stop.Wait()
		for i, in := range all {
			if errs[i] != nil {
				t.Errorf("stop %s: %v; its directory %s is left", in.name, errs[i], in.dir)
				continue
			}
			// BOUNDED, and on this filesystem only: rm is killed at the deadline, and
			// does not follow a mount a namespace left behind out of the scratch.
			if out, err := exec.CommandContext(ctx, "rm", "-rf", "--one-file-system", "--", in.dir).
				CombinedOutput(); err != nil {
				t.Errorf("remove %s: %v: %s; what is left of it stays", in.dir, err, out)
			}
		}
	})
	for _, in := range all {
		if err := os.MkdirAll(filepath.Join(in.dir, "tmp"), 0o1777); err != nil {
			t.Fatal(err)
		}
	}

	hostBusyBefore := hostBusyJiffies(t)
	started := time.Now()
	var wg sync.WaitGroup
	for _, in := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.err = in.run(runCtx, monitor, mode, root, image, runID, memory, vcpu, body)
		}()
	}
	wg.Wait()
	finished := time.Now()
	hostBusySeconds := float64(hostBusyJiffies(t)-hostBusyBefore) / userHZ

	encoder := json.NewEncoder(file)
	for i, in := range all {
		line := map[string]any{
			"run": os.Getenv("BILLET_VIRT_EXPERIMENT_RUN"), "mode": mode, "placement": "spread",
			"workload": workload, "vms": instances, "vcpu": vcpu, "guest": i, "cpus": in.cpus,
			"memory_limit_bytes": int64(memory), "tmp": "a directory on the scratch filesystem",
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

// scratchPerInstance is the free space the scratch filesystem needs for each
// instance: the gostd body keeps two fresh Go build caches of the standard library
// and its tests in /tmp, about 3 GiB together on the reference node.
const scratchPerInstance = 6 * config.GiB

// scratchProtected are the paths whose filesystems the scratch may not be: the
// root, billet's state (the ledger and the node's records) and Docker's storage.
// One that does not exist protects nothing.
var scratchProtected = []string{"/", "/var/lib/billet", "/var/lib/docker"}

// scratchHolds admits a scratch directory and answers the path to use: dir with
// every symlink resolved, so what is checked is what is written. It refuses one
// that is not a filesystem of its own, mounted there (its device differs from its
// parent's), one on the filesystem of any protected path, which a bind mount of
// that filesystem is, and one with less than scratchPerInstance free for each
// instance. What fills it then fills nothing the node or its ledger keeps.
func scratchHolds(dir string, instances int, protected []string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%q is not an absolute path", dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	var here, parent unix.Stat_t
	if err := unix.Stat(resolved, &here); err != nil {
		return "", err
	}
	if err := unix.Stat(filepath.Dir(resolved), &parent); err != nil {
		return "", err
	}
	if here.Dev == parent.Dev {
		return "", fmt.Errorf("%s is on its parent's filesystem; mount a filesystem of its own there", resolved)
	}
	for _, path := range protected {
		var kept unix.Stat_t
		err := unix.Stat(path, &kept)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("could not tell which filesystem %s is on: %w", path, err)
		}
		if here.Dev == kept.Dev {
			return "", fmt.Errorf("%s is on the filesystem %s is on", resolved, path)
		}
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(resolved, &fs); err != nil {
		return "", err
	}
	free, need := fs.Bavail*uint64(fs.Bsize), uint64(instances)*uint64(scratchPerInstance)
	if free < need {
		return "", fmt.Errorf("%s has %d bytes free; %d instances need %d", resolved, free, instances, need)
	}

	return resolved, nil
}

// openResults opens the results file to append to, refusing anything but a
// regular file without waiting on it: an existing path is looked at before it is
// opened, and the open neither follows a symlink nor blocks on a FIFO.
func openResults(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a %s, not a regular file", path, info.Mode().Type())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%s is not a regular file", path), err, file.Close())
	}

	return file, nil
}

// virtInstance is one container or process of the cost experiment.
type virtInstance struct {
	name, cpus, dir string
	started, exited time.Time
	summary         usage.Summary
	final           bool
	err             error
	// createUnsettled is a docker create that failed, which does not say whether
	// the daemon made the container; only its removal settles it.
	createUnsettled bool
}

// virtWrapper is what every instance runs: wait for the go file, run the body,
// write its status, and hold the cgroup. $1 is the body, $2 the go file and $3
// the status file, as the instance sees them. A status it could not write ends
// it, which its waiter sees at once, rather than holding a cgroup nobody will
// read a status from.
const virtWrapper = `while [ ! -e "$2" ]; do sleep 0.05; done
bash -c "$1"
rc=$?
{ printf '%s\n' "$rc" >"$3.part" && mv "$3.part" "$3"; } || exit 1
exec sleep infinity`

// run starts the instance, opens its go file once the sampler has its baseline,
// and measures it until its status is written, taking the final reading while
// its cgroup still holds it.
func (in *virtInstance) run(ctx context.Context, monitor *usage.Monitor, mode, root, image, runID string,
	memory config.ByteSize, vcpu int, body string,
) error {
	goFile, status := filepath.Join(in.dir, "go"), filepath.Join(in.dir, "status")
	var cgroupDir string
	switch mode {
	case "process":
		// A TRANSIENT SERVICE, so stopping it kills everything in its cgroup, its
		// wrapper kept through a workload process the OOM killer takes, and a mount
		// namespace of its own, so proc, the devices, /dev/shm and its /tmp go with
		// it and nothing is mounted on the host's own tree. A setup that fails
		// writes its status before it exits.
		inner := `set -u
R=$1 D=$2
fail() { echo 125 >"$D/status.part"; mv "$D/status.part" "$D/status"; exit 1; }
mount -t proc proc "$R/proc" || fail
mount --rbind /dev "$R/dev" || fail
mount -t tmpfs tmpfs "$R/dev/shm" || fail
mount --bind "$D" "$R/mnt" || fail
mount --bind "$D/tmp" "$R/tmp" || fail
exec chroot "$R" /bin/bash -c "$3" wrapper "$4" /mnt/go /mnt/status`
		unit := in.name + ".service"
		if out, err := exec.CommandContext(ctx, "systemd-run", "--quiet", "--unit="+unit, "--collect",
			"-p", "AllowedCPUs="+in.cpus, "-p", fmt.Sprintf("MemoryMax=%d", int64(memory)), "-p", "MemorySwapMax=0",
			"-p", "OOMPolicy=continue",
			"--", "unshare", "--mount", "--propagation", "private", "--", "/bin/sh", "-c", inner, "sh",
			root, in.dir, virtWrapper, body).CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-run: %w: %s", err, out)
		}
		cgroupRoot, err := cgroup2Mount(defaultProcMountsPath)
		if err != nil {
			return err
		}
		cgroupDir = filepath.Join(cgroupRoot, "system.slice", unit)
	default:
		// CREATED, THEN STARTED: a create that settled has a name and a run label
		// the cleanup removes by, and a start that failed leaves a container it
		// finds the same way.
		//
		// NOTHING BUT THE SCRATCH IS WRITTEN: the container's root is read-only and
		// it keeps no log, so Docker's storage, on the host's own filesystem, gets
		// only the container's record.
		mem := strconv.FormatInt(int64(memory), 10)
		out, err := exec.CommandContext(ctx, "docker", "create", "--name", in.name,
			"--label", "billet-virtcost="+runID, "--read-only", "--log-driver", "none",
			"--cpuset-cpus", in.cpus, "--memory", mem, "--memory-swap", mem,
			"-v", in.dir+":/mnt", "-v", filepath.Join(in.dir, "tmp")+":/tmp",
			image, "/bin/bash", "-c", virtWrapper, "wrapper", body, "/mnt/go", "/mnt/status").CombinedOutput()
		if err != nil {
			in.createUnsettled = true
			return fmt.Errorf("docker create: %w: %s", err, out)
		}
		id := strings.TrimSpace(string(out))
		if out, err := exec.CommandContext(ctx, "docker", "start", in.name).CombinedOutput(); err != nil {
			return fmt.Errorf("docker start: %w: %s", err, out)
		}
		if cgroupDir, err = containerCgroup(ctx, in.name, id); err != nil {
			return err
		}
	}

	// THE CGROUP IS THE INSTANCE'S OWN, read back with the cpus the kernel
	// applies and the memory it was given, so a lookup that found another cgroup
	// is never measured as this one.
	if err := cgroupHolds(ctx, cgroupDir, in.cpus, memory); err != nil {
		return err
	}
	monitor.Start(in.name, usage.Target{CgroupDir: cgroupDir}, vcpu)
	defer monitor.Forget(in.name)
	// THE BASELINE FIRST, as the sampler acknowledges it: Start can return with
	// it still pending, and a workload let go before it would be measured from
	// part way through.
	if err := baselined(ctx, monitor, in.name); err != nil {
		return err
	}
	in.started = time.Now()
	if err := os.WriteFile(goFile, nil, 0o644); err != nil {
		return fmt.Errorf("open the go file: %w", err)
	}

	for {
		raw, err := os.ReadFile(status)
		switch {
		case err == nil:
			in.exited = time.Now()
			in.summary, in.final = monitor.Final(in.name)
			if code := strings.TrimSpace(string(raw)); code != "0" {
				return fmt.Errorf("the workload exited %s", code)
			}
			return nil
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("read the workload's status: %w", err)
		}
		// AN INSTANCE THAT DIED WITHOUT A STATUS is a failure now, not at the
		// deadline.
		if alive, err := instanceAlive(ctx, mode, in.name); err != nil || !alive {
			if _, statErr := os.Stat(status); statErr == nil {
				continue
			}
			return errors.Join(fmt.Errorf("the instance ended without writing a status"), err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the workload had not finished by the run's deadline: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// baselined waits for the sampler to say it has an instance's baseline, which a
// Start it was too busy to take gets at its next tick.
func baselined(ctx context.Context, monitor *usage.Monitor, name string) error {
	deadline := time.Now().Add(10 * time.Second)
	for !monitor.Baselined(name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the sampler had no baseline for %s after 10s", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return nil
}

// instanceAlive reports whether an instance's unit or container is still running.
func instanceAlive(ctx context.Context, mode, name string) (bool, error) {
	if mode == "process" {
		out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "--value",
			name+".service").Output()
		if err != nil {
			return false, fmt.Errorf("systemctl show: %w", err)
		}
		return strings.TrimSpace(string(out)) == "active", nil
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false, fmt.Errorf("docker inspect: %w", err)
	}

	return strings.TrimSpace(string(out)) == "true", nil
}

// stopInstance stops whatever of an instance is still there and answers nil only
// on proof nothing of it runs: a service's cgroup gone, or no container left
// carrying the run's label, read twice a few seconds apart. It also says how many
// containers it removed, which is what settles a create that never answered.
func stopInstance(ctx context.Context, mode, name, runID string) (int, error) {
	if mode == "process" {
		unit := name + ".service"
		out, err := exec.CommandContext(ctx, "systemctl", "stop", unit).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "not loaded") {
			return 0, fmt.Errorf("systemctl stop: %w: %s", err, out)
		}
		cgroupRoot, err := cgroup2Mount(defaultProcMountsPath)
		if err != nil {
			return 0, err
		}
		dir := filepath.Join(cgroupRoot, "system.slice", unit)
		for {
			_, err := os.Stat(dir)
			if errors.Is(err, os.ErrNotExist) {
				return 0, nil
			}
			if err != nil {
				return 0, fmt.Errorf("could not tell whether %s is gone: %w", dir, err)
			}
			select {
			case <-ctx.Done():
				return 0, fmt.Errorf("%s still exists: %w", dir, ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	removed := 0
	for round := range 2 {
		out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=billet-virtcost="+runID,
			"--filter", "name=^"+name+"$").Output()
		if err != nil {
			return removed, fmt.Errorf("docker ps: %w", err)
		}
		for _, id := range strings.Fields(string(out)) {
			if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput(); err != nil {
				return removed, fmt.Errorf("docker rm %s: %w: %s", id, err, out)
			}
			removed++
		}
		if round == 0 {
			select {
			case <-ctx.Done():
				return removed, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}

	return removed, nil
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
// cpus and memory it was given: the cpus the kernel applies (cpuset.cpus.effective),
// compared as sets, waiting briefly while the cgroup is still being set up.
func cgroupHolds(ctx context.Context, dir, cpus string, memory config.ByteSize) error {
	want, err := parseCPUList(cpus)
	if err != nil {
		return fmt.Errorf("the instance's cpus %q: %w", cpus, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		gotCPUs, errCPUs := os.ReadFile(filepath.Join(dir, "cpuset.cpus.effective"))
		gotMem, errMem := os.ReadFile(filepath.Join(dir, "memory.max"))
		if errCPUs == nil && errMem == nil {
			got, err := parseCPUList(strings.TrimSpace(string(gotCPUs)))
			if err != nil {
				return fmt.Errorf("%s's cpus read %q: %w", dir, gotCPUs, err)
			}
			if formatCPUList(got) != formatCPUList(want) {
				return fmt.Errorf("%s confines its instance to cpus %s, not %s", dir, formatCPUList(got),
					formatCPUList(want))
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

// readOnlyMount reports whether dir is a mount point mounted read-only, by its own
// line in /proc/mounts.
func readOnlyMount(dir string) bool {
	if dir == "" {
		return false
	}
	raw, err := os.ReadFile(defaultProcMountsPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 3 && fields[1] == filepath.Clean(dir) {
			return slices.Contains(strings.Split(fields[3], ","), "ro")
		}
	}

	return false
}

// THE WRAPPER PUBLISHES ITS BODY'S STATUS OR ENDS: it holds its cgroup only once
// the status is written, and a status it cannot write ends it at once, which its
// waiter sees as an instance that died without one.
func TestTheVirtWrapperPublishesItsStatusOrEnds(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the wrapper holds its cgroup with GNU sleep infinity, which a BSD sleep refuses")
	}

	dir := t.TempDir()
	goFile, status := filepath.Join(dir, "go"), filepath.Join(dir, "status")
	if err := os.WriteFile(goFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := exec.CommandContext(t.Context(), "bash", "-c", virtWrapper, "wrapper", "exit 3", goFile, status)
	if err := held.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		defer close(exited)
		waitErr = held.Wait()
	}()
	// KILLED AND JOINED HOWEVER THE TEST ENDS, the wrapper and its sleep.
	t.Cleanup(func() {
		if err := held.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop the wrapper: %v", err)
		}
		<-exited
	})
	deadline := time.After(10 * time.Second)
	for {
		raw, err := os.ReadFile(status)
		if err == nil {
			if got := string(raw); got != "3\n" {
				t.Errorf("the wrapper published %q; want the body's status 3", got)
			}
			break
		}
		select {
		case <-exited:
			t.Fatalf("the wrapper ended (%v) without publishing a status it could write", waitErr)
		case <-deadline:
			t.Fatal("the wrapper published no status in 10s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case <-exited:
		t.Errorf("the wrapper ended (%v) after publishing; want it holding its cgroup", waitErr)
	case <-time.After(200 * time.Millisecond):
	}

	// Bounded, since a wrapper that held on without its status would never end.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	unwritable := filepath.Join(dir, "missing", "status")
	err := exec.CommandContext(ctx, "bash", "-c", virtWrapper, "wrapper", "true", goFile, unwritable).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("a wrapper that could not write its status answered %v; want it to end with status 1", err)
	}
}

// THE SCRATCH IS A FILESYSTEM OF ITS OWN: a directory on its parent's filesystem,
// where a compile that fills it fills what the node keeps, is refused, and so is
// one on a protected path's filesystem however it is reached, and one without room
// for its instances. What is admitted is the path with its symlinks resolved.
func TestTheScratchMustBeAFilesystemOfItsOwn(t *testing.T) {
	t.Parallel()

	if _, err := scratchHolds(t.TempDir(), 0, nil); err == nil {
		t.Error("a directory on its parent's filesystem was accepted as the scratch")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(wd, "/dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scratchHolds(relative, 0, nil); err == nil {
		t.Errorf("the relative path %s was accepted as the scratch", relative)
	}
	// /dev is a filesystem of its own on Linux and macOS alike, with nowhere near
	// a million instances' room.
	if got, err := scratchHolds("/dev", 0, nil); err != nil || got != "/dev" {
		t.Errorf("/dev, a mount of its own, was admitted as %q (%v); want it as itself", got, err)
	}
	if _, err := scratchHolds("/dev", 1<<20, nil); err == nil {
		t.Error("a scratch without room for its instances was accepted")
	}

	// A SYMLINK IS ADMITTED AS WHAT IT NAMES, and a protected path's filesystem is
	// refused through one, as /dev/shm/scratch -> /var/lib/billet would be.
	link := filepath.Join(t.TempDir(), "scratch")
	if err := os.Symlink("/dev", link); err != nil {
		t.Fatal(err)
	}
	if got, err := scratchHolds(link, 0, nil); err != nil || got != "/dev" {
		t.Errorf("a symlink to /dev was admitted as %q (%v); want /dev itself", got, err)
	}
	if _, err := scratchHolds(link, 0, []string{"/dev"}); err == nil {
		t.Error("a symlink to a protected path's filesystem was accepted as the scratch")
	}
	if got, err := scratchHolds("/dev", 0, []string{filepath.Join(t.TempDir(), "absent")}); err != nil ||
		got != "/dev" {
		t.Errorf("a protected path that does not exist refused /dev (%q, %v)", got, err)
	}
}

// THE RESULTS FILE IS A REGULAR FILE, opened without waiting: a FIFO nobody reads
// is refused at once rather than blocking the run before its cleanup, and so is a
// symlink, while a new file and an existing one are appended to.
func TestTheResultsFileIsARegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 1)
	go func() {
		f, err := openResults(fifo)
		if err == nil {
			_ = f.Close()
		}
		refused <- err
	}()
	select {
	case err := <-refused:
		if err == nil {
			t.Error("a FIFO was opened as the results file")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("opening a FIFO nobody reads as the results file blocked")
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openResults(link); err == nil {
		_ = f.Close()
		t.Error("a symlink was opened as the results file")
	}

	results := filepath.Join(dir, "results.jsonl")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := openResults(results)
		if err != nil {
			t.Fatalf("open a regular results file: %v", err)
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := os.ReadFile(results); err != nil || string(raw) != "one\ntwo\n" {
		t.Errorf("the results file holds %q (%v); want both lines appended", raw, err)
	}
}
