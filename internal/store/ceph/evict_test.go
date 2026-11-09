package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// evictingClient is cacheClient with a runner of the test's own in front of the
// fake, and the fake's sessions.
func evictingClient(t *testing.T, f *cacheFake, run runner) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(run), WithCacheSessions(f.readSessions))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// expiredGeneration names a generation the way cacheName does, at a given time.
func expiredGeneration(named time.Time, nonce byte) string {
	return fmt.Sprintf("billet-cache/cache-g-%d-%s", named.Unix(), strings.Repeat(string(nonce), 24))
}

// callIndex is the position of the first call carrying every fragment, or -1.
func (f *cacheFake) callIndex(fragments ...string) int {
	return slices.IndexFunc(f.calls, func(call []string) bool {
		return !slices.ContainsFunc(fragments, func(fragment string) bool {
			return !slices.Contains(call, fragment)
		})
	})
}

// trashDeletedUnderLock reports the first `trash rm` the fake ran while it held
// the cache lock, or "".
func trashDeletedUnderLock(f *cacheFake) string {
	if len(f.trashRemovedLocked) == 0 {
		return ""
	}

	return "trash rm " + f.trashRemovedLocked[0]
}

// AN EXPIRED WRITABLE VOLUME GOES TO THE TRASH, AFTER THE LOCK, AND A GENERATION
// IS REMOVED WITH `rbd rm`. The volume's `rbd rm` was cut short by the command's
// bound under the lock every writer waits on; `rbd trash mv` is metadata only and
// the purge deletes the data. Eviction deletes from the trash only between its
// rounds, with the lock released.
func TestEvictionTrashesAnExpiredVolumeAndRemovesAGenerationWithRm(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	volume := orphanVolume(old, 'a')
	generation := expiredGeneration(old, 'b')
	f.addVolume(volume)
	f.addVolume(generation)
	f.freshVolume(now)
	f.trash["id-discarded"] = strings.TrimPrefix(orphanVolume(old, 'c'), "billet-cache/")

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if f.images[volume] || !trashMoved(f, volume) || f.ranWith("rm", volume) {
		t.Errorf("the expired volume was not moved to the trash, or was removed with rbd rm")
	}

	if released, moved := f.callIndex("lock", "rm"), f.callIndex("trash", "mv", volume); released < 0 ||
		moved < released {
		t.Errorf("the volume was moved (call %d) before the cache lock was released (call %d)",
			moved, released)
	}

	if f.images[generation] || trashMoved(f, generation) || !f.ranWith("rm", generation) {
		t.Errorf("the expired generation was not removed with rbd rm")
	}

	if _, ok := f.trash["id-discarded"]; ok || !f.ranWith("trash", "rm") {
		t.Error("eviction's purge between rounds left the discard in the trash")
	}

	if call := trashDeletedUnderLock(f); call != "" {
		t.Errorf("eviction ran %q under the cache lock", call)
	}

	if n, err := c.PurgeTrash(t.Context()); err != nil || n != 1 || len(f.trash) != 0 {
		t.Errorf("PurgeTrash = %d, %v, trash %v; want the volume eviction moved there deleted",
			n, err, f.trash)
	}

	requireCacheImageRules(t, f.calls, "rm", "trash mv")
}

// A VOLUME ANOTHER NODE'S JOB HOLDS OPEN IS KEPT, AND SO IS ONE THIS NODE'S
// SESSIONS NAME, because `rbd trash mv` would move either. Sessions that cannot
// be read keep every volume and say so, and the generations are evicted anyway.
func TestEvictionKeepsAVolumeInUse(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	watched := orphanVolume(old, 'a')
	inSession := orphanVolume(old, 'b')
	orphan := orphanVolume(old, 'c')

	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	for _, image := range []string{watched, inSession, orphan} {
		f.addVolume(image)
	}
	f.freshVolume(now)
	f.watchers[watched] = 1
	f.inSession[strings.TrimPrefix(inSession, "billet-cache/")] = true

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	for _, kept := range []string{watched, inSession} {
		if !f.images[kept] || trashMoved(f, kept) || f.ranWith("rm", kept) {
			t.Errorf("eviction moved or removed %s, which is in use", kept)
		}
	}

	if !trashMoved(f, orphan) {
		t.Error("eviction kept the volume nothing holds, so the kept ones prove nothing")
	}

	unread := newCacheFake()
	uc := evictingClient(t, unread, unread.run)
	generation := expiredGeneration(old, 'd')
	unread.addVolume(orphan)
	unread.addVolume(generation)
	unread.freshVolume(now)
	unread.sessionsErr = errors.New("node: read cache custody entry 1 of 2: permission denied")

	if err := uc.Evict(t.Context(), 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("Evict = %v, want the unreadable sessions reported", err)
	}

	if !unread.images[orphan] || trashMoved(unread, orphan) || unread.ranWith("rm", orphan) {
		t.Error("eviction moved a volume without reading the node's sessions")
	}

	if unread.images[generation] {
		t.Error("unreadable sessions stopped the generations' eviction")
	}
}

