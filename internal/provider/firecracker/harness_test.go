package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

const (
	testDeployment      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherTestDeployment = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// The whole package is tested against a FAKE VMM rather than a hypervisor, and
// the fake is a real unix socket serving real HTTP — because the thing worth
// testing is what billet says to a VMM and what it concludes from the answer,
// and a hand-written stub of that conversation would agree with billet's own
// mistakes. The two backends before this one each shipped a defect of exactly
// that shape.
//
// What no test here can reach is /dev/kvm and the jailer's chroot; those were
// measured on the reference host and the findings are recorded in the comments
// they justify.

// fakeVMM is a microVM's API socket, as firecracker actually answers it.
type fakeVMM struct {
	mu sync.Mutex

	id    string
	state string
	// puts records every configuration call IN ORDER, which is what proves the
	// credential was placed before the guest could ask for it.
	puts []recordedPut
	// refuse makes one path answer as firecracker does when it rejects a request.
	refuse map[string]string
	// dieOnStart makes the VMM stop answering when it is told to start, which is
	// the measured shape of a boot that fails after the jailer has already exited 0.
	dieOnStart bool
	// ignoreResumeFlag makes a snapshot load leave the guest running even when it
	// was asked to load it paused, which is the shape a restore must refuse.
	ignoreResumeFlag bool
	// stayPausedOnResume makes a resume answer success and leave the guest paused.
	stayPausedOnResume bool
	// createTakes is how long writing a snapshot holds its call open.
	createTakes time.Duration
	// root is the jail root the VMM is chrooted in, where a snapshot it is asked to
	// write lands, and onCreate runs after it has written one, so a test can do what
	// a VMM that has been taken over could do to its own files.
	root     string
	onCreate func(root string)
	// vcpus and memMiB are the guest shape it reports, as a loaded snapshot set it.
	vcpus  int
	memMiB int64

	listener net.Listener
	server   *http.Server
	stopped  bool
}

type recordedPut struct {
	method string
	path   string
	body   map[string]any
}

// startFakeVMM serves a VMM's API on the socket path a jail would put it at.
func startFakeVMM(t *testing.T, socket, id string) *fakeVMM {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("make the socket directory: %v", err)
	}

	// THE SHAPE aSpec ASKS FOR, which is what a snapshot of one of its guests loads as.
	v := &fakeVMM{id: id, state: "Not started", refuse: map[string]string{}, vcpus: 2, memMiB: 2048}

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}

	v.listener = ln
	v.server = &http.Server{Handler: v, ReadHeaderTimeout: time.Second}

	go func() {
		if err := v.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Not t.Errorf: this runs after the test may have finished.
			_ = err
		}
	}()

	t.Cleanup(v.stop)

	return v
}

func (v *fakeVMM) stop() {
	v.mu.Lock()
	already := v.stopped
	v.stopped = true
	v.mu.Unlock()

	if already {
		return
	}

	if err := v.server.Close(); err != nil {
		panic("closing a test vmm: " + err.Error())
	}
}

func (v *fakeVMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if r.Method == http.MethodGet && r.URL.Path == "/machine-config" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"vcpu_count": v.vcpus, "mem_size_mib": v.memMiB, "smt": false})

		return
	}

	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]string{
			"id": v.id, "state": v.state, "vmm_version": "1.16.1", "app_name": "Firecracker",
		})

		return
	}

	if message, refused := v.refuse[r.URL.Path]; refused {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"fault_message": message})

		return
	}

	// A body that does not decode is recorded as nil, which every assertion on it
	// then fails on — which is the right outcome for a request billet built wrong.
	var body map[string]any

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		body = nil
	}

	v.puts = append(v.puts, recordedPut{method: r.Method, path: r.URL.Path, body: body})

	// THE RUN STATE A REAL VMM KEEPS, measured on v1.16.1: a paused load reports
	// Paused, PATCH /vm moves between Paused and Running, and GET / says which.
	//
	// LITERALS, NOT billet's CONSTANTS: a fake that shared them would agree with a
	// wrong one. And only the method the real API takes for each path moves the state.
	switch {
	case r.Method == http.MethodPatch && r.URL.Path == "/vm":
		switch body["state"] {
		case "Paused":
			v.state = "Paused"
		case "Resumed":
			if !v.stayPausedOnResume {
				v.state = "Running"
			}
		}
	case r.Method == http.MethodPut && r.URL.Path == "/snapshot/load":
		if resume, isBool := body["resume_vm"].(bool); (isBool && resume) || v.ignoreResumeFlag {
			v.state = "Running"
		} else {
			v.state = "Paused"
		}
	case r.Method == http.MethodPut && r.URL.Path == "/snapshot/create":
		// THE WRITE HOLDS THE CALL, as Firecracker's does while it fsyncs memory.
		if v.createTakes > 0 {
			v.mu.Unlock()
			time.Sleep(v.createTakes)
			v.mu.Lock()
		}

		// AND LANDS IN THE CHROOT, at the paths it was given, as the jailed account.
		if v.root != "" {
			if err := v.writeSnapshot(body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}
		}
	}

	if r.URL.Path == "/actions" {
		action, isString := body["action_type"].(string)

		switch {
		case isString && action == "InstanceStart" && v.dieOnStart:
			// EXACTLY WHAT A FAILED BOOT LOOKS LIKE: the calls before this all
			// succeeded, the jailer has already exited 0, and the VMM is gone.
			go v.stop()

		case isString && action == "InstanceStart":
			v.state = stateRunning
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// currentState is the run state the VMM would report now.
func (v *fakeVMM) currentState() string {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.state
}

// calls lists the calls in the order they arrived, as "METHOD /path".
func (v *fakeVMM) calls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	out := make([]string, 0, len(v.puts))
	for _, p := range v.puts {
		out = append(out, p.method+" "+p.path)
	}

	return out
}

// pathsPut lists the configuration calls in the order they arrived.
func (v *fakeVMM) pathsPut() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	out := make([]string, 0, len(v.puts))
	for _, p := range v.puts {
		out = append(out, p.path)
	}

	return out
}

