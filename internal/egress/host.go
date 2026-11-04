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
	// gateway is what its rules were made from, for putting them back.
	gateway netip.Prefix
	// judged says listed is the kernel's answer for rules this process did not make, kept
	// because the kernel stops being able to give it as they are removed.
	judged bool

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

	// lifecycle is held across the whole of an attach, a detach, a recovery and a handover,
	// which all change a guest's rules and then its books. A sweep judges guests while
	// launches attach and fail, so without it a file can be seen before its rules, and rules
	// put back after the detach that removed them with nobody left to take them off. Taken
	// before mu and nftMu. A guest's queries take neither it nor mu around nft, so stopping a
	// resolver under it waits on nothing that waits on it.
	lifecycle sync.Mutex

	mu       sync.Mutex
	notReady error
	// lastReason is the last reason logged for not being ready: a host that stays so asks
	// again at every recovery, and says so once.
	lastReason string
	// scanned says what earlier processes left has been read from the kernel.
	scanned bool
	warned  bool
	live    map[string]*attached
	// foundOnStart are the guests earlier processes left rules for, until
	// Reconcile has judged them.
	foundOnStart map[string]*attached
	// unrestored are running guests that may have a list and whose rules could not be put
	// back, tried again at each recovery, with the mark held for each (0 for none yet).
	// Their files stay: they are all that says so.
	unrestored map[string]uint32
	// orphanMarks are marks the kernel holds for a guest no interface dispatches to, which a
	// restore of that guest takes back: its chains are referenced under them, and could not
	// be deleted otherwise.
	orphanMarks map[string]uint32
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
		slots: map[uint32]struct{}{}, done: map[string]Record{}, unrestored: map[string]uint32{}, orphanMarks: map[string]uint32{},
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

		if reason := err.Error(); reason != h.lastReason {
			h.lastReason = reason
			h.log.Warn("a job on this node cannot have a network allowlist, or its names recorded", "reason", err)
		}

		return
	}

	h.lastReason = ""
	h.log.Info("this node can enforce a network allowlist and record the names its guests resolve")
}

// prepare finds what earlier processes left whether or not this host can attach anything
// new. A FLUSHED FIREWALL TAKES THE ROLE'S GUARD WITH IT, and a running guest's list is not
// the guard's to withhold: Reconcile puts its rules back either way.
func (h *Host) prepare(ctx context.Context) error {
	checked := checkHost(ctx, h.nft)

	if err := h.findExisting(ctx); err != nil && checked == nil {
		return err
	}

	if checked != nil {
		return checked
	}

	if err := h.ensureTables(ctx); err != nil {
		return fmt.Errorf("could not create this node's firewall tables: %w", err)
	}

	return nil
}

// Check answers whether this host could enforce a policy, changing nothing: nil, or
// why not, which includes not being able to tell.
func Check(ctx context.Context) error {
	return checkHost(ctx, runNft)
}

// checkHost asks the host, changing nothing.
//
// THE KERNEL IS ASKED, not its version compared: a dry run of a whole listed guest
// fails on a kernel or an nft without any part of it.
func checkHost(ctx context.Context, run runFunc) error {
	out, err := run(ctx, "", "-j", "list", "chain", "inet", "billet_guard", "input")
	if err != nil {
		if absent(err) {
			return errors.New("the host role's guest firewall is not installed (no chain input in " +
				"table inet billet_guard); converge the host")
		}

		return fmt.Errorf("could not read the host role's guest firewall: %w", err)
	}

	if !strings.Contains(string(out), GuardRuleComment) {
		return fmt.Errorf("the host role's guest firewall has no rule %q, so redirected DNS would be "+
			"dropped; converge the host with the collection from this release", GuardRuleComment)
	}

	script := baseScript() + attachScript(binding{
		key: "readiness", iface: "readiness0", kind: BridgePort, gateway: netip.MustParsePrefix("192.0.2.1/24"),
		mark: markBase, port: 1, listed: true, prefixes: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	})

	if _, err := run(ctx, script, "-c", "-f", "-"); err != nil {
		return fmt.Errorf("this kernel or nft refuses a guest's rules: %w", err)
	}

	return nil
}

