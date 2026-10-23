// Package power lowers a Linux host's CPU ceiling while its node is idle and
// puts back what was there before the node launches anything.
//
// WHAT IT WRITES is cpufreq's sysfs: every policy's scaling_max_freq and the
// global boost switch, under a root the caller names (/sys in production, a
// temporary directory in a test).
//
// A HOST IS NEVER LEFT CAPPED ON PURPOSE, and the record is how. Before the
// first write of a cap, the values the cap is about to change (and every
// policy's ceiling, which a boost switch can move) are installed durably under
// the node's state directory. A restore writes them back, reads each one back,
// and only then removes the record. A record found at a node's start is a cap
// an earlier process left, and the node restores it whatever its configuration
// says now. A record that cannot be read is could-not-tell, never absence.
//
// A leaf: the standard library, durablefile and regularfile.
package power

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// RecordName is the record of what a cap changed, under the node's state
// directory.
const RecordName = "power-baseline.json"

const (
	// recordVersion is the record's schema; a record of another version is
	// refused as could-not-tell rather than read for what it might mean.
	recordVersion = 1
	// recordLimit is far above a record for a thousand CPUs.
	recordLimit = 1 << 20

	// DefaultTick is how often the loop looks at the node.
	DefaultTick = 5 * time.Second
	// DefaultLaunchBound is the longest a launch waits for a restore.
	DefaultLaunchBound = 2 * time.Second
	// DefaultStopBound is the longest a stopping node goes on retrying one.
	DefaultStopBound = 30 * time.Second
	// failureLogEvery spaces the lines a restore that keeps failing logs.
	failureLogEvery = time.Minute
	// DefaultSettle is how long a write may take to read back; settlePoll is
	// how often it is read meanwhile.
	DefaultSettle = time.Second
	settlePoll    = 10 * time.Millisecond
	// wOK is access(2)'s W_OK, which package syscall does not name everywhere.
	wOK = 2
)

// Transitions and outcomes an Observer is told of.
const (
	TransitionCap     = "cap"
	TransitionRestore = "restore"
	OutcomeOK         = "ok"
	OutcomeFailed     = "failed"
)

// TransitionLabels is every transition an Observer is told of.
func TransitionLabels() []string { return []string{TransitionCap, TransitionRestore} }

// OutcomeLabels is every outcome an Observer is told of.
func OutcomeLabels() []string { return []string{OutcomeOK, OutcomeFailed} }

// Observer counts what the controller does. internal/metrics satisfies it
// without importing this package.
type Observer interface {
	// PowerChanged is one cap or restore that wrote something, by how it ended.
	PowerChanged(transition, outcome string)
	// PowerLaunchRestore is how long a launch's restore took, from the launch
	// asking to every value read back.
	PowerLaunchRestore(took time.Duration)
}

type noObserver struct{}

func (noObserver) PowerChanged(string, string)      {}
func (noObserver) PowerLaunchRestore(time.Duration) {}

// Settings is what an idle node does. The zero value caps nothing, which is a
// controller that only restores what an earlier process left.
type Settings struct {
	IdleAfter    time.Duration
	DisableBoost bool
	MaxFreqKHz   int
}

func (s Settings) caps() bool { return s.DisableBoost || s.MaxFreqKHz > 0 }

// Options configure a Controller.
type Options struct {
	// SysfsRoot is where sysfs is mounted: /sys on a host.
	SysfsRoot string
	// StateDir is the node's state directory, which holds the record.
	StateDir string
	Settings Settings
	Log      *slog.Logger
	Observer Observer
	// Tick, LaunchBound and StopBound default to DefaultTick,
	// DefaultLaunchBound and DefaultStopBound.
	Tick, LaunchBound, StopBound time.Duration
}

// state is what the controller knows of the host's ceiling.
type state int32

const (
	// stateUnknown, THE ZERO VALUE, is could-not-tell: a restore is owed before
	// anything else, and nothing is capped from it.
	stateUnknown state = iota
	// stateFull is proved: no record is outstanding.
	stateFull
	// stateCapped is the idle setting in force, with its record installed.
	stateCapped
)

