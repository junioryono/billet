package tart

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// These tests boot a REAL guest and drive the real delivery path.
//
// realtart_test.go proves billet's command shapes against the tart binary using
// OS-less VMs; nothing there boots an operating system, so nothing there can
// answer the question this file exists for:
//
//	DOES THE RUNNER SURVIVE THE EXEC SESSION THAT STARTED IT?
//
// `tart exec` reaches a guest agent over a unix socket, and billet hands the
// runner its registration through it. If that agent kills its session's process
// group when the exec call returns — which is what a well-behaved agent
// plausibly does — then every runner billet starts dies milliseconds after
// launch, `tart list` still says the VM is running, and billet reports a
// launched runner for a job that sits queued until GitHub gives up. That is the
// docker default-command failure exactly, and no unit test can see it: the fake
// guest in tart_test.go runs the script under a shell that has no session
// semantics to violate.
//
// The comment on deliverRegistration promised this measurement before the
// delivery path is trusted on hardware. This is it.
//
// A Linux arm64 guest rather than macOS: the process-group question is about
// the AGENT, which is the same program in both, and a Linux image is a few
// hundred megabytes against a macOS image's tens of gigabytes. The macOS
// acceptance still needs its own run on a real Mac node.

// defaultGuestImage is the image these tests boot unless one is named. Linux
// by default because it is a few hundred megabytes against a macOS image's
// tens of gigabytes, and the guest agent is the same program in both.
const defaultGuestImage = "ghcr.io/cirruslabs/ubuntu:latest"

// guestImage is the image under test. Overridable so the SAME assertions can be
// run against a macOS guest — which is the configuration billet actually
// exists to serve, and the only one that exercises the perl POSIX::setsid
// fallback, since macOS ships no setsid binary:
//
//	BILLET_TART_GUEST_TESTS=1 \
//	BILLET_TART_GUEST_IMAGE=ghcr.io/cirruslabs/macos-tahoe-base:latest \
//	go test -run TestRealGuest ./internal/provider/tart/
func guestImageRef() string {
	if v := os.Getenv("BILLET_TART_GUEST_IMAGE"); v != "" {
		return v
	}

	return defaultGuestImage
}

// requireGuestImage skips unless tart and the pulled image are both present,
// and returns a provider bound to the AMBIENT tart store.
//
// Not an isolated TART_HOME, unlike realtart_test.go: the image lives in the
// operator's own OCI cache, and pointing these tests at an empty temp store
// would make every clone a fresh multi-gigabyte pull. The VMs are created in
// that shared store, which is why every one is billet-named and removed by its
// own cleanup.
func requireGuestImage(t *testing.T) *Provider {
	t.Helper()

	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	if os.Getenv("BILLET_TART_GUEST_TESTS") == "" {
		t.Skip("set BILLET_TART_GUEST_TESTS=1 to boot a real guest (needs " +
			guestImageRef() + " pulled, and takes a minute)")
	}

	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// PROVEN PRESENT rather than assumed: billet refuses to pull inside a
	// launch, so an absent image would otherwise surface as a confusing launch
	// failure in the middle of a test about something else.
	if _, err := p.resolveImage(t.Context(), guestImageRef()); err != nil {
		t.Skipf("pull the guest image first: `tart pull %s` (%v)", guestImageRef(), err)
	}

	return p
}

// bootedGuest launches one VM through the provider and returns its name. The
// image is cloned rather than the provider's own Launch used, because Launch
// delivers a registration and these tests want to drive delivery themselves.
func bootedGuest(t *testing.T, p *Provider, name string) {
	t.Helper()

	bootedGuestWith(t, p, name)
}

