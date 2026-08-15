package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// bytesPerMB is rbd's unit for --size and for what `rbd info` reports.
const bytesPerMB = 1 << 20

// headSlackMB is how much larger than the filesystem the head image is made.
//
// THE SAME SLACK build-guest-image.sh ALLOWS. It exists so a slightly larger
// image next time does not force a resize, and it is matched here so that a head
// created by either publisher is the same shape.
//
// The space past the filesystem is never written, so it retains whatever the
// previous generation left there. That is deliberate and harmless in the way it
// matters: every generation of a golden image is the same public artifact, so
// there is nothing there one job could learn that another could not. It would NOT
// be harmless if this image ever carried per-deployment material, which is one
// more reason the CA is delivered through MMDS at boot rather than baked in.
const headSlackMB = 512

// MaxImageBytes bounds what will be imported, at 1 TiB.
//
// Present so the megabyte rounding cannot overflow, not because anything near it
// is plausible: the guest filesystem is four gigabytes.
const MaxImageBytes = 1 << 40

// ImportGeneration writes a raw filesystem image into the head image and
// publishes it as an immutable generation.
//
// THE SAME SEQUENCE THE BUILD SCRIPT PERFORMS, for the same reasons, because a
// generation published by a pull and one published by a hand-run build have to be
// indistinguishable afterwards: nothing downstream — promotion, verification,
// reaping, the clone path — should be able to tell where an image came from.
//
// WHY IT WRITES THROUGH A MAPPED DEVICE RATHER THAN `rbd import`. `rbd import`
// CREATES an image; it cannot write into one that already exists. Generations are
// snapshots OF one head image, so a pull that imported to a fresh image would
// publish a generation of something else, which no tier could name and no clone
// could descend from.
func (c *Client) ImportGeneration( //nolint:nonamedreturns // the deferred unmap reports through the return value; a local set inside a defer is read after the return value has already been computed, which is the dead-code bug this shape replaced
	ctx context.Context,
	image, rawPath, runnerVersion, kernel string,
	now time.Time,
) (generation string, err error) {
	if err := checkCloneName(image); err != nil {
		return "", err
	}

	info, err := os.Stat(rawPath)
	if err != nil {
		return "", fmt.Errorf("ceph: cannot read the image to import: %w", err)
	}

	if info.Size() <= 0 {
		return "", fmt.Errorf("ceph: %s is empty; refusing to publish a generation of nothing",
			rawPath)
	}

	// ROUNDED UP. A filesystem that is not a whole number of megabytes would be
	// truncated by a rounded-down device, and the truncation lands at the END of
	// the image — which on ext4 is data, not slack.
	// GUARDED AGAINST OVERFLOW BEFORE THE ROUNDING, not after. Adding the rounding
	// term to a size near MaxInt64 wraps negative, and a negative wantMB makes
	// ensureHead conclude that any existing head is large enough -- so the write
	// then fails only after modifying the image somebody else's generations
	// descend from.
	if info.Size() > MaxImageBytes {
		return "", fmt.Errorf("ceph: %s is %d bytes, past the %d-byte bound this will import",
			rawPath, info.Size(), int64(MaxImageBytes))
	}

	wantMB := (info.Size() + bytesPerMB - 1) / bytesPerMB

	// THE CLUSTER-WIDE PUBLISH LOCK, TAKEN BEFORE ANYTHING IS WRITTEN.
	//
	// Two publishers writing one head image interleave their writes, and the first
	// to finish unmaps and snapshots a filesystem that is half the other one. A
	// same-second generation-name collision does not protect against this: the
	// corruption happens before either `snap create`, so the name is decided long
	// after the damage.
	//
	// IT IS THE SAME LOCK build-guest-image.sh TAKES, on the same image, which is
	// the only reason a Go import and a hand-run build exclude each other rather
	// than each holding a lock the other never looks at.
	lock, err := c.TakePublishLock(ctx, now)
	if err != nil {
		return "", err
	}

	defer func() {
		// ON A CONTEXT STRIPPED OF CANCELLATION, for the same reason the unmap is --
		// and it matters more here. The raw write is deliberately unbounded and moves
		// gigabytes, so a caller's deadline can expire during it; releasing on that
		// dead context fails immediately and LEAVES THE LOCK HELD. Nothing reclaims
		// an rbd lock, so every publisher on every node then refuses for six hours,
		// which is precisely the outage StaleLockAfter exists to bound rather than to
		// be relied upon.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		if releaseErr := lock.Release(releaseCtx); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()

	if err := c.ensureHead(ctx, image, wantMB); err != nil {
		return "", err
	}

	device, err := c.mapImage(ctx, image)
	if err != nil {
		return "", err
	}

	// UNMAPPED ON EVERY PATH. A head image left mapped is not merely untidy: the
	// next import maps it a SECOND time rather than failing, which is how a build
	// host accumulates a dozen mappings of one image, and the build script carries
	// a paragraph about the same trap.
	unmapped := false

	// REPORTED THROUGH THE NAMED RETURN, which is the only thing that works. An
	// earlier version set a local in this deferred function and inspected it at the
	// return site -- but a deferred function runs AFTER the return value has been
	// computed, so the variable was always nil and the branch reading it was dead
	// code that looked like error handling.
	defer func() {
		if unmapped {
			return
		}

		// THE CLEANUP OUTLIVES THE DEADLINE THAT KILLED THE IMPORT. The raw write is
		// deliberately unbounded and moves gigabytes, so a caller's deadline can
		// easily expire during it -- and running the unmap on that dead context
		// fails immediately, leaving the head mapped for exactly the reason the
		// cleanup exists. WithoutCancel keeps the credentials and the values while
		// dropping the cancellation.
		unmapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
		defer cancel()

		unmapErr := c.unmapDevice(unmapCtx, device, image)
		if unmapErr == nil {
			return
		}

		// APPENDED TO THE REAL FAILURE, NEVER SUBSTITUTED FOR IT. This runs on the
		// way out of an import that already failed, and a head left mapped is a
		// consequence of that failure rather than its cause -- so replacing the
		// cause would send an operator after the wrong thing.
		if err != nil {
			err = fmt.Errorf("%w (and the head was left mapped: %w)", err, unmapErr)

			return
		}

		err = fmt.Errorf("ceph: %s could not be unmapped and the head is still mapped: %w",
			image, unmapErr)
	}()

	if err := writeImage(rawPath, device, info.Size()); err != nil {
		return "", err
	}

	// UNMAPPED BEFORE THE SNAPSHOT, NOT AFTER. The snapshot must capture what the
	// filesystem actually is, and a mapped device can still hold dirty pages the
	// kernel has not written back. Snapshotting first would capture the image as
	// of some moment nobody chose -- and the resulting generation would boot, or
	// not, depending on timing that never reproduces.
	if unmapErr := c.unmapDevice(ctx, device, image); unmapErr != nil {
		// MARKED DONE EVEN THOUGH IT FAILED, so the deferred cleanup does not try
		// the same unmap a second time and append its own copy of this failure to
		// the message. One attempt, one explanation.
		unmapped = true

		return "", fmt.Errorf("ceph: the image was written but %s could not be unmapped, so the "+
			"head is still mapped and the next import would map it a second time: %w",
			image, unmapErr)
	}

	unmapped = true

	generation = GenerationPrefix + now.UTC().Format(GenerationLayout)

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "snap", "create",
		image+"@"+generation); err != nil {
		return "", fmt.Errorf("ceph: could not publish %s@%s: %w", image, generation, err)
	}

	// RECORDED PER GENERATION, because a tier boots a generation rather than the
	// head. A single head-level key describes the LAST import, which is not what
	// any job runs: generations are immutable and promotion is deliberate, so a
	// fleet can sit on last month's generation while the head advances. An alarm
	// reading the head then reports the newest import as though it were the fleet.
	// THE KERNEL IS RECORDED INSIDE THE LOCK THIS ALREADY HOLDS.
	//
	// It used to be written by the caller after this returned, which put it outside
	// the lock -- so a reap could remove the generation between the publish and the
	// pairing, leaving a kernel recorded against nothing, or the generation
	// published with no pairing at all. Everything that describes a generation is
	// written while the thing that removes generations is excluded.
	if kernel != "" {
		if setErr := c.SetKernel(ctx, image, generation, kernel); setErr != nil {
			if _, rmErr := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "snap", "rm",
				image+"@"+generation); rmErr != nil {
				return "", fmt.Errorf("ceph: %s@%s was published, its kernel could not be "+
					"recorded (%w), and it could not be withdrawn (%w)",
					image, generation, setErr, rmErr)
			}

			return "", fmt.Errorf("ceph: %s could not be published because its kernel could "+
				"not be recorded; the snapshot has been withdrawn: %w", image, setErr)
		}
	}

	if setErr := c.SetRunnerVersion(ctx, image, generation, runnerVersion); setErr != nil {
		// ROLLED BACK RATHER THAN LEFT HALF-PUBLISHED. A snapshot with no recorded
		// runner version is worse than no snapshot at all: NewestGeneration finds
		// it, so `billet images due` reports that nothing needs rebuilding, while
		// every staleness check reads its version as absent and declines to judge
		// it. The fleet then quietly stops being rebuilt -- the exact outage the
		// version is recorded to prevent, caused by recording it badly.
		if _, rmErr := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "snap", "rm",
			image+"@"+generation); rmErr != nil {
			return "", fmt.Errorf("ceph: %s@%s was published, its runner version could not be "+
				"recorded (%w), and it could not be withdrawn (%w); it will look current to "+
				"`billet images due` while reading as unversioned everywhere else",
				image, generation, setErr, rmErr)
		}

		return "", fmt.Errorf("ceph: %s could not be published because its runner version could "+
			"not be recorded; the snapshot has been withdrawn: %w", image, setErr)
	}

	return generation, nil
}

