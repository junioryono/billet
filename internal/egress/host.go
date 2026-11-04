package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

const (
	// upstreamDNSPort is where the host's resolver answers on a guest's gateway.
	upstreamDNSPort = 53
	// maxKeptRecords bounds the records of guests that went and nobody took: a
	// sweep destroys what no report is owed for.
	maxKeptRecords = 256
	// recordSchema is the attachment file's shape. A file of another is treated as
	// no file: the guest stays closed, and is said to.
	recordSchema = 1
	// maxRecordBytes bounds reading a file this package writes.
	maxRecordBytes = 1 << 20
	recordSuffix   = ".json"
)

// ErrNotReady wraps why a host cannot attach anything.
var ErrNotReady = errors.New("this host cannot enforce a network policy")

// errRulesUncertain says rules may be installed that could not be removed: nft failing
// is not proof its transaction did not commit. Even an open guest does not start under
// it, since its DNS may be redirected to a resolver that has gone.
var errRulesUncertain = errors.New("egress: rules may be installed, and could not be removed")

// Attachment names one guest's interface.
type Attachment struct {
	// Key names the guest: its instance's name, unique while it lives.
	Key string
	// Interface is the host's end of the guest's network, which need not exist
	// yet: the rules match it by name, so a guest is never on the network without
	// them.
	Interface string
	// Network is the device holding the gateway's address: the bridge for a
	// BridgePort, the interface itself for a Routed one.
	Network string
	Kind    Kind
}

// attached is one guest this process has rules for.
type attached struct {
	Attachment
	mark   uint32
	listed bool
	policy *Policy
	// gateway and local are what its rules were made from, for putting them back.
	gateway netip.Prefix
	local   []netip.Addr

	// fwd and rec are nil for rules found with no way to serve them.
	fwd *forwarder
	rec *recorder

	admitMu sync.Mutex
	// admitted is what is in the guest's set already, so the common case of an
	// answer holding nothing new runs no process.
	admitted map[netip.Addr]struct{}
}

// Host is the node's one handle on its guests' network policy. Safe for
// concurrent use; several launches attach at once.
type Host struct {
	log      *slog.Logger
	stateDir string
	run      runFunc
	addrOf   func(iface string) (netip.Prefix, error)
	locals   func() ([]netip.Addr, error)
	forget   func(mark uint32) error
	files    durablefile.Installer
	// upstreamPort is where the host's resolver answers, upstreamDNSPort but in a test.
	upstreamPort uint16

	// nftMu serializes nft: each transaction checks a generation it read, and two
	// at once can fail each other for nothing.
	nftMu sync.Mutex

	mu       sync.Mutex
	notReady error
	warned   bool
	live     map[string]*attached
	// foundOnStart are the guests earlier processes left rules for, until
	// Reconcile has judged them.
	foundOnStart map[string]*attached
	// filesJudged says the files with no rules behind them have been judged, which only
	// the first Reconcile can do: until then nothing has said which guests are running.
	filesJudged bool
	// byIface is who the rules matching an interface name belong to, which a
	// launch reusing the name must clear.
	byIface map[string]string
	slots   map[uint32]struct{}
	next    uint32
	done    map[string]Record
	order   []string
}

// Option adjusts a Host.
type Option func(*Host)

// WithRunner replaces running nft, for a test on a machine without a kernel to ask.
func WithRunner(run func(ctx context.Context, script string, args ...string) ([]byte, error)) Option {
	return func(h *Host) { h.run = run }
}

// WithConnectionForget replaces deleting a guest's connections from the tracker, for a
// test on a machine with none.
func WithConnectionForget(forget func(mark uint32) error) Option {
	return func(h *Host) { h.forget = forget }
}

// WithLocalAddresses replaces reading this host's addresses, for a test that wants to
// know what they are.
func WithLocalAddresses(local func() ([]netip.Addr, error)) Option {
	return func(h *Host) { h.locals = local }
}

// WithGateway replaces reading a device's address, for a test with no such device.
func WithGateway(addrOf func(iface string) (netip.Prefix, error)) Option {
	return func(h *Host) { h.addrOf = addrOf }
}

// NewHost returns a Host keeping what it must across a restart under stateDir.
// It is not ready until Prepare has said so.
func NewHost(stateDir string, log *slog.Logger, opts ...Option) *Host {
	h := &Host{
		log: log, stateDir: stateDir, run: runNft, addrOf: interfaceAddress, locals: localAddresses, forget: forgetConnections, upstreamPort: upstreamDNSPort,
		notReady: fmt.Errorf("%w: it has not been prepared", ErrNotReady),
		live:     map[string]*attached{}, foundOnStart: map[string]*attached{}, byIface: map[string]string{},
		slots: map[uint32]struct{}{}, done: map[string]Record{},
	}

	for _, opt := range opts {
		opt(h)
	}

	return h
}

