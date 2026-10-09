package firecracker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Controller is whether a cgroup controller can account for each microVM.
//
// THE ZERO VALUE IS COULD-NOT-TELL, never present: a controller billet has not
// proved is one it does not ask the jailer for, because the jailer refuses a
// launch outright when a key it is given names a file that does not exist.
type Controller int

const (
	// ControllerUnknown means the host could not be read, or read ambiguously.
	ControllerUnknown Controller = iota
	// ControllerMissing means the host proved the controller is unavailable.
	ControllerMissing
	// ControllerPresent means the host proved the jailer can enable it and write
	// its key.
	ControllerPresent
)

func (c Controller) String() string {
	switch c {
	case ControllerPresent:
		return "present"
	case ControllerMissing:
		return "missing"
	default:
		return "could not tell"
	}
}

// Accounting is what a host can measure about each microVM through the
// jailer's per-VM cgroup, beyond the cpu controller every launch already gets.
type Accounting struct {
	// Memory is memory.current, memory.peak, memory.stat and memory.events.
	Memory Controller
	// IO is io.stat, which the jailer can enable only through io.weight, and
	// io.weight exists only where the kernel's weight-based io policy does.
	IO Controller
	// Reason says why either one is not present, for billet check.
	Reason string
	// Root is the cgroup-v2 hierarchy's mount point and Parent the cgroup the
	// jailer creates each microVM's under, both empty when the mount table could
	// not be read. They are what a refusal tells an operator to change.
	Root, Parent string
}

// ErrJobAccountingUnproved is a node.monitoring the host cannot honour: the
// jailer is asked only for a controller the host proved, so a controller left
// unproved would be recorded as unmeasured on every job.
var ErrJobAccountingUnproved = errors.New("node.monitoring is set and this host could not " +
	"prove the jailer can account each microVM's memory and io")

// Require is nil when both memory and io are proved present, and otherwise
// the refusal naming each controller that is not, whether it is missing or
// could not be told, and how to enable both for the jailer's parent cgroup.
func (a Accounting) Require() error {
	var unproved []string
	for _, c := range []struct {
		name  string
		state Controller
	}{{"memory", a.Memory}, {"io", a.IO}} {
		switch c.state {
		case ControllerPresent:
		case ControllerMissing:
			unproved = append(unproved, c.name+" is missing")
		default:
			unproved = append(unproved, "billet could not tell whether "+c.name+" is available")
		}
	}
	if len(unproved) == 0 {
		return nil
	}

	// A HOST WHOSE MOUNT TABLE NAMED NO HIERARCHY still gets the remedy, with
	// the paths it could not read written as placeholders.
	root, parent := a.Root, a.Parent
	if root == "" {
		root, parent = "<cgroup-v2 root>", "<cgroup-v2 root>/<firecracker binary name>"
	}
	reason := ""
	if a.Reason != "" {
		reason = " (" + a.Reason + ")"
	}

	return fmt.Errorf("firecracker: %w: %s%s. What is not proved would be recorded as "+
		"unmeasured on every job, because the jailer is asked only for the controllers this "+
		"host proves it supports. Enable memory and io for %s: both must be listed in "+
		"%s/cgroup.controllers (a kernel booted with cgroup_disable=memory, or holding a "+
		"controller in a cgroup-v1 hierarchy, does not list it); `echo '+memory +io' > "+
		"%s/cgroup.subtree_control` enables them for every cgroup below the root, the jailer's "+
		"parent among them; and io needs the kernel's weight-based io policy "+
		"(CONFIG_BLK_CGROUP_IOCOST), which an io-enabled cgroup shows as io.weight. Then run "+
		"billet check again, or remove node.monitoring to run this node unmeasured",
		ErrJobAccountingUnproved, strings.Join(unproved, " and "), reason, parent, root, root)
}

// Summary is one line for billet check.
func (a Accounting) Summary() string {
	line := "per-job accounting: memory " + a.Memory.String() + ", io " + a.IO.String()
	if a.Reason != "" {
		line += " (" + a.Reason + ")"
	}

	return line
}

// jailerAccountingArgs are the extra --cgroup keys that make the jailer enable
// each proved controller on the path to the microVM's cgroup.
//
// NEITHER KEY LIMITS ANYTHING: memory.max=max is the unlimited default and
// io.weight 100 is the default weight. They exist for the reason cpu.weight=100
// does: the jailer v1.16.1 enables a controller along the path (`+memory` in
// every ancestor's cgroup.subtree_control) only for the controllers its keys
// name, so without them a microVM's cgroup has cpu.stat and no memory or io
// accounting at all (measured on the reference host, 2026-09-25: the parent's
// subtree_control read `cpu` while a VMM held 35.5 GB no billet number could
// see).
func (a Accounting) jailerAccountingArgs() []string {
	var args []string
	if a.Memory == ControllerPresent {
		args = append(args, "--cgroup", "memory.max=max")
	}
	if a.IO == ControllerPresent {
		args = append(args, "--cgroup", "io.weight=default 100")
	}

	return args
}

