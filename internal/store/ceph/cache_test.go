package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

type cacheFake struct {
	calls                [][]string
	images               map[string]bool
	snapshots            map[string]bool
	parents              map[string]string
	depths               map[string]int
	trash                map[string]string
	metadata             map[string]map[string]string
	mappings             map[string]string
	lockCookie           string
	locker               string
	heartbeat            func() string
	heartbeatErr         error
	lockAdds             int
	lockAddErr           error
	lockAddErrOn         int
	cancelLockAddOn      int
	cancelLockAdd        context.CancelFunc
	committedLockAddErr  error
	lockLists            int
	lockListCanceledErr  error
	releaseAfterLockList int
	releaseOn            int
	lockRemoves          int
	lockRemoveErrOn      int
	lockRemoveErr        error
	onLockList           func()
	nextDevice           int
	failMeta             string
	failRemove           bool
	missingImages        map[string]bool
	onMetaList           func()
	metaListErr          error
	// removeLocked records, per metadata key, whether the cache index lock was
	// held at the instant `image-meta remove` ran for it.
	removeLocked map[string]bool
}

type cacheExitError struct {
	code    int
	message string
}

func (e cacheExitError) Error() string { return e.message }
func (e cacheExitError) ExitCode() int { return e.code }

func newCacheFake() *cacheFake {
	return &cacheFake{
		images:    map[string]bool{},
		snapshots: map[string]bool{},
		parents:   map[string]string{},
		depths:    map[string]int{},
		trash:     map[string]string{},
		metadata:  map[string]map[string]string{},
		mappings:  map[string]string{},
		locker:    "client.1234",

		removeLocked: map[string]bool{},
	}
}

func (f *cacheFake) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.calls = append(f.calls, slices.Clone(args))

	if i := slices.Index(args, "lock"); i >= 0 {
		return f.lock(ctx, args[i+1:])
	}

	if i := slices.Index(args, "image-meta"); i >= 0 {
		return f.imageMeta(args[i+1:])
	}

	if i := slices.Index(args, "device"); i >= 0 {
		return f.device(args[i+1:])
	}

	if i := slices.Index(args, "snap"); i >= 0 {
		return f.snap(args[i+1:])
	}

	if i := slices.Index(args, "trash"); i >= 0 {
		return f.trashCommand(args[i+1:])
	}

	for _, verb := range []string{"create", "clone", "cp", "info", "rm", "ls"} {
		if i := slices.Index(args, verb); i >= 0 {
			return f.image(verb, args[i+1:], args)
		}
	}

	return nil, fmt.Errorf("fake does not understand %v", args)
}

func (f *cacheFake) trashCommand(args []string) ([]byte, error) {
	switch args[0] {
	case "mv":
		image := args[1]
		if !f.images[image] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		_, name, _ := strings.Cut(image, "/")
		id := "id-" + name
		f.trash[id] = name
		delete(f.images, image)

		return nil, nil
	case "list":
		type entry struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}

		entries := make([]entry, 0, len(f.trash))
		for id, name := range f.trash {
			entries = append(entries, entry{ID: id, Name: name})
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.ID, b.ID) })

		return json.Marshal(entries)
	case "rm":
		_, id, _ := strings.Cut(args[1], "/")
		name, ok := f.trash[id]
		if !ok {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		image := "billet-cache/" + name
		for _, parent := range f.parents {
			if strings.HasPrefix(parent, image+"@") {
				return nil, fmt.Errorf("exit status 39: %w", cacheExitError{
					code: 39,
					message: "rbd: image has snapshots - these must be deleted with 'rbd snap purge' " +
						"before the image can be removed",
				})
			}
		}

		delete(f.trash, id)
		delete(f.parents, image)
		delete(f.depths, image)

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown trash verb %q", args[0])
	}
}

func (f *cacheFake) lock(ctx context.Context, args []string) ([]byte, error) {
	switch args[0] {
	case "add":
		f.lockAdds++
		if f.lockAddErr != nil && (f.lockAddErrOn == 0 || f.lockAdds == f.lockAddErrOn) {
			return nil, f.lockAddErr
		}
		if f.lockAdds == f.releaseOn {
			f.lockCookie = ""
		}
		if f.lockCookie != "" {
			return nil, cacheExitError{code: 16, message: "exit status 16"}
		}

		f.lockCookie = args[2]
		if f.lockAdds == f.cancelLockAddOn && f.cancelLockAdd != nil {
			f.cancelLockAdd()
			if f.committedLockAddErr != nil {
				return nil, f.committedLockAddErr
			}

			return nil, ctx.Err()
		}

		return nil, nil
	case "ls":
		f.lockLists++
		if f.onLockList != nil {
			hook := f.onLockList
			f.onLockList = nil
			hook()
		}
		if err := ctx.Err(); err != nil {
			if f.lockListCanceledErr != nil {
				return nil, f.lockListCanceledErr
			}

			return nil, err
		}
		if f.lockCookie == "" {
			return []byte("[]"), nil
		}

		out, err := json.Marshal([]lockEntry{{ID: f.lockCookie, Locker: f.locker}})
		if f.lockLists == f.releaseAfterLockList {
			f.lockCookie = ""
		}

		return out, err
	case "rm":
		f.lockRemoves++
		if f.lockRemoves == f.lockRemoveErrOn && f.lockRemoveErr != nil {
			return nil, f.lockRemoveErr
		}
		if f.lockCookie == "" {
			return nil, cacheExitError{code: 2, message: "exit status 2"}
		}
		if args[2] != f.lockCookie || args[3] != f.locker {
			return nil, errors.New("wrong lock owner")
		}

		f.lockCookie = ""

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown lock verb %q", args[0])
	}
}

