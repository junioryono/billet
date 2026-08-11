package config

import (
	"strings"
	"testing"
)

// A SITE IS A PLACE WHERE COMPUTE AND ITS STORAGE SHARE A FAST NETWORK, and the
// reason it is a declared block rather than a free string is that the failure it
// prevents is silent.
//
// A cache belongs to a site. A node that means "home" and types "hom" does not
// get an error under a free-string scheme — it gets its own site, with its own
// empty cache, and every job placed there runs cold while looking entirely
// healthy. Declaring the set once and refusing anything outside it turns that
// into a message at startup.
func TestANodeCannotJoinASiteThatWasNeverDeclared(t *testing.T) {
	body := withSites("home") + "\n"
	body = strings.Replace(body, "  name: epyc-1", "  name: epyc-1\n  site: hom", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a node whose site is not in the sites block")
	}

	if !strings.Contains(err.Error(), "hom") || !strings.Contains(err.Error(), "site") {
		t.Errorf("the error does not name the offending site: %v", err)
	}
}

// The same rule from the other side: a tier may REQUIRE a site, the way it can
// already pin a node, and requiring one that does not exist is a tier that can
// never be placed rather than a tier that is merely idle.
func TestATierCannotRequireASiteThatWasNeverDeclared(t *testing.T) {
	body := withSites("home")
	body = strings.Replace(body, "  - label: billet-4vcpu-ubuntu-2404",
		"  - label: billet-4vcpu-ubuntu-2404\n    site: elsewhere", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a tier pinned to a site that is not declared")
	}

	if !strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("the error does not name the offending site: %v", err)
	}
}

// NAMING A SITE WITH NO SITES BLOCK IS THE TYPO CASE, not the "sites are
// optional" case. An operator who wrote a site meant something by it, so
// silently ignoring the field is the one outcome that helps nobody.
func TestASiteWithoutASitesBlockIsRefused(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1", "  name: epyc-1\n  site: home", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a node with a site but no sites block")
	}

	if !strings.Contains(err.Error(), "sites") {
		t.Errorf("the error does not point at the missing sites block: %v", err)
	}
}

// Sites remain entirely optional: a single-machine deployment never writes one,
// and that has to keep loading exactly as it does today.
func TestASiteIsOptional(t *testing.T) {
	if _, err := Load(writeConfig(t, validConfig)); err != nil {
		t.Fatalf("a config with no sites block no longer loads: %v", err)
	}
}

// A declared site that a node and a tier both name is the ordinary case.
func TestADeclaredSiteIsAccepted(t *testing.T) {
	body := withSites("home")
	body = strings.Replace(body, "  name: epyc-1", "  name: epyc-1\n  site: home", 1)
	body = strings.Replace(body, "  - label: billet-4vcpu-ubuntu-2404",
		"  - label: billet-4vcpu-ubuntu-2404\n    site: home", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a node and tier at a declared site were refused: %v", err)
	}
}

// A DUPLICATE IS AMBIGUOUS RATHER THAN REDUNDANT once a site carries storage
// configuration (#23/#25): two blocks with one name give two answers to "which
// storage", and the one that wins would be whichever the parser saw last.
func TestADuplicateSiteIsRefused(t *testing.T) {
	body := withSites("home", "home")

	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted the same site declared twice")
	}
}

// An unnamed site cannot be referred to, so it can only be a mistake.
func TestASiteMustHaveAName(t *testing.T) {
	body := validConfig + "\nsites:\n  - name: \"\"\n"

	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a site with no name")
	}
}

// withSites is the valid config plus a sites block declaring each name.
func withSites(names ...string) string {
	var b strings.Builder

	b.WriteString(validConfig)
	b.WriteString("\nsites:\n")

	for _, n := range names {
		b.WriteString("  - name: " + n + "\n")
	}

	return b.String()
}