// ONE IMAGE THAT FAILS DOES NOT HOLD BACK THE REST. Each failure is reported and
// the pass goes on to the next generation and the next volume.
func TestAnEvictionFailureOnOneImageDoesNotStopThePass(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	stuckGeneration := expiredGeneration(old, '1')
	generation := expiredGeneration(old.Add(time.Second), '2')
	stuckVolume := orphanVolume(old, '3')
	volume := orphanVolume(old.Add(time.Second), '4')

	f := newCacheFake()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		switch {
		case slices.Contains(args, "rm") && slices.Contains(args, stuckGeneration):
			f.calls = append(f.calls, slices.Clone(args))

			return nil, errors.New("signal: killed")
		case slices.Contains(args, "status") && slices.Contains(args, stuckVolume):
			f.calls = append(f.calls, slices.Clone(args))

			return nil, errors.New("exit status 110: rbd: error: (110) Connection timed out")
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	for _, image := range []string{stuckGeneration, generation, stuckVolume, volume} {
		f.addVolume(image)
	}
	f.freshVolume(now)

	err := c.Evict(t.Context(), 7*24*time.Hour)
	for _, reported := range []string{stuckGeneration, stuckVolume} {
		if err == nil || !strings.Contains(err.Error(), reported) {
			t.Errorf("Evict = %v, want the failure on %s reported", err, reported)
		}
	}

	if !f.images[stuckGeneration] || !f.images[stuckVolume] || trashMoved(f, stuckVolume) {
		t.Error("an image whose removal or proof failed was not kept")
	}

	if f.images[generation] {
		t.Error("a failed generation removal stopped the generations after it")
	}

	if !trashMoved(f, volume) {
		t.Error("a volume that could not be judged stopped the volumes after it")
	}
}

// A GENERATION NEVER GOES TO THE TRASH. `rbd rm` refuses a generation a client on
// another node holds open, which this host's mappings cannot show, and that
// refusal is the guard; `rbd trash mv` would move it.
func TestEvictionNeverMovesAGenerationToTheTrash(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	held := expiredGeneration(old, 'a')
	free := expiredGeneration(old, 'b')

	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	f.addVolume(held)
	f.addVolume(free)
	f.freshVolume(now)
	f.watchers[held] = 1

	if err := c.Evict(t.Context(), 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "still has watchers") {
		t.Errorf("Evict = %v, want rbd's refusal of the held generation reported", err)
	}

	if !f.images[held] {
		t.Error("eviction removed a generation another node holds open")
	}

	if f.images[free] || !f.ranWith("rm", free) {
		t.Error("eviction kept an expired generation nothing holds")
	}

	for _, generation := range []string{held, free} {
		if trashMoved(f, generation) {
			t.Errorf("eviction moved %s to the trash", generation)
		}
	}
}

// THE GENERATIONS' PART HOLDS THE CACHE LOCK FOR A BOUNDED TIME. Every writer
// waits on that lock, and a failed removal no longer ends the pass, so a pass of
// slow removals stops at evictionLockBudget, says so, and the next pass goes on.
func TestEvictionHoldsTheCacheLockForABoundedTime(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	generations := []string{
		expiredGeneration(old, '1'), expiredGeneration(old, '2'), expiredGeneration(old, '3'),
	}

	f := newCacheFake()
	clock := time.Now()
	slow := true
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slow && slices.Contains(args, "rm") && !slices.Contains(args, "lock") {
			clock = clock.Add(evictionLockBudget / 2)
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	for _, generation := range generations {
		f.addVolume(generation)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "1 generation(s) from cache-g-") {
		t.Errorf("Evict = %v, want the generation left for the next pass reported", err)
	}

	if f.images[generations[0]] || f.images[generations[1]] {
		t.Error("eviction kept a generation inside its lock budget")
	}

	if !f.images[generations[2]] || f.ranWith("rm", generations[2]) {
		t.Fatal("eviction went on removing generations past its lock budget")
	}

	slow = false

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil || f.images[generations[2]] {
		t.Errorf("the next pass = %v and kept %v, want the waiting generation removed", err,
			f.images[generations[2]])
	}
}

// A ROUND WHOSE LAST GENERATION SPENDS THE BUDGET ENDS THE PASS, as one cut short
// does: it held the lock as long, and another round would hold it again.
func TestARoundThatSpendsItsBudgetOnItsLastGenerationEndsThePass(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	generations := []string{expiredGeneration(old, '1'), expiredGeneration(old, '2')}

	f := newCacheFake()
	clock := time.Now()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") && !slices.Contains(args, "lock") && !slices.Contains(args, "trash") {
			clock = clock.Add(evictionLockBudget / 2)
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	for _, generation := range generations {
		f.addVolume(generation)
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if f.images[generations[0]] || f.images[generations[1]] {
		t.Fatal("the round kept a generation inside its budget, so it proves nothing here")
	}
	if f.lockAdds != 1 {
		t.Errorf("eviction took the cache lock %d times, want one round", f.lockAdds)
	}
}

// AN EARLIER ROUND'S FAILURE IS REPORTED, though a later round succeeds: the later
// round has not retried what the earlier could not finish.
func TestAnEvictionRoundsFailureOutlivesALaterCleanRound(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	generation := expiredGeneration(old, 'a')

	f := newCacheFake()
	f.trash["id-stuck"] = strings.TrimPrefix(orphanVolume(old, 'b'), "billet-cache/")
	failed := false
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if !failed && slices.Contains(args, "trash") && slices.Contains(args, "billet-cache/id-stuck") {
			failed = true
			f.calls = append(f.calls, slices.Clone(args))

			return nil, errors.New("injected purge failure")
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	f.addVolume(generation)
	f.freshVolume(time.Now())

	err := c.Evict(t.Context(), 7*24*time.Hour)

	if f.images[generation] || !failed {
		t.Fatal("want the first purge to fail and its round to remove the generation, so a second runs")
	}
	if err == nil || !strings.Contains(err.Error(), "injected purge failure") {
		t.Errorf("Evict = %v, want the first round's purge failure reported", err)
	}
}

// A PASS CUT SHORT RESUMES WHERE IT STOPPED. Generations that fail slowly at the
// front of the order would otherwise spend every pass's budget, and the
// removable one behind them would never be reached.
func TestEvictionResumesAfterTheGenerationsThatSpentItsBudget(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	stuck := []string{expiredGeneration(old, '1'), expiredGeneration(old, '2')}
	behind := expiredGeneration(old, '3')

	f := newCacheFake()
	clock := time.Now()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") && (slices.Contains(args, stuck[0]) || slices.Contains(args, stuck[1])) {
			f.calls = append(f.calls, slices.Clone(args))
			clock = clock.Add(evictionLockBudget / 2)

			return nil, errors.New("signal: killed")
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	for _, generation := range append(slices.Clone(stuck), behind) {
		f.addVolume(generation)
	}

	for pass := range 2 {
		if err := c.Evict(t.Context(), 7*24*time.Hour); err == nil {
			t.Fatalf("pass %d reported no failure although two removals failed", pass+1)
		}
	}

	if f.images[behind] {
		t.Error("two passes never reached the generation behind the ones that spent the budget")
	}

	if !f.images[stuck[0]] || !f.images[stuck[1]] {
		t.Error("a generation whose removal failed was not kept")
	}
}

// THE BUDGET STARTS WHEN THE LOCK IS TAKEN, so the index records eviction
// removes before the generations spend it too.
func TestEvictionBudgetCoversTheIndexRecordsItRemoves(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	generation := expiredGeneration(old, 'a')

	f := newCacheFake()
	clock := time.Now()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "image-meta") && slices.Contains(args, "remove") {
			clock = clock.Add(evictionLockBudget / 2)
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	f.addVolume(generation)

	f.metadata[c.cacheIndex()] = map[string]string{}
	for i := range 3 {
		f.metadata[c.cacheIndex()][activeKey(fmt.Sprintf("lease-%d", i))] = "not json"
	}

	if err := c.Evict(t.Context(), 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "wait for the next eviction pass") {
		t.Errorf("Evict = %v, want the work past the budget reported", err)
	}

	if len(f.metadata[c.cacheIndex()]) != 1 || !f.images[generation] || f.ranWith("rm", generation) {
		t.Errorf("eviction went on past its budget: index %v, generation kept %v",
			f.metadata[c.cacheIndex()], f.images[generation])
	}
}

// ONE CAUSE CAN FAIL EVERY IMAGE ALIKE, so an eviction error names the first
// evictionFailuresReported failures and counts the rest; and why the pass
// stopped is never one of the failures counted away.
func TestEvictionReportsABoundedNumberOfFailures(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	attempted := evictionFailuresReported + 2

	f := newCacheFake()
	f.failRemove = true
	clock := time.Now()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") && !slices.Contains(args, "lock") {
			clock = clock.Add(evictionLockBudget / time.Duration(attempted))
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	for i := range attempted + 1 {
		f.addVolume(expiredGeneration(old.Add(time.Duration(i)*time.Second), 'a'))
	}

	err := c.Evict(t.Context(), 7*24*time.Hour)
	if err == nil {
		t.Fatal("Evict reported no failure although every removal failed")
	}

	if named := strings.Count(err.Error(), "injected remove failure"); named != evictionFailuresReported ||
		!strings.Contains(err.Error(), "and 2 more eviction failure(s)") {
		t.Errorf("Evict named %d failures in %v, want %d and the other 2 counted", named, err,
			evictionFailuresReported)
	}

	if !strings.Contains(err.Error(), "1 generation(s) from cache-g-") {
		t.Errorf("Evict = %v, want the generation left for the next pass named", err)
	}
}

// setIndexRecord writes one pointer or generation record into the cache index.
func setIndexRecord(t *testing.T, f *cacheFake, c *Client, metadataKey string, record cachePointer) {
	t.Helper()

	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode %s: %v", metadataKey, err)
	}
	if f.metadata[c.cacheIndex()] == nil {
		f.metadata[c.cacheIndex()] = map[string]string{}
	}
	f.metadata[c.cacheIndex()][metadataKey] = string(encoded)
}

// A SUPERSEDED GENERATION GOES supersededGrace AFTER ITS LAST USE, NOT THE KEY'S
// RETENTION (#799), and only on a positive reading: its record names its key and
// that key's pointer names another generation. Everything else, used two hours ago,
// is kept for the retention: a key's current generation, one a job holds, one with
// no record and one whose record names no key.
func TestASupersededGenerationLeavesAfterItsGraceNotItsRetention(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	twoHours := now.Add(-2 * time.Hour)
	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	f.freshVolume(now)

	const key, other = "acme/api/go", "acme/api/docker"
	current := expiredGeneration(twoHours, 'a')
	superseded := expiredGeneration(twoHours, 'b')
	recent := expiredGeneration(twoHours, 'c')
	held := expiredGeneration(twoHours, 'd')
	otherCurrent := expiredGeneration(twoHours, 'e')
	unrecorded := expiredGeneration(twoHours, 'f')
	keyless := expiredGeneration(twoHours, '0')
	pointerless := expiredGeneration(twoHours, '1')
	aliased := expiredGeneration(twoHours, '2')

	used := map[string]time.Time{
		current: now, superseded: twoHours, recent: now.Add(-supersededGrace / 2), held: twoHours,
		otherCurrent: twoHours, unrecorded: twoHours, keyless: twoHours, pointerless: twoHours,
		aliased: twoHours,
	}
	for handle, at := range used {
		f.addVolume(handle)
		f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": at.Format(time.RFC3339Nano)}
	}

	record := func(k, handle string, usedAt time.Time) cachePointer {
		return cachePointer{Key: k, Generation: "gen-" + handle, Handle: handle, UsedAt: usedAt,
			RetentionHours: 7 * 24}
	}
	setIndexRecord(t, f, c, pointerKey(key), record(key, current, now))
	setIndexRecord(t, f, c, pointerKey(other), record(other, otherCurrent, twoHours))
	for _, handle := range []string{current, superseded, recent, held} {
		setIndexRecord(t, f, c, generationKey(key, "gen-"+handle), record(key, handle, used[handle]))
	}
	setIndexRecord(t, f, c, generationKey(other, "gen-"+otherCurrent), record(other, otherCurrent, twoHours))
	setIndexRecord(t, f, c, generationKey(key, "gen-"+keyless), record("", keyless, twoHours))
	setIndexRecord(t, f, c, generationKey("acme/api/gone", "gen-"+pointerless),
		record("acme/api/gone", pointerless, twoHours))
	// TWO RECORDS NAME ONE GENERATION: one says superseded, the other cannot.
	setIndexRecord(t, f, c, generationKey(key, "gen-"+aliased), record(key, aliased, twoHours))
	setIndexRecord(t, f, c, generationKey("acme/api/gone", "gen-"+aliased),
		record("acme/api/gone", aliased, twoHours))

	active, err := json.Marshal(cacheActive{Key: key, Handle: held, Generation: "gen-" + held,
		Expires: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("encode active record: %v", err)
	}
	f.metadata[c.cacheIndex()][activeKey("lease-1")] = string(active)

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if f.images[superseded] {
		t.Error("eviction kept a superseded generation unused for longer than its grace")
	}
	if _, ok := f.metadata[c.cacheIndex()][generationKey(key, "gen-"+superseded)]; ok {
		t.Error("eviction left the removed generation's record")
	}

	for handle, why := range map[string]string{
		current:      "the key's current generation",
		recent:       "a superseded generation used inside its grace",
		held:         "a superseded generation a job's active record names",
		otherCurrent: "another key's current generation, inside its retention",
		unrecorded:   "a generation no record names",
		keyless:      "a generation whose record names no key",
		pointerless:  "a generation whose key has no pointer",
		aliased:      "a generation one of whose records cannot say it is superseded",
	} {
		if !f.images[handle] || f.ranWith("rm", handle) {
			t.Errorf("eviction removed %s (%s)", why, handle)
		}
	}
}

// ONE PASS UNWINDS A LINEAGE A COMPACTION CUT OFF. Each generation is parented
// through the writer it was published from, so the history behind a compacted
// current generation leaves a writer and a generation at a time, and the pass
// alternates the purge and a round of eviction until a round removes nothing,
// never deleting from the trash under the cache lock.
func TestOnePassUnwindsALineageACompactionCutOff(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	generations := cutOffLineage(t, f, c, "acme/api/go")
	current := generations[len(generations)-1]

	if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if !f.images[current.Handle] {
		t.Fatal("eviction removed the key's current generation")
	}
	for _, candidate := range generations[:len(generations)-1] {
		if f.images[candidate.Handle] {
			t.Errorf("one pass kept superseded ancestor %s behind a compacted current generation",
				candidate.Handle)
		}
	}
	if len(f.trash) != 0 {
		t.Errorf("one pass left retired writers behind a compacted current generation: %v", f.trash)
	}
	if call := trashDeletedUnderLock(f); call != "" {
		t.Errorf("eviction ran %q under the cache lock", call)
	}
}

// A LOCK WHOSE RELEASE FAILED MAY STILL BE HELD, and every writer waits on it, so
// the pass runs no further round and says why.
func TestAFailedLockReleaseEndsTheEvictionPass(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	generations := cutOffLineage(t, f, c, "acme/api/go")

	f.lockRemoveErrOn = f.lockRemoves + 1
	f.lockRemoveErr = errors.New("injected lock release failure")
	adds := f.lockAdds

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := c.Evict(ctx, 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "injected lock release failure") {
		t.Errorf("Evict = %v, want the failed release reported", err)
	}

	if f.lockAdds != adds+1 {
		t.Errorf("eviction asked for the cache lock %d time(s) after a release failed, want once",
			f.lockAdds-adds)
	}
	if f.images[generations[3].Handle] || !f.images[generations[2].Handle] {
		t.Error("want the one round before the failed release to have run, and no other")
	}
}

// THE LOCK IS LEFT FREE BETWEEN ROUNDS for longer than a waiting writer's retry,
// because nothing queues a writer and a purge with nothing to delete is quick.
func TestEvictionLeavesTheCacheLockFreeBetweenRounds(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newCacheFake()
		c := cacheClient(t, f)
		cutOffLineage(t, f, c, "acme/api/go")

		var released time.Time
		var gaps []time.Duration
		c.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
			out, err := f.run(ctx, bin, args)
			if slices.Contains(args, "lock") && err == nil {
				switch {
				case slices.Contains(args, "rm"):
					released = time.Now()
				case slices.Contains(args, "add") && !released.IsZero():
					gaps = append(gaps, time.Since(released))
				}
			}

			return out, err
		}

		if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil {
			t.Fatalf("Evict: %v", err)
		}

		if len(gaps) < 2 {
			t.Fatalf("eviction took the lock again %d time(s), want a round per link of the lineage",
				len(gaps))
		}
		for i, gap := range gaps {
			if gap < evictionRoundGap(c.cacheLockRetry) {
				t.Errorf("round %d took the lock %s after the last released it, want at least %s",
					i+2, gap, evictionRoundGap(c.cacheLockRetry))
			}
		}
	})
}

// cutOffLineage publishes five generations of one key, the fifth compacted, so the
// four before it are a lineage nothing current reads, each used two hours ago:
// past supersededGrace and far inside the retention. It returns the generations
// oldest first.
func cutOffLineage(t *testing.T, f *cacheFake, c *Client, key string) []storecontract.Candidate {
	t.Helper()

	now := time.Now().UTC()
	trashed := len(f.trash)
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
		lease, fence, err := c.acquireWriterAt(t.Context(), key, fmt.Sprintf("lease-%d", i+1), time.Minute,
			commitTime)
		if err != nil {
			t.Fatalf("generation %d AcquireWriter: %v", i+1, err)
		}
		candidate, err := c.snapshotAt(t.Context(), volume, commitTime)
		if err != nil {
			t.Fatalf("generation %d Snapshot: %v", i+1, err)
		}
		if err := c.publishCASAt(t.Context(), key, previous, candidate, lease, fence, commitTime); err != nil {
			t.Fatalf("generation %d PublishCAS: %v", i+1, err)
		}
		generations = append(generations, candidate)
		previous = candidate.Generation
	}

	current := generations[len(generations)-1]
	if parent := f.parents[current.Handle]; parent != "" {
		t.Fatalf("the current generation was not compacted: it still has parent %q", parent)
	}
	if len(f.trash)-trashed != len(generations) {
		t.Fatalf("the lineage retired %d writers to the trash, want %d", len(f.trash)-trashed, len(generations))
	}

	for _, candidate := range generations[:len(generations)-1] {
		f.metadata[candidate.Handle][cacheMetaPrefix+"used_at"] = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	}

	return generations
}