func (f *cacheFake) imageMeta(args []string) ([]byte, error) {
	verb, image := args[0], args[1]
	// A cold site has no cache-index image yet, so rbd answers ENOENT to any
	// image-meta command against it. Model that when the test asks for it, so a
	// cold-index lookup exercises the real "no such image" path rather than the
	// convenience empty map below.
	if f.missingImages[image] {
		return nil, errors.New("rbd: (2) No such file or directory")
	}
	if f.metadata[image] == nil {
		f.metadata[image] = map[string]string{}
	}

	switch verb {
	case "set":
		if args[2] == f.failMeta {
			return nil, errors.New("injected metadata failure")
		}

		f.metadata[image][args[2]] = args[3]

		return nil, nil
	case "get":
		if f.heartbeatErr != nil && strings.HasPrefix(args[2], "billet.heartbeat.") {
			return nil, f.heartbeatErr
		}
		if f.heartbeat != nil && strings.HasPrefix(args[2], "billet.heartbeat.") {
			value := f.heartbeat()
			if f.heartbeatErr != nil {
				return nil, f.heartbeatErr
			}

			return []byte(value + "\n"), nil
		}
		value, ok := f.metadata[image][args[2]]
		if !ok {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		return []byte(value + "\n"), nil
	case "remove":
		f.removeLocked[args[2]] = f.lockCookie != ""
		delete(f.metadata[image], args[2])

		return nil, nil
	case "list":
		if f.metaListErr != nil {
			return nil, f.metaListErr
		}
		var lines []string
		for key, value := range f.metadata[image] {
			lines = append(lines, key+" "+value)
		}

		slices.Sort(lines)

		// Models a reap that lands mid-list: rbd pages image-meta, so a pointer
		// captured in the returned view may already be gone by the time a caller
		// acts on it. The hook fires AFTER the view is built, so a later get of
		// the same key misses — exactly the torn-read the confirm guards against.
		if f.onMetaList != nil {
			hook := f.onMetaList
			f.onMetaList = nil
			hook()
		}

		return []byte(strings.Join(lines, "\n")), nil
	default:
		return nil, fmt.Errorf("unknown image-meta verb %q", verb)
	}
}

func (f *cacheFake) device(args []string) ([]byte, error) {
	switch args[0] {
	case "map":
		image := args[1]
		if !f.images[image] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		device := fmt.Sprintf("/dev/rbd%d", f.nextDevice)
		f.nextDevice++
		f.mappings[device] = image

		return []byte(device + "\n"), nil
	case "list":
		type mapping struct {
			Pool   string `json:"pool"`
			Name   string `json:"name"`
			Device string `json:"device"`
		}

		var out []mapping
		for device, image := range f.mappings {
			pool, name, _ := strings.Cut(image, "/")
			out = append(out, mapping{Pool: pool, Name: name, Device: device})
		}

		if out == nil {
			out = []mapping{}
		}

		return json.Marshal(out)
	case "unmap":
		delete(f.mappings, args[1])

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown device verb %q", args[0])
	}
}

func (f *cacheFake) snap(args []string) ([]byte, error) {
	switch args[0] {
	case "create":
		f.snapshots[args[1]] = true

		return nil, nil
	case "rm":
		delete(f.snapshots, args[1])

		return nil, nil
	case "purge":
		prefix := args[1] + "@"
		for snapshot := range f.snapshots {
			if strings.HasPrefix(snapshot, prefix) {
				delete(f.snapshots, snapshot)
			}
		}

		return nil, nil
	default:
		return nil, fmt.Errorf("unknown snap verb %q", args[0])
	}
}

func (f *cacheFake) image(verb string, tail, all []string) ([]byte, error) {
	switch verb {
	case "create":
		f.images[tail[0]] = true
		f.depths[tail[0]] = 0

		return nil, nil
	case "clone":
		if !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		parent := strings.SplitN(tail[0], "@", 2)[0]
		depth := f.depths[parent] + 1
		if depth > maxCacheCloneDepth {
			return nil, fmt.Errorf("fake: clone depth %d exceeds %d", depth, maxCacheCloneDepth)
		}

		f.images[tail[1]] = true
		f.parents[tail[1]] = tail[0]
		f.depths[tail[1]] = depth

		return nil, nil
	case "cp":
		if !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		f.images[tail[1]] = true
		f.depths[tail[1]] = 0

		return nil, nil
	case "info":
		if !f.images[tail[0]] && !f.snapshots[tail[0]] {
			return nil, errors.New("rbd: (2) No such file or directory")
		}

		return []byte(`{"size":1073741824}`), nil
	case "rm":
		if f.failRemove {
			return nil, errors.New("injected remove failure")
		}
		for _, parent := range f.parents {
			if strings.HasPrefix(parent, tail[0]+"@") {
				return nil, fmt.Errorf("exit status 39: %w", cacheExitError{
					code:    39,
					message: "rbd: image has snapshots with linked clones",
				})
			}
		}

		delete(f.images, tail[0])
		delete(f.parents, tail[0])
		delete(f.depths, tail[0])

		return nil, nil
	case "ls":
		pool := valueAfter(all, "-p")
		// rbd ls --format json answers an empty pool with [], not null; billet
		// rejects a null list, so the fake must return a non-nil empty slice.
		names := []string{}
		for image := range f.images {
			imagePool, name, _ := strings.Cut(image, "/")
			if imagePool == pool {
				names = append(names, name)
			}
		}

		slices.Sort(names)

		return json.Marshal(names)
	default:
		return nil, fmt.Errorf("unknown image verb %q", verb)
	}
}

func valueAfter(args []string, key string) string {
	for i := range len(args) - 1 {
		if args[i] == key {
			return args[i+1]
		}
	}

	return ""
}

func cacheClient(t *testing.T, f *cacheFake) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run), withFilesystemVerifier(func(context.Context, string) (storecontract.Filesystem, error) {
			return storecontract.Filesystem{
				Type: "ext4", UUID: "dcab7af5-4ae7-4cc1-8ddb-1db18956c389", Clean: true,
			}, nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

func (f *cacheFake) ranWith(fragments ...string) bool {
	for _, call := range f.calls {
		if slices.ContainsFunc(fragments, func(fragment string) bool {
			return !slices.Contains(call, fragment)
		}) {
			continue
		}

		return true
	}

	return false
}

func TestCacheLockStaleAfterIsTenMinutes(t *testing.T) {
	t.Parallel()

	if CacheLockStaleAfter != 10*time.Minute {
		t.Fatalf("cache-index recovery begins after %s, want the documented ten-minute bound",
			CacheLockStaleAfter)
	}
}

func TestCacheIndexLockWaitIncludesRecoveryMargin(t *testing.T) {
	t.Parallel()

	const minimumRecoveryGap = 30 * time.Second
	wantMinimum := CacheLockStaleAfter + HeartbeatObservation + minimumRecoveryGap
	if cacheLockRecoveryGap < minimumRecoveryGap || cacheLockWaitLimit < wantMinimum {
		t.Fatalf("cache-index wait limit %s does not include the %s recovery margin after %s",
			cacheLockWaitLimit, minimumRecoveryGap, CacheLockStaleAfter+HeartbeatObservation)
	}
}

func TestCacheIndexLockReleasesAnInitialAddCommittedAsItsCallerCancels(t *testing.T) {
	t.Parallel()

	addErr := errors.New("lock add response was lost")
	ctx, cancel := context.WithCancel(t.Context())
	f := newCacheFake()
	f.cancelLockAddOn = 1
	f.cancelLockAdd = cancel
	f.committedLockAddErr = addErr
	c := cacheClient(t, f)

	ran := false
	err := c.withCacheLock(ctx, time.Now(), func(time.Time) error {
		ran = true

		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ambiguously acknowledged canceled acquisition returned %v, want cancellation", err)
	}
	if !errors.Is(err, addErr) {
		t.Fatalf("ambiguously acknowledged canceled acquisition lost the add failure: %v", err)
	}
	if ran {
		t.Fatal("cache work ran after its caller canceled during lock acquisition")
	}
	if f.lockCookie != "" {
		t.Fatalf("canceled acquisition stranded cache-index lock %q", f.lockCookie)
	}
}

func TestCacheIndexLockReleasesAPostBreakAddCommittedAsItsCallerCancels(t *testing.T) {
	t.Parallel()

	addErr := errors.New("reacquisition response was lost")
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-deadhost-7-deadbeefdeadbeef-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	ctx, cancel := context.WithCancel(t.Context())
	f := newCacheFake()
	f.lockCookie = holder
	f.cancelLockAddOn = 2
	f.cancelLockAdd = cancel
	f.committedLockAddErr = addErr
	c := cacheClient(t, f)
	c.observation = time.Millisecond

	ran := false
	err := c.withCacheLock(ctx, now, func(time.Time) error {
		ran = true

		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ambiguously acknowledged canceled reacquisition returned %v, want cancellation", err)
	}
	if !errors.Is(err, addErr) {
		t.Fatalf("ambiguously acknowledged canceled reacquisition lost the add failure: %v", err)
	}
	if ran {
		t.Fatal("cache work ran after its caller canceled during stale-lock reacquisition")
	}
	if f.lockCookie != "" {
		t.Fatalf("canceled reacquisition stranded cache-index lock %q", f.lockCookie)
	}
	if !f.ranWith("lock", "rm", holder) {
		t.Fatal("the stale holder was not removed before the canceled reacquisition")
	}
}

func TestCacheIndexLockPreservesReleaseFailureAfterCanceledAmbiguousAdds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		holder          string
		cancelLockAddOn int
		lockRemoveErrOn int
	}{
		{name: "initial add", cancelLockAddOn: 1, lockRemoveErrOn: 1},
		{
			name: "post-break add",
			holder: fmt.Sprintf("billet-import-deadhost-7-acdeacdeacdeacde-%d",
				now.Add(-100*CacheLockStaleAfter).Unix()),
			cancelLockAddOn: 2,
			lockRemoveErrOn: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addErr := errors.New("lock add response was lost")
			releaseErr := errors.New("lock release response was lost")
			ctx, cancel := context.WithCancel(t.Context())
			f := newCacheFake()
			f.lockCookie = tc.holder
			f.cancelLockAddOn = tc.cancelLockAddOn
			f.cancelLockAdd = cancel
			f.committedLockAddErr = addErr
			f.lockRemoveErrOn = tc.lockRemoveErrOn
			f.lockRemoveErr = releaseErr
			c := cacheClient(t, f)
			c.observation = time.Millisecond

			err := c.withCacheLock(ctx, now, func(time.Time) error {
				t.Fatal("cache work ran after cancellation during ambiguous lock acquisition")

				return nil
			})
			for name, want := range map[string]error{
				"add": addErr, "cancellation": context.Canceled, "release": releaseErr,
			} {
				if !errors.Is(err, want) {
					t.Errorf("canceled ambiguous acquisition lost its %s failure: %v", name, err)
				}
			}
		})
	}
}

func TestCacheIndexLockReclaimsAHolderThatWasFreshWhenWaitingBegan(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-deadhost-7-0123456789abcdef-%d", now.Unix())
	f := newCacheFake()
	f.lockCookie = holder
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockRetry = time.Millisecond
	elapsedCalls := 0
	c.cacheLockElapsed = func() time.Duration {
		elapsedCalls++
		if elapsedCalls == 1 {
			return CacheLockStaleAfter + time.Second
		}

		return CacheLockStaleAfter + c.observation + 2*time.Second
	}

	lockedAt := time.Time{}

	// GENEROUS ON THE WALL CLOCK, DETERMINISTIC ON THE PROPERTY.
	//
	// This carried 100ms, which is not a bound on anything the test asserts — it
	// is a bound on how long the machine takes to run two fake commands and a
	// 1ms timer. The happy path finishes in 0.00s, so 100ms reads like ample
	// headroom, and it is not: all it takes is the goroutine being descheduled
	// once, which a saturated `-race` run of the whole repository does regularly.
	// That is what made this test flake in CI.
	//
	// "Within one bounded wait" is a claim about the number of RETRIES, so it is
	// asserted as one below, off the controlled clock rather than the real one.
	// This deadline is now only a net against a genuine hang.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := c.withCacheLock(ctx, now, func(at time.Time) error {
		lockedAt = at

		return nil
	}); err != nil {
		t.Fatalf("fresh leaked cache-index lock was not reclaimed within one bounded wait: %v", err)
	}

	// EXACTLY ONE RETRY, which is what "one bounded wait" means and what the wall
	// clock was standing in for. withCacheLock reads the elapsed clock once per
	// retry to age its next attempt, and once more to stamp lockedAt — so two
	// calls is one wait, and a third would be a second wait.
	if elapsedCalls != 2 {
		t.Errorf("the lock was reclaimed after %d elapsed-clock reads, want 2 — one retry to "+
			"age the attempt past the holder's staleness, then the stamp. More than that is "+
			"more than one bounded wait", elapsedCalls)
	}
	if !f.ranWith("lock", "rm", holder) {
		t.Fatalf("the initially fresh leaked holder was not reclaimed; billet ran %v", f.calls)
	}
	if !lockedAt.After(now.Add(CacheLockStaleAfter + c.observation)) {
		t.Fatalf("cache work began at %s, before the controlled stale recovery completed", lockedAt)
	}
}

