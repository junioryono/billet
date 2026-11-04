package config

import (
	"strings"
	"testing"
)

// TWO SPELLINGS OF ONE IMAGE SHARE ONE VERIFICATION, and a registry's port is not a tag: read
// as one, ghcr.example:5000/a/b would name no tag and tart would read it as :latest.
func TestATartImageKeyIsTheReferenceAsTartResolvesIt(t *testing.T) {
	t.Parallel()

	for image, want := range map[string]string{
		"ghcr.io/cirruslabs/macos:latest":          "ghcr.io/cirruslabs/macos:latest",
		"ghcr.io/cirruslabs/macos":                 "ghcr.io/cirruslabs/macos:latest",
		"ghcr.io/cirruslabs/macos@verified":        "ghcr.io/cirruslabs/macos:latest",
		"ghcr.io/cirruslabs/macos:26@verified":     "ghcr.io/cirruslabs/macos:26",
		"registry.example:5000/a/b":                "registry.example:5000/a/b:latest",
		"registry.example:5000/a/b:7":              "registry.example:5000/a/b:7",
		"ghcr.io/a/b@sha256:0123":                  "ghcr.io/a/b@sha256:0123",
		"my-local-vm":                              "my-local-vm",
		"my-local-vm@verified":                     "my-local-vm",
		"  ghcr.io/cirruslabs/macos:26@verified  ": "ghcr.io/cirruslabs/macos:26",
	} {
		if got := TartImageKey(image); got != want {
			t.Errorf("TartImageKey(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestOnlyANamedImageIsVerified(t *testing.T) {
	t.Parallel()

	for image, want := range map[string]bool{
		"ghcr.io/a/b@verified": true,
		"local@verified":       true,
		"@verified":            false,
		"ghcr.io/a/b":          false,
		"ghcr.io/a/b@sha256:0": false,
	} {
		if _, got := TartVerifiedImage(image); got != want {
			t.Errorf("TartVerifiedImage(%q) = %v, want %v", image, got, want)
		}
	}
}

// A DIGEST AND @verified SAY TWO THINGS ABOUT ONE IMAGE, so the tier is refused rather than
// one of them being ignored, and the refusal reaches every path a catalogue is loaded by,
// because alloc.New applies the same check to one built in code. Another backend's image is
// not tart's to judge.
func TestATierCannotCombineVerifiedWithADigest(t *testing.T) {
	t.Parallel()

	tart := []ProviderKind{ProviderTart}

	for name, tc := range map[string]struct {
		tier Tier
		says string
	}{
		"a digest at the top level": {
			tier: Tier{Providers: tart, Image: "ghcr.io/a/b@sha256:0123@verified"},
			says: "with a digest",
		},
		"a digest for tart": {
			tier: Tier{Launch: map[ProviderKind]TierLaunch{
				ProviderTart: {Image: "ghcr.io/a/b@sha256:0123@verified"},
			}},
			says: "with a digest",
		},
		"nothing before it": {
			tier: Tier{Providers: tart, Image: "@verified"}, says: "names no image",
		},
		"a tag":      {tier: Tier{Providers: tart, Image: "ghcr.io/a/b:1@verified"}},
		"a local VM": {tier: Tier{Providers: tart, Image: "local@verified"}},
		"no verification at all, with a digest": {
			tier: Tier{Providers: tart, Image: "ghcr.io/a/b@sha256:0123"},
		},
		"another backend's image, which tart never reads": {
			tier: Tier{Providers: []ProviderKind{ProviderFirecracker}, Image: "a@sha256:0123@verified"},
		},
		"another backend's image beside a good one for tart": {
			tier: Tier{
				Providers: []ProviderKind{ProviderFirecracker, ProviderTart},
				Image:     "a@sha256:0123@verified",
				Launch: map[ProviderKind]TierLaunch{
					ProviderTart: {Image: "ghcr.io/a/b:1@verified"},
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			errs := tc.tier.ImageOptionErrors("tiers[0]")
			if tc.says == "" {
				if len(errs) != 0 {
					t.Fatalf("refused: %v", errs)
				}

				return
			}

			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.says) {
				t.Fatalf("answered %v, want one error saying %q", errs, tc.says)
			}
		})
	}
}

// AN OLDER NODE HANDS `@verified` TO `tart fqn` AS A NAME, so what holds it back asks of the
// image a Tart node would launch, which a tier's launch block can set apart from its image.
func TestTheCatalogueSaysWhetherATartTierNamesVerified(t *testing.T) {
	t.Parallel()

	tart := []ProviderKind{ProviderTart}

	for name, tc := range map[string]struct {
		tiers []Tier
		want  bool
	}{
		"none": {tiers: []Tier{{Providers: tart, Image: "ghcr.io/a/b:1"}}},
		"one":  {tiers: []Tier{{Providers: tart, Image: "ghcr.io/a/b:1@verified"}}, want: true},
		"in a launch block": {
			tiers: []Tier{{Providers: tart, Launch: map[ProviderKind]TierLaunch{ProviderTart: {Image: "local@verified"}}}},
			want:  true,
		},
		"another backend's, which means something else there": {
			tiers: []Tier{{Providers: []ProviderKind{ProviderFirecracker}, Image: "ubuntu-2404-x64@verified"}},
		},
	} {
		if got := CatalogueNamesTartVerified(tc.tiers); got != tc.want {
			t.Errorf("%s: answered %v, want %v", name, got, tc.want)
		}
	}
}

// THE WHOLE LOAD, because a rule refusing a name with an @ in it elsewhere in validation
// would leave the spelling unusable while the unit tests above stayed green.
func TestAFileNamingAVerifiedTartImageLoads(t *testing.T) {
	t.Parallel()

	for image, wantErr := range map[string]string{
		"ghcr.io/cirruslabs/macos-tahoe-xcode:26@verified": "",
		"macos-26@verified":                "",
		"ghcr.io/a/b@sha256:0123@verified": "with a digest",
		"@verified":                        "names no image",
	} {
		body := validConfig + strings.Replace(macOSTier, "image: macos-26", "image: \""+image+"\"", 1)

		_, err := Load(writeConfig(t, body))

		switch {
		case wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", image, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("%s: answered %v, want %q", image, err, wantErr)
		}
	}
}
