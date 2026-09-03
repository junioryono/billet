// Package durablefile installs a file so that a power loss cannot leave anything
// else pointing at a name that is not there.
//
// ONE ORDERING, IN ONE PLACE: metadata change, file sync, atomic rename, containing
// directory sync. Each step is obvious and the sequence is not, which is why it is a
// primitive rather than four lines every caller writes again.
//
// THE STEP EVERYBODY FORGETS IS THE LAST ONE. fsync(2) says plainly that flushing a
// file does not flush the DIRECTORY ENTRY that names it, and that an explicit fsync
// on the containing directory is required. `billet images pull` installed a kernel
// with a sync, a chmod after that sync and a rename — and then published Ceph
// metadata naming the file. A crash in between leaves a complete, remotely visible
// generation whose paired kernel disappeared in recovery: nodes resolve a verified
// generation and cannot boot the exact kernel it was verified against, which is the
// matched-pair invariant the firecracker backend rests on.
//
// THE MODE CHANGE COMES FIRST, before the sync rather than after it, because a sync
// flushes the inode as it is at that moment. A chmod behind it is a metadata change
// nothing has committed, so the file can come back with the mode the temporary file
// was created with — 0600, which is not readable by the account that boots a guest.
package durablefile

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Installer performs the durable install, with a seam at each step.
//
// THE ZERO VALUE IS THE REAL THING. A caller cannot accidentally construct one that
// does nothing, and a test overrides exactly the step it wants to fail. The hooks
// exist because the property that matters here — that a caller does not go on to
// publish something REMOTE when a local step failed — is only observable if a local
// step can be made to fail, and none of these can be provoked by ordinary means.
type Installer struct {
	SetMode  func(f *os.File, mode fs.FileMode) error
	SyncFile func(f *os.File) error
	Rename   func(from, to string) error
	SyncDir  func(dir string) error
}

// Install writes name into dir durably and returns the path it landed at.
//
// The write callback receives the staged file and may refuse: an error from it
// aborts before anything is renamed and is returned unchanged, which is what lets a
// caller verify a digest as it copies rather than after the file has a real name.
//
// WHAT IS LEFT BEHIND ON FAILURE, and it differs by step. Anything up to and
// including the rename leaves nothing under the final name — the staged file is
// removed, and no reader ever saw it. A failure of the DIRECTORY SYNC leaves the
// file where it is: the entry may already be durable, an unlink is not itself
// durable either, and the caller's retry re-checks the content and flushes the
// directory again. What must not happen — and is the whole point — is the caller
// treating that failure as success.
func (i Installer) Install(
	dir, name string,
	mode fs.FileMode,
	write func(w io.Writer) error,
) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("durablefile: %q is not a file name", name)
	}

	final := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, ".durable-*")
	if err != nil {
		return "", fmt.Errorf("durablefile: cannot stage a file in %s: %w", dir, err)
	}

	staged := tmp.Name()
	renamed := false

	defer func() {
		_ = tmp.Close()

		if !renamed {
			_ = os.Remove(staged)
		}
	}()

	if err := write(tmp); err != nil {
		return "", err
	}

	// BEFORE THE SYNC. See the package comment: a mode set afterwards is a metadata
	// change nothing has committed.
	if err := i.setMode()(tmp, mode); err != nil {
		return "", fmt.Errorf("durablefile: cannot set the mode of %s: %w", final, err)
	}

	if err := i.syncFile()(tmp); err != nil {
		return "", fmt.Errorf("durablefile: cannot flush %s: %w", final, err)
	}

	// CLOSED BEFORE THE RENAME, so a write buffered in the file object cannot land
	// after the name is published. The deferred Close then does nothing.
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("durablefile: cannot close the staged %s: %w", final, err)
	}

	if err := i.rename()(staged, final); err != nil {
		return "", fmt.Errorf("durablefile: cannot put %s in place: %w", final, err)
	}

	renamed = true

	// THE RENAME IS NOT THE COMMIT; THIS IS.
	if err := i.SyncDirectory(dir); err != nil {
		return "", err
	}

	return final, nil
}

// SyncDirectory flushes a directory's entries, so a rename into it survives a crash.
//
// EXPORTED BECAUSE THE REPAIR PATH NEEDS IT ALONE. A caller that finds the file
// already present — an interrupted run that renamed and then died — has nothing to
// install and everything still to commit, and returning success there would leave
// exactly the state Install exists to prevent, reachable by retrying.
func (i Installer) SyncDirectory(dir string) error { return i.syncDir()(dir) }

