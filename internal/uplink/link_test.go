package uplink

import "testing"

// THE UPLINK IS THE DEFAULT ROUTE'S INTERFACE, the lowest metric of those up.
// The table is /proc/net/route's format as the reference node printed it.
func TestTheUplinkIsTheDefaultRoutesInterface(t *testing.T) {
	t.Parallel()

	table := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eno2np1	00000000	FE01A8C0	0003	0	0	100	00000000	0	0	0
wlan0	00000000	FE01A8C0	0003	0	0	600	00000000	0	0	0
eno9	00000000	FE01A8C0	0002	0	0	1	00000000	0	0	0
eno2np1	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
billet1	0001FAAC	00000000	0001	0	0	0	00FFFFFF	0	0	0
`

	iface, err := defaultInterface([]byte(table))
	if err != nil || iface != "eno2np1" {
		t.Fatalf("the uplink is %q (%v); want eno2np1, the lowest metric of the default routes that are up", iface, err)
	}

	if _, err := defaultInterface([]byte("Iface\tDestination\n")); err == nil {
		t.Fatal("a table with no default route named an uplink")
	}
}

// THE GATEWAY IS THE ONE THE SHAPED INTERFACE'S DEFAULT ROUTE GOES BY, read in
// the host's byte order: the reference node's FE01A8C0 is its gateway
// 192.168.1.254 (2026-10-16). A route without RTF_GATEWAY, or one by another
// interface, names none of the shaped interface's.
func TestTheGatewayIsTheShapedInterfacesDefaultRoutes(t *testing.T) {
	t.Parallel()

	table := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eno2np1	00000000	FE01A8C0	0003	0	0	100	00000000	0	0	0
eno2np1	00000000	0101A8C0	0003	0	0	900	00000000	0	0	0
wlan0	00000000	0102A8C0	0003	0	0	50	00000000	0	0	0
ppp0	00000000	00000000	0001	0	0	10	00000000	0	0	0
tun0	00000000	0100000A	0001	0	0	20	00000000	0	0	0
eno2np1	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`

	for iface, want := range map[string]string{
		"eno2np1": "192.168.1.254", // the lowest metric of its two
		"wlan0":   "192.168.2.1",
		"ppp0":    "", // a point-to-point link: no gateway to ask
		"tun0":    "", // an address the route is not flagged to go by (RTF_GATEWAY unset)
	} {
		_, gateway, err := defaultRoute([]byte(table), iface)
		if err != nil || gateway != want {
			t.Errorf("%s's gateway is %q (%v), want %q", iface, gateway, err, want)
		}
	}

	if _, _, err := defaultRoute([]byte(table), "eno9"); err == nil {
		t.Error("an interface with no default route named a gateway")
	}

	// THE UPLINK ITSELF IS STILL THE LOWEST METRIC OF ALL.
	if iface, err := defaultInterface([]byte(table)); err != nil || iface != "ppp0" {
		t.Errorf("the uplink is %q (%v), want ppp0", iface, err)
	}
}

// WHAT MOVED IS THE DIFFERENCE IN BITS, and a counter that restarted moved nothing.
func TestCountersSinceIsBitsAndToleratesARestart(t *testing.T) {
	t.Parallel()

	sent, received := Counters{Sent: 1500, Received: 10}.Since(Counters{Sent: 500, Received: 20})
	if sent != 8000 || received != 0 {
		t.Fatalf("sent %d received %d bits; want 8000 and 0", sent, received)
	}
}
