package host

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider/ec2"
)

// AN EC2 TIER'S AMI BOOTS THE ARCHITECTURE THE TIER NAMES, or every launch would be
// refused by the node; an image not resolved, or of an architecture billet does not
// know, is judged by nothing here.
func TestAnEC2TiersAMIIsHeldToItsArchitecture(t *testing.T) {
	t.Parallel()

	tier := func(label string, arch config.Arch, ami string) config.Tier {
		return config.Tier{Label: label, Provider: config.ProviderEC2, Arch: arch, Image: ami}
	}

	images := []ec2.ImageInfo{
		{ImageID: "ami-arm", Found: true, Arch: config.ArchARM64},
		{ImageID: "ami-x64", Found: true, Arch: config.ArchX64},
		{ImageID: "ami-odd", Found: true},
		{ImageID: "ami-missing"},
	}

	for _, tc := range []struct {
		name    string
		tier    config.Tier
		wantErr string
	}{
		{name: "matching", tier: tier("graviton", config.ArchARM64, "ami-arm")},
		{name: "another architecture", tier: tier("graviton", config.ArchARM64, "ami-x64"),
			wantErr: "graviton names arm64 and its AMI ami-x64 is x64"},
		{name: "an image of unknown architecture", tier: tier("odd", config.ArchARM64, "ami-odd")},
		{name: "an image not resolved", tier: tier("missing", config.ArchX64, "ami-missing")},
		{name: "a tier naming none", tier: tier("any", "", "ami-x64")},
	} {
		err := ec2TierArchMismatch(&config.Config{Tiers: []config.Tier{tc.tier}}, images)

		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: answered %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// AN ARCHITECTURE IS WRITTEN ONLY ONCE EVERY HOST CAN READ IT (ADR-022): a billet
// from before it refuses a billet.yaml that carries arch, so a catalogue naming one
// is refused while any registered host predates it, and a host whose registration
// predates recording its range cannot say it does not.
func TestArchIsRefusedWhileARegisteredHostPredatesIt(t *testing.T) {
	t.Parallel()

	named := &config.Config{Tiers: []config.Tier{{Label: "a", Arch: config.ArchARM64}}}
	unnamed := &config.Config{Tiers: []config.Tier{{Label: "a"}}}

	current := alloc.NodeWire{Name: "new-1", Release: "v0.30.0", Max: nodeapi.VersionArchitectures}
	older := alloc.NodeWire{Name: "old-1", Release: "v0.29.0", Max: nodeapi.VersionArchitectures - 1}
	unsaid := alloc.NodeWire{Name: "unsaid-1"}

	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		hosts   []alloc.NodeWire
		gone    map[string]bool
		wantErr []string
	}{
		{name: "every host current", cfg: named, hosts: []alloc.NodeWire{current}},
		{name: "an older host", cfg: named, hosts: []alloc.NodeWire{current, older},
			wantErr: []string{"1 registered host(s)", "old-1 (v0.29.0)", "every controller"}},
		{name: "a host that cannot say", cfg: named, hosts: []alloc.NodeWire{unsaid},
			wantErr: []string{"unsaid-1 (a release it did not name)"}},
		{name: "an older host decommissioned", cfg: named, hosts: []alloc.NodeWire{current, older},
			gone: map[string]bool{"old-1": true}},
		{name: "no tier names one", cfg: unnamed, hosts: []alloc.NodeWire{older}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := archBeforeEveryHost(tc.cfg, tc.hosts, tc.gone)

			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatal("an arch every host cannot read was accepted")
			}

			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}