// A GENERATION WHOSE RECORD OUTLIVED WHAT IT NAMES IS A MISS. Eviction purges a
// generation's snapshot before its `rbd rm`, which a child refuses, and removes the
// image before the record, whose removal can fail; a clone of either answers
// ErrMiss under the lock, and records nothing that names it.
func TestCloningAGenerationWhoseSnapshotIsGoneIsAMiss(t *testing.T) {
	t.Parallel()

	for name, imagePresent := range map[string]bool{"snapshot purged": true, "image removed": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			c := cacheClient(t, f)
			key := "acme/api/go"
			handle := expiredGeneration(time.Now(), 'a')
			setIndexRecord(t, f, c, generationKey(key, "gen-a"), cachePointer{
				Key: key, Generation: "gen-a", Handle: handle, UsedAt: time.Now(), RetentionHours: 7 * 24,
			})
			if imagePresent {
				f.addVolume(handle)
				f.metadata[handle] = map[string]string{cacheMetaPrefix + "lineage": "1"}
			} else {
				f.missingImages = map[string]bool{handle: true}
			}
			before := maps.Clone(f.metadata[c.cacheIndex()])

			if _, err := c.Clone(t.Context(), key, "gen-a"); !errors.Is(err, storecontract.ErrMiss) {
				t.Fatalf("Clone = %v, want ErrMiss", err)
			}

			if !maps.Equal(f.metadata[c.cacheIndex()], before) {
				t.Errorf("a clone of a generation that is gone changed the index: %v",
					f.metadata[c.cacheIndex()])
			}
			if f.ranWith("clone") {
				t.Error("a clone was attempted outside the lock of a generation the lock could see was gone")
			}
		})
	}
}

