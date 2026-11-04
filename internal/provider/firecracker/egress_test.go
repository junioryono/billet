package firecracker

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/egress"
)

// egressKernel stands in for nft under a real egress.Host, and calls onScript with each
// script, which is where a test looks at what the fake host holds at that moment.
type egressKernel struct {
	mu       sync.Mutex
	scripts  []string
	refuse   func(script string) error
	onScript func(script string)
	networks []string
}

func (k *egressKernel) run(_ context.Context, script string, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")

	k.mu.Lock()
	defer k.mu.Unlock()

	if script != "" {
		if k.refuse != nil && !slices.Contains(args, "-c") {
			if err := k.refuse(script); err != nil {
				return nil, err
			}
		}

		if !slices.Contains(args, "-c") {
			k.scripts = append(k.scripts, script)
		}

		if k.onScript != nil {
			k.onScript(script)
		}

		return nil, nil
	}

	switch {
	case strings.Contains(call, "billet_guard"):
		return []byte(`{"nftables":[{"rule":{"comment":"` + egress.GuardRuleComment + `"}}]}`), nil
	case strings.Contains(call, "list map"):
		return []byte(`{"nftables":[{"map":{"elem":[]}}]}`), nil
	case strings.Contains(call, "list counter"):
		return []byte(`{"nftables":[{"counter":{"packets":1}}]}`), nil
	case strings.Contains(call, "list set"):
		return []byte(`{"nftables":[{"set":{"elem":[]}}]}`), nil
	}

	return nil, nil
}

func (k *egressKernel) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()

	return slices.Clone(k.scripts)
}

func (k *egressKernel) holds(needle string) bool {
	return slices.ContainsFunc(k.all(), func(s string) bool { return strings.Contains(s, needle) })
}

// egressHarness is a harness whose provider holds a prepared Host on a fake kernel, with
// its gateway on loopback so the resolver really binds.
func egressHarness(t *testing.T, k *egressKernel) (*harness, *egress.Host) {
	t.Helper()

	host := egress.NewHost(t.TempDir(), slog.Default(), egress.WithRunner(k.run),
		egress.WithConnectionForget(func(uint32) error { return nil }),
		egress.WithGateway(func(network string) (netip.Prefix, error) {
			k.mu.Lock()
			defer k.mu.Unlock()

			k.networks = append(k.networks, network)

			return netip.MustParsePrefix("127.0.0.1/8"), nil
		}))
	t.Cleanup(host.Close)
	host.Prepare(t.Context())

	if err := host.Ready(); err != nil {
		t.Fatalf("not ready: %v", err)
	}

	return newHarness(t, WithEgress(host)), host
}

