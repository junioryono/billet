package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	storecontract "github.com/junioryono/billet/internal/store"
)

var _ storecontract.GenerationChecker = (*Client)(nil)

// e2fsckUncorrected is the bit e2fsck sets in its exit status for errors it left
// uncorrected, which under -n is every error it found. e2fsck 1.47.2 answered 12
// (this bit with 8, "cannot proceed") for a Go cache generation whose journal
// superblock was corrupt, measured 2026-10-13; 8 alone is a check that could not
// run, and says nothing about the filesystem.
const e2fsckUncorrected = 4

// GenerationDamaged checks a published generation's own snapshot, mapped
// read-only, with `e2fsck -f -n`. true is proof of damage even when the unmap
// after it failed, which the next sweep retries; false with an error is
// could-not-tell.
//
// THE GENERATION, NOT A JOB'S COPY OF IT. Clone copies a generation whose
// lineage is at its limit or unrecorded, and a damaged copy says nothing about
// the generation it was made from.
func (c *Client) GenerationDamaged(ctx context.Context, key, generation string) (bool, error) {
	if err := checkCacheKey(key); err != nil {
		return false, err
	}
	handle, err := c.generationHandle(ctx, key, generation)
	if err != nil {
		return false, err
	}

	damaged := false
	err = c.withGenerationMap(ctx, handle, generation, func(device string) error {
		var checkErr error
		damaged, checkErr = c.damaged(ctx, device)

		return checkErr
	})

	return damaged, err
}

// generationHandle is the image that holds a key's generation as its snapshot.
func (c *Client) generationHandle(ctx context.Context, key, generation string) (string, error) {
	if generation == "" {
		return "", errors.New("ceph: a generation check needs a generation")
	}

	var pointer cachePointer
	ok, err := c.readJSON(ctx, generationKey(key, generation), &pointer)
	if err != nil {
		return "", err
	}
	if !ok {
		// A generation published before generation records were written is
		// recorded only by the pointer that names it.
		if ok, err = c.readJSON(ctx, pointerKey(key), &pointer); err != nil {
			return "", err
		}
	}
	if !ok || pointer.Generation != generation {
		return "", fmt.Errorf("%w: cache %q has no generation %q", storecontract.ErrMiss, key, generation)
	}
	if !strings.HasPrefix(pointer.Handle, c.cfg.CachePool+"/cache-g-") {
		return "", fmt.Errorf("ceph: cache %q generation %q is held by %s, not a generation image",
			key, generation, bounded(pointer.Handle))
	}

	return pointer.Handle, nil
}

// withGenerationMap maps handle@snapshot read-only, runs fn on the device and
// unmaps it, all under checkSlot.
//
// UNDER ONE SLOT, so every read-only generation mapping on this host while the
// slot is held is this call's: a map that failed (the kernel can finish one
// after the command's bound) is cleaned up by unmapping them all, and the
// sweep in Evict can do the same for what a crash left behind.
func (c *Client) withGenerationMap(
	ctx context.Context, handle, snapshot string, fn func(device string) error,
) error {
	select {
	case c.checkSlot <- struct{}{}:
		defer func() { <-c.checkSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}

	spec := handle + "@" + snapshot
	out, err := c.rbdMap(ctx, spec, "--read-only")
	device := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(device, "/dev/rbd") {
		if err == nil {
			err = fmt.Errorf("%s answered %s, which is not a device path", c.bin, bounded(device))
		}

		return errors.Join(fmt.Errorf("ceph: map %s read-only: %w", spec, err), c.unmapGenerationMaps(ctx))
	}

	return errors.Join(fn(device), c.unmapDevice(ctx, device, spec))
}

// unmapGenerationMaps unmaps every read-only mapping of a generation's snapshot
// in the cache pool. Called with checkSlot held, under a context of its own,
// because it cleans up after a caller that may have given up.
func (c *Client) unmapGenerationMaps(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
	defer cancel()

	out, err := c.rbdCmd(cleanupCtx, true, "device", "list")
	if err != nil {
		return fmt.Errorf("ceph: list the mapped rbd devices: %w", err)
	}
	var devices []mappedDevice
	if err := json.Unmarshal(trimSpace(out), &devices); err != nil || devices == nil {
		return fmt.Errorf("ceph: %s did not answer with a json device list; is it the rbd command?", c.bin)
	}

	var failures []error
	for _, d := range devices {
		if d.Pool != c.cfg.CachePool || !strings.HasPrefix(d.Name, "cache-g-") ||
			d.Snap == "" || d.Snap == "-" || d.Device == "" {
			continue
		}
		if err := c.unmapDevice(cleanupCtx, d.Device, d.Name+"@"+d.Snap); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// sweepGenerationMaps unmaps the read-only generation mappings a crash or a
// failed unmap left behind, which would otherwise keep their images from ever
// being evicted.
func (c *Client) sweepGenerationMaps(ctx context.Context) error {
	select {
	case c.checkSlot <- struct{}{}:
		defer func() { <-c.checkSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}

	return c.unmapGenerationMaps(ctx)
}

// checkDamage runs `e2fsck -f -n` on device: damaged, clean, or could-not-tell.
func checkDamage(ctx context.Context, device string) (bool, error) {
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		return false, fmt.Errorf("e2fsck is required to check a cache generation: %w", err)
	}

	return checkDamageWith(ctx, e2fsck, device)
}

// checkDamageWith is checkDamage with the e2fsck to run. Its output is
// discarded, never buffered: a badly damaged filesystem's report is as long as
// its damage.
func checkDamageWith(ctx context.Context, e2fsck, device string) (bool, error) {
	err := exec.CommandContext(ctx, e2fsck, "-f", "-n", device).Run()
	if err == nil {
		return false, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() > 0 &&
		exitErr.ExitCode()&e2fsckUncorrected != 0 {
		return true, nil
	}

	return false, fmt.Errorf("check the filesystem on %s: %w", device, err)
}
