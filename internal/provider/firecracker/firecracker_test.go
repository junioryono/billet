package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// THE HEADLINE FINDING, and the one that would be silently wrong in production.
//
// The jailer canonicalises --exec-file and names its chroot after the RESULT, so a
// provider that used the configured path's basename would build and enumerate
// `<base>/firecracker/…` while every real jail sat in `<base>/firecracker-v1.16.1/…`.
// The reference host installs the binary exactly that way on purpose.
//
// The consequence is not a failed launch. List reads that directory, and an empty
// read is an inventory with nothing in it — which the control plane acts on by
// freeing the capacity of every lease absent from it, while the guests those leases
// paid for keep running.
func TestTheJailIsNamedAfterTheResolvedBinaryNotTheSymlink(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	j := h.p.jailFor(theInstance)

	if base := filepath.Base(filepath.Dir(j.dir())); base != "firecracker-v1.16.1" {
		t.Errorf("the jail directory is named %q; the jailer will use the resolved binary's "+
			"name, firecracker-v1.16.1", base)
	}

	if strings.Contains(j.dir(), "/firecracker/") {
		t.Errorf("the jail is under the SYMLINK's name, which is not where the jailer puts "+
			"it: %s", j.dir())
	}
}

// AND A BINARY THE JAILER WOULD REFUSE IS REFUSED AT CONSTRUCTION, once, rather
// than on every launch.
func TestABinaryTheJailerWouldRefuseIsCaughtAtConstruction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	renamed := filepath.Join(dir, "vmm")

	if err := os.WriteFile(renamed, []byte("#!/bin/true\n"), 0o600); err != nil {
		t.Fatalf("write the stub binary: %v", err)
	}

	_, err := New("deployment-a", config.FirecrackerConfig{
		BinaryPath:  renamed,
		JailerPath:  filepath.Join(dir, "jailer"),
		KernelImage: renamed,
		ChrootBase:  dir,
		JailUser:    "billet",
		Bridge:      "br0",
	}, &fakeDisk{}, withJailUser(1000, 1000))
	if err == nil {
		t.Fatal("New accepted a binary whose name the jailer will not exec")
	}

	if !strings.Contains(err.Error(), "does not contain") {
		t.Errorf("the error does not say what the jailer requires: %v", err)
	}
}

// A CHROOT BASE WHOSE SOCKET PATH WOULD NOT FIT IS REFUSED AT CONSTRUCTION.
//
// A unix socket address is a fixed-size field, and a VMM's socket sits six
// components below the base. The VMM is unaffected — inside the jail the same
// socket is `/run/firecracker.socket` — so firecracker binds it happily and billet,
// dialling by the full path from outside, gets `bind: invalid argument` per launch,
// after the root disk has already been cloned, naming no field.
//
// Found by this package's own tests failing that way before the check existed.
func TestAChrootBaseThatWouldNotFitASocketIsRefused(t *testing.T) {
	t.Parallel()

	deep := filepath.Join(shortDir(t), strings.Repeat("d", 120))

	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("stage a deep chroot base: %v", err)
	}

	bin := filepath.Join(shortDir(t), "firecracker-v1.16.1")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o600); err != nil {
		t.Fatalf("write the stub binary: %v", err)
	}

	_, err := New("deployment-a", config.FirecrackerConfig{
		BinaryPath: bin, JailerPath: bin, KernelImage: bin,
		ChrootBase: deep, JailUser: "billet", Bridge: "br0",
	}, &fakeDisk{}, withJailUser(1000, 1000))
	if err == nil {
		t.Fatal("New accepted a chroot base whose api socket billet could never dial")
	}

	if !strings.Contains(err.Error(), "chroot_base") {
		t.Errorf("the error does not name the field to shorten: %v", err)
	}
}

// AND THE CHECK IS MADE AGAINST A FULL-LENGTH LEASE ID, not whichever one happened
// to be launched first — or the answer would depend on the job.
func TestTheSocketBudgetIsMeasuredAgainstAFullLengthLease(t *testing.T) {
	t.Parallel()

	// A base sized so that a 32-character lease id overflows and a short one does
	// not. The check must refuse it regardless.
	base := "/" + strings.Repeat("b", maxSocketPath-88+1)

	if err := checkSocketPath(base, "firecracker-v1.16.1"); err == nil {
		t.Errorf("checkSocketPath accepted a base that only fits a shorter lease id than alloc "+
			"mints (%d characters)", leaseIDLength)
	}
}

