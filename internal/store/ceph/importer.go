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
func (c *Client) ImportGeneration(
	ctx context.Context,
	image, rawPath, runnerVersion string,
	now time.Time,
) (string, error) {
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
	wantMB := (info.Size() + bytesPerMB - 1) / bytesPerMB

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

	// Kept so a failed cleanup is not merely swallowed: it is reported alongside
	// the real failure below rather than instead of it.
	var unmapFailed error

	defer func() {
		if unmapped {
			return
		}

		// BEST EFFORT, AND THE FAILURE IS NOT THE ONE BEING REPORTED. This runs on
		// the way out of an import that already failed, so returning this error
		// would replace the reason the import failed with a consequence of it.
		if err := c.unmapDevice(ctx, device, image); err != nil {
			unmapFailed = err
		}
	}()

	if err := writeImage(rawPath, device); err != nil {
		if unmapFailed != nil {
			return "", fmt.Errorf("%w (and the head was left mapped: %w)", err, unmapFailed)
		}

		return "", err
	}

	// UNMAPPED BEFORE THE SNAPSHOT, NOT AFTER. The snapshot must capture what the
	// filesystem actually is, and a mapped device can still hold dirty pages the
	// kernel has not written back. Snapshotting first would capture the image as
	// of some moment nobody chose -- and the resulting generation would boot, or
	// not, depending on timing that never reproduces.
	if err := c.unmapDevice(ctx, device, image); err != nil {
		return "", err
	}

	unmapped = true

	generation := GenerationPrefix + now.UTC().Format(GenerationLayout)

	if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "snap", "create",
		image+"@"+generation); err != nil {
		return "", fmt.Errorf("ceph: could not publish %s@%s: %w", image, generation, err)
	}

	// RECORDED PER GENERATION, because a tier boots a generation rather than the
	// head. A single head-level key describes the LAST import, which is not what
	// any job runs: generations are immutable and promotion is deliberate, so a
	// fleet can sit on last month's generation while the head advances. An alarm
	// reading the head then reports the newest import as though it were the fleet.
	if err := c.SetRunnerVersion(ctx, image, generation, runnerVersion); err != nil {
		return generation, fmt.Errorf("ceph: %s@%s was published but its runner version could "+
			"not be recorded, so staleness checks will not see it: %w", image, generation, err)
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

		if _, err := c.rbdCmd(ctx, false, "-p", c.cfg.ImagePool, "create",
			"--size", fmt.Sprintf("%dM", wantMB), image); err != nil {
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
		"--size", fmt.Sprintf("%dM", wantMB), image); err != nil {
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
func writeImage(rawPath, device string) error {
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

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("ceph: could not write the image to %s: %w", device, err)
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
