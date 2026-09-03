package initconfig

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// AnsibleVar is the variable the junioryono.billet.host role renders billet.yaml
// from. ONE constant read by the emitter and by the test that pins it to the
// role's template, so a rename there cannot silently produce a block the role
// ignores — which would converge a host with no config change and no error.
const AnsibleVar = "billet_config"

// AnsibleVars nests a generated config under the variable the host role reads.
//
// The role's template is `{{ billet_config | to_nice_yaml }}` — the value IS the
// billet.yaml, verbatim — so the only thing between a generated config and a
// usable inventory entry is this indentation. Without it the operator retypes
// the generator's output by hand, which is how a ceiling ends up naming more
// vCPU than the machine has.
//
// INDENTED TEXTUALLY RATHER THAN RE-ENCODED, so the generated comments survive.
// They say why the ceiling is what it is and what the host still has to provide,
// and the inventory is the file a person reads and edits. Ansible drops them when
// it renders, which is correct: they are for the reader, not for the host.
func AnsibleVars(body string, companions map[string]bool) (string, error) {
	// A mapping, PROVED rather than assumed. Everything below is string handling,
	// so a body that was not a mapping would indent just as happily and produce an
	// inventory whose billet_config is a string or a list — which the role renders
	// into a billet.yaml that fails to load on the host, one converge later.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return "", fmt.Errorf("the generated config is not YAML: %w", err)
	}
	if len(doc) == 0 {
		return "", errors.New("the generated config has no keys")
	}

	var b strings.Builder

	// THE BLOCK HAS TO CONVERGE ON ITS OWN, and billet_config alone does not.
	// The role's provisioning flags default to a Firecracker host, and it REFUSES
	// a docker or ec2 node while they are set — so the emission for those backends
	// is a valid config the named role declines before it renders anything. Naming
	// them here is the difference between an inventory entry and a fragment with a
	// footnote nobody read.
	//
	// Sorted, so the same generation emits the same bytes and re-running produces
	// a diff only where something actually changed.
	for _, name := range slices.Sorted(maps.Keys(companions)) {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(strconv.FormatBool(companions[name]))
		b.WriteString("\n")
	}

	b.WriteString(AnsibleVar + ":\n")
	for line := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
		// A blank line stays blank. Indenting it would leave trailing whitespace
		// in the operator's inventory, which several editors strip on save and
		// then show as a diff nobody made.
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")

			continue
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String(), nil
}

// AnsibleCompanions are the role variables that must accompany a generation for
// the role to accept it, keyed by name.
//
// The role provisions a Firecracker host by default — guest bridges and a Ceph
// cluster — and asserts that those flags MATCH the node's provider before it
// touches anything. A docker or ec2 node with the defaults left alone fails that
// assertion, so the two backends that need them off say so in the block itself.
// A firecracker node needs nothing here: the defaults already describe it, and
// stating billet_ceph_enabled for it would claim this host should bootstrap a
// cluster, which is a decision the operator makes and not one a generator can.
func AnsibleCompanions(p config.ProviderKind) map[string]bool {
	if p == config.ProviderFirecracker {
		return nil
	}

	return map[string]bool{
		"billet_firecracker_enabled": false,
		"billet_ceph_enabled":        false,
	}
}