// A LAUNCH IS ONLY A LAUNCH IF THE VMM SAYS SO.
//
// Measured on the reference host: `jailer --daemonize` exits 0 for a VM that died
// during startup, leaving a pid file and an API socket behind. That is the same
// shape as the docker default-command bug — every signal reported success and no
// runner ever started — so this asserts billet reaches the opposite conclusion.
func TestAVMMThatDiesOnStartIsAFailedLaunchEvenThoughTheJailerExitedZero(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.onJailer = func(id string) {
		vmm := h.serveVMM(t, id)
		vmm.dieOnStart = true
	}

	inst, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatalf("Launch reported success for a microVM that died on startup: %+v", inst)
	}

	// AND IT HANDED THE CAPACITY BACK. A launch that failed must not leave a clone
	// holding pool space, because nothing above will ever look for it: no jail
	// carries its name once the unwind has removed it.
	if got := h.disk.discards(); len(got) != 1 || got[0] != theInstance {
		t.Errorf("the root disk of a failed launch was not discarded, got %v", got)
	}
}

// THE CREDENTIAL IS IN THE METADATA SERVICE AND NOWHERE ELSE.
//
// Not in argv, where every process on the host reads it out of /proc — the mistake
// the container backend documents at length — and not on a disk, where it would
// outlive the read. This asserts both halves, because either alone passes while the
// other leaks.
func TestTheRunnerRegistrationIsOnlyEverInTheMetadataService(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	if argv := h.everyArgument(); strings.Contains(argv, theCredential) {
		t.Errorf("the runner registration reached a command line, where any local process can "+
			"read it:\n%s", argv)
	}

	body, called := vmm.bodyPut("/mmds")
	if !called {
		t.Fatal("the metadata service was never given the registration")
	}

	if !strings.Contains(renderJSON(t, body), theCredential) {
		t.Errorf("the metadata does not carry the registration: %v", body)
	}
}

// AND IT IS THERE BEFORE THE GUEST EXISTS.
//
// This is the whole reason the backend drives the API instead of handing firecracker
// a --config-file: a config file starts the machine as it is read, so the credential
// would be placed into a guest that is already running and could already have asked.
func TestTheCredentialIsPlacedBeforeTheGuestIsStarted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	paths := vmm.pathsPut()

	mmds, start := indexOf(paths, "/mmds"), indexOf(paths, "/actions")
	if mmds < 0 || start < 0 {
		t.Fatalf("the launch did not both configure metadata and start the guest: %v", paths)
	}

	if mmds > start {
		t.Errorf("the guest was started before the registration was placed: %v", paths)
	}

	// AND THE SERVICE ITSELF IS CONFIGURED FIRST, or the data has nowhere to be
	// served from and the guest reads nothing.
	if cfg := indexOf(paths, "/mmds/config"); cfg < 0 || cfg > mmds {
		t.Errorf("the metadata service was not configured before it was filled: %v", paths)
	}
}

// V2, WHICH IS A SECURITY PROPERTY RATHER THAN A VERSION NUMBER. Under V1 any
// process in the guest reads the metadata with a bare GET, so a workflow step could
// take the registration; V2 requires a session token first. Billet insists on the
// same thing for the instances its ec2 backend launches.
func TestTheMetadataServiceRequiresASessionToken(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	body, called := vmm.bodyPut("/mmds/config")
	if !called {
		t.Fatal("the metadata service was never configured")
	}

	version, isString := body["version"].(string)
	if !isString || version != "V2" {
		t.Errorf("the metadata service is configured as %q; V1 lets any process in the guest "+
			"read the registration with a bare GET", body["version"])
	}
}

// EVERY LAUNCH CREATES A CGROUP, and the reason is measured rather than tidy: the
// jailer makes one only when it is given at least one --cgroup, and once any VM on
// the host has been started with one, a later VM started WITHOUT one fails outright
// with `CgroupMove … Resource busy`. The two forms cannot coexist on a machine.
func TestEveryLaunchPassesACgroupArgument(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	if !h.ranWith("--cgroup") {
		t.Errorf("the jailer was invoked without --cgroup, so it creates no cgroup — and a "+
			"host that has ever launched one WITH it then refuses this: %s", h.everyArgument())
	}

	if !h.ranWith("--cgroup-version") {
		t.Errorf("the jailer was invoked without --cgroup-version: %s", h.everyArgument())
	}
}

