package tart

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// isolatedProvider is a stub-backed provider that has been told to isolate
// untrusted guests, which is the only way this backend runs them.
func isolatedProvider(t *testing.T, s *stub) *Provider {
	t.Helper()

	p := newProvider(t, s)
	WithConfig(config.TartConfig{UntrustedIsolation: config.IsolationSoftnet})(p)

	return p
}

// UNTRUSTED WORK IS REFUSED UNTIL SOMETHING CONFINES IT, and configuring that is
// the operator's assertion rather than billet's inference.
func TestUntrustedWorkIsRefusedWithoutConfiguredIsolation(t *testing.T) {
	p := newProvider(t, newStub(t))

	err := p.Accepts(provider.TrustUntrusted)
	if err == nil {
		t.Fatal("untrusted work was accepted on a node with no isolation configured")
	}

	// The diagnostic, not merely an error: an operator who reads this has to
	// learn which key turns it on.
	if !strings.Contains(err.Error(), "node.tart.untrusted_isolation") {
		t.Errorf("Accepts = %v, want the setting that enables it named", err)
	}
}

func TestUntrustedWorkIsAcceptedOnceIsolationIsConfigured(t *testing.T) {
	p := isolatedProvider(t, newStub(t))

	if err := p.Accepts(provider.TrustUntrusted); err != nil {
		t.Errorf("Accepts(untrusted) = %v, want it admitted under softnet", err)
	}
}

// UNKNOWN STAYS REFUSED, and that is a different judgement from untrusted:
// untrusted is a classification billet made and can place under a policy chosen
// for it, while unknown means billet could not classify the job at all.
func TestUnclassifiedWorkIsRefusedEvenWithIsolationConfigured(t *testing.T) {
	p := isolatedProvider(t, newStub(t))

	if err := p.Accepts(provider.TrustUnknown); err == nil {
		t.Fatal("work billet could not classify was accepted because isolation happened " +
			"to be configured, which is not what isolation was configured for")
	}
}

// A MECHANISM THIS BUILD CANNOT ENFORCE IS A REFUSAL, NOT A DEFAULT. The danger
// is a newer config read by an older binary: admitting the job and booting it
// with tart's default NAT would run untrusted work on the trusted network while
// reporting it as isolated.
func TestAnUnknownIsolationMechanismRefusesUntrustedWork(t *testing.T) {
	p := newProvider(t, newStub(t))
	p.cfg = config.TartConfig{UntrustedIsolation: "nftables-from-2027"}

	if err := p.Accepts(provider.TrustUntrusted); err == nil {
		t.Fatal("an isolation mechanism this build cannot enforce admitted untrusted work")
	}

	flags, err := p.netFlags(provider.TrustUntrusted)
	if err == nil {
		t.Fatalf("netFlags = %v, want a refusal rather than %d flags", flags, len(flags))
	}

	if len(flags) != 0 {
		t.Errorf("netFlags returned %v alongside its refusal; a caller that ignores the "+
			"error would boot with no isolation at all", flags)
	}
}

// THE FLAG REACHES tart run, which is the only thing that actually confines the
// guest. Asserting Accepts alone would leave the whole mechanism untested: a
// backend that admits the job and then boots it on the default network is the
// exact failure this pair of tests exists to catch.
func TestAnUntrustedGuestBootsUnderSoftnet(t *testing.T) {
	s := newStub(t)
	p := isolatedProvider(t, s)

	spec := validSpec("billet-lease1")
	spec.Trust = provider.TrustUntrusted

	// The launch itself is not the subject and may fail further on; the boot has
	// already happened by then, and its argv is what this asserts. Reported
	// rather than blanked, so a failure that moves EARLIER than the boot — which
	// would make the assertion below vacuous — is visible in the output.
	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Logf("Launch failed after the boot, which is expected here: %v", err)
	}

	if !strings.Contains(s.argv(t), "--net-softnet") {
		t.Errorf("an untrusted guest was booted without --net-softnet; tart's default is "+
			"shared NAT, which reaches the host and lets a guest spoof the bridge.\nargv:\n%s",
			s.argv(t))
	}
}

