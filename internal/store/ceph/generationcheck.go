package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

var _ storecontract.GenerationChecker = (*Client)(nil)

// generationMapSlot is checkSlot for every Client in this process: a node holds
// more than one (its cache store, its root disks), and a sweep by one must not
// unmap a check the other is reading.
var generationMapSlot = make(chan struct{}, 1)

// generationMapLockPoll is how often a check waiting on another process's
// generation-map lock tries again.
const generationMapLockPoll = 100 * time.Millisecond

// WithGenerationMapLock extends the generation-map exclusion to every process on
// the host through an flock on path, which must be in a directory only billet's
// account can write (a node's lock_dir): a file another account could create
// there could be held to stop every check. Without it the exclusion is this
// process's alone.
func WithGenerationMapLock(path string) Option {
	return func(c *Client) { c.mapLock = path }
}

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
// after the command's bound) is cleaned up by unmapping them all in handle's
// pool, and the sweeps (Evict's of the cache pool, a pull's of the image pool)
// can do the same for what a crash left behind.
func (c *Client) withGenerationMap(
	ctx context.Context, handle, snapshot string, fn func(device string) error,
) error {
	release, err := c.holdGenerationMaps(ctx)
	if err != nil {
		return err
	}
	defer release()

	pool, _, _ := strings.Cut(handle, "/")
	spec := handle + "@" + snapshot
	out, err := c.rbdMap(ctx, spec, "--read-only")
	device := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(device, "/dev/rbd") {
		if err == nil {
			err = fmt.Errorf("%s answered %s, which is not a device path", c.bin, bounded(device))
		}

		return errors.Join(fmt.Errorf("ceph: map %s read-only: %w", spec, err),
			c.unmapGenerationMaps(ctx, pool))
	}

	return errors.Join(fn(device), c.unmapDevice(ctx, device, spec))
}

// unmapGenerationMaps unmaps every read-only mapping of a generation's snapshot
// in pool. Called with checkSlot held, under a context of its own, because it
// cleans up after a caller that may have given up.
func (c *Client) unmapGenerationMaps(ctx context.Context, pool string) error {
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
		if !c.generationMap(d, pool) {
			continue
		}
		// NO UNMAP STARTS PAST THE BUDGET. One already started runs to its own
		// bound (unmapDevice will not be cut short), so the slot is held for at
		// most c.wait plus one unmap's bound, and the rest wait for the next sweep.
		if cleanupCtx.Err() != nil {
			failures = append(failures, fmt.Errorf("ceph: the generation-map cleanup ran out of time; "+
				"%s and any after it are left for the next sweep", d.Device))

			break
		}
		if err := c.unmapDevice(cleanupCtx, d.Device, d.Name+"@"+d.Snap); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// generationMap reports whether a kernel mapping is a read-only map of a
// generation's snapshot in pool, which only withGenerationMap makes: a cache
// generation's in the cache pool, or in the image pool a golden image's that
// LearnArchitectures reads.
//
// A NAME BILLET GIVES A GENERATION, EXACTLY, as every destructive pass requires: a
// lookalike an operator mapped by hand is not billet's to unmap. A head (`-`), which
// an import writes through, and a completion root are neither.
func (c *Client) generationMap(d mappedDevice, pool string) bool {
	if d.Pool != pool || d.Snap == "" || d.Snap == "-" || d.Device == "" || d.Namespace != "" {
		return false
	}

	switch d.Pool {
	case c.cfg.CachePool:
		kind, _, named := cacheImageName(d.Name)

		return named && kind == "g"
	case c.cfg.ImagePool:
		_, isGeneration := ParseGeneration(d.Snap)

		return isGeneration && checkCloneName(d.Name) == nil
	default:
		return false
	}
}

// SweepGenerationMaps unmaps the read-only maps of golden-image generations a crash
// or a failed unmap left behind, under the host's generation-map lock: a pull or
// refresh runs it first, so a snapshot LearnArchitectures left mapped is not mapped
// twice. The image pool alone (ADR-022): the cache's maps are Evict's. A client
// without that lock sweeps nothing (errNoMapLock), since what it would unmap may be
// another process's read in progress.
func (c *Client) SweepGenerationMaps(ctx context.Context) error {
	if c.mapLock == "" {
		return errNoMapLock
	}

	return c.sweepGenerationMaps(ctx, c.cfg.ImagePool)
}

// GenerationMapLockPath is the file whose flock this client's read-only generation
// maps are held under, empty for none: what a unit declares it writes, and what a
// caller proves it was given.
func (c *Client) GenerationMapLockPath() string {
	return c.mapLock
}

// sweepGenerationMaps unmaps the read-only generation mappings in pool a crash or
// a failed unmap left behind, which would otherwise keep their images from ever
// being evicted.
func (c *Client) sweepGenerationMaps(ctx context.Context, pool string) error {
	release, err := c.holdGenerationMaps(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.unmapGenerationMaps(ctx, pool)
}

// holdGenerationMaps takes checkSlot and, when the client has one, the host's
// generation-map lock, in that order; release gives both back.
func (c *Client) holdGenerationMaps(ctx context.Context) (func(), error) {
	select {
	case c.checkSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	leaveSlot := func() { <-c.checkSlot }
	if c.mapLock == "" {
		return leaveSlot, nil
	}

	unlock, err := TakeGenerationMapLock(ctx, c.mapLock)
	if err != nil {
		leaveSlot()

		return nil, err
	}

	return func() {
		unlock()
		leaveSlot()
	}, nil
}

// TakeGenerationMapLock takes the host's generation-map lock at path, waiting for
// another holder until ctx ends, and answers its release: what every client given
// WithGenerationMapLock takes around a read-only generation map, and what a unit's
// path declaration exercises.
func TakeGenerationMapLock(ctx context.Context, path string) (func(), error) {
	f, err := openGenerationMapLock(path)
	if err != nil {
		return nil, fmt.Errorf("ceph: open the generation-map lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()

			return nil, fmt.Errorf("ceph: take the generation-map lock: %w", err)
		}

		timer := time.NewTimer(generationMapLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()

			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	return func() {
		// THE CLOSE RELEASES THE LOCK whatever the unlock answers: an flock belongs
		// to the open file description, and this is its only descriptor.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // the Close below releases it
		_ = f.Close()
	}, nil
}

// openGenerationMapLock opens the lock file, putting it in place 0660 when it is
// absent.
//
// GROUP-WRITABLE, because node.lock_dir may be a SETGID directory two accounts
// share, and a file only its creator could open would stop every check the
// other account makes. PUT IN PLACE WHOLE: the file is made under a temporary
// name, its mode set on the descriptor (so no umask narrows it), and only then
// linked to its name, which never replaces a file already there. A crash midway
// leaves a temporary file, never a lock file another account cannot open.
func openGenerationMapLock(path string) (*os.File, error) {
	return openGenerationMapLockRacing(path, func(string) {})
}

// openGenerationMapLockRacing is openGenerationMapLock with beforeLink run, given
// the temporary file, at the moment another process may create the file first.
func openGenerationMapLockRacing(path string, beforeLink func(tmp string)) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if !errors.Is(err, os.ErrNotExist) {
		return f, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := errors.Join(tmp.Chmod(0o660), tmp.Close()); err != nil {
		return nil, err
	}
	beforeLink(tmp.Name())
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	return os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
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
