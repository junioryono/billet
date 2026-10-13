package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/hostupgrade"
)

// keepRecoveries is how many decided transactions' recovery directories the
// upgrade root keeps, the one just decided among them.
const keepRecoveries = 3

// recoveryTempName is the name stageClaim's MkdirTemp gives a transaction's
// recovery directory. The role's transactions use recoveryNameGrammar and are
// the role's to keep or remove.
var recoveryTempName = regexp.MustCompile(`^upgrade-\d+$`)

// pruneRecoveries removes the recovery directories of decided transactions
// beyond the newest keepRecoveries, and answers the names it removed. The
// caller holds the transaction lock, and root is the descriptor that lock
// validated.
//
// A FLEET THAT UPDATES ITSELF CANNOT LEAVE RETENTION TO A PERSON. Every
// committed upgrade keeps its ledger snapshot and two binaries, about 380 MB on
// a controller, and 45 of them held 6.7 GB of a 15 GB root that had reached 98%
// (2026-10-13).
//
// ONLY A DIRECTORY WHOSE OWN JOURNAL RECORDS A DECISION IS REMOVED. The one the
// claim names (by identity, not by the pointer's text), one with no journal,
// one whose journal cannot be read, is not trusted or names a step this build
// does not know, and anything not shaped like a directory stageClaim made are
// kept, because could-not-tell is not "finished". Directories are ordered by
// their journals' own last update, and the journal is the last thing a removal
// unlinks, so a removal cut short is judged again and finished by the next pass.
func pruneRecoveries(root *os.File) ([]string, error) {
	active, claimed, err := claimedDir(root)
	if err != nil {
		return nil, err
	}

	entries, err := listDir(root)
	if err != nil {
		return nil, err
	}

	type decided struct {
		name   string
		at     time.Time
		judged os.FileInfo
	}

	var found []decided

	for _, e := range entries {
		if !recoveryTempName.MatchString(e.Name()) {
			continue
		}

		var journal *hostupgrade.Journal

		judged, ok := judgedDirAt(root, e.Name(), func() bool {
			j, err := readJournalUnder(root, filepath.Join(upgradeRoot, e.Name()))
			journal = j

			return err == nil && j.Step.Decided()
		})
		if !ok || (claimed && os.SameFile(active, judged)) {
			continue
		}

		at, err := time.Parse(time.RFC3339Nano, journal.UpdatedAt)
		if err != nil {
			continue
		}

		found = append(found, decided{name: e.Name(), at: at, judged: judged})
	}

	if len(found) <= keepRecoveries {
		return nil, nil
	}

	sort.Slice(found, func(i, j int) bool { return found[i].at.After(found[j].at) })

	var (
		removed []string
		errs    []error
	)

	for _, d := range found[keepRecoveries:] {
		if err := removeTreeAt(root, d.name, d.judged, hostupgrade.JournalName); err != nil {
			errs = append(errs, err)

			continue
		}

		removed = append(removed, d.name)
	}

	return removed, errors.Join(errs...)
}

// claimedDir is the identity of what the claim names, following the pointer
// the way the kernel does, so a relative or unclean target is still
// recognised. It answers false when there is no claim, and an error when it
// cannot tell, which stops the prune rather than leaving the claim unprotected.
func claimedDir(root *os.File) (os.FileInfo, bool, error) {
	fd, err := unix.Openat(int(root.Fd()), activePointer, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No claim, or a pointer to nothing, which names no directory to keep.
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("open what the upgrade claim names before removing old recovery "+
			"directories: %w", err)
	}

	f := os.NewFile(uintptr(fd), activePath())
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("examine what the upgrade claim names before removing old recovery "+
			"directories: %w", err)
	}

	return info, true, nil
}

// judgedDirAt answers the identity of the directory name under dir, when the
// same directory is there before and after judge, and judge accepted it.
func judgedDirAt(dir *os.File, name string, judge func() bool) (os.FileInfo, bool) {
	before, err := dirInfoAt(dir, name)
	if err != nil || !judge() {
		return nil, false
	}

	after, err := dirInfoAt(dir, name)
	if err != nil || !os.SameFile(before, after) {
		return nil, false
	}

	return before, true
}