// interfaceAddress is a device's IPv4 address and prefix.
func interfaceAddress(iface string) (netip.Prefix, error) {
	dev, err := net.InterfaceByName(iface)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("egress: find %s: %w", iface, err)
	}

	addrs, err := dev.Addrs()
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("egress: read the addresses of %s: %w", iface, err)
	}

	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}

		if v4 := n.IP.To4(); v4 != nil {
			addr, _ := netip.AddrFromSlice(v4)
			bits, _ := n.Mask.Size()

			return netip.PrefixFrom(addr, bits), nil
		}
	}

	return netip.Prefix{}, fmt.Errorf("egress: %s has no IPv4 address", iface)
}

// localAddresses is every IPv4 address this host answers on but loopback's, which a
// guest has one of its own of and cannot route to.
func localAddresses() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("egress: read this host's addresses: %w", err)
	}

	var out []netip.Addr

	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}

		if addr, ok := netip.AddrFromSlice(n.IP.To4()); ok && !addr.IsLoopback() {
			out = append(out, addr)
		}
	}

	return out, nil
}

// Prepare proves this host can enforce a policy, and finds what earlier processes
// left. A host that cannot is NOT an error here: the node may never be asked for
// a list, and an open guest needs none of it. Ready says why, and a listed launch
// is refused with that.
func (h *Host) Prepare(ctx context.Context) {
	err := h.prepare(ctx)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.notReady = nil
	if err != nil {
		h.notReady = fmt.Errorf("%w: %w", ErrNotReady, err)
		h.log.Warn("a job on this node cannot have a network allowlist, or its names recorded", "reason", err)

		return
	}

	h.log.Info("this node can enforce a network allowlist and record the names its guests resolve")
}

func (h *Host) prepare(ctx context.Context) error {
	tables, err := checkHost(ctx, h.nft)
	if err != nil {
		return err
	}

	if !tables {
		if _, err := h.nft(ctx, baseScript(), "-f", "-"); err != nil {
			return fmt.Errorf("could not create this node's firewall tables: %w", err)
		}
	}

	return h.findExisting(ctx)
}

// Check answers whether this host could enforce a policy, changing nothing: nil, or
// why not, which includes not being able to tell.
func Check(ctx context.Context) error {
	_, err := checkHost(ctx, runNft)

	return err
}

// checkHost asks the host, and answers whether billet's tables exist yet.
//
// THE KERNEL IS ASKED, not its version compared: a dry run of a whole listed guest
// fails on a kernel or an nft without any part of it, and changes nothing.
func checkHost(ctx context.Context, run runFunc) (bool, error) {
	out, err := run(ctx, "", "-j", "list", "chain", "inet", "billet_guard", "input")
	if err != nil {
		if absent(err) {
			return false, errors.New("the host role's guest firewall is not installed (no chain input in " +
				"table inet billet_guard); converge the host")
		}

		return false, fmt.Errorf("could not read the host role's guest firewall: %w", err)
	}

	if !strings.Contains(string(out), GuardRuleComment) {
		return false, fmt.Errorf("the host role's guest firewall has no rule %q, so redirected DNS would be "+
			"dropped; converge the host with the collection from this release", GuardRuleComment)
	}

	tables := true

	if _, err := run(ctx, "", "list", "table", "bridge", "billet_egress"); err != nil {
		if !absent(err) {
			return false, fmt.Errorf("could not read this node's firewall tables: %w", err)
		}

		tables = false
	}

	script := attachScript(binding{
		key: "readiness", iface: "readiness0", kind: BridgePort, gateway: netip.MustParsePrefix("192.0.2.1/24"),
		mark: markBase, port: 1, listed: true, prefixes: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	})
	if !tables {
		script = baseScript() + script
	}

	if _, err := run(ctx, script, "-c", "-f", "-"); err != nil {
		return false, fmt.Errorf("this kernel or nft refuses a guest's rules: %w", err)
	}

	return tables, nil
}

// nft runs one nft command, one at a time.
func (h *Host) nft(ctx context.Context, script string, args ...string) ([]byte, error) {
	h.nftMu.Lock()
	defer h.nftMu.Unlock()

	return h.run(ctx, script, args...)
}