// bootedGuestWith boots a guest with extra `tart run` flags, so a test can ask
// for an isolated network without duplicating the boot dance.
func bootedGuestWith(t *testing.T, p *Provider, name string, runArgs ...string) {
	t.Helper()

	if _, err := p.run(t.Context(), "clone", guestImageRef(), name); err != nil {
		t.Fatalf("clone: %v", err)
	}

	t.Cleanup(func() { removeGuest(t, p, name) })

	if err := p.writeOwner(name); err != nil {
		t.Fatalf("writeOwner: %v", err)
	}

	// THROUGH BILLET'S OWN BOOT PATH, flags included. This used to run tart
	// directly because startDetached took no extra arguments, which meant the one
	// test that boots an isolated guest proved nothing about how billet boots
	// one. Now the isolation flags travel the same code the node uses.
	if err := p.startDetached(name, runArgs...); err != nil {
		t.Fatalf("startDetached %s with %v: %v", name, runArgs, err)
	}

	// The guest agent answers only once the guest is up. Waiting for it IS the
	// readiness signal billet uses in production (deliverRegistration retries on
	// exactly this), so waiting the same way here keeps the test honest.
	deadline := time.Now().Add(3 * time.Minute)

	for {
		if err := p.execStdin(t.Context(), name, "", "/bin/sh", "-c", "exit 0"); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("the guest agent never answered: %v", err)
		}

		time.Sleep(2 * time.Second)
	}
}

// removeGuest tears a test VM down on a fresh bounded context, and DELETES ONLY
// what it could prove stopped.
//
// The test's own context is already cancelled by cleanup time, so a derived one
// would refuse every call; and an unbounded one can hang a suite forever.
// Deleting a VM whose stop failed would destroy the evidence that it is still
// running, which is the same rule Destroy follows in production — so an
// unproved VM is left in place with the exact command to remove it.
func removeGuest(t *testing.T, p *Provider, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	defer cancel()

	if _, err := p.run(ctx, "stop", name); err != nil && !isNotRunning(err) && !isMissingVM(err) {
		t.Logf("cleanup could not stop %s (%v); leaving it in place — "+
			"remove it with `tart delete %s` once it is stopped", name, err, name)

		return
	}

	if _, err := p.run(ctx, "delete", name); err != nil && !isMissingVM(err) {
		t.Logf("cleanup could not delete %s: %v; remove it with `tart delete %s`",
			name, err, name)
	}
}

// guestSays runs a command in the guest and returns its stdout.
func guestSays(t *testing.T, p *Provider, name, script string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), p.tart, "exec", name, "/bin/sh", "-c", script)

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "TART_HOME="+p.home)

	if err := cmd.Run(); err != nil {
		t.Fatalf("guest exec %q: %v: %s", script, err, stderr.String())
	}

	return strings.TrimSpace(stdout.String())
}

// guestSpawnFile is where the test runner records one line per process it
// starts, inside the guest. Counting lines is portable; `pgrep -f` is not —
// macOS pgrep has no -c, so the first macOS run reported an EMPTY count and the
// suite read that as "no runner survived" when the runner was in fact alive.
const guestSpawnFile = "billet-test-spawns"

// probeCommand is a stand-in runner: it records itself and then stays up.
func probeCommand() []string {
	return []string{"/bin/sh", "-c",
		`printf '%s\n' "$$" >> "$HOME/` + guestSpawnFile + `"; exec sleep 600`}
}

// runnerIsAlive asks the guest the SAME question billet's own proof asks —
// is the announced pid still the announced process — rather than matching a
// process name, which differs between guest kinds and changes under exec.
func runnerIsAlive(t *testing.T, p *Provider, name string) bool {
	t.Helper()

	out := guestSays(t, p, name, birthFunc+`p=$(cat "$HOME/`+runnerPIDFile+`" 2>/dev/null || true)
b=$(cat "$HOME/`+runnerBirthFile+`" 2>/dev/null || true)
if [ -z "$p" ] || [ -z "$b" ]; then echo no; exit 0; fi
if ! kill -0 "$p" 2>/dev/null; then echo no; exit 0; fi
now=$(billet_birth "$p" || true)
if [ "$now" = "$b" ]; then echo yes; else echo no; fi`)

	return strings.TrimSpace(out) == "yes"
}

