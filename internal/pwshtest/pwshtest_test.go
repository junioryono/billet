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

// ISOLATE IS WHAT THE TESTS GET: after it, pwsh found by name on PATH is the
// wrapper, each start by that name has a cache of its own, and the cleanup
// removes them. A stand-in pwsh is the only one on PATH, so no real pwsh on this
// machine can answer for it.
//
// SERIAL: it sets PATH for the process, and it writes an executable and runs it.
func TestIsolateWrapsThePwshFoundOnPath(t *testing.T) {
	stubs := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s\\n' \"$XDG_CACHE_HOME\"\n"
	if err := os.WriteFile(filepath.Join(stubs, "pwsh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", stubs+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	cleanup, err := Isolate()
	if err != nil {
		t.Fatal(err)
	}

	found, err := exec.LookPath("pwsh")
	if err != nil {
		t.Fatal(err)
	}

	if filepath.Dir(found) == stubs {
		t.Fatalf("pwsh on PATH is still the stand-in %s, not the wrapper", found)
	}

	start := func() string {
		t.Helper()

		out, err := exec.CommandContext(t.Context(), "pwsh").CombinedOutput()
		if err != nil {
			t.Fatalf("pwsh by name failed: %v\n%s", err, out)
		}

		return strings.TrimSpace(string(out))
	}

	first, second := start(), start()
	if first == "" || first == second {
		t.Fatalf("two starts by name had the caches %q and %q, want two of their own", first, second)
	}

	for _, cache := range []string{first, second} {
		if info, err := os.Stat(cache); err != nil || !info.IsDir() {
			t.Errorf("the cache %q is not a directory: %v", cache, err)
		}
	}

	cleanup()

	for _, cache := range []string{first, second, filepath.Dir(found)} {
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Errorf("%s outlived the cleanup: %v", cache, err)
		}
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