// findExisting reads who holds which interface and mark, from the kernel, and what
// each was attached with, from its file.
func (h *Host) findExisting(ctx context.Context) error {
	marks := map[string]uint32{}

	out, err := h.nft(ctx, "", "-j", "list", "map", "ip", "billet_egress_nat", "jobs")
	if err != nil {
		return fmt.Errorf("could not read this node's firewall tables: %w", err)
	}

	elements, err := parseVerdictMap(out)
	if err != nil {
		return err
	}

	for _, e := range elements {
		var mark uint32
		if err := json.Unmarshal(e.key, &mark); err != nil {
			return fmt.Errorf("egress: a mark in nft's map: %w", err)
		}

		marks[strings.TrimPrefix(e.target, "dns_")] = mark
	}

	found := map[string]*attached{}

	for _, kind := range []Kind{BridgePort, Routed} {
		family, name, _ := strings.Cut(kind.table(), " ")

		out, err := h.nft(ctx, "", "-j", "list", "map", family, name, "guests")
		if err != nil {
			return fmt.Errorf("could not read this node's firewall tables: %w", err)
		}

		elements, err := parseVerdictMap(out)
		if err != nil {
			return err
		}

		for _, e := range elements {
			var iface string
			if err := json.Unmarshal(e.key, &iface); err != nil {
				return fmt.Errorf("egress: an interface in nft's map: %w", err)
			}

			key := strings.TrimPrefix(e.target, "job_")
			found[key] = &attached{
				Attachment: Attachment{Key: key, Interface: iface, Kind: kind}, mark: marks[key],
			}
		}
	}

	h.mu.Lock()

	for key, a := range found {
		h.foundOnStart[key] = a
		h.byIface[a.Interface] = key
		h.slots[a.mark] = struct{}{}
	}

	h.mu.Unlock()

	return nil
}

// judgeFilesWithoutRules reads the files no rule stands behind, given what is running.
// Before anything can be attaching, since the first recovery comes before the first
// launch, which is the only time a file with no rules cannot be one mid-attach.
//
// A FILE FOR A GUEST THAT IS NOT RUNNING, OR AN OPEN ONE, IS NOTHING: a process died
// between writing it and installing the rules. A LISTED GUEST THAT IS RUNNING HAS LOST
// ITS RULES, and is running open: the rules go back, and what they count from here is
// marked as not the whole job. Reading this as nothing, and deleting the file, is how a
// flushed firewall would have left a tier's job open with no record it ever had a list.
func (h *Host) judgeFilesWithoutRules(ctx context.Context, running map[string]bool) {
	entries, err := os.ReadDir(h.stateDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.log.Warn("could not look for guest policy files with no rules", "dir", h.stateDir, "error", err)
		}

		return
	}

	for _, e := range entries {
		key, ours := strings.CutSuffix(e.Name(), recordSuffix)
		if !ours || !keyRe.MatchString(key) {
			continue
		}

		h.mu.Lock()
		_, live := h.live[key]
		_, found := h.foundOnStart[key]
		h.mu.Unlock()

		if live || found {
			continue
		}

		file, err := h.readFile(key)
		if err != nil || !running[key] || !file.Listed {
			h.removeFile(key)

			continue
		}

		h.restoreRules(ctx, file)
	}
}

// restoreRules puts a running listed guest's rules back from its file.
func (h *Host) restoreRules(ctx context.Context, file attachmentFile) {
	h.log.Error("a running guest with a network allowlist has lost its rules, so it has been running "+
		"open; they are being put back. Something other than billet removed them: `nft flush ruleset`, "+
		"or a firewall service that does", "guest", file.Key)

	p, err := Compile(file.Allow)
	if err == nil {
		err = h.ensureTables(ctx)
	}

	if err == nil {
		err = h.attach(ctx, Attachment{Key: file.Key, Interface: file.Interface, Network: file.Network,
			Kind: file.Kind}, p)
	}

	if err != nil {
		h.log.Error("could not put a running guest's network rules back, so it is still running open",
			"guest", file.Key, "error", err)

		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if a := h.live[file.Key]; a != nil {
		a.rec.incomplete()
	}
}

// ensureTables creates billet's tables if they have gone.
func (h *Host) ensureTables(ctx context.Context) error {
	_, err := h.nft(ctx, "", "list", "table", "bridge", "billet_egress")
	if err == nil {
		return nil
	}

	if !absent(err) {
		return err
	}

	_, err = h.nft(ctx, baseScript(), "-f", "-")

	return err
}

// verifyLive checks that this process's listed guests still have their rules, and puts
// back any that have gone. Recovery runs at every registration, so a flush is found at
// the next, rather than only when the job ends.
func (h *Host) verifyLive(ctx context.Context) {
	h.mu.Lock()
	guests := slices.Collect(maps.Values(h.live))
	h.mu.Unlock()

	for _, a := range guests {
		if !a.listed || a.fwd == nil {
			continue
		}

		family, name, _ := strings.Cut(a.Kind.table(), " ")
		if _, err := h.nft(ctx, "", "list", "chain", family, name, "job_"+a.Key); !absent(err) {
			continue
		}

		h.reinstall(ctx, a)
	}
}

// reinstall puts a live listed guest's rules back. Its set is empty again, so what its
// names admitted must be asked for again, which closes rather than opens.
func (h *Host) reinstall(ctx context.Context, a *attached) {
	h.log.Error("a running guest with a network allowlist has lost its rules, so it has been running "+
		"open; they are being put back. Something other than billet removed them: `nft flush ruleset`, "+
		"or a firewall service that does", "guest", a.Key)

	h.mu.Lock()
	_, still := h.live[a.Key]
	h.mu.Unlock()

	if !still {
		return
	}

	err := h.ensureTables(ctx)
	if err == nil {
		_, err = h.nft(ctx, attachScript(binding{
			key: a.Key, iface: a.Interface, kind: a.Kind, gateway: a.gateway, mark: a.mark, port: a.fwd.port,
			listed: true, prefixes: a.policy.prefixes, local: a.local,
		}), "-f", "-")
	}

	if err != nil {
		h.log.Error("could not put a running guest's network rules back, so it is still running open",
			"guest", a.Key, "error", err)

		return
	}

	a.rec.incomplete()

	a.admitMu.Lock()
	defer a.admitMu.Unlock()

	a.admitted = map[netip.Addr]struct{}{}
}

// Ready says whether a listed guest can be attached here: nil, or why not.
func (h *Host) Ready() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.notReady
}

