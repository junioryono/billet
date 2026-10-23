package firecracker

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// twoGroups is a host of two cache groups of two cores each, numbered as an
// EPYC numbers them: a core's SMT sibling is its number plus the core count.
var twoGroups = topology{{{0, 4}, {1, 5}}, {{2, 6}, {3, 7}}}

// writeCPUSysfs lays out a /sys/devices/system/cpu for twoGroups: online cpus,
// an L1 and an L3 per cpu, and each cpu's core siblings.
func writeCPUSysfs(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(root, "online"), "0-7")
	for cpu := range 8 {
		dir := filepath.Join(root, "cpu"+strconv.Itoa(cpu))
		core, group := cpu%4, "0-1,4-5"
		if core >= 2 {
			group = "2-3,6-7"
		}
		write(filepath.Join(dir, "cache", "index0", "level"), "1")
		write(filepath.Join(dir, "cache", "index0", "shared_cpu_list"), strconv.Itoa(core)+","+strconv.Itoa(core+4))
		write(filepath.Join(dir, "cache", "index3", "level"), "3")
		write(filepath.Join(dir, "cache", "index3", "shared_cpu_list"), group)
		write(filepath.Join(dir, "topology", "thread_siblings_list"), strconv.Itoa(core)+","+strconv.Itoa(core+4))
	}

	return root
}

// THE HOST'S SHAPE IS READ FROM THE KERNEL: groups by the highest cache level a
// cpu reports, cores by their SMT siblings, both in the order of their lowest
// cpu.
func TestTheTopologyGroupsCPUsByLastLevelCacheAndCore(t *testing.T) {
	t.Parallel()

	topo, err := readTopology(writeCPUSysfs(t))
	if err != nil {
		t.Fatalf("readTopology: %v", err)
	}
	if !slices.EqualFunc(topo, twoGroups, func(a, b [][]int) bool {
		return slices.EqualFunc(a, b, slices.Equal[[]int])
	}) {
		t.Fatalf("topology = %v, want %v", topo, twoGroups)
	}
}

// A CPU THE READER CANNOT DESCRIBE refuses the whole topology, never a part of it.
func TestATopologyWithAnUnreadableCPUIsRefused(t *testing.T) {
	t.Parallel()

	root := writeCPUSysfs(t)
	if err := os.Remove(filepath.Join(root, "cpu5", "topology", "thread_siblings_list")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := readTopology(root); err == nil || !strings.Contains(err.Error(), "cpu5") {
		t.Fatalf("a cpu with no sibling list read as %v", err)
	}
}

// WHAT EACH PLACEMENT GIVES A GUEST, from an empty host and around cpus another
// guest holds.
func TestEachPlacementTakesTheCPUsItsShapeNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		placement string
		vcpus     int
		taken     []int
		want      []int
	}{
		{"compact takes a whole core", config.CPUPlacementCompact, 2, nil, []int{0, 4}},
		{"compact fills the lowest group", config.CPUPlacementCompact, 4, nil, []int{0, 4, 1, 5}},
		{"compact skips a core in use", config.CPUPlacementCompact, 2, []int{0}, []int{1, 5}},
		{"compact moves to a group with room", config.CPUPlacementCompact, 4, []int{0}, []int{2, 6, 3, 7}},
		{"compact spans groups only when none holds it", config.CPUPlacementCompact, 6, nil,
			[]int{0, 4, 1, 5, 2, 6}},
		{"compact refuses what the host cannot hold", config.CPUPlacementCompact, 9, nil, nil},
		{"spread takes one thread per core", config.CPUPlacementSpread, 2, nil, []int{0, 1}},
		{"spread goes where the most cores are free", config.CPUPlacementSpread, 2, []int{0}, []int{2, 3}},
		{"spread spans groups by free cores", config.CPUPlacementSpread, 3, nil, []int{0, 1, 2}},
		{"packed takes one thread per core from the lowest group", config.CPUPlacementPacked, 2, nil,
			[]int{0, 1}},
		{"packed stays in the lowest group with a free core", config.CPUPlacementPacked, 1, []int{0},
			[]int{1}},
		{"packed spills to the next group in order", config.CPUPlacementPacked, 3, nil, []int{0, 1, 2}},
		{"packed skips a half-held core", config.CPUPlacementPacked, 2, []int{4}, []int{1, 2}},
		{"packed refuses more vcpus than free cores", config.CPUPlacementPacked, 5, nil, nil},
		{"spread refuses more vcpus than free cores", config.CPUPlacementSpread, 5, nil, nil},
		{"no placement places nothing", "", 2, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			taken := map[int]bool{}
			for _, cpu := range tc.taken {
				taken[cpu] = true
			}
			if got := place(twoGroups, tc.placement, tc.vcpus, taken); !slices.Equal(got, tc.want) {
				t.Errorf("place(%q, %d, taken %v) = %v, want %v", tc.placement, tc.vcpus, tc.taken, got,
					tc.want)
			}
		})
	}
}