func TestCacheIndexLockRecoversOnItsOwnStaleBound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	staleCookie := fmt.Sprintf("billet-import-deadhost-7-deadbeefdeadbeef-%d",
		now.Add(-CacheLockStaleAfter-time.Minute).Unix())
	f := newCacheFake()
	f.lockCookie = staleCookie
	c := cacheClient(t, f)
	c.observation = time.Millisecond

	ran := false
	if err := c.withCacheLock(t.Context(), now, func(time.Time) error {
		ran = true

		return nil
	}); err != nil {
		t.Fatalf("a silent cache-index lock past its recovery bound was not reclaimed: %v", err)
	}
	if !ran {
		t.Fatal("the cache operation never resumed after reclaiming its stale lock")
	}
	if !f.ranWith("lock", "rm", staleCookie) {
		t.Fatalf("the stale cache-index holder was not removed; billet ran %v", f.calls)
	}
}

func TestCacheIndexLockKeepsASilentHolderInsideItsBound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-feedfacefeedface-%d",
		now.Add(-CacheLockStaleAfter+time.Minute).Unix())
	f := newCacheFake()
	f.lockCookie = holder
	f.releaseOn = 2
	c := cacheClient(t, f)
	c.cacheLockRetry = time.Millisecond

	ran := false
	if err := c.withCacheLock(t.Context(), now, func(time.Time) error {
		ran = true

		return nil
	}); err != nil {
		t.Fatalf("cache work did not resume after a recent holder released the index: %v", err)
	}
	if !ran {
		t.Fatal("cache work never ran after the recent holder released the index")
	}
	if f.lockAdds != 2 {
		t.Fatalf("billet tried to take the cache-index lock %d times, want one wait and one retry", f.lockAdds)
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet removed a cache-index holder before its recovery bound")
	}
	if f.ranWith("image-meta", "get", heartbeatKey(holder)) {
		t.Fatal("billet waited through heartbeat observations for a holder too recent to break")
	}
}

func TestCacheIndexLockKeepsAnOldHolderWhoseHeartbeatMoves(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-abcd1234abcd1234-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	beats := 0
	f := newCacheFake()
	f.lockCookie = holder
	f.releaseOn = 2
	f.heartbeat = func() string {
		beats++

		return strconv.Itoa(beats)
	}
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockRetry = time.Millisecond

	if err := c.withCacheLock(t.Context(), now, func(time.Time) error { return nil }); err != nil {
		t.Fatalf("cache work did not resume after the live old holder released the index: %v", err)
	}
	if beats != 2 {
		t.Fatalf("billet read %d heartbeats, want the two observations needed for liveness", beats)
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet removed a live cache-index holder because of its age")
	}
}

func TestCacheIndexLockRefusesToBreakWhenHeartbeatCannotBeRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-aaaaaaaaaaaaaaaa-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	heartbeatErr := cacheExitError{code: 13, message: "rbd: permission denied"}
	f := newCacheFake()
	f.lockCookie = holder
	f.heartbeatErr = heartbeatErr
	c := cacheClient(t, f)
	c.observation = time.Millisecond

	ran := false
	err := c.withCacheLock(t.Context(), now, func(time.Time) error {
		ran = true

		return nil
	})
	if !errors.Is(err, heartbeatErr) {
		t.Fatalf("unreadable holder heartbeat returned %v, want the Ceph failure", err)
	}
	if ran {
		t.Fatal("cache work ran without proving the old holder was silent")
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet broke an old holder whose heartbeat could not be read")
	}
}

