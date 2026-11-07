package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// withEgress is validConfig's tier with a network block inserted after its image.
func withEgress(t *testing.T, block string) string {
	t.Helper()

	body := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n", "    image: ubuntu-2404-x64\n"+block, 1)
	if body == validConfig {
		t.Fatal("the tier in validConfig has changed, so this case patches nothing")
	}

	return body
}

func TestATierWithNoNetworkBlockIsOpen(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Tiers[0].EgressOf(); got != nil {
		t.Fatalf("EgressOf = %+v, want nil: a tier that says nothing stays open", got)
	}

	if cfg.Tiers[0].NeedsEgressAwareNode() {
		t.Fatal("an open tier was held to a node that reads network.egress")
	}
}

func TestAListedTierKeepsItsEntriesAsWritten(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withEgress(t,
		"    network:\n      egress:\n        allow: [Example.com, \"*.npmjs.org\", 203.0.113.0/24]\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Tiers[0].EgressOf()
	if got == nil || strings.Join(got.Allow, " ") != "Example.com *.npmjs.org 203.0.113.0/24" {
		t.Fatalf("EgressOf = %+v", got)
	}

	if !cfg.Tiers[0].NeedsEgressAwareNode() {
		t.Fatal("a listed tier was not held to a node that reads network.egress")
	}
}

// AN EMPTY LIST IS A LIST, the most restrictive there is, never an open tier.
func TestAnEmptyAllowListIsStillListed(t *testing.T) {
	t.Parallel()

	for name, block := range map[string]string{
		"empty":  "    network:\n      egress:\n        allow: []\n",
		"absent": "    network:\n      egress: {}\n",
		"null":   "    network:\n      egress:\n        allow:\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(writeConfig(t, withEgress(t, block)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			got := cfg.Tiers[0].EgressOf()
			if got == nil || len(got.Allow) != 0 {
				t.Fatalf("EgressOf = %+v, want a list with nothing on it", got)
			}
		})
	}
}

