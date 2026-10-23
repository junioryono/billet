package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// Which host CPUs a microVM's vCPU threads may run on, when the node asks for a
// placement (node.firecracker.cpu_placement). Without one every guest floats
// over every CPU, as it always has.
//
// THE CPUS ARE CLAIMED LIKE A UID OR A TAP: one claim file per host CPU, named
// by its number and holding the jail that took it, linked into place under the
// claims lock and recorded in the jail's resources. So a node that restarts
// finds what each running guest holds by reading what it recorded, a crash
// mid-launch leaves claims the reaper frees once their jail is gone, and two
// deployments sharing a host contend in one place. The cgroup's cpuset.cpus is
// what the kernel enforces; the claim is what keeps two guests from being given
// the same CPUs.
//
// A GUEST THAT DOES NOT FIT IS LAUNCHED UNPINNED, never refused: a host's
// allocator budget is vCPUs, which may exceed its hardware threads, and the
// placement is an optimisation of where work runs, not a limit on how much.

// cpuSysfs is where the kernel describes the host's CPUs.
const cpuSysfs = "/sys/devices/system/cpu"

// topology is the host's CPUs as the placement sees them: last-level cache
// groups (an EPYC CCD shares one L3), in the order of their lowest CPU, each a
// list of physical cores, each the SMT siblings that share it.
type topology [][][]int

// readTopology reads which CPUs share a last-level cache and which share a
// core, for every online CPU.
//
// EVERY ONLINE CPU OR NONE: a CPU whose cache or sibling list cannot be read is
// an error, because a placement drawn from part of the host could hand a guest a
// CPU it then shares with a group the reader never saw.
func readTopology(root string) (topology, error) {
	online, err := os.ReadFile(filepath.Join(root, "online"))
	if err != nil {
		return nil, fmt.Errorf("firecracker: read the online CPUs: %w", err)
	}
	cpus, err := parseCPUList(strings.TrimSpace(string(online)))
	if err != nil {
		return nil, fmt.Errorf("firecracker: read the online CPUs: %w", err)
	}
	if len(cpus) == 0 {
		return nil, errors.New("firecracker: the host reports no online CPUs")
	}

	groups := map[string]map[string][]int{}
	for _, cpu := range cpus {
		dir := filepath.Join(root, "cpu"+strconv.Itoa(cpu))
		llc, err := lastLevelCache(dir)
		if err != nil {
			return nil, fmt.Errorf("firecracker: read cpu%d's last-level cache: %w", cpu, err)
		}
		siblings, err := os.ReadFile(filepath.Join(dir, "topology", "thread_siblings_list"))
		if err != nil {
			return nil, fmt.Errorf("firecracker: read cpu%d's core siblings: %w", cpu, err)
		}
		core := strings.TrimSpace(string(siblings))
		if groups[llc] == nil {
			groups[llc] = map[string][]int{}
		}
		groups[llc][core] = append(groups[llc][core], cpu)
	}

	var topo topology
	for _, cores := range groups {
		var group [][]int
		for _, threads := range cores {
			sort.Ints(threads)
			group = append(group, threads)
		}
		sort.Slice(group, func(a, b int) bool { return group[a][0] < group[b][0] })
		topo = append(topo, group)
	}
	sort.Slice(topo, func(a, b int) bool { return topo[a][0][0] < topo[b][0][0] })

	return topo, nil
}

// lastLevelCache names the highest cache level a CPU reports, by the CPUs that
// share it.
func lastLevelCache(dir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "cache"))
	if err != nil {
		return "", err
	}
	level, shared := -1, ""
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "index") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "cache", entry.Name(), "level"))
		if err != nil {
			return "", err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return "", fmt.Errorf("cache %s has level %q", entry.Name(), strings.TrimSpace(string(raw)))
		}
		if n <= level {
			continue
		}
		list, err := os.ReadFile(filepath.Join(dir, "cache", entry.Name(), "shared_cpu_list"))
		if err != nil {
			return "", err
		}
		level, shared = n, strings.TrimSpace(string(list))
	}
	if level < 0 {
		return "", errors.New("no cache levels are reported")
	}

	return shared, nil
}

