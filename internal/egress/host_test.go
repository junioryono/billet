package egress

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	guestKey = "billet-0123456789abcdef"
	guestTap = "bt-7"
)

func aGuest() Attachment {
	return Attachment{Key: guestKey, Interface: guestTap, Network: "billet0", Kind: BridgePort}
}

// fakeNft stands in for the kernel: it answers listings from what a test staged and
// keeps every script in the order it ran.
type fakeNft struct {
	mu      sync.Mutex
	scripts []string
	calls   []string
	guard   string
	guests  string
	// owners and replies answer listings of those maps, with none empty.
	owners, replies string
	jobs            string
	denyHeld        bool
	// tableListing answers a listing of a whole table; with none, it cannot be read.
	tableListing func(call string) string
	// setListing replaces the denied set's listing.
	setListing string
	// forgotten is the marks whose connections were deleted.
	forgotten []uint32
	// counted says the counters still exist, which a delete of them ends.
	gone     bool
	refuse   func(script string, args []string) error
	onScript func(script string)
}

const anEmptyMap = `{"nftables":[{"map":{"elem":[]}}]}`

func newFakeNft() *fakeNft {
	return &fakeNft{guard: `{"nftables":[{"rule":{"comment":"` + GuardRuleComment + `"}}]}`,
		guests: anEmptyMap, jobs: anEmptyMap}
}

func (f *fakeNft) run(_ context.Context, script string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)

	if f.refuse != nil {
		if err := f.refuse(script, args); err != nil {
			return nil, err
		}
	}

	switch {
	case script != "":
		f.scripts = append(f.scripts, call+"\n"+script)

		if strings.Contains(script, "delete counter") {
			f.gone = true
		}

		if f.onScript != nil {
			f.onScript(script)
		}

		return nil, nil
	case strings.Contains(call, "chain inet billet_guard input"):
		return []byte(f.guard), nil
	case strings.Contains(call, "list table"):
		if f.tableListing == nil {
			return nil, errors.New("the fake was not told what the tables hold")
		}

		return []byte(f.tableListing(call)), nil
	case strings.Contains(call, "list map ip billet_egress_nat jobs"):
		return []byte(f.jobs), nil
	case strings.Contains(call, "list map bridge billet_egress guests"):
		return []byte(f.guests), nil
	case strings.Contains(call, "list map ip billet_egress_nat owners") && f.owners != "":
		return []byte(f.owners), nil
	case strings.Contains(call, "list map bridge billet_egress replies") && f.replies != "":
		return []byte(f.replies), nil
	case strings.Contains(call, "list map"):
		return []byte(anEmptyMap), nil
	case strings.Contains(call, "list chain") && strings.Contains(call, "deny_"):
		if !f.denyHeld {
			return nil, errors.New("Error: No such file or directory")
		}

		return nil, nil
	case f.gone && (strings.Contains(call, "list counter") || strings.Contains(call, "list set")):
		return nil, errors.New("Error: No such file or directory")
	case strings.Contains(call, "list counter"):
		return []byte(`{"nftables":[{"counter":{"packets":3,"bytes":180}}]}`), nil
	case strings.Contains(call, "list set") && f.setListing != "":
		return []byte(f.setListing), nil
	case strings.Contains(call, "list set"):
		return []byte(`{"nftables":[{"set":{"elem":[{"elem":{"val":{"concat":["198.51.100.2","tcp",443]},` +
			`"counter":{"packets":3,"bytes":180}}}]}}]}`), nil
	}

	return nil, fmt.Errorf("the fake has no answer to nft %s", call)
}

func (f *fakeNft) allScripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.scripts)
}

// aHost is a Host on f, with its gateway on loopback so its resolver really binds.
func aHost(t *testing.T, f *fakeNft) *Host {
	t.Helper()

	h := NewHost(filepath.Join(t.TempDir(), "egress"), slog.Default(), WithRunner(f.run),
		WithGateway(func(string) (netip.Prefix, error) { return netip.MustParsePrefix("127.0.0.1/8"), nil }),
		WithLocalAddresses(func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.9.0.5")}, nil }),
		WithConnectionForget(func(mark uint32) error {
			f.mu.Lock()
			defer f.mu.Unlock()

			f.forgotten = append(f.forgotten, mark)

			return nil
		}))
	t.Cleanup(h.Close)

	return h
}

func preparedHost(t *testing.T, f *fakeNft) *Host {
	t.Helper()

	h := aHost(t, f)
	h.Prepare(t.Context())

	if err := h.Ready(); err != nil {
		t.Fatalf("not ready: %v", err)
	}

	return h
}

func scriptWith(t *testing.T, f *fakeNft, needle string) string {
	t.Helper()

	for _, s := range f.allScripts() {
		if strings.Contains(s, needle) {
			return s
		}
	}

	t.Fatalf("no script holds %q in %q", needle, f.allScripts())

	return ""
}

func TestAHostIsNotReadyUntilItIsProvedSo(t *testing.T) {
	t.Parallel()

	if err := aHost(t, newFakeNft()).Ready(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a host nobody prepared answered %v", err)
	}

	for name, tc := range map[string]struct {
		change func(*fakeNft)
		want   string
	}{
		"the role never ran": {
			change: func(f *fakeNft) {
				f.refuse = func(_ string, args []string) error {
					if strings.Contains(strings.Join(args, " "), "billet_guard") {
						return errors.New("Error: No such file or directory")
					}

					return nil
				}
			},
			want: "not installed",
		},
		// COULD NOT TELL IS NOT NO: a host billet cannot read is not one without a guard.
		"the guard cannot be read": {
			change: func(f *fakeNft) {
				f.refuse = func(_ string, args []string) error {
					if strings.Contains(strings.Join(args, " "), "billet_guard") {
						return errors.New("Operation not permitted")
					}

					return nil
				}
			},
			want: "could not read",
		},
		"the role is older than this binary": {
			change: func(f *fakeNft) { f.guard = `{"nftables":[]}` },
			want:   "would be dropped",
		},
		"the kernel refuses a guest's rules": {
			change: func(f *fakeNft) {
				f.refuse = func(_ string, args []string) error {
					if slices.Contains(args, "-c") {
						return errors.New("Error: Could not process rule: Not supported")
					}

					return nil
				}
			},
			want: "refuses a guest's rules",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFakeNft()
			tc.change(f)

			h := aHost(t, f)
			h.Prepare(t.Context())

			err := h.Ready()
			if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Ready = %v, want it naming %q", err, tc.want)
			}

			// A LIST IS REFUSED, WITH THAT REASON, AND NOTHING WAS INSTALLED FOR IT.
			err = h.Attach(t.Context(), aGuest(), listed(t, "example.com"))
			if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Attach of a list = %v", err)
			}

			// AN OPEN GUEST RUNS AS IT WOULD HAVE, UNWATCHED.
			if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
				t.Fatalf("Attach of an open guest = %v", err)
			}

			for _, s := range f.allScripts() {
				if strings.Contains(s, "job_"+guestKey) && !strings.HasPrefix(s, "-c") {
					t.Fatalf("rules were installed anyway: %s", s)
				}
			}
		})
	}
}

// PREPARING MAKES THE TABLES WHAT THEY SHOULD BE, whatever they held, in one transaction: each base
// chain is flushed before its rules go back, or a second run would double them, and a chain that
// lost them would stay without. The dry run comes first and changes nothing.
func TestPreparingLeavesTheTablesAsTheyShouldBe(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	preparedHost(t, f)

	got := f.allScripts()
	if len(got) != 2 || !strings.HasPrefix(got[0], "-c -f -\n") || !strings.Contains(got[0], "add table bridge billet_egress") ||
		!strings.HasPrefix(got[1], "-f -\n") || !strings.Contains(got[1], "add table bridge billet_egress") {
		t.Fatalf("scripts = %q, want the dry run of tables and a guest, then the tables", got)
	}

	lines := strings.Split(baseScript(), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "add rule ") {
			continue
		}

		fields := strings.Fields(line)
		flush := fmt.Sprintf("flush chain %s %s %s", fields[2], fields[3], fields[4])

		if !slices.Contains(lines[:i], flush) {
			t.Errorf("%q is not preceded by %q", line, flush)
		}
	}
}

// RUN AGAIN, A GUEST'S SCRIPT REPAIRS WHAT IS THERE: appended to a chain that kept its rules it would
// double them, and the first set of them would still decide.
func TestAGuestsRulesCanBeInstalledOverWhatIsLeftOfThem(t *testing.T) {
	t.Parallel()

	for _, listedGuest := range []bool{true, false} {
		lines := strings.Split(attachScript(binding{
			key: "k", iface: "bt-1", kind: BridgePort, gateway: netip.MustParsePrefix("192.0.2.1/24"), mark: markBase | 1,
			port: 1, listed: listedGuest,
		}), "\n")

		for i, line := range lines {
			if fields := strings.Fields(line); len(fields) > 4 && fields[0] == "add" && fields[1] == "chain" {
				if want := "flush " + strings.TrimPrefix(line, "add "); i+1 >= len(lines) || lines[i+1] != want {
					t.Errorf("listed %v: %q is not followed by %q", listedGuest, line, want)
				}
			}
		}
	}
}

// A PORT'S OWN IP TRAFFIC IS REFUSED FOR EVERY GUEST WITH RULES, and the name goes with them: a frame
// to a link-local MAC skips the bridge's hook, and so every chain a guest has.
func TestEveryGuestsPortIsTurnedAwayAtTheIPLayer(t *testing.T) {
	t.Parallel()

	const element = `bridge_ports { "bt-1" }`

	for _, listedGuest := range []bool{true, false} {
		b := binding{key: "k", iface: "bt-1", kind: BridgePort, gateway: netip.MustParsePrefix("192.0.2.1/24"),
			mark: markBase | 1, port: 1, listed: listedGuest}

		if s := attachScript(b); !strings.Contains(s, "add element inet billet_egress_routed "+element) {
			t.Errorf("listed %v: the attach does not name the port:\n%s", listedGuest, s)
		}

		if s := detachScript("k", "bt-1", BridgePort, markBase|1, listedGuest); !strings.Contains(s,
			"delete element inet billet_egress_routed "+element) {
			t.Errorf("listed %v: the detach leaves the name refused for the next guest:\n%s", listedGuest, s)
		}
	}

	if s := attachScript(binding{key: "k", iface: "ve-1", kind: Routed, gateway: netip.MustParsePrefix("192.0.2.1/24"),
		mark: markBase | 1, port: 1, listed: true}); strings.Contains(s, "bridge_ports") {
		t.Errorf("a routed interface's traffic is all IP, and would be refused:\n%s", s)
	}

	for _, want := range []string{
		"priority raw",
		"add rule inet billet_egress_routed port_l3 iifname @bridge_ports drop",
	} {
		if !strings.Contains(baseScript(), want) {
			t.Errorf("the base script has no %q", want)
		}
	}
}

