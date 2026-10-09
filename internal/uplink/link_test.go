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

// WHAT MOVED IS THE DIFFERENCE IN BITS, and a counter that restarted moved nothing.
func TestCountersSinceIsBitsAndToleratesARestart(t *testing.T) {
	t.Parallel()

	sent, received := Counters{Sent: 1500, Received: 10}.Since(Counters{Sent: 500, Received: 20})
	if sent != 8000 || received != 0 {
		t.Fatalf("sent %d received %d bits; want 8000 and 0", sent, received)
	}
}
