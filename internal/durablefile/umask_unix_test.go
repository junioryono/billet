//go:build unix

package durablefile

import "syscall"

// setUmask narrows the process umask and returns what it was.
//
// BUILD-TAGGED FOR THE SAME REASON THE STATE LOCK IS. syscall.Umask does not exist
// on windows, and billet does not run there — a port would need this file's
// equivalent along with the flock.
func setUmask(mask int) int { return syscall.Umask(mask) }
