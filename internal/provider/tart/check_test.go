package tart

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CheckHost proves the four facts a tart node needs — platform, binary,
// `exec` capability, readable store — and each refusal names what to do.
func TestCheckHostReportsTheHostItProved(t *testing.T) {
	s := newStub(t)
	s.vm(t, "billet-lease1", "running", testOwner)

	p := newProvider(t, s)
	p.goos = "darwin"

	report, err := p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost: %v", err)
	}

	if report.Version != "2.36.0" {
		t.Errorf("Version = %q, want the binary's own answer", report.Version)
	}

	if report.VMs != 1 {
		t.Errorf("VMs = %d, want 1", report.VMs)
	}

	if report.StoreLock != p.storeLockPath() {
		t.Errorf("StoreLock = %q, want %q", report.StoreLock, p.storeLockPath())
	}

	if _, err := os.Stat(report.StoreLock); err != nil {
		t.Errorf("CheckHost named a store lock it never placed (%v)", err)
	}

	// PROVED, not merely named. This is one half of a pair: the contended test
	// below asserts the OTHER value, and a CheckHost that skipped the lock
	// entirely cannot produce this one — which is what stops either test from
	// passing against a preflight that does not try.
	if !report.StoreLockProved {
		t.Errorf("StoreLockProved = false on an uncontended store (%s); billet did not "+
			"actually take the lock it reports", report.StoreLockWhy)
	}
}

// A CHECK RUNS BESIDE A LIVE NODE, so a held store lock is the ordinary state
// and must not fail the preflight. It is NOT a pass either, and saying it was
// is the defect this pair now guards: EWOULDBLOCK proves a file description
// holds the lock, not that the holder is billet, that it is making progress, or
// that a real mutation would ever acquire it — so a permanently wedged store
// would have reported a clean host and then failed every launch and teardown.
//
// The other half is that it must not WAIT: the mutation window is minutes, and
// an operator's preflight stalling that long on a healthy host is the same
// defect wearing a different face.
func TestCheckHostReportsAHeldStoreLockAsUnprovedRatherThanFine(t *testing.T) {
	s := newStub(t)

	p := newProvider(t, s)
	p.goos = "darwin"

	holdTheStore(t, s.home)

	start := time.Now()

	report, err := p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost refused a host whose store lock was simply in use: %v", err)
	}

	if report.StoreLock == "" {
		t.Error("CheckHost reported no store lock path although the lock plainly exists")
	}

	// THE HALF THAT CANNOT BE FAKED BY NOT LOOKING. Its sibling above asserts
	// the opposite value on an uncontended store, so a CheckHost that skipped
	// lockStore fails one of the two whichever way it defaults.
	if report.StoreLockProved {
		t.Error("CheckHost reported the store lock as proved while another process held it, " +
			"so a wedged store passes the preflight and then fails every teardown")
	}

	if !strings.Contains(report.StoreLockWhy, "could not prove it can take it") {
		t.Errorf("StoreLockWhy = %q, want it to say what was not established", report.StoreLockWhy)
	}

	// checkStoreLockWait is two seconds; the mutation window is two minutes.
	// Anything near the latter means the preflight took the wrong clock.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("CheckHost waited %s for a lock it only needed to ask about", elapsed)
	}
}

// AND A CANCELLED CHECK IS NOT A PASSING ONE. lockStore wraps the caller's
// context error AND errStoreLockBusy on the same branch, so a switch that asked
// about contention first read an interrupted preflight as a healthy host — a
// real defect, found in review, and the reason ctx.Err() is consulted before
// anything else.
func TestCheckHostDoesNotReadACancelledWaitAsAHealthyStore(t *testing.T) {
	s := newStub(t)

	p := newProvider(t, s)
	p.goos = "darwin"

	holdTheStore(t, s.home)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// CANCELLED EXACTLY WHERE IT MATTERS, through the seam, because racing it
	// from a goroutine could not be made to land there. Keying off the stub's
	// argv log cancelled during the `tart list` instead — one run in three,
	// measured — and the test then asserted against an error from a different
	// call entirely. The seam fires immediately before the lock attempt, so the
	// wait below begins on an already-cancelled context and the branch under
	// test is the only one reachable.
	p.beforeStoreLock = cancel

	_, err := p.CheckHost(ctx)
	if err == nil {
		t.Fatal("CheckHost reported a healthy host although the operator cancelled it while " +
			"it was waiting for a lock it never took")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("CheckHost = %v, want the cancellation surfaced", err)
	}

	// THE SPECIFIC BRANCH, because cancelling a preflight produces an error from
	// wherever it happened to land, and only one of those places is the collapse
	// under test. Without this the test passes when the cancel arrives early and
	// some other tart call fails on it — which proves nothing about the switch.
	if !strings.Contains(err.Error(), "could not finish proving the store lock") {
		t.Errorf("CheckHost = %v, want the refusal to come from the store-lock branch; the "+
			"cancellation landed somewhere else, so this run says nothing about it", err)
	}
}

