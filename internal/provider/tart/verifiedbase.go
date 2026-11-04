package tart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/regularfile"
)

// A base is a local VM billet owns, a verified copy every launch clones (ADR-021). Its
// name is outside the `billet-` prefix provider.LeaseOf reads as a lease, so no node's
// List, sweep or inventory treats it as compute, and the deployment in it keeps two
// deployments' bases apart.
const (
	basePrefix  = "billetbase-"
	probePrefix = "billetprobe-"

	baseDisk   = "disk.img"
	baseConfig = "config.json"
	baseNVRAM  = "nvram.bin"
	// baseOverlay marks a stacked VM, whose layers stay in the store it was cloned in.
	baseOverlay = "overlay.asif"
	// baseSuspended is what `tart run` would resume instead of booting cold.
	baseSuspended = "state.vzvmsave"

	// privateStores holds, under node.state_dir, the stores remote images are pulled in.
	privateStores = "tart-private"
)

// hashName is the short form of an image key used in the names of what belongs to it.
func hashName(key string) string {
	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:])[:12]
}

func (p *Provider) baseName(key string, generation int64) string {
	return basePrefix + p.owner + "-" + hashName(key) + "-" + strconv.FormatInt(generation, 10)
}

func (p *Provider) probeName(key string) string {
	return probePrefix + p.owner + "-" + hashName(key)
}

// privateStore is where one image's verification pulls, this deployment's alone.
func (p *Provider) privateStore(key string) string {
	return filepath.Join(p.stateDir, privateStores, p.owner+"-"+hashName(key))
}

func (p *Provider) baseLockPath(base string) string {
	return p.billetPath("base-" + base + ".lock")
}

func (p *Provider) custodyPath(probe string) string {
	return p.billetPath("probe-" + probe + ".custody")
}

func (p *Provider) verificationLockPath(hash string) string {
	return p.billetPath("verify-" + p.owner + "-" + hash + ".lock")
}

// runShared runs tart in the shared store with its automatic pruning off, because tart
// reclaims cache space after any clone and this could otherwise evict an image another
// deployment uses.
func (p *Provider) runShared(ctx context.Context, args ...string) (string, error) {
	return p.runIn(ctx, p.home, nil, args...)
}

// runIn runs tart against one store with automatic pruning off, streaming to progress
// when one is given.
func (p *Provider) runIn(ctx context.Context, home string, progress io.Writer, args ...string) (string, error) {
	// #nosec G204 -- the binary is operator configuration and every argument is built
	// here, from typed config and validated names. No shell.
	cmd := exec.CommandContext(ctx, p.tart, args...)
	cmd.WaitDelay = commandWaitDelay
	cmd.Env = append(os.Environ(), "TART_HOME="+home, "TART_NO_AUTO_PRUNE=1")

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if progress != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, progress), io.MultiWriter(&stderr, progress)
	}

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", p.tart, args[0], err,
			strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// errNotRegular is a VM's file replaced by something else, which is an answer about the
// file, unlike a read that failed.
var errNotRegular = errors.New("not a regular file")

// fileIdentities reads the identity of each of a VM's three files, refusing a symlink,
// since tart would follow one and a record would name what it points at.
func fileIdentities(dir string) ([]baseFile, error) {
	names := []string{baseDisk, baseConfig, baseNVRAM}
	files := make([]baseFile, 0, len(names))

	for _, name := range names {
		var st unix.Stat_t

		if err := unix.Lstat(filepath.Join(dir, name), &st); err != nil {
			return nil, fmt.Errorf("read the identity of %s: %w", name, err)
		}

		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, fmt.Errorf("%s is %w", name, errNotRegular)
		}

		files = append(files, identityFrom(name, &st))
	}

	return files, nil
}