// THE ORDER OF A LISTED GUEST'S RULES IS THE POLICY, so it is pinned. A prefix must
// never be a way around the resolver, or to another guest; IPv6 must never reach a
// rule written for IPv4.
func TestAListedGuestsRulesAreInTheOrderThePolicyNeeds(t *testing.T) {
	t.Parallel()

	s := attachScript(binding{
		key: guestKey, iface: guestTap, kind: BridgePort, gateway: netip.MustParsePrefix("172.20.0.1/24"),
		mark: 0xb1e70007, port: 42751, listed: true, prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		local: []netip.Addr{
			netip.MustParseAddr("172.20.0.1"), netip.MustParseAddr("172.20.1.1"), netip.MustParseAddr("10.0.0.5"),
		},
	})

	ordered := []string{
		"ether type arp accept",
		"ether type ip6 counter name ipv6_c_" + guestKey + " drop",
		"ip daddr 255.255.255.255 udp dport 67 accept",
		"ip saddr != 172.20.0.0/24 goto deny_" + guestKey,
		"ip daddr 172.20.0.1 meta l4proto { tcp, udp } th dport 53 meta mark set 0xb1e70007 accept",
		"ip daddr 172.20.0.1 accept",
		"meta l4proto { tcp, udp } th dport 53 counter name outside_c_" + guestKey + " goto deny_" + guestKey,
		// THE HOST'S OTHER ADDRESSES, BUT NOT ITS GATEWAY TWICE, AND AFTER DNS IS REFUSED.
		"ip daddr { 172.20.1.1, 10.0.0.5 } accept",
		"ip daddr 172.20.0.0/24 goto deny_" + guestKey,
		"ip daddr @allow_" + guestKey + " accept",
		"ip daddr @cidr_" + guestKey + " accept",
		"add rule bridge billet_egress job_" + guestKey + " goto deny_" + guestKey,
		`add element bridge billet_egress guests { "bt-7" : jump job_` + guestKey + " }",
	}

	at := -1

	for _, want := range ordered {
		i := strings.Index(s, want)
		if i < 0 {
			t.Fatalf("the script has no %q:\n%s", want, s)
		}

		if i <= at {
			t.Fatalf("%q is out of order:\n%s", want, s)
		}

		at = i
	}

	// WHAT REFUSES, IN ORDER, AND A DROP LAST: falling off this chain is the base chain's
	// accept, so a reject that lets a frame through would open the guest.
	at = -1

	for _, want := range []string{
		"deny_" + guestKey + " ip protocol { tcp, udp } update @denied_" + guestKey + " { ip daddr . ip protocol . th dport }",
		"deny_" + guestKey + " counter name denied_c_" + guestKey,
		"deny_" + guestKey + " ip protocol tcp reject with tcp reset",
		"deny_" + guestKey + " reject",
		"deny_" + guestKey + " drop",
	} {
		i := strings.Index(s, want)
		if i <= at {
			t.Fatalf("%q is missing or out of order:\n%s", want, s)
		}

		at = i
	}

	for _, want := range []string{
		"add element bridge billet_egress cidr_" + guestKey + " { 203.0.113.0/24 }",
		// THE MARK ON THE CONNECTION, AND THE TWO WAYS A PACKET CAN DISAGREE WITH IT.
		"udp dport 53 ct mark set 0xb1e70007 redirect to :42751",
		"tcp dport 53 ct mark set 0xb1e70007 redirect to :42751",
		"add element ip billet_egress_nat jobs { 0xb1e70007 : jump dns_" + guestKey + " }",
		"own_" + guestKey + " meta mark != 0xb1e70007 drop",
		"own_" + guestKey + " ct mark != 0xb1e70007 drop",
		"add element ip billet_egress_nat owners { 0xb1e70007 : jump own_" + guestKey + " }",
		// A REPLY LEAVES BY THE OWNER'S PORT ONLY.
		"reply_" + guestKey + ` oifname != "bt-7" drop`,
		"add element bridge billet_egress replies { 0xb1e70007 : jump reply_" + guestKey + " }",
		// AND THE PORT IS CUT OFF, COUNTING WHAT WAS AIMED AT A PEER.
		"fwd_" + guestKey + " meta pkttype { broadcast, multicast } drop",
		"fwd_" + guestKey + " counter name denied_c_" + guestKey + " drop",
		`add element bridge billet_egress ports { "bt-7" : jump fwd_` + guestKey + " }",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script has no %q:\n%s", want, s)
		}
	}
}

// A ROUTED GUEST IS THE SAME POLICY ON ANOTHER HOOK, which is the whole of what #510
// needs from this package: same rules, its own table, no bridge-only matches.
func TestARoutedGuestIsTheSamePolicyOnAnotherHook(t *testing.T) {
	t.Parallel()

	s := attachScript(binding{
		key: guestKey, iface: "veth7", kind: Routed, gateway: netip.MustParsePrefix("172.21.0.1/30"),
		mark: 0xb1e70007, port: 42751, listed: true,
	})

	for _, want := range []string{
		"add rule inet billet_egress_routed job_" + guestKey + " meta nfproto ipv6 counter name ipv6_c_" + guestKey + " drop",
		"add chain inet billet_egress_routed deny_" + guestKey,
		`add element inet billet_egress_routed guests { "veth7" : jump job_` + guestKey + " }",
		"add element ip billet_egress_nat jobs { 0xb1e70007 : jump dns_" + guestKey + " }",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script has no %q:\n%s", want, s)
		}
	}

	for _, refused := range []string{"ether type", "bridge billet_egress", "add element inet billet_egress_routed cidr_"} {
		if strings.Contains(s, refused) {
			t.Errorf("the script holds %q:\n%s", refused, s)
		}
	}
}

// AN OPEN GUEST'S CHAIN ONLY MARKS: nothing in it can refuse a packet.
func TestAnOpenGuestsChainJudgesNothing(t *testing.T) {
	t.Parallel()

	s := attachScript(binding{
		key: guestKey, iface: guestTap, kind: BridgePort, gateway: netip.MustParsePrefix("172.20.0.1/24"),
		mark: 0xb1e70007, port: 42751,
	})

	if !strings.Contains(s, "meta mark set 0xb1e70007") || !strings.Contains(s, "redirect to :42751") {
		t.Fatalf("the script does not watch DNS:\n%s", s)
	}

	for _, refused := range []string{"reject", "goto", "add set", "add counter", " accept", "fwd_"} {
		if strings.Contains(s, refused) {
			t.Errorf("an open guest's script holds %q:\n%s", refused, s)
		}
	}

	// THE ONLY DROPS ARE ITS OWN CONNECTIONS' AND REPLIES', WHICH GUARD OTHERS FROM IT.
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, " drop") && !strings.Contains(line, "own_") && !strings.Contains(line, "reply_") {
			t.Errorf("an open guest's script drops: %q", line)
		}
	}
}

// WHAT IS INSTALLED IS WHAT THE POLICY SAYS, through Attach: a listed guest given an open
// chain, or rules without its prefixes, passes every test that calls attachScript itself.
func TestAnAttachInstallsThePolicyItWasGiven(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	p := listed(t, "example.com", "203.0.113.0/24")
	if err := h.Attach(t.Context(), aGuest(), p); err != nil {
		t.Fatal(err)
	}

	script := scriptWith(t, f, "add chain bridge billet_egress job_"+guestKey)
	port := h.live[guestKey].fwd.port

	for _, want := range []string{
		"goto deny_" + guestKey,
		"cidr_" + guestKey + " { 203.0.113.0/24 }",
		"ip daddr { 10.9.0.5 } accept",
		fmt.Sprintf("redirect to :%d", port),
		`guests { "` + guestTap + `" : jump job_` + guestKey,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the installed script has no %q:\n%s", want, script)
		}
	}
}

// A RESOLVER STOPS WITH ITS RULES, on every path that takes them off: a socket left bound is
// a goroutine and a port for as long as the node runs, and nothing else ever closes it.
func TestAGuestsResolverStopsWithItsRules(t *testing.T) {
	t.Parallel()

	gone := func(t *testing.T, port uint16) {
		t.Helper()

		udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)))
		if err != nil {
			t.Fatalf("the resolver's port %d is still bound: %v", port, err)
		}

		_ = udp.Close()
	}

	t.Run("detach", func(t *testing.T) {
		t.Parallel()

		h := preparedHost(t, newFakeNft())
		if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
			t.Fatal(err)
		}

		port := h.live[guestKey].fwd.port
		h.Detach(t.Context(), guestKey)
		gone(t, port)
	})

	t.Run("handover", func(t *testing.T) {
		t.Parallel()

		h := preparedHost(t, newFakeNft())
		if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
			t.Fatal(err)
		}

		port := h.live[guestKey].fwd.port
		h.HandOver(t.Context())
		gone(t, port)
	})

	t.Run("a name reused", func(t *testing.T) {
		t.Parallel()

		h := preparedHost(t, newFakeNft())
		if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
			t.Fatal(err)
		}

		port := h.live[guestKey].fwd.port

		if err := h.Attach(t.Context(), Attachment{Key: "billet-next", Interface: guestTap, Network: "billet0",
			Kind: BridgePort}, nil); err != nil {
			t.Fatal(err)
		}

		gone(t, port)
	})

	t.Run("an install that failed", func(t *testing.T) {
		t.Parallel()

		f := newFakeNft()
		h := preparedHost(t, f)

		var port uint16

		f.refuse = func(script string, _ []string) error {
			if m := regexp.MustCompile(`redirect to :(\d+)`).FindStringSubmatch(script); m != nil {
				n, err := strconv.ParseUint(m[1], 10, 16)
				if err != nil {
					return err
				}

				port = uint16(n)

				return errors.New("Error: Could not process rule: Device or resource busy")
			}

			return nil
		}

		if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err == nil {
			t.Fatal("Attach succeeded")
		}

		if port == 0 {
			t.Fatal("this stages nothing: no rules were attempted")
		}

		gone(t, port)
	})
}