// Controller caps and restores one host's cpufreq ceiling.
type Controller struct {
	root, stateDir string
	settings       Settings
	log            *slog.Logger
	observer       Observer

	tick, launchBound, stopBound, settle time.Duration

	// write is one sysfs write; a seam so a test can fail one.
	write func(path, value string) error

	// sem serializes every read and write of the host's ceiling and of the
	// record, and every change of st. A channel so a launch can wait for it
	// with a bound.
	sem chan struct{}
	// st is written only under sem and read without it by a launch's fast path.
	st atomic.Int32

	mu       sync.Mutex
	busyAt   time.Time
	stopping bool

	// failing and logged date a streak of failed restores and its last line.
	// Under sem.
	failing, logged time.Time
}

// New builds a controller; it reads and writes nothing until asked.
func New(opts Options) (*Controller, error) {
	if opts.SysfsRoot == "" || opts.StateDir == "" {
		return nil, errors.New("power: a sysfs root and a state directory are required")
	}
	if opts.Settings.caps() && opts.Settings.IdleAfter <= 0 {
		return nil, errors.New("power: a controller that caps needs how long the node must be idle first")
	}

	c := &Controller{
		root: opts.SysfsRoot, stateDir: opts.StateDir, settings: opts.Settings,
		log: opts.Log, observer: opts.Observer,
		tick: opts.Tick, launchBound: opts.LaunchBound, stopBound: opts.StopBound,
		write:  writeSysfs,
		sem:    make(chan struct{}, 1),
		busyAt: time.Now(),
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.observer == nil {
		c.observer = noObserver{}
	}
	if c.tick <= 0 {
		c.tick = DefaultTick
	}
	if c.launchBound <= 0 {
		c.launchBound = DefaultLaunchBound
	}
	if c.stopBound <= 0 {
		c.stopBound = DefaultStopBound
	}
	c.settle = DefaultSettle

	return c, nil
}

// record is what a cap changed and what a restore writes back.
type record struct {
	Version int `json:"version"`
	// Boost is the global boost switch, "" when the cap did not touch it.
	Boost string `json:"boost,omitempty"`
	// Policies are keyed by the policy directory's name (policy0).
	Policies map[string]policyRecord `json:"policies"`
}

type policyRecord struct {
	MaxFreq string `json:"scaling_max_freq"`
	// Boost is the policy's own boost switch, kept when the cap writes the
	// global one, which moves it.
	Boost string `json:"boost,omitempty"`
}

func (c *Controller) cpufreq() string {
	return filepath.Join(c.root, "devices", "system", "cpu", "cpufreq")
}

func (c *Controller) boostPath() string { return filepath.Join(c.cpufreq(), "boost") }

func (c *Controller) policyPath(policy, file string) string {
	return filepath.Join(c.cpufreq(), policy, file)
}

func (c *Controller) recordPath() string { return filepath.Join(c.stateDir, RecordName) }

// policies are the cpufreq policy directories, in order.
func (c *Controller) policies() ([]string, error) {
	entries, err := os.ReadDir(c.cpufreq())
	if err != nil {
		return nil, fmt.Errorf("power: list the cpufreq policies under %s: %w", c.cpufreq(), err)
	}

	var out []string
	for _, e := range entries {
		if isPolicy(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.SortFunc(out, func(a, b string) int { return policyNumber(a) - policyNumber(b) })

	return out, nil
}

// policyNumber orders policy names numerically; isPolicy has admitted them.
func policyNumber(name string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(name, "policy"))
	if err != nil {
		return -1
	}

	return n
}

// readSysfs reads one attribute. NOT THROUGH regularfile, which refuses sysfs
// by design: these are the driver's attributes and reading them is the point.
func readSysfs(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("power: read %s: %w", path, err)
	}

	return strings.TrimSpace(string(b)), nil
}

// writeSysfs writes one attribute that must already exist: O_CREATE is left
// out so a missing attribute is an error rather than a new file.
func writeSysfs(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("power: open %s for writing: %w", path, err)
	}
	_, werr := f.WriteString(value)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("power: write %q to %s: %w", value, path, err)
	}

	return nil
}