// AND A TRUSTED GUEST IS NOT CONFINED, which is not a nicety: softnet blocks the
// private address space, so applying it to trusted work would cut those guests
// off from the site's own services.
func TestATrustedGuestKeepsTheDefaultNetwork(t *testing.T) {
	s := newStub(t)
	p := isolatedProvider(t, s)

	if _, err := p.Launch(t.Context(), validSpec("billet-lease1")); err != nil {
		t.Logf("Launch failed after the boot, which is not what this asserts: %v", err)
	}

	// ANCHORED FIRST. "argv does not contain --net-softnet" is also true of a
	// launch that never booted anything, so without this the assertion below
	// passes for the wrong reason the moment anything upstream breaks.
	if !strings.Contains(s.argv(t), "run billet-lease1") {
		t.Fatalf("the guest was never booted, so the flags it booted with prove "+
			"nothing.\nargv:\n%s", s.argv(t))
	}

	if strings.Contains(s.argv(t), "--net-softnet") {
		t.Errorf("a trusted guest was confined to public destinations only.\nargv:\n%s", s.argv(t))
	}
}

// THE CONSTRUCTOR RE-APPLIES THE CONFIG RULES, because New is exported and a
// caller that built the struct in code never passed through config.Load. These
// values are interpolated into a shell script that runs inside the guest, so
// "every resolver parses as an IP address" is what makes that interpolation
// safe rather than a place to quote defensively and hope.
func TestNewRefusesAResolverThatIsNotAnAddress(t *testing.T) {
	s := newStub(t)

	_, err := New(testOwner, WithBinary(s.bin), WithHome(s.home),
		WithConfig(config.TartConfig{
			UntrustedIsolation: config.IsolationSoftnet,
			UntrustedDNS:       []string{"1.1.1.1; rm -rf /"},
		}))
	if err == nil {
		t.Fatal("New accepted a resolver that is not an IP address, and that value reaches " +
			"a shell inside the guest")
	}
}

// THE RESOLVER SCRIPT IS RUN, not read.
//
// Its first version asserted that the generated text CONTAINED the addresses
// and the string "echo failed", which passes if the verification is unreachable,
// deleted down to that literal, or reached and then exits successfully anyway.
// What has to hold is behavioural: a guest where the mechanisms do not take
// effect must make this script FAIL, because billet proceeds to deliver a
// single-use registration on the strength of its success.
//
// Driven against fake commands on PATH, so the shell does the deciding.
func TestTheResolverScriptFailsWhenResolutionNeverWorks(t *testing.T) {
	dir := t.TempDir()
	resolveFakes(t, dir, false)
	fakeCommand(t, dir, "sudo", "#!/bin/sh\nexit 0\n")

	out, err := runScript(t, dir, resolverScript([]string{"1.1.1.1", "8.8.8.8"}, 1))
	if err == nil {
		t.Fatalf("the script succeeded on a guest that never resolves: %s", out)
	}

	if !strings.Contains(out, "failed") {
		t.Errorf("script output = %q, want it to say it failed", out)
	}
}

// And the other direction, so the failure above cannot be "it always fails".
func TestTheResolverScriptSucceedsOnceResolutionWorks(t *testing.T) {
	dir := t.TempDir()
	resolveFakes(t, dir, true)
	fakeCommand(t, dir, "sudo", "#!/bin/sh\nexit 0\n")

	out, err := runScript(t, dir, resolverScript([]string{"1.1.1.1"}, 1))
	if err != nil {
		t.Fatalf("the script failed on a guest that resolves: %v: %s", err, out)
	}

	if !strings.Contains(out, "ok") {
		t.Errorf("script output = %q, want ok", out)
	}
}

