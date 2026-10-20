package config

import (
	"fmt"
	"slices"
	"strings"
)

// ImageComponent is a part of GitHub's runner image that billet's published guest
// image leaves out, because its license forbids billet redistributing it (#584). A
// deployment that accepts it in images.accept has it installed from its vendor
// into each new generation, inside a completion microVM, before the generation is
// imported.
type ImageComponent string

// The components a deployment may accept. Each name is the one a published
// manifest declares and the one a generation records.
const (
	ComponentAndroidSDK    ImageComponent = "android-sdk"
	ComponentCodeQL        ImageComponent = "codeql"
	ComponentFirefox       ImageComponent = "firefox"
	ComponentGoogleChrome  ImageComponent = "google-chrome"
	ComponentMicrosoftEdge ImageComponent = "microsoft-edge"
)

// ImageComponents is every component a deployment may accept, sorted.
func ImageComponents() []ImageComponent {
	return []ImageComponent{
		ComponentAndroidSDK,
		ComponentCodeQL,
		ComponentFirefox,
		ComponentGoogleChrome,
		ComponentMicrosoftEdge,
	}
}

// IsImageComponent reports whether name is a component this build knows.
func IsImageComponent(name string) bool {
	return slices.Contains(ImageComponents(), ImageComponent(name))
}

// Accepted is the set of components this deployment accepts, sorted, with no
// duplicates; a nil config accepts none.
func (c *ImagesConfig) Accepted() []ImageComponent {
	if c == nil {
		return nil
	}

	out := make([]ImageComponent, 0, len(c.Accept))
	for _, name := range c.Accept {
		if !slices.Contains(out, ImageComponent(name)) {
			out = append(out, ImageComponent(name))
		}
	}
	slices.Sort(out)

	return out
}

// validateImages refuses an images.accept naming a component this build does not
// know or naming one twice.
//
// AN UNKNOWN NAME IS NOT IGNORED. Accepting a license is the operator's act, so a
// misspelt "google-crome" that installed nothing would leave a deployment that
// believes it accepted Chrome and a tier that quietly lacks it.
func (c *Config) validateImages() []error {
	if c.Images == nil {
		return nil
	}

	var errs []error
	seen := map[string]bool{}
	for _, name := range c.Images.Accept {
		if !IsImageComponent(name) {
			known := make([]string, 0, len(ImageComponents()))
			for _, k := range ImageComponents() {
				known = append(known, string(k))
			}
			errs = append(errs, fmt.Errorf("images.accept: %q is not a component billet installs; "+
				"the components are %s", name, strings.Join(known, ", ")))

			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("images.accept: %q is named twice", name))
		}
		seen[name] = true
	}

	return errs
}
