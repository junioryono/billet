package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
)

// jail is one microVM's chroot, as the jailer lays it out.
//
// THE DIRECTORY IS NAMED AFTER THE RESOLVED EXEC FILE, NOT THE PATH BILLET PASSED,
// and that is measured rather than read. The jailer canonicalises --exec-file and
// uses the resulting BASENAME, so pointing it at `/usr/local/bin/firecracker` —
// which the reference host deliberately keeps as a symlink to
// `firecracker-v1.16.1`, so `firecracker --version` always matches what is on disk
// — produces `/srv/jailer/firecracker-v1.16.1/<id>/root`.
//
// Getting this wrong is not a cosmetic error. List enumerates this directory, and a
// List that reads an empty directory reports an inventory with nothing in it — which
// the control plane acts on by freeing the capacity of every lease absent from it,
// while the microVMs those leases paid for are still running. The first attempt at
// this smoke test built the jail under `firecracker/` and firecracker booted into an
// empty chroot and aborted with no message at all.
type jail struct {
	// base is the chroot base directory, the jailer's --chroot-base-dir.
	base string
	// execName is the basename of the resolved firecracker binary.
	execName string
	// id is the jail id, which is billet's instance name.
	id string
}

// dir is the per-VM directory billet owns.
func (j jail) dir() string { return filepath.Join(j.base, j.execName, j.id) }

// root is the chroot itself, which the jailer creates and the VMM sees as `/`.
func (j jail) root() string { return filepath.Join(j.dir(), "root") }

// socket is the VMM's API socket, as seen from the host.
func (j jail) socket() string { return filepath.Join(j.root(), "run", "firecracker.socket") }

// ownerFile records which billet deployment a jail belongs to.
//
// OUTSIDE THE CHROOT, deliberately: `root/` is the VMM's whole filesystem, and
// billet's bookkeeping is not the VMM's business. It also cannot then be confused
// with a resource the guest might reach.
//
// THE MARKER IS WHAT MAKES List SAFE. The chroot base is one directory shared by
// every billet on the machine, so without it two installations enumerate each
// other's microVMs — and List feeds a loop that destroys. It is the same job the
// docker backend's owner LABEL and the ec2 backend's owner TAG do, in the only
// place this backend has to write one.
func (j jail) ownerFile() string { return filepath.Join(j.dir(), "billet-owner") }

// resourceFile records what the HOST allocated for this microVM — its uid, its gid
// and its network device — as opposed to what its lease implies.
//
// OUTSIDE THE CHROOT, like the owner marker, and for a sharper reason: the uid in it
// is the account the VMM runs as, so a VMM that could rewrite this file could make
// teardown release somebody else's uid or delete somebody else's device.
//
// WRITTEN DOWN BECAUSE IT CANNOT BE DERIVED. A device name that fits the kernel's
// 15-character limit cannot encode a 32-character lease id, so it is ALLOCATED — and
// an allocated name is one teardown can only find again by having recorded it.
func (j jail) resourceFile() string { return filepath.Join(j.dir(), "billet-resources") }

// pidFile is where the jailer records the VMM's process id. Named after the
// resolved binary, like the chroot directory, and for the same reason.
func (j jail) pidFile() string { return filepath.Join(j.root(), j.execName+".pid") }

// kernelPath is where the guest kernel lands inside the chroot.
func (j jail) kernelPath() string { return filepath.Join(j.root(), guestKernel) }

// rootDiskPath is where the root disk's device node lands inside the chroot.
func (j jail) rootDiskPath() string { return filepath.Join(j.root(), guestRootDisk) }

// The names a jailed VMM sees. They are paths inside the chroot, so they are
// absolute from the guest VMM's point of view and relative to root() from ours.
const (
	guestKernel   = "vmlinux"
	guestRootDisk = "rootdisk"
)

// resolveExec follows a binary's symlinks, returning the real path and the
// directory name the jailer will derive from it.
//
// BOTH, AND THE RESOLVED PATH IS WHAT THE JAILER IS GIVEN. The jailer canonicalises
// --exec-file itself, so handing it the configured symlink would leave billet's
// pinned name and the jailer's chosen one free to disagree the moment an operator
// retargeted it mid-run.
//
// A path that cannot be resolved is an error rather than a fallback to the
// basename: the fallback would be silently wrong for exactly the installation
// layout the reference host uses, and the failure it produces — an empty inventory
// — is the expensive one.
func resolveExec(execFile string) (string, string, error) {
	resolved, err := filepath.EvalSymlinks(execFile)
	if err != nil {
		return "", "", fmt.Errorf("firecracker: resolve %s, which is what the jailer names its "+
			"chroot after: %w", execFile, err)
	}

	name := filepath.Base(resolved)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", "", fmt.Errorf("firecracker: %s resolves to %s, which has no file name for "+
			"the jailer to name a chroot after", execFile, resolved)
	}

	// THE JAILER REQUIRES IT, and finding out at launch is finding out per job. It
	// refuses an --exec-file whose name does not contain `firecracker`, so a
	// deployment that renamed the binary gets one sentence at construction instead
	// of a failure on every launch.
	if !strings.Contains(name, "firecracker") {
		return "", "", fmt.Errorf("firecracker: %s resolves to %s, and the jailer refuses an "+
			"--exec-file whose name does not contain \"firecracker\"", execFile, name)
	}

	return resolved, name, nil
}

