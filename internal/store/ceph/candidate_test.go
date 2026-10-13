package ceph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// publishedGeneration publishes one generation of key and answers it.
func publishedGeneration(t *testing.T, c *Client) storecontract.Candidate {
	t.Helper()

	const key = "acme/api/go"

	volume, err := c.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lease, fence, err := c.AcquireWriter(t.Context(), key, "writer", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	candidate, err := c.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := c.PublishCAS(t.Context(), key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}

	return candidate
}

// A generation is checked through a read-only map of its own snapshot, which is
// unmapped afterwards; the check's answer is the store's, could-not-tell
// included, and a generation the key never had is never mapped.
func TestAGenerationIsCheckedThroughAReadOnlyMapOfItsOwnSnapshot(t *testing.T) {
	t.Parallel()

	couldNotTell := errors.New("e2fsck: exit status 8")
	for _, tc := range []struct {
		name    string
		damaged bool
		err     error
	}{
		{name: "damaged", damaged: true},
		{name: "sound"},
		{name: "could not tell", err: couldNotTell},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			v := &verifiedSpecs{f: f, verdict: clean}
			c := verifyingClient(t, f, v)
			generation := publishedGeneration(t, c)
			var checked []string
			var readOnly []bool
			c.damaged = func(_ context.Context, device string) (bool, error) {
				checked = append(checked, f.mappings[device])
				readOnly = append(readOnly, v.mappedReadOnly(device))

				return tc.damaged, tc.err
			}

			damaged, err := c.GenerationDamaged(t.Context(), "acme/api/go", generation.Generation)
			if damaged != tc.damaged || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("GenerationDamaged = %v, %v; want %v, %v", damaged, err, tc.damaged, tc.err)
			}
			want := generation.Handle + "@" + generation.Generation
			if !slices.Equal(checked, []string{want}) || !readOnly[0] {
				t.Fatalf("checked %v (read-only %v), want %s read-only", checked, readOnly, want)
			}
			if len(f.mappings) != 0 {
				t.Errorf("devices are still mapped: %v", f.mappings)
			}

			checked = nil
			_, err = c.GenerationDamaged(t.Context(), "acme/api/go", "1791000000-0000000000000000000000ab")
			if !errors.Is(err, storecontract.ErrMiss) || len(checked) != 0 {
				t.Errorf("a generation the key never had answered %v after checking %v", err, checked)
			}
		})
	}
}

// A map that the kernel finishes as its caller gives up is unmapped all the
// same, under a context of its own.
func TestAGenerationMapThatLandsAsItsCallerGivesUpIsUnmapped(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	v := &verifiedSpecs{f: f, verdict: clean}
	c := verifyingClient(t, f, v)
	generation := publishedGeneration(t, c)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.run = func(runCtx context.Context, bin string, args []string) ([]byte, error) {
		if slices.Contains(args, "map") && slices.Contains(args, "--read-only") {
			if _, err := f.run(runCtx, bin, args); err != nil {
				return nil, err
			}
			cancel()

			return nil, errors.New("exit status 110: rbd: map failed: (110) Connection timed out")
		}

		return f.run(runCtx, bin, args)
	}

	if _, err := c.GenerationDamaged(ctx, "acme/api/go", generation.Generation); err == nil {
		t.Fatal("GenerationDamaged answered although its map failed")
	}
	if len(f.mappings) != 0 {
		t.Fatalf("the map that landed is still mapped: %v", f.mappings)
	}
}

// Eviction first unmaps a read-only generation mapping a crash left behind, and
// no other mapping.
func TestEvictionUnmapsAGenerationMappingACrashLeftBehind(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c := cacheClient(t, f)
	f.mappings["/dev/rbd40"] = "billet-cache/cache-g-1791233900-6916d9e459925223c4c82c1b@1791233900-7f26324f3e5fd2d379cf1af8"
	f.mappings["/dev/rbd41"] = "billet-cache/cache-v-1791233827-6d7c5816deb0541e07660197"
	f.mappings["/dev/rbd42"] = "billet-cache/billet-71d12938a937b3369325e5f25dee1f5e"
	f.mappings["/dev/rbd43"] = "billet-cache/cache-g-1791184358-ce496b62ed22214a13fbed7b"
	f.mappings["/dev/rbd44"] = "billet-cache/cache-v-1791233827-6d7c5816deb0541e07660197@1791233900-7f26324f3e5fd2d379cf1af8"
	f.mappings["/dev/rbd45"] = "billet-images/ubuntu-2404-x64@g20261013005155"
	f.mappings["/dev/rbd46"] = "billet-cache/cache-g-1791000000-by-hand@backup"

	if err := c.Evict(t.Context(), time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, ok := f.mappings["/dev/rbd40"]; ok {
		t.Error("the generation mapping a crash left behind is still mapped")
	}
	for _, device := range []string{"/dev/rbd41", "/dev/rbd42", "/dev/rbd43", "/dev/rbd44", "/dev/rbd45", "/dev/rbd46"} {
		if _, ok := f.mappings[device]; !ok {
			t.Errorf("eviction unmapped %s, which is no generation's snapshot", device)
		}
	}
}

// The damage check runs `e2fsck -f -n <device>` and reads its exit status: the
// uncorrected-errors bit is damage, and anything else that is not 0 (8 alone, a
// signal, a missing executable, a cancelled check) is could-not-tell.
func TestTheDamageCheckReadsE2fsckExitStatus(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		body    string
		damaged bool
		err     bool
	}{
		{"clean", "exit 0", false, false},
		{"uncorrected", "exit 4", true, false},
		{"uncorrected and cannot proceed", "exit 12", true, false},
		{"operational error", "exit 8", false, true},
		{"signal", "kill -9 $$", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			e2fsck := filepath.Join(dir, "e2fsck")
			script := "#!/bin/sh\nprintf '%s ' \"$@\" > " + filepath.Join(dir, "args") + "\n" + tc.body + "\n"
			if err := os.WriteFile(e2fsck, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			damaged, err := checkDamageWith(t.Context(), e2fsck, "/dev/rbd9")
			if damaged != tc.damaged || (err != nil) != tc.err {
				t.Fatalf("checkDamageWith = %v, %v; want %v and an error %v", damaged, err, tc.damaged, tc.err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil || string(args) != "-f -n /dev/rbd9 " {
				t.Fatalf("e2fsck ran with %q (%v), want -f -n /dev/rbd9", args, err)
			}
		})
	}

	if damaged, err := checkDamageWith(t.Context(), filepath.Join(t.TempDir(), "absent"), "/dev/rbd9"); damaged || err == nil {
		t.Errorf("a missing e2fsck answered %v, %v; want could-not-tell", damaged, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if damaged, err := checkDamageWith(ctx, "/bin/sh", "/dev/rbd9"); damaged || err == nil {
		t.Errorf("a cancelled check answered %v, %v; want could-not-tell", damaged, err)
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
