//go:build linux

package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// THE REAL THING, short of a real guest: the kernel's nftables, a bridge, and a guest
// in a network namespace of its own, on each kind of interface.
//
// IT CHANGES THE FIREWALL AND THE DEVICES OF THE NAMESPACE IT RUNS IN, so it runs only
// when asked, and only in one that is thrown away afterwards:
//
//	docker run --rm --privileged -v "$PWD":/src -w /src golang \
//	  sh -c 'apt-get install -y nftables iproute2; BILLET_TEST_EGRESS_NETNS=1 \
//	  go test -run TestRealEgress ./internal/egress'
const realEgressEnv = "BILLET_TEST_EGRESS_NETNS"

var (
	realAllowed  = netip.MustParseAddr("198.51.100.1")
	realHardcode = netip.MustParseAddr("198.51.100.2")
	realPrefix   = netip.MustParseAddr("198.51.100.200")
	realOutside  = netip.MustParseAddr("8.8.8.8")
)

func requireRealHost(t *testing.T) {
	t.Helper()

	if os.Getenv(realEgressEnv) == "" {
		t.Skipf("set %s=1, in a network namespace that is thrown away, to run this", realEgressEnv)
	}

	if os.Geteuid() != 0 {
		t.Skip("not root: this creates devices, namespaces and firewall tables")
	}

	for _, bin := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed: %v", bin, err)
		}
	}
}

func sh(t *testing.T, args ...string) {
	t.Helper()

	if out, err := exec.CommandContext(context.Background(), args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// inNamespace runs fn on a thread in a named network namespace. The thread is never
// unlocked, so the runtime throws it away rather than reuse it elsewhere; a socket made
// here stays in the namespace wherever it is later read.
func inNamespace(t *testing.T, name string, fn func()) {
	t.Helper()

	failed := make(chan error, 1)

	go func() {
		runtime.LockOSThread()

		ns, err := os.Open(filepath.Join("/var/run/netns", name))
		if err != nil {
			failed <- err

			return
		}
		defer ns.Close()

		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNET); err != nil {
			failed <- err

			return
		}

		fn()
		failed <- nil
	}()

	if err := <-failed; err != nil {
		t.Fatalf("enter namespace %s: %v", name, err)
	}
}

// realNetwork is a guest, an interface billet filters it on, and an internet beyond
// the host with listeners on it.
type realNetwork struct {
	t        *testing.T
	guestNS  string
	netNS    string
	iface    string
	network  string
	gateway  netip.Addr
	upstream *realUpstream
	// other is an address of the host's that is not the guest's gateway, with a
	// service on it the guard admits and a DNS server it would admit too.
	other netip.Addr
}

// guestCachePort is the node's guest cache's port in the role's guard.
const guestCachePort = 7718

// realUpstream answers as the host's dnsmasq would, on the gateway's port 53.
type realUpstream struct {
	mu    sync.Mutex
	asked []string
}

func (u *realUpstream) names() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.asked)
}

func (u *realUpstream) answer(t *testing.T, q []byte) []byte {
	read, ok := readQuestion(q)
	if !ok {
		t.Errorf("a query this package would not forward reached the upstream: %x", q)

		return nil
	}

	u.mu.Lock()
	u.asked = append(u.asked, read.name)
	u.mu.Unlock()

	return anAnswer(t, q, nil, realAllowed)
}

func (u *realUpstream) serve(t *testing.T, gateway netip.Addr) {
	t.Helper()

	addr := netip.AddrPortFrom(gateway, upstreamDNSPort)

	udp, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(addr))
	if err != nil {
		t.Fatalf("listen as the upstream: %v", err)
	}

	tcp, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(addr))
	if err != nil {
		t.Fatalf("listen as the upstream: %v", err)
	}

	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
	})

	go func() {
		for {
			buf := make([]byte, maxResponse)

			n, from, err := udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			_, _ = udp.WriteToUDPAddrPort(u.answer(t, buf[:n]), from)
		}
	}()

	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}

			if q, err := readFrame(conn, maxQuery); err == nil {
				_ = writeFrame(conn, u.answer(t, q))
			}

			_ = conn.Close()
		}
	}()
}