// checkSocketPath refuses a layout whose API socket billet could not dial.
//
// A UNIX SOCKET ADDRESS IS A FIXED-SIZE FIELD — 108 bytes on Linux, 104 on darwin —
// and the VMM's socket sits at the bottom of a path billet composes: the chroot
// base, the resolved binary's name, `billet-` plus a 32-character lease id, then
// `root/run/firecracker.socket`. The jailer's own default base leaves only a
// handful of characters spare, so a slightly longer one is not an exotic
// configuration.
//
// THE VMM IS UNAFFECTED, WHICH IS WHY THIS IS BILLET'S PROBLEM ALONE. Inside the
// jail the same socket is `/run/firecracker.socket`, so firecracker binds it
// happily and billet — dialling from outside, by the full path — gets `bind:
// invalid argument`. Per launch, at the point where the compute has already been
// cloned, and naming no field.
//
// CHECKED AGAINST A FULL-LENGTH ID rather than the one in hand, so the answer does
// not depend on which lease happened to be launched first.
func checkSocketPath(base, execName string) error {
	widest := jail{base: base, execName: execName, id: provider.InstanceName(strings.Repeat("0", leaseIDLength))}

	if got, limit := len(widest.socket()), maxSocketPath; got > limit {
		return fmt.Errorf("firecracker: a microVM's api socket would be %d bytes at %s, and the "+
			"operating system's limit for one is %d; shorten node.firecracker.chroot_base",
			got, widest.socket(), limit)
	}

	return nil
}

// leaseIDLength is how many characters alloc's lease ids carry. Hard-coded rather
// than imported, because a provider may not reach up into the allocator — and a
// value that grew would make this check optimistic rather than wrong, which is the
// direction that fails loudly at the next launch instead of silently.
const leaseIDLength = 32

// maxSocketPath is what a unix socket address can hold on this platform, less the
// terminator. Taken from the kernel's own structure rather than from a constant
// billet would have to keep in step: it is 108 on Linux and 104 on darwin.
const maxSocketPath = len(unix.RawSockaddrUnix{}.Path) - 1

// build lays out everything the VMM needs inside its chroot, before the jailer runs.
//
// THE JAILER CREATES ONLY WHAT IT KNOWS ABOUT — measured, that is /dev/kvm,
// /dev/net/tun, /dev/userfaultfd and /dev/urandom. The kernel image and the root
// disk are billet's to place, and a chroot means placing them INSIDE it: a path
// that is correct from the host is meaningless to a process whose root has moved.
// ErrJailExists is returned when a lease's chroot survived a previous run.
//
// A SENTINEL BECAUSE THE UNWIND MUST NOT TOUCH IT. Every other launch failure
// unwinds what the launch MADE; this one is a refusal to touch what was already
// there, so tearing it down would destroy the state of a microVM this launch never
// created — and the error would still be telling the operator it had been left
// alone.
var ErrJailExists = errors.New("a previous microVM for this lease was not cleaned up")

// claim creates the jail directory and marks it as this deployment's, before
// anything else exists.
//
// FIRST, AND SEPARATE FROM THE REST, because the marker is what makes a half-built
// jail findable. A crash between the clone and the marker leaves a mapped device and
// a cache-pool image that NOTHING can attribute: no jail carries the name, so List
// reports nothing, Find says "not found", the caller concludes nothing started, and
// the lease is freed while the clone holds pool space forever. Creating the jail
// first inverts that — a crash leaves a jail List reports and whose Destroy discards
// the disk.
func (p *Provider) claim(j jail) error {
	// os.Mkdir RATHER THAN Stat-THEN-MkdirAll. The check and the create have to be
	// one operation or two concurrent launches of one lease both pass the check:
	// MkdirAll tolerates an existing directory, so both would proceed and the race
	// would surface later and messily, as the jailer's mknod refusing an existing
	// /dev/net/tun. EEXIST here is exactly the refusal wanted, atomically.
	if err := os.MkdirAll(filepath.Dir(j.dir()), 0o755); err != nil {
		return fmt.Errorf("firecracker: create the chroot base at %s: %w",
			filepath.Dir(j.dir()), err)
	}

	// EVERY DIRECTORY A JAILER HAS BUILT IN, not just this binary's. The Mkdir below
	// is atomic and is what settles a race between two launches of one lease, but it
	// only sees the CURRENT exec directory — so after a binary bump a lease whose
	// old jail survived would be allowed a second one, and then two jails share one
	// clone name and one device while Find and Destroy act on whichever sorts first.
	if existing, found, err := p.findJail(j.id); err != nil {
		return err
	} else if found {
		return fmt.Errorf("firecracker: %w: %s already exists, and the jailer cannot reuse it",
			ErrJailExists, existing.dir())
	}

	if err := os.Mkdir(j.dir(), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Reusing an id whose chroot survived fails inside the jailer with
			// `Failed to create /dev/net/tun via mknod inside the jail: File
			// exists` — measured — which names a device rather than the leftover
			// that caused it.
			return fmt.Errorf("firecracker: %w: %s already exists, and the jailer cannot reuse it",
				ErrJailExists, j.dir())
		}

		return fmt.Errorf("firecracker: create the jail at %s: %w", j.dir(), err)
	}

	return p.writeOwner(j)
}

