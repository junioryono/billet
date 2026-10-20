package firecracker

import (
	"context"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// completionDisk is a fake disk that also resolves completion roots.
type completionDisk struct {
	*fakeDisk

	asked []string
}

func (d *completionDisk) ResolveCompletionRoot(_ context.Context, ref, contract string) (string, error) {
	d.asked = append(d.asked, ref+" "+contract)

	return ref, nil
}

// A TIER'S LAUNCH RESOLVES THROUGH ResolveGeneration, which refuses a completion
// root; only a spec marked CompletionRoot reaches the completion resolver, and a
// disk without one refuses it rather than falling back.
func TestOnlyACompletionSpecResolvesACompletionRoot(t *testing.T) {
	tierDisk := &fakeDisk{}
	tier := &Provider{disk: tierDisk}
	if got, err := tier.resolveRoot(t.Context(), provider.Spec{Image: "ubuntu-2404-x64@g1"}); err != nil ||
		got != "ubuntu-2404-x64@g1" {
		t.Fatalf("a tier's launch resolved %q, %v", got, err)
	}
	if tierDisk.resolvedContract != GuestContract {
		t.Fatalf("a tier's launch did not go through ResolveGeneration (contract %q)", tierDisk.resolvedContract)
	}

	bare := &fakeDisk{}
	refused := &Provider{disk: bare}
	_, err := refused.resolveRoot(t.Context(), provider.Spec{Name: "probe", Image: "ubuntu-2404-x64@c1",
		CompletionRoot: true})
	if err == nil || !strings.Contains(err.Error(), "asks for a completion root and this node's disk has none") {
		t.Fatalf("got %v, want the completion spec refused by a disk with no completion roots", err)
	}
	if bare.resolvedContract != "" {
		t.Fatal("a completion spec fell back to ResolveGeneration")
	}

	disk := &completionDisk{fakeDisk: &fakeDisk{}}
	completing := &Provider{disk: disk}
	got, err := completing.resolveRoot(t.Context(), provider.Spec{Image: "ubuntu-2404-x64@c1", CompletionRoot: true})
	if err != nil || got != "ubuntu-2404-x64@c1" || len(disk.asked) != 1 || disk.resolvedContract != "" {
		t.Fatalf("a completion spec resolved %q, %v, through %v", got, err, disk.asked)
	}
}
