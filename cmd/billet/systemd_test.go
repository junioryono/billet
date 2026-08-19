package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyReadyIsOptional(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")

	if err := notifyReady(); err != nil {
		t.Fatalf("notify without systemd: %v", err)
	}
}

func TestNotifyReadySignalsSystemdSocket(t *testing.T) {
	dir := t.TempDir()
	digest := sha256.Sum256([]byte(dir))
	shortDir := "/tmp/bn-" + hex.EncodeToString(digest[:6])
	if err := os.Symlink(dir, shortDir); err != nil {
		t.Fatalf("create short readiness path: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(shortDir) })
	path := filepath.Join(shortDir, "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen for readiness: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("NOTIFY_SOCKET", path)

	if err := notifyReady(); err != nil {
		t.Fatalf("notify readiness: %v", err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set readiness deadline: %v", err)
	}

	message := make([]byte, 64)
	n, _, err := listener.ReadFromUnix(message)
	if err != nil {
		t.Fatalf("read readiness: %v", err)
	}
	if got, want := string(message[:n]), "READY=1"; got != want {
		t.Fatalf("readiness message = %q, want %q", got, want)
	}
}
