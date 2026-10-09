package docker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// A CGROUP IS THE CONTAINER'S ONLY WHEN IT NAMES THE CONTAINER, so a pid reused
// between docker inspect and the /proc read is not measured as the container.
func TestACgroupMustNameItsContainer(t *testing.T) {
	const id = "4f1c0ffee"
	for rel, want := range map[string]bool{
		"/system.slice/docker-4f1c0ffee.scope": true,
		"/docker/4f1c0ffee":                    true,
		"/system.slice/docker-beef.scope":      false,
		"/user.slice/session-3.scope":          false,
		"/docker/4f1c0ffee/child":              false,
	} {
		if got := cgroupNames(rel, id); got != want {
			t.Errorf("cgroupNames(%q) = %v, want %v", rel, got, want)
		}
	}
	if cgroupNames("/docker/", "") {
		t.Error("an empty container id matched a cgroup")
	}
}

func TestAContainersCgroupIsReadUnderEitherDriver(t *testing.T) {
	for _, tc := range []struct {
		name, proc, want, err string
	}{
		{"systemd driver", "0::/system.slice/docker-4f1c.scope\n", "/system.slice/docker-4f1c.scope", ""},
		{"cgroupfs driver", "0::/docker/4f1c\n", "/docker/4f1c", ""},
		{"a hybrid host picks the unified line", "12:memory:/docker/4f1c\n0::/docker/4f1c\n", "/docker/4f1c", ""},
		{"cgroup v1 only", "12:memory:/docker/4f1c\n11:cpu:/docker/4f1c\n", "", "no cgroup-v2 entry"},
		{"the root is not a container's", "0::/\n", "", "not a container's own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unifiedCgroup(tc.proc)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("unifiedCgroup = %q, %v; want an error saying %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("unifiedCgroup = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// containerProc stages /proc/<pid>/cgroup under a fixture root and returns a
// provider that reads it, with start answering each start-time read.
func containerProc(t *testing.T, pid int, cgroup string, start func() (uint64, error)) *Provider {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}
	p := New("billet-selftest")
	p.procRoot = root
	p.processStart = func(got int) (uint64, error) {
		if got != pid {
			t.Errorf("the start time of pid %d was read, want %d", got, pid)
		}

		return start()
	}

	return p
}

// A CONTAINER'S TARGET CARRIES ITS INIT'S PID AND START, AND ITS OWN eth0: the
// sampler reads the interface through /proc/<pid>/net/dev, the container's own
// view (no rx/tx swap), and only while the pid still started then.
func TestAContainersTargetNamesItsNetworkThroughItsInit(t *testing.T) {
	const id, pid = "3a8a1ae66dc2", 4159321
	p := containerProc(t, pid, "0::/system.slice/docker-3a8a1ae66dc2.scope\n",
		func() (uint64, error) { return 186542363, nil })

	got, err := p.usageTargetOf(id, pid, "bridge")
	if err != nil {
		t.Fatalf("usageTargetOf: %v", err)
	}
	want := provider.UsageTarget{
		CgroupDir: "/sys/fs/cgroup/system.slice/docker-3a8a1ae66dc2.scope",
		PID:       pid, PIDStart: 186542363, NetDevice: "eth0", NetNamespace: true,
	}
	if got != want {
		t.Errorf("usageTargetOf = %+v, want %+v", got, want)
	}
}

// ONLY A NAMESPACE OF ITS OWN IS COUNTED: a container in the host's network,
// another container's, a named one or none passes every pid proof, and its
// eth0 is not this job's traffic, so its network is left unmeasured while its
// cgroup is still read.
func TestAContainerSharingANetworkNamespaceHasItsNetworkUnmeasured(t *testing.T) {
	const id, pid = "3a8a1ae66dc2", 4159321
	for mode, owns := range map[string]bool{
		"default": true, "bridge": true, "billet-builds": true,
		"host": false, "none": false, "container:4f1c0ffee": false, "ns:/proc/1/ns/net": false, "": false,
	} {
		t.Run(mode, func(t *testing.T) {
			p := containerProc(t, pid, "0::/docker/3a8a1ae66dc2\n", func() (uint64, error) { return 186542363, nil })

			got, err := p.usageTargetOf(id, pid, mode)
			if err != nil {
				t.Fatalf("usageTargetOf: %v", err)
			}
			if got.CgroupDir != "/sys/fs/cgroup/docker/3a8a1ae66dc2" {
				t.Errorf("the cgroup is %q, want the container's whatever its network", got.CgroupDir)
			}
			if measured := got.NetNamespace || got.NetDevice != ""; measured != owns {
				t.Errorf("network mode %q: net device %q namespace %v, want measured %v",
					mode, got.NetDevice, got.NetNamespace, owns)
			}
		})
	}
}

// AND NO TARGET IS MADE FROM A PID THAT IS NOT PROVED TO BE THE CONTAINER'S,
// before, during or after its start is read.
func TestAContainersTargetIsRefusedForAPidItCannotProve(t *testing.T) {
	const id, pid = "3a8a1ae66dc2", 4159321
	const ours = "0::/docker/3a8a1ae66dc2\n"

	// start answers the start-time read for a provider reading /proc under
	// procRoot; it returns what to read and an error to fail the test with.
	for _, tc := range []struct {
		name, cgroup string
		start        func(procRoot string) (uint64, error, error)
		says         string
	}{
		{"a pid in another container's cgroup", "0::/docker/beef\n", nil, "not container"},
		{"a start that cannot be read", ours, func(string) (uint64, error, error) {
			return 0, errors.New("no such process"), nil
		}, "start time"},
		{"a pid that changed hands while its start was read", ours, func(procRoot string) (uint64, error, error) {
			// The pid is now a host process's: the second proof must see it.
			path := filepath.Join(procRoot, "proc", strconv.Itoa(pid), "cgroup")

			return 999, nil, os.WriteFile(path, []byte("0::/user.slice/session-3.scope\n"), 0o600)
		}, "changed hands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p *Provider
			p = containerProc(t, pid, tc.cgroup, func() (uint64, error) {
				if tc.start == nil {
					return 186542363, nil
				}
				start, err, stageErr := tc.start(p.procRoot)
				if stageErr != nil {
					t.Errorf("restage: %v", stageErr)
				}

				return start, err
			})

			got, err := p.usageTargetOf(id, pid, "bridge")
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("usageTargetOf = %+v, %v; want a refusal saying %q", got, err, tc.says)
			}
		})
	}
}
