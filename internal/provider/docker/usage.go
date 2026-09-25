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
)

// cgroupMount is where a cgroup-v2 host mounts the unified hierarchy.
const cgroupMount = "/sys/fs/cgroup"

// UsageTarget says where a running container's counters are: the cgroup its
// init process is in, read from /proc, so the answer is the same under
// Docker's systemd driver (system.slice/docker-<id>.scope) and its cgroupfs
// driver (docker/<id>).
//
// Its network is not measured: a container's veth is not named after it, and
// guessing one would report another container's traffic.
func (p *Provider) UsageTarget(ctx context.Context, instanceID string) (provider.UsageTarget, error) {
	out, err := p.run(ctx, "inspect", "--format", "{{.State.Pid}}", instanceID)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		return provider.UsageTarget{}, fmt.Errorf("docker: container %s has no running process to measure", instanceID)
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("docker: read the cgroup of container %s: %w", instanceID, err)
	}
	rel, err := unifiedCgroup(string(raw))
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("docker: container %s: %w", instanceID, err)
	}

	return provider.UsageTarget{CgroupDir: filepath.Join(cgroupMount, rel)}, nil
}

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
