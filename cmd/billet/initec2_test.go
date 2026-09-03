package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A PRICE OVERRIDE FOR AN UNDECLARED SHAPE IS A TYPO, refused rather than
// silently ignored — otherwise billet fetches a price the operator thought pinned.
func TestResolveShapeTypesRefusesAnOverrideForAnUndeclaredShape(t *testing.T) {
	_, err := resolveShapeTypes(
		[]string{"c7i.xlarge"},
		map[string]config.USDPerHour{"c7i.xlarg": 170000}, // typo: missing 'e'
	)
	if err == nil {
		t.Fatal("accepted a --price for a shape not in --instance-type")
	}
	if !strings.Contains(err.Error(), "--price") || !strings.Contains(err.Error(), "c7i.xlarg") {
		t.Errorf("refusal names neither the flag nor the shape: %v", err)
	}
}

// DUPLICATE AND EMPTY SHAPE NAMES ARE REFUSED; a valid list is returned in order.
func TestResolveShapeTypes(t *testing.T) {
	if _, err := resolveShapeTypes([]string{"c7i.xlarge", "c7i.xlarge"}, nil); err == nil ||
		!strings.Contains(err.Error(), "listed twice") {
		t.Errorf("duplicate not refused: %v", err)
	}
	if _, err := resolveShapeTypes([]string{"  "}, nil); err == nil {
		t.Errorf("empty shape name not refused")
	}

	got, err := resolveShapeTypes([]string{" c7i.xlarge ", "c7i.2xlarge"},
		map[string]config.USDPerHour{"c7i.xlarge": 170000})
	if err != nil {
		t.Fatalf("resolveShapeTypes: %v", err)
	}
	if len(got) != 2 || got[0] != "c7i.xlarge" || got[1] != "c7i.2xlarge" {
		t.Errorf("normalized types = %v", got)
	}
}

// A MALFORMED, ZERO, NEGATIVE, OR DUPLICATE PRICE OVERRIDE IS REFUSED.
func TestParsePriceOverrides(t *testing.T) {
	bad := [][]string{
		{"c7i.xlarge"},                         // no '='
		{"=0.17"},                              // no type
		{"c7i.xlarge="},                        // no price
		{"c7i.xlarge=0"},                       // zero
		{"c7i.xlarge=-1"},                      // ParseUSDPerHour rejects the sign
		{"c7i.xlarge=0.17", "c7i.xlarge=0.20"}, // duplicate
	}
	for _, raw := range bad {
		if _, err := parsePriceOverrides(raw); err == nil {
			t.Errorf("parsePriceOverrides(%v) accepted a bad entry", raw)
		}
	}

	got, err := parsePriceOverrides([]string{"c7i.xlarge=0.17", "m7i.large=0.096"})
	if err != nil {
		t.Fatalf("parsePriceOverrides: %v", err)
	}
	if got["c7i.xlarge"] != 170000 || got["m7i.large"] != 96000 {
		t.Errorf("parsed overrides = %v", got)
	}
}

// AN EC2-ONLY FLAG IS REFUSED ON ANOTHER PROVIDER BY PRESENCE, so --max-vcpu 0 or
// a negative is caught the same as a positive — a value check would let it pass.
func TestRefuseEC2OnlyFlags(t *testing.T) {
	if err := refuseEC2OnlyFlags(config.ProviderDocker,
		map[string]bool{"max-vcpu": true}, false); err == nil ||
		!strings.Contains(err.Error(), "--max-vcpu") {
		t.Errorf("--max-vcpu not refused on docker by presence: %v", err)
	}
	if err := refuseEC2OnlyFlags(config.ProviderFirecracker,
		map[string]bool{"region": true, "subnet": true}, false); err == nil ||
		!strings.Contains(err.Error(), "--region") || !strings.Contains(err.Error(), "--subnet") {
		t.Errorf("ec2 flags not refused on firecracker: %v", err)
	}
	if err := refuseEC2OnlyFlags(config.ProviderDocker,
		map[string]bool{"org": true, "runner-group": true}, false); err != nil {
		t.Errorf("non-ec2 flags wrongly refused: %v", err)
	}
}

// DECLARED CAPACITY EXEMPTS ONLY THE TWO CAPACITY FLAGS.
//
// An emission can describe a machine it is not running on, which is what lets a
// controller generate an inventory for a host that has no billet on it yet. That
// exemption must not become a general amnesty: cloud PLACEMENT on a host-run
// backend is still meaningless and still refused.
func TestDeclaredCapacityExemptsOnlyCapacity(t *testing.T) {
	for _, name := range declaredCapacityFlags {
		if err := refuseEC2OnlyFlags(config.ProviderFirecracker,
			map[string]bool{name: true}, true); err != nil {
			t.Errorf("--%s refused for a declaring emission: %v", name, err)
		}
		if err := refuseEC2OnlyFlags(config.ProviderFirecracker,
			map[string]bool{name: true}, false); err == nil {
			t.Errorf("--%s accepted without a declaring emission", name)
		}
	}

	// Placement is not capacity, and stays refused either way.
	for _, name := range []string{"region", "subnet", "security-group", "instance-type"} {
		if err := refuseEC2OnlyFlags(config.ProviderDocker,
			map[string]bool{name: true}, true); err == nil ||
			!strings.Contains(err.Error(), "--"+name) {
			t.Errorf("--%s was exempted along with capacity: %v", name, err)
		}
	}
}
