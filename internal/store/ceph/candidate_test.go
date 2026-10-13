package ceph

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

const sourceUUID = "dcab7af5-4ae7-4cc1-8ddb-1db18956c389"

// verifiedSpecs is a filesystem verifier that answers by the spec each device
// maps, and records the specs it was asked about with whether the map was
// read-only.
type verifiedSpecs struct {
	f        *cacheFake
	verdict  func(spec string) (storecontract.Filesystem, error)
	specs    []string
	readOnly []bool
}

func (v *verifiedSpecs) verify(_ context.Context, device string) (storecontract.Filesystem, error) {
	spec := v.f.mappings[device]
	v.specs = append(v.specs, spec)
	v.readOnly = append(v.readOnly, v.mappedReadOnly(device))

	return v.verdict(spec)
}

// mappedReadOnly reports whether the map that produced device asked for a
// read-only mapping: the device names are handed out in order of the maps.
func (v *verifiedSpecs) mappedReadOnly(device string) bool {
	maps := 0
	for _, call := range v.f.calls {
		if !slices.Contains(call, "device") || !slices.Contains(call, "map") {
			continue
		}
		if "/dev/rbd"+strconv.Itoa(maps) == device {
			return slices.Contains(call, "--read-only")
		}
		maps++
	}

	return false
}

func clean(string) (storecontract.Filesystem, error) {
	return storecontract.Filesystem{Type: "ext4", UUID: sourceUUID, Clean: true}, nil
}

func verifyingClient(t *testing.T, f *cacheFake, v *verifiedSpecs) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run), withFilesystemVerifier(v.verify), WithCacheSessions(f.readSessions))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// candidateVolume is a writable volume whose snapshot takes the path asked for:
// a clone of the staging snapshot, or the compacting copy a lineage at its depth
// limit takes.
func candidateVolume(t *testing.T, c *Client, f *cacheFake, compacted bool) storecontract.Volume {
	t.Helper()

	key := "acme/api/go"
	first, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !compacted {
		return first
	}

	lease, fence, err := c.AcquireWriter(t.Context(), key, "first", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := c.Snapshot(t.Context(), first)
	if err != nil {
		t.Fatalf("Snapshot of the first generation: %v", err)
	}
	if err := c.PublishCAS(t.Context(), key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	volume, err := c.Clone(t.Context(), key, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if f.metadata[volume.Handle] == nil {
		f.metadata[volume.Handle] = map[string]string{}
	}
	f.metadata[volume.Handle][cacheMetaPrefix+"lineage"] = strconv.Itoa(maxCacheCloneDepth - 1)

	return volume
}

// A candidate's filesystem is checked as it will be cloned: its frozen snapshot,
// mapped read-only on a device of its own, which is unmapped afterwards.
func TestACandidateIsVerifiedThroughAReadOnlyMapOfItsFrozenSnapshot(t *testing.T) {
	t.Parallel()

	for _, compacted := range []bool{false, true} {
		t.Run("compacted="+strconv.FormatBool(compacted), func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			v := &verifiedSpecs{f: f, verdict: clean}
			c := verifyingClient(t, f, v)
			volume := candidateVolume(t, c, f, compacted)
			v.specs, v.readOnly = nil, nil
			copies := countCalls(f, "cp")

			candidate, err := c.Snapshot(t.Context(), volume)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if copied := countCalls(f, "cp") > copies; copied != compacted {
				t.Fatalf("the snapshot copied the lineage: %v, want %v", copied, compacted)
			}

			published := candidate.Handle + "@" + candidate.Generation
			at := slices.Index(v.specs, published)
			if at < 0 {
				t.Fatalf("the verifier was asked about %v, never the published %s", v.specs, published)
			}
			if !v.readOnly[at] {
				t.Errorf("the published %s was mapped writable to be verified", published)
			}
			for device, spec := range f.mappings {
				if spec == published {
					t.Errorf("the published %s is still mapped at %s", published, device)
				}
			}
		})
	}
}

// A candidate whose published bytes fail the check, or hold another filesystem
// than the one verified before it was made, is removed and never published.
func TestACandidateThatFailsAsPublishedIsRemovedAndNeverPublished(t *testing.T) {
	t.Parallel()

	damaged := func(spec string) (storecontract.Filesystem, error) {
		if strings.Contains(spec, "/cache-g-") {
			return storecontract.Filesystem{}, errors.New("the ext4 filesystem is not clean: exit status 12")
		}

		return clean(spec)
	}
	substituted := func(spec string) (storecontract.Filesystem, error) {
		if strings.Contains(spec, "/cache-g-") {
			return storecontract.Filesystem{Type: "ext4", UUID: "0b7c5e1d-1111-4222-8333-944455556666", Clean: true}, nil
		}

		return clean(spec)
	}

	// A check that fails while naming the same filesystem is still a failure.
	damagedSameFilesystem := func(spec string) (storecontract.Filesystem, error) {
		filesystem := storecontract.Filesystem{Type: "ext4", UUID: sourceUUID, Clean: true}
		if strings.Contains(spec, "/cache-g-") {
			filesystem.Clean = false

			return filesystem, errors.New("the ext4 filesystem is not clean: exit status 4")
		}

		return filesystem, nil
	}

	for name, verdict := range map[string]func(string) (storecontract.Filesystem, error){
		"damaged": damaged, "substituted": substituted, "damaged, same filesystem": damagedSameFilesystem,
	} {
		for _, compacted := range []bool{false, true} {
			t.Run(name+"/compacted="+strconv.FormatBool(compacted), func(t *testing.T) {
				t.Parallel()

				f := newCacheFake()
				v := &verifiedSpecs{f: f, verdict: clean}
				c := verifyingClient(t, f, v)
				volume := candidateVolume(t, c, f, compacted)
				before := f.metadata[c.cacheIndex()][pointerKey("acme/api/go")]
				generations := map[string]bool{}
				for image := range f.images {
					generations[image] = strings.Contains(image, "/cache-g-")
				}
				v.verdict = verdict

				if _, err := c.Snapshot(t.Context(), volume); err == nil {
					t.Fatal("Snapshot answered a candidate whose published bytes failed the check")
				}

				for image := range f.images {
					if strings.Contains(image, "/cache-g-") && !generations[image] {
						t.Errorf("the failed candidate %s is still in the pool", image)
					}
				}
				for snapshot := range f.snapshots {
					if strings.HasPrefix(snapshot, volume.Handle+"@") {
						t.Errorf("the staging snapshot %s was left behind", snapshot)
					}
				}
				if len(f.mappings) != 0 {
					t.Errorf("devices are still mapped: %v", f.mappings)
				}
				if after := f.metadata[c.cacheIndex()][pointerKey("acme/api/go")]; after != before {
					t.Errorf("the pointer moved from %q to %q", before, after)
				}
				if !f.ranWith("trash", "mv", volume.Handle) {
					t.Error("the volume behind the failed candidate was not moved to the trash")
				}
			})
		}
	}
}

func countCalls(f *cacheFake, verb string) int {
	n := 0
	for _, call := range f.calls {
		if slices.Contains(call, verb) {
			n++
		}
	}

	return n
}