// THE LOCK BUDGET IS THE PASS'S: rounds that each stay inside it still stop once
// together they have held the lock that long, release included, because a writer
// whose retry is in flight at every gap waits through all of them.
func TestTheEvictionPassHoldsTheLockForItsBudgetInAll(t *testing.T) {
	t.Parallel()

	// THE FIRST ROUND REMOVES BOTH LINEAGES' LEAVES and, with its release, spends
	// all but a quarter of the budget, which leaves the second round room for one of
	// the two generations it could otherwise remove.
	for name, cost := range map[string]struct{ removal, release time.Duration }{
		"removals":              {removal: evictionLockBudget * 3 / 8},
		"removals and releases": {removal: evictionLockBudget / 4, release: evictionLockBudget / 4},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			c := cacheClient(t, f)
			first := cutOffLineage(t, f, c, "acme/api/go")
			second := cutOffLineage(t, f, c, "acme/web/go")

			clock := time.Now()
			c.clock = func() time.Time { return clock }
			c.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
				out, err := f.run(ctx, bin, args)
				switch {
				case err != nil || !slices.Contains(args, "rm") || slices.Contains(args, "trash"):
				case slices.Contains(args, "lock"):
					clock = clock.Add(cost.release)
				default:
					clock = clock.Add(cost.removal)
				}

				return out, err
			}

			if err := c.Evict(t.Context(), 7*24*time.Hour); err != nil &&
				!strings.Contains(err.Error(), "wait for the next eviction pass") {
				t.Fatalf("Evict = %v, want no failure but the budget's stop", err)
			}

			if f.images[first[3].Handle] || f.images[second[3].Handle] {
				t.Fatal("the first round kept a lineage's leaf, inside the budget, so the test proves nothing")
			}
			if kept := btoi(f.images[first[2].Handle]) + btoi(f.images[second[2].Handle]); kept != 1 {
				t.Errorf("the second round kept %d of the two generations it could reach, want 1: the "+
					"rounds together hold the lock for the pass's budget, not each its own", kept)
			}
			for _, generation := range []storecontract.Candidate{first[0], first[1], second[0], second[1]} {
				if !f.images[generation.Handle] {
					t.Errorf("eviction removed %s after the pass's budget was spent", generation.Handle)
				}
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

// A FAILED RELEASE IS REPORTED HOWEVER MANY GENERATIONS FAILED: why a pass stopped
// is never one of the failures the cap counts away.
func TestAFailedReleaseIsReportedPastTheFailureCap(t *testing.T) {
	t.Parallel()

	old := time.Now().Add(-8 * 24 * time.Hour)
	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	for i := range evictionFailuresReported + 2 {
		f.addVolume(expiredGeneration(old.Add(time.Duration(i)*time.Second), 'a'))
	}
	f.failRemove = true
	f.lockRemoveErrOn = f.lockRemoves + 1
	f.lockRemoveErr = errors.New("injected lock release failure")

	err := c.Evict(t.Context(), 7*24*time.Hour)

	if err == nil || !strings.Contains(err.Error(), "injected lock release failure") ||
		!strings.Contains(err.Error(), "may still be held") {
		t.Errorf("Evict = %v, want the failed release reported by name", err)
	}
	if err != nil && !strings.Contains(err.Error(), "more eviction failure(s)") {
		t.Errorf("Evict = %v, want the cap reached, so it proves nothing otherwise", err)
	}
}

// THE SWEEP TAKES ONLY SUPERSEDED GENERATIONS, and does none of the full pass's
// work: an expired generation no key moved past, an expired active record and the
// full pass's resume point are all left for the full pass.
func TestTheSupersededSweepTakesOnlySupersededGenerations(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	twoHours := now.Add(-2 * time.Hour)
	old := now.Add(-8 * 24 * time.Hour)

	f := newCacheFake()
	c := evictingClient(t, f, f.run)

	const key = "acme/api/go"
	current := expiredGeneration(twoHours, 'a')
	superseded := expiredGeneration(twoHours, 'b')
	expired := expiredGeneration(old, 'c')

	for handle, at := range map[string]time.Time{current: now, superseded: twoHours, expired: old} {
		f.addVolume(handle)
		f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": at.Format(time.RFC3339Nano)}
	}

	record := func(handle string, usedAt time.Time) cachePointer {
		return cachePointer{Key: key, Generation: "gen-" + handle, Handle: handle, UsedAt: usedAt,
			RetentionHours: 7 * 24}
	}
	setIndexRecord(t, f, c, pointerKey(key), record(current, now))
	setIndexRecord(t, f, c, generationKey(key, "gen-"+current), record(current, now))
	setIndexRecord(t, f, c, generationKey(key, "gen-"+superseded), record(superseded, twoHours))
	f.metadata[c.cacheIndex()][activeKey("lease-gone")] = "not json"

	c.evictResume = "cache-g-0-resume"

	if err := c.EvictSuperseded(t.Context()); err != nil {
		t.Fatalf("EvictSuperseded: %v", err)
	}

	if f.images[superseded] {
		t.Error("the sweep kept a superseded generation past its grace")
	}
	if !f.images[current] || !f.images[expired] || f.ranWith("rm", expired) {
		t.Error("the sweep removed the key's current generation or an expired one no key moved past")
	}
	if _, ok := f.metadata[c.cacheIndex()][activeKey("lease-gone")]; !ok {
		t.Error("the sweep removed an expired active record, which is the full pass's bookkeeping")
	}
	if c.evictResume != "cache-g-0-resume" {
		t.Errorf("the sweep moved the full pass's resume point to %q", c.evictResume)
	}
}

// A SWEEP CUT SHORT RESUMES WHERE IT STOPPED, on a resume point of its own:
// superseded generations that fail slowly at the front of its order would
// otherwise spend every sweep's budget, and the one behind them would wait for the
// full pass.
func TestASweepCutShortResumesWhereItStopped(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	twoHours := now.Add(-2 * time.Hour)

	const key = "acme/api/go"
	current := expiredGeneration(now, 'a')
	stuck := []string{expiredGeneration(twoHours, '1'), expiredGeneration(twoHours.Add(time.Second), '2')}
	behind := expiredGeneration(twoHours.Add(2*time.Second), '3')

	f := newCacheFake()
	clock := time.Now()
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "rm") && (slices.Contains(args, stuck[0]) || slices.Contains(args, stuck[1])) {
			f.calls = append(f.calls, slices.Clone(args))
			clock = clock.Add(supersededSweepBudget / 2)

			return nil, errors.New("signal: killed")
		}

		return f.run(ctx, bin, args)
	}
	c := evictingClient(t, f, run)
	c.clock = func() time.Time { return clock }
	c.evictResume = "cache-g-0-resume"

	record := func(handle string, usedAt time.Time) cachePointer {
		return cachePointer{Key: key, Generation: "gen-" + handle, Handle: handle, UsedAt: usedAt,
			RetentionHours: 7 * 24}
	}
	f.addVolume(current)
	setIndexRecord(t, f, c, pointerKey(key), record(current, now))
	for _, handle := range append(slices.Clone(stuck), behind) {
		f.addVolume(handle)
		f.metadata[handle] = map[string]string{cacheMetaPrefix + "used_at": twoHours.Format(time.RFC3339Nano)}
		setIndexRecord(t, f, c, generationKey(key, "gen-"+handle), record(handle, twoHours))
	}

	for sweep := range 2 {
		if err := c.EvictSuperseded(t.Context()); err == nil {
			t.Fatalf("sweep %d reported no failure although two removals failed", sweep+1)
		}
	}

	if f.images[behind] {
		t.Error("two sweeps never reached the superseded generation behind the ones that spent the budget")
	}
	if !f.images[stuck[0]] || !f.images[stuck[1]] {
		t.Error("a generation whose removal failed was not kept")
	}
	if c.evictResume != "cache-g-0-resume" {
		t.Errorf("the sweep moved the full pass's resume point to %q", c.evictResume)
	}
}

