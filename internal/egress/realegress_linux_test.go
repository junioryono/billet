//go:build linux

package egress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
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

	if out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// takeAway runs a command that takes away what may not be there, so its failing says nothing,
// and with no test's context, since it runs in a cleanup too.
func takeAway(args ...string) {
	_ = exec.CommandContext(context.Background(), args[0], args[1:]...).Run() //nolint:errcheck // what it clears may not be there
}

// inNamespace runs fn on a thread in a named network namespace. The thread is never
// unlocked, so the runtime throws it away rather than reuse it elsewhere; a socket made
// here stays in the namespace wherever it is later read.
func inNamespace(t *testing.T, name string, fn func()) {
	t.Helper()

	failed := make(chan error, 1)

	go func() {
		runtime.LockOSThread()

		ns, err := os.Open("/var/run/netns/" + name)
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
	// outside6 is an address beyond the host that only IPv6 reaches, with a service on it.
	outside6 netip.Addr
	// guestAddr is the guest's own address.
	guestAddr netip.Addr
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
	t.Helper()

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

	var wg sync.WaitGroup

	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()

		wg.Wait()
	})

	wg.Add(2)

	go func() {
		defer wg.Done()

		for {
			buf := make([]byte, maxResponse)

			n, from, err := udp.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			_, _ = udp.WriteToUDPAddrPort(u.answer(t, buf[:n]), from) //nolint:errcheck // a client that left fails for itself
		}
	}()

	go func() {
		defer wg.Done()

		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}

			if q, err := readFrame(conn, maxQuery); err == nil {
				_ = writeFrame(conn, u.answer(t, q)) //nolint:errcheck // a client that left fails for itself
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
	six := "fd00:54b"

	if kind == Routed {
		tag, subnet, six = "r", "172.31.92", "fd00:54c"
	}

	n := &realNetwork{
		t: t, guestNS: "egress-guest-" + tag, netNS: "egress-net-" + tag, iface: "egresstap" + tag,
		network: "egressbr" + tag, gateway: netip.MustParseAddr(subnet + ".1"), upstream: &realUpstream{},
		outside6: netip.MustParseAddr(six + ":2::1"), guestAddr: netip.MustParseAddr(subnet + ".2"),
	}

	uplink, wan := "egressup"+tag, "egresswan"+tag

	// A THROWN-AWAY NAMESPACE STARTS WITH IT DOWN, and a resolver reaches its upstream on the
	// gateway's address over it, so every name would time out as if the rules had dropped it.
	sh(t, "ip", "link", "set", "lo", "up")

	t.Cleanup(func() {
		for _, args := range [][]string{
			{"ip", "netns", "del", n.guestNS}, {"ip", "netns", "del", n.netNS},
			{"ip", "link", "del", n.network}, {"ip", "link", "del", n.iface}, {"ip", "link", "del", uplink},
		} {
			takeAway(args...)
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
		takeAway("ip", "route", "del", a.String()+"/32")
		sh(t, "ip", "route", "add", a.String()+"/32", "via", wide+".2")
	}

	sh(t, "sysctl", "-qw", "net.ipv4.ip_forward=1")

	// AND AN IPv6 PATH TO THE SAME PLACE, or refusing it would prove nothing: the bridges
	// billet makes carry none, and this one does so that a listed guest's chain is what
	// stops it.
	for _, ns := range []string{n.guestNS, n.netNS} {
		sh(t, "ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=0")
	}

	sh(t, "sysctl", "-qw", "net.ipv6.conf.all.disable_ipv6=0")
	sh(t, "sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
	sh(t, "ip", "-6", "addr", "add", six+"::1/64", "dev", n.network, "nodad")
	sh(t, "ip", "-n", n.guestNS, "-6", "addr", "add", six+"::2/64", "dev", "eth0", "nodad")
	sh(t, "ip", "-n", n.guestNS, "-6", "route", "add", "default", "via", six+"::1", "dev", "eth0")
	sh(t, "ip", "-6", "addr", "add", six+":1::1/64", "dev", uplink, "nodad")
	sh(t, "ip", "-n", n.netNS, "-6", "addr", "add", six+":1::2/64", "dev", wan, "nodad")
	sh(t, "ip", "-n", n.netNS, "-6", "addr", "add", n.outside6.String()+"/128", "dev", "lo")
	sh(t, "ip", "-n", n.netNS, "-6", "route", "add", "default", "via", six+":1::1", "dev", wan)
	sh(t, "ip", "-6", "route", "add", n.outside6.String()+"/128", "via", six+":1::2", "dev", uplink)

	// THE ROLE'S GUARD, the rule the node needs and the drop it has to get past. Its drop is
	// held to IPv4 only because the role's bridges carry no IPv6: this one does, for the
	// control, and neighbour discovery has to reach the host for it to work.
	script := fmt.Sprintf(`add table inet billet_guard
add chain inet billet_guard input { type filter hook input priority filter; policy accept; }
add rule inet billet_guard input iifname %q tcp dport { 53, 7718 } accept
add rule inet billet_guard input iifname %q udp dport { 53, 67 } accept
add rule inet billet_guard input iifname %q ct status dnat ct original proto-dst 53 accept comment %q
add rule inet billet_guard input iifname %q meta nfproto ipv4 counter drop
`, n.network, n.network, n.network, GuardRuleComment, n.network)

	cmd := exec.CommandContext(t.Context(), "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install the guard: %v: %s", err, out)
	}

	t.Cleanup(func() {
		takeAway("nft", "delete", "table", "inet", "billet_guard")
	})

	n.other = netip.MustParseAddr(wide + ".1")
	n.listen(n.other, guestCachePort)
	n.listen(n.other, upstreamDNSPort)

	inNamespace(t, n.netNS, func() {
		for _, a := range []netip.Addr{realAllowed, realHardcode, realPrefix} {
			n.listen(a, 443)
		}

		n.listen(realOutside, 53)
		n.listenUDPEcho(realOutside, 53)
		n.listen(n.outside6, 443)
	})

	n.upstream.serve(t, n.gateway)

	return n
}

// listenUDPEcho answers every datagram with itself, which parses as an answer to a query:
// enough to say an outside resolver was reached.
func (n *realNetwork) listenUDPEcho(addr netip.Addr, port uint16) {
	u, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, port)))
	if err != nil {
		n.t.Errorf("listen on %s: %v", addr, err)

		return
	}

	done := make(chan struct{})

	n.t.Cleanup(func() {
		_ = u.Close()
		<-done
	})

	go func() {
		defer close(done)

		buf := make([]byte, maxResponse)

		for {
			read, from, err := u.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			_, _ = u.WriteToUDPAddrPort(buf[:read], from) //nolint:errcheck // a client that left fails for itself
		}
	}()
}