// hashFile is the sha256 of a regular file, read in a way a caller can cancel.
func hashFile(ctx context.Context, path string) (string, error) {
	f, _, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		return "", err
	}

	defer func() { _ = f.Close() }()

	sum := sha256.New()

	if _, err := io.Copy(sum, contextReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

// contextReader ends a long read with its caller, because a base's disk is tens of
// gigabytes.
type contextReader struct {
	ctx context.Context //nolint:containedctx // lives for one io.Copy
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(b)
}

// clonedConfigFields are what a clone regenerates, so they say nothing about content:
// the MAC address when it clashes, and a Linux guest's machine identifier. Both are read
// from tart's source (VMConfig.swift, 2026-11-04).
var clonedConfigFields = []string{"macAddress", "machineIdentifier"}

// hashContent computes a VM's exact hash and the key that judges distinct content.
func hashContent(ctx context.Context, dir string) (string, string, error) {
	disk, err := hashFile(ctx, filepath.Join(dir, baseDisk))
	if err != nil {
		return "", "", err
	}

	nvram, err := hashFile(ctx, filepath.Join(dir, baseNVRAM))
	if err != nil {
		return "", "", err
	}

	raw, err := regularfile.ReadFile(filepath.Join(dir, baseConfig), maxVMConfig,
		regularfile.Options{NoFollow: true})
	if err != nil {
		return "", "", err
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", "", fmt.Errorf("read the VM's configuration: %w", err)
	}

	for _, field := range clonedConfigFields {
		delete(fields, field)
	}

	// json.Marshal sorts a map's keys, so one configuration has one encoding.
	canonical, err := json.Marshal(fields)
	if err != nil {
		return "", "", fmt.Errorf("encode the VM's configuration: %w", err)
	}

	combine := func(config []byte) string {
		sum := sha256.New()
		configSum := sha256.Sum256(config)
		_, _ = fmt.Fprintf(sum, "disk %s\nnvram %s\nconfig %s\n", disk, nvram, hex.EncodeToString(configSum[:]))

		return "sha256:" + hex.EncodeToString(sum.Sum(nil))
	}

	return combine(raw), combine(canonical), nil
}

// sameDevice refuses a private store the base could not be renamed out of.
func sameDevice(a, b string) error {
	var sa, sb unix.Stat_t

	if err := unix.Stat(a, &sa); err != nil {
		return err
	}

	if err := unix.Stat(b, &sb); err != nil {
		return err
	}

	if deviceOf(&sa) != deviceOf(&sb) {
		return fmt.Errorf("%s and %s are on different filesystems, and a base moves from its private "+
			"store into tart's by a rename; put node.state_dir on TART_HOME's filesystem", a, b)
	}

	return nil
}

// refuseStacked says whether a VM directory stands on its own. A stacked VM's directory
// holds only a writable overlay, and the layers beneath it stay in the store it was
// cloned in, so moving it would leave them behind. Whether the images a deployment names
// are stacked is measured on the reference Mac before this is relied on.
func refuseStacked(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, baseOverlay)); err == nil {
		return errors.New("the image is stacked: its VM holds a writable overlay over layers in the " +
			"store it was cloned in, so it cannot be kept as a base of its own")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for a stacked overlay: %w", err)
	}

	for _, name := range []string{baseDisk, baseConfig, baseNVRAM} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("the image's VM does not hold its own %s: %w", name, err)
		}
	}

	return nil
}

// checkManifestDigest holds what tart stored for a pull to the digest it asked for:
// `tart fqn` alone answers the name it was asked for, whatever the registry served. The
// private store is fresh, so it holds this digest's entry and no other, and that is what
// lets the walk not depend on how tart encodes a registry's host in a path.
func checkManifestDigest(store, digest string) error {
	root := filepath.Join(store, "cache", "OCIs")

	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if path == root && errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipAll
		}

		if err != nil {
			return err
		}

		if d.Type().IsRegular() && d.Name() == "manifest.json" && filepath.Base(filepath.Dir(path)) == digest {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("read the pulled manifest: %w", err)
	}

	if len(found) != 1 {
		return fmt.Errorf("the private store holds %d manifests for %s, not one", len(found), digest)
	}

	raw, err := regularfile.ReadFile(found[0], manifestLimit, regularfile.Options{NoFollow: true})
	if err != nil {
		return fmt.Errorf("read the pulled manifest: %w", err)
	}

	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("asked the registry for %s, and the manifest tart stored hashes to %s", digest, got)
	}

	return nil
}

// moveVMDirectory renames a VM directory within or into the shared store, under billet's
// store lock and then tart's, running no tart command under either.
func (p *Provider) moveVMDirectory(ctx context.Context, from, to string) error {
	unlock, err := p.lockStore(ctx, "move "+filepath.Base(to)+" into tart's store")
	if err != nil {
		return err
	}

	defer unlock()

	tartLock, err := p.lockTartHome(ctx)
	if err != nil {
		return err
	}

	defer tartLock.release()

	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("a VM already carries the name %s", filepath.Base(to))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check that %s is free: %w", filepath.Base(to), err)
	}

	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("move %s into tart's store: %w", filepath.Base(to), err)
	}

	return syncDir(filepath.Dir(to))
}

