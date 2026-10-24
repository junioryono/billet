package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// vmService is the executable Virtualization.framework runs each VM in: an XPC
// service launchd starts on tart's behalf, one per VM, parented to launchd
// rather than to `tart run`. The guest's CPU, memory and disk are charged to
// it, and tart's own process holds almost none (macOS 27, tart 2.37.0,
// measured on the reference Mac 2026-09-30).
const vmService = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/" +
	"com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"

// hostProcessStart reads a process's start from the running kernel.
func hostProcessStart(pid int) (uint64, error) {
	c, err := usage.Reader{Root: "/"}.ProcessCounters(pid)
	if err != nil {
		return 0, err
	}

	return c.Start, nil
}

// UsageTarget says which process is a running VM: the one Virtualization.framework
// process that holds the VM's disk open.
//
// OWNERSHIP IS THE OPEN DISK, which names the VM; nothing about the process
// itself does. So the disk must be this deployment's own (the VM directory's
// ownership marker, with no symlink at either level, since lsof follows one to
// whatever it names), and the same file before and after. The holder is proved
// twice around reading its start time, so the start that every later read is
// checked against is the VM's and not that of a process that took the pid in
// between.
func (p *Provider) UsageTarget(ctx context.Context, instanceID string) (provider.UsageTarget, error) {
	if _, ok := provider.LeaseOf(instanceID); !ok || filepath.Base(instanceID) != instanceID {
		return provider.UsageTarget{}, fmt.Errorf("tart: %q is not an instance billet names", instanceID)
	}
	disk, err := p.ownDisk(instanceID)
	if err != nil {
		return provider.UsageTarget{}, err
	}
	path := filepath.Join(p.vmDir(instanceID), "disk.img")
	pid, err := p.diskHolder(ctx, path)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("tart: find %s's VM process: %w", instanceID, err)
	}
	start, err := p.processStart(pid)
	if err != nil {
		return provider.UsageTarget{}, fmt.Errorf("tart: read the start of %s's VM process: %w", instanceID, err)
	}
	if again, err := p.diskHolder(ctx, path); err != nil || again != pid {
		return provider.UsageTarget{}, fmt.Errorf("tart: %s's VM process changed while it was being read",
			instanceID)
	}
	if after, err := p.ownDisk(instanceID); err != nil || !os.SameFile(disk, after) {
		return provider.UsageTarget{}, fmt.Errorf("tart: %s's disk changed while its VM was being found",
			instanceID)
	}

	target := provider.UsageTarget{PID: pid, PIDStart: start, Process: true}
	// A DEVICE THAT CANNOT BE FOUND COSTS THE TRAFFIC ALONE: the rest of the VM
	// is measured, and its network is reported unmeasured.
	dev, err := p.netDevice(ctx, instanceID, pid)
	if err != nil {
		p.log.Warn("could not find a VM's network device; its traffic will not be measured",
			"vm", instanceID, "error", err)

		return target, nil
	}
	// THE HOST'S END OF THE VM'S LINK: what the host sent on it is what the guest
	// received (measured on macOS 27.0.1, 2026-10-24).
	target.NetDevice, target.NetHostView = dev, true

	return target, nil
}