func (n *realNetwork) listen(addr netip.Addr, port uint16) {
	network := "tcp4"
	if addr.Is6() {
		network = "tcp6"
	}

	l, err := net.ListenTCP(network, net.TCPAddrFromAddrPort(netip.AddrPortFrom(addr, port)))
	if err != nil {
		n.t.Errorf("listen on %s: %v", addr, err)

		return
	}

	done := make(chan struct{})

	n.t.Cleanup(func() {
		_ = l.Close()
		<-done
	})

	go func() {
		defer close(done)

		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			_ = c.Close()
		}
	}()
}

// connect says what a guest's connect came to.
func (n *realNetwork) connect(addr netip.Addr, port uint16) error {
	var err error

	inNamespace(n.t, n.guestNS, func() {
		var conn net.Conn

		conn, err = (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp4", netip.AddrPortFrom(addr, port).String())
		if err == nil {
			_ = conn.Close()
		}
	})

	return err
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

	if err := n.connect(addr, port); err != nil {
		n.t.Fatalf("connect to %s:%d: %v", addr, port, err)
	}
}

// mustBeRefused holds a connect to being refused: a drop would look the same to a test that
// only asked whether it failed, and a job would sit in its timeout.
func (n *realNetwork) mustBeRefused(addr netip.Addr, port uint16) {
	n.t.Helper()

	err := n.connect(addr, port)
	if err == nil {
		n.t.Fatalf("connected to %s:%d", addr, port)
	}

	if !errors.Is(err, unix.ECONNREFUSED) {
		n.t.Fatalf("connect to %s:%d = %v, want it refused", addr, port, err)
	}
}

func realHost(t *testing.T, dir string) *Host {
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
			takeAway(append([]string{"nft", "delete", "table"}, strings.Fields(table)...)...)
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

			// THE CONTROLS: all of it answers before there is a list, or a refusal below
			// could be nothing to do with the rules.
			n.mustConnect(realHardcode, 443)
			n.mustConnect(realOutside, 53)

			if _, err := n.resolve("udp4", realOutside, "allowed.test."); err != nil {
				t.Fatalf("control: an outside resolver over UDP: %v", err)
			}

			if err := n.connect6(); err != nil {
				t.Fatalf("control: an IPv6 connect: %v", err)
			}

			h := realHost(t, t.TempDir())
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
			if err := n.connect6(); err == nil {
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

			if _, err := n.resolve("udp4", realOutside, "allowed.test."); err != nil {
				t.Fatalf("after the rules: an outside resolver over UDP: %v", err)
			}

			if err := n.connect6(); err != nil {
				t.Fatalf("after the rules: an IPv6 connect: %v", err)
			}
		})
	}
}