// THE FILE BEFORE THE RULES, or a process dying between them leaves rules whose policy
// nothing can say.
func TestAGuestsFileIsWrittenBeforeItsRules(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	var fileAtRules bool

	f.onScript = func(script string) {
		if strings.Contains(script, "add chain bridge billet_egress job_"+guestKey) {
			_, err := h.readFile(guestKey)
			fileAtRules = err == nil
		}
	}

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	if !fileAtRules {
		t.Fatal("the rules went in before the file that says what they are")
	}

	file, err := h.readFile(guestKey)
	if err != nil {
		t.Fatal(err)
	}

	if !file.Listed || !slices.Equal(file.Allow, []string{"example.com"}) || file.Network != "billet0" {
		t.Fatalf("file = %+v", file)
	}
}

// A LIST THAT CANNOT BE INSTALLED REFUSES THE LAUNCH AND LEAVES NOTHING.
func TestAListThatCannotBeInstalledLeavesNothing(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Error: Could not process rule: Device or resource busy")
		}

		return nil
	}

	err := h.Attach(t.Context(), aGuest(), listed(t, "example.com"))
	if err == nil || !strings.Contains(err.Error(), "install the rules") {
		t.Fatalf("Attach = %v", err)
	}

	if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its file was left: %v", err)
	}

	if len(h.live) != 0 || len(h.slots) != 0 || len(h.byIface) != 0 {
		t.Fatalf("books left: live %v, slots %v, byIface %v", h.live, h.slots, h.byIface)
	}
}

// AN OPEN GUEST'S INSTALL FAILING COSTS ITS NAMES, NEVER THE JOB.
func TestAnOpenGuestRunsWhenItCannotBeWatched(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Error: Could not process rule: Device or resource busy")
		}

		return nil
	}

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatalf("Attach = %v", err)
	}

	if len(h.live) != 0 {
		t.Fatalf("a guest whose rules failed is on the books: %v", h.live)
	}
}

func TestANameThatCannotBeRenderedSafelyIsRefused(t *testing.T) {
	t.Parallel()

	h := preparedHost(t, newFakeNft())

	for name, a := range map[string]Attachment{
		"a key that escapes": {Key: `x }; delete table bridge billet_egress; {`, Interface: guestTap},
		"a capital key":      {Key: "Billet-1", Interface: guestTap},
		"an empty key":       {Interface: guestTap},
		"a device quote":     {Key: guestKey, Interface: `bt"7`},
		"a device too long":  {Key: guestKey, Interface: "0123456789abcdef"},
		"no device":          {Key: guestKey},
	} {
		if err := h.Attach(t.Context(), a, listed(t, "example.com")); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// AN NFT THAT FAILED MAY HAVE COMMITTED: a process killed after the kernel's commit reports
// the same as one the kernel refused. So what it would have made is removed, and a removal
// finding nothing is the proof of none.
func TestAFailedInstallIsNotProofNothingWasInstalled(t *testing.T) {
	t.Parallel()

	failInstall := func(deleteErr error) func(string, []string) error {
		return func(script string, _ []string) error {
			switch {
			case strings.Contains(script, "add chain bridge billet_egress job_"):
				return errors.New("signal: killed")
			case strings.Contains(script, "delete chain bridge billet_egress job_"):
				return deleteErr
			}

			return nil
		}
	}

	for name, removal := range map[string]error{
		"it had committed":     nil,
		"it had not committed": errors.New("Error: No such file or directory"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFakeNft()
			h := preparedHost(t, f)
			f.refuse = failInstall(removal)

			err := h.Attach(t.Context(), aGuest(), listed(t, "example.com"))
			if err == nil || errors.Is(err, errRulesUncertain) {
				t.Fatalf("Attach = %v", err)
			}

			if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("its file was left: %v", err)
			}

			if len(h.byIface) != 0 || len(h.slots) != 0 {
				t.Fatalf("books left: %v, %v", h.byIface, h.slots)
			}
		})
	}

	// NEITHER ANSWER: THE RULES MAY BE THERE, SO THE BOOKS KEEP THEM, AND EVEN AN OPEN GUEST
	// DOES NOT START, SINCE ITS DNS MAY BE REDIRECTED TO A RESOLVER THAT HAS GONE.
	for name, p := range map[string]*Policy{"a list": listed(t, "example.com"), "an open guest": Observe()} {
		t.Run(name+", could not tell", func(t *testing.T) {
			t.Parallel()

			f := newFakeNft()
			h := preparedHost(t, f)
			f.refuse = failInstall(errors.New("signal: killed"))

			err := h.Attach(t.Context(), aGuest(), p)
			if !errors.Is(err, errRulesUncertain) {
				t.Fatalf("Attach = %v, want it refused as uncertain", err)
			}

			if _, err := h.readFile(guestKey); err != nil {
				t.Fatalf("the file that names the rules was removed: %v", err)
			}

			if h.byIface[guestTap] != guestKey || len(h.slots) != 1 {
				t.Fatalf("books: %v, %v", h.byIface, h.slots)
			}

			// A LAUNCH UNWINDING CLEARS THEM ONCE THEY WILL GO.
			f.refuse = nil
			h.Detach(t.Context(), guestKey)

			if len(h.byIface) != 0 || len(h.slots) != 0 {
				t.Fatalf("books after the detach: %v, %v", h.byIface, h.slots)
			}
		})
	}
}

// RULES SOMEONE ELSE REMOVED STILL COME OFF: one missing object fails the transaction whole,
// and left at that every launch drawing their interface would be refused for them.
func TestRulesThatArePartlyGoneStillComeOff(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatal(err)
	}

	f.refuse = func(script string, _ []string) error {
		if strings.Count(script, "\n") > 1 && strings.Contains(script, "delete chain") {
			return errors.New("Error: No such file or directory")
		}

		if strings.Contains(script, "delete chain bridge billet_egress job_") {
			return errors.New("Error: No such file or directory")
		}

		return nil
	}

	h.Detach(t.Context(), guestKey)

	if holder, held := h.byIface[guestTap]; held {
		t.Fatalf("the name is still held by %s", holder)
	}

	scriptWith(t, f, "delete chain ip billet_egress_nat dns_"+guestKey)

	if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its file was left: %v", err)
	}

	// AND SOMETHING ELSE FAILING IS STILL A FAILURE.
	f = newFakeNft()
	h = preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatal(err)
	}

	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "delete chain") {
			return errors.New("Error: Could not process rule: Device or resource busy")
		}

		return nil
	}

	h.Detach(t.Context(), guestKey)

	if h.byIface[guestTap] != guestKey {
		t.Fatal("rules that would not go left the books")
	}
}

// A GUEST'S DNS CONNECTIONS GO WITH ITS RULES, and when its resolver changes: their translation
// outlives the rule, so one socket held across the change would go on reaching a dead port.
func TestAGuestsConnectionsGoWhenItsResolverDoes(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatal(err)
	}

	mark := h.live[guestKey].mark

	h.HandOver(t.Context())

	if !slices.Equal(f.forgotten, []uint32{mark}) {
		t.Fatalf("forgot %v after a handover, want %#x", f.forgotten, mark)
	}

	f = newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h = aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if !slices.Equal(f.forgotten, []uint32{0xb1e70009}) {
		t.Fatalf("forgot %v after a new resolver, want the adopted guest's", f.forgotten)
	}
}

// DETACHING IS READ, THEN REMOVED, and what it read is kept for one reader.
func TestDetachKeepsWhatTheRulesCountedForOneReader(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	if _, ok := h.Take(guestKey); ok {
		t.Fatal("a guest still attached had a record to take")
	}

	h.Detach(t.Context(), guestKey)

	script := scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)
	for _, want := range []string{
		`delete element bridge billet_egress guests { "bt-7" }`,
		"delete chain bridge billet_egress deny_" + guestKey,
		"delete set bridge billet_egress allow_" + guestKey,
		"delete counter bridge billet_egress denied_c_" + guestKey,
		"delete chain ip billet_egress_nat dns_" + guestKey,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the detach has no %q:\n%s", want, script)
		}
	}

	rec, ok := h.Take(guestKey)
	if !ok {
		t.Fatal("nothing to take after the detach")
	}

	want := []Denied{{Addr: netip.MustParseAddr("198.51.100.2"), Protocol: "tcp", Port: 443, Packets: 3}}
	if !rec.Listed || rec.DeniedPackets != 3 || rec.OutsideResolverPackets != 3 || rec.IPv6Packets != 3 ||
		!slices.Equal(rec.Denied, want) || rec.Incomplete {
		t.Fatalf("record = %+v", rec)
	}

	if _, ok := h.Take(guestKey); ok {
		t.Fatal("a record was taken twice")
	}

	if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its file was left: %v", err)
	}
}

// COUNTERS THAT COULD NOT BE READ ARE NOT A GUEST REFUSED NOTHING, and a set of destinations
// is one of those counts.
func TestCountsThatCouldNotBeReadAreNotZero(t *testing.T) {
	t.Parallel()

	for _, listing := range []string{"list counter", "list set"} {
		f := newFakeNft()
		h := preparedHost(t, f)

		if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
			t.Fatal(err)
		}

		f.refuse = func(_ string, args []string) error {
			if strings.Contains(strings.Join(args, " "), listing) {
				return errors.New("Operation not permitted")
			}

			return nil
		}

		h.Detach(t.Context(), guestKey)

		rec, ok := h.Take(guestKey)
		if !ok || !rec.Incomplete {
			t.Errorf("%s: record = %+v, %v, want it marked incomplete", listing, rec, ok)
		}
	}
}