// netDevice is the host's end of a VM's network link. Virtualization.framework
// gives each VM a vmenet interface of its own, a member of the bridge its
// network is shared on (bridge100, under tart's shared network and softnet
// alike), created for the userspace_ethernet control socket of whoever carries
// the VM's frames and living as long as it does: the VM's own process on the
// shared network, and under softnet the softnet process its `tart run` started.
//
// TWO FACTS MUST AGREE, and either alone is refused. The socket's unit names
// the interface (unit N is vmenet N-1, measured on macOS 27.0.1, 2026-10-24),
// and the kernel says who holds it; the bridge's address table must have
// learned the VM's configured address on that same interface. The table alone
// is what the guest sent, and a guest can claim another's address.
func (p *Provider) netDevice(ctx context.Context, name string, vm int) (string, error) {
	mac, err := p.vmMAC(name)
	if err != nil {
		return "", err
	}
	unit, err := p.ethernetUnit(ctx, vm)
	if err != nil {
		return "", err
	}
	if unit == 0 {
		carrier, err := p.softnetOf(ctx, name, mac)
		if err != nil {
			return "", err
		}
		if unit, err = p.ethernetUnit(ctx, carrier); err != nil {
			return "", err
		}
		if unit == 0 {
			return "", fmt.Errorf("neither %s's VM process nor its softnet holds a VM network link", name)
		}
	}
	dev := fmt.Sprintf("vmenet%d", unit-1)

	bridges, err := p.bridges()
	if err != nil {
		return "", fmt.Errorf("list the host's bridges: %w", err)
	}
	var found []string
	for _, bridge := range bridges {
		members, err := p.bridgeMembers(ctx, bridge, mac)
		if err != nil {
			return "", err
		}
		found = append(found, members...)
	}
	if len(found) != 1 || found[0] != dev {
		return "", fmt.Errorf("%s's link is %s, but the bridges learned its address %s on [%s]", name, dev, mac,
			strings.Join(found, ", "))
	}

	return dev, nil
}

// ethernetUnit is the unit of the one userspace_ethernet control socket pid
// holds, or zero when it holds none.
//
// lsof names one as "[ctl com.apple.userspace_ethernet id 14 unit 1]" (macOS
// 27.0.1, 2026-10-24); its warnings are on, and any of them refuses, as for the
// disk.
func (p *Provider) ethernetUnit(ctx context.Context, pid int) (int, error) {
	// #nosec G204 -- the binary is fixed and the one variable argument is a pid.
	// No shell.
	cmd := exec.CommandContext(ctx, p.lsof, "-nP", "+w", "-a", "-p", strconv.Itoa(pid), "-F", "n")
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	complaint := strings.TrimSpace(stderr.String())
	if err != nil {
		return 0, fmt.Errorf("list process %d's sockets: %w: %s", pid, err, complaint)
	}
	if complaint != "" {
		return 0, fmt.Errorf("lsof warned listing process %d's sockets, so its answer may be short: %s", pid,
			complaint)
	}
	var units []int
	for line := range strings.Lines(stdout.String()) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "n[ctl com.apple.userspace_ethernet ")
		if !ok {
			continue
		}
		_, after, found := strings.Cut(rest, " unit ")
		digits, closed := strings.CutSuffix(after, "]")
		unit, err := strconv.Atoi(digits)
		if !found || !closed || err != nil || unit < 1 || unit > 9999 {
			return 0, fmt.Errorf("process %d holds a VM network socket billet cannot read: %q", pid,
				strings.TrimSpace(line))
		}
		units = append(units, unit)
	}
	switch len(units) {
	case 0:
		return 0, nil
	case 1:
		return units[0], nil
	default:
		return 0, fmt.Errorf("process %d holds %d VM network sockets", pid, len(units))
	}
}