// connect6 tries an IPv6 connect from the guest.
func (n *realNetwork) connect6() error {
	var err error

	inNamespace(n.t, n.guestNS, func() {
		var conn net.Conn

		conn, err = (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp6", netip.AddrPortFrom(n.outside6, 443).String())
		if err == nil {
			_ = conn.Close()
		}
	})

	return err
}

// A PROCESS THAT GOES LEAVES A LISTED GUEST CLOSED, and the next one's resolver picks
// up the same rules and what they already admitted. Between the two the guest resolves
// nothing, and an address nothing admitted stays refused.
func TestRealEgressSurvivesTheNodeProcess(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	dir := t.TempDir()
	h := realHost(t, dir)
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

	next := realHost(t, dir)
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
	h := realHost(t, t.TempDir())
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

	guest := h.live[a.Key]
	before := len(n.upstream.names())

	h.HandOver(t.Context())

	n.mustResolve("udp4", "after.test.", dnsmessage.RCodeSuccess)

	if got := n.upstream.names(); len(got) != before+1 {
		t.Fatalf("the host's resolver heard %q", got)
	}

	// STRAIGHT TO IT, AND NOT THROUGH A RESOLVER THAT STILL THINKS IT IS WATCHING: the
	// upstream hears the query either way.
	if rec := guest.rec.freeze(counts{}); rec.Queries != 0 {
		t.Fatalf("the node's resolver heard %d queries after it handed the guest back", rec.Queries)
	}
}

// ONE UDP SOCKET HELD ACROSS A CHANGE OF RESOLVER KEEPS WORKING: a connection keeps its
// translation whatever became of the rule that made it, so the guest's next query from the
// same port would otherwise reach a port nothing is bound to.
func TestRealEgressMovesAHeldSocketToTheNextResolver(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	dir := t.TempDir()
	h := realHost(t, dir)
	a := realAttachment(n, BridgePort)

	p, err := Compile([]string{"allowed.test"})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Attach(t.Context(), a, p); err != nil {
		t.Fatal(err)
	}

	const port = 41053

	if got := n.resolveFrom(port, "allowed.test."); got != dnsmessage.RCodeSuccess {
		t.Fatalf("before the change: %v", got)
	}

	h.HandOver(t.Context())
	h.Close()

	next := realHost(t, dir)
	next.Reconcile(t.Context(), map[string]bool{a.Key: true})

	if got := n.resolveFrom(port, "allowed.test."); got != dnsmessage.RCodeSuccess {
		t.Fatalf("after the change: %v", got)
	}

	next.Detach(t.Context(), a.Key)
}

// resolveFrom asks the gateway from one fixed source port, which is the same connection each
// time, and fails the test on no answer.
func (n *realNetwork) resolveFrom(port uint16, name string) dnsmessage.RCode {
	n.t.Helper()

	q := aQuery(n.t, name, dnsmessage.TypeA)

	var (
		resp []byte
		err  error
	)

	inNamespace(n.t, n.guestNS, func() {
		var conn *net.UDPConn

		conn, err = net.DialUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(n.guestAddr, port)),
			net.UDPAddrFromAddrPort(netip.AddrPortFrom(n.gateway, upstreamDNSPort)))
		if err != nil {
			return
		}
		defer conn.Close()

		if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
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
		n.t.Fatalf("resolve %s from port %d: %v", name, port, err)
	}

	var p dnsmessage.Parser

	header, err := p.Start(resp)
	if err != nil {
		n.t.Fatalf("the answer does not parse: %v", err)
	}

	return header.RCode
}