// launchListed launches theInstance under a list, against a fake VMM, as h.launch does
// with no policy; onJailer sees the moment the jailer runs.
func launchListed(t *testing.T, h *harness, onJailer func(id string)) {
	t.Helper()

	h.mu.Lock()
	h.onJailer = func(id string) {
		if onJailer != nil {
			onJailer(id)
		}

		h.serveVMM(t, id)
	}
	h.mu.Unlock()

	spec := aSpec()
	spec.Egress = aList(t)

	if _, err := h.p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

func aList(t *testing.T) *egress.Policy {
	t.Helper()

	p, err := egress.Compile([]string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// THERE IS NO MOMENT AT WHICH THE GUEST'S INTERFACE IS ON THE BRIDGE WITHOUT ITS RULES.
// They go in before the tap exists, on its name, which is why a launch that attached
// after the tap, or after the guest booted, is the mutation this holds against.
func TestAGuestsRulesAreInBeforeItsTapOrItsVMMExist(t *testing.T) {
	t.Parallel()

	k := &egressKernel{}
	h, _ := egressHarness(t, k)

	var (
		sawRules       bool
		rulesBeforeTap bool
		rulesBeforeVMM bool
		tap            string
	)

	k.onScript = func(script string) {
		if strings.Contains(script, "add chain bridge billet_egress job_"+theInstance) {
			sawRules = true
		}
	}

	h.refuse = func(_ string, args []string) error {
		if slices.Contains(args, "tuntap") {
			rulesBeforeTap = sawRules

			if i := slices.Index(args, "dev"); i >= 0 {
				tap = args[i+1]
			}
		}

		return nil
	}

	launchListed(t, h, func(string) { rulesBeforeVMM = sawRules })

	// ON THE TAP THE GUEST GOT, AND THE GATEWAY OF ITS BRIDGE: rules on any other name
	// install cleanly and filter nothing.
	if want := `guests { "` + tap + `" : jump job_` + theInstance; tap == "" || !k.holds(want) {
		t.Errorf("no rule binds the guest's tap %q: %q", tap, k.all())
	}

	if want := h.p.bridgeFor(aSpec().Trust); !slices.Equal(k.networks, []string{want}) {
		t.Errorf("the gateway was read from %q, want the guest's bridge %q", k.networks, want)
	}

	if !rulesBeforeTap {
		t.Fatal("the tap existed before the guest's rules")
	}

	if !rulesBeforeVMM {
		t.Fatal("the jailer ran before the guest's rules were in")
	}
}

// A LIST THAT CANNOT BE APPLIED REFUSES THE LAUNCH, NEVER RUNS THE JOB OPEN: no tap, no
// VMM, and nothing left behind.
func TestAListThatCannotBeInstalledRefusesTheLaunch(t *testing.T) {
	t.Parallel()

	k := &egressKernel{refuse: func(script string) error {
		if strings.Contains(script, "add chain bridge billet_egress job_") {
			return errors.New("Error: Could not process rule: Device or resource busy")
		}

		return nil
	}}
	h, _ := egressHarness(t, k)

	spec := aSpec()
	spec.Egress = aList(t)

	_, err := h.p.Launch(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), "install the rules") {
		t.Fatalf("Launch = %v, want the rules named", err)
	}

	for _, run := range h.commands() {
		if run.bin == ipBinary && slices.Contains(run.args, "tuntap") {
			t.Fatal("a tap was made for a guest that cannot have its rules")
		}

		if strings.Contains(run.bin, "jailer") {
			t.Fatal("a VMM was started for a guest that cannot have its rules")
		}
	}

	if _, err := os.Stat(h.p.jailFor(theInstance).dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused launch left its jail: %v", err)
	}

	if got := h.disk.discards(); len(got) != 1 {
		t.Errorf("a refused launch did not discard its root disk: %v", got)
	}
}

// A LIST THAT CANNOT BE APPLIED IS REFUSED AHEAD OF EVERYTHING ELSE, even when the caller
// never asked first: a provider built with no host, and a host not ready.
func TestAListIsRefusedWhereThereIsNoWayToApplyIt(t *testing.T) {
	t.Parallel()

	spec := aSpec()
	spec.Egress = aList(t)

	h := newHarness(t)

	if err := h.p.EgressReady(); !errors.Is(err, egress.ErrNotReady) {
		t.Fatalf("EgressReady with no host = %v", err)
	}

	if _, err := h.p.Launch(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "cannot be applied") {
		t.Fatalf("Launch with no host = %v", err)
	}

	if got := h.disk.clonedFrom(); got != "" {
		t.Fatalf("cloned %q for a guest that was refused", got)
	}

	unprepared := newHarness(t, WithEgress(egress.NewHost(t.TempDir(), slog.Default())))

	if _, err := unprepared.p.Launch(t.Context(), spec); !errors.Is(err, egress.ErrNotReady) {
		t.Fatalf("Launch on a host not prepared = %v", err)
	}
}

// AN OPEN GUEST ON A PROVIDER WITH NO HOST IS AS IT ALWAYS WAS: h.launch fails the test if
// its launch does.
func TestAGuestWithNoListNeedsNoHost(t *testing.T) {
	t.Parallel()

	newHarness(t).launch(t)
}

// A DESTROY TAKES THE RULES OFF WHILE IT STILL HOLDS THE LEASE: once it lets go, a launch of
// this name can have rules of its own, and a detach after that would take those and run its
// guest open. The jail is what the destroy removes last, so seeing it at the detach shows it
// came ahead of that.
func TestADestroyTakesTheRulesOffBeforeItLetsTheLeaseGo(t *testing.T) {
	t.Parallel()

	k := &egressKernel{}
	h, _ := egressHarness(t, k)

	launchListed(t, h, nil)

	var jailAtDetach bool

	k.onScript = func(script string) {
		if strings.Contains(script, "delete chain bridge billet_egress job_"+theInstance) {
			_, err := os.Stat(h.p.jailFor(theInstance).dir())
			jailAtDetach = err == nil
		}
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if !jailAtDetach {
		t.Fatal("the rules came off after the destroy had finished with the lease")
	}
}

// RULES COME OFF ONLY ON PROOF THE GUEST IS GONE: a Destroy that could not stop it leaves
// it closed, and the one that does stop it removes them.
func TestAGuestsRulesComeOffOnlyOnceItIsProvedGone(t *testing.T) {
	t.Parallel()

	k := &egressKernel{}
	h, host := egressHarness(t, k)

	launchListed(t, h, nil)

	j := h.p.jailFor(theInstance)

	good, err := os.ReadFile(j.pidFile())
	if err != nil {
		t.Fatalf("read the pid file: %v", err)
	}

	if err := os.WriteFile(j.pidFile(), []byte("not a number"), 0o600); err != nil {
		t.Fatalf("stage a corrupt pid file: %v", err)
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err == nil {
		t.Fatal("Destroy proceeded although it could not tell whether the VMM was running")
	}

	if k.holds("delete chain bridge billet_egress job_" + theInstance) {
		t.Fatal("a guest that may still be running lost its rules, and would run open")
	}

	if _, ok := host.Take(theInstance); ok {
		t.Fatal("a guest that may still be running had a final record")
	}

	if err := os.WriteFile(j.pidFile(), good, 0o600); err != nil {
		t.Fatalf("restore the pid file: %v", err)
	}

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if !k.holds("delete chain bridge billet_egress job_" + theInstance) {
		t.Fatalf("a destroyed guest kept its rules: %q", k.all())
	}

	rec, ok := host.Take(theInstance)
	if !ok || !rec.Listed {
		t.Fatalf("record = %+v, %v", rec, ok)
	}
}

// A LAUNCH THAT FAILS AFTER ITS RULES WENT IN TAKES THEM OFF, AND ITS RECORD WITH THEM:
// no report is owed for a guest that never ran, and its name goes back into the pool.
func TestAFailedLaunchTakesItsRulesOff(t *testing.T) {
	t.Parallel()

	k := &egressKernel{}
	h, host := egressHarness(t, k)
	h.jailerErr = errors.New("jailer refused")

	spec := aSpec()
	spec.Egress = aList(t)

	if _, err := h.p.Launch(t.Context(), spec); err == nil {
		t.Fatal("Launch reported success although the jailer failed")
	}

	if !k.holds("add chain bridge billet_egress job_" + theInstance) {
		t.Fatal("this stages nothing: no rules went in")
	}

	if !k.holds("delete chain bridge billet_egress job_" + theInstance) {
		t.Fatalf("a failed launch left its rules: %q", k.all())
	}

	if _, ok := host.Take(theInstance); ok {
		t.Fatal("a guest that never ran has a record to report")
	}
}

// A LAUNCH THE UNWIND COULD NOT FINISH KEEPS ITS RULES, and the Destroy that does finish it
// takes them: an unwind that failed has not said what is left, so nothing here reads its
// failure as proof.
func TestALaunchThatCouldNotUnwindKeepsItsRules(t *testing.T) {
	t.Parallel()

	k := &egressKernel{}
	h, _ := egressHarness(t, k)
	h.jailerErr = errors.New("jailer refused")
	h.disk.discardErr = errors.New("and the disk could not be discarded")

	spec := aSpec()
	spec.Egress = aList(t)

	if _, err := h.p.Launch(t.Context(), spec); err == nil {
		t.Fatal("Launch reported success although the jailer failed")
	}

	if !k.holds("add chain bridge billet_egress job_" + theInstance) {
		t.Fatal("this stages nothing: no rules went in")
	}

	if k.holds("delete chain bridge billet_egress job_" + theInstance) {
		t.Fatalf("rules came off a guest nothing has proved gone: %q", k.all())
	}

	h.disk.discardErr = nil

	if _, err := h.p.Destroy(t.Context(), theInstance); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if !k.holds("delete chain bridge billet_egress job_" + theInstance) {
		t.Fatalf("the destroy that finished the job left its rules: %q", k.all())
	}
}
