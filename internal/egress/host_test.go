package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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
	mu       sync.Mutex
	scripts  []string
	calls    []string
	guard    string
	tables   bool
	guests   string
	jobs     string
	denyHeld bool
	refuse   func(script string, args []string) error
	onScript func(script string)
}

const anEmptyMap = `{"nftables":[{"map":{"elem":[]}}]}`

func newFakeNft() *fakeNft {
	return &fakeNft{guard: `{"nftables":[{"rule":{"comment":"` + GuardRuleComment + `"}}]}`, tables: true,
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

		if f.onScript != nil {
			f.onScript(script)
		}

		return nil, nil
	case strings.Contains(call, "chain inet billet_guard input"):
		return []byte(f.guard), nil
	case strings.HasPrefix(call, "list table"):
		if !f.tables {
			return nil, errors.New("Error: No such file or directory")
		}

		return nil, nil
	case strings.Contains(call, "list map ip billet_egress_nat jobs"):
		return []byte(f.jobs), nil
	case strings.Contains(call, "list map bridge billet_egress guests"):
		return []byte(f.guests), nil
	case strings.Contains(call, "list map inet billet_egress_routed guests"):
		return []byte(anEmptyMap), nil
	case strings.Contains(call, "list chain") && strings.Contains(call, "deny_"):
		if !f.denyHeld {
			return nil, errors.New("Error: No such file or directory")
		}

		return nil, nil
	case strings.Contains(call, "list counter"):
		return []byte(`{"nftables":[{"counter":{"packets":3,"bytes":180}}]}`), nil
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
		WithGateway(func(string) (netip.Prefix, error) { return netip.MustParsePrefix("127.0.0.1/8"), nil }))
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

func TestPreparingCreatesTheTablesOnlyWhereThereAreNone(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	f.tables = false
	preparedHost(t, f)

	if got := f.allScripts(); len(got) != 2 || !strings.HasPrefix(got[0], "-c -f -\n") ||
		!strings.Contains(got[0], "add table bridge billet_egress") ||
		!strings.Contains(got[1], "add table bridge billet_egress") {
		t.Fatalf("scripts = %q, want the dry run of tables and a guest, then the tables", got)
	}

	f = newFakeNft()
	preparedHost(t, f)

	for _, s := range f.allScripts() {
		if strings.Contains(s, "add table") {
			t.Fatalf("a host with its tables got more: %s", s)
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

	for _, want := range []string{
		"add element bridge billet_egress cidr_" + guestKey + " { 203.0.113.0/24 }",
		"redirect to :42751",
		"add element ip billet_egress_nat jobs { 0xb1e70007 : jump dns_" + guestKey + " }",
		"ip protocol tcp reject with tcp reset",
		"update @denied_" + guestKey + " { ip daddr . ip protocol . th dport }",
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

	for _, refused := range []string{"reject", "drop", "goto", "add set", "add counter", " accept"} {
		if strings.Contains(s, refused) {
			t.Errorf("an open guest's script holds %q:\n%s", refused, s)
		}
	}
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

// COUNTERS THAT COULD NOT BE READ ARE NOT A GUEST REFUSED NOTHING.
func TestCountsThatCouldNotBeReadAreNotZero(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := preparedHost(t, f)

	if err := h.Attach(t.Context(), aGuest(), listed(t, "example.com")); err != nil {
		t.Fatal(err)
	}

	f.refuse = func(_ string, args []string) error {
		if strings.Contains(strings.Join(args, " "), "list counter") {
			return errors.New("Operation not permitted")
		}

		return nil
	}

	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || !rec.Incomplete {
		t.Fatalf("record = %+v, %v, want it marked incomplete", rec, ok)
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
func aFoundGuest(f *fakeNft, key, iface string, mark uint32) {
	f.guests = fmt.Sprintf(`{"nftables":[{"map":{"elem":[[%q,{"jump":{"target":"job_%s"}}]]}}]}`, iface, key)
	f.jobs = fmt.Sprintf(`{"nftables":[{"map":{"elem":[[%d,{"jump":{"target":"dns_%s"}}]]}}]}`, mark, key)
}

// keepFile stages the file an earlier process wrote for a guest.
func keepFile(t *testing.T, h *Host, a Attachment, p *Policy) {
	t.Helper()

	if err := h.writeFile(a, p); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileGivesARunningListedGuestANewResolverAndKeepsItClosed(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	aFoundGuest(f, guestKey, guestTap, 0xb1e70009)

	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))
	h.Prepare(t.Context())

	h.Reconcile(t.Context(), map[string]bool{guestKey: true})

	script := scriptWith(t, f, "flush chain ip billet_egress_nat dns_"+guestKey)
	if !strings.Contains(script, "redirect to :") {
		t.Fatalf("the redirect was not pointed anywhere:\n%s", script)
	}

	for _, s := range f.allScripts() {
		// THE READINESS DRY RUN NAMES ITS OWN, AND CHANGES NOTHING.
		if !strings.HasPrefix(s, "-c ") &&
			(strings.Contains(s, "delete chain") || strings.Contains(s, "add chain bridge billet_egress job_")) {
			t.Fatalf("a running listed guest's rules were touched: %s", s)
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
	aFoundGuest(f, guestKey, guestTap, 0xb1e70009)

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
	aFoundGuest(f, guestKey, guestTap, 0xb1e70009)

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
	aFoundGuest(f, guestKey, guestTap, 0xb1e70009)

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
	aFoundGuest(f, guestKey, guestTap, 0xb1e70009)

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

func TestPreparingRemovesAFileWithNoRules(t *testing.T) {
	t.Parallel()

	f := newFakeNft()
	h := aHost(t, f)
	keepFile(t, h, aGuest(), listed(t, "example.com"))

	h.Prepare(t.Context())

	if _, err := h.readFile(guestKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file with no rules was kept: %v", err)
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

	if err := h.Attach(t.Context(), aGuest(), listed(t, "allowed.example")); err != nil {
		t.Fatal(err)
	}

	guest := h.live[guestKey]

	resp := askUDP(t, guest.fwd.port, aQuery(t, "allowed.example.", dnsmessage.TypeA))
	if got := rcode(t, resp); got != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", got)
	}

	// THE SET WAS FILLED BY THE TIME THE ANSWER ARRIVED, SO THIS NEEDS NO WAIT.
	scriptWith(t, f, "add element bridge billet_egress allow_"+guestKey+" { 192.0.2.10 }")

	h.Detach(t.Context(), guestKey)

	rec, ok := h.Take(guestKey)
	if !ok || !slices.Equal(rec.Names[anAddr], []string{"allowed.example"}) || rec.Queries != 1 {
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