// bodyPut returns the body sent to a path, and whether it was ever called.
func (v *fakeVMM) bodyPut(path string) (map[string]any, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for _, p := range v.puts {
		if p.path == path {
			return p.body, true
		}
	}

	return nil, false
}

// fakeDisk stands in for the ceph store.
type fakeDisk struct {
	mu sync.Mutex

	device string
	// resolved is what ResolveGeneration answers, and resolveErr is what it refuses
	// with — the two halves of a tier naming `@verified`.
	resolved         string
	resolvedContract string
	resolveErr       error
	cloneErr         error

	// kernel is what a generation records as its paired kernel, and kernelErr is
	// what the lookup refuses with. An empty kernel means the generation records
	// none, which is the ordinary case for an image built by hand.
	kernel    string
	kernelErr error

	// cloneGone makes the next N clones fail as though the generation had been
	// removed, which is the race the launch re-resolves around.
	cloneGone int
	// resolvedAfter is what ResolveGeneration answers once cloneGone has fired.
	resolvedAfter string
	discarded     []string
	cloned        []string
	cloneSizes    []config.ByteSize
	discardErr    error
	// onClone runs inside CloneRoot, so a test can observe what the host looked
	// like at the moment the disk was cloned rather than afterwards.
	onClone func()
	// onDiscard runs inside DiscardRoot, so a test can change the host between the
	// discard and the releases a teardown makes after it.
	onDiscard func()
}

// resolve stands in for the store turning `@verified` into a generation. The fake
// answers with whatever it is given unless a test says otherwise, because almost
// every test is about something else.
func (d *fakeDisk) ResolveGeneration(_ context.Context, image, contract string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resolvedContract = contract

	if d.resolveErr != nil {
		return "", d.resolveErr
	}

	if d.resolved != "" {
		return d.resolved, nil
	}

	return image, nil
}

func (d *fakeDisk) CloneRoot(
	_ context.Context, image, name string, capacity config.ByteSize,
) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.onClone != nil {
		d.onClone()
	}

	if d.cloneErr != nil {
		return "", d.cloneErr
	}

	// THE GENERATION WAS REMOVED BETWEEN RESOLVING IT AND CLONING IT, which is the
	// race the launch re-resolves around.
	if d.cloneGone > 0 {
		d.cloneGone--

		if d.resolvedAfter != "" {
			d.resolved = d.resolvedAfter
		}

		return "", errGenerationGone
	}

	d.cloned = append(d.cloned, image+" -> "+name)
	d.cloneSizes = append(d.cloneSizes, capacity)

	return d.device, nil
}

func (d *fakeDisk) DiscardRoot(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.discarded = append(d.discarded, name)

	if d.onDiscard != nil {
		d.onDiscard()
	}

	return d.discardErr
}

func (d *fakeDisk) KernelFor(_ context.Context, _, _ string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.kernelErr != nil {
		return "", false, d.kernelErr
	}

	if d.kernel == "" {
		return "", false, nil
	}

	return d.kernel, true, nil
}

// errGenerationGone stands in for the store's missing-snapshot error.
var errGenerationGone = errors.New("no such image")

