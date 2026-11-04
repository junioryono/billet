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
	"strconv"
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

	cmd := exec.CommandContext(ctx, nftBinary(), args...) //nolint:gosec // G204: nft, with arguments this package composes
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

// validMark says a mark is one a guest could hold. Zero matters most: it is what every
// packet and connection nobody marked carries, so rules or a deletion naming it reach all
// of them.
func validMark(mark uint32) bool {
	return mark&^uint32(maxSlot) == markBase && mark&maxSlot != 0
}

// absent reports whether an nft listing failed because the object is not there,
// the one failure that is an answer: nft words it so on every exit it has.
func absent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such file or directory")
}

// baseScript makes the three tables what they should be, atomically, whatever they hold:
// each base chain holds only the rules here, so flushing it and adding them back in one
// transaction leaves no moment without them, and running it again changes nothing.
//
// A BRIDGE PORT HAS NO BUSINESS AT THE IP LAYER. A frame to a link-local MAC is handed up
// from the port without passing the bridge's prerouting hook, so no guest's chain sees it,
// and it arrives as a packet on the port itself, which no rule of the role's guard names.
// Whether that reaches a socket bound on the gateway was left to the host's rp_filter, and
// for IPv6 to nothing at all. Every frame of a guest's that is meant for the host arrives
// on the bridge's device instead, so anything arriving on the port is refused.
func baseScript() string {
	return `add table bridge billet_egress
add map bridge billet_egress guests { type ifname : verdict; }
add chain bridge billet_egress prerouting { type filter hook prerouting priority -200; policy accept; }
flush chain bridge billet_egress prerouting
add rule bridge billet_egress prerouting iifname vmap @guests
add map bridge billet_egress ports { type ifname : verdict; }
add chain bridge billet_egress forward { type filter hook forward priority 0; policy accept; }
flush chain bridge billet_egress forward
add rule bridge billet_egress forward iifname vmap @ports
add rule bridge billet_egress forward oifname vmap @ports
add map bridge billet_egress replies { type mark : verdict; }
add chain bridge billet_egress output { type filter hook output priority 0; policy accept; }
flush chain bridge billet_egress output
add rule bridge billet_egress output ct mark vmap @replies
add table inet billet_egress_routed
add map inet billet_egress_routed guests { type ifname : verdict; }
add chain inet billet_egress_routed prerouting { type filter hook prerouting priority -200; policy accept; }
flush chain inet billet_egress_routed prerouting
add rule inet billet_egress_routed prerouting iifname vmap @guests
add map inet billet_egress_routed replies { type mark : verdict; }
add chain inet billet_egress_routed output { type filter hook output priority 0; policy accept; }
flush chain inet billet_egress_routed output
add rule inet billet_egress_routed output ct mark vmap @replies
add set inet billet_egress_routed bridge_ports { type ifname; }
add chain inet billet_egress_routed port_l3 { type filter hook prerouting priority raw; policy accept; }
flush chain inet billet_egress_routed port_l3
add rule inet billet_egress_routed port_l3 iifname @bridge_ports drop
add table ip billet_egress_nat
add map ip billet_egress_nat jobs { type mark : verdict; }
add chain ip billet_egress_nat prerouting { type nat hook prerouting priority dstnat - 1; policy accept; }
flush chain ip billet_egress_nat prerouting
add rule ip billet_egress_nat prerouting meta mark vmap @jobs
add map ip billet_egress_nat owners { type mark : verdict; }
add chain ip billet_egress_nat own { type filter hook prerouting priority dstnat + 10; policy accept; }
flush chain ip billet_egress_nat own
add rule ip billet_egress_nat own meta mark vmap @owners
add rule ip billet_egress_nat own ct mark vmap @owners
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

// attachScript installs a guest's rules, in one transaction across the tables: the
// guest's chain, its sets and its redirect exist together or not at all. RUN AGAIN IT
// REPAIRS THEM: each chain is flushed as it is made, and a set keeps what is in it.
//
// A LISTED GUEST, FRAME BY FRAME, in this order and no other:
//
//   - ARP, which reaches no further than the bridge.
//   - IPv6 is refused and counted: the bridges carry none, and a guest has no
//     path for it, so this says so rather than leaving it unwatched.
//   - DHCP's broadcast, by its destination: udp port 67 alone would be a way to
//     send a datagram anywhere.
//   - A source address outside the guest's subnet is refused: the resolver answers
//     whoever a query says it is from, so this is what stops a query being a way to
//     have the host send its bytes anywhere.
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
//     fails in milliseconds instead of timing out: TCP's own reset for TCP.
//
// THE LAST RULE IS A DROP, because a reject is defined for what it can answer: what it
// does with a frame of another ethertype is not this package's to leave to chance, and
// falling off this chain is the base chain's accept.
//
// A LISTED GUEST'S PORT IS CUT OFF FROM ITS BRIDGE'S OTHER PORTS. The rules above read
// IP addresses, and a bridge delivers by MAC: a frame to a peer's MAC holding an allowed
// public address would pass them and reach the peer, which could relay it under a
// broader policy. No peer's frame reaches it either. What the host itself sends is not
// held to this: a guest that claims a peer's MAC or address on the bridge can be sent
// what the host meant for that peer (ADR-023).
//
// AN OPEN GUEST'S CHAIN ONLY MARKS DNS, and falls through to the base chain's
// accept.
//
// EVERY GUEST'S DNS CONNECTION NAMES ITS OWNER. The redirect is chosen when a connection
// is created and kept by the connection tracker, which knows nothing of interfaces: a
// guest forging another's source address and port joins that connection, and is sent to
// that guest's resolver under that guest's policy, or to dnsmasq itself if that guest
// has no redirect. So the redirect stamps its own mark on the connection, and a packet
// is dropped unless the mark its interface gave it and its connection's are one.
//
// AND A REPLY GOES OUT THE OWNER'S INTERFACE ONLY. A guest forging a neighbour's source
// address opens a connection of its own whose answers would reach that neighbour with
// the query's bytes in them, which no rule above sees: they leave from the host.
func attachScript(b binding) string {
	var s strings.Builder

	table := b.kind.table()
	line := func(format string, args ...any) { fmt.Fprintf(&s, format+"\n", args...) }
	rule := func(chain, format string, args ...any) {
		line("add rule %s %s_%s %s", table, chain, b.key, fmt.Sprintf(format, args...))
	}
	chainIn := func(in, chain string) {
		line("add chain %s %s_%s", in, chain, b.key)
		line("flush chain %s %s_%s", in, chain, b.key)
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

		chainIn(table, "deny")
		rule("deny", "ip protocol { tcp, udp } update @denied_%s { ip daddr . ip protocol . th dport }", b.key)
		rule("deny", "counter name denied_c_%s", b.key)
		rule("deny", "ip protocol tcp reject with tcp reset")
		rule("deny", "reject")
		rule("deny", "drop")

		chainIn(table, "job")

		if b.kind == BridgePort {
			rule("job", "ether type arp accept")
			rule("job", "ether type ip6 counter name ipv6_c_%s drop", b.key)
			rule("job", "ip daddr 255.255.255.255 udp dport 67 accept")
		} else {
			rule("job", "meta nfproto ipv6 counter name ipv6_c_%s drop", b.key)
		}

		rule("job", "ip saddr != %s goto deny_%s", b.gateway.Masked(), b.key)
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

		if b.kind == BridgePort {
			// A BROADCAST IS A GUEST'S ARP AND DHCP, flooded to every port as a matter
			// of course: only what was aimed at a peer is a refusal worth counting.
			chainIn(table, "fwd")
			rule("fwd", "meta pkttype { broadcast, multicast } drop")
			rule("fwd", "counter name denied_c_%s drop", b.key)
			line(`add element %s ports { "%s" : jump fwd_%s }`, table, b.iface, b.key)
		}
	} else {
		chainIn(table, "job")
		rule("job", "%s", markDNS)
	}

	line(`add element %s guests { "%s" : jump job_%s }`, table, b.iface, b.key)

	if b.kind == BridgePort {
		line(`add element %s bridge_ports { "%s" }`, Routed.table(), b.iface)
	}

	chainIn(natTable, "dns")
	s.WriteString(redirectRules(b.key, b.mark, b.port))
	line("add element %s jobs { %#x : jump dns_%s }", natTable, b.mark, b.key)

	chainIn(natTable, "own")
	line("add rule %s own_%s meta mark != %#x drop", natTable, b.key, b.mark)
	line("add rule %s own_%s ct mark != %#x drop", natTable, b.key, b.mark)
	line("add element %s owners { %#x : jump own_%s }", natTable, b.mark, b.key)

	chainIn(table, "reply")
	rule("reply", `oifname != "%s" drop`, b.iface)
	line("add element %s replies { %#x : jump reply_%s }", table, b.mark, b.key)

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

// redirectRules send a guest's marked DNS to its socket, naming it the connection's owner.
func redirectRules(key string, mark uint32, port uint16) string {
	return fmt.Sprintf("add rule %[1]s dns_%[2]s udp dport 53 ct mark set %#[3]x redirect to :%[4]d\n"+
		"add rule %[1]s dns_%[2]s tcp dport 53 ct mark set %#[3]x redirect to :%[4]d\n", natTable, key, mark, port)
}

// quarantineScript sends a guest's DNS nowhere, for a guest no resolver here will serve:
// its redirect names a port this process does not hold, which the kernel can give the next
// guest's resolver, whose policy would then answer it. THE OWNER'S CHAIN TOO: a redirect
// is read only when a connection is made, so one the guest holds would go on past it, and
// that chain sees every packet. Attaching it again puts both back.
func quarantineScript(key string, mark uint32) string {
	var s strings.Builder

	for _, c := range []struct{ prefix, element string }{{"dns", "jobs"}, {"own", "owners"}} {
		fmt.Fprintf(&s, "add chain %s %s_%s\n", natTable, c.prefix, key)
		fmt.Fprintf(&s, "flush chain %s %s_%s\n", natTable, c.prefix, key)
		fmt.Fprintf(&s, "add rule %s %s_%s drop\n", natTable, c.prefix, key)
		fmt.Fprintf(&s, "add element %s %s { %#x : jump %s_%s }\n", natTable, c.element, mark, c.prefix, key)
	}

	return s.String()
}

// closedScript gives a guest nothing but ARP, for one whose rules have gone and whose policy
// nothing can read: it cannot be given its list back, and must not run open.
func closedScript(key, iface string, kind Kind) string {
	var s strings.Builder

	table := kind.table()

	fmt.Fprintf(&s, "add chain %s job_%s\n", table, key)
	fmt.Fprintf(&s, "flush chain %s job_%s\n", table, key)

	if kind == BridgePort {
		fmt.Fprintf(&s, "add rule %s job_%s ether type arp accept\n", table, key)
	}

	fmt.Fprintf(&s, "add rule %s job_%s drop\n", table, key)
	fmt.Fprintf(&s, "add element %s guests { \"%s\" : jump job_%s }\n", table, iface, key)

	return s.String()
}

// detachScript removes everything attachScript made for one guest.
func detachScript(key, iface string, kind Kind, mark uint32, listed bool) string {
	var s strings.Builder

	table := kind.table()

	fmt.Fprintf(&s, "delete element %s guests { \"%s\" }\n", table, iface)
	fmt.Fprintf(&s, "delete chain %s job_%s\n", table, key)
	fmt.Fprintf(&s, "delete element %s replies { %#x }\n", table, mark)
	fmt.Fprintf(&s, "delete chain %s reply_%s\n", table, key)

	if kind == BridgePort {
		fmt.Fprintf(&s, "delete element %s bridge_ports { \"%s\" }\n", Routed.table(), iface)
	}

	if listed {
		if kind == BridgePort {
			fmt.Fprintf(&s, "delete element %s ports { \"%s\" }\n", table, iface)
			fmt.Fprintf(&s, "delete chain %s fwd_%s\n", table, key)
		}

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
	fmt.Fprintf(&s, "delete element %s owners { %#x }\n", natTable, mark)
	fmt.Fprintf(&s, "delete chain %s own_%s\n", natTable, key)

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

// ruleTable is what one table holds, as far as a guest's enforcement rests on it.
type ruleTable struct {
	// rules counts the rules in each chain.
	rules map[string]int
	// maps is each verdict map, element by the raw JSON of its key to the chain it jumps to.
	maps map[string]map[string]string
	// sets is each set's elements, by their raw JSON.
	sets map[string][]string
}

// parseRuleTable reads a listing of one table. A table that is not there holds nothing.
func parseRuleTable(out []byte) (ruleTable, error) {
	t := ruleTable{rules: map[string]int{}, maps: map[string]map[string]string{}, sets: map[string][]string{}}

	var doc nftDocument
	if err := json.Unmarshal(out, &doc); err != nil {
		return t, fmt.Errorf("egress: read nft's listing: %w", err)
	}

	for _, entry := range doc.Nftables {
		if raw, ok := entry["rule"]; ok {
			var r struct {
				Chain string `json:"chain"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				return t, fmt.Errorf("egress: a rule in nft's listing: %w", err)
			}

			t.rules[r.Chain]++
		}

		if raw, ok := entry["map"]; ok {
			var named struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &named); err != nil {
				return t, fmt.Errorf("egress: a map in nft's listing: %w", err)
			}

			elements, err := parseVerdictMap([]byte(`{"nftables":[{"map":` + string(raw) + `}]}`))
			if err != nil {
				return t, err
			}

			t.maps[named.Name] = map[string]string{}
			for _, e := range elements {
				t.maps[named.Name][string(e.key)] = e.target
			}
		}

		if raw, ok := entry["set"]; ok {
			var set struct {
				Name string            `json:"name"`
				Elem []json.RawMessage `json:"elem"`
			}
			if err := json.Unmarshal(raw, &set); err != nil {
				return t, fmt.Errorf("egress: a set in nft's listing: %w", err)
			}

			for _, e := range set.Elem {
				t.sets[set.Name] = append(t.sets[set.Name], string(e))
			}
		}
	}

	return t, nil
}

