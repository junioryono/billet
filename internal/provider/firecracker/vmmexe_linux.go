//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// apiPeerExecutable opens the executable of the process serving a jail's API socket.
//
// THE KERNEL NAMES THAT PROCESS, NOT THE JAIL. The pid file and a process's command
// line are both things the jailed account can write; the credentials of the process
// that called listen() on the socket billet is about to talk to are not. So the peer
// of a fresh connection is the VMM that will take the capture, and its uid must be
// the one this microVM was given.
//
// HELD BY A PIDFD while its executable is opened, and proved still alive after, so
// a pid the kernel reused in between is never mistaken for it.
func apiPeerExecutable(socket string, uid int) (*os.File, error) {
	conn, err := net.DialTimeout("unix", socket, apiTimeout)
	if err != nil {
		return nil, fmt.Errorf("firecracker: connect to %s: %w", socket, err)
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("firecracker: %s is not a unix socket", socket)
	}

	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("firecracker: read the peer of %s: %w", socket, err)
	}

	var (
		cred    *unix.Ucred
		credErr error
	)

	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("firecracker: read the peer of %s: %w", socket, err)
	}

	if credErr != nil {
		return nil, fmt.Errorf("firecracker: read the peer of %s: %w", socket, credErr)
	}

	if uid <= 0 || cred.Uid != uint32(uid) { //nolint:gosec // a uid billet claimed fits a uint32.
		return nil, fmt.Errorf("firecracker: %s is served by uid %d, not the microVM's uid %d",
			socket, cred.Uid, uid)
	}

	handle, alive, err := openVMM(int(cred.Pid))
	if err != nil {
		return nil, err
	}

	if !alive {
		return nil, fmt.Errorf("firecracker: the process serving %s has exited", socket)
	}
	defer handle.close()

	exe, err := os.Open(fmt.Sprintf("/proc/%d/exe", cred.Pid))
	if err != nil {
		return nil, fmt.Errorf("firecracker: open the executable serving %s: %w", socket, err)
	}

	if err := unix.PidfdSendSignal(handle.fd, 0, nil, 0); err != nil {
		return nil, errors.Join(fmt.Errorf("firecracker: the process serving %s exited while its "+
			"executable was opened: %w", socket, err), exe.Close())
	}

	return exe, nil
}