// Attach puts p on a guest's interface before it exists. A nil p is a guest with
// no policy, whose interface name may still carry a dead guest's rules, and those
// are cleared: a name is reused, and the next guest must not inherit them.
//
// A LISTED GUEST THAT CANNOT BE ATTACHED IS AN ERROR, which refuses its launch. AN
// OPEN ONE IS NOT: it runs as it would have, its names unrecorded, and this says
// so once.
func (h *Host) Attach(ctx context.Context, a Attachment, p *Policy) error {
	if !keyRe.MatchString(a.Key) {
		return fmt.Errorf("egress: %q cannot name a guest's rules", a.Key)
	}

	if !ifaceRe.MatchString(a.Interface) {
		return fmt.Errorf("egress: %q cannot name a guest's interface", a.Interface)
	}

	if err := h.clearName(ctx, a.Interface, a.Key); err != nil {
		return fmt.Errorf("egress: %s still carries a guest's rules, and they could not be cleared: %w",
			a.Interface, err)
	}

	if p == nil {
		return nil
	}

	if err := h.Ready(); err != nil {
		if p.Listed() {
			return err
		}

		h.warnUnwatched(err)

		return nil
	}

	err := h.attach(ctx, a, p)
	if err != nil && !p.Listed() && !errors.Is(err, errRulesUncertain) {
		h.log.Warn("could not watch a guest's DNS; its names will not be recorded", "guest", a.Key, "error", err)

		return nil
	}

	return err
}

func (h *Host) warnUnwatched(reason error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.warned {
		h.warned = true
		h.log.Warn("jobs on this node run without their names recorded", "reason", reason)
	}
}

// clearName removes whatever rules hold iface or key: a dead guest's, whose
// detach failed or which an earlier process left.
func (h *Host) clearName(ctx context.Context, iface, key string) error {
	h.mu.Lock()

	var stale []*attached

	if holder, ok := h.byIface[iface]; ok {
		if a := h.takeLocked(holder); a != nil {
			stale = append(stale, a)
		}
	}

	if a := h.takeLocked(key); a != nil {
		stale = append(stale, a)
	}

	h.mu.Unlock()

	var errs []error

	for _, a := range stale {
		if a.fwd != nil {
			a.fwd.stop()
		}

		if err := h.remove(ctx, a); err != nil {
			errs = append(errs, err)

			// KEPT, so the next attempt finds it rather than the kernel's refusal.
			h.keepForLater(a)
		}
	}

	return errors.Join(errs...)
}

// takeLocked removes one guest from the books, answering it.
func (h *Host) takeLocked(key string) *attached {
	a := h.live[key]
	delete(h.live, key)

	if a == nil {
		a = h.foundOnStart[key]
		delete(h.foundOnStart, key)
	}

	if a != nil && h.byIface[a.Interface] == key {
		delete(h.byIface, a.Interface)
	}

	return a
}

// allocMark takes a free mark.
func (h *Host) allocMark() (uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for range maxSlot {
		h.next = h.next%maxSlot + 1

		mark := markBase | h.next
		if _, taken := h.slots[mark]; !taken {
			h.slots[mark] = struct{}{}

			return mark, nil
		}
	}

	return 0, errors.New("egress: every mark is in use")
}

