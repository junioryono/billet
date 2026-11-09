package firecracker

import (
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A CGROUP THE KERNEL IS STILL RELEASING IS WAITED FOR, AND THEN REMOVED: its process was
// proved gone, and on 6.8 the cgroup answers EBUSY for a moment after.
func TestACgroupStillBeingReleasedIsRemovedOnceItIsEmpty(t *testing.T) {
	t.Parallel()

	busy := 3
	calls := 0

	var slept time.Duration

	err := removeEmptiedCgroup("/sys/fs/cgroup/fc/billet-x", func(string) error {
		calls++
		if calls <= busy {
			return unix.EBUSY
		}

		return nil
	}, func(d time.Duration) { slept += d }, cgroupReleaseWait)
	if err != nil {
		t.Fatalf("removeEmptiedCgroup = %v", err)
	}

	if calls != busy+1 || slept != time.Duration(busy)*cgroupReleasePoll {
		t.Fatalf("tried %d times and waited %s, want %d tries and %s", calls, slept, busy+1,
			time.Duration(busy)*cgroupReleasePoll)
	}
}

// ONE THAT STAYS BUSY IS REFUSED, as one rmdir refused it before: nothing is forced, and a
// cgroup a live process holds is a failure the teardown must hear.
func TestACgroupThatStaysBusyIsRefusedAfterTheWait(t *testing.T) {
	t.Parallel()

	var slept time.Duration

	err := removeEmptiedCgroup("/sys/fs/cgroup/fc/billet-x", func(string) error { return unix.EBUSY },
		func(d time.Duration) { slept += d }, 200*time.Millisecond)
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("removeEmptiedCgroup = %v, want EBUSY", err)
	}

	if slept != 200*time.Millisecond {
		t.Fatalf("waited %s, want the bound", slept)
	}
}

// ANY OTHER REFUSAL IS NOT WAITED ON, AND AN ABSENT CGROUP IS GONE.
func TestOnlyABusyCgroupIsWaitedFor(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"not empty":  {unix.ENOTEMPTY, unix.ENOTEMPTY},
		"permission": {unix.EACCES, unix.EACCES},
		"absent":     {os.ErrNotExist, nil},
	} {
		calls := 0
		err := removeEmptiedCgroup("/x", func(string) error {
			calls++

			return tc.err
		}, func(time.Duration) { t.Fatalf("%s: waited", name) }, cgroupReleaseWait)

		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) || calls != 1 {
			t.Errorf("%s: err = %v after %d calls, want %v after 1", name, err, calls, tc.want)
		}
	}
}