// aPeer is a second guest on the same bridge.
type aPeer struct {
	ns    string
	iface string
	addr  netip.Addr
	mac   net.HardwareAddr
}

func (n *realNetwork) addPeer() aPeer {
	n.t.Helper()

	p := aPeer{ns: "egress-peer", iface: "egresstapp", addr: netip.MustParseAddr("172.31.91.3")}

	n.t.Cleanup(func() {
		takeAway("ip", "netns", "del", p.ns)
		takeAway("ip", "link", "del", p.iface)
	})

	sh(n.t, "ip", "netns", "add", p.ns)
	sh(n.t, "ip", "link", "add", p.iface, "type", "veth", "peer", "name", "eth0", "netns", p.ns)
	sh(n.t, "ip", "link", "set", p.iface, "master", n.network)
	sh(n.t, "ip", "link", "set", p.iface, "up")
	sh(n.t, "ip", "-n", p.ns, "addr", "add", p.addr.String()+"/24", "dev", "eth0")
	sh(n.t, "ip", "-n", p.ns, "link", "set", "eth0", "up")
	sh(n.t, "ip", "-n", p.ns, "link", "set", "lo", "up")
	sh(n.t, "ip", "-n", p.ns, "route", "add", "default", "via", n.gateway.String())

	p.mac = macIn(n.t, p.ns, "eth0")

	return p
}

func macIn(t *testing.T, ns, name string) net.HardwareAddr {
	t.Helper()

	var mac net.HardwareAddr

	inNamespace(t, ns, func() {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			t.Errorf("read %s's address: %v", name, err)

			return
		}

		mac = iface.HardwareAddr
	})

	return mac
}

// ipv4Frame is an Ethernet frame holding a UDP datagram, written byte by byte, as a root guest
// can write it.
func ipv4Frame(dstMAC, srcMAC net.HardwareAddr, src, dst netip.AddrPort, data []byte) []byte {
	udp := binary.BigEndian.AppendUint16(nil, src.Port())
	udp = binary.BigEndian.AppendUint16(udp, dst.Port())
	udp = binary.BigEndian.AppendUint16(udp, uint16(8+len(data)))
	udp = binary.BigEndian.AppendUint16(udp, 0)
	udp = append(udp, data...)

	ip := []byte{0x45, 0}
	ip = binary.BigEndian.AppendUint16(ip, uint16(20+len(udp)))
	ip = append(ip, 0, 1, 0, 0, 64, 17, 0, 0)
	ip = append(ip, src.Addr().AsSlice()...)
	ip = append(ip, dst.Addr().AsSlice()...)

	var sum uint32
	for i := 0; i < len(ip); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}

	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}

	binary.BigEndian.PutUint16(ip[10:], ^uint16(sum))

	return rawFrame(dstMAC, srcMAC, unix.ETH_P_IP, append(ip, udp...))
}