// listDir lists dir through a descriptor of its own: a listing continues from
// where the last one on the same descriptor stopped, so a second listing of a
// descriptor already read answers nothing.
func listDir(dir *os.File) ([]os.DirEntry, error) {
	f, err := openDirAt(int(dir.Fd()), ".", dir.Name())
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir.Name(), err)
	}

	return entries, nil
}

// openDirAt opens a directory relative to dir, never through a link.
func openDirAt(dir int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return os.NewFile(uintptr(fd), path), nil
}

// dirInfoAt is the identity of the directory name under dir.
func dirInfoAt(dir *os.File, name string) (os.FileInfo, error) {
	f, err := openDirAt(int(dir.Fd()), name, filepath.Join(dir.Name(), name))
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	return f.Stat()
}

// maxRemoveDepth bounds how deep a removal descends. A recovery directory is
// three levels deep; a deeper tree is not one billet wrote.
const maxRemoveDepth = 8

// removeTreeAt removes the directory name under dir and everything beneath it,
// through a descriptor that is the directory judged. proof, the entry that made
// it removable, is unlinked after everything else, so a removal cut short still
// carries what the next pass judges it by. Every step is relative to a
// descriptor and none follows a link, so a link inside the tree is removed and
// what it names is not.
func removeTreeAt(dir *os.File, name string, judged os.FileInfo, proof string) error {
	path := filepath.Join(dir.Name(), name)

	if err := requireSameMount(dir, name, path); err != nil {
		return err
	}

	top, err := openDirAt(int(dir.Fd()), name, path)
	if err != nil {
		return err
	}

	defer func() { _ = top.Close() }()

	if info, err := top.Stat(); err != nil || !os.SameFile(info, judged) {
		return fmt.Errorf("%s is no longer the directory that was judged; it is kept", path)
	}

	if err := removeContents(top, path, proof, 0); err != nil {
		return err
	}

	// FLUSHED BEFORE THE PROOF GOES: the order of the unlinks is not the order
	// they reach the disk, and a proof whose removal outlived the others' in a
	// power cut would leave contents nothing can prove removable.
	if err := syncDirFD(top); err != nil {
		return err
	}

	if err := removeEntryAt(top, proof, filepath.Join(path, proof), 0); err != nil {
		return err
	}

	if err := unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return syncDirFD(dir)
}

// removeContents removes everything in the open directory dir but keep.
func removeContents(dir *os.File, path, keep string, depth int) error {
	if depth >= maxRemoveDepth {
		return fmt.Errorf("%s is nested more than %d directories deep, which no directory billet "+
			"removes is; it is kept", path, maxRemoveDepth)
	}

	entries, err := listDir(dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.Name() == keep {
			continue
		}

		if err := removeEntryAt(dir, e.Name(), filepath.Join(path, e.Name()), depth); err != nil {
			return err
		}
	}

	return nil
}

func removeEntryAt(parent *os.File, name, path string, depth int) error {
	var st unix.Stat_t

	switch err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("examine %s: %w", path, err)
	}

	if modeOf(&st)&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}

		return nil
	}

	if err := requireSameMount(parent, name, path); err != nil {
		return err
	}

	sub, err := openDirAt(int(parent.Fd()), name, path)
	if err != nil {
		return err
	}

	defer func() { _ = sub.Close() }()

	if err := removeContents(sub, path, "", depth+1); err != nil {
		return err
	}

	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

// requireSameMount refuses a directory under parent that is another mount: on
// another device, or the root of a mount on the same one. O_NOFOLLOW does not
// stop a removal descending into a mount, and an rmdir refuses it only after
// everything inside is gone.
func requireSameMount(parent *os.File, name, path string) error {
	var above, below unix.Stat_t

	if err := unix.Fstat(int(parent.Fd()), &above); err != nil {
		return fmt.Errorf("examine %s: %w", parent.Name(), err)
	}

	if err := unix.Fstatat(int(parent.Fd()), name, &below, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("examine %s: %w", path, err)
	}

	mount, err := isMountRootAt(int(parent.Fd()), name)

	switch {
	case err != nil:
		return fmt.Errorf("could not tell whether %s is a mount, so it is kept: %w", path, err)
	case mount || above.Dev != below.Dev:
		return fmt.Errorf("%s is a mount, which billet never removes into; it is kept", path)
	}

	return nil
}