// newRealNetwork builds it, as kind, under names no two kinds share.
func newRealNetwork(t *testing.T, kind Kind) *realNetwork {
	t.Helper()
	requireRealHost(t)

	tag := "b"
	subnet := "172.31.91"

	if kind == Routed {
		tag, subnet = "r", "172.31.92"
	}

	n := &realNetwork{
		t: t, guestNS: "egress-guest-" + tag, netNS: "egress-net-" + tag, iface: "egresstap" + tag,
		network: "egressbr" + tag, gateway: netip.MustParseAddr(subnet + ".1"), upstream: &realUpstream{},
	}

	uplink, wan := "egressup"+tag, "egresswan"+tag

	t.Cleanup(func() {
		for _, args := range [][]string{
			{"ip", "netns", "del", n.guestNS}, {"ip", "netns", "del", n.netNS},
			{"ip", "link", "del", n.network}, {"ip", "link", "del", n.iface}, {"ip", "link", "del", uplink},
		} {
			_ = exec.CommandContext(context.Background(), args[0], args[1:]...).Run()
		}
	})

	sh(t, "ip", "netns", "add", n.guestNS)
	sh(t, "ip", "netns", "add", n.netNS)
	sh(t, "ip", "link", "add", n.iface, "type", "veth", "peer", "name", "eth0", "netns", n.guestNS)
	sh(t, "ip", "link", "set", n.iface, "up")

	if kind == BridgePort {
		sh(t, "ip", "link", "add", n.network, "type", "bridge")
		sh(t, "ip", "addr", "add", n.gateway.String()+"/24", "dev", n.network)
		sh(t, "ip", "link", "set", n.network, "up")
		sh(t, "ip", "link", "set", n.iface, "master", n.network)
	} else {
		n.network = n.iface
		sh(t, "ip", "addr", "add", n.gateway.String()+"/24", "dev", n.iface)
	}

	sh(t, "ip", "-n", n.guestNS, "addr", "add", subnet+".2/24", "dev", "eth0")
	sh(t, "ip", "-n", n.guestNS, "link", "set", "eth0", "up")
	sh(t, "ip", "-n", n.guestNS, "link", "set", "lo", "up")
	sh(t, "ip", "-n", n.guestNS, "route", "add", "default", "via", n.gateway.String())

	// THE INTERNET: a namespace beyond the host, routed to, whose addresses are not the
	// host's own, so an allowed packet is forwarded as a guest's is and not delivered
	// locally where the guard would judge it.
	wide := map[Kind]string{BridgePort: "192.0.2", Routed: "192.0.3"}[kind]

	sh(t, "ip", "link", "add", uplink, "type", "veth", "peer", "name", wan, "netns", n.netNS)
	sh(t, "ip", "addr", "add", wide+".1/24", "dev", uplink)
	sh(t, "ip", "link", "set", uplink, "up")
	sh(t, "ip", "-n", n.netNS, "addr", "add", wide+".2/24", "dev", wan)
	sh(t, "ip", "-n", n.netNS, "link", "set", wan, "up")
	sh(t, "ip", "-n", n.netNS, "link", "set", "lo", "up")
	sh(t, "ip", "-n", n.netNS, "route", "add", "default", "via", wide+".1")

	for _, a := range []netip.Addr{realAllowed, realHardcode, realPrefix, realOutside} {
		sh(t, "ip", "-n", n.netNS, "addr", "add", a.String()+"/32", "dev", "lo")
		_ = exec.CommandContext(context.Background(), "ip", "route", "del", a.String()+"/32").Run()
		sh(t, "ip", "route", "add", a.String()+"/32", "via", wide+".2")
	}

	sh(t, "sysctl", "-qw", "net.ipv4.ip_forward=1")

	// THE ROLE'S GUARD, the rule the node needs and the drop it has to get past.
	script := fmt.Sprintf(`add table inet billet_guard
add chain inet billet_guard input { type filter hook input priority filter; policy accept; }
add rule inet billet_guard input iifname %q tcp dport { 53, 7718 } accept
add rule inet billet_guard input iifname %q udp dport { 53, 67 } accept
add rule inet billet_guard input iifname %q ct status dnat ct original proto-dst 53 accept comment %q
add rule inet billet_guard input iifname %q counter drop
`, n.network, n.network, n.network, GuardRuleComment, n.network)

	cmd := exec.CommandContext(context.Background(), "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install the guard: %v: %s", err, out)
	}

	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "nft", "delete", "table", "inet", "billet_guard").Run()
	})

	n.other = netip.MustParseAddr(wide + ".1")
	n.listen(n.other, guestCachePort)
	n.listen(n.other, upstreamDNSPort)

	inNamespace(t, n.netNS, func() {
		for _, a := range []netip.Addr{realAllowed, realHardcode, realPrefix} {
			n.listen(a, 443)
		}

		n.listen(realOutside, 53)
	})

	n.upstream.serve(t, n.gateway)

	return n
}

