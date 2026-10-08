package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A WAITING TIER IS NAMED, each by its own hosts, and a tier that is not
// waiting never is.
func TestStatusNamesATierWaitingForACacheAwareHost(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{{Label: "strict"}, {Label: "plain"}}
	lines, err := cacheAwareWaits(tiers, func(t config.Tier) (bool, error) { return t.Label == "strict", nil })
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "tier strict WAITS") {
		t.Fatalf("lines = %q, %v; want the strict tier alone named", lines, err)
	}
}
