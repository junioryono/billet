//go:build !unix

package main

import "os"

// openForInspection is the portable fallback. O_NONBLOCK is a POSIX concept and
// Windows has no FIFO in the sense that blocks here, so a plain open is
// equivalent on the platforms this file covers.
func openForInspection(path string) (*os.File, error) {
	return os.Open(path)
}