func (n *realNetwork) listen(addr netip.Addr, port uint16) {
	l, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(addr, port)))
	if err != nil {
		n.t.Errorf("listen on %s: %v", addr, err)

		return
	}

	n.t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()
}

// connect says what a guest's connect came to, and how long it took.
func (n *realNetwork) connect(addr netip.Addr, port uint16) (time.Duration, error) {
	var (
		took time.Duration
		err  error
	)

	inNamespace(n.t, n.guestNS, func() {
		started := time.Now()

		var conn net.Conn

		conn, err = (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp4", netip.AddrPortFrom(addr, port).String())
		took = time.Since(started)

		if err == nil {
			_ = conn.Close()
		}
	})

	return took, err
}

// resolve asks name of a resolver from the guest, answering the rcode and the error.
func (n *realNetwork) resolve(network string, resolver netip.Addr, name string) (dnsmessage.RCode, error) {
	q := aQuery(n.t, name, dnsmessage.TypeA)

	var (
		resp []byte
		err  error
	)

	inNamespace(n.t, n.guestNS, func() {
		var conn net.Conn

		conn, err = (&net.Dialer{Timeout: 5 * time.Second}).Dial(network,
			netip.AddrPortFrom(resolver, upstreamDNSPort).String())
		if err != nil {
			return
		}
		defer conn.Close()

		if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}

		if network == "tcp4" {
			if err = writeFrame(conn, q); err != nil {
				return
			}

			resp, err = readFrame(conn, maxResponse)

			return
		}

		if _, err = conn.Write(q); err != nil {
			return
		}

		buf := make([]byte, maxResponse)

		var read int

		read, err = conn.Read(buf)
		resp = buf[:read]
	})

	if err != nil {
		return 0, err
	}

	var p dnsmessage.Parser

	header, err := p.Start(resp)
	if err != nil {
		return 0, err
	}

	return header.RCode, nil
}

func (n *realNetwork) mustResolve(network, name string, want dnsmessage.RCode) {
	n.t.Helper()

	got, err := n.resolve(network, n.gateway, name)
	if err != nil {
		n.t.Fatalf("%s %s: %v", network, name, err)
	}

	if got != want {
		n.t.Fatalf("%s %s = %v, want %v", network, name, got, want)
	}
}

func (n *realNetwork) mustConnect(addr netip.Addr, port uint16) {
	n.t.Helper()

	if _, err := n.connect(addr, port); err != nil {
		n.t.Fatalf("connect to %s:%d: %v", addr, port, err)
	}
}

// mustBeRefused holds a connect to being refused, and at once: a drop would look the
// same to a test that only asked whether it failed, and a job would sit in its timeout.
func (n *realNetwork) mustBeRefused(addr netip.Addr, port uint16) {
	n.t.Helper()

	took, err := n.connect(addr, port)
	if err == nil {
		n.t.Fatalf("connected to %s:%d", addr, port)
	}

	if !errors.Is(err, unix.ECONNREFUSED) {
		n.t.Fatalf("connect to %s:%d = %v, want it refused", addr, port, err)
	}

	if took > time.Second {
		n.t.Fatalf("connect to %s:%d took %s to be refused: TCP's retransmit is a second", addr, port, took)
	}
}

func realHost(t *testing.T, n *realNetwork, dir string) *Host {
	t.Helper()

	h := NewHost(dir, slog.Default())
	t.Cleanup(h.Close)

	h.Prepare(t.Context())

	if err := h.Ready(); err != nil {
		t.Fatalf("this host is not ready: %v", err)
	}

	return h
}

func dropTables(t *testing.T) {
	t.Helper()

	cleanup := func() {
		for _, table := range []string{"bridge billet_egress", "inet billet_egress_routed", "ip billet_egress_nat"} {
			args := append([]string{"delete", "table"}, strings.Fields(table)...)
			_ = exec.CommandContext(context.Background(), "nft", args...).Run()
		}
	}

	cleanup()
	t.Cleanup(cleanup)
}

func realAttachment(n *realNetwork, kind Kind) Attachment {
	return Attachment{Key: "billet-real-" + string(kind), Interface: n.iface, Network: n.network, Kind: kind}
}

