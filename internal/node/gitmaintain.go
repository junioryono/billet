package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// gitMaxPacks is how many packs a mirror may hold before a geometric
	// roll-up merges the small ones: each refresh leaves one, and pack-objects
	// pays for every pack a fetch it serves has to search.
	gitMaxPacks = 8
	// gitFullRepackEvery is how often a mirror in use is rewritten as one pack
	// with a bitmap, with what no ref reaches set aside in a cruft pack.
	gitFullRepackEvery = 24 * time.Hour
	// gitPruneEvery is how often a mirror in use is pruned of what no ref has
	// reached for gitPruneAfter.
	gitPruneEvery = 7 * 24 * time.Hour
	// gitPruneAfter is how long an object no ref reaches is kept, counted from
	// the first pass that set it aside in the cruft pack (freshenPacks).
	gitPruneAfter = 14 * 24 * time.Hour
	// gitRepackedStamp and gitPrunedStamp name the files in a mirror whose
	// mtimes are its last full repack and its last pruning.
	gitRepackedStamp = "billet-repacked"
	gitPrunedStamp   = "billet-pruned"
)

// gitMaintainConfig is how a mirror is repacked: no automatic gc or
// maintenance, and two threads, so a repack never takes the node's cores from
// its jobs. It carries no header: maintenance fetches nothing.
var gitMaintainConfig = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "pack.threads=2"}

// maintenanceDue is the maintenance a mirror needs now, or "" for none:
// pruning once a week, which repacks it too, so a daily repack never stands in
// its way; a full repack once a day; or a geometric roll-up once it holds
// gitMaxPacks packs.
//
// A STAMP THAT CANNOT BE READ IS NO PASS THAT REWRITES OR REMOVES: pruning is
// the half that cannot be undone, and keeping objects is the safe answer;
// either stamp unknown, only a roll-up runs. A mirror never pruned is dated
// from now, so a new one is not pruned on its first day.
func (s *CacheService) maintenanceDue(path string) string {
	pruned := filepath.Join(path, gitPrunedStamp)
	if _, err := os.Stat(pruned); errors.Is(err, fs.ErrNotExist) {
		if err := s.stamp(pruned); err != nil {
			return ""
		}
	}
	repackDue, repackErr := s.stampDue(path, gitRepackedStamp, s.git.fullRepackEvery)
	pruneDue, pruneErr := s.stampDue(path, gitPrunedStamp, gitPruneEvery)
	switch {
	case repackErr == nil && pruneErr == nil && pruneDue:
		return gitMaintainPrune
	case repackErr == nil && pruneErr == nil && repackDue:
		return gitMaintainFull
	}
	packs, err := filepath.Glob(filepath.Join(path, "objects", "pack", "*.pack"))
	if err == nil && len(packs) >= s.git.maxPacks {
		return gitMaintainGeometric
	}

	return ""
}

// stampDue is whether the stamp name in path is every old, or missing.
func (s *CacheService) stampDue(path, name string, every time.Duration) (bool, error) {
	info, err := os.Stat(filepath.Join(path, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, err
	}

	return !s.now().Before(info.ModTime().Add(every)), nil
}

// stamp sets the stamp at path to now.
func (s *CacheService) stamp(path string) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	now := s.now()

	return os.Chtimes(path, now, now)
}

// maintain runs maintenance of kind on the mirror of request's repository,
// after a refresh and in its place: the refresh slot stays held, so no fetch
// runs beside a repack.
//
// A REPACK THAT REMOVES NOTHING SHARES THE MIRROR WITH WHAT IT SERVES: it
// deletes the packs it rewrote under upload-packs reading them, which git
// survives (103 full fetches beside 90 repacks, full and geometric, none
// failed; git 2.43, 2026-10-22), and every object is still there afterwards.
// The mirror is held for reading throughout, as a refresh holds it, so no claim
// renames it aside mid-pass.
//
// PRUNING HAS IT ALONE: an upload-pack that found an object before the prune
// and reads it after has already started its answer, which then fails. New
// requests go to GitHub from the moment pruning is wanted, the ones being
// served are let finish, and a mirror they do not free soon is pruned later.
//
// A PRUNE THAT CANNOT HAVE THE MIRROR LEAVES IT ROLLED UP INSTEAD, so a busy
// mirror still gets the passes that need only share it.
func (s *CacheService) maintain(
	ctx context.Context, request gitRequest, repository int64, mirror *gitMirror, kind string,
) {
	if !s.maintainPass(ctx, request, repository, mirror, kind) || kind != gitMaintainPrune {
		return
	}
	packs, err := filepath.Glob(filepath.Join(s.git.mirrorPath(request.key()), "objects", "pack", "*.pack"))
	if err == nil && len(packs) >= s.git.maxPacks {
		s.maintainPass(ctx, request, repository, mirror, gitMaintainGeometric)
	}
}

