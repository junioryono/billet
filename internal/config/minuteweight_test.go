package config

import (
	"strings"
	"testing"
)

func TestAMinuteWeightIsAnExactDecimalOrRefused(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]MinuteWeight{
		"1": 1000, "2": 2000, "0.5": 500, "1.5": 1500, "0.001": 1, "10.25": 10250, "1000": 1000000,
	} {
		got, err := ParseMinuteWeight(in)
		if err != nil || got != want {
			t.Errorf("ParseMinuteWeight(%q) = %d, %v; want %d", in, got, err, want)
		}
	}

	for _, in := range []string{"", "0", "0.0", "-1", "+1", "1e3", "1.", ".5", "1.2345", "1000.001", "10000", "abc", "1.5.5"} {
		if got, err := ParseMinuteWeight(in); err == nil {
			t.Errorf("ParseMinuteWeight(%q) = %d, want a refusal", in, got)
		}
	}
}

func TestAMinuteWeightWritesTheDecimalItRead(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"1", "0.5", "1.5", "0.001", "10.25", "1000"} {
		got, err := ParseMinuteWeight(in)
		if err != nil {
			t.Fatalf("ParseMinuteWeight(%q): %v", in, err)
		}

		if got.Decimal() != in {
			t.Errorf("%q wrote back as %q", in, got.Decimal())
		}
	}
}

// BOTH DIRECTIONS OF THE KEY, through the config an operator writes: a weight is
// taken as written, and one that is not a weight stops the load.
func TestATierNamesItsMinuteWeightOrIsRefused(t *testing.T) {
	const anchor = "    vcpu: 4\n"

	cfg, err := Load(writeConfig(t, strings.Replace(validConfig, anchor, anchor+"    minute_weight: 1.5\n", 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tier, ok := cfg.TierByLabel("billet-4vcpu-ubuntu-2404")
	if !ok {
		t.Fatal("the tier went missing")
	}
	if tier.MinuteWeight != 1500 {
		t.Errorf("minute_weight = %d, want 1500", tier.MinuteWeight)
	}

	other, ok := cfg.TierByLabel("billet-8vcpu-ubuntu-2404")
	if !ok {
		t.Fatal("the other tier went missing")
	}
	if other.MinuteWeight != 0 {
		t.Errorf("a tier naming no weight holds %d, want 0", other.MinuteWeight)
	}

	for _, bad := range []string{"0", "-2", "2x", "1.2345", "1001"} {
		if _, err := Load(writeConfig(t, strings.Replace(validConfig, anchor,
			anchor+"    minute_weight: "+bad+"\n", 1))); err == nil || !strings.Contains(err.Error(), "minute_weight") {
			t.Errorf("minute_weight %s loaded with %v, want a refusal naming the key", bad, err)
		}
	}
}
