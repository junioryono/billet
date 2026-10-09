package flows

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNoAddress says the lease file holds no address for the hardware address
// asked about: the guest has not asked for one yet, or never will. It is an
// answer, distinct from a file that could not be read.
var ErrNoAddress = errors.New("flows: no DHCP lease for that hardware address")

// AddressFor reads a dnsmasq lease file and returns the address leased to mac.
//
// Each line is "<expiry> <mac> <address> <hostname> <client id>". A guest's MAC
// is billet's own choice, made per job, so the address dnsmasq leased to it is
// the guest's for as long as the job runs. A line that does not parse is an
// error rather than a line to skip: a file this reader does not understand
// cannot be told apart from one that holds no lease.
func AddressFor(data []byte, mac net.HardwareAddr) (netip.Addr, error) {
	addr, _, err := leaseFor(data, mac)

	return addr, err
}

// leaseFor is AddressFor, also returning when the lease expires. A lease with
// no expiry (dnsmasq writes 0 for an infinite one) has no grant time to judge,
// and is an error.
func leaseFor(data []byte, mac net.HardwareAddr) (netip.Addr, time.Time, error) {
	var (
		found   netip.Addr
		expires time.Time
	)

	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 {
			return netip.Addr{}, time.Time{}, fmt.Errorf("flows: lease file line %d has %d fields, want at least 3", i+1, len(fields))
		}

		hw, err := net.ParseMAC(fields[1])
		if err != nil {
			// dnsmasq writes a client without a hardware address as "*", and a
			// non-Ethernet one with a type prefix; neither is a guest of ours.
			if fields[1] == "*" || strings.Contains(fields[1], "-") {
				continue
			}

			return netip.Addr{}, time.Time{}, fmt.Errorf("flows: lease file line %d: %w", i+1, err)
		}

		if !strings.EqualFold(hw.String(), mac.String()) {
			continue
		}

		addr, err := netip.ParseAddr(fields[2])
		if err != nil {
			return netip.Addr{}, time.Time{}, fmt.Errorf("flows: lease file line %d: %w", i+1, err)
		}

		secs, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || secs <= 0 {
			return netip.Addr{}, time.Time{}, fmt.Errorf("flows: lease file line %d: expiry %q is not a time "+
				"a lease's grant can be read from", i+1, fields[0])
		}

		// TWO ADDRESSES FOR ONE MAC IS NOT AN ANSWER. dnsmasq keeps one lease
		// per client, so this is a file being rewritten or one billet does not
		// understand, and either address could be the wrong one.
		if found.IsValid() && found != addr.Unmap() {
			return netip.Addr{}, time.Time{}, fmt.Errorf("flows: lease file holds %s and %s for %s", found, addr, mac)
		}

		found, expires = addr.Unmap(), time.Unix(secs, 0)
	}

	if !found.IsValid() {
		return netip.Addr{}, time.Time{}, ErrNoAddress
	}

	return found, expires, nil
}

// Switches are the kernel settings flows need: nf_conntrack_acct, without
// which every flow reads zero bytes, and nf_conntrack_timestamp, without which
// no flow can be told apart from the previous holder of its address.
var Switches = []string{"nf_conntrack_acct", "nf_conntrack_timestamp"}

// Ready reports whether the host counts bytes on tracked flows and stamps
// their start, reading the Switches under root ("/" on a host). It answers yes
// (nil), no (an error naming the switch that is off) or could-not-tell (an
// error saying the setting could not be read), and only yes lets flows be
// measured.
func Ready(root string) error {
	for _, name := range Switches {
		raw, err := os.ReadFile(filepath.Join(root, "proc", "sys", "net", "netfilter", name))
		if err != nil {
			return fmt.Errorf("flows: could not tell whether %s is on: %w", name, err)
		}

		switch v := strings.TrimSpace(string(raw)); v {
		case "1":
		case "0":
			return fmt.Errorf("flows: net.netfilter.%s is 0, and per-destination bytes need it set to 1", name)
		default:
			return fmt.Errorf("flows: could not tell whether %s is on: it reads %q", name, v)
		}
	}

	return nil
}
