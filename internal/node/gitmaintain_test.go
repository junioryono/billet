package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// REFRESHES LEAVE A MIRROR IN FEW PACKS: the first refresh is followed by a
// full repack with a bitmap, and once refreshes have left the mirror its
// most packs a geometric roll-up merges them, each through the real refresh.
func TestRefreshesKeepAMirrorInFewPacks(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	service.git.maxPacks = 3
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	path := service.git.mirrorPath(key)
	awaitIdleMirror(t, service, key)
	if maintained := observer.maintained(); !slices.Equal(maintained, []string{"full ok"}) {
		t.Fatalf("the new mirror's maintenance was %v, want one full repack", maintained)
	}
	if bitmaps := packFiles(t, path, "*.bitmap"); len(bitmaps) != 1 {
		t.Fatalf("the full repack left %d bitmaps, want 1", len(bitmaps))
	}

	for i := range 6 {
		upstream.commitTo(t, "main", map[string]string{"f": strings.Repeat("x", i+1) + "\n"})
		advertise(t, node, token, "api")
		awaitIdleMirror(t, service, key)
	}
	if maintained := observer.maintained(); !slices.Contains(maintained, "geometric ok") {
		t.Fatalf("six refreshes past the most packs were never rolled up: %v", maintained)
	}
	if packs := packFiles(t, path, "*.pack"); len(packs) > service.git.maxPacks {
		t.Errorf("the mirror holds %d packs after its roll-ups, more than %d", len(packs), service.git.maxPacks)
	}
}