// AN OPEN GUEST HAS NOTHING TO COUNT, which is complete rather than a read that failed: its
// rules made no counters to read.
func TestAnOpenGuestsRecordIsCompleteWithNoCounts(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatal(err)
	}

	f.refuse = func(_ string, args []string) error {
		if strings.Contains(strings.Join(args, " "), "list counter") {
			return errors.New("an open guest has no counter to read")
		}

		return nil
	}

	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || rec.Incomplete || rec.DeniedPackets != 0 || rec.Listed {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

// A FULL SET HAS STOPPED NOTICING: a guest filling it with decoys hides what it tries next.
func TestAFullDeniedSetIsALowerBound(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	elements := make([]string, 0, MaxDenied)
	for i := range MaxDenied {
		elements = append(elements, fmt.Sprintf(`{"elem":{"val":{"concat":["198.18.%d.%d","tcp",1]},"counter":{"packets":1}}}`,
			i/256, i%256))
	}

	f.setListing = `{"nftables":[{"set":{"elem":[` + strings.Join(elements, ",") + `]}}]}`

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Incomplete {
		t.Fatalf("incomplete %v, %v", rec.Incomplete, ok)
	}
}

// RULES THAT WOULD NOT GO WAIT FOR THE NEXT GUEST ON THE NAME, which must not inherit
// them, and which is refused if they still will not go.
func TestANextGuestDoesNotInheritADeadGuestsRules(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	busy := func(script string, _ []string) error {
		if strings.Contains(script, "delete chain bridge billet_egress job_") {
			return errors.New("Error: Could not process rule: Device or resource busy")
		}

		return nil
	}

	f.refuse = busy
	h.Detach(t.Context(), guestKey)

	next := Attachment{Key: "billet-fedcba9876543210", Interface: guestTap, Network: "billet0", Kind: BridgePort}

	// A GUEST WITH NO POLICY ASKS TOO: it would otherwise run under the dead one's.
	for name, p := range map[string]*Policy{"no policy": nil, "a list": listed(t, "example.com")} {
		if err := h.Attach(t.Context(), next, p); err == nil || !strings.Contains(err.Error(), "could not be cleared") {
			t.Fatalf("%s: Attach = %v, want the launch refused", name, err)
		}
	}

	f.refuse = nil

	if err := h.Attach(t.Context(), next, nil); err != nil {
		t.Fatalf("Attach once the rules can go = %v", err)
	}

	scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)

	if holder, held := h.byIface[guestTap]; held {
		t.Fatalf("the name is still held by %s", holder)
	}
}

// aFoundGuest stages rules an earlier process left.
func aFoundGuest(f *fakeNft, mark uint32) {
	f.guests = fmt.Sprintf(`{"nftables":[{"map":{"elem":[[%q,{"jump":{"target":"job_%s"}}]]}}]}`, guestTap, guestKey)
	f.jobs = fmt.Sprintf(`{"nftables":[{"map":{"elem":[[%d,{"jump":{"target":"dns_%s"}}]]}}]}`, mark, guestKey)
}

// keepFile stages the file an earlier process wrote for a guest.
func keepFile(t *testing.T, h *Host, a Attachment, p *Policy) {
	t.Helper()

	if err := h.writeFile(a, p, 0); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileGivesARunningListedGuestANewResolverAndKeepsItClosed(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "flush chain ip billet_egress_nat dns_"+guestKey)
	if !strings.Contains(script, "redirect to :") {
		t.Fatalf("the redirect was not pointed anywhere:\n%s", script)
	}

	for _, s := range f.allScripts() {
		// PUT IN AGAIN OVER WHAT IS THERE, AND NOTHING TAKEN AWAY: its sets keep what they hold.
		if strings.Contains(s, "delete ") {
			t.Fatalf("a running listed guest's rules were removed: %s", s)
		}
	}

	// A DETACH READS WHAT THE KERNEL KEPT COUNTING, AND SAYS IT DID NOT SEE IT ALL.
	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || !rec.Listed || !rec.Incomplete || rec.DeniedPackets != 3 {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	scriptWith(t, f, "delete element ip billet_egress_nat jobs { 0xb1e70009 }")
}

func TestReconcileHandsARunningOpenGuestBackToTheResolver(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), Observe())
	h.Prepare(t.Context())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)
	if strings.Contains(script, "delete set") {
		t.Fatalf("an open guest's removal names a list's objects, which fails whole:\n%s", script)
	}

	if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("its file was left: %v", err)
	}
}

// NOTHING CAN SAY WHAT THIS GUEST MAY RESOLVE, AND OPENING IT WOULD BE A GUESS.
func TestARunningListedGuestWithNoFileStaysClosedThroughAHandOver(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})
	h.HandOver(t.Context())

	for _, s := range f.allScripts() {
		if strings.Contains(s, "delete") {
			t.Fatalf("a closed guest's rules were removed: %s", s)
		}
	}

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Listed || !rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	script := scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)
	if !strings.Contains(script, "delete set bridge billet_egress allow_"+guestKey) {
		t.Fatalf("its detach left a list's objects:\n%s", script)
	}
}

func TestReconcileRemovesOnlyWhatIsGoneAndHasAFile(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.Prepare(t.Context())

	// RULES WITH NO FILE MAY BE ANOTHER DEPLOYMENT'S ON THIS HOST.
	h.Reconcile(t.Context(), map[string]bool{})

	for _, s := range f.allScripts() {
		if strings.Contains(s, "delete") {
			t.Fatalf("rules nobody here can vouch for were removed: %s", s)
		}
	}

	f = newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h = aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	f.denyHeld = true

	h.Reconcile(t.Context(), map[string]bool{})

	script := scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)
	if !strings.Contains(script, "delete set") {
		t.Fatalf("a list's objects were left:\n%s", script)
	}
}

// RECONCILE RUNS AT EVERY REGISTRATION, AND A LAUNCH CAN HAVE ATTACHED SINCE ITS LIST:
// that guest is not in the list, and is not gone.
func TestReconcileDoesNotTouchWhatThisProcessAttached(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	h.Reconcile(t.Context(), map[string]bool{})

	for _, s := range f.allScripts() {
		if strings.Contains(s, "delete") {
			t.Fatalf("a guest attached since the list was removed: %s", s)
		}
	}
}

// A FILE WITH NO RULES IS NOTHING ONLY ONCE SOMETHING HAS SAID THE GUEST IS NOT RUNNING. Read as
// nothing at the start, a flushed firewall and a restart would have left a listed job running
// open with no record it ever had a list.
func TestAFileWithNoRulesIsJudgedByWhatIsRunning(t *testing.T) {
	t.Parallel()

	start := func(t *testing.T, p *Policy) (*fakeNft, *Host) {
		t.Helper()

		f := newFakeNft()
		h := aHost(t, f)
		keepFile(t, h, aGuest(), p)
		h.Prepare(t.Context())

		if _, err := h.readFile(guestKey); err != nil {
			t.Fatalf("preparing removed a file nothing has judged yet: %v", err)
		}

		return f, h
	}

	t.Run("a listed guest still running has its rules put back", func(t *testing.T) {
		t.Parallel()

		f, h := start(t, listed(t, "example.com", "203.0.113.0/24"))

		h.Reconcile(t.Context(), map[string]bool{guestKey: true})

		script := scriptWith(t, f, "add chain bridge billet_egress job_"+guestKey)
		if !strings.Contains(script, `guests { "`+guestTap+`" : jump`) || !strings.Contains(script, "203.0.113.0/24") {
			t.Fatalf("the rules put back are not its own:\n%s", script)
		}

		h.Detach(t.Context(), guestKey)

		if rec, ok := h.Take(guestKey); !ok || !rec.Incomplete || !rec.Listed {
			t.Fatalf("record = %+v, %v: what ran open for a while is not the whole job", rec, ok)
		}
	})

	t.Run("an open guest still running is left alone", func(t *testing.T) {
		t.Parallel()

		f, h := start(t, Observe())

		h.Reconcile(t.Context(), map[string]bool{guestKey: true})

		if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("its file was kept: %v", err)
		}

		for _, s := range f.allScripts() {
			if strings.Contains(s, "job_"+guestKey) {
				t.Fatalf("rules were installed for it: %s", s)
			}
		}
	})

	t.Run("a guest that has gone leaves nothing", func(t *testing.T) {
		t.Parallel()

		f, h := start(t, listed(t, "example.com"))

		h.Reconcile(t.Context(), map[string]bool{})

		if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("its file was kept: %v", err)
		}

		for _, s := range f.allScripts() {
			if strings.Contains(s, "job_"+guestKey) {
				t.Fatalf("rules were installed for it: %s", s)
			}
		}
	})

	// LATER RECOVERIES JUDGE THEM TOO: what one could not read is read again, and none is a
	// launch's, which holds the lifecycle lock from its file to its rules.
	t.Run("a later recovery judges them too", func(t *testing.T) {
		t.Parallel()

		f := newFakeNft()
		h := preparedHost(t, f)
		h.Reconcile(t.Context(), map[string]bool{})
		keepFile(t, h, aGuest(), listed(t, "example.com"))
		h.Reconcile(t.Context(), map[string]bool{guestKey: true})

		if h.live[guestKey] == nil {
			t.Fatal("a running guest's list was not put back")
		}
	})
}

