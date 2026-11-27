package ceph

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// KeyGenerations is one cache key's generations in the pool, as the cache index
// records them.
type KeyGenerations struct {
	Key string
	// Generations is how many of the key's recorded generations are still images
	// in the pool; Superseded is how many of those its pointer no longer names.
	Generations int
	Superseded  int
	// Current is the generation the key's pointer names, "" when none does.
	Current string
	// LastUsed is the latest use any of its records gives.
	LastUsed time.Time
}

// KeyGenerations counts each key's generations from the cache index and the pool's
// listing, most generations first: two reads, whatever the pool holds.
//
// SAID BEFORE CEPH SAYS IT. On 2026-11-07 one key held 845 generations and most of a
// full pool (#799); eviction now bounds that, and this is how an operator sees a key
// that eviction cannot keep up with, or whose generations something still pins.
//
// A record whose image is gone is not counted: eviction removes the image before
// its record, and an interrupted pass leaves the record for the next.
func (c *Client) KeyGenerations(ctx context.Context) ([]KeyGenerations, error) {
	metadata, err := c.cacheIndexMetadata(ctx)
	if err != nil {
		return nil, err
	}

	images, err := c.cacheImages(ctx)
	if err != nil {
		return nil, err
	}

	present := make(map[string]bool, len(images))
	for _, name := range images {
		present[c.cfg.CachePool+"/"+name] = true
	}

	current := map[string]string{}
	byKey := map[string]*KeyGenerations{}
	seen := map[string]map[string]bool{}

	entry := func(key string) *KeyGenerations {
		if byKey[key] == nil {
			byKey[key] = &KeyGenerations{Key: key}
			seen[key] = map[string]bool{}
		}

		return byKey[key]
	}

	for name, value := range metadata {
		var record cachePointer

		switch {
		case strings.HasPrefix(name, cacheMetaPrefix+"pointer."):
			if json.Unmarshal([]byte(value), &record) != nil || record.Key == "" || record.Handle == "" {
				continue
			}

			current[record.Key] = record.Handle

			if e := entry(record.Key); record.UsedAt.After(e.LastUsed) {
				e.LastUsed = record.UsedAt
			}
		case strings.HasPrefix(name, cacheMetaPrefix+"generation."):
			if json.Unmarshal([]byte(value), &record) != nil || record.Key == "" || record.Handle == "" ||
				!present[record.Handle] {
				continue
			}

			e := entry(record.Key)
			if !seen[record.Key][record.Handle] {
				seen[record.Key][record.Handle] = true
				e.Generations++
			}

			if record.UsedAt.After(e.LastUsed) {
				e.LastUsed = record.UsedAt
			}
		}
	}

	out := make([]KeyGenerations, 0, len(byKey))

	for key, e := range byKey {
		e.Current = current[key]

		for handle := range seen[key] {
			if handle != e.Current {
				e.Superseded++
			}
		}

		if e.Generations > 0 || e.Current != "" {
			out = append(out, *e)
		}
	}

	slices.SortFunc(out, func(a, b KeyGenerations) int {
		if n := cmp.Compare(b.Generations, a.Generations); n != 0 {
			return n
		}

		return cmp.Compare(a.Key, b.Key)
	})

	return out, nil
}