// A RESOLVER IS DATA, NEVER CODE, and this is the assertion that keeps it so.
//
// The first version of this backend argued that interpolating a resolver
// unquoted was safe because every one had parsed as an IP address. That was
// false: netip.ParseAddr accepts an IPv6 ZONE and puts no restriction on its
// contents, so `2001:db8::1%x;touch${IFS}/pwned` parses — and the value is
// written into a shell script that runs with sudo inside the guest. Zones are
// refused by config validation now, and this proves the SCRIPT holds anyway,
// because the next thing some parser tolerates should not be an execution.
func TestAResolverCannotEscapeIntoTheGuestShell(t *testing.T) {
	// EVERY BRANCH, because they are mutually exclusive and which one runs is
	// decided by what is on PATH. Testing whichever the host happens to select
	// leaves the other two carrying the same values with nothing asserted.
	for _, branch := range []struct {
		name string
		// fakes installs whatever selects this branch, and must arrange for
		// `ran` to be created when the branch is actually taken.
		fakes func(t *testing.T, dir, ran string)
	}{
		{"macos", func(t *testing.T, dir, ran string) {
			t.Helper()
			// This branch reaches its command THROUGH sudo, so the fake has to
			// exec rather than merely succeed.
			fakeCommand(t, dir, "sudo", "#!/bin/sh\n[ \"$1\" = \"-n\" ] && shift\nexec \"$@\"\n")
			// It must list a service, or the configuring loop body never runs.
			fakeCommand(t, dir, "networksetup", `#!/bin/sh
if [ "${1:-}" = "-listallnetworkservices" ]; then
  printf 'An asterisk denotes disabled\nWi-Fi\n'
  exit 0
fi
: > "`+ran+`"
`)
		}},
		{"systemd-resolved", func(t *testing.T, dir, ran string) {
			t.Helper()
			fakeCommand(t, dir, "resolvectl", "#!/bin/sh\nexit 0\n")
			// DISPATCHED ON ARGV. A sudo that records every invocation is
			// satisfied by the unconditional `sudo dscacheutil -flushcache` at
			// the end of the script — so the sentinel appeared even with the
			// whole branch deleted. Only this branch's own sink counts.
			fakeCommand(t, dir, "sudo", sudoSentinel("resolved.conf.d/billet.conf", ran))
		}},
		{"resolv.conf", func(t *testing.T, dir, ran string) {
			t.Helper()
			// Neither of the above installed, so the script falls through.
			fakeCommand(t, dir, "sudo", sudoSentinel("/etc/resolv.conf", ran))
		}},
	} {
		t.Run(branch.name, func(t *testing.T) {
			resolverInjectionCases(t, branch.fakes)
		})
	}
}

// resolverInjectionCases drives the hostile values through the branch the
// supplied fakes select, and PROVES that branch ran.
func resolverInjectionCases(t *testing.T, fakes func(t *testing.T, dir, ran string)) {
	t.Helper()

	dir := t.TempDir()
	realTools(t, dir)
	resolveFakes(t, dir, false)
	fakeCommand(t, dir, "sudo", "#!/bin/sh\nexit 0\n")

	ran := filepath.Join(dir, "branch-ran")
	fakes(t, dir, ran)

	marker := filepath.Join(dir, "pwned")

	// THE PAYLOAD USES ONLY SHELL BUILTINS, and that is not a detail. The first
	// version ran `touch`, which is not on the PATH this harness gives the
	// script — so the injection fired, created nothing, and the test passed.
	// Mutation-tested: with `touch` the unquoted-interpolation mutant SURVIVED.
	// A redirection needs no binary and cannot be disarmed by a narrow PATH.
	//
	// Every syntactic context the addresses reach: the positional list itself,
	// the macOS argument list, the resolved.conf printf, and the resolv.conf
	// loop.
	for _, hostile := range []string{
		"2001:db8::1%x;: > " + marker,
		"1.1.1.1' ; : > " + marker + " ; echo '",
		"$(: > " + marker + ")",
		"`: > " + marker + "`",
	} {
		if _, err := runScript(t, dir, resolverScript([]string{hostile}, 1)); err == nil {
			t.Errorf("the script succeeded with resolver %q, which it cannot have resolved",
				hostile)
		}

		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("resolver %q executed inside the guest shell", hostile)
		}

		// THE NAMED BRANCH ACTUALLY RAN. The three are mutually exclusive, so
		// without this a subtest can pass having exercised a different one —
		// and deleting a branch's setup would leave all three green.
		if _, err := os.Stat(ran); err != nil {
			t.Fatalf("the configuration branch under test never ran (%v), so resolver %q "+
				"was never put through it", err, hostile)
		}

		if err := os.Remove(ran); err != nil {
			t.Fatalf("reset the branch sentinel: %v", err)
		}
	}
}