func TestCacheIndexLockRefusesToBreakOnAMalformedHeartbeat(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-bbbbbbbbbbbbbbbb-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	f := newCacheFake()
	f.lockCookie = holder
	f.heartbeat = func() string { return "not-a-counter" }
	c := cacheClient(t, f)
	c.observation = time.Millisecond

	ran := false
	err := c.withCacheLock(t.Context(), now, func(time.Time) error {
		ran = true

		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid heartbeat counter") {
		t.Fatalf("malformed holder heartbeat returned %v, want a fail-closed diagnostic", err)
	}
	if ran {
		t.Fatal("cache work ran without a valid heartbeat observation")
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet broke an old holder whose heartbeat was malformed")
	}
}

func TestCacheIndexLockRetriesWhenAnOldHolderReleasesDuringObservation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-f1a15eedf1a15eed-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	reads := 0
	f := newCacheFake()
	f.lockCookie = holder
	f.heartbeat = func() string {
		reads++
		if reads == 2 {
			f.lockCookie = ""
			f.heartbeatErr = errors.New("rbd: (2) No such file or directory")

			return ""
		}

		return "1"
	}
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockRetry = time.Millisecond

	ran := false
	if err := c.withCacheLock(t.Context(), now, func(time.Time) error {
		ran = true

		return nil
	}); err != nil {
		t.Fatalf("cache work did not retry after the observed holder released normally: %v", err)
	}
	if !ran {
		t.Fatal("cache work never ran after the observed holder released")
	}
	if f.lockAdds != 2 {
		t.Fatalf("billet attempted the lock %d times, want the observed holder then one retry", f.lockAdds)
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet tried to remove a holder that had already released the index")
	}
}

func TestCacheIndexLockRetriesWhenAHolderReleasesAfterRevalidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-f1a1ace0f1a1ace0-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	f := newCacheFake()
	f.lockCookie = holder
	f.releaseAfterLockList = 2
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockRetry = time.Millisecond

	if err := c.withCacheLock(t.Context(), now, func(time.Time) error { return nil }); err != nil {
		t.Fatalf("cache work did not retry after the revalidated holder released: %v", err)
	}
	if f.lockAdds != 2 {
		t.Fatalf("billet attempted the lock %d times, want the raced holder then one retry", f.lockAdds)
	}
}

func TestCacheIndexLockRetriesWhenAReacquisitionWinnerAlreadyReleased(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-deadhost-7-decafbaddecafbad-%d",
		now.Add(-100*CacheLockStaleAfter).Unix())
	f := newCacheFake()
	f.lockCookie = holder
	f.lockAddErr = cacheExitError{code: 16, message: "exit status 16"}
	f.lockAddErrOn = 2
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockRetry = time.Millisecond

	if err := c.withCacheLock(t.Context(), now, func(time.Time) error { return nil }); err != nil {
		t.Fatalf("cache work did not retry after the reacquisition winner released: %v", err)
	}
	if f.lockAdds != 3 {
		t.Fatalf("billet attempted the lock %d times, want stale holder, lost race, and retry", f.lockAdds)
	}
}

func TestCacheIndexLockStopsWaitingWhenItsCallerCancels(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-ca11ed00ca11ed00-%d",
		now.Add(-time.Minute).Unix())
	readErr := fmt.Errorf("lock listing observed cancellation: %w", context.Canceled)
	ctx, cancel := context.WithCancel(t.Context())
	f := newCacheFake()
	f.lockCookie = holder
	f.onLockList = cancel
	f.lockListCanceledErr = readErr
	c := cacheClient(t, f)
	c.cacheLockRetry = time.Hour

	ran := false
	err := c.withCacheLock(ctx, now, func(time.Time) error {
		ran = true

		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache-index wait returned %v, want context.Canceled", err)
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("exit-16 reconciliation did not preserve caller cancellation at the lock read: %v", err)
	}
	if ran {
		t.Fatal("cache work ran without acquiring the index after cancellation")
	}
	if f.lockAdds != 1 {
		t.Fatalf("billet attempted the lock %d times after cancellation, want one", f.lockAdds)
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet removed a live cache-index holder when its own caller cancelled")
	}
}

func TestCacheIndexLockRefusesAnUnknownHolderWithoutRetrying(t *testing.T) {
	t.Parallel()

	for _, holder := range []string{
		"somebody-elses-tool-1234567890",
		"billet-import-external-tool-1234567890",
		"billet-build-external-tool-1234567890",
	} {
		t.Run(holder, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			f.lockCookie = holder
			c := cacheClient(t, f)
			c.observation = time.Millisecond
			c.cacheLockRetry = time.Millisecond

			err := c.withCacheLock(t.Context(), time.Now(), func(time.Time) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "age cannot be established") {
				t.Fatalf("unknown cache-index holder returned %v, want a dated fail-closed refusal", err)
			}
			if f.lockAdds != 1 {
				t.Fatalf("billet attempted an unknown cache-index holder %d times, want one", f.lockAdds)
			}
			if f.ranWith("lock", "rm") {
				t.Fatal("billet removed a cache-index holder whose age it could not establish")
			}
		})
	}
}

func TestCacheIndexLockDoesNotRetryNonContentionAddFailures(t *testing.T) {
	t.Parallel()

	for _, holder := range []string{
		"",
		"billet-import-otherhost-7-beadbeadbeadbead-1234567890",
	} {
		t.Run(map[bool]string{true: "empty listing", false: "populated listing"}[holder == ""],
			func(t *testing.T) {
				t.Parallel()

				permissionErr := cacheExitError{code: 13, message: "rbd: permission denied"}
				f := newCacheFake()
				f.lockCookie = holder
				f.lockAddErr = permissionErr
				c := cacheClient(t, f)
				c.cacheLockRetry = time.Millisecond
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()

				err := c.withCacheLock(ctx, time.Now(), func(time.Time) error { return nil })
				if !errors.Is(err, permissionErr) {
					t.Fatalf("non-contention lock-add failure returned %v, want the original error", err)
				}
				if f.lockAdds != 1 {
					t.Fatalf("non-contention lock-add failure was attempted %d times, want one", f.lockAdds)
				}
				if f.ranWith("lock", "rm") {
					t.Fatal("billet removed a holder after a non-contention lock-add failure")
				}
			})
	}
}

func TestAcquireWriterStartsItsLifetimeAfterCacheIndexContention(t *testing.T) {
	t.Parallel()

	now := time.Now()
	ttl := time.Minute
	f := newCacheFake()
	f.lockCookie = fmt.Sprintf("billet-import-livehost-7-de1a9ed0de1a9ed0-%d",
		now.Add(-time.Minute).Unix())
	f.releaseOn = 2
	c := cacheClient(t, f)
	c.cacheLockRetry = 20 * time.Millisecond

	lease, _, err := c.acquireWriterAt(t.Context(), "acme/api/npm", "job-1", ttl, now)
	if err != nil {
		t.Fatalf("AcquireWriter after contention: %v", err)
	}
	if extension := lease.Expires.Sub(now.Add(ttl)); extension < 10*time.Millisecond {
		t.Fatalf("writer lifetime began before the cache-index wait; extension = %s, want at least 10ms", extension)
	}
}

func TestAcquireWriterStartsItsLifetimeAfterStaleLockObservation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ttl := time.Minute
	f := newCacheFake()
	f.lockCookie = fmt.Sprintf("billet-import-deadhost-7-0b5e7ed00b5e7ed0-%d",
		now.Add(-CacheLockStaleAfter-time.Minute).Unix())
	c := cacheClient(t, f)
	c.observation = time.Millisecond
	c.cacheLockElapsed = func() time.Duration { return 2 * time.Minute }

	lease, _, err := c.acquireWriterAt(t.Context(), "acme/api/npm", "job-1", ttl, now)
	if err != nil {
		t.Fatalf("AcquireWriter after stale-lock observation: %v", err)
	}
	if want := now.Add(2*time.Minute + ttl); !lease.Expires.Equal(want) {
		t.Fatalf("writer expires at %s, want post-acquisition time %s", lease.Expires, want)
	}
}

