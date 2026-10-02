package tart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

const usageVM = "billet-c8b3dc17240b3ccb03e16d03efa9f22d"

// goneHolder is a pid lsof listed whose process has since exited.
const goneHolder = 4242

// usageHost is a provider over a TART_HOME holding usageVM's disk, with a fake
// lsof that answers each call with the next of answers (the last repeats) and
// logs its argv, and process paths and starts the test chooses.
type usageHost struct {
	p       *Provider
	disk    string
	argvLog string
	paths   map[int]string
	starts  map[int]uint64
}

func newUsageHost(t *testing.T, answers ...string) usageHost {
	t.Helper()

	home := t.TempDir()
	disk := filepath.Join(home, "vms", usageVM, "disk.img")
	if err := os.MkdirAll(filepath.Dir(disk), 0o750); err != nil {
		t.Fatalf("make the VM directory: %v", err)
	}
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatalf("write the disk: %v", err)
	}
	marker := filepath.Join(filepath.Dir(disk), ownerMarker)
	if err := os.WriteFile(marker, []byte(testOwner+"\n"), 0o600); err != nil {
		t.Fatalf("write the ownership marker: %v", err)
	}
	bin := t.TempDir()
	argvLog := filepath.Join(bin, "argv")
	counter := filepath.Join(bin, "calls")
	var script strings.Builder
	fmt.Fprintf(&script, "#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nn=$(cat %q 2>/dev/null || echo 0)\n"+
		"echo $((n + 1)) > %q\ncase \"$n\" in\n", argvLog, counter, counter)
	for i, a := range answers {
		label := fmt.Sprint(i)
		if i == len(answers)-1 {
			label = "*"
		}
		fmt.Fprintf(&script, "  %s) %s ;;\n", label, a)
	}
	script.WriteString("esac\n")
	lsof := filepath.Join(bin, "lsof")
	if err := forkSafeWriteFile(lsof, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("write the lsof shim: %v", err)
	}

	p, err := New(testOwner, WithHome(home))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := usageHost{p: p, disk: disk, argvLog: argvLog, paths: map[int]string{}, starts: map[int]uint64{}}
	p.lsof = lsof
	p.processPath = func(pid int) (string, error) {
		if path, ok := h.paths[pid]; ok {
			return path, nil
		}
		if pid == goneHolder {
			return "", fmt.Errorf("read the executable: %w", syscall.ESRCH)
		}

		return "", errors.New("operation not permitted")
	}
	p.processStart = func(pid int) (uint64, error) {
		if start, ok := h.starts[pid]; ok {
			return start, nil
		}

		return 0, errors.New("no such process")
	}

	return h
}

// A VM IS THE ONE VIRTUALIZATION PROCESS HOLDING ITS DISK, read with the start
// that process had, and nothing else that holds the disk is taken for it.
func TestAVMIsTheVirtualizationProcessHoldingItsDisk(t *testing.T) {
	t.Parallel()

	h := newUsageHost(t, "echo 74204; echo 74207")
	h.paths[74204] = "/opt/homebrew/Cellar/tart/2.37.0/libexec/tart.app/Contents/MacOS/tart"
	h.paths[74207] = vmService
	h.starts[74207] = 16664085551217

	target, err := h.p.UsageTarget(t.Context(), usageVM)
	if err != nil {
		t.Fatalf("UsageTarget: %v", err)
	}
	want := provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true}
	if target != want {
		t.Errorf("target is %+v, want %+v", target, want)
	}
	argv, err := os.ReadFile(h.argvLog)
	if err != nil {
		t.Fatalf("read lsof's argv: %v", err)
	}
	if want := "-t +w -- " + h.disk + "\n"; string(argv) != want+want {
		t.Errorf("lsof was asked %q, want the disk twice: %q", argv, want+want)
	}
}

