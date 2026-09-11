package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// THE ROLE'S OUTER BOUNDS MIRROR THE COMMANDS' PHASES. The host role wraps
// every endpoint command in `timeout`, and a bound that expires inside a
// phase the command was allowed loses the answer after the node was stopped
// or started; so the role's vars carry the counts and bounds the commands
// are built from, and this test holds each to the Go constant it mirrors,
// because a constant changed here alone would loosen or tighten the role's
// bound silently.
func TestTheRoleMirrorsTheEndpointCommandsPhaseBudgets(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host",
		"vars", "main.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var vars map[string]any
	if err := yaml.Unmarshal(body, &vars); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{
		"billet_migration_judgement_attempts": migrateJudgementAttempts,
		"billet_migration_bracket_attempts":   bracketAttempts,
		"billet_migration_observation_bound":  int(lifeops.DefaultTimeout.Seconds()),
		"billet_migration_start_bound":        int((deploy.UnitStartTimeout + lifecycleDeadlineMargin).Seconds()),
	}

	for name, value := range want {
		got, ok := vars[name].(int)
		if !ok {
			t.Errorf("%s: the role's vars/main.yml carries %v (%T), want the integer %d", name, vars[name], vars[name], value)

			continue
		}

		if got != value {
			t.Errorf("%s: the role says %d and the command is built from %d", name, got, value)
		}
	}
}

// THE HOLDER GRAMMAR HAS ONE VECTOR TABLE, written by this test from
// checkHolder's own verdicts and read by the collection's Python twin
// (plugins/filter/holder.py, tests/holder_check.py), so the role's reading of
// a receipt's `run` refuses exactly what the command refuses: bytes rather
// than characters, every Unicode space and control, the replacement
// character, a slash.
func TestTheHolderVectorTableIsCheckHoldersOwn(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"h1", "ci-run-12", "ci-${USER}", "na\u00efve", "a.b_c-d:e@f", "\u65e5\u672c\u8a9e",
		strings.Repeat("a", 200), strings.Repeat("a", 201),
		strings.Repeat("\u00e9", 100), strings.Repeat("\u00e9", 101),
		"", "a b", "a\tb", "a\nb", "a\rb", "a\vb", "a\fb", "a/b", "/",
		// The Latin-1 spaces Go's IsSpace names, then the White_Space code
		// points outside it, then the controls (C0, DEL, C1), the replacement
		// character, and the format characters that are neither space nor
		// control (a zero-width space is a name's character to both).
		"a\u0085b", "a\u00a0b", "a\u1680b", "a\u2000b", "a\u200ab", "a\u2028b", "a\u2029b", "a\u202fb", "a\u205fb", "a\u3000b",
		"a\x00b", "a\x1fb", "a\x7fb", "a\u0080b", "a\u009fb", "a\ufffdb",
		"a\u200bb", "a\u200db", "a\u2060b", "a\ufeffb",
	}

	type row struct {
		Holder string `json:"holder"`
		Valid  bool   `json:"valid"`
	}

	rows := make([]row, 0, len(inputs))
	for _, in := range inputs {
		rows = append(rows, row{Holder: in, Valid: checkHolder(in) == nil})
	}

	out, err := json.MarshalIndent(map[string]any{"vectors": rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	out = append(out, '\n')
	path := filepath.Join(fixturesRoot, "holder-vectors.json")

	if updateFixtures() {
		mustOK(t, os.WriteFile(path, out, 0o644))

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", err)
	}

	if !bytes.Equal(want, out) {
		t.Errorf("the holder vector table differs from checkHolder's verdicts:\n--- fixture\n%s\n--- checkHolder\n%s", want, out)
	}
}