func TestPublishLockDoesNotInheritTheCacheRecoveryBound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	holder := fmt.Sprintf("billet-import-livehost-7-cafebabecafebabe-%d",
		now.Add(-CacheLockStaleAfter-time.Minute).Unix())
	f := newCacheFake()
	f.lockCookie = holder
	c := cacheClient(t, f)
	c.observation = time.Millisecond

	if _, err := c.TakePublishLock(t.Context(), now); err == nil {
		t.Fatal("the golden-image publish lock inherited the shorter cache recovery bound")
	}
	if f.ranWith("lock", "rm", holder) {
		t.Fatal("billet removed a live golden-image publisher at the cache recovery bound")
	}
}

func TestCacheMatchUsesExactThenNewestWithinTheFirstRestorePrefix(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	pointers := []cachePointer{
		{Key: "scope/npm-linux-old", Generation: "g-old", PublishedAt: now.Add(-time.Hour)},
		{Key: "scope/npm-linux-new", Generation: "g-new", PublishedAt: now},
		{Key: "scope/npm-other", Generation: "g-other", PublishedAt: now.Add(time.Hour)},
	}
	f.metadata[c.cacheIndex()] = map[string]string{}
	for _, pointer := range pointers {
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("marshal pointer: %v", err)
		}
		f.metadata[c.cacheIndex()][pointerKey(pointer.Key)] = string(encoded)
	}

	key, generation, err := c.Match(t.Context(), "scope/missing",
		[]string{"scope/npm-linux-", "scope/npm-"})
	if err != nil || key != "scope/npm-linux-new" || generation != "g-new" {
		t.Fatalf("restore match = %q %q, %v", key, generation, err)
	}
	key, generation, err = c.Match(t.Context(), "scope/npm-linux-old", []string{"scope/npm-"})
	if err != nil || key != "scope/npm-linux-old" || generation != "g-old" {
		t.Fatalf("exact match = %q %q, %v", key, generation, err)
	}
}

// TestCacheLookupsNeverTakeTheIndexLock proves Match and Current read lock-free.
// A crashed holder that keeps heartbeating holds the cache-index lock past any
// stale-break, so a reader that took the lock would stall every job's every
// cache step for the full 12-minute recovery bound. The lookups must not touch
// the lock at all. The sentinel lockAddErr gives the assertion teeth: if a
// lookup ever attempts `lock add`, it fails fast with this error rather than
// returning the pointer, so reverting either lookup to withCacheLock turns the
// hits below into errors instead of a 12-minute hang.
func TestCacheLookupsNeverTakeTheIndexLock(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	f.lockCookie = "held-by-a-crashed-writer"
	f.locker = "client.9999"
	f.lockAddErr = errors.New("a cache lookup must not take the cache-index lock")

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	pointer := cachePointer{Key: "scope/npm", Generation: "g1", PublishedAt: now}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey("scope/npm"): string(encoded)}

	key, generation, err := c.Match(t.Context(), "scope/npm", nil)
	if err != nil || key != "scope/npm" || generation != "g1" {
		t.Fatalf("exact Match behind a held lock = %q %q, %v; want a lock-free hit", key, generation, err)
	}
	key, generation, err = c.Match(t.Context(), "scope/absent", []string{"scope/"})
	if err != nil || key != "scope/npm" || generation != "g1" {
		t.Fatalf("prefix Match behind a held lock = %q %q, %v; want a lock-free hit", key, generation, err)
	}
	generation, err = c.Current(t.Context(), "scope/npm")
	if err != nil || generation != "g1" {
		t.Fatalf("Current behind a held lock = %q, %v; want a lock-free hit", generation, err)
	}

	if f.lockAdds != 0 {
		t.Fatalf("cache lookups took the index lock %d time(s); reads must be lock-free", f.lockAdds)
	}
}

// TestCacheLookupsMissCleanlyIncludingAColdIndex proves the miss branches keep
// the wrapped ErrMiss identity the fail-open contract depends on, both against a
// populated index with no matching key and against a cold site whose
// .cache-index image does not exist yet — the case the removed lock used to
// paper over by lazily creating the image before every lookup.
func TestCacheLookupsMissCleanlyIncludingAColdIndex(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	f.metadata[c.cacheIndex()] = map[string]string{}

	if _, _, err := c.Match(t.Context(), "scope/absent", []string{"scope/"}); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Match with no entries = %v, want ErrMiss", err)
	}
	if _, err := c.Current(t.Context(), "scope/absent"); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Current with no entries = %v, want ErrMiss", err)
	}

	cold := newCacheFake()
	cc := cacheClient(t, cold)
	cold.missingImages = map[string]bool{cc.cacheIndex(): true}

	if _, _, err := cc.Match(t.Context(), "scope/absent", []string{"scope/"}); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Match on a cold index = %v, want ErrMiss", err)
	}
	if _, err := cc.Current(t.Context(), "scope/absent"); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Current on a cold index = %v, want ErrMiss", err)
	}
	// A lookup must observe the cold index, not create it: creating an RBD image
	// is a writer's job, and a read that did so would be a write side effect.
	if cold.ranWith("create") {
		t.Fatal("a cold-index lookup created the .cache-index image; reads must not create it")
	}
}

// TestCacheEvictFailsClosedWhenItCannotReadTheIndex proves the strict error
// handling in cacheIndexMetadata: if the index metadata list fails, Evict must
// surface the error, not proceed with an empty protected set and sweep every old
// unmapped cache image. (A merely-absent index is not this case — takeLock
// recreates it before the list — so the realistic failure is a metadata list
// error, e.g. a transient cluster fault.)
func TestCacheEvictFailsClosedWhenItCannotReadTheIndex(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	f.metaListErr = errors.New("rbd: error listing image metadata: (108) Cannot send after transport endpoint shutdown")
	// An old, unmapped generation image with no pointer or lease protecting it.
	// If the metadata error were softened to an empty view, Evict would compute
	// an empty protected set and delete this — so seeding it gives the no-removal
	// assertion something real to protect.
	f.images[c.cfg.CachePool+"/cache-g-1000000000-deadbeef"] = true

	err := c.Evict(t.Context(), 7*24*time.Hour)
	if err == nil {
		t.Fatal("Evict returned nil when it could not read the index; it must fail closed")
	}
	// An image removal is `rbd rm <handle>` or `rbd trash mv <handle>`; the
	// lock-release `lock rm` is not one, so match the verb specifically rather
	// than any call containing "rm".
	for _, call := range f.calls {
		if slices.Contains(call, "lock") {
			continue
		}
		if slices.Contains(call, "rm") || (slices.Contains(call, "trash") && slices.Contains(call, "mv")) {
			t.Fatalf("Evict removed an image (%v) while it could not read the index", call)
		}
	}
}

