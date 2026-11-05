package ceph

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	storecontract "github.com/junioryono/billet/internal/store"
)

// provenDeviceClient is a cache client whose verifier records what it was asked
// to read, with one volume to snapshot and another job's volume beside it.
func provenDeviceClient(
	t *testing.T, f *cacheFake, run runner,
) (*Client, storecontract.Volume, storecontract.Volume, *[]string) {
	t.Helper()

	var read []string
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(run),
		withFilesystemVerifier(func(_ context.Context, device string) (storecontract.Filesystem, error) {
			read = append(read, device)

			return storecontract.Filesystem{
				Type: "ext4", UUID: "dcab7af5-4ae7-4cc1-8ddb-1db18956c389", Clean: true,
			}, nil
		}), WithCacheSessions(f.readSessions))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	volume, err := c.Create(t.Context(), "acme/api/docker", 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := c.Create(t.Context(), "acme/web/docker", 1<<30)
	if err != nil {
		t.Fatalf("Create the other job's volume: %v", err)
	}

	return c, volume, other, &read
}

func unmapped(f *cacheFake) [][]string {
	var calls [][]string
	for _, call := range f.calls {
		if slices.Contains(call, "device") && slices.Contains(call, "unmap") {
			calls = append(calls, call)
		}
	}

	return calls
}

// A NUMBER THE KERNEL HAS GIVEN TO ANOTHER JOB IS NEITHER READ NOR UNMAPPED. A
// failed snapshot unmaps and discards its volume, and the retry read whatever held
// the recorded number by then: on 2026-11-04 another job's running root disk,
// reported "not clean", with an unmap of it next had it looked clean. Whether the
// volume's own image is gone or only unmapped decides what the caller does next,
// and neither touches that device.
func TestASnapshotOfAVolumeNoLongerMappedTouchesTheDeviceThatTookItsNumber(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		removed bool
		want    error
	}{
		"its image is gone":  {removed: true, want: storecontract.ErrVolumeGone},
		"its image is there": {want: storecontract.ErrDeviceUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			c, volume, other, read := provenDeviceClient(t, f, f.run)

			delete(f.mappings, volume.Device)
			if tc.removed {
				delete(f.images, volume.Handle)
			}
			stale := volume
			stale.Device = other.Device
			before := len(unmapped(f))

			_, err := c.Snapshot(t.Context(), stale)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Snapshot = %v, want %v", err, tc.want)
			}
			if len(*read) != 0 {
				t.Errorf("the check read %v, which is another job's device", *read)
			}
			if got := unmapped(f)[before:]; len(got) != 0 {
				t.Errorf("unmapped %v, and that number is another job's device", got)
			}
			if f.mappings[other.Device] == "" {
				t.Errorf("the other job's %s lost its mapping", other.Device)
			}
		})
	}
}

// THE KERNEL'S TABLE IS THE ANSWER, so a number that is wrong for a volume still
// mapped elsewhere reads and unmaps the volume's own device.
func TestASnapshotUsesTheDeviceTheVolumeIsMappedToNow(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c, volume, other, read := provenDeviceClient(t, f, f.run)

	moved := volume
	moved.Device = other.Device

	if _, err := c.Snapshot(t.Context(), moved); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if !slices.Contains(*read, volume.Device) || slices.Contains(*read, other.Device) {
		t.Errorf("the check read %v, want the volume's own %s and not %s", *read, volume.Device, other.Device)
	}
	if f.mappings[volume.Device] != "" {
		t.Errorf("the volume's own %s is still mapped to %s", volume.Device, f.mappings[volume.Device])
	}
	if f.mappings[other.Device] == "" {
		t.Errorf("the other job's %s lost its mapping", other.Device)
	}
}

// A TABLE THAT CANNOT BE READ IS COULD-NOT-TELL, never gone: the publication is
// retried, and a volume that is still there is not given up on.
func TestASnapshotWhoseMappingsCannotBeReadIsNotGone(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	failing := false
	run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if failing && slices.Contains(args, "device") && slices.Contains(args, "list") {
			return nil, errors.New("exit status 1: rbd: the monitors did not answer")
		}

		return f.run(ctx, bin, args)
	}
	c, volume, _, read := provenDeviceClient(t, f, run)
	failing = true

	_, err := c.Snapshot(t.Context(), volume)

	if !errors.Is(err, storecontract.ErrDeviceUnknown) || errors.Is(err, storecontract.ErrVolumeGone) {
		t.Fatalf("Snapshot = %v, want ErrDeviceUnknown", err)
	}
	if len(*read) != 0 {
		t.Errorf("the check read %v without knowing whose device it was", *read)
	}
	if f.mappings[volume.Device] == "" {
		t.Errorf("the volume lost its mapping though nothing was proved")
	}
}

// A MAPPED SNAPSHOT OF THE IMAGE IS NOT ITS WRITABLE HEAD, though the kernel's
// table lists it under the image's name.
func TestAMappedSnapshotIsNotTheVolumesDevice(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c, volume, _, _ := provenDeviceClient(t, f, f.run)

	image := f.mappings[volume.Device]
	delete(f.mappings, volume.Device)
	f.mappings["/dev/rbd77"] = image + "@1793768786-05bbfa71efa771269ed2f629"

	if device, err := c.MappedDevice(t.Context(), volume); !errors.Is(err, storecontract.ErrDeviceUnknown) {
		t.Fatalf("MappedDevice = %q, %v, want ErrDeviceUnknown", device, err)
	}
}