func (p *Provider) build(j jail, device string, res resources) error {
	if err := os.MkdirAll(j.root(), 0o700); err != nil {
		return fmt.Errorf("firecracker: create the jail at %s: %w", j.root(), err)
	}

	if err := p.mknod(j.rootDiskPath(), device, res.UID, res.GID); err != nil {
		return err
	}

	// THE CHROOT, AFTER EVERYTHING IN IT THAT THE VMM MUST OWN. The jailer drops to
	// this uid before exec and the VMM writes its log and its socket in here, so the
	// directory has to be the account's — and chowning as each file appeared would
	// leave a window where a partly-built jail is already writable by it.
	if err := p.chown(j.root(), res.UID, res.GID); err != nil {
		return err
	}

	// AND THE KERNEL LAST, AFTER THE CHOWN, WHICH IS THE WHOLE REASON FOR THE ORDER.
	//
	// It is placed as a hard LINK to the operator's only copy, so it is the same
	// inode — and chowning a link chowns the file. Handing it to the jail uid would
	// give the account every VMM runs as the power to chmod and rewrite the kernel
	// that every later microVM on the host boots, persistently, without ever leaving
	// the chroot.
	//
	// An earlier version narrowed the chown from the jail to the chroot believing
	// that protected it. It did not: the kernel lives INSIDE the chroot, because the
	// VMM has to open it by a path under its own root. Placing it afterwards is what
	// actually keeps it, and the preflight proves it is readable without being owned.
	return p.placeKernel(j)
}

// writeOwner records the deployment this jail belongs to.
func (p *Provider) writeOwner(j jail) error {
	if err := os.WriteFile(j.ownerFile(), []byte(p.owner+"\n"), 0o600); err != nil {
		return fmt.Errorf("firecracker: record which deployment owns %s: %w", j.dir(), err)
	}

	return nil
}

// writeResources records what a microVM was allocated.
func (p *Provider) writeResources(j jail, res resources) error {
	encoded, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("firecracker: encode the resources of %s: %w", j.id, err)
	}

	if err := os.WriteFile(j.resourceFile(), append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("firecracker: record what %s was allocated: %w", j.id, err)
	}

	return nil
}

// resourcesOf reads what a microVM was allocated.
//
// AN ABSENT FILE IS THE ZERO VALUE AND NOT AN ERROR, because a launch is interrupted
// between claiming a jail and filling it far more easily than the file is corrupted —
// and teardown must still be able to remove a jail that never got that far. A file
// that is THERE and unreadable is an error: something was allocated and billet cannot
// tell what, so releasing nothing is better than releasing a guess.
func resourcesOf(j jail) (resources, error) {
	raw, err := os.ReadFile(j.resourceFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resources{}, nil
		}

		return resources{}, fmt.Errorf("firecracker: read what %s was allocated: %w", j.id, err)
	}

	var res resources
	if err := json.Unmarshal(raw, &res); err != nil {
		return resources{}, fmt.Errorf("firecracker: %s does not hold a resource record billet "+
			"wrote", j.resourceFile())
	}

	return res, nil
}

// ownerOf reads the deployment a jail belongs to.
//
// THREE ANSWERS, NOT TWO, because the caller destroys. A marker that says another
// deployment and a marker billet could not READ are different facts: the first is
// somebody else's microVM, the second is billet's own bookkeeping failing. Both are
// left alone, and only a marker that matches admits a jail to the inventory.
func ownerOf(j jail) (string, error) {
	raw, err := os.ReadFile(j.ownerFile())
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(raw)), nil
}

