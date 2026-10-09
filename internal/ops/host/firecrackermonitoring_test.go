package host

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/firecracker"
)

// stageCgroupHost writes a cgroup-v2 hierarchy whose root offers controllers
// (or, with controllers nil, whose controller list cannot be read) and returns
// the mount table naming it and its root.
func stageCgroupHost(t *testing.T, controllers []string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "cgroup")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("stage %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("stage %s: %v", path, err)
		}
	}

	if controllers == nil {
		// A directory where the list should be fails a read whoever runs this.
		if err := os.MkdirAll(filepath.Join(root, "cgroup.controllers"), 0o700); err != nil {
			t.Fatalf("stage: %v", err)
		}
	} else {
		list := strings.Join(controllers, " ") + "\n"
		write(filepath.Join(root, "cgroup.controllers"), list)
		write(filepath.Join(root, "cgroup.subtree_control"), list)
		write(filepath.Join(root, "system.slice", "io.weight"), "default 100\n")
	}

	mounts := filepath.Join(dir, "mounts")
	write(mounts, "cgroup2 "+root+" cgroup2 rw,nosuid 0 0\n")

	return mounts, root
}

// BILLET CHECK REFUSES WHAT THE NODE WOULD: with node.monitoring, a Firecracker
// host that did not prove both memory and io fails the check, naming each
// controller and whether it is missing or could not be told; without it, the
// same host passes this gate and goes on to the checks it always had (which on
// a machine with no /dev/kvm then fail for that reason, not this one).
func TestCheckRefusesAFirecrackerHostMonitoringCannotMeasure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		controllers []string
		monitoring  bool
		says        []string
	}{
		{"both present", []string{"cpu", "io", "memory"}, true, nil},
		{"memory missing", []string{"cpu", "io"}, true, []string{"memory is missing"}},
		{"io missing", []string{"cpu", "memory"}, true, []string{"io is missing"}},
		{"an unreadable controller list", nil, true,
			[]string{"could not tell whether memory", "could not tell whether io"}},
		{"memory missing, monitoring off", []string{"cpu", "io"}, false, nil},
		{"unreadable, monitoring off", nil, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mounts, root := stageCgroupHost(t, tc.controllers)
			dir := t.TempDir()
			bin := filepath.Join(dir, "firecracker-v1.16.1")
			if err := os.WriteFile(bin, nil, 0o600); err != nil {
				t.Fatalf("stage the binary: %v", err)
			}
			cfg := &config.Config{Node: &config.NodeConfig{
				Provider: config.ProviderFirecracker,
				Firecracker: &config.FirecrackerConfig{
					BinaryPath: bin, JailerPath: filepath.Join(dir, "jailer"),
					KernelImage: filepath.Join(dir, "vmlinux"), ChrootBase: "/srv/jail",
					JailUIDMin: 900000, JailUIDCount: 8, Bridge: "br0",
				},
			}}
			if tc.monitoring {
				cfg.Node.Monitoring = &config.NodeMonitoringConfig{}
			}

			var out bytes.Buffer
			env := cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }}

			err := checkFirecrackerHost(t.Context(), env, cfg, firecracker.WithMountTable(mounts))
			if tc.says == nil {
				// PAST THE GATE AND INTO CheckHost, whose first two checks this
				// staged host fails: /dev/kvm where this account cannot open it,
				// and otherwise the staged binary, which is empty and will not
				// report a version.
				reachedCheckHost := err != nil && (errors.Is(err, firecracker.ErrNoKVM) ||
					strings.Contains(err.Error(), "would not report its version"))
				if !reachedCheckHost || errors.Is(err, firecracker.ErrJobAccountingUnproved) {
					t.Fatalf("checkFirecrackerHost = %v, want it to pass the accounting gate and fail "+
						"on /dev/kvm or the staged binary", err)
				}

				return
			}
			if !errors.Is(err, firecracker.ErrJobAccountingUnproved) {
				t.Fatalf("checkFirecrackerHost = %v, want the accounting refusal", err)
			}
			if !strings.Contains(out.String(), "per-job accounting: memory ") {
				t.Errorf("the refused check did not print both controllers' states:\n%s", out.String())
			}
			for _, want := range append(tc.says, filepath.Join(root, "firecracker-v1.16.1"),
				"CONFIG_BLK_CGROUP_IOCOST") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}