func (d *fakeDisk) GenerationGone(err error) bool { return errors.Is(err, errGenerationGone) }

// clonedFrom is the image reference the last clone was made from, which is how a
// test tells a resolved generation from the alias that named it.
func (d *fakeDisk) clonedFrom() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.cloned) == 0 {
		return ""
	}

	return d.cloned[len(d.cloned)-1]
}

func (d *fakeDisk) discards() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.discarded...)
}

// recordedRun is one command billet ran.
type recordedRun struct {
	bin  string
	args []string
}

// harness is a provider wired to fakes, plus what they recorded.
type harness struct {
	p    *Provider
	disk *fakeDisk

	mu   sync.Mutex
	runs []recordedRun
	// onJailer runs when the jailer is invoked, which is where a test starts the
	// fake VMM — the same ordering the real thing has.
	onJailer func(id string)
	refuse   func(bin string, args []string) error
	// jailerErr makes the jailer fail.
	jailerErr error
	// chowned is every path billet asked to chown, in order.
	chowned []string
	// devices is the host's network devices by name, and the uid the kernel reports
	// as each one's owner (-1 for a device that is not a tap). `ip tuntap add` and
	// `ip link del` change it as they would change the kernel's, and a test puts a
	// device billet did not make here to see what teardown does with it.
	devices map[string]int
	// ownerErr makes asking the kernel whose a device is fail.
	ownerErr error
}

// tapOwner is the kernel's answer for the fake host.
func (h *harness) tapOwner(tap string) (int, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.ownerErr != nil {
		return 0, false, h.ownerErr
	}

	owner, ok := h.devices[tap]

	return owner, ok, nil
}

// putDevice places a network device on the fake host, as something other than this
// launch would.
func (h *harness) putDevice(name string, owner int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.devices[name] = owner
}

// hasDevice reports whether the fake host still has a network device.
func (h *harness) hasDevice(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	_, ok := h.devices[name]

	return ok
}

// chownedPaths returns every path billet asked to chown.
func (h *harness) chownedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.chowned...)
}

// theLease is a lease id of the shape alloc mints: 32 hex characters.
const theLease = "9d7ab98c551bceec9a81e5da847f6837"

// theInstance is what billet names the compute for that lease.
var theInstance = provider.InstanceName(theLease)

