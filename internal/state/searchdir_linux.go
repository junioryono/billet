package state

import "golang.org/x/sys/unix"

// searchOnlyFlag opens a directory for traversal without requiring read.
//
// O_PATH is Linux's spelling. The descriptor supports fstat and serves as the
// dirfd for openat, which is all the lock needs — it does NOT support fchmod,
// which is why only the operator-chosen directory uses this: billet's own
// default may have to be tightened, and that path keeps O_RDONLY.
func searchOnlyFlag() int { return unix.O_PATH | unix.O_DIRECTORY }