// countRules is how many rules a script puts in each chain, by "family table chain".
func countRules(script string) map[string]int {
	out := map[string]int{}

	for line := range strings.SplitSeq(script, "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[0] == "add" && fields[1] == "rule" {
			out[strings.Join(fields[2:5], " ")]++
		}
	}

	return out
}

// heldRules is what the three tables hold.
type heldRules struct {
	bridge, routed, nat ruleTable
}

func (h heldRules) of(table string) ruleTable {
	switch table {
	case BridgePort.table():
		return h.bridge
	case Routed.table():
		return h.routed
	}

	return h.nat
}

// enforces answers whether these tables hold what a listed guest's rules need, from the
// base chains' dispatch to its last rule. EVERY CHAIN HOLDS AS MANY RULES AS ITS RENDERING
// WOULD PUT THERE: a chain that exists and nothing jumps to, that a flush emptied, or that
// has lost the one rule that refuses, refuses nothing. It does not read the rules' text, so
// a rule rewritten in place by hand reads as whole.
func (h heldRules) enforces(b binding) bool {
	for chain, want := range countRules(baseScript() + attachScript(b)) {
		fields := strings.Fields(chain)
		if h.of(fields[0] + " " + fields[1]).rules[fields[2]] != want {
			return false
		}
	}

	guests, nat := h.of(b.kind.table()), h.nat
	name, number := fmt.Sprintf("%q", b.iface), strconv.FormatUint(uint64(b.mark), 10)

	own := guests.maps["guests"][name] == "job_"+b.key && guests.maps["replies"][number] == "reply_"+b.key &&
		nat.maps["jobs"][number] == "dns_"+b.key && nat.maps["owners"][number] == "own_"+b.key

	if b.kind != BridgePort {
		return own
	}

	return own && guests.maps["ports"][name] == "fwd_"+b.key && slices.Contains(h.routed.sets["bridge_ports"], name)
}

// dispatches answers whether a guest's chain is reached and holds anything.
func (h heldRules) dispatches(key, iface string, kind Kind) bool {
	guests := h.of(kind.table())

	return guests.rules["prerouting"] > 0 && guests.maps["guests"][fmt.Sprintf("%q", iface)] == "job_"+key &&
		guests.rules["job_"+key] > 0
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
