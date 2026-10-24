//go:build darwin

package usage

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// hostNet reads a device's received and sent bytes and packets: from the running
// kernel's interface list on a Mac, and from a fixture's /sys/class/net under any
// other root.
func (r Reader) hostNet(t Target) ([4]int64, bool) {
	if r.Root != "/" {
		return r.sysfsNet(t)
	}
	v, err := interfaceCounters(t.NetDevice)

	return v, err == nil
}

// interfaceCounters reads an interface's counters from the kernel's interface
// list in the form whose counters are 64-bit (NET_RT_IFLIST2, an RTM_IFINFO2
// message carrying an if_data64 per interface): received and sent bytes, then
// received and sent packets.
func interfaceCounters(name string) ([4]int64, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return [4]int64{}, fmt.Errorf("usage: find interface %s: %w", name, err)
	}
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBType(unix.NET_RT_IFLIST2), 0)
	if err != nil {
		return [4]int64{}, fmt.Errorf("usage: read the interface list: %w", err)
	}
	v, found, err := countersIn(rib, ifi.Index)
	if err != nil {
		return [4]int64{}, err
	}
	if !found {
		return [4]int64{}, fmt.Errorf("usage: interface %s (index %d) is not in the interface list", name, ifi.Index)
	}

	return v, nil
}

// countersIn finds the RTM_IFINFO2 message for the interface with index in an
// interface list, and reads its counters. The whole list is read first: one
// that does not parse, or that holds the interface twice, answers nothing.
func countersIn(rib []byte, index int) ([4]int64, bool, error) {
	var v [4]int64
	found := false
	for len(rib) > 0 {
		if len(rib) < 4 {
			return [4]int64{}, false, fmt.Errorf("usage: the interface list ends in %d stray bytes", len(rib))
		}
		n := int(binary.NativeEndian.Uint16(rib))
		if n < 4 || n > len(rib) {
			return [4]int64{}, false, fmt.Errorf("usage: an interface list message claims %d of %d bytes",
				n, len(rib))
		}
		msg := rib[:n]
		rib = rib[n:]
		if msg[3] != unix.RTM_IFINFO2 {
			continue
		}
		if n < unix.SizeofIfMsghdr2 {
			return [4]int64{}, false, fmt.Errorf("usage: an interface message of %d bytes is shorter than %d",
				n, unix.SizeofIfMsghdr2)
		}
		// COPIED, NOT CAST: a message starts wherever the one before it ended,
		// which need not suit the header's 64-bit counters.
		var h unix.IfMsghdr2
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&h)), unix.SizeofIfMsghdr2), msg)
		if int(h.Index) != index {
			continue
		}
		if found {
			return [4]int64{}, false, fmt.Errorf("usage: the interface list holds interface %d twice", index)
		}
		d := h.Data
		v, found = [4]int64{int64(d.Ibytes), int64(d.Obytes), int64(d.Ipackets), int64(d.Opackets)}, true
	}

	return v, found, nil
}