// The refusal on a non-macOS host names the platform, because "executable file
// not found in $PATH" sends an operator hunting for an install that cannot
// exist there.
func TestCheckHostRefusesANonDarwinHostByName(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.goos = "linux"

	_, err := p.CheckHost(t.Context())
	if err == nil || !strings.Contains(err.Error(), "needs a macOS host, and this is linux") {
		t.Fatalf("CheckHost = %v, want a refusal naming the platform", err)
	}
}

// A missing binary is named with the install command, not left as a bare exec
// error.
func TestCheckHostNamesTheInstallForAMissingBinary(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.goos = "darwin"
	p.tart = s.bin + "-not-installed"

	_, err := p.CheckHost(t.Context())
	if err == nil || !strings.Contains(err.Error(), "brew install openai/tools/tart") {
		t.Fatalf("CheckHost = %v, want a refusal naming the install", err)
	}
}

// The real binary, when present: the version answers, the exec capability
// probe passes, and the real store parses. This is the line that notices a
// tart release changing any of the three.
func TestRealTartCheckHost(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	realHome(t)

	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	report, err := p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost against the real tart: %v", err)
	}

	if report.Version == "" {
		t.Error("the real tart reported no version")
	}

	// THE LOCK WAS ACTUALLY PLACED, not merely reported. A host that cannot
	// serialize name mutations cannot tear a guest down, and discovering that on
	// the first teardown of the day holds a lease's capacity while somebody works
	// out why — so the preflight proves it, which means the file is on disk.
	if report.StoreLock == "" {
		t.Fatal("CheckHost reported no store lock path, so `billet check` cannot tell an " +
			"operator which collision domain this node joined")
	}

	if _, err := os.Stat(report.StoreLock); err != nil {
		t.Errorf("CheckHost named %s as the store lock and nothing is there (%v), so the "+
			"placeability it claims to prove was never proved", report.StoreLock, err)
	}
}

// fakeSoftnet writes an executable and points the provider at it. The stat
// seam is what lets a test describe ownership it could not otherwise create
// without running the suite as root.
func fakeSoftnet(t *testing.T, p *Provider, mode os.FileMode, uid uint32) {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "softnet")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake softnet: %v", err)
	}

	p.softnet = bin
	p.softnetStat = func(string) (os.FileMode, uint32, error) { return mode, uid, nil }
}

// THE SUCCESS CASE, asserted rather than logged. Every other softnet test
// asserts a refusal, so without this a mutation that always answered "not
// configured" would pass the whole file.
func TestSoftnetSetuidRootIsAConfiguredGrant(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	fakeSoftnet(t, p, 0o755|os.ModeSetuid, 0)

	report := p.checkSoftnet()
	if !report.GrantConfigured {
		t.Fatalf("a setuid binary owned by root was not reported as configured: %s", report.Why)
	}
}

// THE FAILURE THIS CHECK EXISTS FOR: a setuid bit on a binary the installing
// user owns. Everything looks configured — the `s` is right there in `ls` — and
// softnet still refuses with "root privileges are required to run", because
// setuid confers the OWNER's privileges and the owner is not root. Measured on
// a real Mac after exactly that mistake.
func TestSoftnetOwnedByTheInstallingUserIsNotAGrant(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	fakeSoftnet(t, p, 0o755|os.ModeSetuid, 501)

	report := p.checkSoftnet()
	if report.GrantConfigured {
		t.Fatal("a setuid binary owned by a non-root user was reported as a working grant")
	}

	if !strings.Contains(report.Why, "owned by uid 501") {
		t.Errorf("Why = %q, want it to name the ownership as the problem", report.Why)
	}

	// BOTH COMMANDS, IN ORDER. chown CLEARS the setuid bit — measured on macOS
	// — so advising chown alone leaves the binary root-owned and still unusable,
	// which reads as the fix having done nothing.
	chown := strings.Index(report.Why, "chown root")
	chmod := strings.Index(report.Why, "chmod u+s")

	switch {
	case chown < 0 || chmod < 0:
		t.Errorf("Why = %q, want both the chown and the chmod", report.Why)
	case chmod < chown:
		t.Errorf("Why = %q, want chmod AFTER chown: chown clears the setuid bit", report.Why)
	}
}

