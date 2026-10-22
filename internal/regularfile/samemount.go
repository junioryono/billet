package regularfile

import (
	"fmt"
	"os"
)

// Mount is where an open descriptor lives: the device its stat reports and,
// where the kernel says, the mount it was reached through.
//
// THE DEVICE ALONE DOES NOT SAY WHICH MOUNT A FILE IS ON. On an overlay whose
// layers span more than one filesystem and that has no effective xino, a
// directory reports the overlay's device and a file the device of the layer it
// lives in (#123), so a lock billet created beside its directory reads as foreign.
// And a bind mount from the same filesystem shares the device, so a file mounted
// onto a name billet trusts reads as native. The mount identity answers both: a
// file mounted onto a name is the root of a mount of its own, which is never
// the mount of the directory holding the name.
type Mount struct {
	// Dev is the device the descriptor's stat reports.
	Dev uint64
	// Known says the kernel reported the mount identity; without it only Dev is
	// an answer.
	Known bool
	// ID is the mount the descriptor was reached through.
	ID uint64
}

// Holds reports whether entry lives on the mount dir is on. Where either side's
// mount identity is unknown it is the device comparison, which is what billet
// answered before statx reported a mount, so an old kernel is never more
// permissive than before. A side that observed nothing (the zero Mount: no
// mounted filesystem has device 0) holds nothing and is held by nothing.
func (dir Mount) Holds(entry Mount) bool {
	if dir.Known && entry.Known {
		return entry.ID == dir.ID
	}

	return dir.Dev != 0 && entry.Dev == dir.Dev
}

// MountOf reports where an open descriptor lives.
func MountOf(f *os.File) (Mount, error) {
	m, err := mountOf(f)
	if err != nil {
		return Mount{}, fmt.Errorf("examine where %s is mounted: %w", f.Name(), err)
	}

	return m, nil
}