// makeBase makes the base of one verification: pulled into a store of its own for a
// remote reference, cloned in the shared store for a local VM. It answers the record's
// fields with the base under its final name. On any failure no finished base is left.
func (p *Provider) makeBase(
	ctx context.Context, key, digest string, generation int64, progress io.Writer,
) (*VerifiedRecord, error) {
	base := p.baseName(key, generation)
	staging := base + stagingSuffix

	var (
		err    error
		isCopy bool
	)

	if digest != "" {
		err = p.pullIntoStaging(ctx, key, digest, staging, progress)
		isCopy = true
	} else {
		_, err = p.runShared(ctx, "clone", key, staging)
	}

	if err != nil {
		return nil, err
	}

	defer func() {
		if _, serr := os.Lstat(p.vmDir(staging)); serr == nil {
			cleanCtx, cancel := cleanupContext(ctx)
			defer cancel()

			p.deleteVerifiedVM(cleanCtx, staging)
		}
	}()

	if !isCopy {
		if err := refuseStacked(p.vmDir(staging)); err != nil {
			return nil, err
		}
	}

	// A SAVED SUSPENSION WOULD BE RESUMED, and the probe would prove a resume rather
	// than the cold boot every job whose clone loses that state makes.
	if err := os.Remove(filepath.Join(p.vmDir(staging), baseSuspended)); err != nil &&
		!errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove the base's saved suspension: %w", err)
	}

	if err := p.writeOwner(staging); err != nil {
		return nil, err
	}

	guestOS, err := p.guestOSOf(staging)
	if err != nil {
		return nil, err
	}

	dir := p.vmDir(staging)

	before, err := fileIdentities(dir)
	if err != nil {
		return nil, err
	}

	exact, contentKey, err := hashContent(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("hash the base: %w", err)
	}

	after, err := fileIdentities(dir)
	if err != nil {
		return nil, err
	}

	if !slices.Equal(before, after) {
		return nil, errors.New("the base's files changed while they were being hashed")
	}

	// WITH TART'S LOCK RELEASED, under the staging name, before it can carry the name a
	// launch looks for: tart must be able to read it as a VM.
	if _, err := p.runShared(ctx, "get", staging); err != nil {
		return nil, fmt.Errorf("tart cannot read the base as a VM: %w", err)
	}

	if err := p.moveVMDirectory(ctx, dir, p.vmDir(base)); err != nil {
		return nil, err
	}

	return &VerifiedRecord{
		Image:       key,
		Base:        base,
		Generation:  generation,
		Digest:      digest,
		ContentHash: exact,
		ContentKey:  contentKey,
		GuestOS:     guestOS,
		Files:       after,
	}, nil
}

// pullIntoStaging pulls one digest in a store of its own and moves its clone to a
// staging name in the shared store, deleting the private store afterwards. A pull in
// the shared store could replace a digest another deployment pins or evict an entry
// another deployment's unverified tier uses, since tart re-points the cache entry it was
// asked for at whatever the registry returns.
func (p *Provider) pullIntoStaging(ctx context.Context, key, digest, staging string, progress io.Writer) error {
	store := p.privateStore(key)

	if err := os.RemoveAll(store); err != nil {
		return fmt.Errorf("remove an earlier private store: %w", err)
	}

	if err := os.MkdirAll(store, 0o700); err != nil {
		return fmt.Errorf("create the private store: %w", err)
	}

	defer func() {
		if err := os.RemoveAll(store); err != nil {
			p.log.Error("could not remove a private tart store; the next verification of this "+
				"image removes it", "path", store, "error", err)
		}
	}()

	vms := filepath.Join(p.home, "vms")
	if err := os.MkdirAll(vms, 0o755); err != nil {
		return fmt.Errorf("create tart's store: %w", err)
	}

	if err := sameDevice(store, vms); err != nil {
		return err
	}

	ref, err := pullReference(key, digest)
	if err != nil {
		return err
	}

	if _, err := p.runIn(ctx, store, progress, "pull", ref); err != nil {
		return fmt.Errorf("pull %s into a private store: %w", ref, err)
	}

	if err := checkManifestDigest(store, digest); err != nil {
		return err
	}

	if _, err := p.runIn(ctx, store, nil, "clone", ref, staging); err != nil {
		return fmt.Errorf("clone %s in its private store: %w", ref, err)
	}

	private := filepath.Join(store, "vms", staging)

	// ASKED HERE, before the move, because the layers of a stacked VM are what deleting
	// this store would remove.
	if err := refuseStacked(private); err != nil {
		return err
	}

	return p.moveVMDirectory(ctx, private, p.vmDir(staging))
}

// cloneBase clones a recorded base under its lock, comparing its files' identities
// before the clone and again after it completes. A difference discards the clone before
// it boots and marks the base invalid, because tart or an operator changed what was
// probed. Hashing tens of gigabytes on every launch is what this avoids.
func (p *Provider) cloneBase(ctx context.Context, rec VerifiedRecord, target string) error {
	held, err := lockPath(ctx, p.baseLockPath(rec.Base), syscall.LOCK_SH, p.storeLockWindow)
	if err != nil {
		return fmt.Errorf("lock base %s: %w", rec.Base, err)
	}

	defer held.release()

	return p.cloneHeldBase(ctx, rec, target)
}