// CheckHost refuses a host that cannot honour the settings exactly: no
// cpufreq, a ceiling that is not one of a policy's available frequencies or is
// below its floor, or a boost switch it does not have. Nothing is written.
func (c *Controller) CheckHost() error {
	if !c.settings.caps() {
		return nil
	}

	policies, err := c.policies()
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return fmt.Errorf("power: %s holds no cpufreq policy, so this host has no ceiling to lower", c.cpufreq())
	}

	var errs []error
	for _, p := range policies {
		maxPath := c.policyPath(p, "scaling_max_freq")
		if _, err := readSysfs(maxPath); err != nil {
			errs = append(errs, err)

			continue
		}
		if err := syscall.Access(maxPath, wOK); err != nil {
			errs = append(errs, fmt.Errorf("power: %s is not writable here: %w", maxPath, err))
		}
		if c.settings.MaxFreqKHz > 0 {
			errs = append(errs, c.checkCeiling(p)...)
		}
	}

	if c.settings.DisableBoost {
		switch v, err := readSysfs(c.boostPath()); {
		case errors.Is(err, fs.ErrNotExist):
			driver, derr := readSysfs(c.policyPath(policies[0], "scaling_driver"))
			if derr != nil {
				driver = "unreadable"
			}
			errs = append(errs, fmt.Errorf("power: node.power.disable_boost is set, but this host has no %s "+
				"(its cpufreq driver is %q); billet writes no other boost switch", c.boostPath(), driver))
		case err != nil:
			errs = append(errs, err)
		case v != "0" && v != "1":
			errs = append(errs, fmt.Errorf("power: %s reads %q, which is not 0 or 1", c.boostPath(), v))
		default:
			if err := syscall.Access(c.boostPath(), wOK); err != nil {
				errs = append(errs, fmt.Errorf("power: %s is not writable here: %w", c.boostPath(), err))
			}
		}
	}

	return errors.Join(errs...)
}

// checkCeiling refuses a ceiling one policy cannot be set to exactly.
//
// THE KERNEL ROUNDS A VALUE THAT IS NOT IN ITS TABLE to an entry of it, which
// is a frequency nobody wrote, so a policy that lists none is could-not-tell
// and refused rather than trusted to take the value as written.
func (c *Controller) checkCeiling(p string) []error {
	want := c.settings.MaxFreqKHz
	var errs []error

	avail, err := readSysfs(c.policyPath(p, "scaling_available_frequencies"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("power: node.power.idle_max_freq_khz needs a policy's list of the "+
			"frequencies it can be set to, and %s could not be read: %w", p, err))
	case !slices.Contains(strings.Fields(avail), strconv.Itoa(want)):
		errs = append(errs, fmt.Errorf("power: node.power.idle_max_freq_khz %d is not one of %s's available "+
			"frequencies (%s)", want, p, avail))
	}

	minimum, err := readKHz(c.policyPath(p, "scaling_min_freq"))
	switch {
	case err != nil:
		errs = append(errs, err)
	case want < minimum:
		errs = append(errs, fmt.Errorf("power: node.power.idle_max_freq_khz %d is below %s's scaling_min_freq "+
			"%d", want, p, minimum))
	}

	return errs
}

func readKHz(path string) (int, error) {
	v, err := readSysfs(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("power: %s reads %q, which is not a frequency in kHz", path, v)
	}

	return n, nil
}

