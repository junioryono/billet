package ceph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GoldenPrefix begins the name of every golden's system disk in the cache pool.
//
// A NAME OF ITS OWN, so nothing that sweeps the cache pool takes a golden for one
// of its own: the trash purge deletes only `billet-*` root disks and `cache-*`
// volumes, and the orphan report keeps a name billet does not give its volumes.
const GoldenPrefix = "golden-"

// ErrNoSuchGolden is a golden system disk, or its generation, that is not in the
// cache pool.
var ErrNoSuchGolden = errors.New("the golden's system disk or its generation is not in the cache pool")

// PublishRoot makes the root disk clone name the system disk of golden, and answers
// the generation a restore clones, written golden@generation.
//
// IN PLACE, NOT COPIED. The clone is snapshotted as a generation and renamed to the
// golden's name, which keeps it a copy-on-write child of the generation it was
// cloned from and costs two rbd calls (about 2 s on the reference node,
// 2026-10-21), where copying a golden out of the cache pool costs its whole size:
// 80 GiB, fully allocated, for the reference deployment's image.
//
// WHILE IT IS STILL MAPPED, which is the case it exists for: the guest is paused
// and its VMM holds the device open. Measured on the reference node (2026-10-21,
// Ceph 20.2): a rename of a mapped, open clone succeeds, the kernel keeps the name
// it mapped (`rbd device list` and sysfs still give the old one), so the builder's
// teardown unmaps the device by its old name as before and then finds no clone to
// discard, and a fork of the generation holds the builder's disk byte for byte.
//
// THE CALLER FLUSHES FIRST. A guest's writes reach the device through the host's
// page cache, and Firecracker's default cache type never asks the host to flush
// them, so the caller fsyncs the mapped device before this snapshots it.
func (c *Client) PublishRoot(ctx context.Context, name, golden string) (string, error) {
	return c.publishRootAt(ctx, name, golden, time.Now())
}

func (c *Client) publishRootAt(ctx context.Context, name, golden string, now time.Time) (string, error) {
	if err := checkCloneName(name); err != nil {
		return "", err
	}

	if err := checkGoldenName(golden); err != nil {
		return "", err
	}

	generation := GenerationPrefix + now.UTC().Format(GenerationLayout)
	clone := c.cfg.CachePool + "/" + name
	disk := c.cfg.CachePool + "/" + golden

	// THE GENERATION FIRST, on the clone: a rename that succeeds then carries it, and
	// one that fails leaves a snapshot this removes, so the builder's teardown finds
	// the clone as it left it.
	if _, err := c.rbdCmd(ctx, false, keepKernelClient("snap", "create",
		clone+"@"+generation)...); err != nil {
		return "", fmt.Errorf("ceph: snapshot the root disk %s as %s as client.%s: %w",
			clone, generation, c.cfg.User, err)
	}

	if _, err := c.rbdCmd(ctx, false, keepKernelClient("rename", clone, disk)...); err != nil {
		// ON A CONTEXT OF ITS OWN: the usual reason the rename failed is that the
		// caller's context ended, and a removal asked on it fails too.
		if rmErr := c.removeSnapshot(context.WithoutCancel(ctx), clone+"@"+generation); rmErr != nil {
			return "", fmt.Errorf("ceph: rename %s to %s as client.%s: %w (and its snapshot %s could "+
				"not be removed, so the builder's discard will leave the clone in the trash: %w)",
				clone, disk, c.cfg.User, err, generation, rmErr)
		}

		return "", fmt.Errorf("ceph: rename %s to %s as client.%s: %w", clone, disk, c.cfg.User, err)
	}

	return golden + "@" + generation, nil
}

