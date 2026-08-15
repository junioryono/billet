package ceph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A DEVICE THE KERNEL HAS NOT LET GO OF YET IS WAITED FOR, NOT REPORTED.
//
// Teardown stops the VMM and waits for the process to be gone — but a process
// exiting is not the same as its descriptors being closed, and for a moment after
// the last reference drops the kernel client still holds the device. `rbd device
// unmap` answers `(16) Device or resource busy`.
//
// MEASURED BY RUNNING IT: `billet images verify` destroys its probe the instant the
// guest reports back, which is the tightest this gap ever gets, and it failed there
// while the same teardown driven by a test polling every two seconds never did.
// Treating it as a hard failure leaves a mapped device pinning an image nothing can
// remove, and reports an error for a teardown that was correct.
func TestAnUnmapWaitsOutADeviceTheKernelStillHolds(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		unmaps   int
		mappings = `[{"id":"0","pool":"billet-cache","name":"billet-probe","device":"/dev/rbd0"}]`
	)

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")

			switch {
			case strings.Contains(joined, "device list"):
				return []byte(mappings), nil

			case strings.Contains(joined, "device unmap"):
				mu.Lock()
				defer mu.Unlock()

				unmaps++

				// BUSY TWICE AND THEN GONE, which is the shape of the real thing:
				// the kernel releases it a moment later, not never.
				if unmaps < 3 {
					return nil, errors.New("rbd: unmap failed: (16) Device or resource busy")
				}

				return nil, nil

			default:
				return nil, nil
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.DiscardRoot(t.Context(), "billet-probe"); err != nil {
		t.Fatalf("a device the kernel released a moment later was reported as a failure: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if unmaps < 3 {
		t.Errorf("unmap was attempted %d times; it has to be retried while the kernel still "+
			"holds the device", unmaps)
	}
}

// AND EVERY OTHER FAILURE IS STILL IMMEDIATE.
//
// Retrying those would turn a clear error into a slow one, and the operator waiting
// for it learns nothing from the wait. Only the errno that means "not yet" is
// treated as "not yet".
func TestAnUnmapThatFailsForAnyOtherReasonIsNotRetried(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		unmaps int
	)

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")

			switch {
			case strings.Contains(joined, "device list"):
				return []byte(`[{"id":"0","pool":"billet-cache","name":"billet-probe",` +
					`"device":"/dev/rbd0"}]`), nil

			case strings.Contains(joined, "device unmap"):
				mu.Lock()
				defer mu.Unlock()

				unmaps++

				return nil, fmt.Errorf("rbd: unmap failed: (1) Operation not permitted")

			default:
				return nil, nil
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = c.DiscardRoot(t.Context(), "billet-probe")
	if err == nil {
		t.Fatal("an unmap that failed for a real reason was reported as success")
	}

	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("the error does not carry what rbd said: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if unmaps != 1 {
		t.Errorf("unmap was attempted %d times for a failure that will not resolve itself; "+
			"waiting on it makes a clear error into a slow one", unmaps)
	}
}
