package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
)

const (
	lifecycleDirName     = ".billet-instances"
	lifecycleExecDirName = ".billet-instance-executables"
)

// lockLifecycle serializes every launch and teardown that can act on a lease name.
// The node runtime is already a serial command consumer; the host-wide lock extends
// that property across deployments and Firecracker binary versions sharing a host.
func (p *Provider) lockLifecycle(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(p.cfg.ChrootBase, 0o700); err != nil {
		return nil, fmt.Errorf("firecracker: create the lifecycle lock directory: %w", err)
	}

	path := filepath.Join(p.cfg.ChrootBase, ".billet-lifecycle.lock")
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("firecracker: open the lifecycle lock %s: %w", path, err)
	}

	ticker := time.NewTicker(lifecycleLockPoll)
	defer ticker.Stop()

	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, errors.Join(
					fmt.Errorf("firecracker: wait for the lifecycle lock: %w", ctxErr),
					lock.Close(),
				)
			}

			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("firecracker: lock lifecycle operations: %w", err),
				lock.Close())
		}

		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("firecracker: wait for the lifecycle lock: %w", ctx.Err()),
				lock.Close())
		case <-ticker.C:
		}
	}
}

const lifecycleLockPoll = 20 * time.Millisecond

func (p *Provider) lifecycleDir() string {
	return filepath.Join(p.cfg.ChrootBase, lifecycleDirName)
}

func (p *Provider) lifecycleFile(id string) string {
	return filepath.Join(p.lifecycleDir(), id)
}

func (p *Provider) lifecycleExecDir() string {
	return filepath.Join(p.cfg.ChrootBase, lifecycleExecDirName)
}

func (p *Provider) lifecycleExecFile(id string) string {
	return filepath.Join(p.lifecycleExecDir(), id)
}

// reserveLifecycle publishes deployment authority before a launch creates anything
// named after the lease. The caller holds lockLifecycle through the whole launch.
func (p *Provider) reserveLifecycle(ctx context.Context, id string) error {
	if existing, found, err := p.findJail(id); err != nil {
		return err
	} else if found {
		return fmt.Errorf("firecracker: %w: %s already exists, and the jailer cannot reuse it",
			ErrJailExists, existing.dir())
	}

	if err := p.writeLifecycleOwner(id); err != nil {
		if errors.Is(err, os.ErrExist) {
			owner, readErr := lifecycleOwnerOf(p.lifecycleFile(id))
			if readErr != nil {
				return fmt.Errorf("firecracker: a lifecycle reservation for %s exists but its "+
					"deployment owner cannot be read: %w", id, readErr)
			}

			if owner != p.owner {
				return fmt.Errorf("firecracker: %w: %s is reserved by deployment %s",
					ErrJailExists, id, bounded(owner))
			}

			// The lock and the jail check above prove this is not an in-flight or
			// completed launch. Finish any cleanup a prior process left between the
			// durable reservation and the durable jail, then reserve afresh.
			if err := p.cleanupLifecycleOnly(ctx, id); err != nil {
				return fmt.Errorf("firecracker: reconcile the prior launch of %s: %w", id, err)
			}

			return p.writeLifecycleOwner(id)
		}

		return err
	}

	return nil
}

// authorizeLifecycle proves this deployment owns every destructive action for id.
// A pre-reservation jail is upgraded in place only after its exact owner marker
// supplies the missing authority.
func (p *Provider) authorizeLifecycle(id string, j jail, found bool) (bool, error) {
	record, err := lifecycleRecordOf(p.lifecycleFile(id))
	if err == nil {
		if record.owner != p.owner {
			return false, fmt.Errorf("firecracker: %s is reserved by deployment %s, not this one",
				id, bounded(record.owner))
		}

		execName := record.execName
		var execErr error
		if execName != "" {
			if found && execName != j.execName {
				return false, fmt.Errorf("firecracker: lifecycle authority for %s records executable %s, "+
					"but its jail is under %s", id, bounded(execName), bounded(j.execName))
			}

			execErr = p.ensureLifecycleExec(id, execName)
		} else {
			execName, execErr = lifecycleExecOf(p.lifecycleExecFile(id))
			if errors.Is(execErr, os.ErrNotExist) && found {
				execErr = p.writeLifecycleExec(id, j.execName)
				execName = j.execName
			}
		}
		if execErr != nil {
			return false, fmt.Errorf("firecracker: read lifecycle executable for %s: %w", id, execErr)
		}
		if found && execName != j.execName {
			return false, fmt.Errorf("firecracker: lifecycle authority for %s records executable %s, "+
				"but its jail is under %s", id, bounded(execName), bounded(j.execName))
		}

		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("firecracker: read lifecycle authority for %s: %w", id, err)
	}
	if !found {
		// A prior release may have removed the record and then failed to sync this
		// directory. Retrying the sync is what makes an idempotent Destroy finish the
		// durability promise instead of treating pathname absence as sufficient.
		if err := p.releaseLifecycle(id); err != nil {
			return false, fmt.Errorf("firecracker: finish absent lifecycle authority for %s: %w", id, err)
		}

		return false, nil
	}

	owner, err := ownerOf(j)
	if err != nil {
		return false, fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
	}
	if owner != p.owner {
		return false, fmt.Errorf("firecracker: %s belongs to deployment %s, not to this one",
			j.dir(), bounded(owner))
	}
	if err := p.writeLifecycleRecord(id, j.execName); err != nil {
		return false, fmt.Errorf("firecracker: preserve lifecycle authority for %s: %w", id, err)
	}

	return true, nil
}

