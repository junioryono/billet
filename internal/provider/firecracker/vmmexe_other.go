//go:build !linux

package firecracker

import (
	"errors"
	"os"
)

// apiPeerExecutable needs the kernel's credentials for a socket's peer and a process
// handle, which only Linux gives in the form this check needs. Firecracker runs only
// there.
func apiPeerExecutable(string, int) (*os.File, error) {
	return nil, errors.New("firecracker: telling which executable serves a VMM socket needs Linux")
}