// A RECOVERY WAITS FOR AN ATTACH THAT HAS WRITTEN ITS FILE AND NOT YET ITS RULES. A sweep's List was
// taken before the launch, so the file reads as a guest that has gone: judged now, it would be deleted
// from under a guest that is about to have rules, and no later process could say what they were.
func TestARecoveryDoesNotJudgeAnAttachInProgress(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	inAttach, release := make(chan struct{}), make(chan struct{})
	run := h.run
	h.run = func(ctx context.Context, script string, args ...string) ([]byte, error) {
		if strings.Contains(script, "add chain bridge billet_egress job_"+guestKey) {
			close(inAttach)
			<-release
		}

		return run(ctx, script, args...)
	}

	attached := make(chan error, 1)

	go func() { attached <- h.Attach(t.Context(), aGuest(), listed(t, "example.com")) }()

	<-inAttach

	reconciled := make(chan struct{})

	go func() {
		defer close(reconciled)
		h.Reconcile(t.Context(), map[string]bool{})
	}()

	select {
	case <-reconciled:
		t.Error("a recovery ran through an attach in progress")
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := h.readFile(guestKey); err != nil {
		t.Errorf("the file of an attach in progress went: %v", err)
	}

	close(release)

	if err := <-attached; err != nil {
		t.Fatal(err)
	}

	<-reconciled

	if _, err := h.readFile(guestKey); err != nil {
		t.Fatalf("the file of an attached guest went: %v", err)
	}
}

// A LIVE GUEST WHOSE RULES HAVE GONE HAS THEM PUT BACK AT THE NEXT RECOVERY, which runs at every
// registration, and its set starts again: closed, rather than open until the job ends.
func TestAFlushIsFoundAtTheNextRecovery(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	guest := h.live[guestKey]
	if err := h.admit(t.Context(), guest, []netip.Addr{anAddr}); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.tableListing = func(call string) string { return anEnforcingListing(call, guest.mark) }
	f.mu.Unlock()

	before := len(f.allScripts())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if after := len(f.allScripts()); after != before {
		t.Fatalf("rules that are there were installed again: %d scripts", after-before)
	}

	f.mu.Lock()
	f.tableListing = func(string) string { return `{"nftables":[]}` }
	f.mu.Unlock()

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	got := f.allScripts()[before:]
	if len(got) != 1 || !strings.Contains(got[0], "add table bridge billet_egress") ||
		!strings.Contains(got[0], "add chain bridge billet_egress job_"+guestKey) {
		t.Fatalf("scripts = %q, want the tables and the guest's rules in one transaction", got)
	}

	// THE SET IS EMPTY AGAIN, SO THE SAME ADDRESS MUST BE ADMITTED AGAIN.
	if err := h.admit(t.Context(), guest, []netip.Addr{anAddr}); err != nil {
		t.Fatal(err)
	}

	if after := f.allScripts(); !strings.Contains(after[len(after)-1], "add element bridge billet_egress allow_"+guestKey) {
		t.Fatalf("an address the set lost was not added back: %q", after[len(after)-1])
	}

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

// COULD NOT TELL WHETHER A GUEST'S RULES ARE A LIST'S READS AS A LIST, the reading that keeps it
// closed: a failed listing must not hand a listed guest to dnsmasq.
func TestADenyChainThatCouldNotBeReadKeepsAGuestClosed(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.Prepare(t.Context())

	f.refuse = func(_ string, args []string) error {
		if strings.Contains(strings.Join(args, " "), "deny_") {
			return errors.New("Operation not permitted")
		}

		return nil
	}

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	for _, s := range f.allScripts() {
		if strings.Contains(s, "delete") {
			t.Fatalf("a guest nothing could read the rules of was opened: %s", s)
		}
	}
}

// A GUEST WHOSE ADOPTION FAILED STILL HAS WHAT THE KERNEL COUNTED: it was refused things all
// the same, and "no report recorded" would read as a job nothing watched.
func TestAGuestWhoseAdoptionFailedIsStillReported(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	h.addrOf = func(string) (netip.Prefix, error) { return netip.Prefix{}, errors.New("no such device") }

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	// ITS REDIRECT NAMES A PORT NOTHING HOLDS, WHICH THE KERNEL CAN GIVE THE NEXT GUEST'S RESOLVER.
	scriptWith(t, f, "add rule ip billet_egress_nat dns_"+guestKey+" drop")

	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || !rec.Listed || !rec.Incomplete || rec.DeniedPackets != 3 {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	// AND ITS RULES COME OFF AS A LIST'S: left behind, they refuse the next listed launch on this tap.
	for _, want := range []string{
		"delete element bridge billet_egress ports { \"" + guestTap + "\" }", "delete chain bridge billet_egress fwd_" + guestKey,
		"delete chain bridge billet_egress deny_" + guestKey, "delete set bridge billet_egress allow_" + guestKey,
		"delete counter bridge billet_egress denied_c_" + guestKey,
	} {
		scriptWith(t, f, want)
	}
}

// WHAT A GUEST'S RULES WERE JUDGED TO BE IS KEPT: a removal that stopped halfway took the deny chain,
// and the kernel would answer that the second attempt need not remove the sets.
func TestARemovalThatStoppedHalfwayKnowsWhatItWasRemoving(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	h := aHost(t, f)

	a := &attached{Attachment: aGuest(), mark: 0xb1e70009}
	if !h.listedOf(t.Context(), a) {
		t.Fatal("rules with a deny chain did not read as a list's")
	}

	f.mu.Lock()
	f.denyHeld = false
	f.mu.Unlock()

	if !h.listedOf(t.Context(), a) {
		t.Fatal("the second attempt read the kernel again, whose deny chain the first removed")
	}
}

// A RUNNING GUEST WHOSE FILE CANNOT BE READ AND WHOSE RULES HOLD A LIST SENDS ITS DNS NOWHERE: nothing
// here can serve it, and the port it names can come to be another guest's resolver.
func TestAListNobodyCanReadSendsItsDNSNowhere(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	scriptWith(t, f, "flush chain ip billet_egress_nat dns_"+guestKey+"\nadd rule ip billet_egress_nat dns_"+guestKey+" drop")

	f.mu.Lock()
	defer f.mu.Unlock()

	if !slices.Contains(f.forgotten, uint32(0xb1e70009)) {
		t.Fatalf("its connections, which keep the old translation, were not deleted: %v", f.forgotten)
	}
}

// A RESTORE THAT FAILS IS TRIED AGAIN, AND ITS FILE STAYS: a failed launch's cleanup would have deleted
// the only record of a list while its guest went on running open, and the guest would have left no
// report at all. Connections from before the flush go first, named by the mark the file kept.
func TestARestoreThatFailsIsTriedAgainAtTheNextRecovery(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.writeFile(aGuest(), listed(t, "example.com"), 0xb1e70005); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Operation not permitted")
		}

		return nil
	}
	f.mu.Unlock()

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if _, err := h.readFile(guestKey); err != nil {
		t.Fatalf("a failed restore removed the only record of a running guest's list: %v", err)
	}

	if h.live[guestKey] != nil {
		t.Fatal("rules that did not go in are on the books")
	}

	f.mu.Lock()
	f.refuse = nil
	forgotten := slices.Clone(f.forgotten)
	f.mu.Unlock()

	if !slices.Contains(forgotten, uint32(0xb1e70005)) {
		t.Fatalf("forgotten = %v, want the mark from before the flush", forgotten)
	}

	// STILL HELD: a launch given it now would have this guest's next attempt delete its connections.
	if _, held := h.slots[0xb1e70005]; !held {
		t.Fatal("the mark of a guest being retried was handed back")
	}

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if h.live[guestKey] == nil {
		t.Fatal("the next recovery did not try again")
	}

	if got := h.live[guestKey].mark; got != 0xb1e70005 {
		t.Fatalf("mark = %#x, want the one its connections and what is left of its rules carry", got)
	}

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Listed || !rec.Incomplete {
		t.Fatalf("record = %+v, %v: a job that ran open for a while does not read as whole", rec, ok)
	}
}

// RULES THAT NEVER CAME BACK ARE STILL A JOB WITH A LIST, and "no report recorded" would read as a
// job nothing was asked to watch.
func TestAGuestThatEndedUnrestoredIsReportedAsWhatItWas(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))

	f.mu.Lock()
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Operation not permitted")
		}

		return nil
	}
	f.mu.Unlock()

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})
	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Listed || !rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	if _, err := h.readFile(guestKey); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the file of a guest that has gone is still there: %v", err)
	}
}

// A POLICY FILE THAT CANNOT BE READ IS COULD NOT TELL: deleted, a running guest's list is forgotten
// for good. Once a List shows the guest gone, there is nothing left to tell.
func TestAnUnreadableFileOfARunningGuestIsKept(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)
	path := filepath.Join(h.stateDir, guestKey+recordSuffix)
	keepFile(t, h, aGuest(), listed(t, "example.com"))

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		h.Reconcile(t.Context(), map[string]bool{guestKey: true})

		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the file went: %v", err)
		}
	}

	h.Reconcile(t.Context(), map[string]bool{})

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the file of a guest that has gone is still there: %v", err)
	}
}

// A FLUSH TAKES THE ROLE'S GUARD TOO, AND A RUNNING GUEST'S LIST IS NOT THE GUARD'S TO WITHHOLD: its
// rules go back though nothing new may attach, and a converge that restores the guard is noticed at
// the next recovery with no process starting.
func TestAMissingGuardDoesNotKeepARunningGuestsListOff(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	guard := f.guard
	f.guard = `{"nftables":[]}`

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	if err := h.Ready(); err == nil {
		t.Fatal("a host with no guard is ready")
	}

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if h.live[guestKey] == nil {
		t.Fatal("a running listed guest's rules stayed off until something restarted the node")
	}

	scriptWith(t, f, "add chain bridge billet_egress job_"+guestKey)

	if err := h.Ready(); err == nil {
		t.Fatal("a host with no guard became ready")
	}

	// THE KERNEL NOW LISTS WHAT THIS PROCESS PUT BACK, and asking again must not take it for another's.
	f.mu.Lock()
	f.guard = guard
	aFoundGuest(f, h.live[guestKey].mark)
	f.mu.Unlock()

	restored := h.live[guestKey]

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if err := h.Ready(); err != nil {
		t.Fatalf("a converge was not noticed: %v", err)
	}

	// ADOPTED AGAIN IT WOULD LOOK THE SAME ON THE BOOKS, WITH A SECOND RESOLVER LEAKED BEHIND IT.
	if h.live[guestKey] != restored {
		t.Fatal("what this process restored was taken for another's, and adopted")
	}
}

// PUT BACK OVER AN ADOPTED GUEST, ITS RULES STILL HOLD THE HOST'S OTHER ADDRESSES: a guest on the
// other bridge reaches the node's cache across the host.
func TestAnAdoptedGuestsRulesComeBackWithTheHostsAddresses(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if h.live[guestKey] == nil {
		t.Fatal("not adopted")
	}

	f.mu.Lock()
	f.tableListing = func(string) string { return `{"nftables":[]}` }
	f.mu.Unlock()

	before := len(f.allScripts())

	h.verify(t.Context())

	got := f.allScripts()
	if len(got) != before+1 || !strings.Contains(got[before], "10.9.0.5") {
		t.Fatalf("scripts = %q: the host's other address went", got[before:])
	}
}

