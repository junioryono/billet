package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// THE PROJECTION ANSWERS EVERY READER THE SAME WAY THE WHOLE DECLARATION DOES.
//
// The EC2 backend ships only the sections the installers read, because the whole
// file costs five times as much of a 16384-byte user-data budget. The guest build
// ships the file entire. That is two different inputs to
// one set of readers, which is the two-representations problem this repo keeps
// finding -- so the readers are RUN against both and required to agree.
//
// ASKED OF THE SHELL, NOT OF GO. `toolset_versions` is the function the installers
// call, and a Go reimplementation of it here would prove that two pieces of Go
// agree while the shell read something else entirely.
func TestTheProjectionAnswersEveryReaderTheSameWay(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	projected, err := runnerimages.InstallerToolset()
	if err != nil {
		t.Fatalf("InstallerToolset: %v", err)
	}

	dir := t.TempDir()

	full := filepath.Join(dir, "full.json")
	if err := os.WriteFile(full, []byte(runnerimages.ToolsetJSON()), 0o600); err != nil {
		t.Fatalf("write the full declaration: %v", err)
	}

	small := filepath.Join(dir, "projected.json")
	if err := os.WriteFile(small, projected, 0o600); err != nil {
		t.Fatalf("write the projection: %v", err)
	}

	// EVERY TOOL THE DECLARATION NAMES, not only the ones billet installs today.
	// A projection that dropped an entry billet does not yet use would still be
	// wrong, and would be found the day an installer for it is written rather
	// than now.
	if len(ts.Toolcache) == 0 {
		t.Fatal("the declaration names no toolcache tools, so this proves nothing")
	}

	for _, e := range ts.Toolcache {
		a := readVersions(t, full, e.Name)
		b := readVersions(t, small, e.Name)

		if a != b {
			t.Errorf("toolset_versions %s differs: full %q, projection %q", e.Name, a, b)
		}

		if strings.TrimSpace(a) == "" {
			t.Errorf("toolset_versions %s is empty against the FULL declaration, so "+
				"comparing it against the projection proves nothing", e.Name)
		}
	}

	// AND THE JAVA READS, which come from a different key and a different jq
	// expression -- `.java.versions[]` and `.java.default` rather than a select
	// over `.toolcache`.
	for _, expr := range []string{".java.versions[]", ".java.default"} {
		a := jqOf(t, full, expr)
		b := jqOf(t, small, expr)

		if a != b {
			t.Errorf("%s differs: full %q, projection %q", expr, a, b)
		}

		if strings.TrimSpace(a) == "" {
			t.Errorf("%s is empty against the full declaration", expr)
		}
	}

	// AND RUBY'S platform_version, which is the one field read from a toolcache
	// entry that is not its version list.
	const ruby = `.toolcache[] | select(.name == "Ruby") | .platform_version`

	if a, b := jqOf(t, full, ruby), jqOf(t, small, ruby); a != b || strings.TrimSpace(a) == "" {
		t.Errorf("Ruby's platform_version differs or is empty: full %q, projection %q", a, b)
	}
}

// THE PROJECTION CARRIES EXACTLY THE SECTIONS THE SHELL REFERENCES.
//
// DERIVED FROM THE SHELL, NOT FROM THE GO LIST. Comparing the projection against
// installerKeys would be Go agreeing with Go: the list and the projection are the
// same statement, so the test could only fail if one contradicted itself. What
// matters is whether the list matches the FILE -- and it did not. Shipping
// toolcache and java while the installers had grown to read seven more sections
// failed a real build four minutes in, immediately after the last toolcache entry.
//
// The scan is deliberately crude: any top-level key of the declaration that
// appears as `.key` anywhere in the installers is treated as read. A false
// positive costs a few hundred bytes of a budget with thousands spare; a false
// negative is a builder refusing a declaration it was handed.
func TestTheProjectionCarriesExactlyWhatTheShellReferences(t *testing.T) {
	t.Parallel()

	projected, err := runnerimages.InstallerToolset()
	if err != nil {
		t.Fatalf("InstallerToolset: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(projected, &got); err != nil {
		t.Fatalf("the projection is not valid json: %v", err)
	}

	var full map[string]json.RawMessage
	if err := json.Unmarshal([]byte(runnerimages.ToolsetJSON()), &full); err != nil {
		t.Fatalf("the declaration is not valid json: %v", err)
	}

	installers := readScriptFile(t, toolcacheAssetPath)

	for key := range full {
		// `.apt` AND THE COMPILER SECTIONS ARE READ ON THE GO SIDE and interpolated
		// as a command, so the shell never names them and they do not belong here.
		referenced := strings.Contains(installers, "."+key+"[]") ||
			strings.Contains(installers, "."+key+".") ||
			strings.Contains(installers, "'."+key)

		_, carried := got[key]

		switch {
		case referenced && !carried:
			t.Errorf("install-toolcache.sh reads .%s and the projection does not carry it; "+
				"the builder gets an empty answer and refuses the declaration", key)
		case carried && !referenced:
			t.Errorf("the projection carries %q and no installer references it; every key "+
				"is spent out of a 16384-byte user-data budget", key)
		}
	}

	// AND IT IS STILL SMALLER THAN THE THING IT REPLACES, which is why it exists.
	if len(projected) >= len(runnerimages.ToolsetJSON()) {
		t.Errorf("the projection is %d bytes against the full declaration's %d",
			len(projected), len(runnerimages.ToolsetJSON()))
	}
}

// readVersions runs the installers' own toolset_versions against a declaration.
func readVersions(t *testing.T, toolset, tool string) string {
	t.Helper()

	script := "#!/usr/bin/env bash\nset -euo pipefail\nBILLET_TC_TOOLSET=\"$1\"\n" +
		guestImageFunction(t, "toolset_versions") + "\n" +
		"toolset_versions \"$2\"\n"

	return runShell(t, script, toolset, tool)
}

func jqOf(t *testing.T, toolset, expr string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "jq", "-r", expr, toolset).CombinedOutput()
	if err != nil {
		t.Fatalf("jq %s: %v\n%s", expr, err, out)
	}

	return string(out)
}

func runShell(t *testing.T, body string, args ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "bash",
		append([]string{path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("running the harness: %v\n%s", err, out)
	}

	return string(out)
}
