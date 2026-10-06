package ceph

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// cacheCommand reads one recorded rbd call as the verb it ran and the image it
// ran on, or reports that it is neither a creation nor a change.
func cacheCommand(call []string) (verb, image string, ok bool) {
	for i, arg := range call {
		target := func(offset int) (string, string, bool) {
			if i+offset >= len(call) {
				return "", "", false
			}

			return verb, call[i+offset], true
		}

		switch arg {
		case "snap", "trash", "image-meta", "lock":
			if i+1 >= len(call) {
				return "", "", false
			}
			verb = arg + " " + call[i+1]

			return target(2)
		case "create", "rm":
			verb = arg

			return target(1)
		case "clone", "cp":
			verb = arg

			return target(2)
		}
	}

	return "", "", false
}

// imageBase is the image a pool/name@snapshot spec names.
func imageBase(spec string) string {
	_, name, found := strings.Cut(spec, "/")
	if !found {
		name = spec
	}
	name, _, _ = strings.Cut(name, "@")

	return name
}

// requireCacheImageRules holds #394's two rules over every recorded call, and
// that each verb in want ran on a cache image at least once.
//
// A CACHE VOLUME OR GENERATION IS MADE WITH OBJECT-MAP, and every command that
// changes one keeps the kernel client. Created with `layering` alone, a discarded
// volume's deletion removed every object its size could hold: 237 s for a
// 100 GiB volume with 100 MiB written, against 3.5 s with object-map, and the
// trash purge kept both OSDs saturated for hours (2026-10-05). Object-map brings
// exclusive-lock, which the kernel holds while a volume is mapped, so a change
// must not blocklist the kernel client when it breaks a lock an interrupted
// unmap left. The lock images, the publish lock and the cache index, are the
// other way round: `layering` alone, because Ceph documents exclusive-lock as
// incompatible with the advisory locks taken on them, and their `lock rm` keeps
// the blocklist that is its fence. The feature names are checked one by one, so
// an edit to cacheImageFeatures cannot quietly drop them.
func requireCacheImageRules(t *testing.T, calls [][]string, want ...string) {
	t.Helper()

	features := func(call []string) []string {
		i := slices.Index(call, "--image-feature")
		if i < 0 || i+1 >= len(call) {
			return nil
		}

		return strings.Split(call[i+1], ",")
	}
	keepsClient := func(call []string) bool {
		i := slices.Index(call, "--rbd_blocklist_on_break_lock")

		return i >= 0 && i+1 < len(call) && call[i+1] == "false"
	}

	seen := map[string]bool{}

	for _, call := range calls {
		verb, spec, ok := cacheCommand(call)
		if !ok {
			continue
		}
		base := imageBase(spec)

		switch base {
		case LockImageName, cacheIndexName:
			// EXACTLY `layering`, named: with no feature named, rbd would give the
			// lock image the cluster's defaults, exclusive-lock among them.
			if verb == "create" && !slices.Equal(features(call), []string{"layering"}) {
				t.Errorf("a lock image was not created with layering alone: %v", call)
			}
			if verb == "lock rm" && keepsClient(call) {
				t.Errorf("a lock image's lock rm lost its blocklist fence: %v", call)
			}

		default:
			if _, _, named := cacheImageName(base); !named {
				continue
			}
			seen[verb] = true

			switch verb {
			case "create", "clone", "cp":
				have := features(call)
				for _, feature := range []string{
					"layering", "exclusive-lock", "object-map", "fast-diff", "deep-flatten",
				} {
					if !slices.Contains(have, feature) {
						t.Errorf("a cache image was made without %s: %v", feature, call)
					}
				}
			case "rm", "snap create", "snap rm", "snap purge", "trash mv", "image-meta set",
				"image-meta remove":
				if !keepsClient(call) {
					t.Errorf("a command that changes a cache image could blocklist the host: %v", call)
				}
			}
		}
	}

	// THE FLOW REACHED WHAT IT CLAIMS TO, or the rules above judged nothing.
	for _, verb := range want {
		if !seen[verb] {
			t.Errorf("no %q ran on a cache image; the rules were never applied to it", verb)
		}
	}
}

// A CACHE VOLUME'S LIFE, CREATED TO DISCARDED, KEEPS BOTH RULES (#394).
func TestCacheVolumesCarryObjectMapAndNeverBlocklistTheHost(t *testing.T) {
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
	clone, err := c.Clone(t.Context(), "acme/api/npm", "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := c.Discard(t.Context(), clone); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	requireCacheImageRules(t, f.calls, "create", "clone", "snap create", "trash mv")
}
