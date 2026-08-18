package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// ErrNoSuchImage is returned when a golden image or its snapshot is not there.
//
// A SENTINEL BECAUSE THE CALLER'S NEXT MOVE DIFFERS. A missing golden image is a
// deployment that has not published one — an operator action — while a cluster
// that could not be reached is a transient the node should keep retrying. Both
// arrive as `(2) No such file or directory` on stderr, so without this the launch
// path cannot tell "publish an image" from "the monitors are down".
var ErrNoSuchImage = errors.New("the golden image or its snapshot is not in the image pool")

// snapshotSpec is a golden image reference: an image name, an `@`, a snapshot.
//
// BOTH HALVES ARE REQUIRED, and that is a decision rather than a validation
// convenience. Choosing a generation when a caller names only an image is #25's
// job — it is the whole subject of that issue — so #24 refuses to invent a rule
// that would then have to be unpicked. An operator naming `ubuntu-2404-x64` gets
// a message pointing at the snapshot they meant.
//
// The character rules are the ones billet already applies to a pool, for the same
// measured reason: these become POSITIONAL `pool/image@snap` arguments, where rbd
// reads a leading dash as an option it does not recognise.
var snapshotSpec = regexp.MustCompile(`^[^-/@][^/@]*@[^/@]+$`)

// CloneRoot clones a golden image's snapshot into the cache pool, maps it, and
// returns the host device the kernel client gave it.
//
// NO `snap protect`, WHICH IS THE WHOLE POINT OF THE CLONE-V2 PRECONDITION. On a
// clone-v1 cluster the protect step is mandatory and a protected snapshot with a
// live clone can be neither unprotected nor removed, so a generation any running
// job holds would be undeletable. CheckReachable refuses such a cluster, which is
// what lets this function be three commands instead of five.
//
// IT REMOVES THE CLONE IF THE MAP FAILS. A clone that exists and is not mapped is
// invisible to everything above — no jail carries its name, so the sweep never
// looks for it — and it holds pool space until an operator finds it by hand.
// ResolveGeneration turns a tier's image reference into one exact generation.
//
// THE ONLY PLACE THAT KNOWS WHAT `@verified` MEANS, and it answers with a concrete
// reference so that everything downstream — the log line naming what a job booted,
// the clone, the metadata — is about a generation somebody can go and look at. A
// caller that passed the alias through would leave "which image did this job
// actually run?" unanswerable after the fact.
//
// AN EXPLICIT GENERATION IS RETURNED UNCHANGED, including one that was never
// verified: pinning is a decision, and second-guessing it would make a pinned tier
// mean something other than what it says.
func (c *Client) ResolveGeneration(ctx context.Context, image string) (string, error) {
	name, generation, found := strings.Cut(strings.TrimSpace(image), "@")
	if !found || generation != Verified {
		return image, nil
	}

	newest, ok, err := c.NewestVerified(ctx, image)
	if err != nil {
		return "", err
	}

	if !ok {
		return "", fmt.Errorf("ceph: %s names @%s and no generation of %s has passed "+
			"verification, so there is nothing proved to boot; publish one with "+
			"scripts/build-guest-image.sh and `billet images verify`, or pin a generation",
			image, Verified, name)
	}

	return name + "@" + newest.Name, nil
}

func (c *Client) CloneRoot(
	ctx context.Context, image, name string, capacity config.ByteSize,
) (string, error) {
	if !snapshotSpec.MatchString(image) {
		return "", fmt.Errorf("ceph: %s is not a golden image reference: billet clones a "+
			"named SNAPSHOT, written image@snapshot (for example ubuntu-2404-x64@g1), because "+
			"choosing a generation for you is the storage layer's job and it does not exist yet",
			bounded(image))
	}

	if err := checkCloneName(name); err != nil {
		return "", err
	}

	if capacity <= 0 {
		return "", fmt.Errorf("ceph: root disk %s needs a positive capacity, got %s",
			bounded(name), capacity)
	}

	src := c.cfg.ImagePool + "/" + image
	dst := c.cfg.CachePool + "/" + name

	if _, err := c.rbdCmd(ctx, false, "clone", src, dst); err != nil {
		if isNoSuchFile(err) {
			return "", fmt.Errorf("%w: %s could not be cloned as client.%s: %w",
				ErrNoSuchImage, src, c.cfg.User, err)
		}

		return "", fmt.Errorf("ceph: clone %s to %s as client.%s: %w", src, dst, c.cfg.User, err)
	}

	if err := c.growRoot(ctx, dst, capacity); err != nil {
		if rmErr := c.removeClone(ctx, name); rmErr != nil {
			return "", fmt.Errorf("%w (and the clone it made could not be removed, so %s is "+
				"holding pool space: %w)", err, dst, rmErr)
		}

		return "", err
	}

	device, err := c.mapRoot(ctx, name)
	if err != nil {
		// BEST EFFORT, AND ITS FAILURE IS REPORTED RATHER THAN REPLACING THE CAUSE.
		// The caller needs to know why the map failed; a second failure here is a
		// clone left in the pool, which is worth saying and is not the headline.
		if rmErr := c.removeClone(ctx, name); rmErr != nil {
			return "", fmt.Errorf("%w (and the clone it made could not be removed, so %s is "+
				"holding pool space: %w)", err, dst, rmErr)
		}

		return "", err
	}

	return device, nil
}

