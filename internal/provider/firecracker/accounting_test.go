package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cgroupTree writes a stub cgroup-v2 root: its controllers, its
// subtree_control, and child cgroups holding the named files, each a domain
// cgroup (cgroup.type "domain", as every non-root cgroup has a type).
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
		write(filepath.Join(root, child, "cgroup.type"), "domain\n")
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
				t.Helper()

				return cgroupTree(t, "cpu io memory", "cpu io memory",
					map[string][]string{"system.slice": {"io.stat"}})
			},
			memory: ControllerPresent, io: ControllerMissing, reason: "CONFIG_BLK_CGROUP_IOCOST",
		},
		{
			name: "a root without the controllers proves them missing",
			root: func(t *testing.T) string {
				t.Helper()

				return cgroupTree(t, "cpu pids", "cpu", map[string][]string{"system.slice": nil})
			},
			memory: ControllerMissing, io: ControllerMissing, reason: "memory controller is not in",
		},
		{
			name: "a cgroup named io.weight is not the kernel's file",
			root: func(t *testing.T) string {
				t.Helper()

				root := cgroupTree(t, "cpu io memory", "cpu io memory",
					map[string][]string{"system.slice": nil})
				if err := os.Mkdir(filepath.Join(root, "system.slice", "io.weight"), 0o700); err != nil {
					t.Fatalf("stage: %v", err)
				}

				return root
			},
			memory: ControllerPresent, io: ControllerUnknown, reason: "not the kernel's io.weight file",
		},
		{
			name: "a directory named io.weight does not hide a later child's file",
			root: func(t *testing.T) string {
				t.Helper()

				root := cgroupTree(t, "cpu io memory", "cpu io memory",
					map[string][]string{"a.slice": nil, "system.slice": {"io.weight"}})
				if err := os.Mkdir(filepath.Join(root, "a.slice", "io.weight"), 0o700); err != nil {
					t.Fatalf("stage: %v", err)
				}

				return root
			},
			memory: ControllerPresent, io: ControllerPresent,
		},
		{
			name: "a threaded cgroup has no io files and proves nothing",
			root: func(t *testing.T) string {
				t.Helper()

				root := cgroupTree(t, "cpu io memory", "cpu io memory",
					map[string][]string{"worker": nil})
				if err := os.WriteFile(filepath.Join(root, "worker", "cgroup.type"), []byte("threaded\n"), 0o600); err != nil {
					t.Fatalf("stage: %v", err)
				}

				return root
			},
			memory: ControllerPresent, io: ControllerUnknown, reason: "no domain cgroup",
		},
		{
			name: "a directory that is no longer a cgroup proves nothing",
			root: func(t *testing.T) string {
				t.Helper()

				// Not a cgroup, even with a file named io.weight in it: only a
				// child with a type is read.
				root := cgroupTree(t, "cpu io memory", "cpu io memory", nil)
				if err := os.Mkdir(filepath.Join(root, "gone.scope"), 0o700); err != nil {
					t.Fatalf("stage: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, "gone.scope", "io.weight"), nil, 0o600); err != nil {
					t.Fatalf("stage: %v", err)
				}

				return root
			},
			memory: ControllerPresent, io: ControllerUnknown, reason: "no domain cgroup",
		},
		{
			name: "io enabled nowhere below the root cannot be read",
			root: func(t *testing.T) string {
				t.Helper()

				return cgroupTree(t, "cpu io memory", "cpu memory",
					map[string][]string{"system.slice": nil})
			},
			memory: ControllerPresent, io: ControllerUnknown, reason: "cannot be read",
		},
		{
			name: "an unreadable root is could not tell for both",
			root: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "absent")
			},
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

// THE ZERO VALUE REFUSES, as could not tell, and a provider that was not asked
// for accounting refuses nothing on a host that could prove none of it.
func TestRequireRefusesWhatWasNotProved(t *testing.T) {
	err := Accounting{}.Require()
	if !errors.Is(err, ErrJobAccountingUnproved) ||
		!strings.Contains(err.Error(), "could not tell whether memory") ||
		!strings.Contains(err.Error(), "could not tell whether io") {
		t.Errorf("the zero Accounting = %v, want a could-not-tell refusal for both", err)
	}
	if err := (Accounting{Memory: ControllerPresent, IO: ControllerPresent}).Require(); err != nil {
		t.Errorf("both present refused: %v", err)
	}

	noHierarchy := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(noHierarchy, []byte("proc /proc proc rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := newHarness(t, WithMountTable(noHierarchy)).p.RequireJobAccounting(); err != nil {
		t.Errorf("a provider not asked for accounting refused: %v", err)
	}

	err = newHarness(t, WithMountTable(noHierarchy), WithJobAccounting()).p.RequireJobAccounting()
	if !errors.Is(err, ErrJobAccountingUnproved) {
		t.Fatalf("a mount table naming no cgroup-v2 hierarchy = %v, want the refusal", err)
	}
	for _, want := range []string{"could not tell whether memory", "no cgroup-v2 hierarchy",
		"<cgroup-v2 root>/cgroup.subtree_control"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	// A HIERARCHY WITH NOTHING BELOW ITS ROOT cannot show io.weight however its
	// controllers are set, so the remedy says to create the jailer's parent.
	empty := cgroupTree(t, "cpu io memory", "cpu io memory", nil)
	err = newHarness(t, withMounts(t, empty), WithJobAccounting()).p.RequireJobAccounting()
	if !errors.Is(err, ErrJobAccountingUnproved) ||
		!strings.Contains(err.Error(), "could not tell whether io") ||
		!strings.Contains(err.Error(), "`mkdir "+filepath.Join(empty, "firecracker-v1.16.1")+"`") {
		t.Errorf("an empty hierarchy = %v, want a could-not-tell refusal saying to create the jailer's parent", err)
	}
}
