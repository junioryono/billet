//go:build !unix

package setup

import "os"

// fileOwner has no POSIX ownership to read on this platform.
func fileOwner(os.FileInfo) (int, int, bool) { return 0, 0, false }