// writeLifecycleOwner installs one canonical owner record and makes its complete
// path durable before any allocation or teardown is allowed to proceed.
func (p *Provider) writeLifecycleOwner(id string) error {
	return p.writeLifecycleRecord(id, p.execName)
}

func (p *Provider) writeLifecycleRecord(id, execName string) error {
	if err := validateLifecycleExecName(execName); err != nil {
		return err
	}
	if _, err := os.Lstat(p.lifecycleFile(id)); err == nil {
		return fmt.Errorf("firecracker: reserve lifecycle authority for %s: %w", id, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: inspect lifecycle authority for %s: %w", id, err)
	}

	// The owner file is the authority older binaries understand. With it absent,
	// an executable sidecar can only be residue from a crash before publication or
	// from a rollback binary that completed teardown without knowing the sidecar.
	if err := p.removeLifecycleExec(id); err != nil {
		return err
	}
	if err := p.writeLifecycleExec(id, execName); err != nil {
		return err
	}

	err := p.publishLifecycleFile(
		p.lifecycleDir(), p.lifecycleFile(id), "."+id+"-", p.owner+"\n", "authority", id,
	)
	if err != nil {
		return errors.Join(err, p.removeLifecycleExec(id))
	}

	return nil
}

func (p *Provider) writeLifecycleExec(id, execName string) error {
	if err := validateLifecycleExecName(execName); err != nil {
		return err
	}

	return p.publishLifecycleFile(
		p.lifecycleExecDir(), p.lifecycleExecFile(id), "."+id+"-", execName+"\n", "executable", id,
	)
}

func (p *Provider) ensureLifecycleExec(id, execName string) error {
	if existing, err := lifecycleExecOf(p.lifecycleExecFile(id)); err == nil && existing == execName {
		return nil
	}
	if err := p.removeLifecycleExec(id); err != nil {
		return err
	}

	return p.writeLifecycleExec(id, execName)
}

func (p *Provider) publishLifecycleFile(dir, path, prefix, contents, kind, id string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("firecracker: create the lifecycle %s directory %s: %w", kind, dir, err)
	}

	staged, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return fmt.Errorf("firecracker: stage lifecycle %s for %s: %w", kind, id, err)
	}
	defer os.Remove(staged.Name())

	cleanup := func(cause error) error {
		return errors.Join(cause, staged.Close())
	}

	if _, err := staged.WriteString(contents); err != nil {
		return cleanup(fmt.Errorf("firecracker: write lifecycle %s for %s: %w", kind, id, err))
	}
	if err := p.syncFile(staged); err != nil {
		return cleanup(fmt.Errorf("firecracker: sync lifecycle %s for %s: %w", kind, id, err))
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("firecracker: close lifecycle %s for %s: %w", kind, id, err)
	}
	if err := os.Link(staged.Name(), path); err != nil {
		return fmt.Errorf("firecracker: publish lifecycle %s for %s: %w", kind, id, err)
	}
	if err := p.syncDir(dir); err != nil {
		return errors.Join(fmt.Errorf("firecracker: sync lifecycle %s for %s: %w", kind, id, err),
			p.removePublishedLifecycleFile(path, dir, kind, id))
	}
	if err := p.syncDirectoryAncestors(dir); err != nil {
		return errors.Join(err, p.removePublishedLifecycleFile(path, dir, kind, id))
	}

	return nil
}

// releaseLifecycle removes deployment authority only after every owned resource is
// gone. The caller still holds the host-wide lock, so no replacement can start in
// the gap between removal and the directory sync.
func (p *Provider) releaseLifecycle(id string) error {
	if err := os.Remove(p.lifecycleFile(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("firecracker: release lifecycle authority for %s: %w", id, err)
	}
	if err := p.syncLifecycleDir(); err != nil {
		return fmt.Errorf("firecracker: sync released lifecycle authority for %s: %w", id, err)
	}

	return p.removeLifecycleExec(id)
}

func (p *Provider) removeLifecycleExec(id string) error {
	return p.removePublishedLifecycleFile(p.lifecycleExecFile(id), p.lifecycleExecDir(), "executable", id)
}

func (p *Provider) removePublishedLifecycleFile(path, dir, kind, id string) error {
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("firecracker: remove lifecycle %s for %s: %w", kind, id, err)
		}
	}
	if err := p.syncDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("firecracker: sync removed lifecycle %s for %s: %w", kind, id, err)
	}

	return nil
}