func rawFrame(dstMAC, srcMAC net.HardwareAddr, etherType uint16, payload []byte) []byte {
	frame := append(append([]byte{}, dstMAC...), srcMAC...)
	frame = binary.BigEndian.AppendUint16(frame, etherType)

	return append(frame, payload...)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// sendFrame puts a frame on a namespace's eth0, past everything but the host's rules.
func sendFrame(t *testing.T, ns string, frame []byte) {
	t.Helper()

	inNamespace(t, ns, func() {
		iface, err := net.InterfaceByName("eth0")
		if err != nil {
			t.Errorf("find eth0: %v", err)

			return
		}

		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
		if err != nil {
			t.Errorf("open a raw socket: %v", err)

			return
		}
		defer unix.Close(fd)

		if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: iface.Index}); err != nil {
			t.Errorf("send a frame: %v", err)
		}
	})
}

// frameWatch collects what crosses an interface, for asking whether a marker has.
type frameWatch struct {
	mu     sync.Mutex
	frames [][]byte
}

func watchFrames(t *testing.T, ns, name string) *frameWatch {
	t.Helper()

	w := &frameWatch{}

	var fd int

	open := func() {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			t.Errorf("find %s: %v", name, err)

			return
		}

		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
		if err != nil {
			t.Errorf("open a raw socket: %v", err)

			return
		}

		if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: iface.Index}); err != nil {
			t.Errorf("bind a raw socket: %v", err)
		}

		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 100000}); err != nil {
			t.Errorf("bound a raw socket's read: %v", err)
		}
	}

	if ns == "" {
		open()
	} else {
		inNamespace(t, ns, open)
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, 65536)

		for {
			select {
			case <-stop:
				return
			default:
			}

			read, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				continue
			}

			w.mu.Lock()
			w.frames = append(w.frames, bytes.Clone(buf[:read]))
			w.mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		close(stop)
		<-done
		_ = unix.Close(fd)
	})

	return w
}

func (w *frameWatch) saw(marker string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.ContainsFunc(w.frames, func(f []byte) bool { return bytes.Contains(f, []byte(marker)) })
}

