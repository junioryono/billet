package config

import (
	"slices"
	"strings"
	"testing"
)

// A WINDOWS TIER RUNS ON EC2 AND NOWHERE ELSE: every other backend would bind it and
// start something that never registers (ADR-023).
func TestAWindowsTierRunsOnEC2AndNowhereElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		providers []ProviderKind
		want      string
	}{
		{name: "ec2", providers: []ProviderKind{ProviderEC2}},
		{name: "docker", providers: []ProviderKind{ProviderDocker}, want: "runs only on the ec2 provider"},
		{name: "firecracker", providers: []ProviderKind{ProviderFirecracker}, want: "runs only on the ec2 provider"},
		{name: "simulated", providers: []ProviderKind{ProviderSimulated}, want: "runs only on the ec2 provider"},
		{name: "tart", providers: []ProviderKind{ProviderTart}, want: "cannot run Windows guests"},
		{name: "codebuild", providers: []ProviderKind{ProviderCodeBuild}, want: "only as an EC2 AMI"},
		{name: "ec2 with a fallback", providers: []ProviderKind{ProviderEC2, ProviderFirecracker},
			want: `also accepts "firecracker"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := Tier{Label: "win", GuestOS: GuestWindows, Providers: tc.providers}.GuestOSProviderErrors("tier")

			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("a Windows tier on ec2 was refused: %v", errs)
				}

				return
			}

			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("errors = %v, want one saying %q", errs, tc.want)
			}
		})
	}
}

// A LINUX TIER IS NOT ASKED ANY OF THIS.
func TestALinuxTierIsNotHeldToTheWindowsRule(t *testing.T) {
	t.Parallel()

	for _, p := range []ProviderKind{ProviderDocker, ProviderFirecracker, ProviderEC2} {
		if errs := (Tier{Label: "l", GuestOS: GuestLinux, Providers: []ProviderKind{p}}).GuestOSProviderErrors("tier"); len(errs) != 0 {
			t.Errorf("%s: %v", p, errs)
		}
	}
}

func TestAWindowsTiersDefaultCommandIsTheEntryPointItsImageCarries(t *testing.T) {
	t.Parallel()

	windows := Tier{GuestOS: GuestWindows, Provider: ProviderEC2}
	if got := windows.RunnerCommandFor(ProviderEC2); !slices.Equal(got, []string{WindowsRunnerCommand}) {
		t.Errorf("the default command is %q", got)
	}

	windows.Command = []string{`C:\own\start.ps1`}
	if got := windows.RunnerCommandFor(ProviderEC2); !slices.Equal(got, []string{`C:\own\start.ps1`}) {
		t.Errorf("a tier's own command became %q", got)
	}

	linux := Tier{GuestOS: GuestLinux, Provider: ProviderEC2}
	if got := linux.RunnerCommandFor(ProviderEC2); !slices.Equal(got, []string{"/usr/local/bin/billet-runner"}) {
		t.Errorf("a Linux tier's default command became %q", got)
	}
}

// WHAT A WINDOWS GUEST CANNOT USE IS REFUSED, rather than left to read as working.
func TestAWindowsTierRefusesWhatItsGuestHasNoClientFor(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		mutate func(*Tier)
		want   string
	}{
		"nothing":     {mutate: func(*Tier) {}},
		"arch x64":    {mutate: func(t *Tier) { t.Arch = ArchX64 }},
		"arch arm64":  {mutate: func(t *Tier) { t.Arch = ArchARM64 }, want: "runs on x64"},
		"a cache":     {mutate: func(t *Tier) { t.Cache = &TierCache{} }, want: "no client for billet's caches"},
		"intercept":   {mutate: func(t *Tier) { t.Intercept = true }, want: "no client for billet's caches"},
		"cache_scope": {mutate: func(t *Tier) { t.CacheScope = &CacheScope{} }, want: "no client for billet's caches"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tier := Tier{Label: "win", GuestOS: GuestWindows, Provider: ProviderEC2}
			tc.mutate(&tier)

			errs := tier.windowsTierErrors("tier")

			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v", errs)
				}

				return
			}

			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("errors = %v, want one saying %q", errs, tc.want)
			}
		})
	}
}

// THROUGH THE VALIDATOR A FILE PASSES THROUGH, so the rule above is proved used: a helper
// nothing calls would leave every case over it green.
func TestAWindowsTierWithACacheIsRefusedThroughValidation(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	tier := Tier{
		Label: "win", GuestOS: GuestWindows, Provider: ProviderEC2, VCPU: 2, Memory: 8 * GiB,
		Image: "ami-0abc", Cache: &TierCache{},
	}

	errs := cfg.validateGuestOSRules("tier", &tier)
	if !slices.ContainsFunc(errs, func(err error) bool {
		return strings.Contains(err.Error(), "no client for billet's caches")
	}) {
		t.Fatalf("errors = %v, want the cache refused", errs)
	}

	tier.Cache = nil
	tier.Provider = ProviderDocker

	errs = cfg.validateGuestOSRules("tier", &tier)
	if !slices.ContainsFunc(errs, func(err error) bool {
		return strings.Contains(err.Error(), "runs only on the ec2 provider")
	}) {
		t.Fatalf("errors = %v, want docker refused", errs)
	}
}