// TestRealEgress is the four things #540 says a list must do, on each kind of
// interface, with the controls that make them mean something: every refused
// destination is reachable before and after, so it is the rules that refused it.
func TestRealEgress(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	for _, kind := range []Kind{BridgePort, Routed} {
		t.Run(string(kind), func(t *testing.T) {
			dropTables(t)

			n := newRealNetwork(t, kind)

			// THE CONTROLS: all of it answers before there is a list.
			n.mustConnect(realHardcode, 443)
			n.mustConnect(realOutside, 53)

			h := realHost(t, n, t.TempDir())
			a := realAttachment(n, kind)

			p, err := Compile([]string{"allowed.test", realPrefix.String() + "/32"})
			if err != nil {
				t.Fatal(err)
			}

			if err := h.Attach(t.Context(), a, p); err != nil {
				t.Fatalf("Attach: %v", err)
			}

			// AN ADDRESS NOT YET ANSWERED IS REFUSED, and a name admits it, over both
			// protocols, with no wait between the answer and the connect.
			n.mustBeRefused(realAllowed, 443)
			n.mustResolve("udp4", "allowed.test.", dnsmessage.RCodeSuccess)
			n.mustConnect(realAllowed, 443)
			n.mustResolve("tcp4", "allowed.test.", dnsmessage.RCodeSuccess)

			// A NAME OFF THE LIST IS REFUSED, AND THE UPSTREAM NEVER HEARS IT.
			n.mustResolve("udp4", "other.test.", dnsmessage.RCodeRefused)
			n.mustResolve("tcp4", "other.test.", dnsmessage.RCodeRefused)

			if slices.Contains(n.upstream.names(), "other.test") {
				t.Fatalf("the upstream heard an off-list name: %q", n.upstream.names())
			}

			// A HARD-CODED ADDRESS IS REFUSED; A LISTED PREFIX IS NOT.
			n.mustBeRefused(realHardcode, 443)
			n.mustConnect(realPrefix, 443)

			// DNS TO ANY OTHER RESOLVER IS REFUSED, BOTH WAYS.
			n.mustBeRefused(realOutside, 53)

			if _, err := n.resolve("udp4", realOutside, "allowed.test."); err == nil {
				t.Fatal("a resolver other than the node's answered over UDP")
			}

			// THE HOST'S OTHER ADDRESSES ARE THE GUARD'S TO JUDGE: A SERVICE IT ADMITS IS
			// REACHED, AS A GUEST ON THE OTHER BRIDGE REACHES THE NODE'S CACHE, AND THE
			// HOST'S OTHER DNS IS NOT A WAY AROUND THE RESOLVER.
			n.mustConnect(n.other, guestCachePort)
			n.mustBeRefused(n.other, upstreamDNSPort)

			// IPv6 IS REFUSED AND COUNTED, NEVER UNWATCHED.
			sh(t, "ip", "-n", n.guestNS, "addr", "add", "fd00:540::2/64", "dev", "eth0", "nodad")
			sh(t, "ip", "-n", n.guestNS, "route", "add", "default", "via", "fd00:540::1", "dev", "eth0")

			if _, err := n.connect6(); err == nil {
				t.Fatal("an IPv6 connect succeeded")
			}

			h.Detach(t.Context(), a.Key)

			rec, ok := h.Take(a.Key)
			if !ok {
				t.Fatal("no record")
			}

			if !rec.Listed || rec.Incomplete {
				t.Errorf("record = %+v", rec)
			}

			if !slices.Equal(rec.Names[realAllowed], []string{"allowed.test"}) {
				t.Errorf("names = %+v", rec.Names)
			}

			if rec.RefusedQueries != 2 || len(rec.Refused) != 1 || rec.Refused[0].Name != "other.test" {
				t.Errorf("refused %d: %+v", rec.RefusedQueries, rec.Refused)
			}

			if rec.OutsideResolverPackets == 0 || rec.DeniedPackets < rec.OutsideResolverPackets {
				t.Errorf("denied %d, outside %d", rec.DeniedPackets, rec.OutsideResolverPackets)
			}

			if rec.IPv6Packets == 0 {
				t.Error("no IPv6 packet was counted")
			}

			wantDenied := []Denied{
				{Addr: realHardcode, Protocol: "tcp", Port: 443},
				{Addr: n.other, Protocol: "tcp", Port: upstreamDNSPort},
				{Addr: realOutside, Protocol: "tcp", Port: 53},
				{Addr: realOutside, Protocol: "udp", Port: 53},
			}
			for _, want := range wantDenied {
				if !slices.ContainsFunc(rec.Denied, func(d Denied) bool {
					return d.Addr == want.Addr && d.Protocol == want.Protocol && d.Port == want.Port && d.Packets > 0
				}) {
					t.Errorf("denied %+v does not hold %+v", rec.Denied, want)
				}
			}

			// AND THE RULES WERE THE REASON: gone, it all answers again.
			n.mustConnect(realHardcode, 443)
			n.mustConnect(realOutside, 53)
		})
	}
}