// place chooses vcpus host CPUs for one guest under a placement, from those not
// in taken, or nothing when they do not fit the placement's shape.
//
// COMPACT takes whole cores, both SMT siblings, in the lowest cache group with
// room for the guest, so busy guests share as few groups as possible and the
// rest of the host can sit idle; a guest larger than any group's free room takes
// whole cores from the groups in order. SPREAD takes one thread per core, from
// the group with the most free cores, so a guest's vCPUs share neither a core
// nor, until the host fills, a cache with another guest's.
func place(topo topology, placement string, vcpus int, taken map[int]bool) []int {
	if vcpus <= 0 {
		return nil
	}

	switch placement {
	case config.CPUPlacementCompact:
		return placeCompact(topo, vcpus, taken)
	case config.CPUPlacementSpread:
		return placeSpread(topo, vcpus, taken)
	default:
		return nil
	}
}

func freeCores(group [][]int, taken map[int]bool) [][]int {
	var cores [][]int
	for _, core := range group {
		whole := true
		for _, cpu := range core {
			if taken[cpu] {
				whole = false

				break
			}
		}
		if whole {
			cores = append(cores, core)
		}
	}

	return cores
}

func placeCompact(topo topology, vcpus int, taken map[int]bool) []int {
	fill := func(cores [][]int) []int {
		var cpus []int
		for _, core := range cores {
			for _, cpu := range core {
				if len(cpus) == vcpus {
					return cpus
				}
				cpus = append(cpus, cpu)
			}
		}

		return cpus
	}

	// ONE GROUP WHEN ONE HOLDS IT, the lowest that does.
	for _, group := range topo {
		cores := freeCores(group, taken)
		threads := 0
		for _, core := range cores {
			threads += len(core)
		}
		if threads >= vcpus {
			return fill(cores)
		}
	}

	var all [][]int
	for _, group := range topo {
		all = append(all, freeCores(group, taken)...)
	}
	if cpus := fill(all); len(cpus) == vcpus {
		return cpus
	}

	return nil
}

func placeSpread(topo topology, vcpus int, taken map[int]bool) []int {
	type room struct {
		index int
		cores [][]int
	}
	var rooms []room
	for i, group := range topo {
		if cores := freeCores(group, taken); len(cores) > 0 {
			rooms = append(rooms, room{index: i, cores: cores})
		}
	}
	sort.SliceStable(rooms, func(a, b int) bool { return len(rooms[a].cores) > len(rooms[b].cores) })

	var cpus []int
	for _, r := range rooms {
		for _, core := range r.cores {
			if len(cpus) == vcpus {
				return cpus
			}
			cpus = append(cpus, core[0])
		}
	}
	if len(cpus) == vcpus {
		return cpus
	}

	return nil
}

// claimCPUs takes the host CPUs a guest's placement gives it and records them
// in the jail, returning the resources with them, or unchanged when the node
// asks for no placement or the guest does not fit.
func (p *Provider) claimCPUs(j jail, res resources, vcpus int) (resources, error) {
	if p.cfg.CPUPlacement == "" {
		return res, nil
	}

	unlock, err := p.lockClaims()
	if err != nil {
		return res, err
	}
	defer unlock()

	dir := p.claimsDir("cpus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("firecracker: create the cpu claims directory %s: %w", dir, err)
	}

	taken, err := p.takenCPUs(dir)
	if err != nil {
		return res, err
	}

	cpus := place(p.topology, p.cfg.CPUPlacement, vcpus, taken)
	if cpus == nil {
		p.log.Warn("a microVM does not fit the cpu placement, so it runs on every cpu",
			"runner", j.id, "placement", p.cfg.CPUPlacement, "vcpu", vcpus, "taken", len(taken))

		return res, nil
	}

	var held []int
	release := func() error {
		failures := make([]error, 0, len(held))
		for _, cpu := range held {
			failures = append(failures, p.releaseClaim(filepath.Join(dir, strconv.Itoa(cpu)), j.id))
		}

		return errors.Join(failures...)
	}
	for _, cpu := range cpus {
		ok, err := p.take(filepath.Join(dir, strconv.Itoa(cpu)), j.id)
		if err != nil {
			return res, errors.Join(fmt.Errorf("firecracker: claim cpu %d: %w", cpu, err), release())
		}
		// UNDER THE LOCK EVERY CLAIM WAS COUNTED, so a cpu found taken here is a
		// claim another host's process made without it: give back what this took
		// and run unpinned rather than share a cpu with it.
		if !ok {
			return res, release()
		}
		held = append(held, cpu)
	}

	pinned := res
	pinned.CPUs = formatCPUList(cpus)
	if err := p.writeResources(j, pinned); err != nil {
		return res, errors.Join(err, release())
	}

	return pinned, nil
}