// probeAccounting reads, and only reads, whether the memory and io controllers
// can account for a microVM under the cgroup-v2 hierarchy mounted at root.
//
// A controller is present when the root lists it in cgroup.controllers, which
// is what the jailer checks before it accepts a key. io needs one more proof,
// because io.weight is created by the kernel's weight-based io policy (blk-iocost)
// and a kernel without it has the io controller and no io.weight: a child of the
// root that already has the io controller enabled must show the file. Where no
// child has it enabled there is nothing to look at, and that is could not tell.
func probeAccounting(root string) Accounting {
	raw, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return Accounting{Reason: fmt.Sprintf("read %s: %v",
			filepath.Join(root, "cgroup.controllers"), err)}
	}
	available := strings.Fields(string(raw))

	var out Accounting
	var reasons []string

	if slices.Contains(available, "memory") {
		out.Memory = ControllerPresent
	} else {
		out.Memory = ControllerMissing
		reasons = append(reasons, "the memory controller is not in "+root+"/cgroup.controllers")
	}

	switch {
	case !slices.Contains(available, "io"):
		out.IO = ControllerMissing
		reasons = append(reasons, "the io controller is not in "+root+"/cgroup.controllers")
	default:
		verdict, reason := probeIOWeight(root)
		out.IO = verdict
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}

	out.Reason = strings.Join(reasons, "; ")

	return out
}

// probeIOWeight looks for io.weight in a child of root that has io enabled.
func probeIOWeight(root string) (Controller, string) {
	subtree, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return ControllerUnknown, fmt.Sprintf("read %s: %v",
			filepath.Join(root, "cgroup.subtree_control"), err)
	}
	if !slices.Contains(strings.Fields(string(subtree)), "io") {
		return ControllerUnknown, "no cgroup below " + root + " has the io controller " +
			"enabled, so whether this kernel provides io.weight cannot be read"
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return ControllerUnknown, fmt.Sprintf("list %s: %v", root, err)
	}

	sawChild := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sawChild = true
		_, err := os.Stat(filepath.Join(root, entry.Name(), "io.weight"))
		switch {
		case err == nil:
			return ControllerPresent, ""
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return ControllerUnknown, fmt.Sprintf("stat %s: %v",
				filepath.Join(root, entry.Name(), "io.weight"), err)
		}
	}
	if !sawChild {
		return ControllerUnknown, "no cgroup below " + root + " to read io.weight from"
	}

	return ControllerMissing, "the io controller is enabled below " + root +
		" and no cgroup there has io.weight, so this kernel has no weight-based io " +
		"policy (CONFIG_BLK_CGROUP_IOCOST) and the jailer could not enable io"
}

// WithJobAccounting asks the jailer to enable the memory and io controllers
// for each microVM, where the host proves each can be. It is what a node with
// node.monitoring configured passes; without it every launch is exactly the
// cpu-only cgroup it always was.
func WithJobAccounting() Option {
	return func(p *Provider) { p.wantAccounting = true }
}

// Accounting reports what the provider asks the jailer to account for. It is
// the zero value unless WithJobAccounting was given.
func (p *Provider) Accounting() Accounting { return p.accounting }

// RequireJobAccounting refuses a provider built WithJobAccounting on a host
// that did not prove both memory and io (Accounting.Require). A provider built
// without it refuses nothing, and launches with the cpu-only cgroup it always
// did.
func (p *Provider) RequireJobAccounting() error {
	if !p.wantAccounting {
		return nil
	}

	return p.accounting.Require()
}

// hostAccounting reads what this host can account for each microVM, under the
// cgroup-v2 hierarchy the mount table names.
func (p *Provider) hostAccounting() Accounting {
	root, err := cgroup2Mount(p.procMountsPath)
	if err != nil {
		return Accounting{Reason: err.Error()}
	}
	acct := probeAccounting(root)
	acct.Root, acct.Parent = root, filepath.Join(root, p.execName)

	return acct
}

// WithMountTable reads where the cgroup-v2 hierarchy is mounted from path
// rather than /proc/mounts, so a check can be run against a staged hierarchy.
// An empty path keeps the default.
func WithMountTable(path string) Option {
	return func(p *Provider) {
		if path != "" {
			p.procMountsPath = path
		}
	}
}