func (h *Host) freeMark(mark uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.slots, mark)
}

// attach installs p's rules and starts their resolver.
//
// THE FILE BEFORE THE RULES: a process that dies between the two leaves a file
// Reconcile can throw away. The other order leaves rules nobody can say the
// policy of.
func (h *Host) attach(ctx context.Context, a Attachment, p *Policy) error {
	gateway, err := h.addrOf(a.Network)
	if err != nil {
		return err
	}

	// A LIST NEEDS THEM, or a guest on the other bridge loses the node's caches; an
	// open guest's chain judges nothing and never reads them.
	var local []netip.Addr

	if p.Listed() {
		if local, err = h.locals(); err != nil {
			return err
		}
	}

	mark, err := h.allocMark()
	if err != nil {
		return err
	}

	// KEPT WITH THE RULES when they may be installed: the mark and the file are how
	// whoever clears them later names them.
	done, uncertain := false, false

	defer func() {
		if !done && !uncertain {
			h.freeMark(mark)
		}
	}()

	guest := &attached{
		Attachment: a, mark: mark, listed: p.Listed(), policy: p, gateway: gateway, local: local,
		rec: newRecorder(p.Listed(), false), admitted: map[netip.Addr]struct{}{},
	}

	guest.fwd, err = newForwarder(p, guest.rec, gateway, h.upstreamPort, h.log.With("guest", a.Key),
		func(ctx context.Context, addrs []netip.Addr) error { return h.admit(ctx, guest, addrs) })
	if err != nil {
		return err
	}

	defer func() {
		if !done {
			guest.fwd.stop()
		}
	}()

	if err := h.writeFile(a, p); err != nil {
		return err
	}

	defer func() {
		if !done && !uncertain {
			h.removeFile(a.Key)
		}
	}()

	if _, err := h.nft(ctx, attachScript(binding{
		key: a.Key, iface: a.Interface, kind: a.Kind, gateway: gateway, mark: mark, port: guest.fwd.port,
		listed: p.Listed(), prefixes: p.prefixes, local: local,
	}), "-f", "-"); err != nil {
		err = fmt.Errorf("egress: install the rules of %s: %w", a.Key, err)

		if settleErr := h.settleFailedInstall(ctx, guest); settleErr != nil {
			uncertain = true

			return fmt.Errorf("%w: %w: %w", errRulesUncertain, err, settleErr)
		}

		return err
	}

	guest.fwd.serve(ctx)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.live[a.Key] = guest
	h.byIface[a.Interface] = a.Key
	done = true

	return nil
}

// settleFailedInstall establishes that a failed install left nothing, by removing what
// it would have made: a transaction is all or none, so absence is proof of none. If
// that fails too the guest's rules stay on the books for its unwind, the next guest on
// its interface and the next recovery.
func (h *Host) settleFailedInstall(ctx context.Context, a *attached) error {
	ctx = context.WithoutCancel(ctx)

	_, err := h.nft(ctx, detachScript(a.Key, a.Interface, a.Kind, a.mark, a.listed), "-f", "-")
	if err == nil || absent(err) {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.foundOnStart[a.Key] = &attached{Attachment: a.Attachment, mark: a.mark}
	h.byIface[a.Interface] = a.Key

	return err
}

// keepForLater puts rules that would not go back on the books, for the next guest on
// their interface and the next recovery. Unless a guest has taken their interface since,
// which has cleared them.
func (h *Host) keepForLater(a *attached) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if holder, held := h.byIface[a.Interface]; held && holder != a.Key {
		return
	}

	h.foundOnStart[a.Key] = a
	h.byIface[a.Interface] = a.Key
}

// admit adds addresses to a guest's set and returns once they are there. What is
// there already runs nothing, which is nearly every answer.
func (h *Host) admit(ctx context.Context, a *attached, addrs []netip.Addr) error {
	a.admitMu.Lock()
	defer a.admitMu.Unlock()

	var fresh []netip.Addr

	for _, addr := range addrs {
		if _, known := a.admitted[addr]; !known && !slices.Contains(fresh, addr) {
			fresh = append(fresh, addr)
		}
	}

	if len(fresh) == 0 {
		return nil
	}

	if len(a.admitted)+len(fresh) > maxAllowed {
		return fmt.Errorf("egress: the %d addresses a guest's names may answer are used up", maxAllowed)
	}

	if _, err := h.nft(ctx, admitScript(a.Key, a.Kind, fresh), "-f", "-"); err != nil {
		return err
	}

	for _, addr := range fresh {
		a.admitted[addr] = struct{}{}
	}

	return nil
}