// WithdrawRoot undoes a PublishRoot whose golden was never published: it renames
// the golden's disk back to the clone name it was published from and removes the
// generation, so the builder's teardown discards the clone as it does any other.
//
// IDEMPOTENT ON THE RENAME, which a retry after a partial withdrawal meets: a golden
// already gone with the clone back under its name is a rename already done.
func (c *Client) WithdrawRoot(ctx context.Context, ref, name string) error {
	golden, generation, err := splitGolden(ref)
	if err != nil {
		return err
	}

	if err := checkCloneName(name); err != nil {
		return err
	}

	disk := c.cfg.CachePool + "/" + golden
	clone := c.cfg.CachePool + "/" + name

	if _, err := c.rbdCmd(ctx, false, keepKernelClient("rename", disk, clone)...); err != nil &&
		!isNoSuchFile(err) {
		return fmt.Errorf("ceph: rename the unpublished golden %s back to %s as client.%s: %w",
			disk, clone, c.cfg.User, err)
	}

	return c.removeSnapshot(ctx, clone+"@"+generation)
}

// CloneGolden clones a golden's generation into the cache pool, maps it, and
// returns the host device, as CloneRoot does for an image-pool generation.
//
// NEVER GROWN: a restored guest's kernel has the filesystem mounted at the size it
// had at capture.
func (c *Client) CloneGolden(ctx context.Context, ref, name string) (string, error) {
	if _, _, err := splitGolden(ref); err != nil {
		return "", err
	}

	if err := checkCloneName(name); err != nil {
		return "", err
	}

	src := c.cfg.CachePool + "/" + ref
	dst := c.cfg.CachePool + "/" + name

	if _, err := c.rbdCmd(ctx, false, "clone", src, dst); err != nil {
		if isNoSuchFile(err) {
			return "", fmt.Errorf("%w: %s could not be cloned as client.%s: %w",
				ErrNoSuchGolden, src, c.cfg.User, err)
		}

		return "", fmt.Errorf("ceph: clone %s to %s as client.%s: %w", src, dst, c.cfg.User, err)
	}

	device, err := c.mapRoot(ctx, name)
	if err != nil {
		// AS CloneRoot's map failure: whatever this host maps of it is unmapped first,
		// on a context of its own, and a clone that cannot be unmapped stays for the
		// launch's own unwind.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), MapTimeout)
		defer cancel()

		if unmapErr := c.unmapAll(cleanupCtx, name); unmapErr != nil {
			return "", fmt.Errorf("%w (and the clone it made is still mapped here, so %s stays in the "+
				"pool for the launch's cleanup: %w)", err, dst, unmapErr)
		}

		if rmErr := c.removeClone(cleanupCtx, name); rmErr != nil {
			return "", fmt.Errorf("%w (and the clone it made could not be removed, so %s is "+
				"holding pool space: %w)", err, dst, rmErr)
		}

		return "", err
	}

	return device, nil
}

// removeSnapshot removes one snapshot, an absent one being success.
func (c *Client) removeSnapshot(ctx context.Context, spec string) error {
	if _, err := c.rbdCmd(ctx, false, keepKernelClient("snap", "rm", spec)...); err != nil &&
		!isNoSuchFile(err) {
		return fmt.Errorf("ceph: remove the snapshot %s as client.%s: %w", spec, c.cfg.User, err)
	}

	return nil
}

// checkGoldenName refuses a golden name that is not GoldenPrefix and one image name.
func checkGoldenName(golden string) error {
	if err := checkCloneName(golden); err != nil {
		return err
	}

	if !strings.HasPrefix(golden, GoldenPrefix) || golden == GoldenPrefix {
		return fmt.Errorf("ceph: %s is not a golden's system disk: its name begins %s",
			bounded(golden), GoldenPrefix)
	}

	return nil
}

// splitGolden reads golden@generation, refusing anything else.
func splitGolden(ref string) (string, string, error) {
	golden, generation, found := strings.Cut(ref, "@")
	if !found || !snapshotSpec.MatchString(ref) {
		return "", "", fmt.Errorf("ceph: %s is not a golden's generation, written golden@generation",
			bounded(ref))
	}

	if err := checkGoldenName(golden); err != nil {
		return "", "", err
	}

	if _, ok := ParseGeneration(generation); !ok {
		return "", "", fmt.Errorf("ceph: %s does not name a generation billet published", bounded(ref))
	}

	return golden, generation, nil
}
