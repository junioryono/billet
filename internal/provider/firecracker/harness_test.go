package firecracker

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
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

	v := &fakeVMM{id: id, state: "Not started", refuse: map[string]string{}}

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
	resolved   string
	resolveErr error
	cloneErr   error

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
}

// resolve stands in for the store turning `@verified` into a generation. The fake
// answers with whatever it is given unless a test says otherwise, because almost
// every test is about something else.
func (d *fakeDisk) ResolveGeneration(_ context.Context, image string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

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

	h := &harness{disk: &fakeDisk{device: "/dev/rbd0"}}

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
		// mknod and chown need root and a real device; what matters for these tests
		// is that they were CALLED with the right arguments, which the jail
		// assertions check by the file the fake creates.
		withPrivileged(
			func(path, _ string, _, _ int) error {
				return os.WriteFile(path, []byte("device node"), 0o600)
			},
			func(string, int, int) error { return nil },
		),
		WithBootWait(2 * time.Second),
	}

	p, err := New("deployment-a", cfg, h.disk, append(fixed, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h.p = p

	return h
}

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

	return startFakeVMM(t, h.p.jailFor(id).socket(), id)
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
