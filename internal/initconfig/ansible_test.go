package initconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// THE EMITTED VARIABLE MUST REACH THE ROLE'S TEMPLATE.
//
// If the role renamed what it reads, an emission under the old name would be
// valid YAML, set a variable nothing reads, and converge the host with no
// config change and no error — the failure that looks exactly like success.
// Nothing else in the toolchain compares these strings.
//
// The template no longer reads the emitted variable directly. It renders an
// OPERAND its caller binds, so that an ordinary render, a collected rendering
// and a retirement's own bytes cannot be confused for one another, and an
// unbound render fails instead of quietly falling back to inventory. That makes
// the chain two links, and this test pins both: the emission is what the role
// derives its effective configuration from, and every caller binds the operand.
func TestAnsibleVarReachesTheRoleTemplateThroughItsOperand(t *testing.T) {
	role := filepath.Join("..", "..",
		"ansible_collections", "junioryono", "billet", "roles", "host")

	raw, err := os.ReadFile(filepath.Join(role, "templates", "billet.yaml.j2"))
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
	operand := strings.TrimSpace(rendered)

	if operand == "" {
		t.Fatalf("the role template renders no variable:\n%s", body)
	}

	// A SUBSTRING IS NOT A READ. `billet_config` occurs inside
	// `billet_config_document`, so a rename to any longer name satisfies a Contains
	// check while reading nothing. `\b` is what makes this a read rather than an
	// occurrence: Go counts `_` as a word character, so the boundary does not fall
	// between `billet_config` and `_document` and the longer name cannot satisfy it.
	reads := regexp.MustCompile(`\b` + regexp.QuoteMeta(AnsibleVar) + `\b`)

	// A BINDING IS THE OPERAND FOLLOWED BY A COLON, in either spelling the role
	// uses: a task's `vars:` entry writes `operand: value`, and a template lookup
	// writes `template_vars={'operand': value}` with a quote in between.
	bound := regexp.MustCompile(regexp.QuoteMeta(operand) + `['"]?\s*:`)

	// LINK ONE: the emission is what the ordinary pass derives its configuration
	// from. Without this, the role could read some other inventory variable and an
	// emission would again set something nothing reads.
	effective, err := os.ReadFile(filepath.Join(role, "tasks", "effective-config.yml"))
	if err != nil {
		t.Fatalf("read the role's effective configuration entry: %v", err)
	}
	if !reads.Match(effective) {
		t.Errorf("the role derives its effective configuration without reading %q — "+
			"an emission would set a variable nothing reads", AnsibleVar)
	}

	// LINK TWO: every INVOCATION binds the operand, counted per invocation rather
	// than per file: two renders in one file and one binding between them would
	// satisfy a whole-file check while the unbound one falls back to inventory.
	tasks, err := filepath.Glob(filepath.Join(role, "tasks", "*.yml"))
	if err != nil {
		t.Fatalf("list the role's tasks: %v", err)
	}
	invocations := 0
	for _, task := range tasks {
		source, err := os.ReadFile(task)
		if err != nil {
			t.Fatalf("read %s: %v", task, err)
		}
		uses := strings.Count(string(source), "billet.yaml.j2")
		if uses == 0 {
			continue
		}
		invocations += uses
		if bindings := len(bound.FindAllString(string(source), -1)); bindings < uses {
			t.Errorf("%s renders billet.yaml.j2 %d times but binds %q %d times",
				filepath.Base(task), uses, operand, bindings)
		}
	}
	if invocations == 0 {
		t.Errorf("no task renders billet.yaml.j2, so nothing pins the operand %q", operand)
	}
}
