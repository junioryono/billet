package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/initconfig"
)

// hybridArgsMinting is hybridArgs without --generation, so the plan render
// resolves one itself: from the root in --out, or a fresh one.
func hybridArgsMinting(out, cfg string) []string {
	args := hybridArgs(out, cfg)
	i := slices.Index(args, "--generation")

	return slices.Delete(args, i, i+2)
}

// rootGeneration is the fingerprint the root in out declares.
func rootGeneration(t *testing.T, out string) string {
	t.Helper()

	g, found, err := initconfig.HybridGenerationOf(readHybrid(t, out, initconfig.HybridTerraformFile))
	if err != nil || !found {
		t.Fatalf("the root in %s declares no generation (found %v, %v)", out, found, err)
	}

	return g
}

// writeOutputs writes the outputs fixture with its generation replaced.
func writeOutputs(t *testing.T, dir, generation string) string {
	t.Helper()

	path := filepath.Join(dir, "outputs.json")
	body := strings.Replace(hybridOutputsJSON, `"value": "`+testGeneration+`"`, `"value": "`+generation+`"`, 1)
	if generation != testGeneration && body == hybridOutputsJSON {
		t.Fatal("the outputs fixture no longer carries the generation this case rewrites")
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

// THE GENERATION SURVIVES A RE-RUN OF THE PLAN RENDER AND --force (#62). The
// first plan render mints it; every later one into the same --out reads it back
// from the root, so the outputs of the one apply keep matching. A fresh mint on
// the second run would refuse the apply's outputs at the prepare render.
//
// Mutation: dropping the read of the existing root makes the second render
// mint another fingerprint, and this fails.
func TestInitHybridKeepsItsGenerationAcrossReRuns(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("first plan render: %v", err)
	}
	minted := rootGeneration(t, out)
	if minted == testGeneration {
		t.Fatalf("the plan render took the fixture's generation %s without being given it", minted)
	}

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("second plan render: %v", err)
	}
	if got := rootGeneration(t, out); got != minted {
		t.Errorf("a re-run of the plan render moved the generation from %s to %s", minted, got)
	}

	if _, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--force")...); err != nil {
		t.Fatalf("forced plan render: %v", err)
	}
	if got := rootGeneration(t, out); got != minted {
		t.Errorf("--force moved the generation from %s to %s", minted, got)
	}

	// The runbook repeats it, because the prepare render on another machine
	// has only the command it prints.
	if runbook := readHybrid(t, out, HybridRunbookFile); !strings.Contains(runbook, "--generation "+minted) {
		t.Errorf("the runbook's re-render commands do not carry --generation %s", minted)
	}

	// And a prepare render with no flag at all reads it from the root and
	// accepts the outputs of that root.
	outputs := writeOutputs(t, dir, minted)
	if _, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--terraform-output", outputs)...); err != nil {
		t.Errorf("a prepare render over the root that minted %s refused its outputs: %v", minted, err)
	}
}

// AN OPERATOR'S OWN ROOT STILL NAMES THE GENERATION: a root that is not
// billet's own is left alone and the fresh render goes beside it, and the
// fingerprint it declares is the one its outputs carry.
func TestInitHybridReadsTheGenerationOfAForeignRoot(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}
	minted := rootGeneration(t, out)

	root := filepath.Join(out, initconfig.HybridTerraformFile)
	raw, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	edited := "# my own root now\n" + string(raw)
	if err := os.WriteFile(root, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runInitHybrid(t, hybridArgsMinting(out, cfg)...); err != nil {
		t.Fatalf("plan render over an edited root: %v", err)
	}

	beside, err := os.ReadFile(root + ".new")
	if err != nil {
		t.Fatalf("the fresh root did not land beside the operator's: %v", err)
	}
	g, found, err := initconfig.HybridGenerationOf(string(beside))
	if err != nil || !found || g != minted {
		t.Errorf("the render beside the operator's root carries (%q, %v, %v), want %s", g, found, err, minted)
	}
}