// guestSpawnCount is how many runners actually started in the guest.
func guestSpawnCount(t *testing.T, p *Provider, name string) int {
	t.Helper()

	out := guestSays(t, p, name,
		`wc -l < "$HOME/`+guestSpawnFile+`" 2>/dev/null || echo 0`)

	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("unreadable spawn count %q: %v", out, err)
	}

	return n
}

// A GUEST THAT CANNOT RESOLVE IS A GUEST THAT CANNOT WORK, and softnet is what
// breaks it.
//
// Measured, both ways, on a real Mac: under tart's default NAT a guest resolves
// through the DHCP-assigned resolver (the vmnet gateway) and reaches GitHub;
// under softnet that same path is blocked and name resolution fails — while
// ping and TCP/443 both keep working, so nothing looks wrong until every job
// fails to clone. `curl https://github.com` returns 000 while `curl --resolve`
// to the same address returns 200.
//
// The fix is a PUBLIC resolver, and this test is what stops that requirement
// from being a sentence in a comment: it configures one, then proves the thing
// a runner actually has to do — resolve github.com and clone over HTTPS.
//
// Whoever ends up owning that configuration (the guest image, or billet when it
// starts driving softnet per guest), this test fails if the result stops
// holding.
func TestASoftnetGuestCanResolveAndReachGitHub(t *testing.T) {
	p := requireGuestImage(t)

	if os.Getenv("BILLET_TART_SOFTNET_TESTS") == "" {
		t.Skip("set BILLET_TART_SOFTNET_TESTS=1 to test guest isolation " +
			"(needs softnet installed and root-owned setuid — `billet check` reports it)")
	}

	const name = "billet-dnsguest"

	bootedGuestWith(t, p, name, "--net-softnet")

	// FIRST, PROVE SOFTNET IS ACTUALLY ON. Configuring a public resolver and
	// then watching DNS work would pass identically under default NAT, an
	// ignored flag, or no isolation at all — the test would stay green while the
	// mechanism it is named for disappeared. The distinctive signature is the
	// one that was measured: the DHCP-assigned gateway resolver does not answer
	// and nothing else resolves either, while TCP to public addresses is fine.
	// The signature is EGRESS FINE, NAME RESOLUTION DEAD — the combination that
	// was consistent across every measurement. Probing the gateway resolver
	// directly was tried first and is not usable: `dig @gateway` reports no
	// answer even under default NAT, where systemd-resolved is meanwhile
	// resolving through that very gateway, so it flags an isolated guest and a
	// NAT one alike.
	//
	// SPELT PORTABLY, because the same assertions have to run against a macOS
	// guest: `timeout` and bash's /dev/tcp are Linux spellings, and `getent`
	// does not exist on macOS — the pgrep -c lesson, one file over, where a
	// Linux-only probe came back empty and was read as a finding.
	signature := guestSays(t, p, name, `if ! curl -s -o /dev/null --max-time 8 https://1.1.1.1/; then
  echo no-egress
elif command -v getent >/dev/null 2>&1 && getent hosts github.com >/dev/null 2>&1; then
  echo resolves-anyway
elif command -v dscacheutil >/dev/null 2>&1 && dscacheutil -q host -a name github.com 2>/dev/null | grep -q ip_address; then
  echo resolves-anyway
else
  echo isolated
fi`)

	if signature != "isolated" {
		t.Fatalf("this guest does not look isolated (%s), so softnet was not in effect "+
			"and nothing below would prove anything", signature)
	}

	// BILLET'S OWN RESOLVER PATH, not a hand-written stand-in. The first version
	// of this test configured systemd-resolved itself and asserted the outcome,
	// which proved that A public resolver fixes softnet's blocked gateway — a
	// fact about softnet rather than about billet, and it stayed green no matter
	// what billet did. Driving resolverScript makes the mechanism billet ships
	// the thing that has to work, on whichever guest this image is.
	configured := guestSays(t, p, name, resolverScript(config.DefaultUntrustedDNS(), p.resolverWaits))

	if configured != "ok" {
		t.Fatalf("billet could not give the isolated guest a working resolver: %s", configured)
	}

	// THE THING A RUNNER ACTUALLY DOES. Resolution alone would not catch a
	// blocked TLS handshake or a stateful-flow rule dropping the transfer.
	clone := guestSays(t, p, name,
		`rm -rf /tmp/billet-clone
if git clone -q --depth 1 https://github.com/octocat/Hello-World.git /tmp/billet-clone 2>&1; then
  echo cloned
else
  echo "clone failed"
fi`)

	if !strings.Contains(clone, "cloned") {
		t.Errorf("an isolated guest could not clone over HTTPS: %s", clone)
	}
}