func (p *Provider) syncLifecycleDir() error {
	if err := p.syncDir(p.lifecycleDir()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	return nil
}

// cleanupLifecycleOnly finishes an interrupted launch or teardown after the jail
// is provably absent. The caller holds lockLifecycle, so a replacement cannot
// appear between that proof and cleanup.
func (p *Provider) cleanupLifecycleOnly(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("firecracker: lifecycle cleanup of %s was canceled: %w", id, err)
	}

	record, err := lifecycleRecordOf(p.lifecycleFile(id))
	if err != nil {
		return fmt.Errorf("firecracker: read lifecycle authority for %s: %w", id, err)
	}
	if record.owner != p.owner {
		return fmt.Errorf("firecracker: %s is reserved by deployment %s, not this one",
			id, bounded(record.owner))
	}

	execName := record.execName
	if execName != "" {
		if err := p.ensureLifecycleExec(id, execName); err != nil {
			return fmt.Errorf("firecracker: preserve lifecycle executable for %s: %w", id, err)
		}
	} else {
		execName, err = lifecycleExecOf(p.lifecycleExecFile(id))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("firecracker: read lifecycle executable for %s: %w", id, err)
		}
	}
	if execName != "" {
		j := jail{base: p.cfg.ChrootBase, execName: execName, id: id}
		if err := p.removeCgroupFn(j); err != nil {
			return err
		}
	} else if err := p.removeLegacyCgroups(id); err != nil {
		return err
	}

	var failures []error

	if orphan, err := p.claimedBy(id); err != nil {
		failures = append(failures, err)
	} else if err := p.releaseOrphaned(ctx, orphan, id); err != nil {
		failures = append(failures, err)
	}

	if err := p.discardWith(ctx, id, failures); err != nil {
		return err
	}

	return p.releaseLifecycle(id)
}

func (p *Provider) removeLegacyCgroups(id string) error {
	root, dirs, err := p.cgroupExecDirs()
	if err != nil {
		return err
	}

	var failures []error
	for _, execName := range dirs {
		j := jail{base: p.cfg.ChrootBase, execName: execName, id: id}
		if err := p.removeCgroupAtFn(root, j); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// reconcileLifecycleOnly removes records left by a process that exited before it
// could create a durable jail, or after it had completely unwound one.
func (p *Provider) reconcileLifecycleOnly(ctx context.Context, jails map[string]struct{}) error {
	entries, err := os.ReadDir(p.lifecycleDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("firecracker: list lifecycle authority: %w", err)
	}

	for _, entry := range entries {
		id := entry.Name()
		if entry.IsDir() {
			continue
		}
		if _, ours := provider.LeaseOf(id); !ours {
			continue
		}
		if _, found := jails[id]; found {
			continue
		}

		record, err := lifecycleRecordOf(p.lifecycleFile(id))
		if err != nil {
			// A lifecycle-only record is not inventory and supplies no authority when
			// it cannot be read. Preserve it for an operator rather than letting one
			// interrupted publication hide every real jail on the host.
			continue
		}
		if record.owner != p.owner {
			continue
		}
		if record.execName == "" {
			if _, err := lifecycleExecOf(p.lifecycleExecFile(id)); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		if err := p.cleanupLifecycleOnly(ctx, id); err != nil {
			return fmt.Errorf("firecracker: reconcile interrupted lifecycle %s: %w", id, err)
		}
	}

	return nil
}

func lifecycleOwnerOf(path string) (string, error) {
	record, err := lifecycleRecordOf(path)
	if err != nil {
		return "", err
	}

	return record.owner, nil
}

type lifecycleRecord struct {
	owner    string
	execName string
}

func lifecycleRecordOf(path string) (lifecycleRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return lifecycleRecord{}, err
	}

	parts := strings.Split(string(raw), "\n")
	if (len(parts) != 2 && len(parts) != 3) || parts[len(parts)-1] != "" {
		return lifecycleRecord{}, errors.New("lifecycle authority is not one or two newline-terminated fields")
	}

	owner, err := parseOwnerRecord([]byte(parts[0] + "\n"))
	if err != nil {
		return lifecycleRecord{}, err
	}

	record := lifecycleRecord{owner: owner}
	if len(parts) == 3 {
		if err := validateLifecycleExecName(parts[1]); err != nil {
			return lifecycleRecord{}, err
		}
		record.execName = parts[1]
	}

	return record, nil
}

func lifecycleExecOf(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) < 2 || raw[len(raw)-1] != '\n' || strings.Count(string(raw), "\n") != 1 {
		return "", errors.New("lifecycle executable is not one newline-terminated name")
	}

	execName := string(raw[:len(raw)-1])
	if err := validateLifecycleExecName(execName); err != nil {
		return "", err
	}

	return execName, nil
}

func validateLifecycleExecName(execName string) error {
	if execName == "" || execName == "." || execName == ".." ||
		filepath.Base(execName) != execName || strings.ContainsAny(execName, "\r\n") {
		return errors.New("lifecycle record has an invalid executable name")
	}

	return nil
}
