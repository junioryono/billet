package ceph

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// countCalls counts the rbd calls that carry every fragment.
func (f *cacheFake) countCalls(fragments ...string) int {
	n := 0

	for _, call := range f.calls {
		if !slices.ContainsFunc(fragments, func(fragment string) bool { return !slices.Contains(call, fragment) }) {
			n++
		}
	}

	return n
}

// A TRASH ENTRY A CHILD HOLDS IS ASKED ABOUT AGAIN ONLY ONCE IT CAN HAVE CHANGED:
// after this client removed a cache image, or after trashHeldRecheck. Asking
// every pass forked about 1,400 processes a second on the reference deployment
// (2026-10-12) for 1,364 entries that could not be deleted.
func TestAHeldTrashEntryIsAskedAboutAgainOnlyWhenItCanHaveChanged(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)

	clock := time.Now()
	c.clock = func() time.Time { return clock }

	f.trash["id-held"] = "billet-held"
	f.parents["billet-cache/child"] = "billet-cache/billet-held@g1"
	f.trash["id-timed"] = "billet-timed"
	f.parents["billet-cache/other-child"] = "billet-cache/billet-timed@g1"

	asks := func(id string) int { return f.countCalls("trash", "rm", "billet-cache/"+id) }

	if n, err := c.PurgeTrash(t.Context()); n != 0 || err != nil {
		t.Fatalf("first pass: PurgeTrash = %d, %v", n, err)
	}

	if asks("id-held") != 1 || asks("id-timed") != 1 {
		t.Fatalf("the first pass asked %d and %d times, want once each", asks("id-held"), asks("id-timed"))
	}

	// FORWARD ONLY, by a step well inside the recheck, so a recheck of zero fails.
	clock = clock.Add(10 * time.Minute)

	if n, err := c.PurgeTrash(t.Context()); n != 0 || err != nil {
		t.Fatalf("second pass: PurgeTrash = %d, %v", n, err)
	}

	if asks("id-held") != 1 || asks("id-timed") != 1 {
		t.Fatal("a held entry was asked about again with nothing removed and inside the recheck")
	}

	// THE CHILD LEAVES, so the entry it held is due at once, and so is every other.
	if err := c.removeCacheImage(t.Context(), "billet-cache/child"); err != nil {
		t.Fatalf("removeCacheImage: %v", err)
	}

	if n, err := c.PurgeTrash(t.Context()); n != 1 || err != nil {
		t.Fatalf("after the child left: PurgeTrash = %d, %v, want the freed entry purged", n, err)
	}

	if _, ok := f.trash["id-held"]; ok {
		t.Error("the entry its child no longer held is still in the trash")
	}

	if asks("id-timed") != 2 {
		t.Fatalf("a removal did not make the other held entry due: asked %d times", asks("id-timed"))
	}

	// AND WITH NOTHING REMOVED, THE RECHECK MAKES IT DUE, for a child another node
	// removed.
	clock = clock.Add(10 * time.Minute)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}

	if asks("id-timed") != 2 {
		t.Fatal("a held entry was asked about again inside the recheck")
	}

	clock = clock.Add(trashHeldRecheck)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}

	if asks("id-timed") != 3 {
		t.Fatalf("a held entry was not asked about once the recheck passed: asked %d times", asks("id-timed"))
	}

	// AN ENTRY THE TRASH NO LONGER LISTS IS FORGOTTEN, so the memory is bounded by
	// the trash.
	delete(f.trash, "id-timed")

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}

	c.trashHeldMu.Lock()
	defer c.trashHeldMu.Unlock()

	if len(c.trashHeld) != 0 {
		t.Errorf("held entries the trash no longer lists were kept: %v", c.trashHeld)
	}
}

// A FAILURE THAT IS NOT A CHILD'S HOLD IS NOT REMEMBERED: it is asked about on
// the next pass, as before, and reported each time.
func TestATrashFailureThatIsNotAHoldIsAskedAboutEveryPass(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)

	f.trash["id-gone"] = "billet-gone"
	f.trashFailures = map[string]error{"billet-cache/id-gone": fmt.Errorf("exit status 5: %w",
		cacheExitError{code: 5, message: "rbd: error: input/output error"})}

	for pass := 1; pass <= 2; pass++ {
		if _, err := c.PurgeTrash(t.Context()); err == nil {
			t.Fatalf("pass %d: a failed deletion was not reported", pass)
		}

		if got := f.countCalls("trash", "rm", "billet-cache/id-gone"); got != pass {
			t.Fatalf("pass %d asked %d times in all, want %d", pass, got, pass)
		}
	}
}