// RULES THAT CANNOT BE READ ARE COULD NOT TELL: nothing is reinstalled, and the job is not marked as
// having run open.
func TestRulesThatCouldNotBeReadAreLeftAlone(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	before := len(f.allScripts())

	h.verify(t.Context())

	if after := len(f.allScripts()); after != before {
		t.Fatalf("%d scripts ran on rules nobody could read", after-before)
	}

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

// guestRules is what a listed guest's rules are rendered from, on this file's fake host.
func guestRules(mark uint32) binding {
	return binding{
		key: guestKey, iface: guestTap, kind: BridgePort, gateway: netip.MustParsePrefix("127.0.0.1/8"), mark: mark,
		port: 1, listed: true, local: []netip.Addr{netip.MustParseAddr("10.9.0.5")},
	}
}

// enforcingEntries is what nft lists of a table holding those rules, each rule its own entry.
func enforcingEntries(call string, mark uint32) []string {
	verdict := func(name, key, chain string) string {
		return `{"map":{"name":"` + name + `","elem":[[` + key + `,{"jump":{"target":"` + chain + `"}}]]}}`
	}

	iface, number := fmt.Sprintf("%q", guestTap), strconv.FormatUint(uint64(mark), 10)

	var table string

	var entries []string

	switch {
	case strings.Contains(call, "inet billet_egress_routed"):
		table = "inet billet_egress_routed "
		entries = []string{`{"set":{"name":"bridge_ports","elem":[` + iface + `]}}`}
	case strings.Contains(call, "ip billet_egress_nat"):
		table = "ip billet_egress_nat "
		entries = []string{verdict("jobs", number, "dns_"+guestKey), verdict("owners", number, "own_"+guestKey)}
	default:
		table = "bridge billet_egress "
		entries = []string{
			verdict("guests", iface, "job_"+guestKey), verdict("ports", iface, "fwd_"+guestKey),
			verdict("replies", number, "reply_"+guestKey),
		}
	}

	for chain, n := range countRules(baseScript() + attachScript(guestRules(mark))) {
		if name, ours := strings.CutPrefix(chain, table); ours {
			for range n {
				entries = append(entries, `{"rule":{"chain":"`+name+`"}}`)
			}
		}
	}

	return entries
}

func anEnforcingListing(call string, mark uint32) string {
	return `{"nftables":[` + strings.Join(enforcingEntries(call, mark), ",") + `]}`
}

// EVERY PART OF A GUEST'S ENFORCEMENT IS LOOKED FOR: a chain that exists and that nothing jumps to, or
// that a flush emptied, refuses nothing, so taking away any one thing is a list no longer in force.
// ONE RULE IS ENOUGH: the chain that lost only its last refusal falls through to accept. A DOUBLED
// CHAIN IS NOT WHAT WAS RENDERED EITHER, and is put right.
func TestEveryPartOfAListsEnforcementIsLookedFor(t *testing.T) {
	t.Parallel()

	const mark = uint32(0xb1e70009)

	read := func(call string, change func([]string) []string) ruleTable {
		entries := enforcingEntries(call, mark)
		if change != nil {
			entries = change(entries)
		}

		table, err := parseRuleTable([]byte(`{"nftables":[` + strings.Join(entries, ",") + `]}`))
		if err != nil {
			t.Fatalf("%q: %v", entries, err)
		}

		return table
	}

	holds := func(change func([]string) []string, against uint32) bool {
		held := heldRules{
			bridge: read("bridge billet_egress", change), routed: read("inet billet_egress_routed", change),
			nat: read("ip billet_egress_nat", change),
		}

		return held.enforces(guestRules(against))
	}

	if !holds(nil, mark) {
		t.Fatal("whole rules read as not in force")
	}

	if holds(nil, mark+1) {
		t.Fatal("another mark's rules read as this guest's")
	}

	for _, part := range []string{
		`"chain":"prerouting"`, `"chain":"forward"`, `"chain":"output"`, `"chain":"port_l3"`, `"chain":"own"`,
		`"chain":"job_` + guestKey, `"chain":"deny_` + guestKey, `"chain":"fwd_` + guestKey, `"chain":"reply_` + guestKey,
		`"chain":"dns_` + guestKey, `"chain":"own_` + guestKey, `"name":"guests"`, `"name":"ports"`, `"name":"replies"`,
		`"name":"jobs"`, `"name":"owners"`, `"name":"bridge_ports"`,
	} {
		if holds(func(entries []string) []string {
			return slices.DeleteFunc(entries, func(e string) bool { return strings.Contains(e, part) })
		}, mark) {
			t.Errorf("rules without %s read as in force", part)
		}
	}

	job := `"chain":"job_` + guestKey

	if holds(func(entries []string) []string {
		i := slices.IndexFunc(entries, func(e string) bool { return strings.Contains(e, job) })
		if i >= 0 {
			return slices.Delete(entries, i, i+1)
		}

		return entries
	}, mark) {
		t.Error("a guest's chain missing one rule reads as in force")
	}

	if holds(func(entries []string) []string {
		for _, e := range entries {
			if strings.Contains(e, job) {
				return append(entries, e)
			}
		}

		return entries
	}, mark) {
		t.Error("a guest's chain holding one rule twice reads as what was rendered")
	}
}

// A GUEST FOUND WITH NO MARK TAKES THE ONE ITS FILE NAMES, AND ZERO REACHES NOTHING: it is what every
// connection nobody marked carries, so a deletion by it would take the host's, and rules keyed on it
// would catch them.
func TestALostMarkIsNeverRenderedOrForgottenAsZero(t *testing.T) {
	t.Parallel()

	for name, fileMark := range map[string]uint32{"its file names one": 0xb1e70005, "nothing does": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFakeNft()
			f.denyHeld = true
			f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

			h := aHost(t, f)

			if err := h.writeFile(aGuest(), listed(t, "example.com"), fileMark); err != nil {
				t.Fatal(err)
			}

			h.Prepare(t.Context())
			h.Reconcile(t.Context(), map[string]bool{guestKey: true})

			guest := h.live[guestKey]
			if guest == nil {
				t.Fatal("not adopted")
			}

			if !validMark(guest.mark) || (fileMark != 0 && guest.mark != fileMark) {
				t.Fatalf("mark = %#x, want %#x if it was known", guest.mark, fileMark)
			}

			for _, script := range f.allScripts() {
				if strings.Contains(script, "{ 0x0 ") {
					t.Fatalf("rules keyed on no mark:\n%s", script)
				}
			}

			f.mu.Lock()
			defer f.mu.Unlock()

			if slices.Contains(f.forgotten, 0) {
				t.Fatalf("forgotten = %v: every unmarked connection on the host", f.forgotten)
			}
		})
	}
}

