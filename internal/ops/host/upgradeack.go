package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/node"
)

// answerBudget is how long after it starts an updater a node waits on may take
// to answer: the node's ackWait is 90 s from the spawn, and the margin leaves
// room for giving a claim back and sending a refusal inside it.
const answerBudget = 75 * time.Second

// errAnswerLate is a transaction whose preflight outran the time the node
// waits for its answer, so the node will already have counted a refusal.
var errAnswerLate = errors.New("this updater could not resolve and stage the release within the time its " +
	"node waits for an answer, so the node has counted this as a refusal; the rollout retries it")

// errAcceptanceLost is an acceptance the waiting node did not take. A
// transaction it does not know was taken must not go on to stop anything.
var errAcceptanceLost = errors.New("the node waiting for this updater's answer did not take its acceptance, " +
	"so nothing here goes on; the rollout retries it")

// upgradeAck reports whether the updater took responsibility. Progress after
// acceptance belongs to the recovery journal.
type upgradeAck struct {
	path string
	sent bool
	// deadline is when a waiting node stops listening, less answerBudget's
	// margin; zero when nobody waits.
	deadline time.Time
}

// newUpgradeAck accepts an empty path for an operator running the command by
// hand. A path starts the answer's clock, so it is made when the command
// starts.
func newUpgradeAck(path string) *upgradeAck {
	a := &upgradeAck{path: path}
	if path != "" {
		a.deadline = time.Now().Add(answerBudget)
	}

	return a
}

// bound is ctx ending when a waiting node stops listening, so the resolve and
// the staging before the answer cannot outrun it; ctx itself when nobody waits.
func (a *upgradeAck) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.deadline.IsZero() || a.sent {
		return ctx, func() {}
	}

	return context.WithDeadline(ctx, a.deadline)
}

// late is errAnswerLate once the answer's time has passed with nothing sent.
func (a *upgradeAck) late() error {
	if !a.deadline.IsZero() && !a.sent && !time.Now().Before(a.deadline) {
		return errAnswerLate
	}

	return nil
}

// accept follows every preflight that can refuse without consequence, and
// reports whether a waiting node took it; with nobody waiting it is taken.
func (a *upgradeAck) accept() bool { return a.send(node.AckAccepted) }

// refuse leaves an acceptance already sent alone.
func (a *upgradeAck) refuse(err error) {
	if err == nil {
		return
	}

	a.send(node.AckRefused + strings.ReplaceAll(err.Error(), "\n", " "))
}

// send writes the answer once and reports whether it was delivered. Nobody
// waiting, or an answer already sent, delivers nothing more and is not a
// failure.
func (a *upgradeAck) send(line string) bool {
	if a.path == "" || a.sent {
		return true
	}

	a.sent = true

	conn, err := net.DialTimeout("unix", a.path, 5*time.Second) //nolint:noctx // the answer has its own bound, independent of an interrupted transaction
	if err != nil {
		return false
	}

	defer func() { _ = conn.Close() }()

	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return false
	}

	// Leave room for the newline in the reader's MaxAckBytes+1 limit.
	if len(line) > node.MaxAckBytes {
		line = line[:node.MaxAckBytes]
	}

	_, err = fmt.Fprintln(conn, line)

	return err == nil
}

// close gives an unexplained exit a refusal the node can act on.
func (a *upgradeAck) close() {
	a.send(node.AckRefused + "this updater stopped without saying why; look at " +
		upgradeRoot + " on that machine")
}