// takenCPUs is every cpu a live microVM's claim holds, reaping the claims of
// jails that are gone.
func (p *Provider) takenCPUs(dir string) (map[int]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("firecracker: list the cpu claims: %w", err)
	}

	taken := map[int]bool{}
	for _, entry := range entries {
		cpu, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if p.reapClaim(filepath.Join(dir, entry.Name())) {
			continue
		}
		// A CLAIM THE REAPER LEFT IS HELD, whether its jail is live or could not be
		// read: could-not-tell keeps the cpu taken.
		taken[cpu] = true
	}

	return taken, nil
}

// releaseCPUs gives a microVM's cpus back, those still its own.
func (p *Provider) releaseCPUs(list, jailID string) error {
	if list == "" {
		return nil
	}
	cpus, err := parseCPUList(list)
	if err != nil {
		return fmt.Errorf("firecracker: read the cpus %s recorded: %w", jailID, err)
	}

	failures := make([]error, 0, len(cpus))
	for _, cpu := range cpus {
		failures = append(failures,
			p.releaseClaim(filepath.Join(p.claimsDir("cpus"), strconv.Itoa(cpu)), jailID))
	}

	return errors.Join(failures...)
}

// requireCPUPlacement refuses a placement the host cannot carry out: one whose
// topology cannot be read whole, or whose cgroup hierarchy has no cpuset
// controller for the jailer to enable.
func (p *Provider) requireCPUPlacement() error {
	topo, err := readTopology(p.cpuSysfs)
	if err != nil {
		return fmt.Errorf("node.firecracker.cpu_placement is %s, and this host's cpus cannot be read: %w",
			p.cfg.CPUPlacement, err)
	}

	root, err := cgroup2Mount(p.procMountsPath)
	if err != nil {
		return fmt.Errorf("node.firecracker.cpu_placement is %s, and the cgroup hierarchy cannot be "+
			"found: %w", p.cfg.CPUPlacement, err)
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("node.firecracker.cpu_placement is %s, and the cgroup controllers cannot be "+
			"read: %w", p.cfg.CPUPlacement, err)
	}
	if !strings.Contains(" "+strings.TrimSpace(string(controllers))+" ", " cpuset ") {
		return fmt.Errorf("node.firecracker.cpu_placement is %s, and this host's cgroup hierarchy has "+
			"no cpuset controller (%s lists %q)", p.cfg.CPUPlacement,
			filepath.Join(root, "cgroup.controllers"), strings.TrimSpace(string(controllers)))
	}

	p.topology = topo

	return nil
}

// parseCPUList reads the kernel's cpu list form, `0-3,8,64-67`.
func parseCPUList(list string) ([]int, error) {
	if list == "" {
		return nil, nil
	}

	var cpus []int
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("%q is not a cpu list", list)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("%q is not a cpu list", list)
			}
		}
		for cpu := first; cpu <= last; cpu++ {
			cpus = append(cpus, cpu)
		}
	}

	return cpus, nil
}

// formatCPUList writes cpus in the kernel's cpu list form, ascending, with
// runs folded into ranges.
func formatCPUList(cpus []int) string {
	sorted := append([]int(nil), cpus...)
	sort.Ints(sorted)

	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(sorted[i]))
		} else {
			parts = append(parts, strconv.Itoa(sorted[i])+"-"+strconv.Itoa(sorted[j]))
		}
		i = j + 1
	}

	return strings.Join(parts, ",")
}