// THE VMM IS DETACHED FROM BILLET. A node that restarts must leave running jobs
// running — that is what restart recovery adopts — so a VMM that died with its
// launcher would fail every build on the host on every upgrade.
func TestTheVMMOutlivesTheProcessThatStartedIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	if !h.ranWith("--daemonize") {
		t.Errorf("the jailer was not asked to daemonize, so the microVM dies with billet: %s",
			h.everyArgument())
	}
}

// THE GUEST GETS WHAT THE LEASE PAID FOR, in the units the VMM counts in.
func TestTheGuestIsSizedFromTheSpec(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	body, called := vmm.bodyPut("/machine-config")
	if !called {
		t.Fatal("the guest was never sized")
	}

	vcpu, isNumber := body["vcpu_count"].(float64)
	if !isNumber || int(vcpu) != 2 {
		t.Errorf("vcpu_count is %v, want 2", body["vcpu_count"])
	}

	// 2 GiB EXPRESSED IN MiB, because that is the only unit the api takes. A
	// backend that sent bytes here would ask for two billion MiB and be refused,
	// and one that sent GiB would hand a job a five-hundredth of its memory.
	mem, isNumber := body["mem_size_mib"].(float64)
	if !isNumber || int64(mem) != 2048 {
		t.Errorf("mem_size_mib is %v, want 2048", body["mem_size_mib"])
	}

	// SMT OFF. A vCPU the ledger charged for is a core's worth of capacity, and
	// handing a guest two hyperthreads of one core while the ledger recorded two
	// vCPU over-commits the machine by exactly the amount nobody can see.
	if smt, ok := body["smt"].(bool); !ok || smt {
		t.Errorf("smt is %v, want false", body["smt"])
	}
}

// A MEMORY SIZE UNDER A MEGABYTE IS REFUSED RATHER THAN ROUNDED TO ZERO. Integer
// division would hand the VMM `mem_size_mib: 0`, which is a guest with no memory
// reported as a successful launch.
func TestAMemorySizeTooSmallToExpressIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.onJailer = func(id string) { h.serveVMM(t, id) }

	spec := aSpec()
	spec.Memory = 512 * config.KiB

	if _, err := h.p.Launch(t.Context(), spec); err == nil {
		t.Fatal("Launch accepted a guest with less than a megabyte of memory")
	}
}

// A SPEC THAT WOULD PRODUCE A MICROVM THAT CANNOT RUN THE JOB IS REFUSED, and each
// case is refused for its own reason rather than by one catch-all.
func TestASpecThatCannotRunAJobIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*provider.Spec)
		says   string
	}{
		{"no name", func(s *provider.Spec) { s.Name = "" }, "needs a name"},
		{"a name that is not a lease", func(s *provider.Spec) { s.Name = "something-else" },
			"does not name a lease"},
		{"no image", func(s *provider.Spec) { s.Image = "" }, "has no image"},
		{"no registration", func(s *provider.Spec) { s.JITConfig = "" }, "no JIT config"},
		{"no command", func(s *provider.Spec) { s.Command = nil }, "has no command"},
		{"no vcpu", func(s *provider.Spec) { s.VCPU = 0 }, "vCPU"},
		{"no memory", func(s *provider.Spec) { s.Memory = 0 }, "of memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)

			spec := aSpec()
			tc.change(&spec)

			_, err := h.p.Launch(t.Context(), spec)
			if err == nil {
				t.Fatal("Launch accepted it")
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the error does not say why (%q): %v", tc.says, err)
			}

			// AND NOTHING WAS STARTED. A refusal that has already cloned a disk or
			// run the jailer is not a refusal, it is a leak with an error attached.
			if cmds := h.commands(); len(cmds) != 0 {
				t.Errorf("a refused spec still ran %v", cmds)
			}
		})
	}
}