// newHarness builds a provider whose every external effect is recorded.
//
// The binary is a REAL file behind a REAL symlink, because resolveExecName follows
// one and that resolution decides the directory List reads.
func newHarness(t *testing.T, opts ...Option) *harness {
	t.Helper()

	base := t.TempDir()
	bin := filepath.Join(base, "firecracker-v1.16.1")

	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o600); err != nil {
		t.Fatalf("write the stub binary: %v", err)
	}

	link := filepath.Join(base, "firecracker")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatalf("link the stub binary: %v", err)
	}

	// 0644 BECAUSE THAT IS WHAT A REAL ONE MUST BE. The preflight requires the guest
	// kernel to be readable by the unprivileged account each microVM runs as, since
	// billet hard-links it into a jail it does not own and deliberately does not take
	// ownership of it. A fixture at 0600 is a kernel no microVM could boot.
	//
	// This mattered rather than being tidiness: these tests only run where /dev/kvm
	// exists, so a 0600 fixture was green on the machine this was written on and
	// failed on the one the backend actually runs on.
	kernel := filepath.Join(base, "vmlinux")
	if err := os.WriteFile(kernel, []byte("not really a kernel"), 0o644); err != nil {
		t.Fatalf("write the stub kernel: %v", err)
	}

	// CHMOD RATHER THAN TRUSTING THE MODE ABOVE, because os.WriteFile applies it
	// through the umask — so on a machine with a stricter one the fixture would come
	// out 0600 again and this would fail somewhere else entirely.
	if err := os.Chmod(kernel, 0o644); err != nil {
		t.Fatalf("make the stub kernel readable: %v", err)
	}

	h := &harness{disk: &fakeDisk{device: "/dev/rbd0"}, devices: map[string]int{}}

	cfg := config.FirecrackerConfig{
		BinaryPath:   link,
		JailerPath:   filepath.Join(base, "jailer"),
		KernelImage:  kernel,
		ChrootBase:   shortDir(t),
		JailUIDMin:   900000,
		JailUIDCount: 8,
		Bridge:       "br0",
	}

	fixed := []Option{
		// QUIET, so a suite of two dozen launches does not bury a real failure in
		// its own success messages.
		WithLogger(slog.New(slog.DiscardHandler)),
		withRunner(h.record),
		withTapOwner(h.tapOwner),
		// mknod and chown need root and a real device; what matters for these tests
		// is that they were CALLED with the right arguments, which the jail
		// assertions check by the file the fake creates.
		withPrivileged(
			func(path, _ string, _, _ int) error {
				return os.WriteFile(path, []byte("device node"), 0o600)
			},
			// EVERY PATH IN THE TREE, as chownTree changes them, so a chown that
			// reached a snapshot link is seen even when it named the directory.
			func(path string, _, _ int) error {
				h.mu.Lock()
				defer h.mu.Unlock()

				return filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
					if err != nil {
						return err
					}

					h.chowned = append(h.chowned, p)

					return nil
				})
			},
		),
		// GENEROUS, because no test here waits for it to expire: every fake VMM
		// answers, and a failed start is asserted at the start action. At 2 seconds a
		// race-instrumented run on a loaded CI runner missed the fake's first answer
		// and failed a test that had nothing wrong with it.
		WithBootWait(30 * time.Second),
	}

	p, err := New(testDeployment, cfg, h.disk, append(fixed, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.p = p
	cgroupBase := t.TempDir()
	mounts := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(mounts, []byte("cgroup2 "+cgroupBase+" cgroup2 rw 0 0\n"), 0o600); err != nil {
		t.Fatalf("write the stub cgroup mount table: %v", err)
	}
	h.p.procMountsPath = mounts

	// A HOST OF ITS OWN, so a snapshot's compatibility record has something fixed to
	// match, and a test can stage a host it was not taken on.
	cpuinfo := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(cpuinfo, []byte(aCPUInfo), 0o600); err != nil {
		t.Fatalf("write the stub cpuinfo: %v", err)
	}

	h.p.cpuinfoPath = cpuinfo
	h.p.hostKernel = func() (string, error) { return "6.1.155 #1 SMP PREEMPT_DYNAMIC", nil }

	// THE TEST RUNS AS ITSELF, not as a jail uid only root can create files as, so a
	// file it writes into a jail stands in for one the VMM wrote. The real check is
	// held by jailWritten's own test and by the real-host capture.
	h.p.jailWrote = func(st unix.Stat_t, res resources) bool {
		return int(st.Uid) == os.Geteuid() || jailWritten(st, res)
	}

	// AND IT ANSWERS AS THE TEST'S ACCOUNT, not a jail uid, so the uid check on its
	// connections has its own test (vmmexe_linux_test.go).
	h.p.apiAs = answeringAsAnyone

	// THE FAKE VMM IS NO PROCESS, so the executable it "runs" is the configured one.
	h.p.runningVMM = func(context.Context, jail, resources) (*os.File, error) { return os.Open(h.p.execPath) }

	return h
}

// aCPUInfo is two identical x86 cores, as /proc/cpuinfo lists them.
const aCPUInfo = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7713 64-Core Processor
stepping	: 1
microcode	: 0xa0011d5
cpu MHz		: 2000.000
flags		: fpu vme de pse sse sse2 avx avx2

processor	: 1
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7713 64-Core Processor
stepping	: 1
microcode	: 0xa0011d5
cpu MHz		: 3100.000
flags		: avx2 avx sse2 sse pse de vme fpu
`

// record is the runner seam: it captures the argv and, for the jailer, lets the
// test decide what the VMM does next.
func (h *harness) record(_ context.Context, bin string, args []string) ([]byte, error) {
	h.mu.Lock()
	h.runs = append(h.runs, recordedRun{bin: bin, args: append([]string(nil), args...)})
	jailer := h.onJailer
	err := h.jailerErr
	refuse := h.refuse
	h.mu.Unlock()

	// A SEAM FOR "THE HOST SAID NO". Several teardown properties are only about what
	// billet does when a command it issued FAILS, and a fake that always succeeds
	// cannot express them.
	if refuse != nil {
		if err := refuse(bin, args); err != nil {
			return nil, err
		}
	}

	if bin == ipBinary {
		if err := h.changeDevices(args); err != nil {
			return nil, err
		}
	}

	if strings.HasSuffix(bin, "jailer") {
		if err != nil {
			return nil, err
		}

		// THE REAL JAILER WRITES A PID FILE, so the fake does too. Without one, a
		// jail with an API socket and no pid file is the state billet refuses to
		// act on — it cannot tell whether a VMM is running — and every teardown in
		// the suite would stop there. The default points at a process that has
		// already exited, which is what a stopped VMM looks like; a test that needs
		// a live one writes its own over the top.
		h.writeExitedPID(idOf(args))

		if jailer != nil {
			jailer(idOf(args))
		}
	}

	return nil, nil
}

// changeDevices applies an `ip` command billet ran to the fake host's devices, and
// refuses a name the host already has, as the kernel does.
func (h *harness) changeDevices(args []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	switch {
	// tuntap add dev <name> mode tap user <uid>
	case len(args) == 8 && args[0] == "tuntap" && args[1] == "add":
		if _, taken := h.devices[args[3]]; taken {
			return errors.New("ioctl(TUNSETIFF): Device or resource busy; RTNETLINK answers: File exists")
		}

		owner, err := strconv.Atoi(args[7])
		if err != nil {
			owner = -1
		}

		h.devices[args[3]] = owner
	// link del dev <name>
	case len(args) == 4 && args[0] == "link" && args[1] == "del":
		delete(h.devices, args[3])
	}

	return nil
}

// writeExitedPID puts a pid that has certainly exited into a jail's pid file.
//
// The honest default: a VMM that has stopped. It is a REAL pid rather than a made-up
// one, so the check billet performs — is this pid still the microVM — runs against
// the kernel rather than against a number chosen to please it.
func (h *harness) writeExitedPID(id string) {
	if id == "" {
		return
	}

	pid := os.Getpid()

	//nolint:noctx // a process that exists only to exit needs no cancellation
	if cmd := exec.Command("true"); cmd.Run() == nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	j := h.p.jailFor(id)

	//nolint:errcheck // a jail that has not been built yet has nowhere for this to go
	_ = os.WriteFile(j.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// idOf reads the --id out of a recorded jailer invocation.
func idOf(args []string) string {
	for i, a := range args {
		if a == "--id" && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// commands returns every recorded invocation.
func (h *harness) commands() []recordedRun {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]recordedRun(nil), h.runs...)
}

// ranWith reports whether any recorded command carried that argument.
func (h *harness) ranWith(arg string) bool {
	for _, r := range h.commands() {
		for _, a := range r.args {
			if a == arg {
				return true
			}
		}
	}

	return false
}

// everyArgument flattens every command billet ran, for the assertion that a
// credential is nowhere among them.
func (h *harness) everyArgument() string {
	var b strings.Builder

	for _, r := range h.commands() {
		b.WriteString(r.bin)
		b.WriteByte(' ')
		b.WriteString(strings.Join(r.args, " "))
		b.WriteByte('\n')
	}

	return b.String()
}

// serveVMM starts a fake VMM at the jail path the provider will look for, and
// returns it. Called from onJailer so the ordering matches production.
func (h *harness) serveVMM(t *testing.T, id string) *fakeVMM {
	t.Helper()

	v := startFakeVMM(t, h.p.jailFor(id).socket(), id)

	v.mu.Lock()
	v.root = h.p.jailFor(id).root()
	v.mu.Unlock()

	return v
}

// aSpec is a launch that should succeed.
func aSpec() provider.Spec {
	return provider.Spec{
		Name:      theInstance,
		Image:     "ubuntu-2404-x64@g1",
		VCPU:      2,
		Memory:    2 * config.GiB,
		Disk:      20 * config.GiB,
		Command:   []string{"./run.sh"},
		Trust:     provider.TrustTrusted,
		JITConfig: theCredential,
	}
}

// theCredential is a stand-in for a runner registration, distinctive enough to
// find anywhere it should not be.
const theCredential = "JIT-REGISTRATION-e4f19a7c-do-not-leak"

// launch runs a successful launch and returns the VMM that served it.
func (h *harness) launch(t *testing.T) (*provider.Instance, *fakeVMM) {
	t.Helper()

	var vmm *fakeVMM

	h.mu.Lock()
	h.onJailer = func(id string) { vmm = h.serveVMM(t, id) }
	h.mu.Unlock()

	inst, err := h.p.Launch(t.Context(), aSpec())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	return inst, vmm
}

// writeSnapshot writes what a snapshot/create body names, inside the VMM's chroot.
// The caller holds v.mu.
func (v *fakeVMM) writeSnapshot(body map[string]any) error {
	for _, field := range []string{"snapshot_path", "mem_file_path"} {
		name, ok := body[field].(string)
		if !ok {
			return errors.New("snapshot/create without " + field)
		}

		if err := os.WriteFile(filepath.Join(v.root, name), []byte("written by the VMM at "+name), 0o600); err != nil {
			return err
		}
	}

	if v.onCreate != nil {
		v.onCreate(v.root)
	}

	return nil
}

// answeringAsAnyone is the VMM client for fake VMMs, which answer as the test's own
// account rather than a jail uid.
func answeringAsAnyone(socket string, _ int) *vmmAPI { return newVMMAPI(socket) }