// loadRecord reads the record, answering false only when there is none. A
// record that cannot be read, or is not one this package wrote, is an error:
// could-not-tell, never absence.
func (c *Controller) loadRecord() (record, bool, error) {
	body, err := regularfile.ReadFile(c.recordPath(), recordLimit, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, false, nil
	}
	if err != nil {
		return record{}, false, fmt.Errorf("power: read the record of what the last cap changed: %w", err)
	}

	var rec record
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return record{}, false, fmt.Errorf("power: %s is not a record this node wrote: %w", c.recordPath(), err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return record{}, false, fmt.Errorf("power: %s holds more than the one record this node writes",
			c.recordPath())
	}
	if rec.Version != recordVersion {
		return record{}, false, fmt.Errorf("power: %s is version %d, and this node reads version %d",
			c.recordPath(), rec.Version, recordVersion)
	}

	// A RECORD OF NO POLICY RESTORES NOTHING, and taking it as a restore would
	// call a capped host full: a cap always records every policy.
	if len(rec.Policies) == 0 {
		return record{}, false, fmt.Errorf("power: %s records no cpufreq policy", c.recordPath())
	}

	// WHAT A RESTORE WRITES AS ROOT, so it is held to what a cap records: a
	// policy's name, a frequency, a switch.
	if rec.Boost != "" && !isSwitch(rec.Boost) {
		return record{}, false, fmt.Errorf("power: %s records boost %q", c.recordPath(), rec.Boost)
	}
	for name, p := range rec.Policies {
		if !isPolicy(name) || !isDigits(p.MaxFreq) || (p.Boost != "" && !isSwitch(p.Boost)) {
			return record{}, false, fmt.Errorf("power: %s records %q as %+v, which is not a cpufreq policy's "+
				"ceiling and switch", c.recordPath(), name, p)
		}
	}

	return rec, true, nil
}

func isSwitch(v string) bool { return v == "0" || v == "1" }

func isPolicy(name string) bool {
	rest, ok := strings.CutPrefix(name, "policy")

	return ok && isDigits(rest)
}

func isDigits(v string) bool {
	if v == "" || len(v) > 12 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func (c *Controller) installRecord(rec record) error {
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("power: encode the record: %w", err)
	}

	_, err = durablefile.Installer{}.Install(c.stateDir, RecordName, 0o600, func(w io.Writer) error {
		_, err := w.Write(append(body, '\n'))

		return err
	})
	if err != nil {
		return fmt.Errorf("power: install the record of what this cap changes: %w", err)
	}

	return nil
}

func (c *Controller) removeRecord() error {
	if err := os.Remove(c.recordPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("power: remove the record of a restored cap: %w", err)
	}
	if err := (durablefile.Installer{}).SyncDirectory(c.stateDir); err != nil {
		return fmt.Errorf("power: commit the removal of the record of a restored cap: %w", err)
	}

	return nil
}

// write is one value to put at one path, and what it must read back as.
// isSwitch marks a boost switch, which the kernel applies at once.
type write struct {
	path, value string
	isSwitch    bool
}

// restoreWrites is a record's values in the order they go back: the global
// boost switch first, since turning it on can move every policy's own switch
// and ceiling, then each policy's switch, then each ceiling.
func (c *Controller) restoreWrites(rec record) []write {
	var out []write
	if rec.Boost != "" {
		out = append(out, write{c.boostPath(), rec.Boost, true})
	}

	names := make([]string, 0, len(rec.Policies))
	for name := range rec.Policies {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if b := rec.Policies[name].Boost; b != "" {
			out = append(out, write{c.policyPath(name, "boost"), b, true})
		}
	}
	for _, name := range names {
		out = append(out, write{c.policyPath(name, "scaling_max_freq"), rec.Policies[name].MaxFreq, false})
	}

	return out
}

// apply writes every value, then reads every one back. A write that fails does
// not stop the others: a restore puts back as much as it can.
func (c *Controller) apply(writes []write) error {
	var errs []error
	for _, w := range writes {
		// A SWITCH ALREADY AT ITS VALUE IS NOT WRITTEN: the kernel refuses a
		// policy's own boost while the global switch is off, even to the value it
		// already holds (store_local_boost, Linux 7.0). A ceiling is always
		// written, since what it reads can be a write still being applied.
		if w.isSwitch {
			if got, err := readSysfs(w.path); err == nil && got == w.value {
				continue
			}
		}
		if err := c.write(w.path, w.value); err != nil {
			errs = append(errs, err)
		}
	}

	// READ BACK UNTIL IT SETTLES. A ceiling's store updates a frequency QoS
	// request whose notifier only schedules the policy's update (Linux 7.0,
	// cpufreq_notifier_max), so a value read at once can still be the old one.
	deadline := time.Now().Add(c.settle)
	for {
		unsettled := c.unsettled(writes)
		if unsettled == nil || !time.Now().Before(deadline) {
			errs = append(errs, unsettled)

			break
		}
		time.Sleep(settlePoll)
	}

	return errors.Join(errs...)
}

