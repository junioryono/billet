package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A preflight that could not open /dev/kvm cannot say anything else useful, so the
// tests below run only where the device is there. That is the reference host and
// nowhere else — a laptop has no /dev/kvm, and the check is honest about it rather
// than being weakened so it passes everywhere.
func requireKVM(t *testing.T) {
	t.Helper()

	if err := checkKVM(); err != nil {
		t.Skipf("no usable /dev/kvm on this machine: %v", err)
	}
}

// A HOST WITH NO /dev/kvm IS NAMED AS SUCH, with a sentinel, because it is the one
// failure an operator cannot work around by editing the config.
func TestAHostWithoutKVMIsRefusedWithASentinel(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/dev/kvm"); err == nil {
		t.Skip("this machine has /dev/kvm, so the absence cannot be staged")
	}

	h := newHarness(t)

	if _, err := h.p.CheckHost(t.Context()); !errors.Is(err, ErrNoKVM) {
		t.Errorf("a host with no /dev/kvm was not reported as such: %v", err)
	}
}

// THE PREFLIGHT REPORTS THE DIRECTORY THE JAILER WILL ACTUALLY USE, which is
// derived from the resolved binary rather than configured. An operator looking for
// a running guest will not find it under the path they wrote.
func TestThePreflightReportsTheDerivedJailDirectory(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	h := newHarness(t)

	report, err := h.p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost: %v", err)
	}

	if !strings.HasSuffix(report.JailDir, "/firecracker-v1.16.1") {
		t.Errorf("the report names %q, not the directory the jailer derives", report.JailDir)
	}

	// THE RANGE, because a host that has run out of uids stops being able to launch
	// and the number is otherwise invisible.
	if report.JailUIDMin != h.p.cfg.JailUIDMin || report.JailUIDCount != h.p.cfg.JailUIDCount {
		t.Errorf("the report names the uid range %d+%d, want %d+%d",
			report.JailUIDMin, report.JailUIDCount, h.p.cfg.JailUIDMin, h.p.cfg.JailUIDCount)
	}
}

// A KERNEL THAT IS NOT A KERNEL IS CAUGHT HERE. A directory and an empty file both
// pass a stat, and both produce a VMM that accepts the boot source and then fails
// to start the guest — which under --daemonize is a launch reporting success.
func TestAGuestKernelThatIsNotAFileIsRefused(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, dir string) string
		says  string
	}{
		{"a directory", func(t *testing.T, dir string) string {
			t.Helper()

			path := filepath.Join(dir, "kerneldir")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("stage: %v", err)
			}

			return path
		}, "not a regular file"},
		{"an empty file", func(t *testing.T, dir string) string {
			t.Helper()

			path := filepath.Join(dir, "empty")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("stage: %v", err)
			}

			return path
		}, "is empty"},
		{"nothing at all", func(_ *testing.T, dir string) string {
			return filepath.Join(dir, "absent")
		}, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.p.cfg.KernelImage = tc.stage(t, t.TempDir())

			_, err := h.p.CheckHost(t.Context())
			if err == nil {
				t.Fatal("CheckHost accepted it")
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the error does not say %q: %v", tc.says, err)
			}
		})
	}
}

// BOTH BRIDGES ARE PROVEN TO EXIST, and the untrusted one matters more: it is
// consulted only when a fork's pull request arrives, so a typo there is invisible
// until the first untrusted job — on a host that had reported itself healthy.
func TestThePreflightProvesBothBridgesExist(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	h := newHarness(t, func(p *Provider) { p.cfg.UntrustedBridge = "br-untrusted" })

	if _, err := h.p.CheckHost(t.Context()); err != nil {
		t.Fatalf("CheckHost: %v", err)
	}

	for _, bridge := range []string{"br0", "br-untrusted"} {
		if !h.ranWith(bridge) {
			t.Errorf("the preflight did not look for the bridge %q: %s", bridge, h.everyArgument())
		}
	}
}

// AND A MISSING ONE IS A FAILURE RATHER THAN A NOTE. A guest attached to a bridge
// that is not there has no network, and a runner with no network registers and then
// does nothing — which looks like a healthy tier running no jobs.
func TestAMissingBridgeFailsThePreflight(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	h := newHarness(t)
	h.jailerErr = nil

	// `ip link show` is the only `ip` call a preflight makes, so failing every one
	// of them stages exactly the missing bridge.
	h.p.run = func(ctx context.Context, bin string, args []string) ([]byte, error) {
		if bin == ipBinary {
			return nil, errors.New("Device \"br0\" does not exist")
		}

		return h.record(ctx, bin, args)
	}

	_, err := h.p.CheckHost(t.Context())
	if err == nil {
		t.Fatal("CheckHost accepted a host whose bridge is not there")
	}

	if !strings.Contains(err.Error(), "br0") {
		t.Errorf("the error does not name the bridge: %v", err)
	}
}

// A BINARY THAT WILL NOT REPORT ITS VERSION IS A HOST THAT CANNOT LAUNCH. The
// common cause is that it is not installed, and the message says so rather than
// leaving an operator with an exit status.
func TestABinaryThatWillNotAnswerFailsThePreflight(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	h := newHarness(t)
	h.p.run = func(_ context.Context, bin string, _ []string) ([]byte, error) {
		if strings.Contains(bin, "jailer") {
			return nil, errors.New("exec: no such file or directory")
		}

		return []byte("Firecracker v1.16.1\n"), nil
	}

	_, err := h.p.CheckHost(t.Context())
	if err == nil {
		t.Fatal("CheckHost accepted a host with no jailer")
	}

	if !strings.Contains(err.Error(), "jailer") {
		t.Errorf("the error does not name which binary is missing: %v", err)
	}
}

// THE VERSION IS THE LINE THE BINARY BEGINS WITH. Both print a blank line after it,
// and a report carrying the blank is a report an operator cannot read.
func TestTheReportedVersionIsTheFirstLine(t *testing.T) {
	t.Parallel()
	requireKVM(t)

	h := newHarness(t)
	h.p.run = func(_ context.Context, bin string, _ []string) ([]byte, error) {
		if bin == ipBinary {
			return nil, nil
		}

		return []byte("Firecracker v1.16.1\n\nsomething else entirely\n"), nil
	}

	report, err := h.p.CheckHost(t.Context())
	if err != nil {
		t.Fatalf("CheckHost: %v", err)
	}

	if report.Firecracker != "Firecracker v1.16.1" {
		t.Errorf("the reported version is %q", report.Firecracker)
	}
}