// Detach removes a guest's rules once it has gone, keeping what they counted for
// Take. A failure to remove them is kept for the next launch to reuse the
// interface and for Reconcile, and is no reason to think the guest still exists.
func (h *Host) Detach(ctx context.Context, key string) {
	h.mu.Lock()
	a := h.takeLocked(key)
	h.mu.Unlock()

	if a == nil {
		return
	}

	if a.fwd != nil {
		a.fwd.stop()
	}

	// A LIST NOTHING COULD SERVE STILL HAS WHAT THE KERNEL COUNTED: a guest whose
	// adoption failed, or whose file was gone, is reported as a part of the story.
	listed := a.listed
	if a.rec == nil && h.hasDenyChain(ctx, a) {
		listed, a.rec = true, newRecorder(true, true)
	}

	var c counts
	if listed {
		c = h.readCounts(ctx, a)
	}

	if a.rec != nil {
		h.keep(key, a.rec.freeze(c))
	}

	if err := h.remove(ctx, a); err != nil {
		h.log.Warn("could not remove a guest's network rules; the next guest on its interface, or the "+
			"node's next recovery, removes them", "guest", key, "error", err)
		h.keepForLater(a)
	}
}

// remove deletes a guest's rules and its file, and frees its mark.
//
// WHETHER IT WAS LISTED IS THIS PROCESS'S TO SAY WHEN IT ATTACHED IT, and otherwise
// the kernel's: a detach script naming a set that is not there fails whole.
func (h *Host) remove(ctx context.Context, a *attached) error {
	listed := a.listed
	if a.rec == nil {
		listed = h.hasDenyChain(ctx, a)
	}

	script := detachScript(a.Key, a.Interface, a.Kind, a.mark, listed)

	if _, err := h.nft(ctx, script, "-f", "-"); err != nil {
		// ONE OBJECT MISSING FAILS THE WHOLE TRANSACTION, so rules someone else removed
		// would never come off, and every launch drawing their interface would be refused
		// for them. What is there goes one at a time, and what is not is what was asked.
		if !absent(err) {
			return fmt.Errorf("egress: remove the rules of %s: %w", a.Key, err)
		}

		if err := h.removeEachOf(ctx, script); err != nil {
			return fmt.Errorf("egress: remove the rules of %s: %w", a.Key, err)
		}
	}

	h.forgetFlows(a)
	h.freeMark(a.mark)
	h.removeFile(a.Key)

	return nil
}

// removeEachOf runs a removal a statement at a time, taking absence as done.
func (h *Host) removeEachOf(ctx context.Context, script string) error {
	for line := range strings.SplitSeq(strings.TrimSpace(script), "\n") {
		if _, err := h.nft(ctx, line+"\n", "-f", "-"); err != nil && !absent(err) {
			return err
		}
	}

	return nil
}

// forgetFlows deletes a guest's DNS connections, whose translation outlives the rule
// that made them. Failing to costs a guest holding one socket its DNS until the
// connection expires, and nothing else.
func (h *Host) forgetFlows(a *attached) {
	if err := h.forget(a.mark); err != nil {
		h.log.Warn("could not delete a guest's DNS connections; they expire on their own", "guest", a.Key,
			"error", err)
	}
}

// hasDenyChain reports whether a guest's rules are a list's. A listing that failed
// some other way than absence reads as listed, which is the reading that keeps a
// guest closed.
func (h *Host) hasDenyChain(ctx context.Context, a *attached) bool {
	family, name, _ := strings.Cut(a.Kind.table(), " ")
	_, err := h.nft(ctx, "", "list", "chain", family, name, "deny_"+a.Key)

	return !absent(err)
}

// readCounts reads what the kernel counted of a listed guest. A part that could
// not be read says so, and is never read as nothing refused.
func (h *Host) readCounts(ctx context.Context, a *attached) counts {
	family, name, _ := strings.Cut(a.Kind.table(), " ")

	var c counts

	read := func(prefix string, into *int64) {
		out, err := h.nft(ctx, "", "-j", "list", "counter", family, name, prefix+"_"+a.Key)
		if err == nil {
			*into, err = parseCounter(out)
		}

		if err != nil {
			c.unread = true

			h.log.Warn("could not read what a guest was refused", "guest", a.Key, "counter", prefix, "error", err)
		}
	}

	read("denied_c", &c.denied)
	read("outside_c", &c.outside)
	read("ipv6_c", &c.ipv6)

	out, err := h.nft(ctx, "", "-j", "list", "set", family, name, "denied_"+a.Key)
	if err == nil {
		c.destinations, err = parseDeniedSet(out)
	}

	if err != nil {
		c.unread = true

		h.log.Warn("could not read where a guest was refused", "guest", a.Key, "error", err)
	}

	// A FULL SET HAS STOPPED NOTICING, so a guest could hide its later destinations
	// behind decoys: the packets still count, and the list is a lower bound.
	if len(c.destinations) >= MaxDenied {
		c.unread = true
	}

	return c
}