// fakeCommand installs an executable that stands in for a guest binary.
func fakeCommand(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// runScript runs a generated guest script with the fakes IN FRONT of the real
// system path.
//
// Not fakes only: the script legitimately pipes through `tail` and `sed`, and a
// PATH without them made the macOS branch fail silently and record nothing —
// the test then reported that networksetup was never called, which was true and
// not what it was testing.
//
// The fakes still shadow everything that decides an outcome, and `resolveFakes`
// below shadows BOTH resolution commands: on a Mac the real dscacheutil is on
// that system path, and reaching it would make these tests depend on whether
// this machine can resolve github.com.
func runScript(t *testing.T, dir, script string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + dir, "HOME=" + dir}

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// realTools links the coreutils the script genuinely needs into the fake
// directory, so PATH can hold ONLY what a test put there.
//
// Keeping the system path on the end was simpler and wrong: it let a branch be
// selected by whatever the host happened to have — a Mac's own resolvectl or
// networksetup — so a subtest could pass while running a different branch than
// the one it names, and deleting a branch's setup left all of them green.
func realTools(t *testing.T, dir string) {
	t.Helper()

	for _, tool := range []string{"tail", "sed", "grep", "cat", "sleep"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not available to build a controlled PATH: %v", tool, err)
		}

		if err := os.Symlink(path, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("link %s: %v", tool, err)
		}
	}
}

// sudoSentinel builds a fake sudo that records a sentinel ONLY when its
// arguments name the given sink, so a branch's marker cannot be created by some
// other sudo the script makes unconditionally.
func sudoSentinel(sink, ran string) string {
	return `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *` + sink + `*) : > "` + ran + `" ;;
  esac
done
exit 0
`
}

// resolveFakes shadows both ways the script can ask whether a name resolves,
// so a test decides the answer rather than the host does.
func resolveFakes(t *testing.T, dir string, resolves bool) {
	t.Helper()

	code := "1"
	if resolves {
		code = "0"
	}

	fakeCommand(t, dir, "getent", "#!/bin/sh\nexit "+code+"\n")
	// dscacheutil answers by PRINTING; the script greps for ip_address.
	body := "#!/bin/sh\nexit 1\n"
	if resolves {
		body = "#!/bin/sh\nprintf 'ip_address: 140.82.121.4\\n'\n"
	}

	fakeCommand(t, dir, "dscacheutil", body)
}

// unpulledImage is a reference the stub has never been told about, because
// newStub seeds testImage as already present.
const unpulledImage = "ghcr.io/cirruslabs/ubuntu-runner-arm64:latest"

// THE macOS BRANCH GETS THE ADDRESSES AS SEPARATE ARGUMENTS, which the tests
// above never reached: they install getent, so the script takes the Linux path
// and `networksetup` is never called at all.
//
// It matters because that branch is where quoting could plausibly go wrong in
// the OTHER direction — arriving as one argument, or with quotes attached,
// either of which macOS accepts silently and neither of which configures a
// resolver.
func TestTheMacOSBranchPassesEachResolverAsItsOwnArgument(t *testing.T) {
	dir := t.TempDir()
	realTools(t, dir)

	record := filepath.Join(dir, "args")

	// Neither resolution command answers, so the script runs its configuration
	// branch before giving up.
	resolveFakes(t, dir, false)
	fakeCommand(t, dir, "networksetup", `#!/bin/sh
if [ "${1:-}" = "-listallnetworkservices" ]; then
  printf 'An asterisk denotes disabled\nWi-Fi\n'
  exit 0
fi
# -setdnsservers <service> <addr>...: record one line per argument, so the test
# can tell "two addresses" from "one string containing a space".
shift 2
for a in "$@"; do printf '%s\n' "$a" >> "`+record+`"; done
`)
	fakeCommand(t, dir, "sudo", "#!/bin/sh\n[ \"$1\" = \"-n\" ] && shift\nexec \"$@\"\n")

	if _, err := runScript(t, dir, resolverScript([]string{"1.1.1.1", "8.8.8.8"}, 1)); err == nil {
		t.Fatal("the script reported success although nothing can resolve here")
	}

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("networksetup was never called with resolvers: %v", err)
	}

	want := "1.1.1.1\n8.8.8.8\n"
	if string(got) != want {
		t.Errorf("networksetup received %q, want %q — each address as its own bare argument",
			got, want)
	}
}

