package state

import "golang.org/x/sys/unix"

// searchOnlyFlag opens a directory for traversal without requiring read.
//
// FreeBSD exports O_SEARCH from x/sys/unix directly, so unlike darwin there is
// no constant to spell out here. It is listed separately from the generic unix
// fallback for exactly that reason: the fallback exists for platforms that have
// no such facility, and letting FreeBSD land there would have degraded a host
// that can do this perfectly well — refusing a documented drop-box lock
// directory on a platform that supports it.
func searchOnlyFlag() int { return unix.O_SEARCH | unix.O_DIRECTORY }