// growRoot makes a per-job root image at least as large as the tier promised.
//
// GROWN, NEVER SHRUNK. A golden image may be larger than an older tier's request,
// and truncating its clone would cut a live filesystem off at an arbitrary block.
// The cooperative guest runs resize2fs online before starting the runner, which
// turns this block-device capacity into filesystem capacity without modifying the
// immutable generation every job shares.
func (c *Client) growRoot(ctx context.Context, spec string, capacity config.ByteSize) error {
	out, err := c.rbdCmd(ctx, true, "info", spec)
	if err != nil {
		return fmt.Errorf("ceph: inspect the root disk clone %s before sizing it: %w", spec, err)
	}

	var info struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Size <= 0 {
		return fmt.Errorf("ceph: %s did not describe %s as json with a positive size; is it "+
			"the rbd command?", c.bin, spec)
	}

	wantMiB := (int64(capacity) + bytesPerMB - 1) / bytesPerMB
	haveMiB := info.Size / bytesPerMB
	if haveMiB >= wantMiB {
		return nil
	}

	if _, err := c.rbdCmd(ctx, false, "resize", "--size", fmt.Sprintf("%dM", wantMiB),
		spec); err != nil {
		return fmt.Errorf("ceph: root disk clone %s holds %dM and the tier promises %dM, and "+
			"it could not be grown: %w", spec, haveMiB, wantMiB, err)
	}

	return nil
}

// mapRoot maps a cache-pool image and returns the device the kernel gave it.
//
// MAPPING IS NOT IDEMPOTENT — measured. A second `rbd device map` of the same
// image warns `image already mapped as /dev/rbd1` on stderr and then maps it
// AGAIN, at /dev/rbd2. So this is never retried blindly: a retry does not
// converge, it accumulates devices that nothing will unmap because DiscardRoot
// would have to know how many to expect.
func (c *Client) mapRoot(ctx context.Context, name string) (string, error) {
	spec := c.cfg.CachePool + "/" + name

	out, err := c.rbdCmd(ctx, false, "device", "map", spec)
	if err != nil {
		return "", fmt.Errorf("ceph: map %s as client.%s: %w", spec, c.cfg.User, err)
	}

	device := strings.TrimSpace(string(out))

	// THE SHAPE IS CHECKED BECAUSE THIS VALUE BECOMES A mknod TARGET. It is passed
	// to the caller, which stats it for a major and minor number and creates a
	// device node inside a jail from the answer. A value that is not a path rbd
	// just produced has no business reaching that.
	if !strings.HasPrefix(device, "/dev/rbd") {
		return "", fmt.Errorf("ceph: %s answered %s when asked to map %s, which is not a device "+
			"path; is it the rbd command?", c.bin, bounded(device), spec)
	}

	return device, nil
}

// DiscardRoot unmaps every mapping of a per-job clone and removes it.
//
// IDEMPOTENT, because it runs on teardown paths that have already failed once.
// Neither underlying command is idempotent on its own: unmapping a spec nothing
// has mapped answers `(22) Invalid argument`, and removing an image that is not
// there answers `(2) No such file or directory` — both measured, and both mean
// "already done" here.
//
// EVERY MAPPING, NOT ONE. `rbd device unmap <spec>` on an image mapped twice
// reports `mapped more than once, unmapping /dev/rbd1 only` and leaves the other
// in place — so this reads the mapping table and unmaps by DEVICE, which names
// exactly one. An unmapped device left behind pins the image, and the remove then
// fails for a reason that names neither.
func (c *Client) DiscardRoot(ctx context.Context, name string) error {
	if err := checkCloneName(name); err != nil {
		return err
	}

	devices, err := c.mappedDevices(ctx, name)
	if err != nil {
		return err
	}

	for _, device := range devices {
		if err := c.unmapDevice(ctx, device, name); err != nil {
			return err
		}
	}

	return c.removeClone(ctx, name)
}