// A PULL THAT EXITS 0 IS NOT A PULL// A PULL THAT EXITS 0 IS NOT A PULL, and this is the rule that governs every
// other exit code in this backend, arriving in a new place.
//
// tart reclaims space from its own OCI cache to make an operation fit — it is
// documented on `tart pull --help` as automatic pruning — so on a tight disk a
// pull can succeed having evicted something else, and a later CLONE can evict
// what the pull just fetched. Measured: `billet images pull` reported an image
// pulled, and the first job that wanted it was refused because it was gone.
//
// Verifying afterwards turns that into a failure next to the download the
// operator just waited for, rather than a launch error an hour later.
func TestAPullThatLeavesNothingBehindIsAFailure(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	if err := os.WriteFile(s.pullEvicts, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm the eviction: %v", err)
	}

	// A reference the stub has never seen: newStub seeds testImage as already
	// pulled, which would make the verification below pass without a pull.
	err := p.Pull(t.Context(), unpulledImage, io.Discard)
	if err == nil {
		t.Fatal("Pull reported success for an image that is not in the store afterwards")
	}

	// The diagnostic, not merely an error: an operator who reads this has to
	// learn that the disk evicted it, rather than that the download failed.
	if !strings.Contains(err.Error(), "reclaims its own cache") {
		t.Errorf("Pull = %v, want the error to name automatic pruning as the cause", err)
	}
}

// And the ordinary path still works, so the guard above cannot pass by
// refusing everything.
func TestAPullPutsTheImageInTheStore(t *testing.T) {
	s := newStub(t)
	p := newProvider(t, s)

	if p.Pulled(t.Context(), unpulledImage) {
		t.Fatal("the image was already in the store, so this proves nothing")
	}

	if err := p.Pull(t.Context(), unpulledImage, io.Discard); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if !p.Pulled(t.Context(), unpulledImage) {
		t.Error("Pull succeeded and the image is not resolvable afterwards")
	}
}