// TestCacheMatchReturnsTheRepublishedGenerationNotTheScanned proves Match hands
// back the CONFIRMED pointer's generation, not the one captured in the torn
// list. The hook rewrites the pointer to a new generation under the same key
// after the list is taken, so a fix that returned the scanned (now superseded)
// generation would be the exact spurious-hit this guards against.
func TestCacheMatchReturnsTheRepublishedGenerationNotTheScanned(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	scanned := cachePointer{Key: "scope/npm", Generation: "g-old", PublishedAt: now}
	encoded, err := json.Marshal(scanned)
	if err != nil {
		t.Fatalf("marshal scanned: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey("scope/npm"): string(encoded)}
	f.onMetaList = func() {
		republished := cachePointer{Key: "scope/npm", Generation: "g-republished", PublishedAt: now}
		enc, mErr := json.Marshal(republished)
		if mErr != nil {
			t.Errorf("marshal republished: %v", mErr)

			return
		}
		f.metadata[c.cacheIndex()][pointerKey("scope/npm")] = string(enc)
	}

	key, generation, err := c.Match(t.Context(), "scope/absent", []string{"scope/"})
	if err != nil || key != "scope/npm" || generation != "g-republished" {
		t.Fatalf("Match after a republish = %q %q, %v; want the confirmed g-republished", key, generation, err)
	}

	// The republished generation must come from a single atomic point-read, not
	// a second (still torn) list: prove there was exactly one image-meta list and
	// a get of the candidate's pointer key.
	lists := 0
	for _, call := range f.calls {
		if slices.Contains(call, "image-meta") && slices.Contains(call, "list") {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("Match issued %d image-meta list calls; the confirm must be a point-read, not a re-list", lists)
	}
	if !f.ranWith("image-meta", "get", pointerKey("scope/npm")) {
		t.Fatal("Match did not point-read the candidate's pointer key to confirm it")
	}
}

// TestCacheMatchDoesNotFallToALowerPrefixWhenTheFirstWasReaped pins the
// restore-prefix priority under the torn-list race: the first prefix matched a
// pointer in the listed view, so even though that pointer is reaped before the
// confirm, Match must miss rather than silently return a lower-priority prefix's
// cache the atomic read would never have chosen.
func TestCacheMatchDoesNotFallToALowerPrefixWhenTheFirstWasReaped(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	specific := cachePointer{Key: "scope/npm-linux-1", Generation: "g-specific", PublishedAt: now}
	broad := cachePointer{Key: "scope/npm-2", Generation: "g-broad", PublishedAt: now}
	for _, pointer := range []cachePointer{specific, broad} {
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if f.metadata[c.cacheIndex()] == nil {
			f.metadata[c.cacheIndex()] = map[string]string{}
		}
		f.metadata[c.cacheIndex()][pointerKey(pointer.Key)] = string(encoded)
	}
	f.onMetaList = func() {
		delete(f.metadata[c.cacheIndex()], pointerKey("scope/npm-linux-1"))
	}

	key, generation, err := c.Match(t.Context(), "scope/absent",
		[]string{"scope/npm-linux-", "scope/npm-"})
	if !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Match = %q %q, %v; a reaped first-prefix candidate must miss, "+
			"not fall to the broader prefix's g-broad", key, generation, err)
	}
}

// TestCacheMatchConfirmsAPrefixCandidateAgainstAReapDuringTheList proves the
// prefix scan never returns a hit for a generation reaped while the paged
// image-meta list was being read. The hook deletes the candidate pointer after
// the list captured it, so the confirming point-read misses; Match must fall to
// ErrMiss rather than hand back a pointer to a generation that no longer exists
// (a spurious hit is the one direction the cache contract cannot take).
func TestCacheMatchConfirmsAPrefixCandidateAgainstAReapDuringTheList(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	pointer := cachePointer{Key: "scope/npm", Generation: "g-reaped", PublishedAt: now}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey("scope/npm"): string(encoded)}
	f.onMetaList = func() {
		delete(f.metadata[c.cacheIndex()], pointerKey("scope/npm"))
	}

	key, generation, err := c.Match(t.Context(), "scope/absent", []string{"scope/"})
	if !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("Match with a candidate reaped mid-list = %q %q, %v; want ErrMiss, "+
			"not a hit on a gone generation", key, generation, err)
	}
}

func TestACachePointerPublishesOnlyACompleteImmutableCandidate(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()

	volume, err := c.Create(t.Context(), "acme/api/npm", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lease, fence, err := c.acquireWriterAt(t.Context(), "acme/api/npm", "lease-17", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := c.publishCASAt(t.Context(), "acme/api/npm", "", candidate, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if err := c.publishCASAt(t.Context(), "acme/api/npm", "", candidate, lease, fence, now); err != nil {
		t.Fatalf("an ambiguously acknowledged PublishCAS could not be retried: %v", err)
	}

	clone, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone current: %v", err)
	}

	if clone.Generation != candidate.Generation {
		t.Errorf("clone generation = %q, want %q", clone.Generation, candidate.Generation)
	}

	if clone.Device == "" || clone.Lease.ID == "" {
		t.Errorf("clone is not a mapped, eviction-protected volume: %+v", clone)
	}

	if f.images[volume.Handle] {
		t.Error("the writable source clone survived after its immutable candidate was made")
	}
}

func TestPublishCASRefusesAStaleExpectedGenerationAndFence(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/go"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := c.publishCASAt(t.Context(), key, "generation-that-was-never-current",
		candidate, lease, fence, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("stale expected generation returned %v, want ErrConflict", err)
	}

	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence+1, now); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("unissued fence returned %v, want ErrConflict", err)
	}

	if _, err := c.Clone(t.Context(), key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("a refused publication changed the pointer: %v", err)
	}
}

func TestAnUntrustedCallerCanDiscardWithoutPublishing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)

	volume, err := c.Create(t.Context(), "acme/api/pr", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := c.Clone(t.Context(), volume.Key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("discarding a PR write published something: %v", err)
	}
}

func TestDiscardResolvesTheCurrentMappingInsteadOfTrustingAStoredDevice(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	const (
		handle        = "billet-cache/cache-v-lease-17"
		currentDevice = "/dev/rbd8"
		reusedDevice  = "/dev/rbd7"
	)
	f.images[handle] = true
	f.mappings[currentDevice] = handle
	f.mappings[reusedDevice] = "billet-cache/cache-v-another-job"

	volume := storecontract.Volume{Handle: handle, Device: reusedDevice}
	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, ok := f.mappings[currentDevice]; ok {
		t.Fatal("Discard left the volume's current mapping in place")
	}
	if got := f.mappings[reusedDevice]; got != "billet-cache/cache-v-another-job" {
		t.Fatalf("Discard touched a reused device: mapping = %q", got)
	}
	if f.images[handle] {
		t.Fatal("Discard left the cache image in place")
	}

	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("idempotent Discard: %v", err)
	}
}

func TestAPointerWriteFailureLeavesThePreviousPointerUntouched(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/cargo"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}

	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	f.failMeta = pointerKey(key)
	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence, now); err == nil {
		t.Fatal("PublishCAS succeeded although the pointer write failed")
	}

	if _, err := c.Clone(t.Context(), key, ""); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("the failed pointer became visible: %v", err)
	}

	if !f.images[candidate.Handle] {
		t.Error("the failed publication did not leave an orphan candidate for GC")
	}
}

func TestAGenerationRecordFailureLeavesThePreviousPointerUntouched(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now()
	key := "acme/api/cargo"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-1", time.Minute, now,
	)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, now)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, "", first, firstLease, firstFence, now,
	); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	secondVolume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	secondNow := now.Add(time.Second)
	secondLease, secondFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-2", time.Minute, secondNow,
	)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), secondVolume, secondNow)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}

	f.failMeta = generationKey(key, second.Generation)
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, secondNow,
	); err == nil {
		t.Fatal("PublishCAS succeeded although the generation record write failed")
	}

	current, err := c.Current(t.Context(), key)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != first.Generation {
		t.Fatalf("current generation = %q, want previous %q", current, first.Generation)
	}
	if !f.images[second.Handle] {
		t.Error("the failed publication did not leave an orphan candidate for GC")
	}
}