func placedHarness(t *testing.T, placement string) *harness {
	t.Helper()

	h := newHarness(t)
	h.p.cfg.CPUPlacement = placement
	h.p.topology = twoGroups

	return h
}

func claimFile(h *harness, cpu int) string {
	return filepath.Join(h.p.claimsDir("cpus"), strconv.Itoa(cpu))
}

// A PLACED GUEST'S CPUS REACH THE JAILER AS A cpuset KEY, are recorded in its
// jail, are claimed in its name, and are given back by its destroy.
func TestALaunchIsPinnedToTheCPUsItClaimsAndADestroyFreesThem(t *testing.T) {
	t.Parallel()

	h := placedHarness(t, config.CPUPlacementCompact)
	h.launch(t)

	keys := cgroupKeys(jailerArgv(t, h))
	if !slices.Contains(keys, "cpuset.cpus=0,4") {
		t.Fatalf("jailer --cgroup keys = %q, want cpuset.cpus=0,4 among them", keys)
	}
	res, err := resourcesOf(h.p.jailFor(theInstance))
	if err != nil {
		t.Fatalf("resourcesOf: %v", err)
	}
	if res.CPUs != "0,4" {
		t.Fatalf("the jail records cpus %q, want 0,4", res.CPUs)
	}
	for _, cpu := range []int{0, 4} {
		raw, err := os.ReadFile(claimFile(h, cpu))
		if err != nil || strings.TrimSpace(string(raw)) != h.p.jailFor(theInstance).id {
			t.Fatalf("cpu %d's claim = %q, %v; want the jail %s", cpu, raw, err, h.p.jailFor(theInstance).id)
		}
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, cpu := range []int{0, 4} {
		if _, err := os.Stat(claimFile(h, cpu)); !os.IsNotExist(err) {
			t.Errorf("cpu %d is still claimed after the destroy: %v", cpu, err)
		}
	}
}

// A NODE THAT ASKS FOR NO PLACEMENT LAUNCHES EXACTLY AS BEFORE: no cpuset key,
// no claim.
func TestAnUnplacedLaunchHasNoCPUSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	for _, key := range cgroupKeys(jailerArgv(t, h)) {
		if strings.HasPrefix(key, "cpuset.") {
			t.Fatalf("an unplaced launch asked the jailer for %q", key)
		}
	}
	if _, err := os.Stat(h.p.claimsDir("cpus")); !os.IsNotExist(err) {
		t.Fatalf("an unplaced launch made cpu claims: %v", err)
	}
}

func claimedJail(t *testing.T, h *harness, lease string) jail {
	t.Helper()

	j := h.p.jailFor(provider.InstanceName(lease))
	if err := h.p.claim(j); err != nil {
		t.Fatalf("claim %s: %v", lease, err)
	}

	return j
}

// TWO GUESTS NEVER SHARE A CPU, the second takes what the first left, and a
// claim whose jail is gone is reaped rather than holding its cpus for ever.
func TestPlacedGuestsNeverShareACPUAndAGoneJailsCPUsAreReaped(t *testing.T) {
	t.Parallel()

	h := placedHarness(t, config.CPUPlacementCompact)

	first := claimedJail(t, h, strings.Repeat("a", 32))
	one, err := h.p.claimCPUs(first, resources{}, 4)
	if err != nil || one.CPUs != "0-1,4-5" {
		t.Fatalf("the first guest's cpus = %q, %v; want 0-1,4-5", one.CPUs, err)
	}
	second := claimedJail(t, h, strings.Repeat("b", 32))
	two, err := h.p.claimCPUs(second, resources{}, 4)
	if err != nil || two.CPUs != "2-3,6-7" {
		t.Fatalf("the second guest's cpus = %q, %v; want 2-3,6-7", two.CPUs, err)
	}

	if err := first.remove(); err != nil {
		t.Fatalf("remove the first jail: %v", err)
	}
	third := claimedJail(t, h, strings.Repeat("c", 32))
	three, err := h.p.claimCPUs(third, resources{}, 4)
	if err != nil || three.CPUs != "0-1,4-5" {
		t.Fatalf("a guest after the first's jail went has cpus %q, %v; want the first's 0-1,4-5",
			three.CPUs, err)
	}
	if held, err := h.p.claimedBy(third.id); err != nil || held.CPUs != "0-1,4-5" {
		t.Fatalf("claimedBy(third) = %+v, %v; want its cpus found from the claims", held, err)
	}
}

