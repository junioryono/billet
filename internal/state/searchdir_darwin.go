package state

import "golang.org/x/sys/unix"

// oEXEC is darwin's O_EXEC. x/sys/unix does not generate it, which is exactly
// how the wrong conclusion got drawn: absence from a Go constant list was read
// as absence of the facility, and the SDK defines
// `O_SEARCH (O_EXEC | O_DIRECTORY)` — "open directory for search only".
const oEXEC = 0x40000000

// searchOnlyFlag opens a directory for traversal without requiring read.
//
// Measured on darwin against a directory the caller cannot read: the open
// succeeds, fstat reports mode and gid, and openat through the handle both opens
// an existing lock file and creates a new one.
func searchOnlyFlag() int { return oEXEC | unix.O_DIRECTORY }