// UNTRUSTED WORK NEEDS A NETWORK OF ITS OWN, and its absence is what refuses it.
//
// The isolation a microVM provides is the KERNEL, not the bridge it is attached to.
// A guest on the ordinary bridge reaches whatever that bridge reaches, which on the
// reference host is the Ceph cluster, the control-plane database and an overlay
// network that reaches production.
func TestUntrustedWorkIsRefusedUntilItHasABridgeOfItsOwn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if err := h.p.Accepts(provider.TrustUntrusted); err == nil {
		t.Fatal("a fork's pull request was accepted onto the trusted bridge")
	}

	if err := h.p.Accepts(provider.TrustUnknown); err == nil {
		t.Fatal("work billet could not classify was accepted")
	}

	if err := h.p.Accepts(provider.TrustTrusted); err != nil {
		t.Errorf("trusted work was refused: %v", err)
	}

	// AND IT IS ACCEPTED ONCE ONE IS DESCRIBED — the other direction, without which
	// this passes against a backend that simply refuses everything.
	withBridge := newHarness(t, func(p *Provider) { p.cfg.UntrustedBridge = "br-untrusted" })

	if err := withBridge.p.Accepts(provider.TrustUntrusted); err != nil {
		t.Errorf("untrusted work was refused although a bridge was named for it: %v", err)
	}
}

// AND THE REFUSAL IS ENFORCED AT LAUNCH, not only when a caller asks politely. A
// backend that only refuses through Accepts is not a boundary.
func TestLaunchRefusesUntrustedWorkItsOwn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	spec := aSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := h.p.Launch(t.Context(), spec); err == nil {
		t.Fatal("Launch ran untrusted work that Accepts refuses")
	}
}

// AN UNTRUSTED GUEST GOES ON THE UNTRUSTED BRIDGE. The check above proves it is
// admitted; this proves it lands where the operator said, which is the half that
// actually contains it.
func TestAnUntrustedGuestIsAttachedToTheUntrustedBridge(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(p *Provider) { p.cfg.UntrustedBridge = "br-untrusted" })
	h.onJailer = func(id string) { h.serveVMM(t, id) }

	spec := aSpec()
	spec.Trust = provider.TrustUntrusted

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	var attached string

	for _, r := range h.commands() {
		if len(r.args) >= 6 && r.args[0] == "link" && r.args[4] == "master" {
			attached = r.args[5]
		}
	}

	if attached != "br-untrusted" {
		t.Errorf("the guest was attached to bridge %q, not to the untrusted one", attached)
	}
}

// DESTROY TAKES ALL FOUR THINGS AWAY. A guest leaves behind a VMM, a jail, a tap on
// the host bridge and a root disk that is both a mapped kernel device and pool
// space — measured, SIGTERM takes only the first.
func TestDestroyRemovesEverythingAGuestLeavesBehind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	if _, err := os.Stat(j.dir()); err != nil {
		t.Fatalf("the jail was not built: %v", err)
	}

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if _, err := os.Stat(j.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail survived the destroy, so the jailer will refuse this lease a second "+
			"microVM: %v", err)
	}

	if got := h.disk.discards(); len(got) != 1 || got[0] != theInstance {
		t.Errorf("the root disk was not discarded, got %v", got)
	}

	deleted := false

	for _, r := range h.commands() {
		if len(r.args) >= 4 && r.args[0] == "link" && r.args[1] == "del" {
			deleted = true
		}
	}

	if !deleted {
		t.Errorf("the tap device was not removed, so the next launch for this lease collides "+
			"with it: %s", h.everyArgument())
	}
}

// AND IT IS IDEMPOTENT, because teardown runs on paths that have already failed
// once. A second destroy must not turn a recoverable state into a stuck one.
func TestDestroyingTwiceIsNotAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	vmm.stop()

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("the first Destroy: %v", err)
	}

	if err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Errorf("the second Destroy: %v", err)
	}
}

// A NAME BILLET DID NOT ASSIGN IS NOT DESTROYED. Destroy removes a directory tree
// and a network device, so a name that does not encode a lease is refused rather
// than acted on.
func TestDestroyRefusesANameBilletDidNotAssign(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if err := h.p.Destroy(t.Context(), "somebody-elses-vm"); err == nil {
		t.Fatal("Destroy acted on a name billet never assigned")
	}

	if err := h.p.Destroy(t.Context(), ""); err == nil {
		t.Fatal("Destroy acted on an empty id")
	}
}

