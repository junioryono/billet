package ceph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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

// AN EXPIRED WRITABLE VOLUME GOES TO THE TRASH, AFTER THE LOCK, AND A GENERATION
// IS REMOVED WITH `rbd rm`. The volume's `rbd rm` was cut short by the command's
// bound under the lock every writer waits on; `rbd trash mv` is metadata only and
// the purge deletes the data. Eviction still never deletes from the trash.
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

	if _, ok := f.trash["id-discarded"]; !ok || f.ranWith("trash", "rm") {
		t.Error("eviction deleted from the trash")
	}

	if n, err := c.PurgeTrash(t.Context()); err != nil || n != 2 || len(f.trash) != 0 {
		t.Errorf("PurgeTrash = %d, %v, trash %v; want the evicted volume and the discard deleted",
			n, err, f.trash)
	}
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