// maintainPass runs one pass of kind, holding the mirror as kind needs, counts
// and logs how it ended, and says whether it could not have the mirror.
func (s *CacheService) maintainPass(
	ctx context.Context, request gitRequest, repository int64, mirror *gitMirror, kind string,
) bool {
	path := s.git.mirrorPath(request.key())
	started := s.now()
	var err error
	if kind == gitMaintainPrune {
		mirror.pruning.Store(true)
		if lockWithin(ctx, &mirror.use, casReleaseWait) {
			err = s.maintainHeld(ctx, path, repository, kind)
			mirror.use.Unlock()
		} else {
			err = errGitMaintainBusy
		}
		mirror.pruning.Store(false)
	} else {
		waitCtx, cancel := context.WithTimeout(ctx, gitFetchWait)
		held := rlockWithin(waitCtx, &mirror.use)
		cancel()
		if held {
			err = s.maintainHeld(ctx, path, repository, kind)
			mirror.use.RUnlock()
		} else {
			err = errGitMaintainBusy
		}
	}
	if err != nil && errors.Is(context.Cause(ctx), errGitClosed) {
		err = errors.Join(errGitClosed, err)
	}
	took := s.now().Sub(started)
	s.git.observe().GitMaintenance(kind, gitMaintenanceOutcome(err), took)
	took = took.Round(time.Millisecond)
	if err != nil {
		s.log.Info("could not maintain a git mirror; it is served as it is",
			"repository", request.name(), "kind", kind, "took", took, "error", err)

		return errors.Is(err, errGitMaintainBusy)
	}
	s.log.Info("maintained a git mirror", "repository", request.name(), "kind", kind, "took", took)

	return false
}

// errGitMaintainBusy says a maintenance pass could not have the mirror.
var errGitMaintainBusy = errors.New("the mirror was in use")

// maintainHeld is maintain's work, with the mirror held.
func (s *CacheService) maintainHeld(ctx context.Context, path string, repository int64, kind string) error {
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != repository {
		return errors.Join(errors.New("the mirror is another repository's or unverified"), err)
	}
	// ROOM FOR TWICE THE MIRROR, AND WATCHED WHILE IT RUNS: a repack writes its
	// packs beside the ones it replaces before it deletes them, and setting the
	// unreachable objects apart from the rest can lose the deltas between them
	// (a 4.2 MB pack became 8.4 MB of replacements, git 2.51, 2026-10-22), so the
	// estimate is checked before and the filesystem's fill throughout.
	size, err := directorySize(path)
	if err != nil {
		return err
	}
	if room, err := roomFor(s.git.root, s.git.fullFraction, 2*size); err != nil {
		return err
	} else if !room {
		return errGitDiskFull
	}
	git := func(args ...string) error {
		return watchFill(ctx, s.git.root, s.git.fullFraction, func(ctx context.Context) error {
			_, err := s.runGit(ctx, path, nil, append(slices.Clone(gitMaintainConfig), args...)...)

			return err
		})
	}
	switch kind {
	case gitMaintainGeometric:
		return git("repack", "-q", "--geometric=2", "-d")
	case gitMaintainFull:
		// NOTHING EXPIRES HERE: what no ref reaches goes into the cruft pack,
		// loose objects an older fetch left included.
		if err := s.freshenPacks(path); err != nil {
			return err
		}
		if err := git("repack", "-q", "--cruft", "--cruft-expiration=never", "-d", "--write-bitmap-index"); err != nil {
			return err
		}
		if err := git("commit-graph", "write", "--reachable", "--split=replace"); err != nil {
			return err
		}

		return s.stamp(filepath.Join(path, gitRepackedStamp))
	default:
		// AN ABSOLUTE EXPIRY, from the node's clock, never git's own reading of a
		// relative date. Every loose object is freshened first, so none is past
		// it, and the repack packs them all.
		expiry := "@" + strconv.FormatInt(s.now().Add(-gitPruneAfter).Unix(), 10)
		if err := s.freshenPacks(path); err != nil {
			return err
		}
		if err := git("repack", "-q", "--cruft", "--cruft-expiration="+expiry, "-d", "--write-bitmap-index"); err != nil {
			return err
		}
		if err := git("commit-graph", "write", "--reachable", "--split=replace"); err != nil {
			return err
		}
		if err := s.stamp(filepath.Join(path, gitRepackedStamp)); err != nil {
			return err
		}

		return s.stamp(filepath.Join(path, gitPrunedStamp))
	}
}