// nft runs one nft command, one at a time.
func (h *Host) nft(ctx context.Context, script string, args ...string) ([]byte, error) {
	h.nftMu.Lock()
	defer h.nftMu.Unlock()

	return h.run(ctx, script, args...)
}

// findExisting reads who holds which interface and mark, from the kernel. A map that is
// not there holds nothing, which is the flushed host Reconcile is there to find.
//
// A GUEST'S MARK IS LOOKED FOR IN EVERY MAP THAT KEYS ON ONE. A guest found in none has
// none, and stays at zero, which nothing renders or deletes by (validMark).
func (h *Host) findExisting(ctx context.Context) error {
	marks := map[string]uint32{}

	for _, source := range []struct{ family, table, name, prefix string }{
		{"ip", "billet_egress_nat", "jobs", "dns_"},
		{"ip", "billet_egress_nat", "owners", "own_"},
		{"bridge", "billet_egress", "replies", "reply_"},
		{"inet", "billet_egress_routed", "replies", "reply_"},
	} {
		elements, err := h.listMap(ctx, source.family, source.table, source.name)
		if err != nil {
			return err
		}

		for _, e := range elements {
			var mark uint32
			if err := json.Unmarshal(e.key, &mark); err != nil {
				return fmt.Errorf("egress: a mark in nft's map: %w", err)
			}

			key := strings.TrimPrefix(e.target, source.prefix)
			if _, seen := marks[key]; !seen && validMark(mark) {
				marks[key] = mark
			}
		}
	}

	found := map[string]*attached{}

	for _, kind := range []Kind{BridgePort, Routed} {
		family, name, _ := strings.Cut(kind.table(), " ")

		elements, err := h.listMap(ctx, family, name, "guests")
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
	defer h.mu.Unlock()

	h.scanned = true

	for key, mark := range marks {
		h.slots[mark] = struct{}{}

		_, isFound := found[key]
		if _, live := h.live[key]; !isFound && !live {
			h.orphanMarks[key] = mark
		}
	}

	for key, a := range found {
		// A HOST THAT WAS NOT READY ASKS AGAIN, and what it has restored since is its own.
		if _, live := h.live[key]; live {
			continue
		}

		h.foundOnStart[key] = a
		h.byIface[a.Interface] = key
	}

	return nil
}

// listMap is a verdict map's elements, none if it is not there.
func (h *Host) listMap(ctx context.Context, family, table, name string) ([]mapElement, error) {
	out, err := h.nft(ctx, "", "-j", "list", "map", family, table, name)
	if err != nil {
		if absent(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("could not read this node's firewall tables: %w", err)
	}

	return parseVerdictMap(out)
}

// judgeFilesWithoutRules reads the files no rule stands behind, given what is running. None
// is a launch's on its way to its rules: an attach holds the lifecycle lock from its file
// to its rules, and so does this.
func (h *Host) judgeFilesWithoutRules(ctx context.Context, running map[string]bool) {
	entries, err := os.ReadDir(h.stateDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.log.Warn("could not look for guest policy files with no rules", "dir", h.stateDir, "error", err)
	}

	for _, e := range entries {
		if key, ours := strings.CutSuffix(e.Name(), recordSuffix); ours && keyRe.MatchString(key) {
			h.judgeFile(ctx, key, running[key])
		}
	}
}

// judgeFile settles one file with no rules behind it.
//
// A FILE FOR A GUEST THAT IS NOT RUNNING, OR AN OPEN ONE, IS NOTHING: a process died
// between writing it and installing the rules. A LISTED GUEST THAT IS RUNNING HAS LOST
// ITS RULES, and is running open: the rules go back, and what they count from here is
// marked as not the whole job. A FILE THAT CANNOT BE READ OF A GUEST THAT IS RUNNING IS
// NEITHER: nothing can say it is not a list, so it stays and is read again.
func (h *Host) judgeFile(ctx context.Context, key string, running bool) {
	h.mu.Lock()
	_, live := h.live[key]
	_, found := h.foundOnStart[key]
	h.mu.Unlock()

	if live || found {
		h.mu.Lock()
		delete(h.unrestored, key)
		h.mu.Unlock()

		return
	}

	settled := true

	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		if settled {
			delete(h.unrestored, key)
		} else if _, held := h.unrestored[key]; !held {
			h.unrestored[key] = 0
		}
	}()

	if !running {
		h.settleGone(ctx, key)

		return
	}

	file, err := h.readFile(key)

	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		h.log.Error("could not read the policy file of a running guest that has no network rules, so it "+
			"may be a list and running open; it is read again at the next recovery", "guest", key, "error", err)

		settled = false
	case !file.Listed:
		h.removeFile(key)
	default:
		settled = h.restoreRules(ctx, file)
	}
}