// A VOLUME MAPPED TWICE IS ANSWERED BY ITS RECORD ONLY WHEN THE RECORD IS ONE OF
// THEM. A record naming neither says nothing about which is which, which is not
// the same as the volume being gone.
func TestAVolumeMappedTwiceIsAnsweredByARecordThatIsOneOfThem(t *testing.T) {
	t.Parallel()

	f := newCacheFake()
	c, volume, _, _ := provenDeviceClient(t, f, f.run)
	f.mappings["/dev/rbd77"] = f.mappings[volume.Device]

	device, err := c.MappedDevice(t.Context(), volume)
	if err != nil || device != volume.Device {
		t.Fatalf("MappedDevice = %q, %v, want the recorded %s", device, err, volume.Device)
	}

	unknown := volume
	unknown.Device = "/dev/rbd99"
	device, err = c.MappedDevice(t.Context(), unknown)
	if !errors.Is(err, storecontract.ErrDeviceUnknown) {
		t.Fatalf("MappedDevice = %q, %v, want ErrDeviceUnknown", device, err)
	}
}

// A ROW THAT DOES NOT SAY WHOSE IT IS, OR IN ANOTHER NAMESPACE, PROVES NOTHING: the
// first could be this volume's, and the second is a different image of the same
// name, so neither is its device and neither says it is gone.
func TestAMappingTableThatProvesNothingIsNotAnAnswer(t *testing.T) {
	t.Parallel()

	for name, rows := range map[string]string{
		"a null row":                `[null]`,
		"a row without its pool":    `[{"name":"%[1]s","snap":"-","device":"/dev/rbd5"}]`,
		"a row without its image":   `[{"pool":"billet-cache","snap":"-","device":"/dev/rbd5"}]`,
		"a row without a snapshot":  `[{"pool":"billet-cache","name":"%[1]s","device":"/dev/rbd5"}]`,
		"a row without its device":  `[{"pool":"billet-cache","name":"%[1]s","snap":"-"}]`,
		"null instead of a table":   `null`,
		"another namespace's image": `[{"pool":"billet-cache","namespace":"other","name":"%[1]s","snap":"-","device":"/dev/rbd5"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			var answer []byte
			run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
				if answer != nil && slices.Contains(args, "device") && slices.Contains(args, "list") {
					return answer, nil
				}

				return f.run(ctx, bin, args)
			}
			c, volume, _, _ := provenDeviceClient(t, f, run)
			image := strings.TrimPrefix(volume.Handle, "billet-cache/")
			answer = []byte(strings.ReplaceAll(rows, "%[1]s", image))
			delete(f.mappings, volume.Device)
			// The image is gone too, so a table read as "nothing maps it" answers gone.
			delete(f.images, volume.Handle)

			device, err := c.MappedDevice(t.Context(), volume)
			if name == "another namespace's image" {
				if !errors.Is(err, storecontract.ErrVolumeGone) {
					t.Fatalf("MappedDevice = %q, %v, want ErrVolumeGone: that row is not this image", device, err)
				}

				return
			}
			if !errors.Is(err, storecontract.ErrDeviceUnknown) {
				t.Fatalf("MappedDevice = %q, %v, want ErrDeviceUnknown", device, err)
			}
		})
	}
}

// A MISSING ROW IS NOT GONE WHILE THE POOL LISTS THE IMAGE. rbd's device list skips a
// device it could not read and still succeeds, and gone is what has a job's writes
// thrown away, so only the pool's own listing without it says so. rbd status is no
// witness: it answers ENOENT for a pool that will not open and for an image whose
// parent is missing, and either can still hold those writes.
func TestGoneMeansThePoolDoesNotListTheImage(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		removed  bool
		listErr  error
		wantGone bool
	}{
		"no row and the image is listed": {},
		"no row and no image":            {removed: true, wantGone: true},
		"no row and the listing fails": {
			removed: true,
			listErr: errors.New("exit status 1: rbd: the monitors did not answer"),
		},
		"no row and the pool will not open": {
			removed: true,
			listErr: errors.New("rbd: error opening pool 'billet-cache': (2) No such file or directory"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCacheFake()
			var listErr error
			run := func(ctx context.Context, bin string, args []string) ([]byte, error) {
				if slices.Contains(args, "status") {
					return nil, errors.New("rbd: error opening image: (2) No such file or directory")
				}
				if listErr != nil && slices.Contains(args, "ls") {
					return nil, listErr
				}

				return f.run(ctx, bin, args)
			}
			c, volume, _, _ := provenDeviceClient(t, f, run)
			delete(f.mappings, volume.Device)
			if tc.removed {
				delete(f.images, volume.Handle)
			}
			listErr = tc.listErr

			device, err := c.MappedDevice(t.Context(), volume)
			if tc.wantGone {
				if !errors.Is(err, storecontract.ErrVolumeGone) {
					t.Fatalf("MappedDevice = %q, %v, want ErrVolumeGone", device, err)
				}

				return
			}
			if !errors.Is(err, storecontract.ErrDeviceUnknown) {
				t.Fatalf("MappedDevice = %q, %v, want ErrDeviceUnknown", device, err)
			}
		})
	}
}
