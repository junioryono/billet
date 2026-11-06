package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// A macOS guest holds a slot for exactly as long as its process lives (ADR-021).
//
// A slot is a lock on a file of the store's, taken by billet and passed to the `tart
// run` it starts, which inherits the open file description. billet closes its own copy,
// so the kernel keeps the slot held through billet's death or restart and releases it
// when the guest's process and anything it spawned have ended. No elapsed time ever
// releases one. That the descriptor survives `tart run` for the VM's life is measured
// on the reference Mac before this is relied on.
//
// Every Linux guest billet starts passes a token the same way, which is how the census
// tells one from a macOS guest no slot accounts for.
const (
	slotPrefix    = "macos-slot-"
	slotsLock     = "slots.lock"
	slotsCount    = "macos-slots"
	limitPrefix   = "macos-limit-"
	tokenPrefix   = "guest-token-"
	yieldPrefix   = "macos-yield-"
	guestDarwin   = "darwin"
	guestLinux    = "linux"
	slotsLockHold = 30 * time.Second
)

// yieldWait is how long a launch waits for a probe to give up its slot, and how old a
// request a probe still honours.
const yieldWait = 2 * time.Minute

// yieldRetry is how often a waiting launch asks again.
const yieldRetry = 2 * time.Second

// ErrNoSlot is a start the limit refuses, as distinct from one billet could not judge.
// To a verification it is not a verdict on the image: the next one runs it again.
var ErrNoSlot = errors.New("no macOS guest slot is free")

// guestClaim is what a guest's start holds until it has passed it to `tart run`.
type guestClaim struct {
	files []*os.File
}

// release closes billet's copies. Once `tart run` has inherited them, the lock stays
// held there; before that, this frees it.
func (c *guestClaim) release() {
	for _, f := range c.files {
		_ = f.Close()
	}

	c.files = nil
}

func (p *Provider) billetPath(name string) string {
	return filepath.Join(p.home, storeStateDir, name)
}