// A GUEST WITH NO MARK IN THE NAT TABLES HAS ONE IN THE OTHERS: what is left of its rules is keyed on it,
// and they could not be deleted under another.
func TestAMarkIsFoundInAnyMapThatKeysOnOne(t *testing.T) {
	t.Parallel()

	element := func(chain string) string {
		return `{"nftables":[{"map":{"elem":[[` + strconv.FormatUint(0xb1e70007, 10) + `,{"jump":{"target":"` + chain + guestKey + `"}}]]}}]}`
	}

	for name, stage := range map[string]func(*fakeNft){
		"owners":  func(f *fakeNft) { f.owners = element("own_") },
		"replies": func(f *fakeNft) { f.replies = element("reply_") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFakeNft()
			f.denyHeld = true
			f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`
			stage(f)

			h := aHost(t, f)
			keepFile(t, h, aGuest(), listed(t, "example.com"))
			h.Prepare(t.Context())
			h.Reconcile(t.Context(), map[string]bool{guestKey: true})

			if guest := h.live[guestKey]; guest == nil || guest.mark != 0xb1e70007 {
				t.Fatalf("guest = %+v, want the mark that element holds", guest)
			}
		})
	}
}

// A GUEST THAT HAS GONE WITH NO MARK ANYONE CAN SAY HAS NO CONNECTIONS TO DELETE, and zero is not a
// way of asking for them: it is every connection nobody marked.
func TestRemovingAGuestWithNoMarkForgetsNothing(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	keepFile(t, h, aGuest(), Observe())
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{})

	scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.forgotten) != 0 {
		t.Fatalf("forgotten = %v", f.forgotten)
	}
}

// A HOST THAT IS NOT READY HAS NOT REBUILT ITS BASE CHAINS, so an adoption there does: a guest's rules
// are nothing if nothing dispatches to them.
func TestAnAdoptionOnAHostThatIsNotReadyPutsBackTheBaseChains(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.guard = `{"nftables":[]}`
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	if err := h.Ready(); err == nil {
		t.Fatal("a host with no guard is ready")
	}

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "add chain bridge billet_egress job_"+guestKey)
	if !strings.Contains(script, "add rule bridge billet_egress prerouting iifname vmap @guests") {
		t.Fatalf("the guest's rules went in with nothing dispatching to them:\n%s", script)
	}
}

// A GUEST NOTHING CAN SERVE AND NO MARK CAN NAME IS CLOSED: its chain marks DNS with a mark no redirect
// now names, and that would reach the host's resolver.
func TestAQuarantineWithNoMarkClosesTheGuestsChain(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "add rule ip billet_egress_nat dns_"+guestKey+" drop")
	if !strings.Contains(script, "add rule bridge billet_egress job_"+guestKey+" drop") {
		t.Fatalf("its chain goes on marking DNS with a mark nothing redirects:\n%s", script)
	}

	if strings.Contains(script, "{ 0x0 ") {
		t.Fatalf("rules keyed on no mark:\n%s", script)
	}
}

// A MARK ANOTHER GUEST HAS BEEN GIVEN IS NOT THE RESTORED GUEST'S: its connections are that guest's,
// and they are not deleted to tidy up after a flush.
func TestARestoreDoesNotForgetAnotherGuestsConnections(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	other := Attachment{Key: "billet-other", Interface: "bt-8", Network: "billet0", Kind: BridgePort}
	if err := h.Attach(t.Context(), other, Observe()); err != nil {
		t.Fatal(err)
	}

	taken := h.live[other.Key].mark

	if err := h.writeFile(aGuest(), listed(t, "example.com"), taken); err != nil {
		t.Fatal(err)
	}

	h.Reconcile(t.Context(), map[string]bool{guestKey: true, other.Key: true})

	if guest := h.live[guestKey]; guest == nil || guest.mark == taken {
		t.Fatalf("guest = %+v, sharing the mark %#x", guest, taken)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if slices.Contains(f.forgotten, taken) {
		t.Fatalf("forgotten = %v: the connections of a guest that holds %#x", f.forgotten, taken)
	}
}

// A QUARANTINE COVERS WHAT THE REDIRECT CANNOT: a connection the guest already holds skips the nat
// chain, so the owner's chain, which sees every packet, is what stops it.
func TestAQuarantineHoldsWithoutTheTrackerDeleting(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.forget = func(uint32) error { return errors.New("the tracker refused") }
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "add rule ip billet_egress_nat own_"+guestKey+" drop")
	for _, want := range []string{
		"add rule ip billet_egress_nat dns_" + guestKey + " drop", "add element ip billet_egress_nat jobs { 0xb1e70009 : jump dns_" + guestKey + " }",
		"add element ip billet_egress_nat owners { 0xb1e70009 : jump own_" + guestKey + " }", "add table ip billet_egress_nat",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the quarantine has no %q:\n%s", want, script)
		}
	}
}

// A GUEST NOBODY CAN SERVE WHOSE RULES THEN GO IS CLOSED OFF, not left open: its list cannot be read,
// so it cannot have it back.
func TestAGuestWithAnUnreadableListIsClosedOffWhenItsRulesGo(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	f.mu.Lock()
	f.tableListing = func(string) string { return `{"nftables":[]}` }
	f.mu.Unlock()

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "add rule bridge billet_egress job_"+guestKey+" drop")
	for _, want := range []string{
		"add rule ip billet_egress_nat dns_" + guestKey + " drop", "add rule ip billet_egress_nat own_" + guestKey + " drop",
		`add element inet billet_egress_routed bridge_ports { "` + guestTap + `" }`, "add table bridge billet_egress",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("closed off without %q:\n%s", want, script)
		}
	}
}

// A CHAIN THAT IS REACHED IS NOT A GUEST KEPT OFF: with the nat table gone its DNS reaches dnsmasq through
// a job chain that is whole. RULES THAT HOLD ARE LEFT ALONE.
func TestAGuestKeptOffIsCheckedInEveryPart(t *testing.T) {
	t.Parallel()

	const mark = uint32(0xb1e70009)

	read := func(call, without string) ruleTable {
		entries := slices.DeleteFunc(enforcingEntries(call, mark), func(e string) bool {
			return without != "" && strings.Contains(e, without)
		})

		table, err := parseRuleTable([]byte(`{"nftables":[` + strings.Join(entries, ",") + `]}`))
		if err != nil {
			t.Fatal(err)
		}

		return table
	}

	holds := func(without string) bool {
		held := heldRules{bridge: read("bridge billet_egress", without), routed: read("inet billet_egress_routed", without),
			nat: read("ip billet_egress_nat", without)}

		return held.keepsOff(guestKey, guestTap, BridgePort, mark)
	}

	if !holds("") {
		t.Fatal("whole rules read as not keeping it off")
	}

	for _, part := range []string{
		`"name":"jobs"`, `"name":"owners"`, `"chain":"dns_` + guestKey, `"chain":"own_` + guestKey, `"name":"bridge_ports"`,
		`"chain":"port_l3"`, `"name":"guests"`, `"chain":"job_` + guestKey, `"chain":"deny_` + guestKey,
	} {
		if holds(part) {
			t.Errorf("rules without %s read as keeping it off", part)
		}
	}
}

// A FILE THAT CANNOT BE READ AND RULES WITH NO DENY CHAIN COULD BE AN OPEN GUEST OR A LIST THAT LOST
// ITS RULES: removed as an open guest's, the file would go with them, and the list for good.
func TestAnUnreadableFileBehindNoDenyChainIsKept(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	path := filepath.Join(h.stateDir, guestKey+recordSuffix)
	keepFile(t, h, aGuest(), listed(t, "example.com"))

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file went: %v", err)
	}

	for _, script := range f.allScripts() {
		if strings.Contains(script, "delete chain") {
			t.Fatalf("rules that may be a list's were removed:\n%s", script)
		}
	}

	// ITS REDIRECT NAMES A PORT NOBODY HOLDS, WHICHEVER IT TURNS OUT TO BE.
	scriptWith(t, f, "add rule ip billet_egress_nat dns_"+guestKey+" drop")

	// STILL ON THE BOOKS, SO ITS RULES COME OFF WHEN IT GOES: left, they refuse the next guest on its tap.
	h.Detach(t.Context(), guestKey)

	scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)

	// AND ITS REPORT SAYS WHAT COULD NOT BE TOLD: nothing at all would read as nothing asked of it.
	if rec, ok := h.Take(guestKey); !ok || !rec.Listed || !rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	if _, held := h.byIface[guestTap]; held {
		t.Fatal("its interface is still held")
	}

	if _, held := h.slots[0xb1e70009]; held {
		t.Fatal("its mark is still held")
	}
}

// RULES NO INTERFACE DISPATCHES TO STILL HOLD THEIR CHAINS AND THE NEXT GUEST'S ELEMENTS: when their guest
// has gone they come off, under the mark the kernel still has for them.
func TestAGuestThatHasGoneLeavesNoOrphanedRules(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.jobs = `{"nftables":[{"map":{"elem":[[` + strconv.FormatUint(0xb1e70009, 10) + `,{"jump":{"target":"dns_` + guestKey + `"}}]]}}]}`
	f.denyHeld = true

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{})

	script := scriptWith(t, f, "delete chain ip billet_egress_nat dns_"+guestKey)
	if !strings.Contains(script, "delete element ip billet_egress_nat jobs { 0xb1e70009 }") {
		t.Fatalf("what is keyed on its mark stays, and would hold its chain:\n%s", script)
	}

	if _, err := h.readFile(guestKey); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("its file is still there: %v", err)
	}

	if _, held := h.slots[0xb1e70009]; held {
		t.Fatal("its mark is still held")
	}
}

// WHAT IS LEFT OF A GUEST THAT HAS GONE COMES OFF AS ITS FILE SAYS IT WAS: a removal that stopped
// halfway took the deny chain the kernel would have answered from, and sets left behind can be found
// by nothing later. THE FILE GOES ONLY ONCE THEY HAVE: while it cannot be read, what the kernel holds
// stays named, and is asked about again.
func TestLeftoversComeOffAsTheirFileSaysTheyWere(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.jobs = `{"nftables":[{"map":{"elem":[[` + strconv.FormatUint(0xb1e70009, 10) + `,{"jump":{"target":"dns_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	path := filepath.Join(h.stateDir, guestKey+recordSuffix)
	keepFile(t, h, aGuest(), listed(t, "example.com"))

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file went with what it names still there: %v", err)
	}

	for _, script := range f.allScripts() {
		if strings.Contains(script, "delete ") {
			t.Fatalf("removed with nothing to say what they were:\n%s", script)
		}
	}

	if _, held := h.orphanMarks[guestKey]; !held {
		t.Fatal("what the kernel still holds was forgotten")
	}

	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}

	h.Reconcile(t.Context(), map[string]bool{})

	scriptWith(t, f, "delete set bridge billet_egress allow_"+guestKey)

	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the file stayed: %v", err)
	}
}

// A GUEST PROVED GONE IS OWED ITS RULES' REMOVAL, WHATEVER AN OLDER LIST SAYS: a sweep's inventory can
// predate the destroy, and adopting from it starts a resolver and puts rules back for compute that has
// gone, which nothing would then take off.
func TestAnOlderInventoryDoesNotBringBackAGuestProvedGone(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "delete ") {
			return errors.New("Operation not permitted")
		}

		return nil
	}
	f.mu.Unlock()

	h.Detach(t.Context(), guestKey)

	if h.foundOnStart[guestKey] == nil {
		t.Fatal("rules that would not come off were forgotten")
	}

	f.mu.Lock()
	f.refuse = nil
	f.mu.Unlock()

	before := len(f.allScripts())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if h.live[guestKey] != nil {
		t.Fatal("a guest proved gone was adopted from an inventory older than that proof")
	}

	got := f.allScripts()[before:]
	if len(got) == 0 || !strings.Contains(got[0], "delete chain bridge billet_egress job_"+guestKey) {
		t.Fatalf("scripts = %q, want its rules off", got)
	}
}

// A DETACH ASKED AGAIN KEEPS THE REPORT THE FIRST KEPT, since a teardown that failed further on is
// retried, and holds nothing for the guest after the first: a mark freed twice can be the next guest's.
func TestADetachAskedAgainChangesNothing(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	if _, held := h.unrestored[guestKey]; !held {
		t.Fatal("no mark was held for it, so this proves nothing")
	}

	h.Detach(t.Context(), guestKey)

	if _, held := h.unrestored[guestKey]; held {
		t.Fatal("what was held for it stayed")
	}

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || rec.DeniedPackets != 3 {
		t.Fatalf("record = %+v, %v: the second detach replaced what the kernel counted", rec, ok)
	}
}

// A REPLACEMENT MARK IS NOT THE GUEST'S UNTIL ITS RULES COMMIT: what is left of them marks DNS with
// another, and a quarantine under the new one would stop nothing, while the log said it had.
func TestAFailedAdoptionUnderANewMarkClosesTheGuest(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())
	h.locals = func() ([]netip.Addr, error) { return nil, errors.New("no addresses") }
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "add rule ip billet_egress_nat dns_"+guestKey+" drop")
	if !strings.Contains(script, "add rule bridge billet_egress job_"+guestKey+" drop") {
		t.Fatalf("its chain goes on marking DNS with a mark nothing redirects:\n%s", script)
	}
}

// AN ADOPTION KEEPS THE MARK IT INSTALLS, before it installs it: read back after the next flush, the file
// is what names this guest's connections.
func TestAnAdoptionKeepsItsMarkInTheFile(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.denyHeld = true
	f.guests = `{"nftables":[{"map":{"elem":[["` + guestTap + `",{"jump":{"target":"job_` + guestKey + `"}}]]}}]}`

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	guest := h.live[guestKey]
	if guest == nil {
		t.Fatal("not adopted")
	}

	file, err := h.readFile(guestKey)
	if err != nil {
		t.Fatal(err)
	}

	if file.Mark != guest.mark {
		t.Fatalf("file says %#x, its rules %#x", file.Mark, guest.mark)
	}
}

// A MARK NOTHING CAN SAY NAMES NO ELEMENT: nothing is keyed on zero, and asking for it would fail the
// removal's transaction every time.
func TestRemovingAGuestWithNoMarkNamesNoMark(t *testing.T) {
	t.Parallel()

	if s := detachScript(guestKey, guestTap, BridgePort, 0, true); strings.Contains(s, "0x0") {
		t.Fatalf("a removal names no mark:\n%s", s)
	}
}

// A REPORT SAYS A JOB RAN OPEN AS SOON AS THAT IS FOUND, whether or not the rules could be put back.
func TestARepairThatFailsStillSaysTheJobRanOpen(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.tableListing = func(string) string { return `{"nftables":[]}` }
	f.refuse = func(script string, _ []string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Operation not permitted")
		}

		return nil
	}
	f.mu.Unlock()

	h.verify(t.Context())

	f.mu.Lock()
	f.refuse = nil
	f.mu.Unlock()

	h.Detach(t.Context(), guestKey)

	if rec, ok := h.Take(guestKey); !ok || !rec.Incomplete {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

// RECOVERY RUNS AT EVERY REGISTRATION, AND A GUEST IS ADOPTED ONCE: a second resolver would leak
// the first and race it for the guest's queries.
func TestAGuestIsAdoptedOnce(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})
	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	flushes := 0

	for _, s := range f.allScripts() {
		if strings.Contains(s, "flush chain ip billet_egress_nat dns_"+guestKey) {
			flushes++
		}
	}

	if flushes != 1 {
		t.Fatalf("adopted %d times", flushes)
	}
}

// A MARK AN EARLIER PROCESS LEFT IS NOT HANDED OUT AGAIN: two guests on one mark would each be
// sent the other's DNS.
func TestAMarkInUseIsNotReused(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, markBase|1)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	if err := h.Attach(t.Context(), Attachment{Key: "billet-next", Interface: "bt-8", Network: "billet0",
		Kind: BridgePort}, Observe()); err != nil {
		t.Fatal(err)
	}

	if got := h.live["billet-next"].mark; got == markBase|1 {
		t.Fatalf("handed out %#x, which an earlier process's guest holds", got)
	}

	// AND ONE THAT HAS GONE COMES BACK.
	h.Detach(t.Context(), "billet-next")

	if len(h.slots) != 1 {
		t.Fatalf("marks held = %v, want only the earlier guest's", h.slots)
	}
}

// A LAUNCH THAT REUSED A GUEST'S INTERFACE WHILE IT WAS BEING JUDGED OWNS THE NAME: a recovery that
// had read the old guest does not write it back over the new one.
func TestAJudgementDoesNotOverwriteALaunch(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	stale := h.foundOnStart[guestKey]

	next := Attachment{Key: "billet-next", Interface: guestTap, Network: "billet0", Kind: BridgePort}
	if err := h.Attach(t.Context(), next, Observe()); err != nil {
		t.Fatal(err)
	}

	h.reconcileOne(t.Context(), stale, false)

	if h.byIface[guestTap] != next.Key {
		t.Fatalf("the name is held by %q", h.byIface[guestTap])
	}

	if h.live[next.Key] == nil {
		t.Fatal("the launch's guest left the books")
	}
}

// THE ROLE'S GUARD IS WHAT LETS A REDIRECTED QUERY REACH ITS RESOLVER, and the check finds it by a
// comment: so the comment is pinned to the role's file, with the rule's match, above the drop.
func TestTheRolesGuardCarriesTheRuleThisPackageNeeds(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host",
		"templates", "network.nft.j2"))
	if err != nil {
		t.Fatal(err)
	}

	guard := string(raw)

	rule := strings.Index(guard, "ct status dnat ct original proto-dst 53 accept comment \""+GuardRuleComment+"\"")
	drop := strings.Index(guard, `counter drop comment "billet guest -> host"`)

	if rule < 0 || drop < 0 || rule > drop {
		t.Fatalf("the rule is at %d and the drop at %d in:\n%s", rule, drop, guard)
	}
}

