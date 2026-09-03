package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/junioryono/billet/internal/node"
)

// upgradeAck tells whoever started this updater whether it took the job.
//
// WHY THIS EXISTS AT ALL: THE UPDATER IS DETACHED, AND A SPAWN IS NOT AN ANSWER.
// A node that dispatched an upgrade returns as soon as the process starts, so
// every refusal this program makes before it touches anything — a digest that
// disagrees with the fleet's decision, a candidate this build cannot run, a claim
// another upgrade already holds — was invisible to the control plane. The
// coordinator recorded the host as draining and waited forever: the host keeps
// running the old release, stays live, reports the same version, and no
// registration ever contradicts it. One machine wedged the whole rollout, with
// nothing anywhere saying why. A review caught it, and it was made worse by this
// very change adding a new refusal.
//
// IT IS NOT A PROGRESS CHANNEL. It reports exactly one thing — did this updater
// accept responsibility — and closes. Everything after that point is on the disk,
// in the journal, which is what a resume and an operator read.
type upgradeAck struct {
	f    *os.File
	sent bool
}

// newUpgradeAck adopts the descriptor the caller passed, or nothing.
//
// AN ABSENT DESCRIPTOR IS NOT AN ERROR: an operator runs this command by hand and
// has a terminal to read instead.
func newUpgradeAck(fd int) *upgradeAck {
	if fd <= 0 {
		return &upgradeAck{}
	}

	return &upgradeAck{f: os.NewFile(uintptr(fd), "upgrade-ack")}
}

// accept says this updater has taken responsibility and the caller may stop
// waiting. Everything that can refuse without consequence has already run.
func (a *upgradeAck) accept() { a.send(node.AckAccepted) }

// refuse reports why nothing was done. A nil error, or an acceptance already
// sent, writes nothing.
func (a *upgradeAck) refuse(err error) {
	if err == nil {
		return
	}

	a.send(node.AckRefused + strings.ReplaceAll(err.Error(), "\n", " "))
}

func (a *upgradeAck) send(line string) {
	if a.f == nil || a.sent {
		return
	}

	a.sent = true

	// BOUNDED WITH ROOM FOR THE NEWLINE THIS ADDS. The reader takes MaxAckBytes+1
	// so that a maximal payload still arrives terminated; truncating to the limit
	// and then appending would be one byte over on the wire, and an answer the
	// reader saw unterminated is one it refuses.
	if len(line) > node.MaxAckBytes {
		line = line[:node.MaxAckBytes]
	}

	// BEST EFFORT, AND DELIBERATELY NOT FATAL. The caller may have gone away, and
	// an updater that refused to proceed because nobody was listening would turn a
	// diagnostic channel into a dependency of the upgrade.
	_, _ = fmt.Fprintln(a.f, line)

	// CLOSED IMMEDIATELY, so a caller reading this gets EOF rather than waiting on
	// a descriptor an unbounded drain is holding open.
	_ = a.f.Close()
	a.f = nil
}

// close ends the channel for an updater that neither accepted nor refused.
//
// AN UNEXPLAINED SILENCE IS STILL AN ANSWER, and it has to be one the reader can
// act on: closing gives it EOF now instead of a wait that ends in a timeout the
// operator then has to interpret.
func (a *upgradeAck) close() {
	if a.f == nil {
		return
	}

	if !a.sent {
		a.send(node.AckRefused + "this updater stopped without saying why; look at " +
			upgradeRoot + " on that machine")

		return
	}

	_ = a.f.Close()
	a.f = nil
}