func TestEvictionPreservesAnOldGenerationWhileACloneLeaseIsLive(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", now.Add(-8*24*time.Hour).Unix())
	f.images[handle] = true
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	active := cacheActive{
		Key: "acme/api/npm", Handle: handle, Generation: "g1", Expires: now.Add(time.Hour),
	}
	encoded, err := json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal active lease: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{activeKey("lease-1"): string(encoded)}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict with a live clone: %v", err)
	}

	if !f.images[handle] {
		t.Fatal("eviction removed a generation a live clone lease still names")
	}

	active.Expires = now.Add(-time.Hour)
	encoded, err = json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal expired lease: %v", err)
	}
	f.metadata[c.cacheIndex()][activeKey("lease-1")] = string(encoded)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict after the clone expired: %v", err)
	}

	if f.images[handle] {
		t.Fatal("eviction kept an inactive generation beyond its retention window")
	}
}

func TestALiveClonePreservesItsOldPointerThroughPublication(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/long-publication"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(t.Context(), key, "first", time.Hour, old)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, old)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(t.Context(), key, "", first, firstLease, firstFence, old); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	writable, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	var pointer cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey(key)]), &pointer); err != nil {
		t.Fatalf("decode pointer: %v", err)
	}
	pointer.UsedAt = old
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("encode old pointer: %v", err)
	}
	f.metadata[c.cacheIndex()][pointerKey(key)] = string(encoded)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict while clone is live: %v", err)
	}
	if current, err := c.Current(t.Context(), key); err != nil || current != first.Generation {
		t.Fatalf("Current after eviction = %q, %v; want %q", current, err, first.Generation)
	}

	secondLease, secondFence, err := c.acquireWriterAt(t.Context(), key, "second", time.Hour, now)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), writable, now)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}
	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict between snapshot and publication: %v", err)
	}
	if current, err := c.Current(t.Context(), key); err != nil || current != first.Generation {
		t.Fatalf("Current during publication handoff = %q, %v; want %q", current, err, first.Generation)
	}
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, now,
	); err != nil {
		t.Fatalf("PublishCAS second after eviction: %v", err)
	}
}

func TestEvictionReclaimsTheRetiredWriterAfterItsGeneration(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	key := "acme/api/npm"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease, fence, err := c.acquireWriterAt(t.Context(), key, "lease-1", time.Minute, now)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := c.snapshotAt(t.Context(), volume, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(f.trash) != 1 {
		t.Fatalf("retired writer count = %d, want 1", len(f.trash))
	}
	if err := c.publishCASAt(t.Context(), key, "", candidate, lease, fence, now); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict with a live candidate: %v", err)
	}
	if !f.images[candidate.Handle] || len(f.trash) != 1 {
		t.Fatal("eviction removed a current generation or its retired copy-on-write parent")
	}

	old := now.Add(-8 * 24 * time.Hour)
	var pointer cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey(key)]), &pointer); err != nil {
		t.Fatalf("decode current pointer: %v", err)
	}
	pointer.UsedAt = old
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("encode expired pointer: %v", err)
	}
	f.metadata[c.cacheIndex()][pointerKey(key)] = string(encoded)
	f.metadata[c.cacheIndex()][generationKey(key, candidate.Generation)] = string(encoded)
	f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if f.images[candidate.Handle] {
		t.Fatal("eviction kept the expired generation")
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left %d retired writers after their children were removed", len(f.trash))
	}
}

func TestEvictionEventuallyReclaimsAMultiGenerationCloneChain(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/npm-chain"

	firstVolume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstLease, firstFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-1", time.Minute, now,
	)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	first, err := c.snapshotAt(t.Context(), firstVolume, now)
	if err != nil {
		t.Fatalf("Snapshot first: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, "", first, firstLease, firstFence, now,
	); err != nil {
		t.Fatalf("PublishCAS first: %v", err)
	}

	secondVolume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone first: %v", err)
	}
	secondNow := now.Add(time.Second)
	secondLease, secondFence, err := c.acquireWriterAt(
		t.Context(), key, "lease-2", time.Minute, secondNow,
	)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}
	second, err := c.snapshotAt(t.Context(), secondVolume, secondNow)
	if err != nil {
		t.Fatalf("Snapshot second: %v", err)
	}
	if err := c.publishCASAt(
		t.Context(), key, first.Generation, second, secondLease, secondFence, secondNow,
	); err != nil {
		t.Fatalf("PublishCAS second: %v", err)
	}

	for _, candidate := range []storecontract.Candidate{first, second} {
		f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)
	}
	for metadataKey, value := range f.metadata[c.cacheIndex()] {
		if !strings.HasPrefix(metadataKey, cacheMetaPrefix+"pointer.") &&
			!strings.HasPrefix(metadataKey, cacheMetaPrefix+"generation.") {
			continue
		}

		var pointer cachePointer
		if err := json.Unmarshal([]byte(value), &pointer); err != nil {
			t.Fatalf("decode %s: %v", metadataKey, err)
		}
		pointer.UsedAt = old
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("encode %s: %v", metadataKey, err)
		}
		f.metadata[c.cacheIndex()][metadataKey] = string(encoded)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for image := range f.images {
		if strings.Contains(image, "/cache-g-") || strings.Contains(image, "/cache-v-") {
			t.Fatalf("eviction left a cache image after the clone chain expired: %s", image)
		}
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left retired writers after the clone chain expired: %v", f.trash)
	}
	if _, ok := f.metadata[c.cacheIndex()][generationKey(key, first.Generation)]; ok {
		t.Fatal("eviction left metadata for the reclaimed parent generation")
	}
}