// guestOSOf reads which operating system a VM runs from its config.json. A config with
// no `os` is darwin, as tart's own decoder reads one. Anything else refuses, since the
// answer decides whether Apple's limit applies.
func (p *Provider) guestOSOf(name string) (string, error) {
	raw, err := regularfile.ReadFile(filepath.Join(p.vmDir(name), "config.json"), maxVMConfig,
		regularfile.Options{NoFollow: true})
	if err != nil {
		return "", fmt.Errorf("read %s's configuration for its operating system: %w", name, err)
	}

	var cfg struct {
		OS *string `json:"os"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("read %s's configuration for its operating system: %w", name, err)
	}

	switch {
	case cfg.OS == nil, *cfg.OS == guestDarwin:
		return guestDarwin, nil
	case *cfg.OS == guestLinux:
		return guestLinux, nil
	default:
		return "", fmt.Errorf("%s's configuration names an operating system billet does not know", name)
	}
}

// claimGuest takes what a guest needs before its `tart run` starts. name is the name
// the VM's process runs under. A job's launch may wait for a probe to yield; a probe may
// not make anything yield.
func (p *Provider) claimGuest(ctx context.Context, name, guestOS string, isProbe bool) (*guestClaim, error) {
	if guestOS == guestLinux {
		return p.claimLinuxToken(ctx, name)
	}

	limit, err := p.macOSLimitFor(isProbe)
	if err != nil {
		return nil, err
	}

	if isProbe {
		f, _, err := p.admitMacOS(ctx, name, limit)
		if err != nil {
			return nil, err
		}

		return &guestClaim{files: []*os.File{f}}, nil
	}

	return p.admitLaunch(ctx, name, limit)
}

// macOSLimitFor is the node's limit. With no record, a probe refuses, and a launch,
// which the control plane sends only to a registered node, uses Apple's default. A record
// that cannot be read refuses both: it is not the absence of one, and the number a start
// passes on becomes the store's count.
func (p *Provider) macOSLimitFor(isProbe bool) (int, error) {
	if p.macOSLimit != nil {
		limit, known, err := p.macOSLimit()
		if err != nil {
			return 0, fmt.Errorf("read this node's macOS limit: %w", err)
		}

		if known {
			return limit, nil
		}
	}

	if isProbe {
		return 0, errors.New("this node has no record of its macOS limit, which its control plane " +
			"sends at registration, so a macOS probe cannot be counted against it; start the node " +
			"so it registers, and run this again, or upgrade a control plane older than wire 41, " +
			"which does not send it")
	}

	return config.DefaultMacOSVMLimit, nil
}

// claimLinuxToken takes a Linux guest's token, under the slot lock that makes creating
// one and sweeping stale ones exclude each other.
func (p *Provider) claimLinuxToken(ctx context.Context, name string) (*guestClaim, error) {
	held, err := lockPath(ctx, p.billetPath(slotsLock), syscall.LOCK_EX, slotsLockHold)
	if err != nil {
		return nil, fmt.Errorf("lock the guest slots: %w", err)
	}

	defer held.release()

	p.sweepTokens()

	f, err := openLockFile(p.billetPath(tokenPrefix + name))
	if err != nil {
		return nil, fmt.Errorf("create the guest's token: %w", err)
	}

	got, err := tryFlock(f, syscall.LOCK_EX)
	if err == nil && !got {
		err = errors.New("another process holds it")
	}

	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("take the guest's token: %w", err)
	}

	return &guestClaim{files: []*os.File{f}}, nil
}

// sweepTokens removes the tokens of guests whose processes have ended. The caller holds
// the slot lock. Best effort: a token left behind costs a small file.
func (p *Provider) sweepTokens() {
	names, err := filepath.Glob(p.billetPath(tokenPrefix + "*"))
	if err != nil {
		return
	}

	for _, path := range names {
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}

		if free, err := tryFlock(f, syscall.LOCK_EX); err == nil && free {
			_ = os.Remove(path)
		}

		_ = f.Close()
	}
}

// admitLaunch takes a slot for a job, waiting while a probe gives one up. It removes
// its yield request on every way out.
func (p *Provider) admitLaunch(ctx context.Context, name string, limit int) (*guestClaim, error) {
	request := p.billetPath(yieldPrefix + name)
	defer func() { _ = os.Remove(request) }()

	waitCtx, cancel := context.WithTimeout(ctx, p.yieldWait)
	defer cancel()

	retry := time.NewTimer(p.yieldRetry)
	defer retry.Stop()

	for {
		f, probes, err := p.admitMacOS(ctx, name, limit)
		if err == nil {
			return &guestClaim{files: []*os.File{f}}, nil
		}

		if !errors.Is(err, ErrNoSlot) || len(probes) == 0 {
			return nil, err
		}

		asked := false

		for _, probe := range probes {
			removed, cerr := p.removeOrphanProbe(ctx, probe)
			if cerr != nil {
				p.log.Warn("could not remove a probe no verification owns", "vm", probe, "error", cerr)
			}

			asked = asked || !removed
		}

		if asked {
			if err := p.writeYieldRequest(request, name); err != nil {
				return nil, fmt.Errorf("ask a probe to yield its macOS slot: %w", err)
			}
		}

		retry.Reset(p.yieldRetry)

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			return nil, fmt.Errorf("a probe holding a macOS slot did not yield within %s: %w",
				p.yieldWait, ErrNoSlot)
		case <-retry.C:
		}
	}
}

// yieldRequest is a launch's request, named by itself and timed so a probe can ignore
// one a dead launch left.
type yieldRequest struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func (p *Provider) writeYieldRequest(path, name string) error {
	body, err := json.Marshal(yieldRequest{Name: name, At: p.now().UTC()})
	if err != nil {
		return err
	}

	return os.WriteFile(path, body, 0o600)
}

// yieldRequested reports whether a job has asked for a slot within the wait a launch
// gives it. A file that cannot be read says nothing, since a probe that kept running
// past one is what happens with no request at all, and a launch then fails as any does.
func (p *Provider) yieldRequested() bool {
	names, err := filepath.Glob(p.billetPath(yieldPrefix + "*"))
	if err != nil {
		return false
	}

	for _, path := range names {
		raw, err := regularfile.ReadFile(path, markerLimit, regularfile.Options{NoFollow: true})
		if err != nil {
			continue
		}

		var req yieldRequest
		if json.Unmarshal(raw, &req) == nil && p.now().Sub(req.At) < p.yieldWait {
			return true
		}
	}

	return false
}

// slotCensus is what admission reads under the slot lock.
type slotCensus struct {
	recorded int
	held     int
	// probes are the VMs named by the identities of held slots that are probes.
	probes []string
	// free is the first free slot, its lock held, or nil.
	free *os.File
	// uncounted are live guests no slot or token accounts for.
	uncounted []string
}

// admitMacOS takes a slot for name, or answers ErrNoSlot with the probes holding some.
// The whole judgement is under the slot lock, so admission sees one count from start to
// finish.
func (p *Provider) admitMacOS(ctx context.Context, name string, limit int) (*os.File, []string, error) {
	if limit <= 0 {
		return nil, nil, fmt.Errorf("this node's macOS limit is %d, so it runs no macOS guest", limit)
	}

	held, err := lockPath(ctx, p.billetPath(slotsLock), syscall.LOCK_EX, slotsLockHold)
	if err != nil {
		return nil, nil, fmt.Errorf("lock the guest slots: %w", err)
	}

	defer held.release()

	p.sweepTokens()

	recorded, err := p.settleSlotCount(limit)
	if err != nil {
		return nil, nil, err
	}

	census, err := p.readCensus(ctx, recorded)
	if err != nil {
		return nil, nil, err
	}

	if census.free != nil && census.held+1+len(census.uncounted) <= limit {
		if err := p.writeSlotIdentity(census.free, name); err != nil {
			_ = census.free.Close()

			return nil, nil, err
		}

		return census.free, nil, nil
	}

	if census.free != nil {
		_ = census.free.Close()
	}

	why := fmt.Sprintf("this node's limit is %d and %d are held by guests billet started",
		limit, census.held)
	if len(census.uncounted) > 0 {
		why += fmt.Sprintf(", and %d more hold none (%s)", len(census.uncounted),
			strings.Join(census.uncounted, "; "))
	}

	return nil, census.probes, fmt.Errorf("%w: %s", ErrNoSlot, why)
}

// settleSlotCount compares this deployment's limit with the count recorded in the store,
// and answers the count. The first deployment records it. A deployment whose limit
// differs refuses, so two sharing a store cannot together run more than the smaller
// allows; the record is rewritten only while no slot is held and every deployment
// present names one limit, which is how an operator raises or lowers it.
func (p *Provider) settleSlotCount(limit int) (int, error) {
	if err := writeSmallFile(p.billetPath(limitPrefix+p.owner), strconv.Itoa(limit)); err != nil {
		return 0, fmt.Errorf("record this deployment's macOS limit: %w", err)
	}

	// BEFORE ANYTHING ELSE, whichever deployment's number the store recorded: the
	// deployment that agrees with the count would otherwise run up to it while the smaller
	// one refuses, past what that one allows.
	others, err := p.deploymentLimits()
	if err != nil {
		return 0, err
	}

	for owner, other := range others {
		if other != limit {
			return 0, fmt.Errorf("this deployment's macOS limit is %d and deployment %s, sharing this "+
				"store, names %d; deployments on one store must name one limit, so that together they "+
				"run no more than it allows. If that deployment no longer uses this store, remove %s",
				limit, owner, other, p.billetPath(limitPrefix+owner))
		}
	}

	countPath := p.billetPath(slotsCount)

	raw, err := regularfile.ReadFile(countPath, markerLimit, regularfile.Options{NoFollow: true})
	if errors.Is(err, os.ErrNotExist) {
		if err := writeSmallFile(countPath, strconv.Itoa(limit)); err != nil {
			return 0, fmt.Errorf("record the store's macOS slots: %w", err)
		}

		return limit, nil
	}

	if err != nil {
		return 0, fmt.Errorf("read the store's macOS slots: %w", err)
	}

	recorded, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || recorded <= 0 {
		return 0, fmt.Errorf("%s holds no slot count", countPath)
	}

	if recorded == limit {
		return limit, nil
	}

	busy, err := p.anySlotHeld(recorded)
	if err != nil {
		return 0, err
	}

	if busy {
		return 0, fmt.Errorf("this store's macOS slots number %d and this deployment's limit is now "+
			"%d; the count changes only while no macOS guest holds a slot", recorded, limit)
	}

	if err := writeSmallFile(countPath, strconv.Itoa(limit)); err != nil {
		return 0, fmt.Errorf("record the store's macOS slots: %w", err)
	}

	return limit, nil
}

// deploymentLimits reads what every deployment on this store names. One that cannot be
// read refuses, since it may name another limit.
func (p *Provider) deploymentLimits() (map[string]int, error) {
	names, err := filepath.Glob(p.billetPath(limitPrefix + "*"))
	if err != nil {
		return nil, err
	}

	limits := make(map[string]int, len(names))

	for _, path := range names {
		raw, err := regularfile.ReadFile(path, markerLimit, regularfile.Options{NoFollow: true})
		if err != nil {
			return nil, fmt.Errorf("read a deployment's macOS limit: %w", err)
		}

		limit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("%s holds no limit", path)
		}

		limits[strings.TrimPrefix(filepath.Base(path), limitPrefix)] = limit
	}

	return limits, nil
}

// writeSmallFile installs a small file through the one durable ordering.
func writeSmallFile(path, value string) error {
	_, err := (durablefile.Installer{}).Install(filepath.Dir(path), filepath.Base(path), 0o600,
		func(w io.Writer) error {
			_, err := io.WriteString(w, value+"\n")

			return err
		})

	return err
}

func (p *Provider) anySlotHeld(count int) (bool, error) {
	for i := 1; i <= count; i++ {
		f, err := openLockFile(p.slotPath(i))
		if err != nil {
			return false, fmt.Errorf("open a macOS slot: %w", err)
		}

		free, err := tryFlock(f, syscall.LOCK_EX)

		_ = f.Close()

		if err != nil {
			return false, fmt.Errorf("try a macOS slot: %w", err)
		}

		if !free {
			return true, nil
		}
	}

	return false, nil
}

func (p *Provider) slotPath(i int) string {
	return p.billetPath(slotPrefix + strconv.Itoa(i))
}

// readCensus reads the slots and the guests beside them. It keeps the first free slot's
// lock for the caller. It refuses when the processes or their open files cannot be read.
func (p *Provider) readCensus(ctx context.Context, recorded int) (*slotCensus, error) {
	census := &slotCensus{recorded: recorded}

	keep := false
	defer func() {
		if !keep && census.free != nil {
			_ = census.free.Close()
			census.free = nil
		}
	}()

	slots := make([]string, 0, recorded)

	for i := 1; i <= recorded; i++ {
		path := p.slotPath(i)
		slots = append(slots, path)

		f, err := openLockFile(path)
		if err != nil {
			return nil, fmt.Errorf("open a macOS slot: %w", err)
		}

		free, err := tryFlock(f, syscall.LOCK_EX)
		if err != nil {
			_ = f.Close()

			return nil, fmt.Errorf("try a macOS slot: %w", err)
		}

		switch {
		case !free:
			census.held++

			if vm := slotIdentity(f); strings.HasPrefix(vm, probePrefix) {
				census.probes = append(census.probes, vm)
			}

			_ = f.Close()
		case census.free == nil:
			census.free = f
		default:
			_ = f.Close()
		}
	}

	uncounted, err := p.uncountedGuests(ctx, slots)
	if err != nil {
		return nil, err
	}

	census.uncounted = uncounted
	keep = true

	return census, nil
}

// slotIdentity reads the VM a held slot names, which is trusted only because the slot
// is locked and its holder wrote it under the slot lock this reader holds.
func slotIdentity(f *os.File) string {
	buf := make([]byte, markerLimit)

	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}

	vm, _, _ := strings.Cut(strings.TrimSpace(string(buf[:n])), " ")

	return vm
}

// writeSlotIdentity records who holds a slot: the VM's name and the time.
func (p *Provider) writeSlotIdentity(f *os.File, name string) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("write the macOS slot's holder: %w", err)
	}

	if _, err := f.WriteAt([]byte(name+" "+p.now().UTC().Format(time.RFC3339)+"\n"), 0); err != nil {
		return fmt.Errorf("write the macOS slot's holder: %w", err)
	}

	return nil
}

// tartGuestCommand matches the processes every VM tart runs has for its whole life: a
// `tart run`, and a `tart create --from-ipsw`, which installs macOS in a VM it runs
// itself. `ps` shows the resolved path of tart.app, which may hold a space, so a command
// alone cannot say where its executable ends: what matches here is only a candidate, and
// the process's own executable says whether another program merely has these words among
// its arguments.
var (
	tartRunCommand    = regexp.MustCompile(`(^|/)tart\s+run(\s|$)`)
	tartCreateCommand = regexp.MustCompile(`(^|/)tart\s+create\s(.*\s)?--from-ipsw(\s|=|$)`)
)

// isTartGuestCommand reads one `ps` command column.
func (p *Provider) isTartGuestCommand(command string) bool {
	return tartRunCommand.MatchString(command) || tartCreateCommand.MatchString(command)
}

// tartGuests lists the host's live tart guests as pid and command. `ps` sees this user's
// and other users' commands alike; a failure refuses rather than count none.
func (p *Provider) tartGuests(ctx context.Context) (map[int]string, error) {
	// #nosec G204 -- the binary is fixed and the arguments are literals.
	cmd := exec.CommandContext(ctx, p.ps, "-axww", "-o", "pid=,command=")
	cmd.WaitDelay = commandWaitDelay

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", p.ps, err, strings.TrimSpace(stderr.String()))
	}

	guests := map[int]string{}

	for line := range strings.Lines(stdout.String()) {
		field, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}

		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%s answered %q, which is not a pid", p.ps, field)
		}

		command = strings.TrimSpace(command)
		if !p.isTartGuestCommand(command) {
			continue
		}

		// A PROCESS THAT HAS GONE IS NO GUEST, and one whose executable cannot be read is
		// could-not-tell, which refuses.
		exe, err := p.processPath(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read the executable of pid %d: %w", pid, err)
		}

		if filepath.Base(exe) == "tart" {
			guests[pid] = command
		}
	}

	return guests, nil
}

// uncountedGuests is the census: every live tart guest that holds no slot and no token,
// which counts as a macOS guest whatever it is, since billet cannot tell.
func (p *Provider) uncountedGuests(ctx context.Context, slots []string) ([]string, error) {
	guests, err := p.tartGuests(ctx)
	if err != nil {
		return nil, fmt.Errorf("count the guests on this host: %w", err)
	}

	if len(guests) == 0 {
		return nil, nil
	}

	tokens, err := filepath.Glob(p.billetPath(tokenPrefix + "*"))
	if err != nil {
		return nil, fmt.Errorf("list the guest tokens: %w", err)
	}

	accounted := map[int]bool{}

	for _, path := range slices.Concat(slots, tokens) {
		pids, err := p.openers(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("see who holds %s: %w", filepath.Base(path), err)
		}

		for _, pid := range pids {
			accounted[pid] = true
		}
	}

	var uncounted []string

	for pid, command := range guests {
		if !accounted[pid] {
			uncounted = append(uncounted, fmt.Sprintf("pid %d, %s", pid, guestNameIn(command)))
		}
	}

	slices.Sort(uncounted)

	return uncounted, nil
}

// guestNameIn reads the VM's name out of a tart command, for a message.
func guestNameIn(command string) string {
	if _, rest, ok := strings.Cut(command, "tart run "); ok {
		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0]
		}
	}

	if _, rest, ok := strings.Cut(command, "tart create "); ok {
		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0]
		}
	}

	return "unnamed"
}

// SlotReport is what `billet check` says about a store's guests.
type SlotReport struct {
	// Recorded is the store's slot count, zero when no macOS guest ever took one.
	Recorded int
	// Held counts slots a process holds.
	Held int
	// Limits are the limits each deployment present on the store names.
	Limits map[string]int
	// Limit is this node's own, as its control plane said it. LimitKnown false is no
	// record, so a launch uses Apple's default and a probe refuses.
	Limit      int
	LimitKnown bool
	// Uncounted are the guests billet's slots and tokens do not account for.
	Uncounted []string
}

// Slots reads the store's slots and guests for a report, under the slot lock.
func (p *Provider) Slots(ctx context.Context) (SlotReport, error) {
	var report SlotReport

	held, err := lockPath(ctx, p.billetPath(slotsLock), syscall.LOCK_EX, slotsLockHold)
	if err != nil {
		return report, fmt.Errorf("lock the guest slots: %w", err)
	}

	defer held.release()

	raw, err := regularfile.ReadFile(p.billetPath(slotsCount), markerLimit, regularfile.Options{NoFollow: true})

	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return report, fmt.Errorf("read the store's macOS slots: %w", err)
	default:
		if report.Recorded, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
			return report, errors.New("the store's macOS slot count is not a number")
		}
	}

	if report.Limits, err = p.deploymentLimits(); err != nil {
		return report, err
	}

	if p.macOSLimit != nil {
		if report.Limit, report.LimitKnown, err = p.macOSLimit(); err != nil {
			return report, fmt.Errorf("read this node's macOS limit: %w", err)
		}
	}

	census, err := p.readCensus(ctx, report.Recorded)
	if err != nil {
		return report, err
	}

	if census.free != nil {
		_ = census.free.Close()
	}

	report.Held = census.held
	report.Uncounted = census.uncounted

	return report, nil
}
