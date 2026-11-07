package egress

import (
	"cmp"
	"net/netip"
	"slices"
	"sync"
)

const (
	// MaxNamesPerAddr is how many names one address keeps. A shared CDN address
	// answers for hundreds, and the first few are what a reader needs.
	MaxNamesPerAddr = 4
	// MaxNamedAddrs is how many addresses one job keeps names for.
	MaxNamedAddrs = 1024
	// MaxRefusedNames is how many refused names one job keeps. Past it they are
	// still counted: a job tunnelling through its queries asks for a new name
	// every time, and each is at most 253 bytes it chose.
	MaxRefusedNames = 64
	// MaxDenied is how many denied destinations one job keeps with their counts,
	// and the kernel's set for them holds that many too.
	MaxDenied = 256
)

// Refused is one name a listed guest asked for and was not given.
type Refused struct {
	Name  string
	Count int64
}

// Denied is one destination the host refused a guest, from the kernel's set.
type Denied struct {
	Addr netip.Addr
	// Protocol is "tcp" or "udp".
	Protocol string
	Port     uint16
	Packets  int64
}

// Record is what the host saw of one guest's network. It says what the host saw,
// and Incomplete says when that was not all of it.
type Record struct {
	Listed bool
	// Queries counts what the guest asked its resolver, RefusedQueries the part
	// of them a list refused, and Refused names those it kept, most asked first.
	Queries, RefusedQueries int64
	Refused                 []Refused
	// Names is what each address answered for, at most MaxNamesPerAddr each.
	Names map[netip.Addr][]string
	// DeniedPackets counts every IPv4 packet refused, OutsideResolverPackets the
	// part of them that was DNS to another resolver, and IPv6Packets every IPv6
	// packet refused. Denied names the destinations the kernel kept, most packets
	// first. All are zero for an open guest, whose packets are not judged.
	DeniedPackets, OutsideResolverPackets, IPv6Packets int64
	Denied                                             []Denied
	// Incomplete says these are a lower bound: the resolver did not watch the
	// guest from its start (it was adopted from an earlier process), an answer
	// could not be read, more was asked than is kept, or the counters could not
	// be read as the guest went.
	Incomplete bool
}

// recorder is a Record being written by a guest's resolver.
type recorder struct {
	mu      sync.Mutex
	rec     Record
	refused map[string]int64
}

func newRecorder(listed, adopted bool) *recorder {
	return &recorder{
		rec:     Record{Listed: listed, Names: map[netip.Addr][]string{}, Incomplete: adopted},
		refused: map[string]int64{},
	}
}

func (r *recorder) query() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rec.Queries++
}

// refuse counts a refused query; an empty name is one that could not be read.
func (r *recorder) refuse(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rec.RefusedQueries++

	if name == "" {
		return
	}

	if _, kept := r.refused[name]; kept || len(r.refused) < MaxRefusedNames {
		r.refused[name]++

		return
	}

	r.rec.Incomplete = true
}

func (r *recorder) incomplete() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.rec.Incomplete = true
}

func (r *recorder) answer(a answered) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, addr := range a.addrs {
		names, known := r.rec.Names[addr]
		if !known && len(r.rec.Names) >= MaxNamedAddrs {
			r.rec.Incomplete = true

			continue
		}

		for _, name := range a.names {
			if name == "" || slices.Contains(names, name) {
				continue
			}

			if len(names) >= MaxNamesPerAddr {
				break
			}

			names = append(names, name)
		}

		r.rec.Names[addr] = names
	}
}

// freeze is the Record as it stands, given what the kernel counted.
func (r *recorder) freeze(c counts) Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.rec
	out.Names = make(map[netip.Addr][]string, len(r.rec.Names))

	for addr, names := range r.rec.Names {
		out.Names[addr] = slices.Clone(names)
	}

	out.Refused = make([]Refused, 0, len(r.refused))
	for name, n := range r.refused {
		out.Refused = append(out.Refused, Refused{Name: name, Count: n})
	}

	// THE ORDER IS TOTAL, so one set of counts always reads the same.
	slices.SortFunc(out.Refused, func(x, y Refused) int {
		if c := cmp.Compare(y.Count, x.Count); c != 0 {
			return c
		}

		return cmp.Compare(x.Name, y.Name)
	})

	out.DeniedPackets, out.OutsideResolverPackets = c.denied, c.outside
	out.IPv6Packets, out.Denied = c.ipv6, c.destinations
	out.Incomplete = out.Incomplete || c.unread

	return out
}

// counts is what the kernel counted of a listed guest. unread says part of it
// could not be read: not zero, which is what a reader would otherwise take it for.
type counts struct {
	denied, outside, ipv6 int64
	destinations          []Denied
	unread                bool
}
