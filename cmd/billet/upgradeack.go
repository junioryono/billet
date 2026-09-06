package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/node"
)

// upgradeAck reports whether the updater took responsibility. Progress after
// acceptance belongs to the recovery journal.
type upgradeAck struct {
	path string
	sent bool
}

// newUpgradeAck accepts an empty path for an operator running the command by hand.
func newUpgradeAck(path string) *upgradeAck {
	return &upgradeAck{path: path}
}

// accept follows every preflight that can refuse without consequence.
func (a *upgradeAck) accept() { a.send(node.AckAccepted) }

// refuse leaves an acceptance already sent alone.
func (a *upgradeAck) refuse(err error) {
	if err == nil {
		return
	}

	a.send(node.AckRefused + strings.ReplaceAll(err.Error(), "\n", " "))
}

func (a *upgradeAck) send(line string) {
	if a.path == "" || a.sent {
		return
	}

	a.sent = true

	// Best effort: a node that has gone away cannot hold a claimed transaction.
	conn, err := net.DialTimeout("unix", a.path, 5*time.Second) //nolint:noctx // best-effort answer has its own bound, independent of an interrupted transaction
	if err != nil {
		return
	}

	defer func() { _ = conn.Close() }()

	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}

	// Leave room for the newline in the reader's MaxAckBytes+1 limit.
	if len(line) > node.MaxAckBytes {
		line = line[:node.MaxAckBytes]
	}

	_, _ = fmt.Fprintln(conn, line)
}

// close gives an unexplained exit a refusal the node can act on.
func (a *upgradeAck) close() {
	a.send(node.AckRefused + "this updater stopped without saying why; look at " +
		upgradeRoot + " on that machine")
}
