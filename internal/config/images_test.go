package config

import (
	"slices"
	"strings"
	"testing"
)

func TestImagesAcceptTakesTheKnownComponents(t *testing.T) {
	cfg, err := Parse("billet.yaml", []byte(validConfig+
		"images:\n  accept: [microsoft-edge, android-sdk, google-chrome, codeql, firefox]\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := ImageComponents()
	if got := cfg.Images.Accepted(); !slices.Equal(got, want) {
		t.Fatalf("Accepted() = %v, want %v", got, want)
	}
}

// Nothing is accepted unless the operator writes it, with or without an images
// block.
func TestNothingIsAcceptedByDefault(t *testing.T) {
	for name, yaml := range map[string]string{
		"no images block":  validConfig,
		"an empty accept":  validConfig + "images:\n  accept: []\n",
		"only another key": validConfig + "images:\n  source: https://example.com/images\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse("billet.yaml", []byte(yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Images.Accepted(); len(got) != 0 {
				t.Fatalf("Accepted() = %v, want none", got)
			}
		})
	}
}

func TestImagesAcceptRefusesAnUnknownOrRepeatedComponent(t *testing.T) {
	for name, tc := range map[string]struct {
		accept, want string
	}{
		"a misspelling": {`[google-crome]`, `"google-crome" is not a component billet installs; ` +
			`the components are android-sdk, codeql, firefox, google-chrome, microsoft-edge`},
		"another case": {`[Google-Chrome]`, `"Google-Chrome" is not a component billet installs`},
		"a repeat":     {`[codeql, firefox, codeql]`, `"codeql" is named twice`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("billet.yaml", []byte(validConfig+"images:\n  accept: "+tc.accept+"\n"))
			if err == nil || !strings.Contains(err.Error(), "images.accept: "+tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}
