package initconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// generated is a real generation, so these tests run against what the CLI would
// emit rather than a hand-written stand-in that could stay valid while the
// generator changed shape.
func generated(t *testing.T) string {
	t.Helper()

	body, _, err := Generate(Params{
		Org:         "acme",
		Provider:    config.ProviderDocker,
		VCPU:        8,
		Memory:      16 * config.GiB,
		RunnerGroup: "billet-trial",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		Profile:     ProfileLocalService,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return body
}

// WHAT THE ROLE RENDERS MUST BE WHAT INIT GENERATED.
//
// The role's template is `{{ billet_config | to_nice_yaml }}`, so the value of
// that one key becomes the host's whole billet.yaml. An emission that lost a
// key, flattened the nesting, or landed the config beside the variable instead
// of under it would still be valid YAML and would still converge — producing a
// billet.yaml that is missing a section, which surfaces as a startup failure on
// the target rather than as an error here.
//
// Parsed the way Ansible parses it and compared structurally, so the assertion
// is about the data the role receives and not about the bytes.
func TestAnsibleVarsRoundTripsToTheGeneratedConfig(t *testing.T) {
	body := generated(t)

	block, err := AnsibleVars(body, nil)
	if err != nil {
		t.Fatalf("AnsibleVars: %v", err)
	}

	var vars map[string]any
	if err := yaml.Unmarshal([]byte(block), &vars); err != nil {
		t.Fatalf("the emitted block is not YAML: %v\n%s", err, block)
	}

	// Exactly one key: an inventory entry that also carried a stray top-level
	// key would set an unrelated Ansible variable on the host.
	if len(vars) != 1 {
		t.Fatalf("the block sets %d variables, want only %s: %v", len(vars), AnsibleVar, vars)
	}

	got, ok := vars[AnsibleVar]
	switch {
	case !ok:
		t.Fatalf("the block does not set %s: %v", AnsibleVar, vars)
	case got == nil:
		t.Fatalf("%s is null — the config landed beside the variable, not under it:\n%s",
			AnsibleVar, block)
	}

	var want any
	if err := yaml.Unmarshal([]byte(body), &want); err != nil {
		t.Fatalf("the generated config is not YAML: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the role would render a different config than init generated\n got %#v\nwant %#v",
			got, want)
	}
}

// THE COMMENTS ARE THE REASON THIS INDENTS TEXT RATHER THAN RE-ENCODING.
//
// A re-encode through yaml.Marshal produces the same data and drops every
// comment, and the comments are what tell the operator why the ceiling is what
// it is and what the host still has to provide. The inventory is the file a
// person reads; losing them there loses them everywhere.
func TestAnsibleVarsKeepsTheGeneratedComments(t *testing.T) {
	body := generated(t)

	var comments []string
	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") {
			comments = append(comments, trimmed)
		}
	}
	if len(comments) == 0 {
		t.Fatal("the generation carries no comments, so this test cannot prove they survive — " +
			"either the generator stopped explaining itself or this test stopped watching")
	}

	block, err := AnsibleVars(body, nil)
	if err != nil {
		t.Fatalf("AnsibleVars: %v", err)
	}

	for _, c := range comments {
		if !strings.Contains(block, c) {
			t.Errorf("the emitted block dropped a generated comment: %q", c)
		}
	}
}

// A NON-MAPPING IS REFUSED HERE RATHER THAN ON THE TARGET.
//
// Indentation is string handling and would happily nest a list or a scalar,
// producing an inventory whose billet_config is not a config at all. The role
// renders it anyway and the host fails to load it one converge later, with
// nothing pointing back at this step.
func TestAnsibleVarsRefusesWhatIsNotAMapping(t *testing.T) {
	for name, body := range map[string]string{
		"a list":   "- server\n- node\n",
		"a scalar": "just a string\n",
		"empty":    "",
		"comments": "# nothing but a comment\n",
		"not yaml": "server: [unterminated\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AnsibleVars(body, nil); err == nil {
				t.Errorf("%s was accepted as a config", name)
			}
		})
	}
}

// THE VARIABLE NAME IS PINNED TO THE ROLE'S TEMPLATE.
//
// If the role renamed its variable, an emission under the old name would be
// valid YAML, set a variable nothing reads, and converge the host with no
// config change and no error — the failure that looks exactly like success.
// Nothing else in the toolchain compares these two strings.
func TestAnsibleVarMatchesTheRoleTemplate(t *testing.T) {
	path := filepath.Join("..", "..",
		"ansible_collections", "junioryono", "billet",
		"roles", "host", "templates", "billet.yaml.j2")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the role template: %v", err)
	}

	// The VARIABLE THE TEMPLATE ACTUALLY RENDERS, not a mention of it. The template
	// is one expression — `{{ <var> | to_nice_yaml(...) }}` — so the piped name is
	// the whole contract, and a stale reference in a comment beside a renamed
	// variable satisfies a substring check while emitting into nothing.
	body := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(body, "{{") || !strings.HasSuffix(body, "}}") {
		t.Fatalf("the role template is not the single expression this pins:\n%s", body)
	}

	expr := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(body, "{{"), "}}"))
	rendered, _, _ := strings.Cut(expr, "|")

	if got := strings.TrimSpace(rendered); got != AnsibleVar {
		t.Errorf("the role template renders %q, but emissions are written under %q — "+
			"an emission would set a variable nothing reads", got, AnsibleVar)
	}
}
