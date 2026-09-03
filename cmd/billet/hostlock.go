package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// hostLockDir is where the lifecycle lock lives. A variable so a test can put it
// somewhere writable; nothing else changes it.
//
// macOS HAS NO /var/lock AT ALL, so the Linux path is not merely unconventional
// there — every mutating lifecycle command would fail to open its lock before
// doing anything else. And these commands refuse to run as root on macOS,
// because a launch agent lives in a logged-in user's domain, so the lock belongs
// somewhere that account owns rather than in a root-owned system directory.
var hostLockDir = defaultHostLockDir()

func defaultHostLockDir() string {
	// THE SAME SEAM EVERY OTHER PLATFORM DECISION IN THIS PACKAGE USES, so a
	// test pinning hostOS gets the lock path that goes with it rather than the
	// one belonging to the machine running the test.
	if hostOS != "darwin" {
		return "/var/lock"
	}

	// The account database rather than $HOME: what is being excluded is two
	// commands run by the same operator, and a redirected HOME would give them
	// two different locks and no exclusion at all.
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, "Library", "Application Support", "billet")
	}

	return filepath.Join(os.TempDir(), "billet")
}

// hostLock keeps two lifecycle commands from interleaving on one machine.
//
// WHAT IT PREVENTS is a pair that each behave correctly alone: `billet local up`
// starting the services that `billet local down` has just proved idle and is
// about to stop, or two `down`s where one seals while the other has already
// decided what the ledger says. Both produce a host whose services and whose
// admission row disagree, and neither command has any way to notice.
//
// WHAT IT DOES NOT PREVENT, stated because the gap matters: anything on another
// machine. A control plane elsewhere can be resumed while this waits, and an
// operator can run `billet resume` from anywhere. That is not a lock's job —
// the admission generation is what makes such a change observable, and the
// commands re-read it before acting.
//
// It is NOT the state directory lock. That one excludes a second control plane;
// this one excludes a second lifecycle command, and a host with no server has no
// state directory to lock at all.
type hostLock struct{ file *os.File }

// takeHostLock acquires it, or says who has it in terms an operator can act on.
//
// It takes no configuration: the exclusion is between COMMANDS on this machine,
// not between deployments, and a host with no server section has no state
// directory to key it off anyway.
func takeHostLock() (*hostLock, error) {
	if err := os.MkdirAll(hostLockDir, 0o755); err != nil {
		return nil, fmt.Errorf("billet local: make the lock directory %s: %w", hostLockDir, err)
	}

	path := filepath.Join(hostLockDir, "billet-lifecycle.lock")

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("billet local: open the lock at %s: %w", path, err)
	}

	// NON-BLOCKING, because waiting would turn a mistake into a hang. The thing
	// being waited for can be a six-hour drain, and the operator who started a
	// second command wants to be told what is already running, not queued behind
	// it with no output.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()

		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("billet local: another billet lifecycle command is already "+
				"running on this machine. Running two at once leaves this host's services and "+
				"its admission state disagreeing — wait for it to finish, or look at %s", path)
		}

		return nil, fmt.Errorf("billet local: lock %s: %w", path, err)
	}

	return &hostLock{file: file}, nil
}

// release drops the lock. Closing the descriptor releases it, so this cannot
// leave one held by a process that has exited.
func (l *hostLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	return l.file.Close()
}