// AN OBJECT NO REF REACHES IS KEPT FOR THE RETENTION WINDOW, AND THEN PRUNED:
// the daily repack removes nothing, and within the window a fetch of it by id
// is served from the mirror, as GitHub serves a superseded merge commit; past
// it a prune removes it, and a loose object an older fetch left, and the fetch
// goes to GitHub, which still has it.
func TestAnUnreachableObjectIsKeptForItsRetentionThenPruned(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	git := gitClient(t, node, token, githubBasic)
	if output, err := git("clone", "-q", "https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	path := service.git.mirrorPath(key)
	awaitIdleMirror(t, service, key)
	orphan := upstream.commitTo(t, "superseded", map[string]string{"s": "1\n"})
	advertise(t, node, token, "api")
	awaitIdleMirror(t, service, key)
	runIn(t, upstream.repo, "update-ref", "-d", "refs/heads/superseded")
	advertise(t, node, token, "api")
	awaitIdleMirror(t, service, key)
	if !hasObject(t, path, orphan) {
		t.Fatal("the mirror never held the commit, so what follows proves nothing")
	}
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api", auth: gitAuth(githubBasic)}
	fetchByID := func(name string) []string {
		t.Helper()

		observer := &recordingGitObserver{}
		service.SetGitObserver(observer)
		if output, err := git("init", "-q", name); err != nil {
			t.Fatalf("init: %v\n%s", err, output)
		}
		if output, err := git("-C", name, "fetch", "-q", "https://github.com/acme/api.git", orphan); err != nil {
			t.Fatalf("a fetch of the unreachable commit by id: %v\n%s", err, output)
		}

		return gitWithin(t, "the fetch's counted requests", func() []string {
			for t.Context().Err() == nil {
				if requests, _, _ := observer.seen(); len(requests) >= 3 {
					return requests
				}
				time.Sleep(10 * time.Millisecond)
			}

			return nil
		})
	}

	maintain := func(kind string) {
		t.Helper()

		observer := &recordingGitObserver{}
		service.SetGitObserver(observer)
		service.maintain(t.Context(), request, upstream.repositoryID("api"), service.git.mirror(key), kind)
		if want := []string{kind + " ok"}; !slices.Equal(observer.maintained(), want) {
			t.Fatalf("the %s pass was counted as %v, want %v", kind, observer.maintained(), want)
		}
	}

	// THE DAILY REPACK REMOVES NOTHING, even what is past the window: the first
	// sets the commit aside, dated now, and one a window later keeps it.
	maintain(gitMaintainFull)
	later := time.Now().Add(gitPruneAfter + 24*time.Hour)
	service.now = func() time.Time { return later }
	maintain(gitMaintainFull)
	service.now = time.Now
	if !hasObject(t, path, orphan) {
		t.Fatal("the daily repack removed an unreachable commit")
	}
	if requests := fetchByID("within"); !slices.Contains(requests, "mirror served 2xx") ||
		slices.ContainsFunc(requests, func(r string) bool { return strings.HasPrefix(r, "github ") }) {
		t.Errorf("a fetch by id within the retention window was not served from the mirror: %v", requests)
	}
	// A LOOSE OBJECT AN OLDER FETCH LEFT a month ago, no older to the prune that
	// first finds it than anything else it sets aside.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.WriteFile(loose, []byte("left by an older fetch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := runIn(t, path, "hash-object", "-w", loose)
	old := time.Now().Add(-2 * gitPruneAfter)
	if err := os.Chtimes(filepath.Join(path, "objects", blob[:2], blob[2:]), old, old); err != nil {
		t.Fatal(err)
	}
	weekAgo := time.Now().Add(-gitPruneEvery)
	if err := os.Chtimes(filepath.Join(path, gitPrunedStamp), weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	if due := service.maintenanceDue(path); due != gitMaintainPrune {
		t.Fatalf("a mirror pruned a week ago is due %q, want prune", due)
	}
	maintain(gitMaintainPrune)
	if !hasObject(t, path, orphan) {
		t.Fatal("pruning within the retention window removed an unreachable commit")
	}
	if !hasObject(t, path, blob) {
		t.Fatal("the prune that first found a month-old loose object removed it for the age of its file")
	}
	if due := service.maintenanceDue(path); due != "" {
		t.Errorf("a mirror just pruned is due %q, want nothing", due)
	}

	service.now = func() time.Time { return later }
	maintain(gitMaintainPrune)
	if hasObject(t, path, orphan) {
		t.Fatal("pruning past the retention window kept the unreachable commit")
	}
	if hasObject(t, path, blob) {
		t.Fatal("pruning past the retention window kept a loose object an older fetch left")
	}
	if main := runIn(t, upstream.repo, "rev-parse", "main"); !hasObject(t, path, main) {
		t.Fatal("pruning removed the commit main names, so it proves nothing about what it kept")
	}
	if requests := fetchByID("past"); !slices.Contains(requests, "github mirror_lacks_object 2xx") {
		t.Errorf("a fetch by id of a pruned commit was not sent to GitHub: %v", requests)
	}
}

// WHILE A MIRROR IS PRUNED, EVERY NEW REQUEST IS GITHUB'S, advertisement and
// fetch alike, rather than waiting on a hold the prune is taking alone; and a
// prune that cannot have the mirror while a fetch is served from it is
// counted busy and left to a later refresh.
func TestRequestsWhileAMirrorIsPrunedAreGitHubs(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	awaitIdleMirror(t, service, key)
	mirror := service.git.mirror(key)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)

	mirror.pruning.Store(true)
	advertise(t, node, token, "api")
	negotiate(t, node, token, githubBasic, runIn(t, upstream.repo, "rev-parse", "main"))
	mirror.pruning.Store(false)
	requests := gitWithin(t, "the counted requests", func() []string {
		for t.Context().Err() == nil {
			if requests, _, _ := observer.seen(); len(requests) >= 2 {
				return requests
			}
			time.Sleep(10 * time.Millisecond)
		}

		return nil
	})
	if want := []string{"github mirror_maintaining 2xx", "github mirror_maintaining 2xx"}; !slices.Equal(requests, want) {
		t.Fatalf("the requests during a prune were counted as %v, want %v", requests, want)
	}

	// A FETCH BEING SERVED, which the prune waits for, and a request arriving
	// meanwhile, which goes to GitHub rather than waiting behind it; the prune
	// that gives up leaves the mirror rolled up, which needs only to share it.
	service.git.maxPacks = 1
	mirror.use.RLock()
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api", auth: gitAuth(githubBasic)}
	pruned := make(chan struct{})
	go func() {
		defer close(pruned)
		service.maintain(t.Context(), request, upstream.repositoryID("api"), mirror, gitMaintainPrune)
	}()
	gitWithin(t, "the prune asking for the mirror alone", func() struct{} {
		for !mirror.pruning.Load() && t.Context().Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}

		return struct{}{}
	})
	waiting := &recordingGitObserver{}
	service.SetGitObserver(waiting)
	advertise(t, node, token, "api")
	gitWithin(t, "the prune giving up", func() struct{} {
		<-pruned

		return struct{}{}
	})
	mirror.use.RUnlock()
	if requests, _, _ := waiting.seen(); !slices.Equal(requests, []string{"github mirror_maintaining 2xx"}) {
		t.Errorf("a request while a prune waited for the mirror was counted as %v, want it GitHub's", requests)
	}
	if want := []string{"prune busy", "geometric ok"}; !slices.Equal(waiting.maintained(), want) {
		t.Errorf("a prune that could not have the mirror was counted as %v, want %v", waiting.maintained(), want)
	}
	if mirror.pruning.Load() {
		t.Error("a prune that gave up left new requests going to GitHub")
	}
}