func TestTheNetworkBlockRefusesWhatWouldLeaveATierOpen(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ block, want string }{
		"egress with nothing after it": {
			block: "    network:\n      egress:\n", want: "would not restrict the tier",
		},
		"a misspelled allow": {
			block: "    network:\n      egress:\n        allowed: [example.com]\n", want: `no key "allowed"`,
		},
		"a misspelled egress": {
			block: "    network:\n      egres:\n        allow: [example.com]\n", want: `no key "egres"`,
		},
		"a network that is not a mapping": {
			block: "    network: closed\n", want: "network must be a mapping",
		},
		"an entry that is not a list": {
			block: "    network:\n      egress:\n        allow: {a: b}\n", want: "must be a list",
		},
		"an entry that cannot be read": {
			block: "    network:\n      egress:\n        allow: [\"not a name\"]\n", want: "is not a name",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(writeConfig(t, withEgress(t, tc.block)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestParseEgressEntry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw      string
		name     string
		wildcard bool
		prefix   string
	}{
		{raw: "example.com", name: "example.com"},
		{raw: "Example.COM", name: "example.com"},
		{raw: "localhost", name: "localhost"},
		{raw: "*.example.com", name: "example.com", wildcard: true},
		{raw: "*.Sub.Example.com", name: "sub.example.com", wildcard: true},
		{raw: "203.0.113.0/24", prefix: "203.0.113.0/24"},
		{raw: "203.0.113.7/32", prefix: "203.0.113.7/32"},
		{raw: "203.0.113.7", prefix: "203.0.113.7/32"},
	} {
		got, err := ParseEgressEntry(tc.raw)
		if err != nil {
			t.Errorf("ParseEgressEntry(%q): %v", tc.raw, err)

			continue
		}

		prefix := ""
		if got.Prefix.IsValid() {
			prefix = got.Prefix.String()
		}

		if got.Name != tc.name || got.Wildcard != tc.wildcard || prefix != tc.prefix {
			t.Errorf("ParseEgressEntry(%q) = %+v, want name %q wildcard %v prefix %q", tc.raw, got, tc.name,
				tc.wildcard, tc.prefix)
		}
	}
}

func TestParseEgressEntryRefuses(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"":                   "empty",
		"example.com.":       "ends in a dot",
		"*.example.com.":     "ends in a dot",
		"*":                  "not a name",
		"*.com":              "top-level domain",
		"foo.*.com":          "not a name",
		"*example.com":       "not a name",
		"exa mple.com":       "not a name",
		"a..b":               "not a name",
		"-a.example.com":     "not a name",
		"203.0.113":          "no name ends in digits",
		"203.0.113.0/33":     "not an address prefix",
		"example.com/24":     "not an address prefix",
		"203.0.113.5/24":     "write 203.0.113.0/24",
		"0.0.0.0/0":          "every address",
		"2001:db8::/32":      "is IPv6",
		"2001:db8::1":        "is IPv6",
		"::ffff:192.0.2.1":   "is IPv6",
		"fe80::1%eth0":       "is IPv6",
		"https://github.com": "not an address prefix",
	} {
		_, err := ParseEgressEntry(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseEgressEntry(%q) = %v, want an error naming %q", raw, err, want)
		}
	}
}

// ANY PROVIDER A TIER LISTS MAY BE WHERE ITS JOB LANDS, so one that cannot
// enforce the list refuses the tier, whichever position it holds.
func TestOnlyFirecrackerMayCarryAnAllowlist(t *testing.T) {
	t.Parallel()

	listed := &TierNetwork{Egress: &TierEgress{Allow: []string{"example.com"}}}

	if errs := (Tier{Provider: ProviderFirecracker, Network: listed}).EgressErrors("tier"); len(errs) != 0 {
		t.Fatalf("a firecracker tier was refused its list: %v", errs)
	}

	for _, providers := range [][]ProviderKind{
		{ProviderDocker},
		{ProviderTart},
		{ProviderCodeBuild},
		{ProviderEC2},
		{ProviderSimulated},
		{ProviderFirecracker, ProviderEC2},
		{ProviderEC2, ProviderFirecracker},
	} {
		tier := Tier{Providers: providers, Network: listed}

		errs := tier.EgressErrors("tier")
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "would run open") {
			t.Errorf("providers %v: errors = %v, want the one refusal", providers, errs)
		}
	}

	// AND NOTHING IS ASKED OF A TIER WITH NO LIST.
	if errs := (Tier{Provider: ProviderEC2}).EgressErrors("tier"); len(errs) != 0 {
		t.Fatalf("an open EC2 tier was refused: %v", errs)
	}
}

func TestAnAllowlistIsBounded(t *testing.T) {
	t.Parallel()

	allow := make([]string, 0, MaxEgressEntries+1)
	for i := range MaxEgressEntries {
		allow = append(allow, fmt.Sprintf("n%d.example.com", i))
	}

	tier := Tier{Provider: ProviderFirecracker, Network: &TierNetwork{Egress: &TierEgress{Allow: allow}}}
	if errs := tier.EgressErrors("tier"); len(errs) != 0 {
		t.Fatalf("a list at the bound was refused: %v", errs)
	}

	allow = append(allow, "one-more.example.com")
	tier.Network.Egress.Allow = allow

	errs := tier.EgressErrors("tier")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "257 entries") {
		t.Fatalf("errors = %v, want the bound named", errs)
	}
}

func TestGitHubsRunnerEndpointsAreEntriesThisGrammarReads(t *testing.T) {
	t.Parallel()

	for _, raw := range GitHubRunnerEndpoints() {
		if _, err := ParseEgressEntry(raw); err != nil {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// THE LIST ITSELF, AS READ FROM GITHUB'S REFERENCE ON 2026-11-04: every other test loops over
// the function's own output, and an empty list would pass them all while every listed tier
// sat in a queue no runner could take.
func TestGitHubsRunnerEndpointsAreTheOnesItsReferenceNames(t *testing.T) {
	t.Parallel()

	want := []string{
		"github.com", "api.github.com", "codeload.github.com", "*.actions.githubusercontent.com",
		"*.blob.core.windows.net", "objects.githubusercontent.com", "objects-origin.githubusercontent.com",
		"github-releases.githubusercontent.com", "github-registry-files.githubusercontent.com",
		"release-assets.githubusercontent.com",
	}

	if got := GitHubRunnerEndpoints(); !slices.Equal(got, want) {
		t.Fatalf("GitHubRunnerEndpoints = %q, want %q", got, want)
	}
}
