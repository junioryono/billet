package egress

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// bridgeTable holds the chains of guests on a bridge port, routedTable those
	// of guests on a routed interface, and natTable the redirect of both. All
	// three are billet's own, so the host role's reload of its guard, which
	// deletes and recreates its tables, never touches them.
	bridgeTable = "bridge billet_egress"
	routedTable = "inet billet_egress_routed"
	natTable    = "ip billet_egress_nat"

	// GuardRuleComment names the rule the host role's guard carries for redirected
	// DNS. A filter hook's drop in one table cannot be overruled from another, and
	// the guard drops every port on the host it does not list, so a redirect only
	// reaches its socket through this rule. Readiness finds it by this comment.
	GuardRuleComment = "billet egress resolver"

	// markBase and the guest's slot make the mark on its DNS packets, which is
	// how the bridge family's chain, which can only see the interface, tells the
	// ip family's nat, which can only redirect, whose query this is. Measured
	// reaching it on kernel 7.0 and in a Firecracker guest on 2026-11-04.
	markBase = uint32(0xb1e70000)
	maxSlot  = 0xffff

	// maxAllowed is the set of addresses a listed guest's names answered. A job
	// that fills it has its next new address's query fail.
	maxAllowed = 4096

	nftLimit = 5 * time.Second
)

// Kind is how a guest's interface reaches the host.
type Kind string

const (
	// BridgePort is an interface enslaved to a bridge, a Firecracker tap: its
	// frames are seen by the bridge family, and only there does the input
	// interface name the port rather than the bridge.
	BridgePort Kind = "bridge-port"
	// Routed is an interface the host routes through, a workspace's veth.
	Routed Kind = "routed"
)

func (k Kind) table() string {
	if k == Routed {
		return routedTable
	}

	return bridgeTable
}

// keyRe and ifaceRe keep what is rendered into a script to what is safe there:
// the key is an instance's name and the interface a device billet allocated, but
// the rule is the shape, never that the caller is careful.
var (
	keyRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
	ifaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)
)

// runFunc runs nft with script on its standard input, answering its standard
// output. The error carries what it wrote to standard error.
type runFunc func(ctx context.Context, script string, args ...string) ([]byte, error)

// runNft is the real thing.
func runNft(ctx context.Context, script string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nftLimit)
	defer cancel()

	cmd := exec.CommandContext(ctx, nftBinary(), args...)
	cmd.Stdin = strings.NewReader(script)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, bounded(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// nftBinary is nft on PATH, and where the host role's unit runs it from on a
// host whose service PATH leaves it out.
func nftBinary() string {
	if path, err := exec.LookPath("nft"); err == nil {
		return path
	}

	return "/usr/sbin/nft"
}

func bounded(s string) string {
	const limit = 512
	if len(s) > limit {
		return s[:limit] + "..."
	}

	return s
}

// absent reports whether an nft listing failed because the object is not there,
// the one failure that is an answer: nft words it so on every exit it has.
func absent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such file or directory")
}

// baseScript creates the three tables, atomically, so a host holds all or none.
func baseScript() string {
	return `add table bridge billet_egress
add map bridge billet_egress guests { type ifname : verdict; }
add chain bridge billet_egress prerouting { type filter hook prerouting priority -200; policy accept; }
add rule bridge billet_egress prerouting iifname vmap @guests
add table inet billet_egress_routed
add map inet billet_egress_routed guests { type ifname : verdict; }
add chain inet billet_egress_routed prerouting { type filter hook prerouting priority -200; policy accept; }
add rule inet billet_egress_routed prerouting iifname vmap @guests
add table ip billet_egress_nat
add map ip billet_egress_nat jobs { type mark : verdict; }
add chain ip billet_egress_nat prerouting { type nat hook prerouting priority dstnat - 1; policy accept; }
add rule ip billet_egress_nat prerouting meta mark vmap @jobs
`
}

// binding is everything a guest's rules are made from.
type binding struct {
	key      string
	iface    string
	kind     Kind
	gateway  netip.Prefix
	mark     uint32
	port     uint16
	listed   bool
	prefixes []netip.Prefix
	// local is every address this host answers on.
	local []netip.Addr
}

