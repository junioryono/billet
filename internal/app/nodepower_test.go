package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/power"
)

// powerHost is one cpufreq policy under a temporary sysfs root, as the
// reference host reads.
func powerHost(t *testing.T) (root string, get func(string) string) {
	t.Helper()

	root = t.TempDir()
	cpufreq := filepath.Join(root, "devices", "system", "cpu", "cpufreq")
	for rel, value := range map[string]string{
		"boost":                                 "1",
		"policy0/scaling_max_freq":              "2450000",
		"policy0/scaling_min_freq":              "1500000",
		"policy0/scaling_available_frequencies": "2450000 2000000 1500000",
	} {
		path := filepath.Join(cpufreq, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root, func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(cpufreq, rel))
		if err != nil {
			t.Fatal(err)
		}

		return strings.TrimSpace(string(b))
	}
}

func powerConfig(stateDir string, p *config.NodePowerConfig) *config.Config {
	return &config.Config{Node: &config.NodeConfig{Provider: config.ProviderFirecracker, StateDir: stateDir, Power: p}}
}

// capHost leaves the host capped by a node process that then died: its record
// under stateDir, its values on the host.
func capHost(t *testing.T, root, stateDir string) {
	t.Helper()

	ctrl, err := power.New(power.Options{SysfsRoot: root, StateDir: stateDir, Tick: time.Millisecond,
		Settings: power.Settings{IdleAfter: time.Millisecond, DisableBoost: true, MaxFreqKHz: 1500000}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(ctx, func() bool { return true })
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(filepath.Join(stateDir, power.RecordName)); err == nil {
			b, err := os.ReadFile(filepath.Join(root, "devices", "system", "cpu", "cpufreq", "boost"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(b)) == "0" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in process never capped the host")
		}
	}
	cancel()
	<-done
}

// A NODE'S START RESTORES A CAP AN EARLIER PROCESS LEFT, whatever its
// configuration says now, and a node with no node.power and nothing to restore
// gets no policy at all.
func TestANodeStartRestoresALeftoverCapWhateverItsConfigSays(t *testing.T) {
	t.Parallel()

	root, get := powerHost(t)
	stateDir := t.TempDir()

	ctrl, err := openNodePower(t.Context(), powerConfig(stateDir, nil), root, "linux", nil)
	if err != nil || ctrl != nil {
		t.Fatalf("a node with no node.power and no record got %v, %v; want no policy", ctrl, err)
	}

	capHost(t, root, stateDir)

	ctrl, err = openNodePower(t.Context(), powerConfig(stateDir, nil), root, "linux", nil)
	if err != nil || ctrl != nil {
		t.Fatalf("openNodePower: %v, %v; want the leftover restored and no policy", ctrl, err)
	}
	if get("boost") != "1" || get("policy0/scaling_max_freq") != "2450000" {
		t.Errorf("the leftover cap was not restored: boost %s, ceiling %s", get("boost"),
			get("policy0/scaling_max_freq"))
	}
	if _, err := os.Stat(filepath.Join(stateDir, power.RecordName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the leftover record is still there: %v", err)
	}
}

// A LEFTOVER THAT CANNOT BE RESTORED AT THE START KEEPS A POLICY to retry it,
// though this node has no node.power; and the lock-time restore puts back a
// readable one before anything else.
func TestALeftoverThatWillNotRestoreKeepsARetryingPolicy(t *testing.T) {
	t.Parallel()

	root, get := powerHost(t)
	stateDir := t.TempDir()
	capHost(t, root, stateDir)

	restoreLeftoverPower(powerConfig(stateDir, nil), root, "darwin")
	if get("boost") != "0" {
		t.Fatal("a darwin node touched cpufreq")
	}
	restoreLeftoverPower(powerConfig(stateDir, nil), root, "linux")
	if get("boost") != "1" || get("policy0/scaling_max_freq") != "2450000" {
		t.Fatalf("the lock-time restore left the host capped: boost %s", get("boost"))
	}

	record := filepath.Join(stateDir, power.RecordName)
	if err := os.WriteFile(record, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctrl, err := openNodePower(t.Context(), powerConfig(stateDir, nil), root, "linux", nil)
	if err != nil || ctrl == nil {
		t.Fatalf("a leftover that would not restore answered %v, %v; want a policy to retry it", ctrl, err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("the unreadable record was removed: %v", err)
	}
}

// A node.power THE HOST CANNOT HONOUR IS REFUSED AT THE START, after the
// leftover is restored, and on anything but Linux.
func TestANodeStartRefusesAPowerBlockTheHostCannotHonour(t *testing.T) {
	t.Parallel()

	root, get := powerHost(t)
	stateDir := t.TempDir()
	capHost(t, root, stateDir)

	_, err := openNodePower(t.Context(), powerConfig(stateDir, &config.NodePowerConfig{IdleMaxFreqKHz: 1800000}),
		root, "linux", nil)
	if err == nil || !strings.Contains(err.Error(), "not one of policy0's available frequencies") {
		t.Fatalf("a ceiling outside the table answered %v, want a refusal", err)
	}
	if get("boost") != "1" || get("policy0/scaling_max_freq") != "2450000" {
		t.Error("a refused node left the host capped")
	}

	_, err = openNodePower(t.Context(), powerConfig(stateDir, &config.NodePowerConfig{DisableBoost: true}),
		root, "darwin", nil)
	if err == nil || !strings.Contains(err.Error(), "runs on darwin") {
		t.Errorf("node.power on darwin answered %v, want a refusal", err)
	}
	if ctrl, err := openNodePower(t.Context(), powerConfig(stateDir, nil), root, "darwin", nil); ctrl != nil || err != nil {
		t.Errorf("a darwin node with no node.power got %v, %v; want nothing", ctrl, err)
	}
}

// THE NODE'S STOP RESTORES: a node capped while it ran leaves the host at its
// full ceiling, its record gone.
func TestANodeStopRestoresTheCeiling(t *testing.T) {
	t.Parallel()

	root, get := powerHost(t)
	stateDir := t.TempDir()

	ctrl, err := power.New(power.Options{SysfsRoot: root, StateDir: stateDir, Tick: time.Millisecond,
		Settings: power.Settings{IdleAfter: time.Millisecond, DisableBoost: true, MaxFreqKHz: 1500000}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, signal := context.WithCancel(t.Context())
	stop := runNodePower(ctx, ctrl, func() bool { return true })
	t.Cleanup(stop)
	for deadline := time.Now().Add(10 * time.Second); get("boost") != "0"; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the policy never capped an idle node")
		}
	}

	// THE SIGNAL ALONE RESTORES, through the loop, while a drain would still be
	// holding the node, and nothing caps after it.
	signal()
	for deadline := time.Now().Add(10 * time.Second); get("boost") != "1"; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the node's stop signal did not restore the ceiling before the node returned")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if get("boost") != "1" {
		t.Fatal("the policy capped again after the node's stop began")
	}
	stop()

	if get("boost") != "1" || get("policy0/scaling_max_freq") != "2450000" {
		t.Errorf("the stop left the host capped: boost %s, ceiling %s", get("boost"), get("policy0/scaling_max_freq"))
	}
	if _, err := os.Stat(filepath.Join(stateDir, power.RecordName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there after the stop: %v", err)
	}
}
