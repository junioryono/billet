package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// HostReport is what a host check learned about this machine's ability to run
// microVMs.
type HostReport struct {
	// Firecracker and Jailer are the versions the two binaries report.
	Firecracker string
	Jailer      string
	// JailDir is the directory the jailer will build chroots in, which is also the
	// one List enumerates. It is reported because it is DERIVED — from the resolved
	// binary's name — rather than configured, and an operator looking for a running
	// guest will not find it under the path they wrote.
	JailDir string
	// JailUIDMin and JailUIDCount are the range of uids microVMs run as, one per
	// guest. Reported because a range that is too small is a host that stops being
	// able to launch, and the number is otherwise invisible.
	JailUIDMin, JailUIDCount int
	// Bridges are the networks guests attach to, trusted first. An empty untrusted
	// entry means fork pull-request work is refused.
	Bridge          string
	UntrustedBridge string
}

// ErrNoKVM is returned when this machine cannot run a hardware-accelerated guest.
var ErrNoKVM = errors.New("/dev/kvm is not available, so this host cannot run a microVM")

// CheckHost proves this machine can act on its firecracker configuration.
//
// THE SAME DISTINCTION checkEC2Credentials AND CheckReachable MAKE, one backend
// over. Config validation proves the block is coherent; it cannot prove the
// binaries are installed, that /dev/kvm is there, that the jail account exists, or
// that the bridge does. A node that is wrong about any of those validates
// perfectly and then fails on the first job of the day.
//
// IT IS A READ, and the caller says so. Opening /dev/kvm proves the device is there
// and this process may use it; it proves nothing about the jailer's ability to
// chroot or to place a cgroup, which needs root and is not something a diagnostic
// should acquire to find out.
func (p *Provider) CheckHost(ctx context.Context, needsRootResize bool) (HostReport, error) {
	report := HostReport{
		JailDir:         p.cfg.ChrootBase + "/" + p.execName,
		JailUIDMin:      p.cfg.JailUIDMin,
		JailUIDCount:    p.cfg.JailUIDCount,
		Bridge:          p.cfg.Bridge,
		UntrustedBridge: p.cfg.UntrustedBridge,
	}

	// KVM FIRST, because it is the one that cannot be worked around and the one an
	// operator is most likely to be missing — a VM without nested virtualisation
	// enabled, or a kernel module that was never loaded.
	if err := checkKVM(); err != nil {
		return report, err
	}

	for _, bin := range []struct {
		path string
		into *string
	}{{p.cfg.BinaryPath, &report.Firecracker}, {p.cfg.JailerPath, &report.Jailer}} {
		out, err := p.run(ctx, bin.path, []string{"--version"})
		if err != nil {
			return report, fmt.Errorf("firecracker: %s would not report its version, so this "+
				"host could not launch anything: %w", bin.path, err)
		}

		*bin.into = firstLine(string(out))
	}

	if err := p.checkResize2fs(needsRootResize); err != nil {
		return report, err
	}

	if err := p.checkKernelImage(); err != nil {
		return report, err
	}

	if err := p.checkBridges(ctx); err != nil {
		return report, err
	}

	return report, nil
}

// checkResize2fs proves this host can turn a grown root device into filesystem
// capacity before booting it.
func (p *Provider) checkResize2fs(required bool) error {
	if !required {
		return nil
	}

	if _, err := p.lookPath(resize2fsBinary); err != nil {
		return fmt.Errorf("firecracker: %s is not on PATH, so a tier's root disk capacity "+
			"could not be made usable before boot: %w (install e2fsprogs)",
			resize2fsBinary, err)
	}

	return nil
}

// checkKVM proves this machine can run a hardware-accelerated guest.
//
// OPENED RATHER THAN STAT-ED, which is the same distinction checkPrivateKey draws
// about the App key. A stat says a path exists; it says nothing about whether this
// process may use it, and the common failure here is exactly that — /dev/kvm is
// mode 0660 and owned by the `kvm` group, so a node running as an account that is
// not in it sees the device and cannot open it.
func checkKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%w: %w (a bare-metal host needs the kvm module loaded, and a virtual "+
			"one needs nested virtualisation enabled; if the device is there, this process's "+
			"account is probably not in the group that owns it)", ErrNoKVM, err)
	}

	return f.Close()
}

// checkKernelImage proves there is something to boot.
//
// A DIRECTORY AND AN EMPTY FILE BOTH PASS A STAT, and both produce a VMM that
// accepts the boot source and then fails to start the guest — which under
// --daemonize is a launch that reports success.
func (p *Provider) checkKernelImage() error {
	info, err := os.Stat(p.cfg.KernelImage)
	if err != nil {
		return fmt.Errorf("firecracker: the guest kernel %s could not be read: %w",
			p.cfg.KernelImage, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("firecracker: the guest kernel %s is not a regular file",
			p.cfg.KernelImage)
	}

	if info.Size() == 0 {
		return fmt.Errorf("firecracker: the guest kernel %s is empty", p.cfg.KernelImage)
	}

	// READABLE WITHOUT BEING OWNED, which is the other half of not chowning it.
	// billet hard-links this file into every jail and deliberately leaves it owned by
	// whoever installed it, so the unprivileged account the VMM runs as has to be
	// able to open it on the strength of its mode alone. A root-owned 0600 kernel
	// produces a VMM that starts, accepts its boot source, and fails to start the
	// guest — which under --daemonize is a launch reporting success.
	if info.Mode().Perm()&0o004 == 0 {
		return fmt.Errorf("firecracker: the guest kernel %s is mode %04o, so the unprivileged "+
			"account each microVM runs as cannot read it; billet does not take ownership of it "+
			"(that would hand every vmm on this host the power to rewrite it), so `chmod a+r %s`",
			p.cfg.KernelImage, info.Mode().Perm(), p.cfg.KernelImage)
	}

	return nil
}

// checkBridges proves every network a guest could be put on exists.
//
// BOTH OF THEM, and the untrusted one matters more: it is consulted only when a
// fork's pull request arrives, so a typo there is invisible until the first
// untrusted job — which then fails on a host that had reported itself healthy.
func (p *Provider) checkBridges(ctx context.Context) error {
	for _, bridge := range []string{p.cfg.Bridge, p.cfg.UntrustedBridge} {
		if bridge == "" {
			continue
		}

		if _, err := p.run(ctx, ipBinary, []string{"link", "show", "dev", bridge}); err != nil {
			return fmt.Errorf("firecracker: the bridge %s is not on this host, so a guest "+
				"attached to it would have no network: %w", bridge, err)
		}
	}

	return nil
}

// firstLine keeps the line a version answer begins with. Both binaries print their
// version first and a blank line after it.
func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
}
