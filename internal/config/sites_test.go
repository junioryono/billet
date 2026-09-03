package config

import (
	"strings"
	"testing"
)

// A NODE'S SITE IS NOT THIS FILE'S TO JUDGE, and asserting otherwise was wrong
// in exactly the deployment sites exist for.
//
// This config may be the NODE's, on its own machine, where a sites block has no
// reason to exist — sites are declared by the control plane. An earlier version
// of this validation refused node.site whenever the local file listed no sites,
// which would have turned away every remote node that correctly named one of the
// server's places.
//
// The claim is checked where the answer lives: at registration, by the control
// plane. See TestANodeCannotRegisterIntoAnUndeclaredSite in internal/nodeplane.
func TestANodesOwnConfigDoesNotJudgeItsSite(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1", "  name: epyc-1\n  site: anywhere", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a node naming a site its own config does not declare was refused: %v", err)
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

// A tier's site IS this file's to judge, because tiers and sites are declared
// together, in the control plane's config, by the same person.
func TestATierSiteWithoutASitesBlockIsRefused(t *testing.T) {
	body := strings.Replace(validConfig, "  - label: billet-4vcpu-ubuntu-2404",
		"  - label: billet-4vcpu-ubuntu-2404\n    site: home", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a tier with a site but no sites block")
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
	body = strings.Replace(body, "  - label: billet-4vcpu-ubuntu-2404",
		"  - label: billet-4vcpu-ubuntu-2404\n    site: home", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a node and tier at a declared site were refused: %v", err)
	}
}

// A DUPLICATE IS AMBIGUOUS RATHER THAN REDUNDANT once a site carries storage
// configuration: two blocks with one name give two answers to "which
// storage", and the one that wins would be whichever the parser saw last.
func TestADuplicateSiteIsRefused(t *testing.T) {
	body := withSites("home", "home")

	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted the same site declared twice")
	}
}

// An unnamed site cannot be referred to, so it can only be a mistake.
func TestASiteMustHaveAName(t *testing.T) {
	body := validConfig + "\nsites:\n  - name: \"\"\n    store: ceph\n"

	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a site with no name")
	}
}

func TestASiteMustSelectAStorageBackend(t *testing.T) {
	t.Parallel()

	for name, storage := range map[string]string{
		"missing": "",
		"unknown": "magic-disk",
	} {
		body := validConfig + "\nsites:\n  - name: home\n    store: " + storage + "\n"
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s site storage was accepted", name)
		}
	}
}

func TestAnAWSSiteMaySelectEBSAndS3Storage(t *testing.T) {
	t.Parallel()

	body := validConfig + "\nsites:\n  - name: somewhere\n    store: ebs-s3\n"
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load rejected the implemented ebs-s3 site store: %v", err)
	}
}

// withSites is the valid config plus a sites block declaring each name.
func withSites(names ...string) string {
	var b strings.Builder

	b.WriteString(validConfig)
	b.WriteString("\nsites:\n")

	for _, n := range names {
		b.WriteString("  - name: " + n + "\n    store: ceph\n")
	}

	return b.String()
}
