package ceph

import (
	"errors"
	"strings"
	"testing"
)

// A TRASH ENTRY WHOSE MOVE WAS INTERRUPTED IS FINISHED AND DELETED. librbd refuses
// to remove an entry still marked moving, so one cache volume on ubuntu-01 was
// refused on every purge from 2026-11-08 to 2026-11-09, and every sweep reported
// it; asked to move the name the pool no longer holds, librbd completes the move.
func TestAnInterruptedTrashMoveIsFinishedAndDeleted(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	f.trashMoving = map[string]bool{"id-moving": true}
	f.trash["id-moving"] = "cache-v-1794148981-5ffb13c7646acb490f1ceff2"
	c := cacheClient(t, f)

	purged, err := c.PurgeTrash(t.Context())
	if err != nil || purged != 1 {
		t.Fatalf("PurgeTrash = %d, %v; want the interrupted move finished and deleted", purged, err)
	}
	if _, ok := f.trash["id-moving"]; ok {
		t.Fatal("the entry is still in the trash")
	}
	if f.countCalls("trash", "mv", "billet-cache/cache-v-1794148981-5ffb13c7646acb490f1ceff2") != 1 {
		t.Fatal("the interrupted move was not resumed by its name")
	}
}

// NOT WHILE THE POOL STILL HOLDS THE NAME: moving it would move that live image
// into the trash. The entry is left and said, and the live image is untouched.
func TestAnInterruptedMoveIsNotResumedWhileThePoolHoldsTheName(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	f.trashMoving = map[string]bool{"id-moving": true}
	f.trash["id-moving"] = "cache-v-1794148981-5ffb13c7646acb490f1ceff2"
	f.images["billet-cache/cache-v-1794148981-5ffb13c7646acb490f1ceff2"] = true
	c := cacheClient(t, f)

	_, err := c.PurgeTrash(t.Context())
	if err == nil || !strings.Contains(err.Error(), "still in the pool") {
		t.Fatalf("PurgeTrash = %v, want the live name refused", err)
	}
	if !f.images["billet-cache/cache-v-1794148981-5ffb13c7646acb490f1ceff2"] ||
		f.countCalls("trash", "mv") != 0 {
		t.Fatal("a live image of the same name was moved into the trash")
	}
}

// NOR WHEN THE POOL CANNOT SAY: could-not-tell finishes nothing.
func TestAnInterruptedMoveIsNotResumedWhenThePoolCannotSay(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	f.trashMoving = map[string]bool{"id-moving": true}
	f.trash["id-moving"] = "cache-v-1794148981-5ffb13c7646acb490f1ceff2"
	f.infoFailures = map[string]error{
		"billet-cache/cache-v-1794148981-5ffb13c7646acb490f1ceff2": errors.New("rbd: (110) Connection timed out"),
	}
	c := cacheClient(t, f)

	_, err := c.PurgeTrash(t.Context())
	if err == nil || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("PurgeTrash = %v, want could-not-tell", err)
	}
	if f.countCalls("trash", "mv") != 0 || !f.trashMoving["id-moving"] {
		t.Fatal("an interrupted move was resumed without proof the name is gone")
	}
}

// NOR AN IMAGE BILLET DID NOT NAME, which the purge never asks about at all.
func TestAnInterruptedMoveOfAnotherNameIsLeftAlone(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	f.trashMoving = map[string]bool{"id-theirs": true}
	f.trash["id-theirs"] = "somebody-elses-image"
	c := cacheClient(t, f)

	if _, err := c.PurgeTrash(t.Context()); err != nil {
		t.Fatalf("PurgeTrash = %v", err)
	}
	if f.countCalls("trash", "mv") != 0 || f.countCalls("trash", "rm") != 0 {
		t.Fatal("an image billet did not name was touched")
	}
}