// AN IMAGE FOUND OPENABLE IS ASKED ABOUT AGAIN ONLY AFTER openableRecheck, and
// one whose removal was cut short meanwhile is still finished: its first
// unopenable sighting is at most that late, and the proof still needs two.
func TestAnOpenableImageIsAskedAboutAgainOnlyAfterTheRecheck(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)

	clock := time.Now()
	c.clock = func() time.Time { return clock }

	image := fmt.Sprintf("billet-cache/cache-g-%d-0123456789abcdef01234567", clock.Add(-2*time.Hour).Unix())
	f.images[image] = true

	infos := func() int { return f.countCalls("info", image) }

	if _, err := c.PurgeTrash(t.Context()); err != nil || infos() != 1 {
		t.Fatalf("first pass: %v, %d infos, want 1", err, infos())
	}

	// FORWARD ONLY, by a step well inside the recheck, so a recheck of zero fails.
	clock = clock.Add(openableRecheck / 2)

	if _, err := c.PurgeTrash(t.Context()); err != nil || infos() != 1 {
		t.Fatalf("inside the recheck: %v, %d infos, want still 1", err, infos())
	}

	// CUT SHORT BETWEEN SIGHTINGS: no longer openable, still listed.
	delete(f.images, image)
	f.halfRemoved[image] = true

	clock = clock.Add(openableRecheck / 2)

	if n, err := c.PurgeTrash(t.Context()); n != 0 || err != nil || infos() != 2 {
		t.Fatalf("at the recheck: PurgeTrash = %d, %v, %d infos; want a first sighting and nothing removed",
			n, err, infos())
	}

	clock = clock.Add(halfRemovedRecheck)

	if n, err := c.PurgeTrash(t.Context()); n != 1 || err != nil || f.halfRemoved[image] {
		t.Fatalf("a removal cut short after an openable sighting was not finished: %d, %v", n, err)
	}

	// A FINISHED GENERATION MAY HAVE BEEN A HELD WRITER'S CHILD, so it counts as a
	// removal and makes held trash entries due.
	if got := c.cacheRemovals.Load(); got != 1 {
		t.Errorf("a finished half-removed image counted %d removals, want 1", got)
	}
}

// THE REMOVAL COUNT IS READ BEFORE THE ASK, so a generation removed while a
// `trash rm` is in flight makes the entry due on the next pass rather than an hour
// later.
func TestARemovalDuringTheAskMakesTheHeldEntryDue(t *testing.T) {
	t.Parallel()

	f := newCacheFake()

	var c *Client

	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "trash") && slices.Contains(args, "rm") {
			c.cacheRemovals.Add(1)
		}

		return f.run(ctx, bin, args)
	}

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	clock := time.Now()
	c.clock = func() time.Time { return clock }

	f.trash["id-held"] = "billet-held"
	f.parents["billet-cache/child"] = "billet-cache/billet-held@g1"

	for pass := 1; pass <= 2; pass++ {
		if _, err := c.PurgeTrash(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}

		if got := f.countCalls("trash", "rm", "billet-cache/id-held"); got != pass {
			t.Fatalf("after pass %d the entry was asked about %d times, want %d: a removal during the "+
				"ask did not make it due", pass, got, pass)
		}
	}
}

// halfRemovedFixture is a client whose clock the test steers, three old cache
// images named in order, and the fake behind them.
func halfRemovedFixture(t *testing.T, run func(f *cacheFake) runner) (*Client, *cacheFake, *time.Time, []string) {
	t.Helper()

	f := newCacheFake()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run(f)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	clock := time.Now()
	c.clock = func() time.Time { return clock }

	old := clock.Add(-2 * time.Hour).Unix()

	var images []string
	for i := range 3 {
		images = append(images, fmt.Sprintf("billet-cache/cache-v-%d-%024d", old, i))
	}

	return c, f, &clock, images
}

// A PASS CUT BY ITS BUDGET KEEPS WHAT IT KNEW OF THE IMAGES IT DID NOT REACH:
// one found openable is not asked about again before the recheck.
func TestABudgetCutKeepsTheOpenableSightings(t *testing.T) {
	t.Parallel()

	var clock *time.Time

	spend := false

	c, f, clockp, images := halfRemovedFixture(t, func(f *cacheFake) runner {
		return func(ctx context.Context, bin string, args []string) ([]byte, error) {
			if spend && slices.Contains(args, "info") {
				*clock = clock.Add(halfRemovedBudget)
			}

			return f.run(ctx, bin, args)
		}
	})
	clock = clockp

	for _, image := range images[1:] {
		f.images[image] = true
	}

	f.halfRemoved[images[0]] = true

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// THE SECOND PASS SPENDS ITS BUDGET ON THE FIRST IMAGE and stops before the
	// two it found openable.
	spend = true
	*clock = clock.Add(time.Minute)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	spend = false
	*clock = clock.Add(time.Minute)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("third pass: %v", err)
	}

	for _, image := range images[1:] {
		if got := f.countCalls("info", image); got != 1 {
			t.Errorf("%s was asked about %d times, want once: the budget cut forgot it was openable", image, got)
		}
	}
}

// A PASS CANCELLED AFTER ITS LISTING KEEPS WHAT IT KNEW TOO.
func TestACancelledPassKeepsTheOpenableSightings(t *testing.T) {
	t.Parallel()

	var cancel context.CancelFunc

	c, f, clock, images := halfRemovedFixture(t, func(f *cacheFake) runner {
		return func(ctx context.Context, bin string, args []string) ([]byte, error) {
			out, err := f.run(ctx, bin, args)
			if cancel != nil && slices.Contains(args, "ls") {
				cancel()
			}

			return out, err
		}
	})

	for _, image := range images {
		f.images[image] = true
	}

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	cancel = stop
	*clock = clock.Add(time.Minute)

	if _, err := c.PurgeTrash(ctx); err == nil {
		t.Fatal("a cancelled pass reported nothing")
	}

	cancel = nil
	*clock = clock.Add(time.Minute)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("third pass: %v", err)
	}

	for _, image := range images {
		if got := f.countCalls("info", image); got != 1 {
			t.Errorf("%s was asked about %d times, want once: the cancelled pass forgot it was openable", image, got)
		}
	}
}