// softnetOf is the softnet process carrying a VM's frames: the one whose parent
// is the VM's own `tart run <name>` and which was told the VM's address.
func (p *Provider) softnetOf(ctx context.Context, name string, mac net.HardwareAddr) (int, error) {
	// #nosec G204 -- the binary and its arguments are fixed. No shell.
	cmd := exec.CommandContext(ctx, p.ps, "-axo", "pid=,ppid=,args=")
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("list the host's processes: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	type row struct {
		ppid int
		args []string
	}
	rows := map[int]row{}
	for line := range strings.Lines(stdout.String()) {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("ps answered a line billet cannot read: %q", strings.TrimSpace(line))
		}
		rows[pid] = row{ppid: ppid, args: f[2:]}
	}
	var carriers []int
	for pid, r := range rows {
		if filepath.Base(r.args[0]) != "softnet" {
			continue
		}
		parent, ok := rows[r.ppid]
		if !ok || len(parent.args) < 3 || filepath.Base(parent.args[0]) != "tart" || parent.args[1] != "run" ||
			parent.args[2] != name {
			continue
		}
		told := slices.Index(r.args, "--vm-mac-address")
		if told < 0 || told+1 >= len(r.args) {
			return 0, fmt.Errorf("%s's softnet (%d) was told no address", name, pid)
		}
		if given, err := net.ParseMAC(r.args[told+1]); err != nil || !bytes.Equal(given, mac) {
			return 0, fmt.Errorf("%s's softnet (%d) was told address %s, not the VM's %s", name, pid,
				r.args[told+1], mac)
		}
		carriers = append(carriers, pid)
	}
	switch len(carriers) {
	case 0:
		return 0, fmt.Errorf("%s's VM process holds no network link and no softnet of its own is running", name)
	case 1:
		return carriers[0], nil
	default:
		return 0, fmt.Errorf("%d softnet processes carry %s", len(carriers), name)
	}
}

