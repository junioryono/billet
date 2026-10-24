//go:build darwin

package usage

import (
	"net"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ifInfo2 is one RTM_IFINFO2 message for the interface with index, with the
// given counters.
func ifInfo2(index uint16, ibytes, obytes, ipackets, opackets uint64) []byte {
	h := unix.IfMsghdr2{Msglen: unix.SizeofIfMsghdr2, Type: unix.RTM_IFINFO2, Index: index}
	h.Data.Ibytes, h.Data.Obytes, h.Data.Ipackets, h.Data.Opackets = ibytes, obytes, ipackets, opackets

	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(&h)), unix.SizeofIfMsghdr2)...)
}

// AN INTERFACE'S COUNTERS ARE READ FROM ITS OWN MESSAGE, past messages of
// another type and other interfaces, at whatever offset it starts; a list that
// does not parse refuses rather than answering nothing.
func TestAnInterfacesCountersAreReadFromItsOwnMessage(t *testing.T) {
	t.Parallel()

	// An address message of 12 bytes first, so the one sought starts at an
	// offset no 64-bit counter is aligned to.
	other := []byte{12, 0, 5, unix.RTM_NEWMADDR2, 0, 0, 0, 0, 0, 0, 0, 0}
	rib := append(append(append(other, ifInfo2(3, 1, 2, 3, 4)...), ifInfo2(25, 125115, 20964202, 1644,
		13931)...), ifInfo2(27, 9, 9, 9, 9)...)

	v, found, err := countersIn(rib, 25)
	if err != nil || !found || v != [4]int64{125115, 20964202, 1644, 13931} {
		t.Errorf("read %v (found %v, %v), want vmenet0's 125115 in, 20964202 out, 1644 and 13931 packets",
			v, found, err)
	}
	if _, found, err := countersIn(rib, 99); err != nil || found {
		t.Errorf("an interface the list does not hold answered found %v, %v", found, err)
	}
	// WITH THE INTERFACE SOUGHT IN THE LIST, so nothing is answered from a list
	// whose rest does not parse.
	for name, bad := range map[string][]byte{
		"a stray tail":               append(append([]byte(nil), rib...), 1, 2),
		"a length past the list":     append(append([]byte(nil), rib...), 200, 0, 5, unix.RTM_IFINFO2),
		"a short interface after it": append(append([]byte(nil), rib...), 8, 0, 5, unix.RTM_IFINFO2, 0, 0, 0, 0),
		"the interface listed twice": append(append([]byte(nil), rib...), ifInfo2(25, 1, 1, 1, 1)...),
	} {
		if v, found, err := countersIn(bad, 25); err == nil || found {
			t.Errorf("%s was read as a list: %v (found %v)", name, v, found)
		}
	}
}

// THE RUNNING KERNEL'S COUNTERS MOVE WITH WHAT CROSSES THE INTERFACE: traffic
// sent over the loopback is counted on lo0, in and out.
func TestTheKernelsInterfaceCountersMove(t *testing.T) {
	t.Parallel()

	before, err := interfaceCounters("lo0")
	if err != nil {
		t.Fatalf("read lo0: %v", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	self, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("the socket's address is a %T", conn.LocalAddr())
	}
	payload := []byte(strings.Repeat("x", 1000))
	for range 100 {
		if _, err := conn.WriteToUDP(payload, self); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	after, err := interfaceCounters("lo0")
	if err != nil {
		t.Fatalf("read lo0 again: %v", err)
	}
	for i, what := range []string{"bytes in", "bytes out", "packets in", "packets out"} {
		want := int64(100_000)
		if i >= 2 {
			want = 100
		}
		if got := after[i] - before[i]; got < want {
			t.Errorf("lo0's %s moved %d, want at least %d", what, got, want)
		}
	}
	if _, err := interfaceCounters("vmenet9999"); err == nil {
		t.Error("an interface that does not exist was read")
	}
}