// attachScript installs a guest's rules, in one transaction across the tables:
// the guest's chain, its sets and its redirect exist together or not at all.
//
// A LISTED GUEST, FRAME BY FRAME, in this order and no other:
//
//   - ARP, which reaches no further than the bridge.
//   - IPv6 is refused and counted: the bridges carry none, and a guest has no
//     path for it, so this says so rather than leaving it unwatched.
//   - DHCP's broadcast, by its destination: udp port 67 alone would be a way to
//     send a datagram anywhere.
//   - DNS to the gateway is marked for its redirect, and is the only DNS it gets.
//   - Anything else to the gateway is the host's, and the role's guard judges it
//     by port as it does every guest's.
//   - DNS to anywhere else is an outside resolver, and refused whatever the list
//     says of its address: a prefix must not be a way around the resolver, and
//     neither may another bridge's dnsmasq, which the guard would admit.
//   - The host's other addresses are its too, and the guard judges those as well:
//     the node's guest cache listens on one address, which a guest on the other
//     bridge reaches across the host.
//   - The guest's own subnet is another guest, reached without passing the guard,
//     so no prefix on a list can open it.
//   - What the guest's names answered and the list's prefixes. Everything else is
//     recorded, counted and refused at once rather than dropped, so a connect
//     fails in milliseconds instead of timing out: TCP's reset for TCP, since the
//     ICMP form took a retransmit (measured 1 s against 3 ms on 2026-11-04).
//
// AN OPEN GUEST'S CHAIN ONLY MARKS DNS, and falls through to the base chain's
// accept.
func attachScript(b binding) string {
	var s strings.Builder

	table := b.kind.table()
	line := func(format string, args ...any) { fmt.Fprintf(&s, format+"\n", args...) }
	rule := func(chain, format string, args ...any) {
		line("add rule %s %s_%s %s", table, chain, b.key, fmt.Sprintf(format, args...))
	}

	gateway := b.gateway.Addr()
	markDNS := fmt.Sprintf("ip daddr %s meta l4proto { tcp, udp } th dport 53 meta mark set %#x", gateway, b.mark)

	if b.listed {
		line("add set %s allow_%s { type ipv4_addr; size %d; }", table, b.key, maxAllowed)
		line("add set %s cidr_%s { type ipv4_addr; flags interval; auto-merge; }", table, b.key)

		if len(b.prefixes) > 0 {
			elements := make([]string, 0, len(b.prefixes))
			for _, p := range b.prefixes {
				elements = append(elements, p.String())
			}

			line("add element %s cidr_%s { %s }", table, b.key, strings.Join(elements, ", "))
		}

		line("add set %s denied_%s { type ipv4_addr . inet_proto . inet_service; size %d; flags dynamic; counter; }",
			table, b.key, MaxDenied)
		line("add counter %s denied_c_%s", table, b.key)
		line("add counter %s outside_c_%s", table, b.key)
		line("add counter %s ipv6_c_%s", table, b.key)

		line("add chain %s deny_%s", table, b.key)
		rule("deny", "ip protocol { tcp, udp } update @denied_%s { ip daddr . ip protocol . th dport }", b.key)
		rule("deny", "counter name denied_c_%s", b.key)
		rule("deny", "ip protocol tcp reject with tcp reset")
		rule("deny", "reject")

		line("add chain %s job_%s", table, b.key)

		if b.kind == BridgePort {
			rule("job", "ether type arp accept")
			rule("job", "ether type ip6 counter name ipv6_c_%s drop", b.key)
			rule("job", "ip daddr 255.255.255.255 udp dport 67 accept")
		} else {
			rule("job", "meta nfproto ipv6 counter name ipv6_c_%s drop", b.key)
		}

		rule("job", "%s accept", markDNS)
		rule("job", "ip daddr %s accept", gateway)
		rule("job", "meta l4proto { tcp, udp } th dport 53 counter name outside_c_%s goto deny_%s", b.key, b.key)

		if others := otherAddresses(b); len(others) > 0 {
			rule("job", "ip daddr { %s } accept", strings.Join(others, ", "))
		}

		rule("job", "ip daddr %s goto deny_%s", b.gateway.Masked(), b.key)
		rule("job", "ip daddr @allow_%s accept", b.key)
		rule("job", "ip daddr @cidr_%s accept", b.key)
		rule("job", "goto deny_%s", b.key)
	} else {
		line("add chain %s job_%s", table, b.key)
		rule("job", "%s", markDNS)
	}

	line(`add element %s guests { "%s" : jump job_%s }`, table, b.iface, b.key)

	line("add chain %s dns_%s", natTable, b.key)
	line("add rule %s dns_%s udp dport 53 redirect to :%d", natTable, b.key, b.port)
	line("add rule %s dns_%s tcp dport 53 redirect to :%d", natTable, b.key, b.port)
	line("add element %s jobs { %#x : jump dns_%s }", natTable, b.mark, b.key)

	return s.String()
}

// otherAddresses is the host's addresses but the guest's gateway, in rule spelling.
func otherAddresses(b binding) []string {
	out := make([]string, 0, len(b.local))

	for _, addr := range b.local {
		if addr != b.gateway.Addr() {
			out = append(out, addr.String())
		}
	}

	return out
}

// retargetScript points a guest's redirect at a new socket, for a process that
// adopted it. The chain and its sets are kept, so what its names admitted stays
// admitted across the change of process.
func retargetScript(key string, port uint16) string {
	return fmt.Sprintf("flush chain %[1]s dns_%[2]s\nadd rule %[1]s dns_%[2]s udp dport 53 redirect to :%[3]d\n"+
		"add rule %[1]s dns_%[2]s tcp dport 53 redirect to :%[3]d\n", natTable, key, port)
}