// A binary with no setuid bit at all takes the same ordered pair.
func TestSoftnetWithoutSetuidNamesBothCommandsInOrder(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	fakeSoftnet(t, p, 0o755, 0)

	report := p.checkSoftnet()
	if report.GrantConfigured {
		t.Fatal("a plain executable was reported as a working grant")
	}

	chown := strings.Index(report.Why, "chown root")
	chmod := strings.Index(report.Why, "chmod u+s")

	if chown < 0 || chmod < 0 || chmod < chown {
		t.Errorf("Why = %q, want chown then chmod", report.Why)
	}
}

// A softnet BILLET DID NOT FIND BESIDE TART must never be handed to an operator
// as a plain instruction to make setuid-root. `softnet` comes from PATH, so a
// writable directory earlier in it would otherwise turn this check's own output
// into a local privilege-escalation recipe.
func TestSoftnetFromPATHCarriesAWarningBeforeAnyGrantCommand(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	// The stub tart lives elsewhere, so this candidate is not from that install.
	fakeSoftnet(t, p, 0o755, 501)

	report := p.checkSoftnet()
	if report.Trusted {
		t.Fatal("a softnet in an unrelated directory was reported as tart's own")
	}

	if !strings.Contains(report.Why, "verify") {
		t.Errorf("Why = %q, want the caution to come before the privilege command", report.Why)
	}

	// The caution has to PRECEDE the command an operator would copy.
	if v, c := strings.Index(report.Why, "verify"), strings.Index(report.Why, "chown"); c >= 0 && v > c {
		t.Errorf("Why = %q, want the warning before the chown", report.Why)
	}
}

// Any path billet prints into a shell command is quoted, because a path with a
// space or a metacharacter would otherwise change what the operator runs.
func TestSoftnetRemediationQuotesThePath(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	dir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	bin := filepath.Join(dir, "softnet")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	p.softnet = bin
	p.softnetStat = func(string) (os.FileMode, uint32, error) { return 0o755, 0, nil }

	// The RESOLVED path is what gets printed, and on macOS a temp dir resolves
	// through /var -> /private/var, so quoting the original would compare
	// against a path the check never emits.
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if why := p.checkSoftnet().Why; !strings.Contains(why, shellQuote(resolved)) {
		t.Errorf("Why = %q, want the path shell-quoted as %s", why, shellQuote(resolved))
	}
}

// An absent softnet is a state, not an error: a node running only trusted tiers
// needs none of it, so the check reports and does not refuse.
func TestSoftnetAbsentIsReportedNotFatal(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)
	p.softnet = filepath.Join(t.TempDir(), "definitely-not-installed")
	p.goos = "darwin"

	report, err := p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost refused a host with no softnet: %v", err)
	}

	if report.Softnet.GrantConfigured {
		t.Error("an absent softnet was reported as usable")
	}

	if !strings.Contains(report.Softnet.Why, "not installed") {
		t.Errorf("Why = %q, want it to say softnet is not installed", report.Softnet.Why)
	}
}

// THE SYMLINK IS NOT THE FILE. Homebrew puts bin/softnet in front of a Cellar
// path, and the ownership that decides whether softnet can run is the target's
// — a check that stats the link would read the wrong file's mode.
func TestSoftnetIsInspectedThroughItsSymlink(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	dir := t.TempDir()
	target := filepath.Join(dir, "softnet-0.23.0")
	link := filepath.Join(dir, "softnet")

	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake softnet: %v", err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p.softnet = link

	// Resolved on BOTH sides: macOS puts a test's temp dir under /var, which is
	// itself a symlink to /private/var, so comparing a resolved answer against
	// an unresolved expectation fails for a reason unrelated to the check.
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve the expected path: %v", err)
	}

	if got := p.checkSoftnet().Path; got != want {
		t.Errorf("Path = %q, want the resolved target %q", got, want)
	}
}

// The real softnet on a real Mac, when it is there: this is the line that
// notices Homebrew resetting ownership on an upgrade.
func TestRealSoftnetGrant(t *testing.T) {
	if _, err := exec.LookPath("softnet"); err != nil {
		t.Skip("softnet is not installed")
	}

	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	report := p.checkSoftnet()
	t.Logf("softnet %s: configured=%v trusted=%v %s",
		report.Path, report.GrantConfigured, report.Trusted, report.Why)

	if report.Path == "" {
		t.Error("softnet is on PATH but the check could not resolve it")
	}
}
