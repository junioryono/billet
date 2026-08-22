package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func TestRegisteredNodesReportSiteAndLivenessInNameOrder(t *testing.T) {
	t.Parallel()

	a, err := New(openState(t), Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	registrations := []NodeRegistration{
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

	got, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}
	want := []RegisteredNode{
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", Live: false},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "legacy", Provider: config.ProviderDocker, Site: "", Live: true},
	}
	if len(got) != len(want) {
		t.Fatalf("RegisteredNodes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RegisteredNodes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