func TestHandOverGivesBackOnlyOpenGuests(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), Observe()); err != nil {
		t.Fatal(err)
	}

	closedGuest := Attachment{Key: "billet-fedcba9876543210", Interface: "bt-8", Network: "billet0", Kind: BridgePort}
	if err := h.Attach(t.Context(), closedGuest, listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	h.HandOver(t.Context())

	scriptWith(t, f, "delete chain bridge billet_egress job_"+guestKey)

	for _, s := range f.allScripts() {
		if strings.Contains(s, "delete chain bridge billet_egress job_"+closedGuest.Key) {
			t.Fatalf("a listed guest was opened: %s", s)
		}
	}

	if _, err := h.readFile(closedGuest.Key); err != nil {
		t.Fatalf("a listed guest left running lost its file: %v", err)
	}
}

func TestKeptRecordsAreBounded(t *testing.T) {
	t.Parallel()

	h := aHost(t, newFakeNft())

	for i := range maxKeptRecords + 3 {
		h.keep(fmt.Sprintf("guest-%d", i), Record{})
	}

	if len(h.done) != maxKeptRecords {
		t.Fatalf("kept %d", len(h.done))
	}

	if _, ok := h.Take("guest-0"); ok {
		t.Fatal("the oldest was kept")
	}

	if _, ok := h.Take(fmt.Sprintf("guest-%d", maxKeptRecords+2)); !ok {
		t.Fatal("the newest was not kept")
	}
}

// AN ADDRESS ALREADY ADMITTED RUNS NOTHING, which is nearly every answer: each exec is
// a few milliseconds on the guest's DNS.
func TestAnAddressAlreadyAdmittedRunsNothing(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	guest := h.live[guestKey]

	if err := h.admit(t.Context(), guest, []netip.Addr{anAddr, anotherAddr, anAddr}); err != nil {
		t.Fatal(err)
	}

	script := scriptWith(t, f, "add element bridge billet_egress allow_"+guestKey)
	if !strings.Contains(script, "{ 192.0.2.10, 192.0.2.11 }") {
		t.Fatalf("script = %s", script)
	}

	before := len(f.allScripts())

	if err := h.admit(t.Context(), guest, []netip.Addr{anotherAddr, anAddr}); err != nil {
		t.Fatal(err)
	}

	if err := h.admit(t.Context(), guest, nil); err != nil {
		t.Fatal(err)
	}

	if after := len(f.allScripts()); after != before {
		t.Fatalf("ran %d more scripts for addresses already in the set", after-before)
	}
}

func TestAFullSetIsAnErrorNotASilentlyRefusedAddress(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	guest := h.live[guestKey]
	for i := range maxAllowed {
		guest.admitted[netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)})] = struct{}{}
	}

	err := h.admit(t.Context(), guest, []netip.Addr{anAddr})
	if err == nil || !strings.Contains(err.Error(), "used up") {
		t.Fatalf("admit = %v", err)
	}
}

// THE WHOLE PATH: a query for a listed name through a real socket puts its address in
// the kernel's set before the guest hears, and the record comes out at the end.
func TestAQueryThroughAHostReachesTheSetAndTheRecord(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	r := startResolver(t)

	h := aHost(t, f)
	h.upstreamPort = r.port
	h.Prepare(t.Context())

	attachCtx, cancelAttach := context.WithCancel(t.Context())
	defer cancelAttach()

	if err := h.Attach(attachCtx, aGuest(), listed(t, "allowed.example")); err != nil {
		t.Fatal(err)
	}

	guest := h.live[guestKey]

	resp := askUDP(t, guest.fwd.port, aQuery(t, "allowed.example.", dnsmessage.TypeA))
	if got := rcode(t, resp); got != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", got)
	}

	// A RESOLVER OUTLIVES THE LAUNCH THAT ATTACHED IT: its context is long over by now.
	cancelAttach()

	if got := rcode(t, askUDP(t, guest.fwd.port, aQuery(t, "allowed.example.", dnsmessage.TypeA))); got != dnsmessage.RCodeSuccess {
		t.Fatalf("after its launch's context ended the resolver answered %v", got)
	}

	// THE SET WAS FILLED BY THE TIME THE ANSWER ARRIVED, SO THIS NEEDS NO WAIT.
	scriptWith(t, f, "add element bridge billet_egress allow_"+guestKey+" { 192.0.2.10 }")

	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || !slices.Equal(rec.Names[anAddr], []string{"allowed.example"}) || rec.Queries != 2 {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

func TestParsingTheKernelsListings(t *testing.T) {
	t.Parallel()

	// AS nft 1.0.9 WROTE THEM, 2026-11-04.
	const guests = `{"nftables": [{"metainfo": {"version": "1.0.9", "json_schema_version": 1}}, {"map": {` +
		`"family": "bridge", "name": "guests", "table": "billet_egress", "type": "ifname", "handle": 1, ` +
		`"map": "verdict", "elem": [["fc-tap-0", {"jump": {"target": "job_billet-0"}}]]}}]}`

	got, err := parseVerdictMap([]byte(guests))
	if err != nil || len(got) != 1 || string(got[0].key) != `"fc-tap-0"` || got[0].target != "job_billet-0" {
		t.Fatalf("parseVerdictMap = %+v, %v", got, err)
	}

	const denied = `{"nftables": [{"metainfo": {"version": "1.0.9"}}, {"set": {"family": "bridge", "name": "denied_j1",` +
		` "type": ["ipv4_addr", "inet_proto", "inet_service"], "size": 256, "flags": ["dynamic"], "elem": [` +
		`{"elem": {"val": {"concat": ["198.51.100.1", "tcp", 80]}, "counter": {"packets": 2, "bytes": 120}}}, ` +
		`{"elem": {"val": {"concat": ["198.51.100.2", "udp", 53]}, "counter": {"packets": 9, "bytes": 120}}}], ` +
		`"stmt": [{"counter": null}]}}]}`

	set, err := parseDeniedSet([]byte(denied))
	if err != nil {
		t.Fatal(err)
	}

	want := []Denied{
		{Addr: netip.MustParseAddr("198.51.100.2"), Protocol: "udp", Port: 53, Packets: 9},
		{Addr: netip.MustParseAddr("198.51.100.1"), Protocol: "tcp", Port: 80, Packets: 2},
	}
	if !slices.Equal(set, want) {
		t.Fatalf("parseDeniedSet = %+v", set)
	}

	const counter = `{"nftables": [{"counter": {"family": "bridge", "name": "c", "handle": 34, "packets": 7, "bytes": 0}}]}`
	if n, err := parseCounter([]byte(counter)); err != nil || n != 7 {
		t.Fatalf("parseCounter = %d, %v", n, err)
	}

	// WHAT IT CANNOT READ IS AN ERROR, never an empty list or a zero.
	for name, bad := range map[string]func() error{
		"a listing that is not JSON": func() error { _, err := parseDeniedSet([]byte("nope")); return err },
		"a set element of another shape": func() error {
			_, err := parseDeniedSet([]byte(`{"nftables":[{"set":{"elem":[{"elem":{"val":{"concat":["1.2.3.4"]}}}]}}]}`))

			return err
		},
		"a listing of something else": func() error { _, err := parseCounter([]byte(`{"nftables":[{"set":{}}]}`)); return err },
		"a counter with no packets":   func() error { _, err := parseCounter([]byte(`{"nftables":[{"counter":{}}]}`)); return err },
	} {
		if bad() == nil {
			t.Errorf("%s was read", name)
		}
	}
}