func TestLineageCompactionLetsEvictionReclaimHistoryBehindAnActiveCache(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	key := "acme/api/active-chain"
	var previous string
	var generations []storecontract.Candidate

	for i := range 5 {
		var volume storecontract.Volume
		var err error
		if i == 0 {
			volume, err = c.Create(t.Context(), key, 1<<30)
		} else {
			volume, err = c.Clone(t.Context(), key, "")
		}
		if err != nil {
			t.Fatalf("generation %d volume: %v", i+1, err)
		}

		commitTime := now.Add(time.Duration(i) * time.Second)
		lease, fence, err := c.acquireWriterAt(
			t.Context(), key, fmt.Sprintf("lease-%d", i+1), time.Minute, commitTime,
		)
		if err != nil {
			t.Fatalf("generation %d AcquireWriter: %v", i+1, err)
		}
		candidate, err := c.snapshotAt(t.Context(), volume, commitTime)
		if err != nil {
			t.Fatalf("generation %d Snapshot: %v", i+1, err)
		}
		if err := c.publishCASAt(
			t.Context(), key, previous, candidate, lease, fence, commitTime,
		); err != nil {
			t.Fatalf("generation %d PublishCAS: %v", i+1, err)
		}
		generations = append(generations, candidate)
		previous = candidate.Generation
	}

	current := generations[len(generations)-1]
	if parent := f.parents[current.Handle]; parent != "" {
		t.Fatalf("the compacted current generation still has parent %q", parent)
	}
	if got := f.metadata[current.Handle][cacheMetaPrefix+"lineage"]; got != "0" {
		t.Fatalf("compacted lineage depth = %q, want 0", got)
	}
	cpCalls := 0
	for _, call := range f.calls {
		if slices.Contains(call, "cp") {
			cpCalls++
		}
	}
	if cpCalls != 1 {
		t.Fatalf("independent copy count = %d, want one bounded compaction", cpCalls)
	}

	for _, candidate := range generations[:len(generations)-1] {
		f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = old.Format(time.RFC3339Nano)
	}
	for metadataKey, value := range f.metadata[c.cacheIndex()] {
		if !strings.HasPrefix(metadataKey, cacheMetaPrefix+"generation.") {
			continue
		}

		var pointer cachePointer
		if err := json.Unmarshal([]byte(value), &pointer); err != nil {
			t.Fatalf("decode %s: %v", metadataKey, err)
		}
		if pointer.Generation == current.Generation {
			continue
		}
		pointer.UsedAt = old
		encoded, err := json.Marshal(pointer)
		if err != nil {
			t.Fatalf("encode %s: %v", metadataKey, err)
		}
		f.metadata[c.cacheIndex()][metadataKey] = string(encoded)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if !f.images[current.Handle] {
		t.Fatal("eviction removed the active current generation")
	}
	for _, candidate := range generations[:len(generations)-1] {
		if f.images[candidate.Handle] {
			t.Fatalf("eviction kept expired ancestor %s behind an independent current generation",
				candidate.Handle)
		}
	}
	if len(f.trash) != 0 {
		t.Fatalf("eviction left retired writers behind the active cache: %v", f.trash)
	}
}

func TestCloningALegacyGenerationDoesNotCrossTheDepthLimit(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	key := "acme/api/legacy-depth"
	source := "billet-cache/cache-g-1-legacy"
	pointer := cachePointer{
		Generation: "g1", Handle: source, UsedAt: time.Now().UTC(), RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.images[source] = true
	f.snapshots[source+"@g1"] = true
	f.depths[source] = maxCacheCloneDepth
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey(key): string(encoded)}

	volume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone legacy generation: %v", err)
	}
	if parent := f.parents[volume.Handle]; parent != "" {
		t.Fatalf("legacy writable clone still has parent %q", parent)
	}
	if got := f.depths[volume.Handle]; got != 0 {
		t.Fatalf("legacy writable clone depth = %d, want independent depth 0", got)
	}
	if got := f.metadata[volume.Handle][cacheMetaPrefix+"lineage"]; got != "0" {
		t.Fatalf("legacy writable lineage metadata = %q, want 0", got)
	}
	cpCalls := 0
	for _, call := range f.calls {
		if slices.Contains(call, "cp") {
			cpCalls++
		}
	}
	if cpCalls != 1 {
		t.Fatalf("legacy independent copy count = %d, want 1", cpCalls)
	}
}

func TestCanceledCloneCleanupRemovesThePartialImage(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	handle := "billet-cache/cache-v-1-partial"
	leaseID := "cache-a-1-active"
	f.images[handle] = true
	f.mappings["/dev/rbd9"] = handle
	f.metadata[c.cacheIndex()] = map[string]string{activeKey(leaseID): `{"key":"legacy"}`}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	primary := errors.New("copy canceled")

	err := c.cleanupCloneFailure(ctx, leaseID, handle, primary)
	if !errors.Is(err, primary) {
		t.Fatalf("cleanup error = %v, want the primary failure", err)
	}
	if f.images[handle] {
		t.Fatal("canceled clone cleanup left the partial image")
	}
	if _, ok := f.mappings["/dev/rbd9"]; ok {
		t.Fatal("canceled clone cleanup left the partial image mapped")
	}
	if _, ok := f.metadata[c.cacheIndex()][activeKey(leaseID)]; ok {
		t.Fatal("canceled clone cleanup left the active-clone record")
	}
}

func TestSnapshotRefreshesALongLivedWritableCloneBeforeUnmapping(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-v-%d-long-job", old.Unix())
	f.images[handle] = true
	f.depths[handle] = 0
	f.mappings["/dev/rbd7"] = handle
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": old.Format(time.RFC3339Nano),
	}
	volume := storecontract.Volume{
		Key: "acme/api/long-job", Handle: handle, Device: "/dev/rbd7",
	}

	if _, err := c.snapshotAt(t.Context(), volume, now); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	finished := time.Now().UTC()
	refreshed, err := time.Parse(time.RFC3339Nano, f.metadata[handle][cacheMetaPrefix+"used_at"])
	if err != nil {
		t.Fatalf("parse refreshed use time: %v", err)
	}
	if refreshed.Before(now) || refreshed.After(finished) {
		t.Fatalf("writable use time = %s, want a post-lock time between %s and %s",
			refreshed, now, finished)
	}

	refreshCall := -1
	unmapCall := -1
	for i, call := range f.calls {
		if slices.Contains(call, "image-meta") && slices.Contains(call, handle) &&
			strings.Contains(strings.Join(call, "\x00"), cacheMetaPrefix+"used_at") {
			refreshCall = i
		}
		if slices.Contains(call, "unmap") {
			unmapCall = i
		}
	}
	if refreshCall < 0 || unmapCall < 0 || refreshCall >= unmapCall {
		t.Fatalf("writable refresh call %d did not precede unmap call %d", refreshCall, unmapCall)
	}
}

func TestCleanupFailureDoesNotRemainClassifiedAsAColdMiss(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	key := "acme/api/missing-with-orphan"
	source := "billet-cache/cache-g-1-missing"
	pointer := cachePointer{
		Generation: "g1", Handle: source, UsedAt: time.Now().UTC(), RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey(key): string(encoded)}
	f.metadata[source] = map[string]string{cacheMetaPrefix + "lineage": "0"}
	f.failRemove = true

	_, err = c.Clone(t.Context(), key, "")
	if err == nil {
		t.Fatal("Clone succeeded after source and cleanup failures")
	}
	if errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("cleanup failure remained classified as a replaceable cache miss: %v", err)
	}
	if !strings.Contains(err.Error(), "injected remove failure") {
		t.Fatalf("cleanup failure was lost: %v", err)
	}
}

func TestEvictionExpiresAnInactiveCurrentPointer(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	now := time.Now().UTC()
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", now.Add(-8*24*time.Hour).Unix())
	f.images[handle] = true
	pointer := cachePointer{
		Generation: "g1", Handle: handle, UsedAt: now.Add(-8 * 24 * time.Hour),
		RetentionHours: 7 * 24,
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{
		pointerKey("acme/api/npm"):          string(encoded),
		generationKey("acme/api/npm", "g1"): string(encoded),
	}
	f.metadata[handle] = map[string]string{
		cacheMetaPrefix + "used_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano),
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if f.images[handle] {
		t.Fatal("eviction kept an inactive current generation forever")
	}
	if _, present := f.metadata[c.cacheIndex()][pointerKey("acme/api/npm")]; present {
		t.Fatal("eviction left a pointer to the generation it removed")
	}
	if _, present := f.metadata[c.cacheIndex()][generationKey("acme/api/npm", "g1")]; present {
		t.Fatal("eviction left generation metadata for the image it removed")
	}
}

func TestCloningRefreshesTheCurrentPointersLastUse(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	handle := fmt.Sprintf("billet-cache/cache-g-%d-abcdef", old.Unix())
	f.images[handle] = true
	f.snapshots[handle+"@g1"] = true
	pointer := cachePointer{Generation: "g1", Handle: handle, UsedAt: old, RetentionHours: 7 * 24}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("marshal pointer: %v", err)
	}
	f.metadata[c.cacheIndex()] = map[string]string{pointerKey("acme/api/npm"): string(encoded)}
	f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": old.Format(time.RFC3339Nano)}

	volume, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := c.Discard(t.Context(), volume); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	var refreshed cachePointer
	if err := json.Unmarshal([]byte(f.metadata[c.cacheIndex()][pointerKey("acme/api/npm")]),
		&refreshed); err != nil {
		t.Fatalf("decode refreshed pointer: %v", err)
	}
	if !refreshed.UsedAt.After(old) {
		t.Errorf("pointer use stayed at %s", refreshed.UsedAt)
	}
}