// detachScript removes everything attachScript made for one guest.
func detachScript(key, iface string, kind Kind, mark uint32, listed bool) string {
	var s strings.Builder

	table := kind.table()

	fmt.Fprintf(&s, "delete element %s guests { \"%s\" }\n", table, iface)
	fmt.Fprintf(&s, "delete chain %s job_%s\n", table, key)

	if listed {
		fmt.Fprintf(&s, "delete chain %s deny_%s\n", table, key)

		for _, set := range []string{"allow", "cidr", "denied"} {
			fmt.Fprintf(&s, "delete set %s %s_%s\n", table, set, key)
		}

		for _, counter := range []string{"denied_c", "outside_c", "ipv6_c"} {
			fmt.Fprintf(&s, "delete counter %s %s_%s\n", table, counter, key)
		}
	}

	fmt.Fprintf(&s, "delete element %s jobs { %#x }\n", natTable, mark)
	fmt.Fprintf(&s, "delete chain %s dns_%s\n", natTable, key)

	return s.String()
}

func admitScript(key string, kind Kind, addrs []netip.Addr) string {
	elements := make([]string, 0, len(addrs))
	for _, a := range addrs {
		elements = append(elements, a.String())
	}

	return fmt.Sprintf("add element %s allow_%s { %s }\n", kind.table(), key, strings.Join(elements, ", "))
}

// nftDocument is nft's JSON output: a list of single-key objects.
type nftDocument struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

// one returns the only object named kind in a listing.
func one(out []byte, kind string) (json.RawMessage, error) {
	var doc nftDocument
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("egress: read nft's listing: %w", err)
	}

	for _, entry := range doc.Nftables {
		if raw, ok := entry[kind]; ok {
			return raw, nil
		}
	}

	return nil, fmt.Errorf("egress: nft's listing has no %s", kind)
}

func parseCounter(out []byte) (int64, error) {
	raw, err := one(out, "counter")
	if err != nil {
		return 0, err
	}

	var c struct {
		Packets *int64 `json:"packets"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, fmt.Errorf("egress: read nft's counter: %w", err)
	}

	if c.Packets == nil {
		return 0, errors.New("egress: nft's counter has no packet count")
	}

	return *c.Packets, nil
}

// parseDeniedSet reads the kernel's set of refused destinations, most packets
// first. An element this does not read is an error, because a list missing one
// looks complete.
func parseDeniedSet(out []byte) ([]Denied, error) {
	raw, err := one(out, "set")
	if err != nil {
		return nil, err
	}

	var set struct {
		Elem []struct {
			Elem struct {
				Val struct {
					Concat []json.RawMessage `json:"concat"`
				} `json:"val"`
				Counter struct {
					Packets int64 `json:"packets"`
				} `json:"counter"`
			} `json:"elem"`
		} `json:"elem"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("egress: read nft's denied set: %w", err)
	}

	denied := make([]Denied, 0, len(set.Elem))

	for _, e := range set.Elem {
		if len(e.Elem.Val.Concat) != 3 {
			return nil, fmt.Errorf("egress: an element of nft's denied set has %d fields", len(e.Elem.Val.Concat))
		}

		var (
			addr     netip.Addr
			protocol string
			port     uint16
		)

		if err := json.Unmarshal(e.Elem.Val.Concat[0], &addr); err != nil {
			return nil, fmt.Errorf("egress: an address in nft's denied set: %w", err)
		}

		if err := json.Unmarshal(e.Elem.Val.Concat[1], &protocol); err != nil {
			return nil, fmt.Errorf("egress: a protocol in nft's denied set: %w", err)
		}

		if err := json.Unmarshal(e.Elem.Val.Concat[2], &port); err != nil {
			return nil, fmt.Errorf("egress: a port in nft's denied set: %w", err)
		}

		denied = append(denied, Denied{Addr: addr, Protocol: protocol, Port: port, Packets: e.Elem.Counter.Packets})
	}

	slices.SortFunc(denied, func(x, y Denied) int {
		if c := cmp.Compare(y.Packets, x.Packets); c != 0 {
			return c
		}

		if c := x.Addr.Compare(y.Addr); c != 0 {
			return c
		}

		return cmp.Compare(x.Port, y.Port)
	})

	return denied, nil
}

// mapElement is one element of a verdict map: a key and the chain it jumps to.
type mapElement struct {
	key    json.RawMessage
	target string
}

func parseVerdictMap(out []byte) ([]mapElement, error) {
	raw, err := one(out, "map")
	if err != nil {
		return nil, err
	}

	var m struct {
		Elem [][]json.RawMessage `json:"elem"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("egress: read nft's map: %w", err)
	}

	elements := make([]mapElement, 0, len(m.Elem))

	for _, e := range m.Elem {
		if len(e) != 2 {
			return nil, fmt.Errorf("egress: an element of nft's map has %d fields", len(e))
		}

		var verdict struct {
			Jump struct {
				Target string `json:"target"`
			} `json:"jump"`
		}
		if err := json.Unmarshal(e[1], &verdict); err != nil {
			return nil, fmt.Errorf("egress: a verdict in nft's map: %w", err)
		}

		elements = append(elements, mapElement{key: e[0], target: verdict.Jump.Target})
	}

	return elements, nil
}