// THE SWEEP UNWINDS A LINEAGE A COMPACTION CUT OFF, as the full pass does, purging
// the trash between its rounds and never under the lock.
func TestTheSupersededSweepUnwindsALineage(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	generations := cutOffLineage(t, f, c, "acme/api/go")

	if err := c.EvictSuperseded(t.Context()); err != nil {
		t.Fatalf("EvictSuperseded: %v", err)
	}

	if !f.images[generations[len(generations)-1].Handle] {
		t.Fatal("the sweep removed the key's current generation")
	}
	for _, candidate := range generations[:len(generations)-1] {
		if f.images[candidate.Handle] {
			t.Errorf("the sweep kept superseded ancestor %s", candidate.Handle)
		}
	}
	if len(f.trash) != 0 {
		t.Errorf("the sweep left retired writers: %v", f.trash)
	}
	if call := trashDeletedUnderLock(f); call != "" {
		t.Errorf("the sweep ran %q under the cache lock", call)
	}
}

// THE SWEEP HOLDS THE LOCK FOR ITS OWN BUDGET, its rounds together, which is what
// lets it run every few minutes beside writers.
func TestTheSupersededSweepHoldsTheLockForItsBudget(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	first := cutOffLineage(t, f, c, "acme/api/go")
	second := cutOffLineage(t, f, c, "acme/web/go")

	clock := time.Now()
	c.clock = func() time.Time { return clock }
	c.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		out, err := f.run(ctx, bin, args)
		if err == nil && slices.Contains(args, "rm") && !slices.Contains(args, "lock") &&
			!slices.Contains(args, "trash") {
			clock = clock.Add(supersededSweepBudget * 3 / 8)
		}

		return out, err
	}

	if err := c.EvictSuperseded(t.Context()); err != nil &&
		!strings.Contains(err.Error(), "wait for the next eviction pass") {
		t.Fatalf("EvictSuperseded = %v, want no failure but the budget's stop", err)
	}

	if f.images[first[3].Handle] || f.images[second[3].Handle] {
		t.Fatal("the first round kept a lineage's leaf, inside the budget, so the test proves nothing")
	}
	if kept := btoi(f.images[first[2].Handle]) + btoi(f.images[second[2].Handle]); kept != 1 {
		t.Errorf("the second round kept %d of the two generations it could reach, want 1", kept)
	}
	for _, generation := range []storecontract.Candidate{first[0], first[1], second[0], second[1]} {
		if !f.images[generation.Handle] {
			t.Errorf("the sweep removed %s after its budget was spent", generation.Handle)
		}
	}
}