// THE MEASUREMENT the delivery path's correctness rests on.
//
// A runner started through billet's real delivery script must still be running
// after the exec session that started it has ended. If it is not, the nohup and
// subshell orphaning are not enough and the delivery needs a different
// mechanism — setsid in the guest, a systemd transient unit, or the guest
// image owning the runner as a service.
func TestTheRunnerSurvivesTheExecSessionThatStartedIt(t *testing.T) {
	p := requireGuestImage(t)

	// The VM name IS the spec name: Launch clones to spec.Name, so delivery
	// targets it too. Booting one name and delivering to another was a test bug
	// that retried forever against a VM that did not exist.
	const name = "billet-guestsurvive"

	bootedGuest(t, p, name)

	// A stand-in for the runner: a process that lives well past the delivery and
	// is findable by an argument no other process carries. `sleep` is the right
	// shape here — the question is about the SESSION, not about what the process
	// does — and the marker makes the pgrep exact.
	spec := validSpec(name)
	spec.Command = probeCommand()

	if err := p.deliverRegistration(t.Context(), spec); err != nil {
		t.Fatalf("deliverRegistration: %v", err)
	}

	// The delivery has returned, so the exec session is over. Anything the agent
	// was going to do to the process group has happened by the time a fresh
	// session can be established and answer.
	time.Sleep(3 * time.Second)

	if !runnerIsAlive(t, p, name) {
		body := guestSays(t, p, name, `cat "$HOME/`+runnerLog+`" 2>/dev/null || echo "(no log)"`)
		t.Fatalf("THE RUNNER DID NOT SURVIVE ITS DELIVERY SESSION.\n"+
			"The guest agent tears down its exec session's process group, so whatever "+
			"detachment this build uses is not escaping it.\nrunner log:\n%s", body)
	}

	// And exactly one started — a detector that cannot count is how the first
	// macOS run reported a dead runner that was alive.
	if n := guestSpawnCount(t, p, name); n != 1 {
		t.Errorf("the guest started %d runners, want exactly 1", n)
	}

	t.Log("the runner survived the delivery session")

	// And the credential reached it through the ENVIRONMENT rather than argv,
	// which is the other half of the delivery contract and is only checkable
	// inside a real guest: /proc/<pid>/cmdline is what another process on the
	// guest could read.
	// /proc is Linux-only. On a macOS guest another process's argv needs
	// privileges billet's runner account does not have, so the check reports
	// that it could not look rather than passing silently.
	cmdline := guestSays(t, p, name, `if [ -d /proc ]; then
  for pid in $(pgrep -f 'billet-surv[i]vor-marker'); do tr '\0' ' ' < "/proc/$pid/cmdline"; echo; done
else
  echo NO-PROC
fi`)

	if strings.TrimSpace(cmdline) == "NO-PROC" {
		t.Log("guest has no /proc; the argv check is Linux-only and was skipped")
	}

	// PRESENCE ONLY. Printing the offending command line would put the very
	// credential this asserts is hidden into the test log — and this fixture is
	// synthetic today, but the assertion outlives the fixture.
	if strings.Contains(cmdline, spec.JITConfig) {
		t.Error("the registration is visible in the guest's own process list " +
			"(value withheld); it must reach the runner through the environment")
	}
}