// ensureHead creates the head image, or grows it to fit.
//
// GROWN, NEVER SHRUNK. An existing head was sized for whatever the last
// generation needed, and writing a larger filesystem into it fails partway with
// "no space left on device" -- a corrupt image behind a successful-looking
// import, since the write is the only step that would have said so. Shrinking is
// refused rather than performed because EXISTING SNAPSHOTS KEEP THEIR OWN SIZE:
// a smaller head does not reclaim anything a generation still holds, and it would
// truncate the next write.
func (c *Client) ensureHead(ctx context.Context, image string, wantMB int64) error {
	out, err := c.rbdCmd(ctx, true, "-p", c.cfg.ImagePool, "info", image)
	if err != nil {
		// ABSENCE IS THE EXPECTED FIRST RUN, so it is distinguished from a cluster
		// that cannot be reached. Treating every failure as "does not exist" would
		// have an unreachable cluster answered with `rbd create`, which then fails
		// for a second reason and reports that one instead.
		if !isNoSuchFile(err) {
			return fmt.Errorf("ceph: could not ask about %s/%s: %w", c.cfg.ImagePool, image, err)
		}

		// THE SAME GEOMETRY build-guest-image.sh CREATES, and it has to be. Whichever
		// publisher runs first decides the head's size and object size for every
		// generation after it, so a Go-created head and a script-created head that
		// differ would give a deployment different clone capacity and a different
		// object layout depending on which tool happened to go first -- a difference
		// nobody would think to look for.
		//
		// The slack is deliberate: a filesystem is written at its own size and the
		// device is larger, so a slightly larger image next time does not force a
		// resize. --object-size is pinned because Ceph's default is configurable per
		// cluster, so leaving it unset makes the layout depend on the operator's
		// ceph.conf.
		// AN ABSENT POOL LOOKS EXACTLY LIKE AN ABSENT IMAGE. `rbd info` answers ENOENT
		// for both, so reaching here proves only that one of the two is missing --
		// and creating an image in a pool that does not exist fails for a second
		// reason, which is the one the operator would then be shown. Asking the pool
		// directly is what separates "first run" from "that pool is not there".
		if _, listErr := c.list(ctx, c.cfg.ImagePool); listErr != nil {
			return fmt.Errorf("ceph: %s/%s does not exist, and neither does the pool: %w",
				c.cfg.ImagePool, image, listErr)
		}

		if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "create", image,
			"--size", fmt.Sprintf("%dM", wantMB+headSlackMB),
			"--object-size", "4M"); err != nil {
			return fmt.Errorf("ceph: could not create %s/%s: %w", c.cfg.ImagePool, image, err)
		}

		return nil
	}

	var head struct {
		Size int64 `json:"size"`
	}

	if err := json.Unmarshal(out, &head); err != nil {
		return fmt.Errorf("ceph: %s did not describe %s/%s as json; is it the rbd command?",
			c.bin, c.cfg.ImagePool, image)
	}

	haveMB := head.Size / bytesPerMB

	if haveMB >= wantMB {
		return nil
	}

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "resize",
		"--size", fmt.Sprintf("%dM", wantMB+headSlackMB), image); err != nil {
		return fmt.Errorf("ceph: %s/%s holds %dM and the image needs %dM, and it could not be "+
			"grown: %w", c.cfg.ImagePool, image, haveMB, wantMB, err)
	}

	return nil
}