// unmapDevice releases one mapping, waiting out the moment after a VMM exits when
// the kernel still holds it.
//
// EBUSY HERE IS A RACE, NOT A REFUSAL, and it is one billet loses by construction.
// Teardown stops the VMM and waits for the process to be GONE — but a process
// exiting is not the same as its descriptors being closed, and for a moment after
// the last reference drops the kernel client still holds the device. `rbd device
// unmap` then answers `(16) Device or resource busy`.
//
// MEASURED, by running the thing: `billet images verify` destroys its probe the
// instant the guest reports back, which is the tightest this gap ever gets, and it
// failed there while the same teardown driven by a test that polled every two
// seconds never did. Treating it as a hard failure means a teardown that is correct
// in every observable way still reports an error and leaves a mapped device pinning
// an image nothing can remove.
//
// SO IT IS RETRIED, BRIEFLY, AND ONLY FOR THIS. Every other reason unmap can fail is
// a real one and is returned immediately: retrying those would turn a clear error
// into a slow one. The budget is short because the condition is the tail of a close
// rather than any kind of work.
func (c *Client) unmapDevice(ctx context.Context, device, name string) error {
	const (
		attempts = 20
		pause    = 250 * time.Millisecond
	)

	var err error

	for attempt := range attempts {
		if _, err = c.rbdCmd(ctx, false, "device", "unmap", device); err == nil {
			return nil
		}

		if !isDeviceBusy(err) {
			break
		}

		if attempt == attempts-1 {
			break
		}

		// AN EXPLICIT TIMER, STOPPED, because time.After leaves its timer alive
		// until it fires — and this is inside a loop inside a teardown that runs
		// per job.
		timer := time.NewTimer(pause)

		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("ceph: unmap %s, which is a root disk for %s: %w", device, name,
				errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
	}

	return fmt.Errorf("ceph: unmap %s, which is a root disk for %s: %w", device, name, err)
}

// isDeviceBusy reports whether the kernel client still holds a device.
//
// MATCHED ON THE ERRNO IT PRINTS, because `rbd` exits 16 for this and the number is
// the errno rather than a code of its own — measured, like every other exit status
// this package discriminates on.
func isDeviceBusy(err error) bool {
	return strings.Contains(err.Error(), "(16) Device or resource busy") ||
		strings.Contains(strings.ToLower(err.Error()), "device or resource busy")
}

// removeClone deletes a cache-pool image, treating an absent one as success.
func (c *Client) removeClone(ctx context.Context, name string) error {
	spec := c.cfg.CachePool + "/" + name

	if _, err := c.rbdCmd(ctx, false, "rm", spec); err != nil {
		if isNoSuchFile(err) {
			return nil
		}

		return fmt.Errorf("ceph: remove %s as client.%s: %w", spec, c.cfg.User, err)
	}

	return nil
}

// mappedDevice is one row of the kernel client's mapping table.
//
// THE IMAGE FIELD IS `name` IN JSON, though the human-readable table heading for
// the same column reads `image`. Measured against 20.2.3; decoding the heading
// instead yields an empty string for every row, which reads as "nothing is
// mapped" and silently skips the unmap.
type mappedDevice struct {
	Pool   string `json:"pool"`
	Name   string `json:"name"`
	Device string `json:"device"`
}

// mappedDevices lists the host devices a cache-pool image is mapped to.
func (c *Client) mappedDevices(ctx context.Context, name string) ([]string, error) {
	out, err := c.rbdCmd(ctx, true, "device", "list")
	if err != nil {
		return nil, fmt.Errorf("ceph: list the mapped rbd devices: %w", err)
	}

	// A `null` UNMARSHALS INTO A SLICE HAPPILY and would read as "nothing is
	// mapped" — which here means "skip the unmap and remove an image the kernel is
	// still holding". An empty table is `[]`, so nil is not a shape rbd produces.
	var devices []mappedDevice
	if err := json.Unmarshal(trimSpace(out), &devices); err != nil || devices == nil {
		return nil, fmt.Errorf("ceph: %s did not answer with a json device list; is it the rbd "+
			"command?", c.bin)
	}

	var found []string

	for _, d := range devices {
		if d.Pool == c.cfg.CachePool && d.Name == name && d.Device != "" {
			found = append(found, d.Device)
		}
	}

	return found, nil
}

// checkCloneName refuses a clone name billet cannot address.
//
// The same measured rules as a pool name, and for the same reason: this is half
// of a POSITIONAL `pool/image` argument. `/` and `@` would point the spec at
// another pool or a snapshot, and a leading dash is read by rbd as an option it
// does not recognise.
func checkCloneName(name string) error {
	if name == "" {
		return errors.New("ceph: a root disk needs a name")
	}

	if strings.ContainsAny(name, "/@") || strings.HasPrefix(name, "-") ||
		strings.TrimSpace(name) != name {
		return fmt.Errorf("ceph: %s is not a usable image name: billet addresses an image as a "+
			"positional pool/image argument", bounded(name))
	}

	return nil
}

// isNoSuchFile reports whether rbd failed because the thing was not there.
//
// MATCHED ON THE ERRNO rbd PRINTS, not on prose, because that number is what the
// tool is actually reporting and it is stable across the phrasings — `clone
// error`, `delete error` and `error opening image` all carry `(2)`. Both an
// absent image and an absent snapshot produce it, which is what the caller wants:
// either way there is nothing there.
func isNoSuchFile(err error) bool {
	return strings.Contains(err.Error(), "(2) No such file or directory")
}

// trimSpace is bytes.TrimSpace without the import, kept beside its one caller.
func trimSpace(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

// GenerationGone reports whether a CloneRoot failure means the generation is no
// longer there.
//
// THE STORE ANSWERS QUESTIONS ABOUT ITS OWN ERRORS, because the provider may not
// import this package -- so a caller that needs to distinguish "that generation
// was deleted" from "the cluster is unreachable" asks rather than matching a
// sentinel it would have to import.
func (c *Client) GenerationGone(err error) bool {
	return errors.Is(err, ErrNoSuchImage)
}
