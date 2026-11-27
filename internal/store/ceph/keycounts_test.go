package ceph

import (
	"encoding/json"
	"testing"
	"time"
)

// EACH KEY'S GENERATIONS ARE COUNTED FROM THE INDEX, only those still in the pool,
// each once, most first; a key's current one is named and the rest are superseded.
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
	for i, nonce := range []byte{'a', 'b', 'c', 'd', 'x'} {
		gens[string(nonce)] = expiredGeneration(now.Add(time.Duration(i)*time.Minute), nonce)
	}

	for _, present := range []string{"a", "b", "c", "x"} {
		f.addVolume(gens[present])
	}

	const goKey, npmKey = "local.scoped/untrusted/o/r/any/go", "local.scoped/trusted/o/r/main/npm"

	record(pointerKey(goKey), cachePointer{Key: goKey, Handle: gens["c"], UsedAt: now})
	record(generationKey(goKey, gens["a"]), cachePointer{Key: goKey, Handle: gens["a"], UsedAt: now.Add(-2 * time.Hour)})
	record(generationKey(goKey, gens["b"]), cachePointer{Key: goKey, Handle: gens["b"], UsedAt: now.Add(-time.Hour)})
	record(generationKey(goKey, gens["c"]), cachePointer{Key: goKey, Handle: gens["c"], UsedAt: now.Add(-time.Minute)})
	// EVICTED: its image is gone and its record not yet.
	record(generationKey(goKey, gens["d"]), cachePointer{Key: goKey, Handle: gens["d"], UsedAt: now})
	// THE SAME GENERATION RECORDED TWICE, under another record name, counts once.
	record(cacheMetaPrefix+"generation.duplicate", cachePointer{Key: goKey, Handle: gens["a"]})

	record(pointerKey(npmKey), cachePointer{Key: npmKey, Handle: gens["x"], UsedAt: now.Add(-3 * time.Hour)})
	record(generationKey(npmKey, gens["x"]), cachePointer{Key: npmKey, Handle: gens["x"]})

	f.metadata[index][cacheMetaPrefix+"generation.unreadable"] = "{not json"
	f.metadata[index][activeKey("job-1")] = `{"handle":"` + gens["a"] + `"}`

	got, err := c.KeyGenerations(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := []KeyGenerations{
		{Key: goKey, Generations: 3, Superseded: 2, Current: gens["c"], LastUsed: now},
		{Key: npmKey, Generations: 1, Superseded: 0, Current: gens["x"], LastUsed: now.Add(-3 * time.Hour)},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %+v, want %+v", i, got[i], want[i])
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
