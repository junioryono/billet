package main

import (
	"context"
	"errors"
	"go/ast"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// THE PROBE IS GROWN AS THE FLEET GROWS A JOB'S DISK: to the largest a
// firecracker tier asks for, or to what --disk names, and never to nothing,
// because a launch that skipped the grow proved nothing about the step every
// job takes first (#261).
func TestTheProbeDiskIsWhatTheFleetGrowsTo(t *testing.T) {
	t.Parallel()

	firecracker := func(disk config.ByteSize) config.Tier {
		return config.Tier{Provider: config.ProviderFirecracker, Disk: disk}
	}
	for name, tc := range map[string]struct {
		tiers    []config.Tier
		override config.ByteSize
		want     config.ByteSize
	}{
		"the largest firecracker tier": {
			tiers: []config.Tier{firecracker(40 * config.GiB), firecracker(160 * config.GiB)},
			want:  160 * config.GiB,
		},
		"another backend's disk does not count": {
			tiers: []config.Tier{firecracker(40 * config.GiB),
				{Provider: config.ProviderEC2, Disk: 500 * config.GiB}},
			want: 40 * config.GiB,
		},
		"a tier reaching firecracker through providers counts": {
			tiers: []config.Tier{{Providers: []config.ProviderKind{config.ProviderEC2,
				config.ProviderFirecracker}, Disk: 120 * config.GiB}},
			want: 120 * config.GiB,
		},
		"no tier names a disk":    {tiers: []config.Tier{firecracker(0)}, want: verifyDiskFallback},
		"a node config, no tiers": {want: verifyDiskFallback},
		"--disk wins": {
			tiers:    []config.Tier{firecracker(160 * config.GiB)},
			override: 50 * config.GiB,
			want:     50 * config.GiB,
		},
	} {
		if got := verifyDisk(&config.Config{Tiers: tc.tiers}, tc.override); got != tc.want {
			t.Errorf("%s: probe disk %s, want %s", name, got, tc.want)
		}
	}
}

// THE LAUNCH IS ASKED TO GROW THE DISK, which is what sends it through the
// provider's resize2fs, and the guest is asked to say what it booted on.
func TestTheProbeLaunchGrowsItsDiskAndAsksAboutIt(t *testing.T) {
	t.Parallel()

	spec := probeSpec("billet-probe", "ubuntu-2404-x64@g1", "10.0.0.1:7719", "secret", 80*config.GiB)
	if spec.Disk != 80*config.GiB {
		t.Fatalf("the probe launch asks for a %s disk, want 80GiB", spec.Disk)
	}
	if command := strings.Join(spec.Command, " "); !strings.Contains(command, "rootfs=$(df -B1") {
		t.Fatalf("the probe does not ask the guest for its root filesystem's size: %s", command)
	}
}

// A GROW THAT DID NOT REACH THE BOOTED FILESYSTEM FAILS THE VERIFICATION, and
// says so; a filesystem at its grown size, less ext4's own overhead, passes.
func TestTheReportedRootFilesystemMustBeTheGrownOne(t *testing.T) {
	t.Parallel()

	const disk = 80 * config.GiB
	report := func(rootfs string) string {
		lines := []string{
			"jit=probe-secret", "whoami=runner", "runner=2.336.0",
			"docker=29.1.3 storage=overlay2 cgroups=2",
			"buildx=github.com/docker/buildx v0.33.0 7f91f038ac14",
			"compose=2.40.3", "container=1",
		}
		if rootfs != "" {
			lines = append(lines, "rootfs="+rootfs)
		}

		return strings.Join(lines, "\n")
	}
	grown := strconv.FormatInt(int64(disk)/100*97, 10)
	ungrown := strconv.FormatInt(int64(36*config.GiB), 10)

	if err := checkGuestReport(report(grown), "probe-secret", disk); err != nil {
		t.Errorf("a filesystem at its grown size was refused: %v", err)
	}
	for name, tc := range map[string]struct{ rootfs, clause string }{
		"not grown":    {ungrown, "did not reach it"},
		"not reported": {"", "did not report its root filesystem"},
		"not a number": {"df: /: No such file", "is not a number of bytes"},
	} {
		err := checkGuestReport(report(tc.rootfs), "probe-secret", disk)
		if err == nil || !strings.Contains(err.Error(), tc.clause) {
			t.Errorf("%s: checkGuestReport = %v, want a refusal saying %q", name, err, tc.clause)
		}
	}
	if err := checkGuestReport(report(""), "probe-secret", 0); err != nil {
		t.Errorf("with no grow asked for, a report without rootfs was refused: %v", err)
	}
}

// THE REPORT IS JUDGED AGAINST THE DISK THE LAUNCH GREW, not only built with it.
func TestTheReportIsJudgedAgainstTheGrownDisk(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"jit=probe-secret", "whoami=runner", "runner=2.336.0",
		"docker=29.1.3 storage=overlay2 cgroups=2",
		"buildx=github.com/docker/buildx v0.33.0 7f91f038ac14",
		"compose=2.40.3", "container=1",
		"rootfs=" + strconv.FormatInt(int64(36*config.GiB), 10),
	}, "\n")
	report := make(chan string, 1)
	report <- body
	err := awaitGuestReport(t.Context(), report, nil, "ubuntu-2404-x64@g1", "probe-secret",
		80*config.GiB, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "did not reach it") {
		t.Fatalf("awaitGuestReport = %v, want an ungrown filesystem refused", err)
	}
}