// freshenPacks dates every pack of the mirror at path that is not a cruft pack,
// and every loose object, now, before a cruft repack.
//
// AN OBJECT IS DATED BY THE FIRST PASS THAT SETS IT ASIDE: git gives an object
// leaving an ordinary pack for the cruft pack that pack's mtime, a loose one its
// file's, and keeps the date a cruft pack recorded for one already in it
// (measured, git 2.43, 2026-10-22). Undated, an object that stopped being
// reachable since a pack or a file written a month ago would carry the month
// and be pruned the moment it went.
//
// EVERY DIRECTORY IS READ OR THE PASS DOES NOT RUN: a pack a failed listing
// skipped would keep its old date into the prune.
func (s *CacheService) freshenPacks(path string) error {
	now := s.now()
	freshen := func(file string) error {
		if err := os.Chtimes(file, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		return nil
	}
	objects := filepath.Join(path, "objects")
	fanout, err := os.ReadDir(objects)
	if err != nil {
		return err
	}
	for _, dir := range fanout {
		if !dir.IsDir() || len(dir.Name()) != 2 || strings.Trim(dir.Name(), "0123456789abcdef") != "" {
			continue
		}
		loose, err := os.ReadDir(filepath.Join(objects, dir.Name()))
		if err != nil {
			return err
		}
		for _, object := range loose {
			if err := freshen(filepath.Join(objects, dir.Name(), object.Name())); err != nil {
				return err
			}
		}
	}
	packDir := filepath.Join(objects, "pack")
	packs, err := os.ReadDir(packDir)
	if err != nil {
		return err
	}
	cruft := make(map[string]bool)
	for _, file := range packs {
		if base, ok := strings.CutSuffix(file.Name(), ".mtimes"); ok {
			cruft[base] = true
		}
	}
	for _, file := range packs {
		if base, ok := strings.CutSuffix(file.Name(), ".pack"); ok && !cruft[base] {
			if err := freshen(filepath.Join(packDir, file.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

// watchFill runs run, ending it with errGitDiskFull once root's filesystem
// passes fraction full, and with the reason once its fill cannot be read: the
// estimate before it started is no reason to go on blind. run's process group
// goes with its context.
func watchFill(ctx context.Context, root string, fraction float64, run func(context.Context) error) error {
	watched, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(gitWatchEvery)
		defer ticker.Stop()
		for {
			full, err := filledAbove(root, fraction)
			switch {
			case err != nil:
				stop(fmt.Errorf("could not read the git cache's fill: %w", err))

				return
			case full:
				stop(errGitDiskFull)

				return
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	err := run(watched)
	if cause := context.Cause(watched); cause != nil && !errors.Is(cause, context.Canceled) {
		return errors.Join(cause, err)
	}

	return err
}

// roomFor reports whether root's filesystem stays within fraction full once
// extra more bytes are written to it.
func roomFor(root string, fraction float64, extra int64) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return false, err
	}
	total := float64(stat.Blocks) * float64(stat.Bsize)
	if total == 0 {
		return true, nil
	}
	used := (float64(stat.Blocks) - float64(stat.Bavail)) * float64(stat.Bsize)

	return (used+float64(extra))/total <= fraction, nil
}