// FIND ANSWERS FOR A MICROVM, AND SAYS SO WHEN THERE IS NONE.
func TestFindReportsAMicroVMAndItsAbsence(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	inst, found, err := h.p.Find(t.Context(), theInstance)
	if err != nil {
		t.Fatalf("Find before any launch: %v", err)
	}

	if found {
		t.Fatalf("Find invented a microVM: %+v", inst)
	}

	h.launch(t)

	inst, found, err = h.p.Find(t.Context(), theInstance)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if !found {
		t.Fatal("Find missed a running microVM")
	}

	if !inst.Running {
		t.Error("Find reported a running microVM as not running")
	}
}

// ANOTHER BILLET'S MICROVM IS NOT THIS ONE'S TO REPORT OR DESTROY.
//
// The chroot base is one directory shared by every billet on the machine, so
// without the owner marker two installations enumerate each other's guests — and
// List feeds a loop that destroys. This is the job the docker backend's owner label
// and the ec2 backend's owner tag do.
func TestAnotherDeploymentsMicroVMIsInvisible(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	// The same chroot base, a different deployment.
	other := &fakeDisk{device: "/dev/rbd1"}

	stranger, err := New("deployment-b", h.p.cfg, other, withJailUser(1000, 1000))
	if err != nil {
		t.Fatalf("New for the second deployment: %v", err)
	}

	found, err := stranger.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(found) != 0 {
		t.Errorf("a second billet on this machine can see %d of the first's microVMs: %+v",
			len(found), found)
	}

	if _, ok, err := stranger.Find(t.Context(), theInstance); err != nil || ok {
		t.Errorf("Find crossed a deployment boundary: found=%v err=%v", ok, err)
	}
}

// LIST FAILS RATHER THAN REPORTING A SHORTER INVENTORY.
//
// The control plane frees the capacity of any lease ABSENT from this list, so a row
// silently dropped is capacity handed back for a guest that is still executing a
// job — the failure the inventory exists to prevent, caused by the report meant to
// prevent it.
func TestListFailsRatherThanOmittingAMicroVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	// A marker billet cannot read is not evidence that the jail is somebody else's.
	if err := os.Chmod(j.ownerFile(), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() {
		//nolint:errcheck // restoring a temp file so the tree can be removed
		_ = os.Chmod(j.ownerFile(), 0o600)
	})

	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file, so this cannot be staged")
	}

	if _, err := h.p.List(t.Context()); err == nil {
		t.Error("List reported an inventory although it could not read who owns a jail in it")
	}
}

// A JAIL WITH NO MARKER IS REPORTED, NOT SKIPPED. build writes the marker before
// anything else, so a jail without one is a launch interrupted between the mkdir and
// the first write — billet's own, possibly with a mapped root disk behind it, and
// skipping it would leave that unreclaimable.
func TestAnInterruptedLaunchIsStillReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	j := h.p.jailFor(theInstance)
	if err := os.MkdirAll(j.root(), 0o700); err != nil {
		t.Fatalf("stage a half-built jail: %v", err)
	}

	found, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(found) != 1 || found[0].Name != theInstance {
		t.Fatalf("a half-built jail was not reported: %+v", found)
	}

	if found[0].Running {
		t.Error("a jail with no vmm behind it was reported as running")
	}
}

// A CHROOT BASE THAT DOES NOT EXIST YET IS EMPTY, NOT AN ERROR. A node that has
// never launched anything has no such directory, and failing there makes the first
// sweep on a fresh host a failure.
func TestListOnAHostThatHasLaunchedNothingIsEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	found, err := h.p.List(t.Context())
	if err != nil {
		t.Fatalf("List on a fresh host: %v", err)
	}

	if len(found) != 0 {
		t.Errorf("List invented %d microVMs on a host that has launched none", len(found))
	}
}

// "COULD NOT TELL" IS REPORTED AS RUNNING, and that asymmetry is the Instance
// contract's. The caller destroys what is not running, so only an answer that
// PROVES the VMM is gone may make this false — a timeout or a permission error
// would otherwise force-kill a job that is running perfectly well.
func TestAVMMBilletCannotReachIsReportedAsRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	j := h.p.jailFor(theInstance)

	// A socket that exists and hangs: connected, and no answer. That is "cannot
	// tell", not "gone".
	ln := hangingSocket(t, j.socket())
	defer ln.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	running, err := h.p.running(ctx, j)
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if !running {
		t.Error("a microVM billet could not get an answer from was reported as not running, " +
			"which is what makes the caller destroy it")
	}
}