// A GUEST THAT DOES NOT FIT RUNS UNPINNED, holding no claim, rather than being
// refused or given part of what it asked for.
func TestAGuestThatDoesNotFitRunsOnEveryCPU(t *testing.T) {
	t.Parallel()

	h := placedHarness(t, config.CPUPlacementSpread)
	j := claimedJail(t, h, strings.Repeat("d", 32))
	res, err := h.p.claimCPUs(j, resources{UID: 900001}, 5)
	if err != nil {
		t.Fatalf("claimCPUs: %v", err)
	}
	if res.CPUs != "" || res.UID != 900001 {
		t.Fatalf("a guest too large for the placement got %+v; want the resources unchanged", res)
	}
	entries, err := os.ReadDir(h.p.claimsDir("cpus"))
	if err != nil {
		t.Fatalf("read the cpu claims: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("a guest that did not fit left the claim %s", entry.Name())
		}
	}
}

// THE PLACEMENT IS PROVED WHEN THE PROVIDER IS BUILT: a host whose cgroup
// hierarchy has no cpuset controller is refused, naming it, and one that has it
// is given its topology.
func TestAPlacementNeedsACPUSetControllerAndAReadableTopology(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		controllers string
		refused     bool
	}{
		{"without cpuset", "cpu io memory pids", true},
		{"with cpuset", "cpuset cpu io memory pids", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte(tc.controllers+"\n"),
				0o600); err != nil {
				t.Fatalf("write cgroup.controllers: %v", err)
			}
			sysfs := writeCPUSysfs(t)
			_, err := tryHarness(t, withMounts(t, root), func(p *Provider) {
				p.cpuSysfs = sysfs
				p.cfg.CPUPlacement = config.CPUPlacementCompact
			})
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "no cpuset controller") {
					t.Fatalf("a host without cpuset was not refused for it: %v", err)
				}

				return
			}
			if err != nil {
				t.Fatalf("a host with cpuset was refused: %v", err)
			}
		})
	}
}

// THE TOPOLOGY THE CONSTRUCTOR READS IS THE ONE A LAUNCH PLACES ON: a provider
// built from a host's cpus and a placement pins its first guest there, with no
// test reaching in to hand it a topology.
func TestTheConstructorsTopologyIsWhatALaunchIsPlacedOn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpuset cpu io memory\n"),
		0o600); err != nil {
		t.Fatalf("write cgroup.controllers: %v", err)
	}
	sysfs := writeCPUSysfs(t)
	h, err := tryHarness(t, withMounts(t, root), func(p *Provider) {
		p.cpuSysfs = sysfs
		p.cfg.CPUPlacement = config.CPUPlacementSpread
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.launch(t)

	if keys := cgroupKeys(jailerArgv(t, h)); !slices.Contains(keys, "cpuset.cpus=0-1") {
		t.Fatalf("jailer --cgroup keys = %q, want cpuset.cpus=0-1 from the host's own topology", keys)
	}
}

// AN ODD GUEST UNDER COMPACT TAKES HALF ITS LAST CORE, and the other half is
// never given to another guest, since a core is free only when all of it is.
func TestAnOddCompactGuestsLastCoreIsNotShared(t *testing.T) {
	t.Parallel()

	first := place(twoGroups, config.CPUPlacementCompact, 3, map[int]bool{})
	if !slices.Equal(first, []int{0, 4, 1}) {
		t.Fatalf("a 3-vCPU compact guest = %v, want [0 4 1]", first)
	}
	taken := map[int]bool{}
	for _, cpu := range first {
		taken[cpu] = true
	}
	// ONE vCPU, WHICH THE LEFTOVER SIBLING WOULD FIT, and in the lower group: an
	// allocator that offered cpu 5 would take it, so only the second group's
	// first cpu proves the half-held core is kept.
	if second := place(twoGroups, config.CPUPlacementCompact, 1, taken); !slices.Equal(second, []int{2}) {
		t.Fatalf("the next 1-vCPU guest = %v, want [2]: cpu 5 is the sibling of a core the first guest holds",
			second)
	}
}

// THE KERNEL'S CPU LIST FORM, both ways.
func TestCPUListsRoundTrip(t *testing.T) {
	t.Parallel()

	for list, cpus := range map[string][]int{
		"":             nil,
		"3":            {3},
		"0-1,4-5":      {0, 1, 4, 5},
		"0,2,4":        {0, 2, 4},
		"0-7,64-71,96": append(append(seq(0, 7), seq(64, 71)...), 96),
	} {
		got, err := parseCPUList(list)
		if err != nil || !slices.Equal(got, cpus) {
			t.Errorf("parseCPUList(%q) = %v, %v; want %v", list, got, err, cpus)
		}
		if back := formatCPUList(cpus); back != list {
			t.Errorf("formatCPUList(%v) = %q, want %q", cpus, back, list)
		}
	}
	for _, bad := range []string{"a", "3-1", "-1", "1,,2"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("parseCPUList(%q) was accepted", bad)
		}
	}
}

func seq(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}

	return out
}
