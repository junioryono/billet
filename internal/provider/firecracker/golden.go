package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
)

// goldenDiskPrefix begins the name of a golden's system disk. It is the store's
// ceph.GoldenPrefix, spelled here because the provider may not import the store;
// a test on each side pins the spelling.
const goldenDiskPrefix = "golden-"

// goldenDisk is a root disk that can publish a builder's root disk as a golden's
// system disk, withdraw a publication that did not complete, and clone a golden's
// generation for a restore. A disk without it cannot capture or restore.
type goldenDisk interface {
	// PublishRoot makes the clone name the system disk of golden and answers the
	// generation, written golden@generation, without copying it.
	PublishRoot(ctx context.Context, name, golden string) (string, error)
	// WithdrawRoot makes a golden's disk the clone name again, so the builder's
	// teardown discards it.
	WithdrawRoot(ctx context.Context, ref, name string) error
	// CloneGolden clones a golden's generation for a restore and maps it, answering
	// the host device.
	CloneGolden(ctx context.Context, ref, name string) (string, error)
}

// goldens is this provider's disk as a goldenDisk, or why it cannot be one.
func (p *Provider) goldens() (goldenDisk, error) {
	disk, ok := p.disk.(goldenDisk)
	if !ok {
		return nil, fmt.Errorf("firecracker: this node's root disk cannot keep a golden's system disk, " +
			"so a microVM can be neither captured nor restored here")
	}

	return disk, nil
}

// goldenNameFor is the name of the system disk the microVM id's golden is published
// under: the prefix and the builder's lease id.
//
// THE LEASE, NOT THE SNAPSHOT'S NAME. A snapshot is named by whoever captures it and
// its store is this node's, but the cache pool is shared by every node of a site and
// can be by several deployments, so two captures named alike would claim one disk.
// A lease id is 32 random hex characters, the derivation that keeps root clones
// (billet-<lease>) apart, and the record names the disk for the restore that needs it.
//
// THE LEASE IS PROVED A LEASE ID HERE, before the guest is paused: LeaseOf strips a
// prefix and nothing more, and a name the store would refuse is refused after the
// pause, by which time the guest has stopped for nothing.
func goldenNameFor(id string) (string, error) {
	lease, ok := provider.LeaseOf(id)
	if !ok || !isLeaseID(lease) {
		return "", fmt.Errorf("firecracker: %s does not name a lease, so it is not a microVM billet started",
			bounded(id))
	}

	return goldenDiskPrefix + lease, nil
}

// isLeaseID reports whether s has the shape alloc mints: 32 lowercase hex characters.
func isLeaseID(s string) bool {
	if len(s) != 32 {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}

// flushDevice writes a mapped root disk's dirty host pages back to the device.
//
// A GUEST'S WRITES SIT IN THE HOST'S PAGE CACHE: Firecracker's default block cache
// type (Unsafe, which billet configures) advertises no flush to the guest and never
// fsyncs the backing file, so until the host writes them back a snapshot of the
// device taken from the cluster does not hold them. This is that write-back, asked
// for while the guest is paused.
//
// THE DEVICE COMES FROM billet's OWN RECORD, the shape beside the jail root, and is
// opened only if it is a block device under /dev: a path inside the jail root is the
// jailed account's to replace, and root opening it would follow whatever it became.
func flushDevice(device string) error {
	if !filepath.IsAbs(device) || filepath.Dir(device) != "/dev" {
		return fmt.Errorf("firecracker: %s is not a device billet mapped", bounded(device))
	}

	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("firecracker: open the root disk %s to flush it: %w", device, err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("firecracker: stat the root disk %s: %w", device, err)
	}

	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fmt.Errorf("firecracker: %s is not a block device", device)
	}

	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("firecracker: flush the root disk %s: %w", device, err)
	}

	return nil
}