// THE REGISTRATION MUST NOT COME BACK OUT ON STDERR.
//
// The credential goes in on the delivery's STDIN, and an earlier version of
// execStdin put the guest's stderr into the error it returned — which
// deliverRegistration embeds and the node writes to its log. A guest whose
// shell is three characters different (`cat >&2`) reflects a live JIT
// registration into that log, where nothing downstream can filter it: a secret
// out of its field is an opaque string.
//
// The stub here is exactly that guest.
func TestTheRegistrationNeverComesBackThroughStderr(t *testing.T) {
	s := newStub(t)

	if err := os.WriteFile(s.execReflects, []byte("1"), 0o600); err != nil {
		t.Fatalf("arm the reflecting guest: %v", err)
	}

	p := newProvider(t, s)
	p.execRetry = time.Millisecond

	spec := validSpec("billet-lease1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := p.Launch(ctx, spec)
	if err == nil {
		t.Fatal("Launch succeeded against a guest that only echoes its input")
	}

	// ANCHORED: the guest must actually have RECEIVED the registration. Without
	// this the test passes when delivery is deleted, or fails before sending
	// anything — proving only that some error came back with no secret in it.
	got, readErr := os.ReadFile(s.execStdin)
	if readErr != nil || !strings.Contains(string(got), spec.JITConfig) {
		t.Fatalf("the guest never received the registration (%v), so nothing could have "+
			"reflected it and this proves nothing", readErr)
	}

	if strings.Contains(err.Error(), spec.JITConfig) {
		t.Fatalf("the runner registration came back in the launch error, which the node "+
			"logs: %v", err)
	}
}

// AND NOTHING THE GUEST PRINTS ON STDOUT IS QUOTED EITHER.
//
// proveRunning reads the liveness script's stdout and used to render it with
// %q. That stdout is the guest's, and a job dispatched the moment the runner
// registers is running WHILE the proof is still sampling — so a job can choose
// those bytes. Only billet's own vocabulary may be echoed.
func TestAnUnrecognisedGuestAnswerIsNotQuoted(t *testing.T) {
	const canary = "ghp-CANARY-FROM-GUEST-STDOUT"

	// DRIVEN THROUGH proveRunning, not through the classifier alone: testing
	// the helper in isolation passes if proveRunning stops calling it and goes
	// back to quoting stdout, which is the regression that matters.
	s := newStub(t)

	if err := os.WriteFile(s.proveSays, []byte(canary), 0o600); err != nil {
		t.Fatalf("arm the talkative guest: %v", err)
	}

	p := newProvider(t, s)

	err := p.proveRunning(t.Context(), "billet-lease1")
	if err == nil {
		t.Fatal("proveRunning accepted an answer billet does not recognise as proof of life")
	}

	if strings.Contains(err.Error(), canary) {
		t.Errorf("the guest chose bytes in a billet error, which the node logs: %v", err)
	}

	// AND THE CANARY ACTUALLY REACHED THE PROOF. Without this the test passes on
	// any other failure — a missing pid, say — having never exercised the branch
	// it is named for. The unrecognised-answer verdict is what only the talkative
	// guest produces.
	if !strings.Contains(err.Error(), "does not recognise") {
		t.Errorf("proveRunning = %v, want it to report an answer it does not recognise; "+
			"the talkative guest's stdout never reached it", err)
	}

	// And the answers billet DOES know are still reported by name, so the guard
	// above cannot pass by refusing to say anything useful.
	if got := provedAnswer("dead"); !strings.Contains(got, "dead") {
		t.Errorf("provedAnswer(dead) = %q, want the recognised answer named", got)
	}
}

// A CORPSE THAT IS THE ONLY THING IN THE STORE MUST NOT WEDGE THE HOST.
//
// This is the state the whole-store check used to swallow: with one interrupted
// clone and nothing else, `ours == missing == 1` looked identical to "tart can
// see none of our store", so every launch, teardown and check failed until a
// human deleted the directory — which is the exact failure the per-VM lookup
// was added to remove, reached from the other side. The tolerance test could
// not see it, because it always put a healthy VM beside the corpse.
// A DAMAGED LEASE-NAMED DIRECTORY IS REFUSED, because it does not prove what it
// looks like it proves.
//
// `tart get` failing on the layout says tart cannot RECONSTRUCT the VM from its
// store. It does not say an already-running `tart run` has stopped: remove or
// corrupt a live guest's config.json and this is exactly the state, with the
// VMM still executing somebody's job. Dropping the row would free that lease's
// capacity and let a second job land on the machine underneath the first.
//
// So this one wedges the host until a person looks, and that is the direction
// to fail in. An earlier version of this backend treated it as a corpse, on the
// reasoning that tart cannot start what it cannot read — true, and about
// STARTING, which is not the question.
func TestListRefusesADamagedLeaseNamedDirectory(t *testing.T) {
	s := newStub(t)

	damaged := filepath.Join(s.home, "vms", "billet-damagedlease")
	if err := os.MkdirAll(damaged, 0o755); err != nil {
		t.Fatalf("plant the damaged VM: %v", err)
	}

	p := newProvider(t, s)

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("List accepted an inventory that omits a lease-named VM tart cannot read; " +
			"a running guest whose config is damaged looks exactly like this, and its " +
			"capacity would be freed underneath it")
	}

	if !strings.Contains(err.Error(), "billet-damagedlease") {
		t.Errorf("List = %v, want the directory named so an operator can go and look", err)
	}
}

func TestListToleratesACorpseThatIsTheOnlyThingInTheStore(t *testing.T) {
	s := newStub(t)

	orphan := filepath.Join(s.home, "vms", "billet-lonelycorpse"+stagingSuffix)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}

	p := newProvider(t, s)

	instances, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List refused a store whose only entry is an unreadable directory, "+
			"which wedges every launch on the host: %v", err)
	}

	if len(instances) != 0 {
		t.Errorf("List = %+v, want nothing: the only directory there is not a VM", instances)
	}
}
