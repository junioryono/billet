package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// THE TRASH IS PURGED SEVERAL AT A TIME. One at a time ran at about the rate a
// busy node discards, and on 2026-10-03 a backlog of 950 did not shrink.
func TestTheTrashIsPurgedByTrashPurgeWorkersAtOnce(t *testing.T) {
	t.Parallel()

	const entries = 10

	var listing []cacheTrashImage
	for i := range entries {
		listing = append(listing, cacheTrashImage{ID: fmt.Sprintf("id%d", i), Name: fmt.Sprintf("billet-root%d", i)})
	}

	body, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
		removed  []string
	)

	run := func(ctx context.Context, _ string, args []string) ([]byte, error) {
		switch {
		case slices.Contains(args, "trash") && slices.Contains(args, "list"):
			return body, nil
		case slices.Contains(args, "trash") && slices.Contains(args, "rm"):
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()

			// LONG ENOUGH TO OVERLAP, so a serial purge cannot reach a peak above one.
			hold := time.NewTimer(200 * time.Millisecond)
			defer hold.Stop()

			select {
			case <-hold.C:
			case <-ctx.Done():
			}

			mu.Lock()
			inFlight--
			removed = append(removed, args[len(args)-1])
			mu.Unlock()

			return nil, nil
		case slices.Contains(args, "ls"):
			return []byte("[]"), nil
		default:
			return nil, nil
		}
	}

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(run), withPurgeWorkers(trashPurgeWorkers))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	n, err := c.PurgeTrash(t.Context())
	if err != nil {
		t.Fatalf("PurgeTrash: %v", err)
	}

	if n != entries || len(removed) != entries {
		t.Fatalf("purged %d, removed %v; want all %d", n, removed, entries)
	}
	// MORE THAN ONE AND NEVER MORE THAN THE WORKERS: an exact peak would ask the
	// scheduler to start all of them inside one hold.
	if peak < 2 || peak > trashPurgeWorkers {
		t.Fatalf("at most %d deletion(s) ran at once, want between 2 and %d", peak, trashPurgeWorkers)
	}
}

// A FAKE RUNNER IS SERIAL, so the tests whose fakes are not safe for concurrent
// calls never see two deletions at once.
func TestAFakeRunnerPurgesOneAtATime(t *testing.T) {
	t.Parallel()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(func(context.Context, string, []string) ([]byte, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if c.purgeWorkers != 1 {
		t.Fatalf("a client on a fake runner purges with %d workers, want 1", c.purgeWorkers)
	}

	production, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if production.purgeWorkers != trashPurgeWorkers {
		t.Fatalf("a client on the real runner purges with %d workers, want %d",
			production.purgeWorkers, trashPurgeWorkers)
	}
}