func (h *Host) keep(key string, rec Record) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, held := h.done[key]; !held {
		h.order = append(h.order, key)
	}

	h.done[key] = rec

	for len(h.order) > maxKeptRecords {
		delete(h.done, h.order[0])
		h.order = h.order[1:]
	}
}

// Take is what a guest's rules and resolver saw, once, after its Detach. False is
// a guest with nothing to say: never attached, or already taken.
func (h *Host) Take(key string) (Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	rec, ok := h.done[key]
	if ok {
		h.dropLocked(key)
	}

	return rec, ok
}

// Forget drops what a guest's rules saw, for a guest no report is owed for.
func (h *Host) Forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.dropLocked(key)
}

func (h *Host) dropLocked(key string) {
	delete(h.done, key)

	if i := slices.Index(h.order, key); i >= 0 {
		h.order = slices.Delete(h.order, i, i+1)
	}
}

// Reconcile judges the rules an earlier process left, given every instance a
// List that succeeded just showed, keeping nothing this process attached in
// question: a launch can have attached since that List, and it is not this
// function's to undo. So it reads only what was found at the start, and a guest
// stays there until it has been judged: one that could not be served is tried
// again at the next recovery, staying closed until then.
//
//   - A guest no List shows is gone, and so are its rules, if it has a file here;
//     rules with none may be another deployment's, and wait for their name to be
//     reused or their owner.
//   - A running listed guest keeps its chain and sets, so it stays closed exactly
//     as it was, and gets a new resolver.
//   - A running open guest is handed back to dnsmasq: nothing says what it asked
//     before now, and its flows are not followed across a handover either.
//   - A running guest whose file cannot be read is judged by its rules. A list's
//     stay closed, and it is said to, because nothing can say what it may resolve
//     and opening it would be a guess. An open guest's are removed, as they would
//     have been.
func (h *Host) Reconcile(ctx context.Context, running map[string]bool) {
	if h.Ready() != nil {
		return
	}

	h.mu.Lock()
	found := slices.Collect(maps.Values(h.foundOnStart))
	firstJudged := !h.filesJudged
	h.filesJudged = true
	h.mu.Unlock()

	for _, a := range found {
		h.reconcileOne(ctx, a, running[a.Key])
	}

	if firstJudged {
		h.judgeFilesWithoutRules(ctx, running)
	}

	h.verifyLive(ctx)
}

// takeFound takes a guest off the books for judging it, unless a launch reusing its
// interface or its name already has, in which case it is that launch's.
func (h *Host) takeFound(a *attached) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.foundOnStart[a.Key] != a {
		return false
	}

	h.takeLocked(a.Key)

	return true
}

func (h *Host) reconcileOne(ctx context.Context, a *attached, running bool) {
	file, err := h.readFile(a.Key)

	switch {
	case !running && err == nil:
		if h.takeFound(a) {
			h.removeOrKeep(ctx, a)
		}
	case !running:
	case err != nil && h.hasDenyChain(ctx, a):
		h.log.Error("a running guest's network rules have no record of their policy, so it stays as "+
			"closed as they leave it, and resolves nothing; it runs until it ends", "guest", a.Key, "error", err)

		if h.takeFound(a) {
			h.mu.Lock()
			a.listed, a.rec = true, newRecorder(true, true)
			h.live[a.Key] = a
			h.byIface[a.Interface] = a.Key
			h.mu.Unlock()
		}
	case err == nil && file.Listed:
		h.adopt(ctx, a, file)
	default:
		if h.takeFound(a) {
			h.removeOrKeep(ctx, a)
		}
	}
}

func (h *Host) removeOrKeep(ctx context.Context, a *attached) {
	if err := h.remove(ctx, a); err != nil {
		h.log.Warn("could not remove the network rules an earlier process left", "guest", a.Key, "error", err)
		h.keepForLater(a)
	}
}

