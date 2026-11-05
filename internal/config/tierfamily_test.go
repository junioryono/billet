package config

import "testing"

// A FAMILY IS EVERY SETTING BUT A TIER'S NAME, SIZE AND CAPACITY: anything else that
// differs is another kind of tier, and room nothing measures is never made smaller.
func TestATiersFamilyIsEverySettingButItsSize(t *testing.T) {
	t.Parallel()

	base := Tier{Label: "small", Target: "default", RunsOn: "runs-small", Provider: ProviderFirecracker,
		GuestOS: GuestLinux, VCPU: 2, Memory: 8 * GiB, Image: "ubuntu-2404-x64@verified", Disk: 20 * GiB}

	for name, tc := range map[string]struct {
		change func(*Tier)
		same   bool
	}{
		"a larger size and its own names": {func(t *Tier) { t.Label, t.RunsOn, t.VCPU, t.Memory = "big", "runs-big", 8, 32*GiB }, true},
		"its own capacity":                {func(t *Tier) { t.Label, t.WarmPool, t.MaxConcurrent, t.Reserved = "big", 2, 4, 1 }, true},
		"more disk":                       {func(t *Tier) { t.Label, t.Disk = "big", 40*GiB }, true},
		"less disk":                       {func(t *Tier) { t.Label, t.Disk = "big", 10*GiB }, false},
		"a default disk for a set one":    {func(t *Tier) { t.Label, t.Disk = "big", 0 }, false},
		"another image":                   {func(t *Tier) { t.Label, t.Image = "big", "ubuntu-2204-x64@verified" }, false},
		"another command":                 {func(t *Tier) { t.Label, t.Command = "big", []string{"./x"} }, false},
		"another target":                  {func(t *Tier) { t.Label, t.Target = "big", "other" }, false},
		"another runner group":            {func(t *Tier) { t.Label, t.RunnerGroup = "big", "ci" }, false},
		"itself":                          {func(t *Tier) {}, false},
	} {
		other := base
		tc.change(&other)
		if got := base.SameFamily(other); got != tc.same {
			t.Errorf("%s: SameFamily = %t, want %t", name, got, tc.same)
		}
	}

	defaulted := base
	defaulted.Disk = 0
	set := defaulted
	set.Label, set.Disk = "big", 40*GiB
	if defaulted.SameFamily(set) {
		t.Error("a set disk counted as no less than a backend's default, which nobody here can say")
	}
}
