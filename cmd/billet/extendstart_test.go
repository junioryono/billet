package main

import (
	"testing"
	"time"
)

// THE SERVER'S HOST ASKS SYSTEMD FOR TIME, through the process's own
// notification socket. The control plane asks only through what serverHost
// hands it, so a host built without the callback would leave a cold ledger
// scan to be killed by the unit's start bound however the assembly is tested.
func TestTheServersHostAsksSystemdForTimeToOpen(t *testing.T) {
	path, listener := notifySocket(t)
	t.Setenv("NOTIFY_SOCKET", path)

	host := serverHost(processEnv())
	if host.ExtendStart == nil {
		t.Fatal("the server's host has no ExtendStart, so the control plane asks systemd for nothing")
	}

	if err := host.ExtendStart(10 * time.Second); err != nil {
		t.Fatalf("ExtendStart: %v", err)
	}

	if got, want := readNotification(t, listener), "EXTEND_TIMEOUT_USEC=130000000"; got != want {
		t.Errorf("the server's host sent %q, want %q (10s to open plus the unit's 120s)", got, want)
	}
}
