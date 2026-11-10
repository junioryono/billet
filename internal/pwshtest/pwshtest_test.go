package pwshtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/importcheck"
)

func TestNothingOutsideATestImportsThisPackage(t *testing.T) {
	t.Parallel()

	if importers := importcheck.ProductionImporters(t, "github.com/junioryono/billet/internal/pwshtest"); len(importers) > 0 {
		t.Fatalf("the PowerShell test wrapper is imported by production code: %v", importers)
	}
}

// EACH START HAS A CACHE OF ITS OWN, and the real pwsh gets every argument as it
// was given, in a directory whose name a shell would split or end a quote at.
//
// SERIAL: it writes an executable and then runs it, and a fork from a test
// running beside it could hold the file open.
func TestEachStartHasACacheOfItsOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "real pwsh")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$XDG_CACHE_HOME\"\nfor a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\n"
	if err := os.WriteFile(target, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	bin, err := wrap(dir, target)
	if err != nil {
		t.Fatal(err)
	}

	run := func() (string, []string) {
		t.Helper()

		out, err := exec.CommandContext(t.Context(), filepath.Join(bin, "pwsh"),
			"-Command", "Write-Output 'a b'", "$x").CombinedOutput()
		if err != nil {
			t.Fatalf("the wrapper failed: %v\n%s", err, out)
		}

		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")

		return lines[0], lines[1:]
	}

	first, args := run()
	second, _ := run()

	if want := []string{"[-Command]", "[Write-Output 'a b']", "[$x]"}; strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("the real pwsh was given %q, want %q", args, want)
	}

	if first == second {
		t.Errorf("two starts shared the cache %s", first)
	}

	for _, cache := range []string{first, second} {
		if filepath.Dir(cache) != dir {
			t.Errorf("the cache %q is not under %q", cache, dir)
		}

		if info, err := os.Stat(cache); err != nil || !info.IsDir() {
			t.Errorf("the cache %q is not a directory: %v", cache, err)
		}
	}
}