// MkdirAll creates a directory and commits the entries that NAME it.
//
// BECAUSE FLUSHING A DIRECTORY DOES NOT COMMIT THE DIRECTORY. Install flushes the
// entries INSIDE dir, which is what makes the file it just renamed durable — and
// says nothing about the entry for dir itself in its parent. On a fresh host the
// kernel directory does not exist until the first pull creates it, so without this
// a power loss can take the whole directory away and leave exactly the failure
// Install exists to prevent: a published generation naming a kernel that is gone.
//
// EVERY ANCESTOR, EVERY TIME, INCLUDING WHEN THE DIRECTORY ALREADY EXISTS. A
// previous run may have created it and died before its parent was flushed, and a
// retry that skipped the flush would certify that state instead of repairing it —
// the same rule the "already installed" branch of an install follows. It is a
// handful of fsyncs on directories, once per operation.
func (i Installer) MkdirAll(dir string, mode fs.FileMode) error {
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("durablefile: cannot resolve %s: %w", dir, err)
		}

		dir = abs
	}

	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("durablefile: cannot create %s: %w", dir, err)
	}

	// THE RESOLVED PATH AS WELL AS THE ONE THAT WAS ASKED FOR, because os.MkdirAll
	// FOLLOWS symlinked components while a lexical walk climbs the name. Given
	// /var/lib/billet/kernels where billet is a link to /mnt/vol/billet, walking the
	// name flushes /var/lib and never touches /mnt/vol -- which is where the entries
	// that actually name the new directory live. Both are walked: the lexical path
	// commits the link itself, and the resolved path commits what it points at.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("durablefile: cannot resolve %s: %w", dir, err)
	}

	// ONE FLUSH PER DIRECTORY, and the deduplication is not tidiness. Opening a
	// lexical parent FOLLOWS the symlink, so the two walks reach the same physical
	// directories below the link — and a redundant second fsync that fails would
	// reject a pull whose directory the first fsync had already committed. That is a
	// refusal of correct state, which is the failure ADR-005 names.
	seen := map[string]bool{}

	for _, path := range []string{dir, resolved} {
		if err := i.syncAncestors(path, seen); err != nil {
			return err
		}
	}

	return nil
}

// syncAncestors flushes every directory from path's parent up to the root, skipping
// any it has already flushed for this call.
//
// UPWARD, because each flush commits the entries ONE level holds: committing the
// name of `path` means flushing its PARENT, and committing the parent's name means
// flushing the one above it. A directory is no more durable than the shallowest
// entry nothing flushed.
//
// KEYED ON THE RESOLVED NAME, because that is what identifies the directory the
// kernel would actually flush; two spellings of one directory are one fsync.
func (i Installer) syncAncestors(path string, seen map[string]bool) error {
	for at := path; ; {
		parent := filepath.Dir(at)
		if parent == at {
			return nil
		}

		key := parent
		if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
			key = resolvedParent
		}

		if !seen[key] {
			seen[key] = true

			if err := i.SyncDirectory(parent); err != nil {
				return err
			}
		}

		at = parent
	}
}

// SetModeOn and SyncFileHandle are the two file steps, exposed for a caller that is
// REPAIRING an artifact rather than installing one.
//
// THE SAME SEAMS, SO THE REPAIR IS THE SAME OPERATION. A caller that reached for
// f.Chmod and f.Sync directly would be a second implementation of the ordering this
// package exists to hold, and a test could not fail it where it fails Install.
func (i Installer) SetModeOn(f *os.File, mode fs.FileMode) error { return i.setMode()(f, mode) }

// SyncFileHandle flushes a file's contents and metadata.
func (i Installer) SyncFileHandle(f *os.File) error { return i.syncFile()(f) }

func (i Installer) setMode() func(*os.File, fs.FileMode) error {
	if i.SetMode != nil {
		return i.SetMode
	}

	return func(f *os.File, mode fs.FileMode) error { return f.Chmod(mode) }
}

func (i Installer) syncFile() func(*os.File) error {
	if i.SyncFile != nil {
		return i.SyncFile
	}

	return func(f *os.File) error { return f.Sync() }
}

func (i Installer) rename() func(string, string) error {
	if i.Rename != nil {
		return i.Rename
	}

	return os.Rename
}

func (i Installer) syncDir() func(string) error {
	if i.SyncDir != nil {
		return i.SyncDir
	}

	return syncDirectory
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("durablefile: could not open %s to flush it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("durablefile: could not flush %s, so the file in it is not durable "+
			"yet: %w", dir, err)
	}

	return nil
}