// AND A VMM THAT IS PROVABLY GONE IS REPORTED AS GONE, or its lease is held open
// forever for a job that finished.
func TestAStoppedVMMIsReportedAsNotRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	vmm.stop()

	inst, found, err := h.p.Find(t.Context(), theInstance)
	if err != nil || !found {
		t.Fatalf("Find after the vmm stopped: found=%v err=%v", found, err)
	}

	if inst.Running {
		t.Error("a stopped microVM is still reported as running")
	}
}

// A SOCKET ANSWERING FOR A DIFFERENT MICROVM IS NOT THIS ONE RUNNING. Reporting
// true would hold a lease open for a guest that belongs to another lease entirely.
func TestASocketAnsweringForAnotherMicroVMIsNotThisOne(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	vmm.mu.Lock()
	vmm.id = provider.InstanceName("0000000000000000000000000000dead")
	vmm.mu.Unlock()

	// NEITHER ANSWER IS AVAILABLE HERE, which is what an error is for: saying the
	// guest has stopped would make the caller destroy this lease's jail and disk on
	// the strength of a socket billet has just established belongs to something
	// else.
	running, err := h.p.running(t.Context(), h.p.jailFor(theInstance))
	if err == nil {
		t.Errorf("a socket answering for a different microVM produced a verdict (running=%v) "+
			"rather than a refusal", running)
	}
}

// `Not started` IS NOT RUNNING. It is this backend's `created` container: it has a
// socket, a pid and a jail, and it will never run anything, because whatever would
// have started it is gone.
func TestAConfiguredButUnstartedGuestIsNotRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, vmm := h.launch(t)

	vmm.mu.Lock()
	vmm.state = "Not started"
	vmm.mu.Unlock()

	running, err := h.p.running(t.Context(), h.p.jailFor(theInstance))
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if running {
		t.Error("a guest that was never started is reported as running, which holds its lease " +
			"open for a job that cannot begin")
	}
}

// A JAIL LEFT BY AN EARLIER RUN IS NAMED AS THE CAUSE. The jailer's own answer is
// about /dev/net/tun, which names a device rather than the leftover directory that
// produced it.
func TestARelaunchOverALeftoverJailSaysWhatIsInTheWay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.launch(t)

	_, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatal("Launch reused a jail the jailer would have refused")
	}

	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the error does not name the leftover jail: %v", err)
	}
}

// A TAP NAME FITS IN A NETWORK DEVICE NAME, and it is derived from the lease so
// teardown can find it again with nothing but the id.
func TestATapNameFitsTheKernelsLimit(t *testing.T) {
	t.Parallel()

	name := tapName(theInstance)

	if len(name) > maxIfName {
		t.Errorf("the tap name %q is %d characters; the kernel's limit is %d",
			name, len(name), maxIfName)
	}

	if !strings.HasPrefix(name, tapPrefix) {
		t.Errorf("the tap name %q does not mark the device as billet's", name)
	}

	// DERIVED, NOT RANDOM: Destroy recomputes it from the id alone.
	if again := tapName(theInstance); again != name {
		t.Errorf("tapName is not deterministic: %q then %q", name, again)
	}

	// AND TWO LEASES DO NOT SHARE ONE. The truncation makes that a probability
	// rather than a guarantee, so this pins the part that is guaranteed: the name
	// varies with the lease.
	other := tapName(provider.InstanceName("0000000000000000000000000000beef"))
	if other == name {
		t.Errorf("two leases produced the same tap name %q", name)
	}
}

// A FAILED LAUNCH UNWINDS WHAT IT MADE, and says what it could not.
func TestAFailedLaunchLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.jailerErr = errors.New("jailer refused")

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch reported success although the jailer failed")
	}

	if _, err := os.Stat(h.p.jailFor(theInstance).dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed launch left its jail behind, which blocks every retry: %v", err)
	}

	if got := h.disk.discards(); len(got) != 1 {
		t.Errorf("a failed launch did not discard its root disk, got %v", got)
	}
}

// AND THE REASON THE LAUNCH FAILED SURVIVES THE CLEANUP. A cleanup failure that
// replaced the cause would send an operator to the wrong problem entirely.
func TestACleanupFailureDoesNotHideWhyTheLaunchFailed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.jailerErr = errors.New("jailer refused for the original reason")
	h.disk.discardErr = errors.New("and the disk could not be discarded")

	_, err := h.p.Launch(t.Context(), aSpec())
	if err == nil {
		t.Fatal("Launch reported success")
	}

	for _, must := range []string{"original reason", "could not be discarded"} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("the error does not carry %q: %v", must, err)
		}
	}
}