// mapImage maps the head image and returns the device it appeared at.
func (c *Client) mapImage(ctx context.Context, image string) (string, error) {
	out, err := c.rbdCmd(ctx, false, "device", "map", c.cfg.ImagePool+"/"+image)
	if err != nil {
		return "", fmt.Errorf("ceph: could not map %s/%s: %w", c.cfg.ImagePool, image, err)
	}

	device := strings.TrimSpace(string(out))
	if device == "" {
		return "", fmt.Errorf("ceph: mapping %s/%s printed no device path",
			c.cfg.ImagePool, image)
	}

	return device, nil
}

// writeImage copies the raw filesystem onto the mapped device.
//
// NOT BOUNDED BY THE RBD TIMEOUT, deliberately. Every other call here is a
// control operation that answers in milliseconds and is rightly cut off if it
// does not; this one moves four gigabytes and would be severed partway by the
// same bound — leaving a head image containing half a filesystem and no error
// that says so.
func writeImage(rawPath, device string, want int64) error {
	src, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("ceph: cannot read %s: %w", rawPath, err)
	}

	defer func() { _ = src.Close() }()

	// O_WRONLY WITHOUT O_CREATE OR O_TRUNC. The destination is a block device that
	// already exists at exactly the right size; O_CREATE would quietly make a
	// regular FILE if the mapping had gone missing, and the import would then
	// "succeed" into a file nobody reads. O_TRUNC on a block device is meaningless
	// at best.
	dst, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("ceph: cannot write to %s: %w", device, err)
	}

	defer func() { _ = dst.Close() }()

	written, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("ceph: could not write the image to %s: %w", device, err)
	}

	// CHECKED AGAINST THE SIZE THAT WAS MEASURED, because the file was statted
	// earlier and opened later. If it was truncated or replaced in between, io.Copy
	// reports a clean EOF and this would otherwise snapshot a partial filesystem as
	// a published generation.
	if written != want {
		return fmt.Errorf("ceph: %s measured %d bytes and %d were written; it changed underneath "+
			"this import, and a partial filesystem must not be published", rawPath, want, written)
	}

	// SYNCED BEFORE THE CALLER UNMAPS. Without it the snapshot can capture a
	// device whose last writes are still in the kernel's page cache, and the
	// resulting generation boots or does not depending on timing that never
	// reproduces.
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("ceph: could not flush the image to %s: %w", device, err)
	}

	return dst.Close()
}
