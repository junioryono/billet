//go:build darwin

package app

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the user on the far end of conn, as the kernel recorded it when it connected.
func peerUID(conn *net.UnixConn) (int, bool, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false, err
	}

	var (
		cred    *unix.Xucred
		credErr error
	)

	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, false, err
	}

	if credErr != nil {
		return 0, false, credErr
	}

	return int(cred.Uid), true, nil
}