// arrives waits for a marker, as a control must see it; a failure guard, never a condition.
func (w *frameWatch) arrives(t *testing.T, marker string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !w.saw(marker) {
		if time.Now().After(deadline) {
			t.Fatalf("control: %q never arrived, so its absence below would prove nothing", marker)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// staysAway says a marker did not arrive in a moment longer than its control took.
func (w *frameWatch) staysAway(t *testing.T, marker string) {
	t.Helper()

	time.Sleep(300 * time.Millisecond)

	if w.saw(marker) {
		t.Fatalf("%q arrived", marker)
	}
}

// A LIST HOLDS AGAINST WHAT A ROOT GUEST CAN WRITE, not only what its stack would: a frame to a
// neighbour's MAC holding an allowed address, a frame of no protocol at all, a neighbour's
// source address and port, and any source address at all. Every refusal has its control, before
// the list: the same bytes, arriving.
func TestRealEgressHoldsAgainstForgedFrames(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	peer := n.addPeer()

	guestMAC := macIn(t, n.guestNS, "eth0")

	bridge, err := net.InterfaceByName(n.network)
	if err != nil {
		t.Fatal(err)
	}

	gateway := netip.AddrPortFrom(n.gateway, upstreamDNSPort)
	atPeer := watchFrames(t, peer.ns, "eth0")
	atBridge := watchFrames(t, "", n.network)

	// TO A NEIGHBOUR'S MAC, WITH AN ADDRESS THE LIST ADMITS: the rules read addresses, and a
	// bridge delivers by MAC.
	toPeer := func(marker string) []byte {
		return ipv4Frame(peer.mac, guestMAC, netip.AddrPortFrom(n.guestAddr, 40000),
			netip.AddrPortFrom(realPrefix, 443), []byte(marker))
	}

	// OF NO PROTOCOL BILLET READS.
	noProtocol := func(dst net.HardwareAddr, marker string) []byte {
		return rawFrame(dst, guestMAC, 0x88b5, []byte(marker))
	}

	// A QUERY CLAIMING TO BE FROM THE NEIGHBOUR: an off-list name whose answer would reach it.
	forged := func(port uint16, name string) []byte {
		return ipv4Frame(bridge.HardwareAddr, guestMAC, netip.AddrPortFrom(peer.addr, port), gateway,
			aQuery(t, name, dnsmessage.TypeA))
	}

	// THE CONTROLS, WITH NO RULES ON THE GUEST AT ALL.
	sendFrame(t, n.guestNS, toPeer("control-to-peer"))
	atPeer.arrives(t, "control-to-peer")

	sendFrame(t, n.guestNS, noProtocol(peer.mac, "control-no-protocol"))
	atPeer.arrives(t, "control-no-protocol")

	sendFrame(t, n.guestNS, noProtocol(bridge.HardwareAddr, "control-no-protocol-host"))
	atBridge.arrives(t, "control-no-protocol-host")

	sendFrame(t, n.guestNS, forged(41001, "control.attacker.test."))
	atPeer.arrives(t, "attacker") // the upstream's answer, sent to the neighbour it named

	if !slices.Contains(n.upstream.names(), "control.attacker.test") {
		t.Fatalf("control: the forged query did not reach the upstream: %q", n.upstream.names())
	}

	h := realHost(t, t.TempDir())
	a := realAttachment(n, BridgePort)

	p, err := Compile([]string{"allowed.test", realPrefix.String() + "/32"})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.Attach(t.Context(), a, p); err != nil {
		t.Fatal(err)
	}

	// A NEIGHBOUR'S MAC, AND A FRAME OF NO PROTOCOL, TO ANYONE.
	sendFrame(t, n.guestNS, toPeer("listed-to-peer"))
	atPeer.staysAway(t, "listed-to-peer")

	sendFrame(t, n.guestNS, noProtocol(peer.mac, "listed-no-protocol"))
	atPeer.staysAway(t, "listed-no-protocol")

	sendFrame(t, n.guestNS, noProtocol(bridge.HardwareAddr, "listed-no-protocol-host"))
	atBridge.staysAway(t, "listed-no-protocol-host")

	// AND NOR CAN THE NEIGHBOUR REACH IT.
	atGuest := watchFrames(t, n.guestNS, "eth0")
	sendFrame(t, peer.ns, rawFrame(guestMAC, peer.mac, 0x88b5, []byte("peer-to-listed")))
	atGuest.staysAway(t, "peer-to-listed")

	// A NEIGHBOUR'S SOURCE ADDRESS: the off-list name is refused, and the refusal, which
	// would carry it to the neighbour, does not arrive there.
	sendFrame(t, n.guestNS, forged(41002, "echoed.attacker.test."))
	atPeer.staysAway(t, "echoed")

	if slices.Contains(n.upstream.names(), "echoed.attacker.test") {
		t.Fatalf("an off-list name reached the upstream: %q", n.upstream.names())
	}

	// AN ADDRESS OFF THE SUBNET ALTOGETHER: a query is no way to have the host send its bytes
	// wherever it said.
	sendFrame(t, n.guestNS, ipv4Frame(bridge.HardwareAddr, guestMAC,
		netip.MustParseAddrPort("192.0.2.2:41003"), gateway, aQuery(t, "reflected.attacker.test.", dnsmessage.TypeA)))

	atUplink := watchFrames(t, n.netNS, "egresswanb")
	atUplink.staysAway(t, "reflected")

	h.Detach(t.Context(), a.Key)

	rec, ok := h.Take(a.Key)
	if !ok {
		t.Fatal("no record")
	}

	if rec.DeniedPackets == 0 {
		t.Error("nothing the guest was refused was counted")
	}
}

// A CONNECTION IS ITS OWNER'S. A guest forging a neighbour's source address and port joins that
// neighbour's connection, whose redirect was chosen when it was made: without the rule it would
// reach the neighbour's resolver under the neighbour's policy, or dnsmasq itself.
func TestRealEgressKeepsAGuestOutOfItsNeighboursConnection(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	for _, tc := range []struct {
		name    string
		watched bool
		// port differs between cases, since the tracker outlives a case's firewall and a
		// connection from the last would be joined by the next.
		port uint16
	}{
		{"a neighbour nothing watches, whose queries reach dnsmasq", false, 41005},
		{"a neighbour whose own resolver would forward anything", true, 41006},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dropTables(t)

			n := newRealNetwork(t, BridgePort)
			peer := n.addPeer()
			guestMAC := macIn(t, n.guestNS, "eth0")

			bridge, err := net.InterfaceByName(n.network)
			if err != nil {
				t.Fatal(err)
			}

			h := realHost(t, t.TempDir())

			if tc.watched {
				if err := h.Attach(t.Context(), Attachment{Key: "billet-real-peer", Interface: peer.iface,
					Network: n.network, Kind: BridgePort}, Observe()); err != nil {
					t.Fatal(err)
				}
			}

			p, err := Compile([]string{"allowed.test"})
			if err != nil {
				t.Fatal(err)
			}

			if err := h.Attach(t.Context(), realAttachment(n, BridgePort), p); err != nil {
				t.Fatal(err)
			}

			port := tc.port

			ask := func(name string) {
				t.Helper()

				var askErr error

				inNamespace(t, peer.ns, func() {
					var conn *net.UDPConn

					conn, askErr = net.DialUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.addr, port)),
						net.UDPAddrFromAddrPort(netip.AddrPortFrom(n.gateway, upstreamDNSPort)))
					if askErr != nil {
						return
					}
					defer conn.Close()

					if askErr = conn.SetDeadline(time.Now().Add(5 * time.Second)); askErr != nil {
						return
					}

					if _, askErr = conn.Write(aQuery(t, name, dnsmessage.TypeA)); askErr != nil {
						return
					}

					_, askErr = conn.Read(make([]byte, maxResponse))
				})

				if askErr != nil {
					t.Fatalf("the neighbour's own query for %s: %v", name, askErr)
				}
			}

			ask("first.test.")

			// THE SAME FIVE-TUPLE, FROM THE GUEST WITH THE LIST.
			sendFrame(t, n.guestNS, ipv4Frame(bridge.HardwareAddr, guestMAC,
				netip.AddrPortFrom(peer.addr, port), netip.AddrPortFrom(n.gateway, upstreamDNSPort),
				aQuery(t, "stolen.attacker.test.", dnsmessage.TypeA)))

			time.Sleep(300 * time.Millisecond)

			if slices.Contains(n.upstream.names(), "stolen.attacker.test") {
				t.Fatalf("an off-list name left through a neighbour's connection: %q", n.upstream.names())
			}

			// AND THE CONNECTION IS STILL ITS OWNER'S.
			ask("second.test.")

			if !slices.Contains(n.upstream.names(), "second.test") {
				t.Fatalf("the neighbour's own query was lost: %q", n.upstream.names())
			}
		})
	}
}

// THE LIST STANDS ON ITS OWN WHEN THE NODE'S RULES ARE RENDERED FOR A KERNEL: overlapping
// prefixes, which are an error to nft without auto-merge, and an empty list.
func TestRealEgressTakesOverlappingPrefixes(t *testing.T) { //nolint:paralleltest // it owns the namespace's firewall.
	dropTables(t)

	n := newRealNetwork(t, BridgePort)
	h := realHost(t, t.TempDir())
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
