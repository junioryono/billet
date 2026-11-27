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
	// in the pool. Superseded is how many of those its pointer positively no longer
	// names: a key with no pointer has superseded nothing anyone can tell.
	Generations int
	Superseded  int
	// Current is the generation the key's pointer names, "" when none does.
	Current string
	// LastUsed is the latest use any of its records gives.
	LastUsed time.Time
}

// GenerationReport is the pool's generations by key.
type GenerationReport struct {
	// Keys are most generations first.
	Keys []KeyGenerations
	// Generations is how many distinct generations the pool holds that a record
	// names: one shared by two keys counts once here and once in each key's row.
	Generations int
	// Unreadable is how many pointer and generation records could not be read, so
	// how far the counts may fall short.
	Unreadable int
}

// KeyGenerations counts each key's generations from the cache index and the pool's
// listing: two commands whatever the pool holds, neither taking the cache lock.
//
// SAID BEFORE CEPH SAYS IT. On 2026-11-07 one key held 845 generations and most of a
// full pool (#799); eviction now bounds that, and this is how an operator sees a key
// that eviction cannot keep up with, or whose generations something still pins.
//
// A record whose image is gone is not counted: eviction removes the image before
// its record, and an interrupted pass leaves the record for the next. A record that
// cannot be read is counted as unreadable, never as nothing.
func (c *Client) KeyGenerations(ctx context.Context) (GenerationReport, error) {
	metadata, err := c.cacheIndexMetadata(ctx)
	if err != nil {
		return GenerationReport{}, err
	}

	images, err := c.cacheImages(ctx)
	if err != nil {
		return GenerationReport{}, err
	}

	present := make(map[string]bool, len(images))
	for _, name := range images {
		present[c.cfg.CachePool+"/"+name] = true
	}

	var report GenerationReport

	current := map[string]string{}
	byKey := map[string]*KeyGenerations{}
	handles := map[string]map[string]bool{}
	all := map[string]bool{}

	entry := func(key string) *KeyGenerations {
		if byKey[key] == nil {
			byKey[key] = &KeyGenerations{Key: key}
			handles[key] = map[string]bool{}
		}

		return byKey[key]
	}

	for name, value := range metadata {
		pointer := strings.HasPrefix(name, cacheMetaPrefix+"pointer.")
		generation := strings.HasPrefix(name, cacheMetaPrefix+"generation.")

		if !pointer && !generation {
			continue
		}

		var record cachePointer
		if json.Unmarshal([]byte(value), &record) != nil || record.Key == "" || record.Handle == "" {
			report.Unreadable++

			continue
		}

		e := entry(record.Key)
		if record.UsedAt.After(e.LastUsed) {
			e.LastUsed = record.UsedAt
		}

		if pointer {
			current[record.Key] = record.Handle

			continue
		}

		if present[record.Handle] {
			handles[record.Key][record.Handle] = true
			all[record.Handle] = true
		}
	}

	report.Generations = len(all)
	report.Keys = make([]KeyGenerations, 0, len(byKey))

	for key, e := range byKey {
		e.Current = current[key]
		e.Generations = len(handles[key])

		// SUPERSEDED ONLY ON A POSITIVE READING, as eviction reads it: the key's
		// pointer names another generation. A first publication stopped between its
		// generation record and its pointer has superseded nothing.
		if e.Current != "" {
			for handle := range handles[key] {
				if handle != e.Current {
					e.Superseded++
				}
			}
		}

		if e.Generations > 0 || e.Current != "" {
			report.Keys = append(report.Keys, *e)
		}
	}

	slices.SortFunc(report.Keys, func(a, b KeyGenerations) int {
		if n := cmp.Compare(b.Generations, a.Generations); n != 0 {
			return n
		}

		return cmp.Compare(a.Key, b.Key)
	})

	return report, nil
}