// settleGone clears what a guest that has gone left behind with no interface dispatching to
// it: what the kernel still holds under its mark, which would refuse the next guest drawing
// its interface, and its mark and its file.
func (h *Host) settleGone(ctx context.Context, key string) {
	h.mu.Lock()
	reserved := h.unrestored[key]
	orphan, leftover := h.orphanMarks[key]
	delete(h.orphanMarks, key)
	h.mu.Unlock()

	if leftover {
		file, err := h.readFile(key)
		if err == nil {
			h.removeOrKeep(ctx, &attached{Attachment: Attachment{Key: key, Interface: file.Interface,
				Network: file.Network, Kind: file.Kind}, mark: orphan})

			return
		}

		h.log.Warn("could not read the policy file of a guest that has gone, so what the kernel holds of it "+
			"stays", "guest", key, "error", err)
	}

	h.freeMark(reserved)
	h.removeFile(key)
}

// restoreRules puts a running listed guest's rules back from its file, answering whether
// it could.
//
// ITS OWN MARK, IF IT CAN STILL HAVE IT, and held while this is retried: its connections
// went on carrying it through whatever took the rules, and what is left of them is keyed on
// it. Those connections go first, since their translations outlived the rules and a query
// on one would go to a port its old resolver held. One whose mark another guest has since
// been given takes a new one, and its old connections expire by themselves, rather than
// delete that guest's.
func (h *Host) restoreRules(ctx context.Context, file attachmentFile) bool {
	h.log.Error("a running guest with a network allowlist has lost its rules, so it has been running "+
		"open; they are being put back. Something other than billet removed them: `nft flush ruleset`, "+
		"or a firewall service that does", "guest", file.Key)

	mark, err := h.reserveMark(file.Key, file.Mark)
	if err != nil {
		h.log.Error("could not put a running guest's network rules back, so it is still running open; "+
			"this is tried again at the next recovery", "guest", file.Key, "error", err)

		return false
	}

	if err := h.forget(mark); err != nil {
		h.log.Error("could not delete a guest's DNS connections, so a socket it holds may reach whatever "+
			"now listens on its old resolver's port until they expire", "guest", file.Key, "error", err)
	}

	p, err := Compile(file.Allow)
	if err == nil {
		err = h.attach(ctx, Attachment{Key: file.Key, Interface: file.Interface, Network: file.Network,
			Kind: file.Kind}, p, mark)
	}

	if err != nil {
		h.log.Error("could not put a running guest's network rules back, so it is still running open; "+
			"this is tried again at the next recovery", "guest", file.Key, "error", err)

		return false
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.orphanMarks, file.Key)

	if a := h.live[file.Key]; a != nil {
		a.rec.incomplete()
	}

	return true
}