// The sentinel must hold across REAL deliveries in a real guest: a redelivery
// after an ambiguous exec must not start a second runner, because two runners
// consuming one single-use registration is one job's capacity running two
// processes.
func TestARedeliveryInARealGuestStartsNoSecondRunner(t *testing.T) {
	p := requireGuestImage(t)

	const name = "billet-guestonce"

	bootedGuest(t, p, name)

	spec := validSpec(name)
	spec.Command = probeCommand()

	for range 3 {
		if err := p.deliverRegistration(t.Context(), spec); err != nil {
			t.Fatalf("deliverRegistration: %v", err)
		}
	}

	time.Sleep(3 * time.Second)

	if n := guestSpawnCount(t, p, name); n != 1 {
		t.Errorf("three deliveries produced %d runners, want exactly 1", n)
	}

	if !runnerIsAlive(t, p, name) {
		t.Error("the one runner that started is not alive")
	}
}

// Digest pinning against a REAL pulled tag: the round-3 review's residual. The
// fail-closed switch and the phrasings were proven with stubs; this proves the
// happy path — a pulled tag resolves to a digest, and cloning that digest
// works.
func TestRealTartResolvesAPulledTagToADigest(t *testing.T) {
	p := requireGuestImage(t)

	resolved, err := p.resolveImage(t.Context(), guestImageRef())
	if err != nil {
		t.Fatalf("resolveImage on a pulled tag: %v", err)
	}

	if !strings.Contains(resolved, "@sha256:") {
		t.Fatalf("resolveImage = %q, want a digest-qualified reference", resolved)
	}

	// The pin is only worth anything if tart can clone what it returned.
	const name = "billet-digestclone"

	if _, err := p.run(t.Context(), "clone", resolved, name); err != nil {
		t.Fatalf("clone the resolved digest %q: %v", resolved, err)
	}

	// A lease-named VM without a marker is one List refuses to reconcile
	// against, by design — so the clone is marked, exactly as Launch marks its
	// staging clone before renaming it.
	if err := p.writeOwner(name); err != nil {
		t.Fatalf("writeOwner: %v", err)
	}

	t.Cleanup(func() { removeGuest(t, p, name) })

	if _, ok, err := p.Find(t.Context(), name); err != nil || !ok {
		t.Errorf("the digest-cloned VM is not in inventory: %v, %v", ok, err)
	}
}

// xcodeImage is the Xcode-capable macOS image the guest contract asks to be
// proven. It is a 68 GB pull onto a 140 GB virtual disk, which is why it has
// its own switch rather than riding on BILLET_TART_GUEST_TESTS.
const xcodeImage = "ghcr.io/cirruslabs/macos-tahoe-xcode:latest"

