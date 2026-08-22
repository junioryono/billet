package main

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func TestCheckReportsEveryRegisteredNodesSiteAndLiveness(t *testing.T) {
	stateDir := t.TempDir()
	configPath := writeAdminConfig(t, stateDir)
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body = append(body, []byte(`
sites:
  - name: home
    store: ceph
  - name: edge
    store: ceph
`)...)
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatalf("write site config: %v", err)
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	registrations := []alloc.NodeRegistration{
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "legacy", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB},
	}
	var edgeEpoch int64
	for _, registration := range registrations {
		epoch, err := a.RegisterNode(t.Context(), registration)
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", registration.Name, err)
		}
		if registration.Name == "edge-1" {
			edgeEpoch = epoch
		}
	}
	if err := a.NodeGone(t.Context(), "edge-1", edgeEpoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded state: %v", err)
	}

	var checkErr error
	out := capture(t, func() {
		checkErr = cmdCheck(t.Context(), []string{"--config", configPath})
	})
	if checkErr != nil {
		t.Fatalf("billet check: %v", checkErr)
	}
	for _, want := range []string{
		"fleet    edge-1                   at edge via firecracker (offline)",
		"fleet    epyc-1                   at home via firecracker (live)",
		"fleet    epyc-2                   at home via firecracker (live)",
		"fleet    legacy                   at local (implicit) via docker (live)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check output is missing %q:\n%s", want, out)
		}
	}
}