// vmMAC is the hardware address tart gave a VM, from its config.json.
func (p *Provider) vmMAC(name string) (net.HardwareAddr, error) {
	raw, err := os.ReadFile(filepath.Join(p.vmDir(name), "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read %s's configuration: %w", name, err)
	}
	var cfg struct {
		MACAddress string `json:"macAddress"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("read %s's configuration: %w", name, err)
	}
	mac, err := net.ParseMAC(cfg.MACAddress)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("%s's configuration names no Ethernet address", name)
	}

	return mac, nil
}

// bridgeMembers is the members of bridge whose address table holds mac.
//
// `ifconfig <bridge> addr` prints one learned address a line, its octets in
// hex without leading zeros, then the VLAN, the member and the expiry:
// "7a:4c:3:da:6:3 Vlan1 vmenet1 1180 flags=0<>" (macOS 27.0.1, 2026-10-24).
// A line of any other shape refuses, since skipping it could skip the VM.
func (p *Provider) bridgeMembers(ctx context.Context, bridge string, mac net.HardwareAddr) ([]string, error) {
	if !validBridge(bridge) {
		return nil, fmt.Errorf("%q is not a bridge's name", bridge)
	}
	// #nosec G204 -- the binary is fixed and the argument is a bridge name
	// checked above. No shell.
	cmd := exec.CommandContext(ctx, p.ifconfig, bridge, "addr")
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read %s's address table: %w: %s", bridge, err, strings.TrimSpace(stderr.String()))
	}
	var members []string
	for line := range strings.Lines(stdout.String()) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		learned, ok := shortMAC(fields[0])
		if len(fields) < 3 || !ok || !strings.HasPrefix(fields[1], "Vlan") {
			return nil, fmt.Errorf("%s's address table has a line billet cannot read: %q", bridge,
				strings.TrimSpace(line))
		}
		if !bytes.Equal(learned, mac) {
			continue
		}
		if !validVMNet(fields[2]) {
			return nil, fmt.Errorf("%s learned the VM's address on %q, which is not a VM's interface", bridge,
				fields[2])
		}
		members = append(members, fields[2])
	}

	return members, nil
}

// shortMAC parses a hardware address written as six hex octets that may lack
// their leading zeros.
func shortMAC(s string) (net.HardwareAddr, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return nil, false
	}
	mac := make(net.HardwareAddr, 6)
	for i, part := range parts {
		if part == "" || len(part) > 2 {
			return nil, false
		}
		b, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return nil, false
		}
		mac[i] = byte(b)
	}

	return mac, true
}

// validBridge and validVMNet accept the names macOS gives a bridge and a VM's
// interface, a word and a unit number, so neither ever reaches a command line
// or a path as anything else.
func validBridge(name string) bool { return unitName(name, "bridge") }

func validVMNet(name string) bool { return unitName(name, "vmenet") }

func unitName(name, prefix string) bool {
	unit, ok := strings.CutPrefix(name, prefix)
	if !ok || unit == "" || len(unit) > 4 {
		return false
	}
	for _, c := range unit {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// hostBridges is the host's bridge interfaces.
func hostBridges() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var bridges []string
	for _, ifi := range ifaces {
		if validBridge(ifi.Name) {
			bridges = append(bridges, ifi.Name)
		}
	}

	return bridges, nil
}

// ownDisk is the VM's disk, when the VM is this deployment's and neither its
// directory nor its disk is a symlink.
//
// THESE CATCH ACCIDENTS, NOT AN ADVERSARY: a process running as the node's user
// can write TART_HOME, and so can forge the marker as easily as swap a path or
// hard-link a disk. That user is trusted; a guest never reaches these files.
func (p *Provider) ownDisk(name string) (os.FileInfo, error) {
	dir, err := os.Lstat(p.vmDir(name))
	if err != nil {
		return nil, fmt.Errorf("tart: %s has no VM directory: %w", name, err)
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("tart: %s's VM directory is not a directory", name)
	}
	owner, err := p.ownerOf(name)
	if err != nil {
		return nil, fmt.Errorf("tart: %s is not proved to be this deployment's VM: %w", name, err)
	}
	if owner != p.owner {
		return nil, fmt.Errorf("tart: %s belongs to deployment %s, not this one", name, owner)
	}
	disk, err := os.Lstat(filepath.Join(p.vmDir(name), "disk.img"))
	if err != nil {
		return nil, fmt.Errorf("tart: %s has no disk to find its VM by: %w", name, err)
	}
	if !disk.Mode().IsRegular() {
		return nil, fmt.Errorf("tart: %s's disk is not a regular file", name)
	}

	return disk, nil
}

// diskHolder is the one VM process that has disk open. None, several, or one
// that is not a Virtualization.framework VM is an error, never a guess.
func (p *Provider) diskHolder(ctx context.Context, disk string) (int, error) {
	pids, err := p.openers(ctx, disk)
	if err != nil {
		return 0, err
	}
	var vms []int
	for _, pid := range pids {
		path, err := p.processPath(pid)
		// ONLY A HOLDER PROVED GONE IS SKIPPED. One that could not be read is
		// could-not-tell, and skipping it could make another VM the sole one.
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("could not tell what process %d holding %s is: %w", pid, disk, err)
		}
		if path == vmService {
			vms = append(vms, pid)
		}
	}
	switch len(vms) {
	case 0:
		return 0, fmt.Errorf("no VM process has %s open", disk)
	case 1:
		return vms[0], nil
	default:
		return 0, fmt.Errorf("%d VM processes have %s open", len(vms), disk)
	}
}

// openers lists the processes this user can see that have path open.
//
// lsof EXITS 1 BOTH FOR NO OPENER AND FOR AN ERROR, so an empty answer is
// believed only when lsof also said nothing on stderr. Its warnings are turned
// back on (-t turns them off) and any of them refuses, on either exit status.
//
// IT SEES ONLY THIS USER'S PROCESSES, and skips the rest without a word. The disk
// lives in this user's tart store, so another user's process holding it would
// have to be root's; that is outside what billet defends against.
func (p *Provider) openers(ctx context.Context, path string) ([]int, error) {
	// #nosec G204 -- the binary is fixed and the one argument is a path built
	// from TART_HOME and a lease-shaped name. No shell.
	cmd := exec.CommandContext(ctx, p.lsof, "-t", "+w", "--", path)
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	complaint := strings.TrimSpace(stderr.String())
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 &&
		stdout.Len() == 0 && complaint == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", p.lsof, err, complaint)
	}
	if complaint != "" {
		return nil, fmt.Errorf("%s warned, so its answer may be short: %s", p.lsof, complaint)
	}
	var pids []int
	for field := range strings.FieldsSeq(stdout.String()) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("%s answered %q, which is not a pid", p.lsof, field)
		}
		if !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}
