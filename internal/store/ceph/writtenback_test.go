package ceph

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	storecontract "github.com/junioryono/billet/internal/store"
)

type fakeSyncer struct {
	sync   error
	closed *int
}

func (f fakeSyncer) Sync() error { return f.sync }

func (f fakeSyncer) Close() error {
	*f.closed++

	return nil
}

// A WRITE-BACK THAT FAILED IS LOST WRITES; A DEVICE THAT COULD NOT BE OPENED PROVES
// NOTHING. The first is the kernel's report that what was written never reached the
// volume, so a candidate is refused for good; the second refuses this attempt only.
func TestAFailedWriteBackIsLostWrites(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		open, sync error
		lost       bool
	}{
		"written back":            {},
		"a write-back that fails": {sync: errors.New("input/output error"), lost: true},
		"an open that fails":      {open: errors.New("no such device")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			closed := 0
			err := syncDeviceWith("/dev/rbd7", func(device string) (deviceSyncer, error) {
				if device != "/dev/rbd7" {
					t.Errorf("opened %s, want /dev/rbd7", device)
				}
				if tc.open != nil {
					return nil, tc.open
				}

				return fakeSyncer{sync: tc.sync, closed: &closed}, nil
			})

			switch {
			case tc.lost && !errors.Is(err, storecontract.ErrWritesLost):
				t.Fatalf("a failed fsync answered %v, want store.ErrWritesLost", err)
			case tc.lost && !strings.Contains(err.Error(), "input/output error"):
				t.Errorf("the lost writes' answer %q does not carry the fsync's error", err)
			case !tc.lost && errors.Is(err, storecontract.ErrWritesLost):
				t.Fatalf("%s answered lost writes: %v", name, err)
			case tc.open != nil && err == nil:
				t.Fatal("a device that could not be opened answered nothing")
			case tc.open == nil && tc.sync == nil && err != nil:
				t.Fatalf("a device written back answered %v", err)
			}
			if wantClosed := map[bool]int{true: 0, false: 1}[tc.open != nil]; closed != wantClosed {
				t.Errorf("the device was closed %d time(s), want %d", closed, wantClosed)
			}
		})
	}
}

// THE DAMAGE CHECK'S WATCHER SEES A FAILED READ HOWEVER e2fsck'S OUTPUT ARRIVES,
// split anywhere across writes, at the start of a line or after pass 1B's block
// numbers with no newline between, and keeps no more of it than it says; a file
// named like a failed read, in a report of real damage, is not one.
func TestTheWatcherSeesAFailedReadSplitAcrossWrites(t *testing.T) {
	t.Parallel()

	for _, output := range []string{
		"Pass 1: Checking inodes\nError reading block 2081 (Input/output error) while getting next inode\n",
		"Running additional passes to resolve blocks claimed by more than one inode...\n" +
			"Multiply-claimed block(s) in inode 12: 1033Error reading block 1034 (Input/output error).  " +
			"Ignore error? no\n",
		"Error reading extended attribute block 33280 for inode 12.  Clear? no\n",
	} {
		for split := range len(output) {
			w := readFailureWatcher()
			for _, part := range []string{output[:split], output[split:]} {
				if n, err := w.Write([]byte(part)); n != len(part) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", part, n, err)
				}
				if len(w.tail) > w.keep {
					t.Fatalf("the watcher kept %d bytes, more than its %d", len(w.tail), w.keep)
				}
			}
			if !w.seen {
				t.Fatalf("split at %d, the watcher did not see the failed read in %q", split, output)
			}
		}
	}

	for _, parts := range [][]string{
		{"Error rea", "ched the end", " of reading\n"},
		{"Entry 'Error reading' in / (2) has deleted/unused inode 20.  Clear? no\n"},
		{"Inode 12, i_blocks is 8, should be 0.  Fix? no\n", "Block bitmap differences:  -(33904--33919)\n"},
	} {
		w := readFailureWatcher()
		for _, part := range parts {
			if n, err := w.Write([]byte(part)); n != len(part) || err != nil {
				t.Fatalf("Write(%q) = %d, %v", part, n, err)
			}
		}
		if w.seen {
			t.Errorf("the watcher saw a failed read in %q, which reports none", parts)
		}
	}
}

// THE CLIENT'S OWN VERIFIER ASKS FOR THE WRITE-BACK, so every real Snapshot does
// before its filesystem check: a test's verifier replaces it, the constructor's does
// not.
func TestTheClientVerifiesAWrittenBackDevice(t *testing.T) {
	t.Parallel()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if reflect.ValueOf(c.verify).Pointer() != reflect.ValueOf(filesystemVerifier(verifyWrittenBack)).Pointer() {
		t.Error("the client's filesystem verifier is not verifyWrittenBack, so a Snapshot would check a " +
			"filesystem without asking whether its writes reached the volume")
	}

	// AND IT ASKS BEFORE IT CHECKS: a device it cannot open for the write-back is
	// refused for that, not by the filesystem check after it.
	_, err = verifyWrittenBack(t.Context(), filepath.Join(t.TempDir(), "rbd9"))
	if err == nil || !strings.Contains(err.Error(), "to ask for its write-back") {
		t.Errorf("verifyWrittenBack of a device it could not open answered %v, want the write-back's open refused", err)
	}
}