// placeKernel puts the guest kernel where the jailed VMM can open it.
//
// A HARD LINK FIRST, because the kernel is tens of megabytes and every microVM
// needs the same one — the reference host's is 44MB, so a copy per job is 44MB of
// writes and 44MB of page cache for a file that never changes. A link across
// filesystems is impossible rather than slow, so a copy is the fallback and not an
// error.
//
// AND THE LINK IS NEVER GIVEN AWAY. A hard link is the SAME INODE, so chowning the
// name inside the jail would chown the operator's one and only kernel on the host —
// handing it to the account every VMM on the machine runs as. The owner of a file
// may chmod it and write to it, and a VMM is precisely the process the jailer exists
// to contain: one escape would rewrite the kernel that every later microVM boots,
// persistently, without ever leaving the chroot. build's chown covers root/ and this
// stays owned by whoever installed it; the VMM only ever reads it.
//
// WORLD-READABLE, because that is what the jailed account needs and all it needs.
// The file is a kernel image rather than a secret.
func (p *Provider) placeKernel(j jail) error {
	if err := os.Link(p.cfg.KernelImage, j.kernelPath()); err == nil {
		return nil
	}

	src, err := os.Open(p.cfg.KernelImage)
	if err != nil {
		return fmt.Errorf("firecracker: open the guest kernel %s: %w", p.cfg.KernelImage, err)
	}

	defer src.Close()

	dst, err := os.OpenFile(j.kernelPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("firecracker: create %s: %w", j.kernelPath(), err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()

		return fmt.Errorf("firecracker: copy the guest kernel into %s: %w", j.kernelPath(), err)
	}

	if err := dst.Close(); err != nil {
		return fmt.Errorf("firecracker: finish writing %s: %w", j.kernelPath(), err)
	}

	return nil
}

// execDirs lists every directory under the chroot base a jailer has ever built in.
//
// EVERY ONE, NOT JUST THIS BINARY'S, AND THAT IS THE POINT. The chroot is named
// after the RESOLVED --exec-file, and the reference host installs firecracker as a
// versioned filename behind a stable symlink precisely so a version can be bumped
// and rolled back. Retarget that symlink and restart billet — which is a documented
// flow, and which billet promises leaves running jobs running — and every guest
// started before the bump sits under the OLD name.
//
// Reading only the current one would then return an inventory that is empty, short
// and error-free, and the control plane frees the capacity of every lease absent
// from it. A binary upgrade would silently resell every running job's machine. So
// billet looks in all of them, and the owner marker is what keeps that safe: it is
// what decides whose microVM a jail holds, not the directory it happens to be in.
func (p *Provider) execDirs() ([]string, error) {
	entries, err := os.ReadDir(p.cfg.ChrootBase)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A host that has never launched anything has no such directory, and
			// refusing there would make the first sweep on a fresh host a failure.
			return nil, nil
		}

		return nil, fmt.Errorf("firecracker: list the chroot base %s: %w", p.cfg.ChrootBase, err)
	}

	var dirs []string

	for _, entry := range entries {
		// THE JAILER'S OWN RULE, which is what bounds this to directories it made:
		// it refuses an --exec-file whose name does not contain `firecracker`, so
		// nothing else it built is here. Anything else under the base belongs to
		// something that is not billet and is left alone.
		if entry.IsDir() && strings.Contains(entry.Name(), "firecracker") {
			dirs = append(dirs, entry.Name())
		}
	}

	// THE CURRENT BINARY'S DIRECTORY IS ALWAYS ONE OF THEM, even before it exists,
	// so a caller looking for a jail this process is about to create finds the same
	// answer as one looking for a jail it already made.
	if !slices.Contains(dirs, p.execName) {
		dirs = append(dirs, p.execName)
	}

	return dirs, nil
}

// findJail locates an existing jail for an instance, wherever a jailer put it.
func (p *Provider) findJail(name string) (jail, bool, error) {
	dirs, err := p.execDirs()
	if err != nil {
		return jail{}, false, err
	}

	for _, dir := range dirs {
		j := jail{base: p.cfg.ChrootBase, execName: dir, id: name}

		if _, err := os.Stat(j.dir()); err == nil {
			return j, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			// NOT "NOT FOUND". A directory billet could not stat is not evidence
			// that no microVM is there, and the caller's next move on a miss is to
			// conclude nothing started.
			return jail{}, false, fmt.Errorf("firecracker: look for a microVM at %s: %w",
				j.dir(), err)
		}
	}

	return jail{}, false, nil
}

// remove takes a jail and everything in it away.
//
// MANDATORY ON TEARDOWN rather than tidy. The jailer refuses an id whose chroot
// still exists, so a jail left behind is a lease that can never be relaunched — and
// the directory holds a hard link to the guest kernel and a device node, neither of
// which anything else will collect.
func (j jail) remove() error {
	if err := os.RemoveAll(j.dir()); err != nil {
		return fmt.Errorf("firecracker: remove the jail at %s: %w", j.dir(), err)
	}

	return nil
}
