//go:build !linux

package host

// isMountRootAt reports whether the directory name under dir is the root of a
// mount. macOS has no bind mounts, so a mount is a change of device, which the
// caller compares itself.
func isMountRootAt(int, string) (bool, error) { return false, nil }
