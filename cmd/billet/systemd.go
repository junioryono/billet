package main

import (
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
