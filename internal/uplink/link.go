package uplink

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoute is the kernel's IPv4 routing table; a variable so a test can point
// it at a fixture.
var procRoute = "/proc/net/route"

// sysNet is where the kernel lists interfaces.
var sysNet = "/sys/class/net"

// DefaultInterface is the interface the IPv4 default route leaves by, the one
// with the lowest metric when there are several: the uplink, without asking.
func DefaultInterface() (string, error) {
	body, err := os.ReadFile(procRoute)
	if err != nil {
		return "", fmt.Errorf("read the routing table: %w", err)
	}

	return defaultInterface(body)
}

func defaultInterface(table []byte) (string, error) {
	iface, _, err := defaultRoute(table, "")

	return iface, err
}

// defaultRoute is the IPv4 default route with the lowest metric, leaving by
// iface when it is named: the interface it leaves by and its gateway, in the
// spelling a reply's source takes, or empty for a route with none.
func defaultRoute(table []byte, iface string) (string, string, error) {
	best, gateway, bestMetric := "", "", -1
	scanner := bufio.NewScanner(bytes.NewReader(table))

	for first := true; scanner.Scan(); first = false {
		fields := strings.Fields(scanner.Text())
		if first || len(fields) < 8 {
			continue
		}

		// Iface Destination Gateway Flags RefCnt Use Metric Mask: a default
		// route is destination and mask zero, and up (flag 0x1).
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || fields[1] != "00000000" || fields[7] != "00000000" || flags&1 == 0 {
			continue
		}

		if iface != "" && fields[0] != iface {
			continue
		}

		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}

		if bestMetric < 0 || metric < bestMetric {
			best, gateway, bestMetric = fields[0], routeGateway(fields[2], flags), metric
		}
	}

	if best == "" {
		if iface != "" {
			return "", "", fmt.Errorf("this host has no IPv4 default route leaving by %s", iface)
		}

		return "", "", errors.New("this host has no IPv4 default route, so there is no uplink to shape")
	}

	return best, gateway, nil
}

// routeGateway is a route's gateway as an IPv4 address, or empty when the route
// has none (flag 0x2, RTF_GATEWAY, unset, or an address of zero).
//
// THE TABLE PRINTS IT IN THE HOST'S BYTE ORDER, little-endian on the two Linux
// architectures billet builds for: the reference node's `FE01A8C0` is
// 192.168.1.254, its BGW320 (measured 2026-10-16).
func routeGateway(field string, flags uint64) string {
	value, err := strconv.ParseUint(field, 16, 32)
	if err != nil || value == 0 || flags&2 == 0 {
		return ""
	}

	return net.IPv4(byte(value), byte(value>>8), byte(value>>16), byte(value>>24)).String()
}

// DefaultGateway is the gateway of the lowest-metric IPv4 default route leaving
// by iface, or empty when that route has none. A table it cannot read, or no
// default route by iface, is an error.
func DefaultGateway(iface string) (string, error) {
	body, err := os.ReadFile(procRoute)
	if err != nil {
		return "", fmt.Errorf("read the routing table: %w", err)
	}

	_, gateway, err := defaultRoute(body, iface)

	return gateway, err
}

// Speed is the interface's link speed in Mbit/s, or zero when the kernel does
// not know it (a virtual interface reports -1 or nothing).
func Speed(iface string) float64 {
	body, err := os.ReadFile(filepath.Join(sysNet, iface, "speed"))
	if err != nil {
		return 0
	}

	speed, err := strconv.ParseFloat(strings.TrimSpace(string(body)), 64)
	if err != nil || speed <= 0 {
		return 0
	}

	return speed
}

// Counters are an interface's byte totals.
type Counters struct{ Sent, Received uint64 }

// ReadCounters reads the interface's byte totals.
func ReadCounters(iface string) (Counters, error) {
	tx, err := readCounter(iface, "tx_bytes")
	if err != nil {
		return Counters{}, err
	}

	rx, err := readCounter(iface, "rx_bytes")
	if err != nil {
		return Counters{}, err
	}

	return Counters{Sent: tx, Received: rx}, nil
}

func readCounter(iface, name string) (uint64, error) {
	body, err := os.ReadFile(filepath.Join(sysNet, iface, "statistics", name))
	if err != nil {
		return 0, fmt.Errorf("read %s's %s: %w", iface, name, err)
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s's %s: %w", iface, name, err)
	}

	return value, nil
}

// Since is what moved between two readings, in bits. A counter that went
// backwards (the interface was recreated) moved nothing that can be counted.
func (c Counters) Since(before Counters) (uint64, uint64) {
	var sent, received uint64

	if c.Sent >= before.Sent {
		sent = (c.Sent - before.Sent) * 8
	}

	if c.Received >= before.Received {
		received = (c.Received - before.Received) * 8
	}

	return sent, received
}

// ifindex is the kernel's index for an interface, or empty when it cannot say.
// ifaddr is iface's hardware address, or empty when it cannot be read or is
// none (a virtual device answers all zeroes).
func ifaddr(iface string) string {
	body, err := os.ReadFile(filepath.Join(sysNet, iface, "address"))
	if err != nil {
		return ""
	}

	addr := strings.TrimSpace(string(body))
	if strings.Trim(addr, "0:") == "" {
		return ""
	}

	return addr
}

func ifindex(iface string) string {
	body, err := os.ReadFile(filepath.Join(sysNet, iface, "ifindex"))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(body))
}

// errUndecided is an interface lookup that could not tell: never read as
// absence, because absence permits removing a device a redirect may still use.
var errUndecided = errors.New("could not tell which interface has the recorded index")

// nameForIndex is the interface that now has this index, and whether one has
// it: not found only when every interface was read and none has it; anything
// less is an error.
func nameForIndex(index string) (string, bool, error) {
	entries, err := os.ReadDir(sysNet)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", errUndecided, err)
	}

	for _, entry := range entries {
		got := ifindex(entry.Name())
		if got == "" {
			// AN ENTRY WHOSE INDEX COULD NOT BE READ may be the interface itself,
			// renamed between the listing and the read.
			return "", false, fmt.Errorf("%w: %s's index could not be read", errUndecided, entry.Name())
		}

		if got == index {
			return entry.Name(), true, nil
		}
	}

	return "", false, nil
}
