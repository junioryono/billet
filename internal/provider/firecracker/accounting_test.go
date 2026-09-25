package firecracker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cgroupTree writes a stub cgroup-v2 root: its controllers, its
// subtree_control, and child directories holding the named files.
func cgroupTree(t *testing.T, controllers, subtree string, children map[string][]string) string {
	t.Helper()

	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(root, "cgroup.controllers"), controllers+"\n")
	write(filepath.Join(root, "cgroup.subtree_control"), subtree+"\n")
	for child, files := range children {
		if err := os.Mkdir(filepath.Join(root, child), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", child, err)
		}
		for _, file := range files {
			write(filepath.Join(root, child, file), "")
		}
	}

	return root
}

// The reference host's shape (2026-09-25): the root offers memory and io, and
// system.slice has io enabled and io.weight present.
func referenceCgroupRoot(t *testing.T) string {
	t.Helper()

	return cgroupTree(t, "cpuset cpu io memory hugetlb pids rdma misc dmem",
		"cpuset cpu io memory pids", map[string][]string{
			"system.slice":        {"io.weight", "io.stat", "memory.current"},
			"firecracker-v1.16.1": {"cpu.stat"},
		})
}

func TestAccountingIsProvedPerController(t *testing.T) {
	for _, tc := range []struct {
		name       string
		root       func(t *testing.T) string
		memory, io Controller
		reason     string
	}{
		{
			name:   "the reference host proves both",
			root:   referenceCgroupRoot,
			memory: ControllerPresent, io: ControllerPresent,
		},
		{
			name: "a kernel without iocost has io and no io.weight",
			root: func(t *testing.T) string {
				return cgroupTree(t, "cpu io memory", "cpu io memory",
					map[string][]string{"system.slice": {"io.stat"}})
			},
			memory: ControllerPresent, io: ControllerMissing, reason: "CONFIG_BLK_CGROUP_IOCOST",
		},
		{
			name: "a root without the controllers proves them missing",
			root: func(t *testing.T) string {
				return cgroupTree(t, "cpu pids", "cpu", map[string][]string{"system.slice": nil})
			},
			memory: ControllerMissing, io: ControllerMissing, reason: "memory controller is not in",
		},
		{
			name: "io enabled nowhere below the root cannot be read",
			root: func(t *testing.T) string {
				return cgroupTree(t, "cpu io memory", "cpu memory",
					map[string][]string{"system.slice": nil})
			},
			memory: ControllerPresent, io: ControllerUnknown, reason: "cannot be read",
		},
		{
			name:   "an unreadable root is could not tell for both",
			root:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			memory: ControllerUnknown, io: ControllerUnknown, reason: "cgroup.controllers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeAccounting(tc.root(t))
			if got.Memory != tc.memory || got.IO != tc.io {
				t.Errorf("memory %s, io %s; want memory %s, io %s (%s)",
					got.Memory, got.IO, tc.memory, tc.io, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not say %q", got.Reason, tc.reason)
			}
		})
	}
}

// withMounts points the provider's cgroup-v2 discovery at a stub mount table
// before New probes it.
func withMounts(t *testing.T, root string) Option {
	t.Helper()

	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte("cgroup2 "+root+" cgroup2 rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("write the stub mount table: %v", err)
	}

	return func(p *Provider) { p.procMountsPath = mounts }
}

func jailerArgv(t *testing.T, h *harness) []string {
	t.Helper()

	for _, run := range h.commands() {
		if strings.HasSuffix(run.bin, "jailer") {
			return run.args
		}
	}
	t.Fatal("the jailer was never run")

	return nil
}

func cgroupKeys(argv []string) []string {
	var keys []string
	for i, arg := range argv {
		if arg == "--" {
			break
		}
		if arg == "--cgroup" && i+1 < len(argv) {
			keys = append(keys, argv[i+1])
		}
	}

	return keys
}

// A NODE THAT ASKS GETS EXACTLY THE PROVED KEYS, driven through a real launch
// so the test fails if the keys stop reaching the jailer's argv.
func TestAnAccountingNodeAsksTheJailerForProvedControllers(t *testing.T) {
	h := newHarness(t, withMounts(t, referenceCgroupRoot(t)), WithJobAccounting())
	h.launch(t)

	want := []string{"cpu.weight=100", "memory.max=max", "io.weight=default 100"}
	if got := cgroupKeys(jailerArgv(t, h)); !slices.Equal(got, want) {
		t.Errorf("jailer --cgroup keys = %q, want %q", got, want)
	}
}

// A NODE THAT DOES NOT ASK LAUNCHES EXACTLY AS BEFORE, on a host that could
// have honoured it, and a controller the host did not prove is never asked
// for, because the jailer refuses a key whose file is missing.
func TestAccountingIsAskedOnlyWhenWantedAndProved(t *testing.T) {
	h := newHarness(t, withMounts(t, referenceCgroupRoot(t)))
	h.launch(t)
	if got := cgroupKeys(jailerArgv(t, h)); !slices.Equal(got, []string{"cpu.weight=100"}) {
		t.Errorf("without WithJobAccounting the keys are %q, want only cpu.weight", got)
	}

	noIOCost := cgroupTree(t, "cpu io memory", "cpu io memory",
		map[string][]string{"system.slice": {"io.stat"}})
	h = newHarness(t, withMounts(t, noIOCost), WithJobAccounting())
	h.launch(t)
	want := []string{"cpu.weight=100", "memory.max=max"}
	if got := cgroupKeys(jailerArgv(t, h)); !slices.Equal(got, want) {
		t.Errorf("without io.weight the keys are %q, want %q", got, want)
	}
}
