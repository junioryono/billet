//go:build unix

package node

import "syscall"

// detachedAttr puts the updater in its own session.
//
// SETSID RATHER THAN SETPGID, and the difference matters here. A new process
// GROUP escapes a group-directed signal; a new SESSION additionally detaches from
// the controlling terminal, so an operator who started the node from a shell and
// then closes it does not send SIGHUP into a transaction that is midway through
// replacing a binary.
//
// The same lesson as the guest launcher one package over, where nohup, a
// subshell, a double fork and systemd-run --user all failed and only a genuine
// setsid survived — measured, not reasoned about.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
