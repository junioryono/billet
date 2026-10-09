package uplink

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
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
	best, bestMetric := "", -1
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

		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}

		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = fields[0], metric
		}
	}

	if best == "" {
		return "", errors.New("this host has no IPv4 default route, so there is no uplink to shape")
	}

	return best, nil
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
func (c Counters) Since(before Counters) (sent, received uint64) {
	if c.Sent >= before.Sent {
		sent = (c.Sent - before.Sent) * 8
	}

	if c.Received >= before.Received {
		received = (c.Received - before.Received) * 8
	}

	return sent, received
}

// ifindex is the kernel's index for an interface, or empty when it cannot say.
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

// nameForIndex is the interface that now has this index. found is false only
// when every interface was read and none has it; anything less is an error.
func nameForIndex(index string) (name string, found bool, err error) {
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
