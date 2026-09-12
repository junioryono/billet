package main

import "testing"

// pinnedHostAddresses is what every fixture-producing inspector report and
// every retirement test reads as this host's addresses: the retiring
// controller holds 10.0.0.1, the loopbacks and a link-local address.
var pinnedHostAddresses = []hostAddress{
	{Address: "127.0.0.1", Scope: "host", Interface: "lo", Index: 1},
	{Address: "::1", Scope: "host", Interface: "lo", Index: 1},
	{Address: "10.0.0.1", Scope: "global", Interface: "eth0", Index: 2},
	{Address: "fe80::1", Scope: "link", Interface: "eth0", Index: 2},
}

// pinHostAddresses stands the pinned list in for the kernel's, so a report
// is the same on every machine.
func pinHostAddresses(t *testing.T) {
	t.Helper()

	saved := hostInterfaceAddresses
	hostInterfaceAddresses = func() ([]hostAddress, error) { return pinnedHostAddresses, nil }

	t.Cleanup(func() { hostInterfaceAddresses = saved })
}