// NO ANSWER IS A GUESS: no holder, a holder that is not a VM, two VMs, a pid
// that changed hands between the proofs, and an lsof that failed all refuse.
func TestAVMProcessThatCannotBeProvedIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		answers []string
		paths   map[int]string
		starts  map[int]uint64
		want    string
	}{
		{name: "nothing holds the disk", answers: []string{"exit 1"}, want: "no VM process"},
		{
			name: "only tart holds it", answers: []string{"echo 74204"},
			paths: map[int]string{74204: "/opt/homebrew/bin/tart"}, want: "no VM process",
		},
		{
			name: "two VMs hold it", answers: []string{"echo 1; echo 2"},
			paths: map[int]string{1: vmService, 2: vmService}, starts: map[int]uint64{1: 1, 2: 2},
			want: "2 VM processes",
		},
		{
			name: "the holder changed between the proofs", answers: []string{"echo 1", "echo 2"},
			paths: map[int]string{1: vmService, 2: vmService}, starts: map[int]uint64{1: 1, 2: 2},
			want: "changed while it was being read",
		},
		{
			name: "the holder is gone before its start is read", answers: []string{"echo 1"},
			paths: map[int]string{1: vmService}, want: "read the start",
		},
		{
			name: "lsof failed", answers: []string{"echo 'lsof: WARNING: cannot stat()' >&2; exit 1"},
			want: "cannot stat()",
		},
		{name: "lsof answered nonsense", answers: []string{"echo 12x"}, want: "not a pid"},
		{
			name:    "lsof warned and still succeeded",
			answers: []string{"echo 1; echo 'lsof: WARNING: cannot stat()' >&2"},
			paths:   map[int]string{1: vmService}, starts: map[int]uint64{1: 1}, want: "may be short",
		},
		{
			name: "a holder could not be read", answers: []string{"echo 1; echo 2"},
			paths: map[int]string{1: vmService}, starts: map[int]uint64{1: 1}, want: "could not tell what process 2",
		},
		{
			name:    "the disk was replaced between the proofs",
			answers: []string{"echo 1", `for a; do d=$a; done; rm -f "$d"; : > "$d"; echo 1`},
			paths:   map[int]string{1: vmService}, starts: map[int]uint64{1: 1}, want: "disk changed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newUsageHost(t, tc.answers...)
			for pid, path := range tc.paths {
				h.paths[pid] = path
			}
			for pid, start := range tc.starts {
				h.starts[pid] = start
			}
			_, err := h.p.UsageTarget(t.Context(), usageVM)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("UsageTarget answered %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// ONLY A LEASE'S OWN VM IS LOOKED FOR: a name billet did not give, or one that
// would leave the store, is refused before anything runs.
func TestUsageTargetLooksOnlyForALeasesVM(t *testing.T) {
	t.Parallel()

	h := newUsageHost(t, "echo 1")
	h.paths[1], h.starts[1] = vmService, 1
	// AN OPERATOR'S OWN VM, with a disk where billet would look for one.
	theirs := filepath.Join(filepath.Dir(filepath.Dir(h.disk)), "runner-1", "disk.img")
	if err := os.MkdirAll(filepath.Dir(theirs), 0o750); err != nil {
		t.Fatalf("make the operator's VM: %v", err)
	}
	if err := os.WriteFile(theirs, nil, 0o600); err != nil {
		t.Fatalf("write the operator's disk: %v", err)
	}
	for _, name := range []string{"runner-1", "billet-../../etc", "billet-"} {
		if _, err := h.p.UsageTarget(t.Context(), name); err == nil {
			t.Errorf("UsageTarget looked for %q", name)
		}
	}
	if _, err := os.Stat(h.argvLog); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lsof ran for a name that is not a lease's (stat: %v)", err)
	}
}

// A HOLDER PROVED GONE IS SKIPPED: a process that exited between lsof's answer
// and the read of its executable is not a VM, and not a reason to refuse.
func TestAHolderThatExitedIsNotCounted(t *testing.T) {
	t.Parallel()

	h := newUsageHost(t, fmt.Sprintf("echo %d; echo 7", goneHolder))
	h.paths[7], h.starts[7] = vmService, 70
	target, err := h.p.UsageTarget(t.Context(), usageVM)
	if err != nil || target.PID != 7 {
		t.Errorf("UsageTarget = %+v, %v; want pid 7", target, err)
	}
}

// ONLY THIS DEPLOYMENT'S OWN VM, BY ITS OWN FILES: a VM whose marker names
// another deployment or none, or whose directory or disk is a symlink to
// someone else's, is not looked for.
func TestAVMThatIsNotProvedOursIsNotMeasured(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, h usageHost)
	}{
		{"a foreign marker", func(t *testing.T, h usageHost) {
			t.Helper()
			marker := filepath.Join(filepath.Dir(h.disk), ownerMarker)
			if err := os.WriteFile(marker, []byte("fedcba9876543210fedcba9876543210\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"no marker", func(t *testing.T, h usageHost) {
			t.Helper()
			if err := os.Remove(filepath.Join(filepath.Dir(h.disk), ownerMarker)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlinked disk", func(t *testing.T, h usageHost) {
			t.Helper()
			theirs := filepath.Join(t.TempDir(), "disk.img")
			if err := os.WriteFile(theirs, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(h.disk); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(theirs, h.disk); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlinked VM directory", func(t *testing.T, h usageHost) {
			t.Helper()
			dir := filepath.Dir(h.disk)
			theirs := filepath.Join(t.TempDir(), "theirs")
			if err := os.Rename(dir, theirs); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(theirs, dir); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newUsageHost(t, "echo 1")
			h.paths[1], h.starts[1] = vmService, 1
			tc.damage(t, h)
			if target, err := h.p.UsageTarget(t.Context(), usageVM); err == nil {
				t.Errorf("UsageTarget measured %+v", target)
			}
			if _, err := os.Stat(h.argvLog); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("lsof ran for a VM that is not proved ours (stat: %v)", err)
			}
		})
	}
}
