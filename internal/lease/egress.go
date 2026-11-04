package lease

import (
	"fmt"
	"net/netip"
)

const (
	// MaxEgressNamedAddrs is how many addresses one report names. It is the number
	// of destinations a report keeps by name, so every destination it totals can
	// have its names.
	MaxEgressNamedAddrs = MaxJobDestinations
	// MaxEgressNamesPerAddr is how many names one address carries in a report.
	MaxEgressNamesPerAddr = 4
	// MaxEgressRefusedNames and MaxEgressDenied bound what a report lists of what
	// was refused. Past them the refusals are still in its counts.
	MaxEgressRefusedNames = 64
	MaxEgressDenied       = 64
	// maxEgressName is a name's length in its textual form, the bound DNS sets.
	maxEgressName = 253
)

// JobEgress is what the host saw of a job's network, from its guest's resolver and
// the firewall on its interface (ADR-023). It is reported once, as the job's compute
// goes, beside and apart from what the host measured it do, because a tier's list
// is enforced whether or not the node samples anything.
//
// THE GUEST CHOSE EVERY NAME HERE, either by asking for it or by answering it from
// a nameserver it controls, so Validate holds them to bytes a terminal prints as
// themselves, and nothing is decided from them.
type JobEgress struct {
	// Listed says the job's tier had an allowlist; false is an open tier, whose
	// names were recorded and whose packets were not judged.
	Listed bool `json:"listed"`
	// Queries counts what the guest asked its resolver, RefusedQueries those a list
	// refused without forwarding, and RefusedNames the ones kept, most asked first.
	Queries        int64           `json:"queries"`
	RefusedQueries int64           `json:"refused_queries"`
	RefusedNames   []EgressRefusal `json:"refused_names,omitempty"`
	// Names is what each address answered for.
	Names []EgressNames `json:"names,omitempty"`
	// DeniedPackets counts every IPv4 packet refused, OutsideResolverPackets the
	// part of them that was DNS to another resolver, IPv6Packets every IPv6 packet
	// refused, and Denied the destinations kept, most packets first.
	DeniedPackets          int64          `json:"denied_packets"`
	OutsideResolverPackets int64          `json:"outside_resolver_packets"`
	IPv6Packets            int64          `json:"ipv6_packets"`
	Denied                 []EgressDenial `json:"denied,omitempty"`
	// Incomplete says these are a lower bound: the resolver did not watch the guest
	// from its start, more was asked than is kept, or a count could not be read.
	Incomplete bool `json:"incomplete"`
}

// EgressRefusal is a name a list refused a guest, and how many times.
type EgressRefusal struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// EgressNames is what one address answered for.
type EgressNames struct {
	Addr  string   `json:"addr"`
	Names []string `json:"names"`
}

// EgressDenial is a destination the host refused a guest, and how many packets.
type EgressDenial struct {
	Addr     string `json:"addr"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Packets  int64  `json:"packets"`
}

// Validate refuses what the ledger could not keep faithfully, and what a reader's
// terminal should not be handed.
func (e JobEgress) Validate() error {
	if e.Queries < 0 || e.RefusedQueries < 0 || e.DeniedPackets < 0 || e.OutsideResolverPackets < 0 ||
		e.IPv6Packets < 0 {
		return fmt.Errorf("alloc: an egress report has a negative count (queries %d, refused %d, denied %d, "+
			"outside resolver %d, IPv6 %d)", e.Queries, e.RefusedQueries, e.DeniedPackets, e.OutsideResolverPackets,
			e.IPv6Packets)
	}

	if e.RefusedQueries > e.Queries {
		return fmt.Errorf("alloc: an egress report has %d refused of %d queries", e.RefusedQueries, e.Queries)
	}

	if e.OutsideResolverPackets > e.DeniedPackets {
		return fmt.Errorf("alloc: an egress report has %d packets to an outside resolver of %d denied",
			e.OutsideResolverPackets, e.DeniedPackets)
	}

	// AN OPEN GUEST'S PACKETS ARE NOT JUDGED, so a refusal under one says the
	// report is not what it claims.
	if !e.Listed && (e.RefusedQueries != 0 || len(e.RefusedNames) != 0 || e.DeniedPackets != 0 ||
		e.IPv6Packets != 0 || len(e.Denied) != 0) {
		return fmt.Errorf("alloc: an egress report refuses something for a job with no list")
	}

	if err := e.validateLists(); err != nil {
		return err
	}

	return e.validateNames()
}

func (e JobEgress) validateLists() error {
	if len(e.RefusedNames) > MaxEgressRefusedNames {
		return fmt.Errorf("alloc: an egress report names %d refused names, and at most %d are kept",
			len(e.RefusedNames), MaxEgressRefusedNames)
	}

	for _, r := range e.RefusedNames {
		if !validEgressName(r.Name) {
			return fmt.Errorf("alloc: an egress report refuses a name that is not one")
		}

		if r.Count <= 0 {
			return fmt.Errorf("alloc: an egress report refuses %s %d times", r.Name, r.Count)
		}
	}

	if len(e.Denied) > MaxEgressDenied {
		return fmt.Errorf("alloc: an egress report names %d denied destinations, and at most %d are kept",
			len(e.Denied), MaxEgressDenied)
	}

	for _, d := range e.Denied {
		if !canonicalAddr(d.Addr) {
			return fmt.Errorf("alloc: denied destination %q is not an address in its canonical form", d.Addr)
		}

		if d.Protocol != "tcp" && d.Protocol != "udp" {
			return fmt.Errorf("alloc: denied destination %s has protocol %q", d.Addr, d.Protocol)
		}

		if d.Port < 0 || d.Port > 65535 {
			return fmt.Errorf("alloc: denied destination %s has port %d", d.Addr, d.Port)
		}

		if d.Packets < 0 {
			return fmt.Errorf("alloc: denied destination %s has %d packets", d.Addr, d.Packets)
		}
	}

	return nil
}

func (e JobEgress) validateNames() error {
	if len(e.Names) > MaxEgressNamedAddrs {
		return fmt.Errorf("alloc: an egress report names %d addresses, and at most %d are kept", len(e.Names),
			MaxEgressNamedAddrs)
	}

	seen := make(map[string]bool, len(e.Names))

	for _, n := range e.Names {
		if !canonicalAddr(n.Addr) {
			return fmt.Errorf("alloc: named address %q is not an address in its canonical form", n.Addr)
		}

		if seen[n.Addr] {
			return fmt.Errorf("alloc: address %s is named twice", n.Addr)
		}

		seen[n.Addr] = true

		if len(n.Names) == 0 || len(n.Names) > MaxEgressNamesPerAddr {
			return fmt.Errorf("alloc: address %s carries %d names, and 1 to %d are kept", n.Addr, len(n.Names),
				MaxEgressNamesPerAddr)
		}

		for _, name := range n.Names {
			if !validEgressName(name) {
				return fmt.Errorf("alloc: address %s carries a name that is not one", n.Addr)
			}
		}
	}

	return nil
}

func canonicalAddr(s string) bool {
	addr, err := netip.ParseAddr(s)

	return err == nil && addr.Zone() == "" && addr.String() == s
}

// validEgressName reports whether a name is one the node's resolver keeps: lower
// case letters, digits, a hyphen, an underscore and dots.
func validEgressName(name string) bool {
	if name == "" || len(name) > maxEgressName {
		return false
	}

	for i := range len(name) {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}

	return true
}