// unsettled says which written values do not read back yet, or nil.
func (c *Controller) unsettled(writes []write) error {
	var errs []error
	for _, w := range writes {
		got, err := readSysfs(w.path)
		switch {
		case err != nil:
			errs = append(errs, err)
		case got != w.value:
			errs = append(errs, fmt.Errorf("power: %s reads %q after %q was written", w.path, got, w.value))
		}
	}

	return errors.Join(errs...)
}

// restoreLocked puts back what the record holds and removes it. It answers
// whether it wrote anything. Requires sem.
func (c *Controller) restoreLocked(reason string) (bool, error) {
	if state(c.st.Load()) == stateFull {
		return false, nil
	}

	rec, found, err := c.loadRecord()
	if err != nil {
		c.st.Store(int32(stateUnknown))
		c.restoreFailed(reason, err)

		return false, err
	}
	if !found {
		c.st.Store(int32(stateFull))
		c.restoreSucceeded()

		return false, nil
	}

	c.st.Store(int32(stateUnknown))

	// THE RECORD GOES ONLY ONCE EVERY VALUE READS BACK: it is what the next
	// attempt restores from.
	err = c.apply(c.restoreWrites(rec))
	if err == nil {
		err = c.removeRecord()
	}
	if err != nil {
		c.observer.PowerChanged(TransitionRestore, OutcomeFailed)
		c.restoreFailed(reason, err)

		return true, err
	}

	c.st.Store(int32(stateFull))
	c.observer.PowerChanged(TransitionRestore, OutcomeOK)
	c.restoreSucceeded()

	return true, nil
}

// restoreFailed says a restore failed: at once at the start of a streak, and
// at most once a minute after, while the loop goes on retrying. Requires sem.
func (c *Controller) restoreFailed(reason string, err error) {
	now := time.Now()
	if c.failing.IsZero() {
		c.failing = now
	} else if now.Sub(c.logged) < failureLogEvery {
		return
	}
	c.logged = now
	c.log.Error("COULD NOT RESTORE THE CPU'S FULL CEILING; this host may be running capped, and the node "+
		"keeps retrying", "reason", reason, "failing_for", now.Sub(c.failing).Round(time.Second),
		"record", c.recordPath(), "error", err)
}

func (c *Controller) restoreSucceeded() {
	if !c.failing.IsZero() {
		c.log.Info("restored the CPU's full ceiling after failed attempts",
			"failed_for", time.Since(c.failing).Round(time.Second))
	}
	c.failing, c.logged = time.Time{}, time.Time{}
}

// capLocked lowers the ceiling: the record first, then the writes. Requires
// sem and stateFull. A cap that fails part way is restored at once.
func (c *Controller) capLocked(idleFor time.Duration) error {
	if _, found, err := c.loadRecord(); err != nil || found {
		// A RECORD IS ALREADY THERE, or none can be read: either way the values
		// on the host may not be the baseline, and capping now would record a
		// cap as the thing to restore.
		c.st.Store(int32(stateUnknown))
		if err == nil {
			err = fmt.Errorf("power: %s is already there", c.recordPath())
		}

		return err
	}

	rec, writes, err := c.plan()
	if err != nil {
		// NOTHING WAS WRITTEN and no record is there, which is full proved.
		c.st.Store(int32(stateFull))

		return err
	}
	if len(writes) == 0 {
		c.st.Store(int32(stateCapped))

		return nil
	}

	// THE RECORD BEFORE THE FIRST WRITE. From here a restore is owed until one
	// is proved, which stateUnknown says.
	c.st.Store(int32(stateUnknown))
	if err := c.installRecord(rec); err != nil {
		c.observer.PowerChanged(TransitionCap, OutcomeFailed)

		return err
	}

	if err := c.apply(writes); err != nil {
		c.observer.PowerChanged(TransitionCap, OutcomeFailed)
		c.log.Error("could not lower the CPU's ceiling on an idle node; restoring what was there", "error", err)
		_, rerr := c.restoreLocked("failed cap")

		return errors.Join(err, rerr)
	}

	c.st.Store(int32(stateCapped))
	c.observer.PowerChanged(TransitionCap, OutcomeOK)
	c.log.Info("lowered the CPU's ceiling on an idle node", "idle_for", idleFor.Round(time.Second),
		"max_freq_khz", c.settings.MaxFreqKHz, "boost_off", c.settings.DisableBoost, "writes", len(writes))

	return nil
}

