//go:build linux

package peercred

import (
	"net"

	"golang.org/x/sys/unix"
)

// UID is the user on the far end of conn, as the kernel recorded it when it connected.
func UID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var (
		cred    *unix.Ucred
		credErr error
	)

	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}

	if credErr != nil {
		return 0, credErr
	}

	return int(cred.Uid), nil
}
