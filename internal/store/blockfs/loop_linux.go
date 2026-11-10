package blockfs

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// loopBacking asks the kernel for a loop device's backing (LOOP_GET_STATUS64's
// lo_rdevice, the backing inode's device number, encoded as stat encodes one). A loop with
// nothing behind it answers ENXIO, which is not configured.
func loopBacking(loop string) (uint64, bool, error) {
	fd, err := unix.Open("/dev/"+loop, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, false, fmt.Errorf("open /dev/%s: %w", loop, err)
	}
	defer unix.Close(fd)

	info, err := unix.IoctlLoopGetStatus64(fd)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return 0, false, nil
		}

		return 0, false, err
	}

	return info.Rdevice, true, nil
}