// reserveMark holds a mark for restoring a guest: the one already held for it, the one the
// kernel still has for it, the one its file names if that is free, or a new one.
func (h *Host) reserveMark(key string, want uint32) (uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if mark := h.unrestored[key]; validMark(mark) {
		return mark, nil
	}

	mark := h.orphanMarks[key]
	if !validMark(mark) {
		mark = want

		if _, taken := h.slots[mark]; taken || !validMark(mark) {
			var err error
			if mark, err = h.allocMarkLocked(); err != nil {
				return 0, err
			}
		}
	}

	h.slots[mark] = struct{}{}
	h.unrestored[key] = mark

	return mark, nil
}

// ensureTables makes billet's tables what they should be.
func (h *Host) ensureTables(ctx context.Context) error {
	_, err := h.nft(ctx, baseScript(), "-f", "-")

	return err
}

// verify checks that this process's listed guests still have their rules in force, and
// puts them back if not. The caller holds the lifecycle lock.
func (h *Host) verify(ctx context.Context) {
	h.mu.Lock()
	guests := slices.DeleteFunc(slices.Collect(maps.Values(h.live)), func(a *attached) bool { return !a.listed })
	h.mu.Unlock()

	if len(guests) == 0 {
		return
	}

	held, err := h.readRules(ctx)
	if err == nil {
		var local []netip.Addr
		if local, err = h.locals(); err == nil {
			for _, a := range guests {
				switch {
				case a.fwd == nil:
					if !held.keepsOff(a.Key, a.Interface, a.Kind, a.mark) {
						h.closeOff(ctx, a)
					}
				case !held.enforces(a.binding(local)):
					h.reinstall(ctx, a)
				}
			}

			return
		}
	}

	h.log.Warn("could not read this node's firewall rules, so whether its guests' lists are in force "+
		"is not known; this is asked again at the next sweep", "error", err)
}

// binding is what a served guest's rules are made from.
func (a *attached) binding(local []netip.Addr) binding {
	return binding{
		key: a.Key, iface: a.Interface, kind: a.Kind, gateway: a.gateway, mark: a.mark, port: a.fwd.port,
		listed: a.listed, prefixes: a.policy.prefixes, local: local,
	}
}

// closeOff gives a guest whose policy nothing can read, and whose rules have gone, nothing.
func (h *Host) closeOff(ctx context.Context, a *attached) {
	h.log.Error("a running guest with a network allowlist nobody can read has lost its rules, so it has "+
		"been running open; it now reaches nothing. Something other than billet removed them: `nft flush "+
		"ruleset`, or a firewall service that does", "guest", a.Key)

	a.rec.incomplete()

	if !h.ensureMark(a) {
		return
	}

	if _, err := h.nft(ctx, baseScript()+closedScript(a.Key, a.Interface, a.Kind)+quarantineScript(a.Key, a.mark),
		"-f", "-"); err != nil {
		h.log.Error("could not close off a running guest, so it is still running open", "guest", a.Key,
			"error", err)
	}
}

// ensureMark gives a guest with no mark one, answering whether it has one.
func (h *Host) ensureMark(a *attached) bool {
	if validMark(a.mark) {
		return true
	}

	mark, err := h.reserveMark(a.Key, 0)
	if err != nil {
		h.log.Error("could not take a mark for a running guest", "guest", a.Key, "error", err)

		return false
	}

	a.mark = mark

	return true
}

// readRules lists the three tables. One that is not there holds nothing.
func (h *Host) readRules(ctx context.Context) (heldRules, error) {
	read := func(table string) (ruleTable, error) {
		family, name, _ := strings.Cut(table, " ")

		out, err := h.nft(ctx, "", "-j", "list", "table", family, name)
		if absent(err) {
			return parseRuleTable([]byte(`{"nftables":[]}`))
		}

		if err != nil {
			return ruleTable{}, err
		}

		return parseRuleTable(out)
	}

	var (
		held heldRules
		err  error
	)

	if held.bridge, err = read(BridgePort.table()); err != nil {
		return held, err
	}

	if held.routed, err = read(Routed.table()); err != nil {
		return held, err
	}

	held.nat, err = read(natTable)

	return held, err
}

