package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// cgroupMount is where a cgroup-v2 host mounts the unified hierarchy.
const cgroupMount = "/sys/fs/cgroup"

// containerInterface is the container's own end of its network: its first
// interface, which on Docker's default bridge is the only one besides lo.
const containerInterface = "eth0"

// UsageTarget says where a running container's counters are: the cgroup its
// init process is in, read from /proc, so the answer is the same under
// Docker's systemd driver (system.slice/docker-<id>.scope) and its cgroupfs
// driver (docker/<id>); and, for a container with a network namespace of its
// own, its eth0, read through that process's /proc/<pid>/net/dev, which is
// the namespace's own table, so the host never enters it and never has to
// guess which veth is the container's.
func (p *Provider) UsageTarget(ctx context.Context, instanceID string) (provider.UsageTarget, error) {
	out, err := p.run(ctx, "inspect", "--format", "{{.State.Pid}} {{.HostConfig.NetworkMode}}", instanceID)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	fields := strings.Fields(out)
	pid := 0
	if len(fields) > 0 {
		pid, err = strconv.Atoi(fields[0])
	}
	if len(fields) == 0 || err != nil || pid <= 0 {
		return provider.UsageTarget{}, fmt.Errorf("docker: container %s has no running process to measure", instanceID)
	}
	mode := ""
	if len(fields) == 2 {
		mode = fields[1]
	}

	return p.usageTargetOf(instanceID, pid, mode)
}

// ownsNetwork reports whether a container in network mode mode has a network
// namespace of its own: the default bridge or a named network. host and
// another container's namespace (container:<id>, or a path, ns:<path>) carry
// traffic that is not this job's, none has nothing to count, and a mode that
// could not be read is not proved to be either.
func ownsNetwork(mode string) bool {
	return mode != "" && mode != "host" && mode != "none" && !strings.Contains(mode, ":")
}

// usageTargetOf proves pid is container instanceID's init and says where its
// counters are.
//
// THE START TIME IS READ BETWEEN TWO PROOFS, the order firecracker reads its
// VMM's in: the pid inspect returned can have exited and been reused at any
// point, and a start read from a process that took the pid would let every
// later sample prove that process is the container. Both proofs naming the
// same cgroup is what makes the start the container's.
//
// THE NETWORK IS COUNTED ONLY IN A NAMESPACE OF THE CONTAINER'S OWN
// (ownsNetwork): a container started in the host's or another container's
// namespace passes every pid proof, and its eth0 is someone else's traffic.
func (p *Provider) usageTargetOf(instanceID string, pid int, networkMode string) (provider.UsageTarget, error) {
	rel, err := p.containerCgroup(instanceID, pid)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	start, err := p.processStart(pid)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("docker: read the start time of container %s's pid %d: %w",
			instanceID, pid, err)
	}
	if again, err := p.containerCgroup(instanceID, pid); err != nil || again != rel {
		return provider.UsageTarget{}, fmt.Errorf("docker: container %s's pid %d changed hands while it "+
			"was being read", instanceID, pid)
	}

	target := provider.UsageTarget{CgroupDir: filepath.Join(cgroupMount, rel), PID: pid, PIDStart: start}
	if ownsNetwork(networkMode) {
		target.NetDevice, target.NetNamespace = containerInterface, true
	}

	return target, nil
}

// containerCgroup is pid's cgroup-v2 path, refused unless it names the
// container.
func (p *Provider) containerCgroup(instanceID string, pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join(p.procRoot, "proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("docker: read the cgroup of container %s: %w", instanceID, err)
	}
	rel, err := unifiedCgroup(string(raw))
	if err != nil {
		return "", fmt.Errorf("docker: container %s: %w", instanceID, err)
	}
	// THE CGROUP MUST NAME THE CONTAINER, under either driver: the pid inspect
	// returned can have exited and been reused between the two reads, and a
	// reused pid's cgroup is some other process's.
	if !cgroupNames(rel, instanceID) {
		return "", fmt.Errorf("docker: pid %d is in %s, which is not container %s's",
			pid, rel, instanceID)
	}

	return rel, nil
}

// hostProcessStart reads a pid's start time from the real /proc.
func hostProcessStart(pid int) (uint64, error) { return usage.Reader{Root: "/"}.ProcessStart(pid) }

// unifiedCgroup reads the cgroup-v2 path from a /proc/<pid>/cgroup file, whose
// unified entry is the line "0::<path>". A host still on cgroup v1 has other
// lines and no usable answer.
func unifiedCgroup(data string) (string, error) {
	for line := range strings.SplitSeq(data, "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		rel = path.Clean(rel)
		if !strings.HasPrefix(rel, "/") || rel == "/" || strings.Contains(rel, "..") {
			return "", fmt.Errorf("the unified cgroup %q is not a container's own", rel)
		}

		return rel, nil
	}

	return "", errors.New("no cgroup-v2 entry, so this host's containers cannot be measured")
}

// cgroupNames reports whether a cgroup path is a container's own: the last
// element is docker-<id>.scope (systemd driver) or <id> (cgroupfs driver).
func cgroupNames(rel, containerID string) bool {
	if containerID == "" {
		return false
	}
	last := path.Base(rel)

	return last == containerID || last == "docker-"+containerID+".scope"
}