// adopt gives a running listed guest a new resolver, keeping its chain and sets.
// A guest it cannot serve stays closed, since its rules go on refusing.
func (h *Host) adopt(ctx context.Context, a *attached, file attachmentFile) {
	p, err := Compile(file.Allow)
	if err != nil {
		h.log.Error("a running guest's list cannot be read, so it stays closed and resolves nothing",
			"guest", a.Key, "error", err)

		return
	}

	gateway, err := h.addrOf(file.Network)
	if err != nil {
		h.log.Error("could not find a running guest's gateway, so it stays closed and resolves nothing",
			"guest", a.Key, "error", err)

		return
	}

	guest := &attached{
		Attachment: Attachment{Key: a.Key, Interface: a.Interface, Network: file.Network, Kind: a.Kind},
		mark:       a.mark, listed: true, policy: p, gateway: gateway,
		rec: newRecorder(true, true), admitted: map[netip.Addr]struct{}{},
	}

	guest.fwd, err = newForwarder(p, guest.rec, gateway, h.upstreamPort, h.log.With("guest", a.Key),
		func(ctx context.Context, addrs []netip.Addr) error { return h.admit(ctx, guest, addrs) })
	if err != nil {
		h.log.Error("could not bind a running guest's resolver, so it stays closed and resolves nothing",
			"guest", a.Key, "error", err)

		return
	}

	if _, err := h.nft(ctx, retargetScript(a.Key, a.mark, guest.fwd.port), "-f", "-"); err != nil {
		guest.fwd.stop()
		h.log.Error("could not point a running guest at its resolver, so it stays closed and resolves nothing",
			"guest", a.Key, "error", err)

		return
	}

	guest.fwd.serve(ctx)
	h.forgetFlows(guest)

	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.foundOnStart, a.Key)
	h.live[a.Key] = guest
	h.byIface[a.Interface] = a.Key
	h.log.Info("resolving for a listed guest an earlier process launched", "guest", a.Key)
}

// HandOver hands every open guest's DNS back to dnsmasq, for a process leaving
// them running: until the next process has a resolver, there is nothing at the
// other end of their redirect. A listed guest stays closed, and resolves nothing
// until then.
func (h *Host) HandOver(ctx context.Context) {
	h.mu.Lock()

	var open []*attached

	for _, a := range h.live {
		if !a.listed {
			open = append(open, a)
		}
	}

	for _, a := range open {
		h.takeLocked(a.Key)
	}

	h.mu.Unlock()

	for _, a := range open {
		if a.fwd != nil {
			a.fwd.stop()
		}

		h.removeOrKeep(ctx, a)
	}
}

// attachmentFile is what a process keeps of a guest for the next: what the kernel
// cannot say, and what putting a lost guest's rules back needs when the kernel has
// nothing to say.
type attachmentFile struct {
	Schema    int      `json:"schema"`
	Key       string   `json:"key"`
	Interface string   `json:"interface"`
	Kind      Kind     `json:"kind"`
	Network   string   `json:"network"`
	Listed    bool     `json:"listed"`
	Allow     []string `json:"allow,omitempty"`
}

func (h *Host) writeFile(a Attachment, p *Policy) error {
	data, err := json.Marshal(attachmentFile{
		Schema: recordSchema, Key: a.Key, Interface: a.Interface, Kind: a.Kind, Network: a.Network,
		Listed: p.Listed(), Allow: p.allow,
	})
	if err != nil {
		return fmt.Errorf("egress: encode the policy of %s: %w", a.Key, err)
	}

	if err := h.files.MkdirAll(h.stateDir, 0o700); err != nil {
		return fmt.Errorf("egress: create %s: %w", h.stateDir, err)
	}

	if _, err := h.files.Install(h.stateDir, a.Key+recordSuffix, 0o600, func(w io.Writer) error {
		_, err := w.Write(data)

		return err
	}); err != nil {
		return fmt.Errorf("egress: keep the policy of %s: %w", a.Key, err)
	}

	return nil
}

func (h *Host) readFile(key string) (attachmentFile, error) {
	if !keyRe.MatchString(key) {
		return attachmentFile{}, fmt.Errorf("egress: %q is not a guest's key", key)
	}

	data, err := regularfile.ReadFile(filepath.Join(h.stateDir, key+recordSuffix), maxRecordBytes,
		regularfile.Options{NoFollow: true})
	if err != nil {
		return attachmentFile{}, err
	}

	var file attachmentFile
	if err := json.Unmarshal(data, &file); err != nil {
		return attachmentFile{}, fmt.Errorf("egress: read the policy of %s: %w", key, err)
	}

	if file.Schema != recordSchema || file.Key != key {
		return attachmentFile{}, fmt.Errorf("egress: the policy file of %s is schema %d for %q", key, file.Schema,
			file.Key)
	}

	return file, nil
}

func (h *Host) removeFile(key string) {
	err := os.Remove(filepath.Join(h.stateDir, key+recordSuffix))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.log.Warn("could not remove a guest's policy file", "guest", key, "error", err)
	}
}

// Close stops every resolver, for a process leaving. Rules stay: a listed guest
// left running stays closed, and the next process's Reconcile finds it.
func (h *Host) Close() {
	h.mu.Lock()

	guests := slices.Collect(maps.Values(h.live))
	h.mu.Unlock()

	for _, a := range guests {
		if a.fwd != nil {
			a.fwd.stop()
		}
	}
}