// A REQUEST WHOSE REFS MOVED WHILE THE MIRROR IS REPACKED WAITS FOR THE REPACK,
// as it waits for a refresh: the repack holds the refresh slot, so no fetch
// runs beside it. One that ends within the advertisement's wait is followed by
// a refresh and served from the mirror; one that does not leaves the request to
// GitHub, counted under why.
func TestARequestWhileTheMirrorIsRepackedWaitsForIt(t *testing.T) {
	t.Parallel()

	for name, ends := range map[string]bool{"within the wait": true, "past the wait": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			upstream := newGitUpstream(t)
			service, node, token := gitNode(t, upstream)
			service.git.refreshWait = 2 * time.Second
			if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
				"https://github.com/acme/api.git", "c"); err != nil {
				t.Fatalf("clone: %v\n%s", err, output)
			}
			key := "untrusted/acme/api"
			awaitIdleMirror(t, service, key)
			mirror := service.git.mirror(key)
			repacking := &gitRefresh{done: make(chan struct{}), idle: make(chan struct{}), maintaining: true}
			close(repacking.done)
			mirror.mu.Lock()
			mirror.refreshing = repacking
			mirror.mu.Unlock()
			free := func() {
				mirror.mu.Lock()
				mirror.refreshing = nil
				mirror.mu.Unlock()
				close(repacking.idle)
			}
			observer := &recordingGitObserver{}
			service.SetGitObserver(observer)
			upstream.commitTo(t, "main", map[string]string{"m": "1\n"})

			// THE REPACK ENDS ONLY ONCE THE REQUEST WAITS FOR IT, and no refresh
			// starts until it has.
			waiting := make(chan struct{}, 1)
			service.git.beforeRepackWait = func() {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
			fetches := upstream.mirrorFetches.Load()
			advertised := make(chan error, 1)
			go func() { advertised <- advertiseErr(t.Context(), node, token, "api") }()
			gitWithin(t, "the request waiting for the repack", func() struct{} {
				<-waiting

				return struct{}{}
			})
			if upstream.mirrorFetches.Load() != fetches {
				t.Fatal("a refresh fetched beside the repack")
			}
			if ends {
				free()
			} else {
				t.Cleanup(free)
			}
			if err := gitWithin(t, "the advertisement", func() error { return <-advertised }); err != nil {
				t.Fatal(err)
			}
			requests := gitWithin(t, "the counted advertisement", func() []string {
				for t.Context().Err() == nil {
					if requests, _, _ := observer.seen(); len(requests) > 0 {
						return requests
					}
					time.Sleep(10 * time.Millisecond)
				}

				return nil
			})
			want := []string{"github mirror_maintaining 2xx"}
			if ends {
				want = []string{"mirror served 2xx"}
			}
			if !slices.Equal(requests, want) {
				t.Fatalf("the advertisement was counted as %v, want %v", requests, want)
			}
		})
	}
}

// A REFRESH SAYS WHEN ITS MIRROR IS TO BE REPACKED, so a waiter whose refs are
// still behind knows the slot is held for the repack: a new mirror is, and one
// repacked since with few packs is not.
func TestARefreshSaysWhenItsMirrorIsToBeRepacked(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, _, _ := gitNode(t, upstream)
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api", auth: gitAuth(githubBasic)}
	key := request.key()
	refresh := func() *gitRefresh {
		t.Helper()

		started := service.startRefresh(t.Context(), request, upstream.repositoryID("api"),
			service.git.mirror(key), "refs/heads/main", nil, service.git.ask())
		if err := gitWithin(t, "a refresh", func() error {
			<-started.done

			return started.err
		}); err != nil {
			t.Fatalf("a refresh: %v", err)
		}

		return started
	}

	if !refresh().maintaining {
		t.Error("the refresh that made the mirror did not say it is to be repacked")
	}
	awaitIdleMirror(t, service, key)
	if refresh().maintaining {
		t.Error("a refresh of a mirror repacked since, with few packs, said it is to be repacked")
	}
}