// A ROOT DISK THAT COULD NOT BE CLONED IS A LAUNCH FAILURE AND NOTHING ELSE — no
// jail, no tap, no jailer.
func TestAMissingGoldenImageStartsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.disk.cloneErr = errors.New("no such golden image")

	if _, err := h.p.Launch(t.Context(), aSpec()); err == nil {
		t.Fatal("Launch continued without a root disk")
	}

	if cmds := h.commands(); len(cmds) != 0 {
		t.Errorf("billet ran %v although there was no disk to boot from", cmds)
	}
}

// THE VMM IS DROPPED TO THE ACCOUNT THE CONFIGURATION NAMED, and the numbers are
// the ones resolved from it. The jailer takes a uid and a gid rather than a name,
// so a backend that passed the wrong pair would run every guest's VMM as somebody
// else — which for uid 0 is the whole reason the jailer is used at all.
func TestTheJailerIsToldToDropToTheResolvedAccount(t *testing.T) {
	t.Parallel()

	h := newHarness(t, withJailUser(4242, 4343))
	h.launch(t)

	var uid, gid string

	for _, r := range h.commands() {
		for i, a := range r.args {
			if i+1 >= len(r.args) {
				continue
			}

			switch a {
			case "--uid":
				uid = r.args[i+1]
			case "--gid":
				gid = r.args[i+1]
			}
		}
	}

	if uid != "4242" || gid != "4343" {
		t.Errorf("the jailer was told to drop to uid %q gid %q, not to the resolved account: %s",
			uid, gid, h.everyArgument())
	}

	// AND THE TAP IS HANDED TO THE SAME ACCOUNT. The VMM opens it after dropping
	// privileges, so a device left owned by root produces `Open tap device failed:
	// Operation not permitted` — which reads like a bug in the VMM and is not.
	if !h.ranWith("4242") {
		t.Errorf("the tap device was not given to the jailed account: %s", h.everyArgument())
	}
}

// LAUNCH ANSWERS WITH SOMETHING THE CALLER CAN ACT ON. Every one of these fields is
// read: the id is what Destroy is called with, the name is what reconciliation
// matches back to a lease, and Running is what decides whether a job is in progress.
func TestLaunchReturnsAnInstanceTheCallerCanActOn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	inst, _ := h.launch(t)

	if inst.ID != theInstance {
		t.Errorf("the instance id is %q; this backend has no handle but the name it chose", inst.ID)
	}

	if inst.Name != theInstance {
		t.Errorf("the instance name is %q", inst.Name)
	}

	if !inst.Running {
		t.Error("a launch that confirmed the guest is Running reported it as not running")
	}

	// AND THE ID IS WHAT Destroy TAKES, which is the property that makes the two
	// composable at all.
	if err := h.p.Destroy(t.Context(), inst.ID); err != nil {
		t.Errorf("Destroy could not act on the id Launch returned: %v", err)
	}
}

// THE PROVIDER REPORTS ITS OWN KIND, which is what placement compares a lease
// against.
func TestTheProviderNamesItsBackend(t *testing.T) {
	t.Parallel()

	if kind := newHarness(t).p.Kind(); kind != config.ProviderFirecracker {
		t.Errorf("Kind is %q", kind)
	}
}

// A PROVIDER WITH NO STORAGE IS REFUSED AT CONSTRUCTION. Every guest boots from a
// clone, so there is nothing to launch without one — and a nil interface would
// panic on the first job instead, in a control plane that bans panics.
func TestAProviderWithoutStorageIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, err := New("deployment-a", config.FirecrackerConfig{
		BinaryPath: filepath.Join(dir, "firecracker"), ChrootBase: dir,
	}, nil, withJailUser(1000, 1000))
	if err == nil {
		t.Fatal("New accepted a provider with no root disk source")
	}

	if !strings.Contains(err.Error(), "root disk") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// AND ONE WITH NO DEPLOYMENT IDENTITY IS TOO, for the reason the owner marker
// exists: without it every billet on the machine shares one namespace.
func TestAProviderWithoutADeploymentIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := New("  ", config.FirecrackerConfig{}, &fakeDisk{}); err == nil {
		t.Fatal("New accepted a provider with no deployment identity")
	}
}
