package app

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/tart"
)

// A NODE'S TART PROVIDER IS THE ONE THAT CAN RESOLVE `@verified`: built without
// node.state_dir it refuses every verified launch, and the CLI builds every provider here.
func TestANodesTartProviderReadsItsStateDirectory(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderTart,
		StateDir: t.TempDir(),
	}}

	prov, err := NewProvider(cfg, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	p, ok := prov.(*tart.Provider)
	if !ok {
		t.Fatalf("built a %T", prov)
	}

	if _, err := p.VerifiedImages(t.Context()); err != nil {
		t.Fatalf("VerifiedImages: %v", err)
	}
}

// THE PLANE IS TOLD THE ONE LIMIT PLACEMENT HOLDS A NODE TO, and the node's provider reads
// the record the node writes of it. A plane or a provider built without them answers
// silence, which every other test accepts, so the wiring is held by name.
func TestTheMacOSLimitIsWiredAtBothEnds(t *testing.T) {
	t.Parallel()

	for file, want := range map[string]string{
		"controlplane.go": "nodeplane.WithMacOSLimits(cp.cfg.MacOSLimitFor)",
		"node.go":         "macoslimit.Read(stateDir, deployment)",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not say %s", file, want)
		}
	}
}
