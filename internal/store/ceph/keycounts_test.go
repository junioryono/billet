package ceph

import (
	"encoding/json"
	"testing"
	"time"
)

// EACH KEY'S GENERATIONS ARE COUNTED FROM THE INDEX: only those still in the pool,
// each once, most first. A generation is superseded only where the key's pointer
// names another; one two keys share counts once in the pool; a record used more
// recently than its pointer gives the key's last use; and an unreadable record is
// counted as unreadable.
func TestEachKeysGenerationsAreCountedFromTheIndex(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	index := c.cacheIndex()
	f.metadata[index] = map[string]string{}

	now := time.Now().UTC().Truncate(time.Second)

	record := func(name string, p cachePointer) {
		t.Helper()

		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}

		f.metadata[index][name] = string(encoded)
	}

	gens := map[string]string{}
	for i, nonce := range []byte{'a', 'b', 'c', 'd', 'x', 'y'} {
		gens[string(nonce)] = expiredGeneration(now.Add(time.Duration(i)*time.Minute), nonce)
	}

	for _, present := range []string{"a", "b", "c", "x", "y"} {
		f.addVolume(gens[present])
	}

	const (
		goKey     = "local.scoped/untrusted/o/r/any/go"
		npmKey    = "local.scoped/trusted/o/r/main/npm"
		aliasKey  = "local.alias/trusted/o/r/main/npm"
		unpointed = "zz/first-publication-stopped"
	)

	record(pointerKey(goKey), cachePointer{Key: goKey, Handle: gens["c"], UsedAt: now.Add(-time.Hour)})
	record(generationKey(goKey, gens["a"]), cachePointer{Key: goKey, Handle: gens["a"], UsedAt: now.Add(-3 * time.Hour)})
	record(generationKey(goKey, gens["b"]), cachePointer{Key: goKey, Handle: gens["b"], UsedAt: now.Add(-2 * time.Hour)})
	// A RESTORE OF A NAMED GENERATION refreshes its record alone, after the pointer.
	record(generationKey(goKey, gens["c"]), cachePointer{Key: goKey, Handle: gens["c"], UsedAt: now})
	// EVICTED: its image is gone and its record not yet.
	record(generationKey(goKey, gens["d"]), cachePointer{Key: goKey, Handle: gens["d"], UsedAt: now})
	// THE SAME GENERATION RECORDED TWICE, under another record name, counts once.
	record(cacheMetaPrefix+"generation.duplicate", cachePointer{Key: goKey, Handle: gens["a"]})

	// ONE GENERATION, TWO KEYS: once in each row, once in the pool.
	record(pointerKey(npmKey), cachePointer{Key: npmKey, Handle: gens["x"], UsedAt: now.Add(-3 * time.Hour)})
	record(generationKey(npmKey, gens["x"]), cachePointer{Key: npmKey, Handle: gens["x"]})
	record(pointerKey(aliasKey), cachePointer{Key: aliasKey, Handle: gens["x"], UsedAt: now.Add(-4 * time.Hour)})
	record(generationKey(aliasKey, gens["x"]), cachePointer{Key: aliasKey, Handle: gens["x"]})

	// A FIRST PUBLICATION STOPPED BETWEEN ITS RECORD AND ITS POINTER supersedes nothing.
	record(generationKey(unpointed, gens["y"]), cachePointer{Key: unpointed, Handle: gens["y"]})

	f.metadata[index][cacheMetaPrefix+"generation.unreadable"] = "{not json"
	f.metadata[index][cacheMetaPrefix+"pointer.unreadable"] = `{"key":"no-handle"}`
	f.metadata[index][activeKey("job-1")] = `{"handle":"` + gens["a"] + `"}`

	got, err := c.KeyGenerations(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if got.Generations != 5 || got.Unreadable != 2 {
		t.Errorf("the pool holds %d generations with %d unreadable records, want 5 and 2", got.Generations, got.Unreadable)
	}

	want := []KeyGenerations{
		{Key: goKey, Generations: 3, Superseded: 2, Current: gens["c"], LastUsed: now},
		{Key: aliasKey, Generations: 1, Superseded: 0, Current: gens["x"], LastUsed: now.Add(-4 * time.Hour)},
		{Key: npmKey, Generations: 1, Superseded: 0, Current: gens["x"], LastUsed: now.Add(-3 * time.Hour)},
		{Key: unpointed, Generations: 1, Superseded: 0},
	}

	if len(got.Keys) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(got.Keys), len(want), got.Keys)
	}

	for i := range want {
		if got.Keys[i] != want[i] {
			t.Errorf("key %d = %+v, want %+v", i, got.Keys[i], want[i])
		}
	}
}

// AN INDEX THAT CANNOT BE READ IS AN ERROR, never a pool with no keys.
func TestAnUnreadableIndexCountsNothing(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := evictingClient(t, f, f.run)
	f.missingImages = map[string]bool{c.cacheIndex(): true}

	got, err := c.KeyGenerations(t.Context())
	if err == nil {
		t.Fatalf("an unreadable index answered %+v", got)
	}
}