// cloneHeldBase is cloneBase for a caller already holding the base's shared lock.
func (p *Provider) cloneHeldBase(ctx context.Context, rec VerifiedRecord, target string) error {
	if _, err := os.Lstat(p.vmDir(rec.Base)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("base %s of %s is gone from tart's store; verify the image again with "+
			"`billet images verify %s`", rec.Base, rec.Image, rec.Image)
	}

	if err := p.compareBase(ctx, rec); err != nil {
		return err
	}

	if _, err := p.runShared(ctx, "clone", rec.Base, target); err != nil {
		return fmt.Errorf("clone base %s: %w", rec.Base, err)
	}

	if err := p.compareBase(ctx, rec); err != nil {
		cleanCtx, cancel := cleanupContext(ctx)
		defer cancel()

		p.discardStaging(cleanCtx, target)

		return err
	}

	return nil
}

// compareBase holds a base's files to the record's identities and marks it invalid on a
// difference. A failed read is not a difference: it could not tell, and marks nothing.
func (p *Provider) compareBase(ctx context.Context, rec VerifiedRecord) error {
	now, err := fileIdentities(p.vmDir(rec.Base))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errNotRegular) {
		return fmt.Errorf("read base %s: %w", rec.Base, err)
	}

	if err == nil && slices.Equal(now, rec.Files) {
		return nil
	}

	why := "its files are not the ones billet probed"
	if err := p.withRecords(ctx, func(st *verifiedState) (bool, error) {
		for i := range st.verified.Records {
			if st.verified.Records[i].Base == rec.Base {
				st.verified.Records[i].Invalid = why

				return true, nil
			}
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("base %s changed, and that could not be recorded: %w", rec.Base, err)
	}

	return fmt.Errorf("base %s of %s changed since it was verified (%s), so no job boots a copy of "+
		"it; verify the image again with `billet images verify %s`", rec.Base, rec.Image, why, rec.Image)
}

// deleteVerifiedVM removes a base or a staging clone that a failure left, reporting a
// failure rather than returning it, since the next verification removes what remains.
func (p *Provider) deleteVerifiedVM(ctx context.Context, name string) {
	if err := p.deleteOwnedVM(ctx, name); err != nil {
		p.log.Error("could not remove a VM of a failed verification; the next verification of "+
			"its image removes it", "vm", name, "error", err)
	}
}

// deleteOwnedVM deletes a VM in this deployment's own namespace under the store lock.
//
// A MISSING MARKER IS NOT A REFUSAL HERE, unlike a lease name's, because the name itself
// carries this deployment's identity and nobody else's verification makes it; a clone
// interrupted before its marker would otherwise wedge the image. A marker naming another
// deployment, or one that cannot be read, refuses.
func (p *Provider) deleteOwnedVM(ctx context.Context, name string) error {
	unlock, err := p.lockStore(ctx, "delete "+name)
	if err != nil {
		return err
	}

	defer unlock()

	if owner, err := p.ownerOf(name); err == nil {
		if owner != p.owner {
			return fmt.Errorf("%s carries the marker of another deployment", name)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s's marker: %w", name, err)
	}

	// ITS OWN BOUND, whatever the caller's: this holds the store lock, which every
	// deployment on the Mac takes to publish a guest or remove one. What runs out is left
	// for the image's next verification.
	deleteCtx, cancel := context.WithTimeout(ctx, p.deleteWindow)
	defer cancel()

	if _, err := p.runShared(deleteCtx, "delete", name); err != nil && !isMissingVM(err) {
		return fmt.Errorf("delete %s: %w", name, err)
	}

	return nil
}

// deleteBase removes a finished base under its own exclusive lock, so a launch that
// resolved it before it was dropped still completes its clone first.
func (p *Provider) deleteBase(ctx context.Context, base string) error {
	held, err := lockPath(ctx, p.baseLockPath(base), syscall.LOCK_EX, p.storeLockWindow)
	if err != nil {
		return fmt.Errorf("lock base %s to delete it: %w", base, err)
	}

	defer held.release()

	return p.deleteOwnedVM(ctx, base)
}

// deleteBases removes bases dropped from the records. A failure leaves a base no record
// names, which the image's next verification removes, and every one is tried.
func (p *Provider) deleteBases(ctx context.Context, bases []string) error {
	var failures []error

	for _, base := range bases {
		if err := p.deleteBase(ctx, base); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}
