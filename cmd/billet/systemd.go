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

	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("send systemd readiness: %w", err)
	}

	return nil
}