// WHAT IS DUE is read from the repack stamp and the pack count, and a pass is
// counted by what ended it.
func TestMaintenanceIsDueByTheStampAndThePacks(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	now := time.Now()
	service.now = func() time.Time { return now }
	service.git.maxPacks = 2
	path := filepath.Join(t.TempDir(), "m.git")
	packs := filepath.Join(path, "objects", "pack")
	if err := os.MkdirAll(packs, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := func(name string, at time.Time) {
		t.Helper()

		file := filepath.Join(path, name)
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	addPack := func(name string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(packs, name+".pack"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if due := service.maintenanceDue(path); due != gitMaintainFull {
		t.Errorf("a mirror never repacked is due %q, want full", due)
	}
	stamp(gitRepackedStamp, now.Add(-time.Hour))
	addPack("a")
	if due := service.maintenanceDue(path); due != "" {
		t.Errorf("a mirror repacked an hour ago with one pack is due %q, want nothing", due)
	}
	addPack("b")
	if due := service.maintenanceDue(path); due != gitMaintainGeometric {
		t.Errorf("a mirror at its most packs is due %q, want geometric", due)
	}
	if _, err := os.Stat(filepath.Join(path, gitPrunedStamp)); err != nil {
		t.Errorf("a mirror never pruned was not dated from now, so it would be pruned on its first day: %v", err)
	}
	stamp(gitPrunedStamp, now.Add(-gitPruneEvery))
	if due := service.maintenanceDue(path); due != gitMaintainPrune {
		t.Errorf("a mirror pruned a week ago is due %q, want prune", due)
	}
	// A DAILY REPACK NEVER STANDS IN PRUNING'S WAY, or a mirror refreshed about
	// once a day would never be pruned.
	stamp(gitRepackedStamp, now.Add(-gitFullRepackEvery))
	if due := service.maintenanceDue(path); due != gitMaintainPrune {
		t.Errorf("a mirror repacked a day ago and pruned a week ago is due %q, want prune", due)
	}
	stamp(gitPrunedStamp, now.Add(-time.Hour))
	if due := service.maintenanceDue(path); due != gitMaintainFull {
		t.Errorf("a mirror repacked a day ago is due %q, want full", due)
	}
	// A STAMP THAT CANNOT BE READ (here a link to itself) is no pass that
	// rewrites or removes, whatever the other says.
	stamp(gitPrunedStamp, now.Add(-gitPruneEvery))
	repacked := filepath.Join(path, gitRepackedStamp)
	if err := os.Remove(repacked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitRepackedStamp, repacked); err != nil {
		t.Fatal(err)
	}
	if due := service.maintenanceDue(path); due != gitMaintainGeometric {
		t.Errorf("a mirror whose repack stamp cannot be read is due %q, want only a roll-up", due)
	}

	for err, want := range map[error]string{
		nil: gitMaintainOK,
		errors.Join(errGitClosed, errGitDiskFull): gitMaintainClosed,
		errGitDiskFull:     gitMaintainDiskFull,
		errGitMaintainBusy: gitMaintainBusy,
		errNotOurs:         gitMaintainFailed,
	} {
		if got := gitMaintenanceOutcome(err); got != want {
			t.Errorf("gitMaintenanceOutcome(%v) = %q, want %q", err, got, want)
		}
	}
}

// A FULL FILESYSTEM ENDS MAINTENANCE WHILE IT RUNS, not only before it starts:
// a repack can write more than the mirror's own size, so its filesystem is
// watched throughout and the pass ended as disk_full.
func TestAFullFilesystemEndsMaintenance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	untilEnded := func(ctx context.Context) error {
		<-ctx.Done()

		return ctx.Err()
	}
	err := gitWithin(t, "a pass on a filesystem past its fill", func() error {
		return watchFill(t.Context(), root, 0, untilEnded)
	})
	if !errors.Is(err, errGitDiskFull) || gitMaintenanceOutcome(err) != gitMaintainDiskFull {
		t.Fatalf("a pass on a full filesystem ended %v, want disk_full", err)
	}
	ran := false
	if err := watchFill(t.Context(), root, 1, func(context.Context) error {
		ran = true

		return nil
	}); err != nil || !ran {
		t.Fatalf("a pass with room ended %v (ran %v), want it to run and succeed", err, ran)
	}
	if room, err := roomFor(root, 1, 1<<62); err != nil || room {
		t.Errorf("roomFor admitted an exabyte: %v, %v", room, err)
	}
	// A FILL THAT CANNOT BE READ ENDS IT TOO, as a failure rather than a full
	// disk: the estimate before it started is no reason to go on blind.
	err = gitWithin(t, "a pass whose fill cannot be read", func() error {
		return watchFill(t.Context(), filepath.Join(root, "gone"), 1, untilEnded)
	})
	if err == nil || errors.Is(err, errGitDiskFull) || gitMaintenanceOutcome(err) != gitMaintainFailed {
		t.Fatalf("a pass whose fill could not be read ended %v, want failed", err)
	}
}

// AN OBJECT IS DATED BY THE PASS THAT FIRST SETS IT ASIDE, not by the pack it
// was in: one that became unreachable a moment ago, in a pack a month old, is
// kept by the prune that finds it, where git alone would date it a month.
func TestAnObjectIsDatedByThePassThatSetsItAside(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	path := service.git.mirrorPath(key)
	awaitIdleMirror(t, service, key)
	orphan := upstream.commitTo(t, "superseded", map[string]string{"s": "1\n"})
	advertise(t, node, token, "api")
	awaitIdleMirror(t, service, key)

	// A MONTH SINCE THE LAST FULL REPACK, every pack as old, and a week since
	// the last prune; then the branch goes, and the refresh that removes it
	// finds the prune due.
	month := time.Now().Add(-30 * 24 * time.Hour)
	for _, file := range append(packFiles(t, path, "*"), filepath.Join(path, gitRepackedStamp)) {
		if err := os.Chtimes(file, month, month); err != nil {
			t.Fatal(err)
		}
	}
	weekAgo := time.Now().Add(-gitPruneEvery)
	if err := os.Chtimes(filepath.Join(path, gitPrunedStamp), weekAgo, weekAgo); err != nil {
		t.Fatal(err)
	}
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	runIn(t, upstream.repo, "update-ref", "-d", "refs/heads/superseded")
	advertise(t, node, token, "api")
	awaitIdleMirror(t, service, key)

	if want := []string{"prune ok"}; !slices.Equal(observer.maintained(), want) {
		t.Fatalf("the maintenance was %v, want %v", observer.maintained(), want)
	}
	if !hasObject(t, path, orphan) {
		t.Fatal("a commit that became unreachable a moment ago was pruned for the age of its old pack")
	}
}

// A MIRROR WHOSE PACKS CANNOT BE LISTED IS NOT REPACKED: a pack a failed
// listing skipped would keep its old date into the prune.
func TestAMirrorWhosePacksCannotBeListedIsNotFreshened(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	path := filepath.Join(t.TempDir(), "m.git")
	packs := filepath.Join(path, "objects", "pack")
	if err := os.MkdirAll(packs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(packs, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(packs, 0o700) }) //nolint:errcheck // best effort: the temporary directory goes either way
	if err := service.freshenPacks(path); err == nil {
		t.Fatal("a mirror whose packs could not be listed was freshened as though it had none")
	}
}

// awaitIdleMirror waits until the mirror of key has no refresh or repack in
// flight, which goes on after the request that started it was answered.
func awaitIdleMirror(t *testing.T, service *CacheService, key string) {
	t.Helper()

	mirror := service.git.mirror(key)
	gitWithin(t, "the mirror's refresh and repack", func() struct{} {
		for t.Context().Err() == nil {
			mirror.mu.Lock()
			idle := mirror.refreshing == nil
			mirror.mu.Unlock()
			if idle {
				return struct{}{}
			}
			time.Sleep(10 * time.Millisecond)
		}

		return struct{}{}
	})
}

// packFiles is the files in the pack directory of the repository at path that
// match pattern.
func packFiles(t *testing.T, path, pattern string) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(path, "objects", "pack", pattern))
	if err != nil {
		t.Fatal(err)
	}

	return files
}

// hasObject is whether the repository at path holds object, by git's own
// answer: a git that could not be asked fails the test rather than read as
// missing.
func hasObject(t *testing.T, path, object string) bool {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "--batch-check")
	cmd.Dir = path
	cmd.Env = append(isolatedGitEnviron(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+path)
	cmd.Stdin = strings.NewReader(object + "\n")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("ask %s for %s: %v", path, object, err)
	}
	switch answer := strings.TrimSpace(string(output)); {
	case answer == object+" missing":
		return false
	case strings.HasPrefix(answer, object+" "):
		return true
	default:
		t.Fatalf("%s answered %q for %s", path, answer, object)

		return false
	}
}