// reinstall puts a live listed guest's rules back, atomically, over whatever is left of
// them. A set that went with them is empty again, so what its names admitted must be asked
// for again, which closes rather than opens. THE JOB'S REPORT SAYS SO FIRST: this has found
// it ran open, whether or not the repair works.
func (h *Host) reinstall(ctx context.Context, a *attached) {
	h.log.Error("a running guest with a network allowlist has lost its rules, so it has been running "+
		"open; they are being put back. Something other than billet removed them: `nft flush ruleset`, "+
		"or a firewall service that does", "guest", a.Key)

	a.rec.incomplete()

	// READ NOW: a guest on the other bridge reaches the node's cache across the host, on an
	// address that may have changed since it was launched.
	local, err := h.locals()
	if err == nil {
		_, err = h.nft(ctx, baseScript()+attachScript(a.binding(local)), "-f", "-")
	}

	if err != nil {
		h.log.Error("could not put a running guest's network rules back, so it is still running open",
			"guest", a.Key, "error", err)

		return
	}

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
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

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

	err := h.attach(ctx, a, p, 0)
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

	return h.allocMarkLocked()
}

func (h *Host) allocMarkLocked() (uint32, error) {
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
// policy of. RESTORING, with a mark already held for it, the file was there first and
// both are kept whatever happens: the guest is running, and they are all that say it has
// a list.
//
// THE BASE CHAINS GO IN WITH THE GUEST'S, in one transaction: a chain that nothing jumps
// to because something emptied them is a list that was not applied.
func (h *Host) attach(ctx context.Context, a Attachment, p *Policy, reserved uint32) error {
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

	restoring, mark := reserved != 0, reserved
	if !restoring {
		if mark, err = h.allocMark(); err != nil {
			return err
		}
	}

	// KEPT WITH THE RULES when they may be installed: the mark and the file are how
	// whoever clears them later names them.
	done, uncertain := false, false

	defer func() {
		if !done && !uncertain && !restoring {
			h.freeMark(mark)
		}
	}()

	guest := &attached{
		Attachment: a, mark: mark, listed: p.Listed(), policy: p, gateway: gateway,
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

	if err := h.writeFile(a, p, mark); err != nil {
		return err
	}

	defer func() {
		if !done && !uncertain && !restoring {
			h.removeFile(a.Key)
		}
	}()

	if _, err := h.nft(ctx, baseScript()+attachScript(guest.binding(local)), "-f", "-"); err != nil {
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
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	a := h.takeLocked(key)
	_, unrestored := h.unrestored[key]
	h.mu.Unlock()

	if a == nil {
		// RULES THAT NEVER CAME BACK ARE STILL A JOB WITH A LIST: its report says it does not
		// have the whole story, rather than saying nothing was watching.
		if unrestored {
			h.keep(key, newRecorder(true, true).freeze(counts{unread: true}))
			h.settleGone(ctx, key)

			h.mu.Lock()
			delete(h.unrestored, key)
			h.mu.Unlock()
		}

		return
	}

	if a.fwd != nil {
		a.fwd.stop()
	}

	// A LIST NOTHING COULD SERVE STILL HAS WHAT THE KERNEL COUNTED: a guest whose
	// adoption failed, or whose file was gone, is reported as a part of the story.
	listed := h.listedOf(ctx, a)
	if a.rec == nil && listed {
		a.rec = newRecorder(true, true)
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

// listedOf says whether a guest's rules are a list's: this process's to say when it
// attached them, and otherwise the kernel's, asked once, since a removal that stopped
// halfway has taken away what it would answer from.
func (h *Host) listedOf(ctx context.Context, a *attached) bool {
	if a.rec == nil && !a.judged {
		a.listed, a.judged = h.hasDenyChain(ctx, a), true
	}

	return a.listed
}

// remove deletes a guest's rules and its file, and frees its mark.
func (h *Host) remove(ctx context.Context, a *attached) error {
	script := detachScript(a.Key, a.Interface, a.Kind, a.mark, h.listedOf(ctx, a))

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
// that made them. Failing to leaves a socket the guest holds reaching its old resolver's
// port until the connection expires, which the kernel can have given another guest's.
func (h *Host) forgetFlows(a *attached) {
	// ZERO IS EVERY CONNECTION NOBODY MARKED, and a guest found with no mark has none to forget.
	if !validMark(a.mark) {
		return
	}

	if err := h.forget(a.mark); err != nil {
		h.log.Error("could not delete a guest's DNS connections, so a socket it holds may reach whatever now "+
			"listens on its old resolver's port until they expire", "guest", a.Key, "error", err)
	}
}

// quarantine sends a guest's DNS nowhere, for a guest this process cannot serve. A guest
// whose mark nothing could say has its chain closed too, since that chain marks its DNS with
// a mark no redirect now names, and it would reach dnsmasq.
func (h *Host) quarantine(ctx context.Context, a *attached) {
	script := baseScript()

	if !validMark(a.mark) {
		script += closedScript(a.Key, a.Interface, a.Kind)

		if !h.ensureMark(a) {
			return
		}
	}

	if _, err := h.nft(ctx, script+quarantineScript(a.Key, a.mark), "-f", "-"); err != nil {
		h.log.Error("could not stop a guest's DNS going to a resolver this process does not hold, so if the "+
			"kernel gives that port to another guest's resolver it would answer this guest", "guest", a.Key,
			"error", err)
	}

	h.forgetFlows(a)
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
//   - A running listed guest keeps its sets and its mark, and gets a new resolver: its
//     rules go in again over whatever is left of them, in one transaction, so it does
//     not matter what is.
//   - A running open guest is handed back to dnsmasq: nothing says what it asked
//     before now, and its flows are not followed across a handover either.
//   - A running guest whose file cannot be read is judged by its rules. A list's
//     stay closed, and it is said to, because nothing can say what it may resolve
//     and opening it would be a guess. With no deny chain it may be an open guest or
//     a list that lost its rules, so the file is kept and read again. With no file at
//     all, an open guest's are removed, as they would have been.
func (h *Host) Reconcile(ctx context.Context, running map[string]bool) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	// A CONVERGE CAN HAVE PUT RIGHT WHAT WAS WRONG, with no process starting to notice.
	if h.Ready() != nil {
		h.Prepare(ctx)
	}

	h.mu.Lock()
	scanned := h.scanned
	found := slices.Collect(maps.Values(h.foundOnStart))
	h.mu.Unlock()

	if !scanned {
		return
	}

	for _, a := range found {
		h.reconcileOne(ctx, a, running[a.Key])
	}

	h.judgeFilesWithoutRules(ctx, running)
	h.verify(ctx)
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
	case err == nil && file.Listed:
		h.adopt(ctx, a, file)
	case err == nil:
		if h.takeFound(a) {
			h.removeOrKeep(ctx, a)
		}
	case h.hasDenyChain(ctx, a):
		h.log.Error("a running guest's network rules have no record of their policy, so it stays as "+
			"closed as they leave it, and resolves nothing; it runs until it ends", "guest", a.Key, "error", err)

		if h.takeFound(a) {
			h.mu.Lock()
			a.listed, a.rec = true, newRecorder(true, true)
			h.live[a.Key] = a
			h.byIface[a.Interface] = a.Key
			h.mu.Unlock()

			h.quarantine(ctx, a)
		}
	case errors.Is(err, fs.ErrNotExist):
		if h.takeFound(a) {
			h.removeOrKeep(ctx, a)
		}
	default:
		// STILL ON THE BOOKS, so a detach or the next guest on its interface can take its rules
		// off. ITS DNS GOES NOWHERE meanwhile: its redirect names a port this process does not
		// hold, whichever it turns out to be.
		h.log.Error("could not read the policy file of a running guest that has no deny chain, so it may be "+
			"open or a list that lost its rules; its DNS goes nowhere, and the file is read again at the next "+
			"recovery", "guest", a.Key, "error", err)
		h.quarantine(ctx, a)
	}
}

func (h *Host) removeOrKeep(ctx context.Context, a *attached) {
	if err := h.remove(ctx, a); err != nil {
		h.log.Warn("could not remove the network rules an earlier process left", "guest", a.Key, "error", err)
		h.keepForLater(a)
	}
}

// adopt gives a running listed guest a new resolver, and its rules again over whatever is
// left of them, in one transaction: a set keeps what it holds, so what its names admitted
// stays admitted, and what has gone comes back. A guest it cannot serve has its DNS sent
// nowhere, and is tried again at the next recovery.
func (h *Host) adopt(ctx context.Context, a *attached, file attachmentFile) {
	// A NEW MARK IS NOT THE GUEST'S UNTIL ITS RULES COMMIT: what is left of them marks DNS with
	// another, which a quarantine under this one would not stop, so until then a stays without.
	mark := a.mark
	if !validMark(mark) {
		var err error
		if mark, err = h.reserveMark(a.Key, file.Mark); err != nil {
			h.log.Error("could not take a mark for a running guest, so it stays as closed as its rules leave it",
				"guest", a.Key, "error", err)

			return
		}
	}

	fail := func(what string, err error) {
		h.log.Error("could not "+what+" a running guest, so its DNS goes nowhere and it resolves nothing",
			"guest", a.Key, "error", err)
		h.quarantine(ctx, a)
	}

	p, err := Compile(file.Allow)
	if err != nil {
		fail("read the list of", err)

		return
	}

	gateway, err := h.addrOf(file.Network)
	if err != nil {
		fail("find the gateway of", err)

		return
	}

	local, err := h.locals()
	if err != nil {
		fail("read the host's addresses for", err)

		return
	}

	guest := &attached{
		Attachment: Attachment{Key: a.Key, Interface: a.Interface, Network: file.Network, Kind: a.Kind},
		mark:       mark, listed: true, policy: p, gateway: gateway,
		rec: newRecorder(true, true), admitted: map[netip.Addr]struct{}{},
	}

	guest.fwd, err = newForwarder(p, guest.rec, gateway, h.upstreamPort, h.log.With("guest", a.Key),
		func(ctx context.Context, addrs []netip.Addr) error { return h.admit(ctx, guest, addrs) })
	if err != nil {
		fail("bind a resolver for", err)

		return
	}

	// THE FILE BEFORE THE RULES, as an attach: what its connections carry is what it names.
	if err := h.writeFile(guest.Attachment, p, mark); err != nil {
		guest.fwd.stop()
		fail("keep the policy of", err)

		return
	}

	if _, err := h.nft(ctx, baseScript()+attachScript(guest.binding(local)), "-f", "-"); err != nil {
		guest.fwd.stop()
		fail("install the rules of", err)

		return
	}

	guest.fwd.serve(ctx)
	h.forgetFlows(guest)

	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.foundOnStart, a.Key)
	delete(h.unrestored, a.Key)
	delete(h.orphanMarks, a.Key)
	h.live[a.Key] = guest
	h.byIface[a.Interface] = a.Key
	h.log.Info("resolving for a listed guest an earlier process launched", "guest", a.Key)
}

// HandOver hands every open guest's DNS back to dnsmasq, for a process leaving
// them running: until the next process has a resolver, there is nothing at the
// other end of their redirect. A listed guest stays closed, and resolves nothing
// until then.
func (h *Host) HandOver(ctx context.Context) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

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
	// Mark is what its connections carry, which the kernel's tracker keeps after a flush
	// has taken the rules that would have said so.
	Mark uint32 `json:"mark,omitempty"`
}

func (h *Host) writeFile(a Attachment, p *Policy, mark uint32) error {
	data, err := json.Marshal(attachmentFile{
		Schema: recordSchema, Key: a.Key, Interface: a.Interface, Kind: a.Kind, Network: a.Network,
		Listed: p.Listed(), Allow: p.allow, Mark: mark,
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