// plan reads what is there and answers the record of it and the writes a cap
// makes. A ceiling already at or below the setting is left alone: a cap never
// raises one.
func (c *Controller) plan() (record, []write, error) {
	policies, err := c.policies()
	if err != nil {
		return record{}, nil, err
	}
	if len(policies) == 0 {
		return record{}, nil, fmt.Errorf("power: %s holds no cpufreq policy", c.cpufreq())
	}

	rec := record{Version: recordVersion, Policies: make(map[string]policyRecord, len(policies))}
	var writes []write
	for _, p := range policies {
		current, err := readKHz(c.policyPath(p, "scaling_max_freq"))
		if err != nil {
			return record{}, nil, err
		}
		pr := policyRecord{MaxFreq: strconv.Itoa(current)}
		if c.settings.DisableBoost {
			switch b, err := readSysfs(c.policyPath(p, "boost")); {
			case err == nil:
				pr.Boost = b
			case !errors.Is(err, fs.ErrNotExist):
				return record{}, nil, err
			}
		}
		rec.Policies[p] = pr

		if want := c.settings.MaxFreqKHz; want > 0 && current > want {
			writes = append(writes, write{c.policyPath(p, "scaling_max_freq"), strconv.Itoa(want), false})
		}
	}

	if c.settings.DisableBoost {
		b, err := readSysfs(c.boostPath())
		if err != nil {
			return record{}, nil, err
		}
		rec.Boost = b
		if b != "0" {
			writes = append(writes, write{c.boostPath(), "0", true})
		}
	}

	return rec, writes, nil
}

func (c *Controller) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) release() { <-c.sem }

// Restore puts back what a cap changed, if one is outstanding. A node calls it
// at its start, for a cap an earlier process left.
func (c *Controller) Restore(ctx context.Context, reason string) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	return c.restoreSaying(reason)
}

// RestoreLeftover is Restore for a controller nothing else holds yet, which a
// node builds the moment it holds its lock: before its provider or anything
// else that can refuse its start, so a refusal never leaves an earlier
// process's cap in place.
func (c *Controller) RestoreLeftover() error {
	c.sem <- struct{}{}
	defer c.release()

	return c.restoreSaying("start")
}

// restoreSaying is restoreLocked with the line a successful one logs.
// Requires sem.
func (c *Controller) restoreSaying(reason string) error {
	wrote, err := c.restoreLocked(reason)
	if wrote && err == nil {
		c.log.Info("restored the CPU's full ceiling", "reason", reason)
	}

	return err
}

func (c *Controller) markBusy(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if at.After(c.busyAt) {
		c.busyAt = at
	}
}

func (c *Controller) busySince() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.busyAt
}

func (c *Controller) isStopping() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.stopping
}

// BeginStop caps nothing from now on. A node calls it when its stop begins,
// since a drain that follows may hold the host for hours.
func (c *Controller) BeginStop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stopping = true
}

