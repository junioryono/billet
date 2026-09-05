package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// notifyReady tells systemd initialization completed when this process has a
// notification socket. Other service managers and interactive runs are no-ops.
func notifyReady() error {
	return notifySystemd("READY=1", "readiness")
}

// upgradeProbeReady is the sentence a HOLDING probe prints once it has opened
// what it inherited, and serverProbeReadyLine and nodeProbeReadyFormat are the
// two whole lines it appears in. `billet host-upgrade` matches those lines, so
// they are constants here and not strings that happen to agree.
const (
	upgradeProbeReady    = "upgrade probe ready"
	serverProbeReadyLine = "billet server: " + upgradeProbeReady +
		"; workload polling and dispatch are disabled"
	nodeProbeReadyFormat = "billet node %s: " + upgradeProbeReady +
		"; registration and workload polling are disabled"
)

// holdProbe decides what a ready probe does next: announce itself and stay up
// for the service manager that started it, or exit silently for the parent that
// is waiting on it.
//
// TWO CALLERS WITH OPPOSITE CONTRACTS. The Ansible host role runs the probe as
// the unit's own ExecStart under Type=notify and stops it itself, so there it
// must stay up until told to. `billet host-upgrade` runs it as a plain child and
// waits for it to EXIT, and a probe that waited there instead hung every
// self-upgrade at the probe step with the services already stopped: measured
// 2026-09-05 in the rollout rehearsal, seventeen minutes at ctx.Done() with
// nobody who would ever tell it to stop.
//
// THE FLAG DECIDES, NOT THE ENVIRONMENT. A version of this read NOTIFY_SOCKET as
// the tell, and it is not one: a node's detached updater inherits its unit's
// socket, so would the candidate it runs, and a node-dispatched upgrade would
// have held exactly as before. Only the role's probe unit passes
// --upgrade-probe-hold, so only the role's probe holds.
//
// AND THE LINE IS PRINTED ONLY BY A PROBE THAT WILL HOLD. Every release through
// v0.9.0 printed it and then held, so to the parent the line MEANS "I will not
// exit on my own; stop me". A probe that exits once ready says nothing, and its
// exit status is its whole answer. Keeping those two shapes disjoint is what lets
// the parent stop a holder without ever mistaking a stop for a verdict.
func holdProbe(ctx context.Context, hold bool, line string) {
	if !hold {
		return
	}

	fmt.Println(line)

	<-ctx.Done()
}

// notifyStatus tells the service manager what this process is doing right now.
//
// WHAT IT IS FOR IS A STANDBY, and without it `systemctl status` on a waiting
// controller says "active (running)" and nothing else — which is true and
// answers none of the question an operator actually has, namely which machine
// holds the deployment. READY=1 is sent BEFORE the wait, because the unit is
// Type=notify with TimeoutStartSec=120 and withholding readiness would have
// systemd kill a standby at two minutes forever — so the readiness line cannot
// carry this and a second message has to.
func notifyStatus(text string) error {
	// NEWLINES ARE STRIPPED RATHER THAN REFUSED. The protocol is newline-separated
	// key=value, so an embedded one would inject a second assignment; the only
	// thing that reaches here is billet's own sentence about a claim holder, and
	// the holder is a string an operator chose. Truncating the status is a worse
	// answer than flattening it.
	return notifySystemd("STATUS="+strings.ReplaceAll(text, "\n", " "), "status")
}

// notifySystemd sends one datagram to the service manager, or does nothing.
//
// ONE IMPLEMENTATION FOR BOTH MESSAGES, because the socket handling is the part
// that is easy to get wrong — an abstract socket's leading '@' has to become a
// NUL, and every service manager other than systemd sets no NOTIFY_SOCKET at all.
func notifySystemd(message, what string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}

	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("dial systemd notification socket: %w", err)
	}

	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("send systemd %s: %w", what, err)
	}

	return nil
}
