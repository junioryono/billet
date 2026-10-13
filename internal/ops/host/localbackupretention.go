package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/deployarchive"
)

// keepLocalArchives is how many archives the packaged backup directory keeps
// on the host, the one just written among them.
const keepLocalArchives = 7

// pruneLocalArchives removes archives in the packaged backup directory beyond
// the newest keepLocalArchives, once this run's upload has finished. It runs
// only when current is a child of localBackupDir: a directory an operator
// named with --out is theirs.
//
// THE BUCKET IS THE PROOF. The daily timer writes about 170 MB a day on a
// controller and nothing removed any of it, and 61 archives held most of a
// 15 GB root at 97% (2026-10-13). An archive is removed only when the bucket
// holds its own manifest, byte for byte, under the name that manifest gives
// it, which is the name its upload used: the manifest is uploaded last and
// carries every entry's digest, so the bucket holding it is the bucket holding
// the whole archive. One without a readable manifest, or one the bucket does
// not hold, is kept whatever its age. ADVISORY: a failure is said and decides
// nothing.
func pruneLocalArchives(ctx context.Context, env cli.Env, store archiveStore, deployment, current string) {
	if filepath.Dir(current) != localBackupDir {
		return
	}

	removed, err := pruneArchivesIn(ctx, store, deployment, localBackupDir)
	if len(removed) > 0 {
		fmt.Fprintf(env.Stdout, "\nprune    removed %d older local archives that the bucket holds; the newest %d\n",
			len(removed), keepLocalArchives)
		fmt.Fprintf(env.Stdout, "         are kept in %s\n", localBackupDir)
	}

	if err != nil {
		fmt.Fprintf(env.Stdout, "\nwarn     older local archives were not all pruned: %v\n", err)
	}
}

// pruneArchivesIn is pruneLocalArchives' work on one directory, answering the
// names it removed.
//
// WHAT IS REMOVED IS WHAT WAS JUDGED: the directory is the one opened at
// parent, and an archive is removed only through a descriptor that is the same
// directory before and after its manifest was read. The manifest is the last
// thing a removal unlinks, so a removal cut short is judged again by the next
// pass, from the manifest alone, and finished.
func pruneArchivesIn(ctx context.Context, store archiveStore, deployment, parent string) ([]string, error) {
	complete, err := store.List(ctx, deployment)
	if err != nil {
		return nil, fmt.Errorf("list the bucket, so nothing local is removed: %w", err)
	}

	listed := make(map[string]bool, len(complete))
	for _, a := range complete {
		listed[a.Name] = true
	}

	fd, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", parent, err)
	}

	dir := os.NewFile(uintptr(fd), parent)
	defer func() { _ = dir.Close() }()

	opened, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("examine %s: %w", parent, err)
	}

	if info, err := os.Lstat(parent); err != nil || !os.SameFile(opened, info) {
		return nil, fmt.Errorf("%s changed while it was being opened, so nothing local is removed", parent)
	}

	entries, err := listDir(dir)
	if err != nil {
		return nil, err
	}

	type local struct {
		name   string
		id     deployarchive.Identity
		judged os.FileInfo
	}

	var archives []local

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		var id deployarchive.Identity

		judged, ok := judgedDirAt(dir, e.Name(), func() bool {
			read, err := deployarchive.ReadIdentity(filepath.Join(parent, e.Name()))
			id = read

			return err == nil
		})
		if !ok {
			continue
		}

		archives = append(archives, local{name: e.Name(), id: id, judged: judged})
	}

	if len(archives) <= keepLocalArchives {
		return nil, nil
	}

	sort.Slice(archives, func(i, j int) bool { return archives[i].id.CreatedAt.After(archives[j].id.CreatedAt) })

	var (
		removed []string
		errs    []error
	)

	for _, l := range archives[keepLocalArchives:] {
		if !listed[l.id.Name] {
			continue
		}

		// THE NAME IS EIGHT HEX DIGITS OF THE MANIFEST'S DIGEST, which a listing
		// can share with another archive; the bucket's manifest bytes cannot.
		held, err := l.id.HeldBy(ctx, store, store.Prefix(deployment)+l.id.Name)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		if !held {
			continue
		}

		// THE MANIFEST PROVES THE BUCKET HOLDS WHAT IT DECLARES, NOT WHAT ELSE IS
		// HERE: a file it does not declare, or one changed since, is local data
		// nothing else holds.
		if err := l.id.VerifyRemainder(filepath.Join(parent, l.name)); err != nil {
			errs = append(errs, fmt.Errorf("%w; it is kept", err))

			continue
		}

		if err := removeTreeAt(dir, l.name, l.judged, deployarchive.EntryManifest); err != nil {
			errs = append(errs, err)

			continue
		}

		removed = append(removed, l.name)
	}

	return removed, errors.Join(errs...)
}