// THE XCODE ACCEPTANCE: a real iOS target, built by a real xcodebuild, inside a
// guest billet launched through its ordinary path.
//
// `xcodebuild -version` would prove only that a binary exists. What has to be
// true for billet to serve iOS CI is that a compile and a link actually
// complete on the simulator SDK, in a VM that billet cloned, marked, booted,
// delivered a registration to, and can tear down again.
//
// Measured on an M-series host: boot to guest agent ~20s, `swift build` 14s,
// `xcodebuild` for iphonesimulator 15s, on Xcode 26.5 / macOS 26.4.
func TestRealGuestBuildsAnXcodeProject(t *testing.T) {
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}

	if os.Getenv("BILLET_TART_XCODE_TESTS") == "" {
		t.Skip("set BILLET_TART_XCODE_TESTS=1 to build with a real Xcode guest " +
			"(needs " + xcodeImage + " pulled — a 68GB image)")
	}

	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.resolveImage(t.Context(), xcodeImage); err != nil {
		t.Skipf("pull the Xcode image first: `tart pull %s` (%v)", xcodeImage, err)
	}

	spec := validSpec("billet-xcodeacceptance")
	spec.Image = xcodeImage
	spec.Command = probeCommand()
	// Xcode needs room; the 4GiB floor is a minimum, not a working size.
	spec.VCPU = 4
	spec.Memory = 8 << 30
	spec.Disk = 0

	t.Cleanup(func() { removeGuest(t, p, spec.Name) })

	// The whole boot rides inside Launch, because delivery retries until the
	// guest agent answers.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	if _, err := p.Launch(ctx, spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !runnerIsAlive(t, p, spec.Name) {
		t.Fatal("the runner is not alive in the Xcode guest")
	}

	t.Logf("toolchain: %s", guestSays(t, p, spec.Name,
		`printf '%s / %s' "$(sw_vers -productVersion)" "$(xcodebuild -version | head -1)"`))

	// A REAL COMPILE UNIT. An empty file would link without exercising the
	// frontend, and a version string proves nothing at all.
	build := guestSays(t, p, spec.Name, `set -e
rm -rf ~/billet-xcode-acceptance
mkdir -p ~/billet-xcode-acceptance/App
cd ~/billet-xcode-acceptance
cat > App/main.swift <<'SWIFT'
import Foundation
struct Build { let name: String; func greet() -> String { "billet built \(name)" } }
print(Build(name: "on tart").greet())
SWIFT
cat > Package.swift <<'PKG'
// swift-tools-version:5.9
import PackageDescription
let package = Package(name: "App", targets: [.executableTarget(name: "App", path: "App")])
PKG
xcodebuild -scheme App -sdk iphonesimulator -destination "generic/platform=iOS Simulator" \
  -derivedDataPath /tmp/billet-dd build 2>&1 | tail -3`)

	if !strings.Contains(build, "BUILD SUCCEEDED") {
		t.Fatalf("the iOS target did not build:\n%s", build)
	}

	// And the ordinary teardown still works on a guest this large.
	teardown, err := p.Destroy(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped", teardown)
	}
}

// A full provider Launch against a real guest, end to end: clone the pinned
// digest, mark it, boot it, deliver the registration, see the runner running,
// then destroy it and watch it go.
func TestRealGuestLaunchRunsARunnerAndDestroyRemovesIt(t *testing.T) {
	p := requireGuestImage(t)

	spec := validSpec("billet-lease3")
	spec.Image = guestImageRef()
	spec.Command = probeCommand()
	// MEASURED FLOOR: Virtualization.framework refuses a macOS VM under 4GiB
	// ("LessThanMinimalResourcesError: VM should have 4294967296 bytes of
	// memory at minimum"), so a probe sized like a Linux guest fails before it
	// boots. Disk is left at the image's own size.
	spec.VCPU = 2
	spec.Memory = 4 << 30
	spec.Disk = 0

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := p.Destroy(ctx, spec.Name); err != nil {
			t.Logf("cleanup destroy: %v", err)
		}
	})

	// Launch's own delivery retries while the guest boots, so this call carries
	// the whole boot. The provider's production timeout is the node's command
	// timeout; this test's is the context's.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	inst, err := p.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !inst.Running {
		t.Error("Launch returned an instance that does not report running")
	}

	if !runnerIsAlive(t, p, spec.Name) {
		t.Error("Launch returned but the guest's runner is not alive")
	}

	if n := guestSpawnCount(t, p, spec.Name); n != 1 {
		t.Errorf("the guest started %d runners after one Launch, want 1", n)
	}

	teardown, err := p.Destroy(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Errorf("Teardown = %v, want stopped", teardown)
	}

	if _, ok, err := p.Find(t.Context(), spec.Name); err != nil || ok {
		t.Errorf("Find after destroy = %v, %v; want absent", ok, err)
	}

	if _, err := os.Stat(p.vmDir(spec.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the VM directory survived Destroy: %v", err)
	}
}