// TWO GENERATIONS IN ONE DIRECTORY ARE REFUSED, and a later render that cannot
// tell which generation its outputs must belong to is refused too, rather than
// minting one that no apply ever wrote.
func TestInitHybridRefusesAnUnboundGeneration(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := writeOutputs(t, dir, testGeneration)

	_, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--terraform-output", outputs)...)
	if err == nil || !strings.Contains(err.Error(), "--generation") {
		t.Fatalf("a prepare render with no generation and no root was not refused by the flag: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridTerraformFile)); !os.IsNotExist(statErr) {
		t.Errorf("the refused render wrote a root (stat: %v)", statErr)
	}

	if _, err := runInitHybrid(t, hybridArgs(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}

	other := "fedc-ba98-7654-3210"
	args := append(hybridArgsMinting(out, cfg), "--generation", other)
	_, err = runInitHybrid(t, args...)
	if err == nil {
		t.Fatal("a second generation was rendered into a directory whose root declares another")
	}
	for _, want := range []string{other, testGeneration} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if got := rootGeneration(t, out); got != testGeneration {
		t.Errorf("the refused render moved the root's generation to %s", got)
	}

	for _, bad := range []string{"", "0123"} {
		if _, err := runInitHybrid(t, append(hybridArgsMinting(out, cfg), "--generation", bad)...); err == nil {
			t.Errorf("--generation %q was accepted", bad)
		}
	}
}

// OUTPUTS FROM ANOTHER GENERATION ARE REFUSED BEFORE A PLACEHOLDER IS FILLED,
// even when their name and region match: that is the case #62 says a name and
// a region cannot see.
//
// Mutation: dropping the generation comparison lets this render succeed.
func TestInitHybridRefusesAnotherGenerationsOutputs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	other := "fedc-ba98-7654-3210"
	outputs := writeOutputs(t, dir, other)

	_, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
	if err == nil {
		t.Fatal("outputs written by another generation were accepted")
	}
	for _, want := range []string{other, testGeneration, "--terraform-output"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridInventoryFile)); !os.IsNotExist(statErr) {
		t.Errorf("the refused render wrote an inventory (stat: %v)", statErr)
	}
}

// THE ACCOUNT IS COMPARED WHEN STS ANSWERS AND REPORTED WHEN IT CANNOT: a
// different account is refused, the same one passes in silence, and an answer
// nobody could get is said out loud and does not stop a render the generation
// already binds.
func TestInitHybridComparesTheAccountWhenItCan(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "none.yaml")
	outputs := writeOutputs(t, dir, testGeneration)

	t.Run("another account", func(t *testing.T) {
		answerAccount(t, "210987654321", nil)
		out := filepath.Join(t.TempDir(), "acme-ci")

		_, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err == nil {
			t.Fatal("outputs minted in another account were accepted")
		}
		for _, want := range []string{"210987654321", hybridTestAccount} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name account %s: %v", want, err)
			}
		}
		if _, statErr := os.Stat(filepath.Join(out, initconfig.HybridInventoryFile)); !os.IsNotExist(statErr) {
			t.Errorf("the refused render wrote an inventory (stat: %v)", statErr)
		}
	})

	t.Run("the same account", func(t *testing.T) {
		answerAccount(t, hybridTestAccount, nil)
		out := filepath.Join(t.TempDir(), "acme-ci")

		printed, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err != nil {
			t.Fatalf("outputs from this account were refused: %v", err)
		}
		if strings.Contains(printed, "NOT verified") {
			t.Errorf("a verified account was reported as unverified:\n%s", printed)
		}
	})

	t.Run("could not tell", func(t *testing.T) {
		// The suite's own default: no AWS.
		out := filepath.Join(t.TempDir(), "acme-ci")

		printed, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
		if err != nil {
			t.Fatalf("an account nobody could ask about stopped the render: %v", err)
		}
		for _, want := range []string{"NOT verified", hybridTestAccount, errNoAWSInTheSuite.Error()} {
			if !strings.Contains(printed, want) {
				t.Errorf("the report of an unverified account does not say %q:\n%s", want, printed)
			}
		}
	})
}
