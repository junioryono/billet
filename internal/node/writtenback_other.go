//go:build !linux

package node

import "errors"

// syncFilesystem cannot ask anything off Linux, where the node mounts no cache
// volume of its own, so it says it could not tell: nothing it answers for is
// published on its word.
func syncFilesystem(string) error {
	return errors.New("node: a mounted cache volume is written back only on Linux")
}
