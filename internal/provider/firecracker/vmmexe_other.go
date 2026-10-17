//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"net"
	"os"
)

// apiPeerExecutable needs the kernel's credentials for a socket's peer and a process
// handle, which only Linux gives in the form this check needs. Firecracker runs only
// there.
func apiPeerExecutable(context.Context, string, int) (*os.File, error) {
	return nil, errors.New("firecracker: telling which executable serves a VMM socket needs Linux")
}

// peerUID needs the kernel's credentials for a socket's peer, which this check reads
// only on Linux.
func peerUID(net.Conn) (int, error) {
	return 0, errors.New("firecracker: telling whose process answers a VMM socket needs Linux")
}