// THE COMMAND HANDS THE VERIFICATION THE FLEET'S SIZE. A structural test,
// because cmdImagesVerify needs a real node to run: what it checks is that the
// disk verifyGuestImage launches on is the one verifyDisk chose, so a later edit
// cannot quietly verify on an ungrown disk again.
func TestTheVerifyCommandLaunchesOnTheDiskItChose(t *testing.T) {
	t.Parallel()

	fn := findFunc(t, "cmdImagesVerify")
	chosen, passed := false, false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 && isIdent(n.Lhs[0], "disk") && isCallTo(n.Rhs[0], "verifyDisk") {
				chosen = true
			}
		case *ast.CallExpr:
			if isCallTo(n, "verifyGuestImage") {
				for _, arg := range n.Args {
					passed = passed || isIdent(arg, "disk")
				}
			}
		}

		return true
	})
	if !chosen || !passed {
		t.Fatalf("cmdImagesVerify chose its disk with verifyDisk: %v; passed it to "+
			"verifyGuestImage: %v", chosen, passed)
	}
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == name
}

func isCallTo(expr ast.Expr, name string) bool {
	call, ok := expr.(*ast.CallExpr)

	return ok && isIdent(call.Fun, name)
}

// launchRecorder records what a verification asks the backend to launch and
// refuses it, so the verification ends at once; its Destroy finds nothing.
type launchRecorder struct {
	provider.Provider

	launched []provider.Spec
}

func (l *launchRecorder) Launch(_ context.Context, spec provider.Spec) (*provider.Instance, error) {
	l.launched = append(l.launched, spec)

	return nil, errors.New("refused by the test")
}

func (l *launchRecorder) Destroy(context.Context, string) (provider.Teardown, error) {
	return provider.TeardownStopped, nil
}

// THE VERIFICATION LAUNCHES ON THE GROWN DISK, through the launch every job
// takes, rather than only building a spec that says so. Serial: it replaces
// listenGuestReport.
func TestAVerificationLaunchesOnTheGrownDisk(t *testing.T) {
	listen := listenGuestReport
	t.Cleanup(func() { listenGuestReport = listen })
	listenGuestReport = func(context.Context, string, int, string, chan<- string,
	) (*http.Server, string, <-chan error, error) {
		return &http.Server{ReadHeaderTimeout: time.Second}, "127.0.0.1:7719", nil, nil
	}

	backend := &launchRecorder{}
	err := verifyGuestImage(t.Context(), backend, "br0", 7719, "ubuntu-2404-x64@g1", "probe",
		120*config.GiB, time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not launch") {
		t.Fatalf("verifyGuestImage = %v, want the refused launch reported", err)
	}
	if len(backend.launched) != 1 || backend.launched[0].Disk != 120*config.GiB {
		t.Fatalf("launched %+v, want one probe on a 120GiB disk", backend.launched)
	}
}