// connect6 tries an IPv6 connect from the guest.
func (n *realNetwork) connect6() (time.Duration, error) {
	var (
		took time.Duration
		err  error
	)

	inNamespace(n.t, n.guestNS, func() {
		started := time.Now()

		var conn net.Conn

		conn, err = (&net.Dialer{Timeout: time.Second}).Dial("tcp6", "[fd00:540::1]:443")
		took = time.Since(started)

		if err == nil {
			_ = conn.Close()
		}
	})

	return took, err
}

// A PROCESS THAT GOES LEAVES A LISTED GUEST CLOSED, and the next one's resolver picks
// up the same rules and what they already admitted. Between the two the guest resolves
// nothing, and an address nothing admitted stays refused.
func TestRealEgressSurvivesTheNodeProcess(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	dir := t.TempDir()
	h := realHost(t, n, dir)
	a := realAttachment(n, BridgePort)

	p, err := Compile([]string{"allowed.test"})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Attach(t.Context(), a, p); err != nil {
		t.Fatal(err)
	}

	n.mustResolve("udp4", "allowed.test.", dnsmessage.RCodeSuccess)
	n.mustBeRefused(realHardcode, 443)

	h.HandOver(t.Context())
	h.Close()

	// NO PROCESS: CLOSED, THE RESOLVER GONE, AND WHAT WAS ADMITTED STILL ADMITTED.
	if _, err := n.resolve("udp4", n.gateway, "allowed.test."); err == nil {
		t.Fatal("a listed guest resolved with no resolver to ask")
	}

	n.mustBeRefused(realHardcode, 443)
	n.mustConnect(realAllowed, 443)

	next := realHost(t, n, dir)
	next.Reconcile(t.Context(), map[string]bool{a.Key: true})

	n.mustResolve("udp4", "allowed.test.", dnsmessage.RCodeSuccess)
	n.mustResolve("udp4", "other.test.", dnsmessage.RCodeRefused)
	n.mustBeRefused(realHardcode, 443)

	next.Detach(t.Context(), a.Key)

	rec, ok := next.Take(a.Key)
	if !ok || !rec.Incomplete {
		t.Fatalf("record = %+v, %v: a resolver that did not watch from the start says so", rec, ok)
	}

	if rec.DeniedPackets == 0 {
		t.Fatal("the kernel's counts did not survive the process")
	}
}

// AN OPEN GUEST IS WATCHED AND NEVER STOPPED, AND A STOP HANDS IT BACK: its DNS answers
// from the host's resolver once the node's is gone.
func TestRealEgressWatchesAnOpenGuestAndHandsItBack(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	h := realHost(t, n, t.TempDir())
	a := realAttachment(n, BridgePort)

	if err := h.Attach(t.Context(), a, Observe()); err != nil {
		t.Fatal(err)
	}

	n.mustResolve("udp4", "anything.test.", dnsmessage.RCodeSuccess)
	n.mustResolve("tcp4", "anything.test.", dnsmessage.RCodeSuccess)
	n.mustConnect(realHardcode, 443)
	n.mustConnect(realOutside, 53)

	h.Detach(t.Context(), a.Key)

	rec, ok := h.Take(a.Key)
	if !ok || rec.Listed || rec.Queries != 2 || !slices.Equal(rec.Names[realAllowed], []string{"anything.test"}) {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	if err := h.Attach(t.Context(), a, Observe()); err != nil {
		t.Fatal(err)
	}

	before := len(n.upstream.names())

	h.HandOver(t.Context())

	n.mustResolve("udp4", "after.test.", dnsmessage.RCodeSuccess)

	if got := n.upstream.names(); len(got) != before+1 {
		t.Fatalf("the host's resolver heard %q", got)
	}
}

// THE LIST STANDS ON ITS OWN WHEN THE NODE'S RULES ARE RENDERED FOR A KERNEL: overlapping
// prefixes, which are an error to nft without auto-merge, and an empty list.
func TestRealEgressTakesOverlappingPrefixes(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	h := realHost(t, n, t.TempDir())
	a := realAttachment(n, BridgePort)

	p, err := Compile([]string{"198.51.100.0/24", "198.51.100.200/32", "198.51.100.128/25"})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Attach(t.Context(), a, p); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	n.mustConnect(realPrefix, 443)
	n.mustConnect(realHardcode, 443)
	n.mustBeRefused(realOutside, 53)

	h.Detach(t.Context(), a.Key)

	empty, err := Compile(nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Attach(t.Context(), a, empty); err != nil {
		t.Fatalf("Attach of an empty list: %v", err)
	}

	n.mustBeRefused(realPrefix, 443)
}
