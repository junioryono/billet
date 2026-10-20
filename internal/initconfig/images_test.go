package initconfig

import (
	"regexp"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// A GENERATED FIRECRACKER CONFIG ACCEPTS NOTHING, AND SAYS WHAT IT COULD. Each
// component billet knows is named with a link to its license, so a component
// added to the closed set without a line here fails, and the block loads as an
// empty acceptance rather than an absent one.
func TestAGeneratedFirecrackerConfigAcceptsNoComponentAndNamesEach(t *testing.T) {
	body := mustGenerate(t, firecrackerParams())

	cfg := parse(t, body)
	if cfg.Images == nil || cfg.Images.Accept == nil || len(cfg.Images.Accept) != 0 {
		t.Fatalf("images.accept is %+v, want written and empty", cfg.Images)
	}

	assertNamesEveryComponent(t, body)
}

// A CONFIG FOR A BACKEND THAT BOOTS NO IMAGE BILLET PUBLISHES CARRIES NO BLOCK.
func TestAGeneratedDockerConfigCarriesNoImagesBlock(t *testing.T) {
	p := firecrackerParams()
	p.Provider = config.ProviderDocker
	p.RunnerGroup = "billet-trial"
	p.Workflows = []string{trialWorkflow}

	if body := mustGenerate(t, p); strings.Contains(body, "images:") {
		t.Errorf("a docker config carries an images block:\n%s", body)
	}
}

// THE HYBRID FIRECRACKER HOST CARRIES THE SAME BLOCK, inside the billet_config the
// host role renders verbatim.
func TestTheHybridFirecrackerHostAcceptsNoComponentAndNamesEach(t *testing.T) {
	body := renderHybridLocal(HybridParams{}, false, "10.0.0.1:7717", 8, 16*config.GiB)

	var doc struct {
		Images struct {
			Accept []string `yaml:"accept"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the hybrid host's config is not YAML: %v\n%s", err, body)
	}
	if doc.Images.Accept == nil || len(doc.Images.Accept) != 0 {
		t.Fatalf("images.accept is %v, want written and empty:\n%s", doc.Images.Accept, body)
	}

	assertNamesEveryComponent(t, body)
}

func assertNamesEveryComponent(t *testing.T, body string) {
	t.Helper()

	// AN ENTRY RUNS FROM ITS NAME TO THE NEXT NAME, or to the end of the list, and
	// its continuation lines are indented past the names.
	starts := regexp.MustCompile(`(?m)^# {3}([a-z][a-z-]*) +\S`).FindAllStringSubmatchIndex(body, -1)
	entries := map[string]string{}
	for i, at := range starts {
		end := strings.Index(body[at[0]:], "\n#\n")
		if end < 0 {
			end = len(body) - at[0]
		}
		end += at[0]
		if i+1 < len(starts) && starts[i+1][0] < end {
			end = starts[i+1][0]
		}
		entries[body[at[2]:at[3]]] = body[at[0]:end]
	}

	for _, c := range config.ImageComponents() {
		entry, found := entries[string(c)]
		if !found {
			t.Errorf("the generated config does not describe %s", c)

			continue
		}
		if !strings.Contains(entry, "(https://") {
			t.Errorf("the generated config names %s without a link to its license:\n%s", c, entry)
		}
	}
}