// RestoreBeforeLaunch puts back the full ceiling before a launch starts its
// compute, waiting for it at most the launch bound.
//
// THE CALLER HAS ALREADY MADE THE NODE BUSY (its launch is held, so the loop's
// idle question answers no) before it calls this, and that is what closes the
// race with a cap: the loop marks the state unknown before it asks whether the
// node is idle, so either this call sees unknown and waits for the cap to end
// and then restores it, or the cap's question sees the launch and caps nothing.
//
// A RESTORE THAT FAILS OR OVERRUNS DOES NOT STOP THE LAUNCH: a capped host runs
// a job slower, not wrong. It is logged, it goes on in the background, and the
// loop retries one that failed.
func (c *Controller) RestoreBeforeLaunch(ctx context.Context, lease string) {
	asked := time.Now()
	c.markBusy(asked)

	if state(c.st.Load()) == stateFull {
		return
	}

	done := make(chan error, 1)
	go func() {
		// NOT BOUND BY THE LAUNCH: a restore that has begun is finished, since the
		// host is owed it whether or not this launch still waits.
		c.sem <- struct{}{}
		wrote, err := c.restoreLocked("launch")
		c.release()

		took := time.Since(asked)
		if wrote && err == nil {
			c.observer.PowerLaunchRestore(took)
			c.log.Info("restored the CPU's full ceiling before a launch", "lease", lease,
				"took", took.Round(time.Microsecond))
		}
		done <- err
	}()

	timer := time.NewTimer(c.launchBound)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			c.log.Error("launching on a host whose CPU ceiling could not be restored; the job may run slower",
				"lease", lease, "error", err)
		}
	case <-timer.C:
		c.log.Error("the CPU ceiling's restore did not finish within its bound; launching anyway, and it "+
			"goes on in the background", "lease", lease, "bound", c.launchBound)
	case <-ctx.Done():
	}
}

// Run looks at the node every tick until ctx ends: it retries a restore that
// is owed, restores a cap the node is busy under, and caps a node idle for the
// settings' IdleAfter. idle answers whether the node holds nothing, and is
// asked under the controller's lock.
func (c *Controller) Run(ctx context.Context, idle func() bool) {
	ticker := time.NewTicker(c.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.step(ctx, idle)
		}
	}
}

// step is one look at the node.
func (c *Controller) step(ctx context.Context, idle func() bool) {
	// CANCELLABLE, so a loop waiting behind a restore stuck in the kernel still
	// returns when the node stops.
	if c.acquire(ctx) != nil {
		return
	}
	defer c.release()

	switch state(c.st.Load()) {
	case stateUnknown:
		// A FAILURE IS restoreFailed's TO SAY, at most once a minute, and the next
		// tick tries again.
		if wrote, err := c.restoreLocked("retry"); wrote && err == nil {
			c.log.Info("restored the CPU's full ceiling on a retry")
		}

	case stateCapped:
		// BUSY WITHOUT A LAUNCH: an adoption, or a launch whose own restore lost
		// the race for the bound. Either way the host is owed its ceiling.
		if c.isStopping() || !idle() {
			if wrote, err := c.restoreLocked("busy"); wrote && err == nil {
				c.log.Info("restored the CPU's full ceiling on a node that is no longer idle")
			}
		}

	case stateFull:
		if !c.settings.caps() || c.isStopping() {
			return
		}
		now := time.Now()
		if !idle() {
			c.markBusy(now)

			return
		}
		idleFor := now.Sub(c.busySince())
		if idleFor < c.settings.IdleAfter {
			return
		}

		// UNKNOWN BEFORE THE SECOND QUESTION, which is what a launch's fast path
		// reads: one that began before this store is seen below, and one that
		// begins after it waits for the cap and restores it.
		c.st.Store(int32(stateUnknown))
		if !idle() {
			c.st.Store(int32(stateFull))
			c.markBusy(now)

			return
		}
		if err := c.capLocked(idleFor); err != nil {
			// TRIED AGAIN AFTER ANOTHER IDLE PERIOD, not on the next tick.
			c.markBusy(now)
			c.log.Warn("did not lower the CPU's ceiling on an idle node", "error", err)
		}
	}
}

// Close caps nothing more and restores the full ceiling, retrying for at most
// the stop bound. A node calls it once its loop has returned.
func (c *Controller) Close(ctx context.Context) error {
	c.BeginStop()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.stopBound)
	defer cancel()

	for {
		err := c.Restore(ctx, "stop")
		if err == nil {
			return nil
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("power: the node stopped without restoring the CPU's full ceiling; the next "+
				"node process restores it from %s: %w", c.recordPath(), err)
		case <-timer.C:
		}
	}
}
