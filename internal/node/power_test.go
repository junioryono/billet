package node

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/power"
	"github.com/junioryono/billet/internal/provider"
)

// probingProvider reads the host's CPU ceiling at the moment it is asked to
// start the compute, which is the moment the restore has to have landed by.
type probingProvider struct {
	*fakeProvider
	atLaunch func()
}

func (p *probingProvider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	p.atLaunch()

	return p.fakeProvider.Launch(ctx, spec)
}

// A LAUNCH RESTORES THE HOST'S FULL CPU CEILING BEFORE THE PROVIDER STARTS
// ANYTHING, on a host the idle policy capped through its own loop.
func TestALaunchRestoresTheCPUCeilingBeforeTheProviderStarts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cpufreq := filepath.Join(root, "devices", "system", "cpu", "cpufreq")
	set := func(rel, value string) {
		t.Helper()
		path := filepath.Join(cpufreq, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	get := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(cpufreq, rel))
		if err != nil {
			t.Fatal(err)
		}

		return strings.TrimSpace(string(b))
	}
	set("boost", "1")
	set("policy0/scaling_max_freq", "2450000")
	set("policy0/scaling_min_freq", "1500000")
	set("policy0/scaling_available_frequencies", "2450000 2000000 1500000")

	ctrl, err := power.New(power.Options{
		SysfsRoot: root, StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		Settings: power.Settings{IdleAfter: 20 * time.Millisecond, DisableBoost: true, MaxFreqKHz: 1500000},
		Tick:     5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, &probingProvider{fakeProvider: p, atLaunch: func() {
		if got := get("policy0/scaling_max_freq"); got != "2450000" {
			t.Errorf("the provider was asked to start a guest with scaling_max_freq %s, want 2450000", got)
		}
		if got := get("boost"); got != "1" {
			t.Errorf("the provider was asked to start a guest with boost %s, want 1", got)
		}
	}}, nil, WithPower(ctrl))

	// CAPPED BY THE POLICY'S OWN LOOP, asking the runner as the node does.
	loop, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctrl.Run(loop, func() bool { return !r.Holding() })
	}()
	for deadline := time.Now().Add(10 * time.Second); get("boost") != "0"; {
		if time.Now().After(deadline) {
			t.Fatal("the idle policy never capped an idle node")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	<-done
	if got := get("policy0/scaling_max_freq"); got != "1500000" {
		t.Fatalf("capped with scaling_max_freq %s, want 1500000", got)
	}

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, RunID: 1, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(p.launched) != 1 {
		t.Fatalf("started %d instances, want 1", len(p.launched))
	}
}
