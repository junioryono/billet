//go:build !linux

package blockfs

import "errors"

// loopBacking has no loop devices to ask about off Linux; a host that lists one is one
// this build cannot read.
func loopBacking(string) (uint64, bool, error) {
	return 0, false, errors.New("loop devices are read only on linux")
}
