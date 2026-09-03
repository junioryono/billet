package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE COVERAGE CHECK, RUN — not read.
//
// Everything else in check-guest-image.sh needs root and a mounted image, which
// is why this section had no test and why both of its expensive failures shipped:
// a loop that ran zero times and reported success, and a declared `*` that
// glob-expanded against the working directory. Neither is visible to a reader;
// both are obvious the moment the loop executes.
//
// THE DECOY FILES ARE THE POINT of the second case. CodeQL's declared version is
// literally `*`, so an unquoted `for glob in $declared_globs` iterates the
// DIRECTORY the gate was started from. Measured from the repo root, a three-entry
// declaration iterated 21 times and the gate failed once per filename, so no
// image could ever be published.
func TestTheCoverageCheckIteratesTheDeclarationAndNotTheDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// toolcache is <tool>/<version> entries to create.
		toolcache []string
		// unpublished is what the build's record says.
		unpublished []string
		wantPass    bool
		wantSaid    string
	}{
		{
			name: "every declared line has an entry",
			toolcache: []string{
				"node/22.20.0", "node/24.20.0",
				"go/1.24.0", "go/1.25.0", "go/1.26.0",
				"Python/3.10.1", "Python/3.11.1", "Python/3.12.1",
				"Python/3.13.1", "Python/3.14.1",
				"PyPy/3.9.1", "PyPy/3.10.1", "PyPy/3.11.1",
				"Ruby/3.2.1", "Ruby/3.3.1", "Ruby/3.4.1",
				"CodeQL/2.26.4",
			},
			unpublished: []string{"Ruby 4.0.*"},
			wantPass:    true,
		},
		{
			// THE ONE THE OLD PREFIX ACCEPTED. PyPy declares a bare `3.9`, and a
			// prefix match on `3.9` is satisfied by `3.90.1` -- a different line,
			// answering a promise about 3.9 with an entry for something else.
			name: "a neighbouring line does not satisfy a bare minor",
			toolcache: []string{
				"node/22.20.0", "node/24.20.0",
				"go/1.24.0", "go/1.25.0", "go/1.26.0",
				"Python/3.10.1", "Python/3.11.1", "Python/3.12.1",
				"Python/3.13.1", "Python/3.14.1",
				"PyPy/3.90.1", "PyPy/3.10.1", "PyPy/3.11.1",
				"Ruby/3.2.1", "Ruby/3.3.1", "Ruby/3.4.1",
				"CodeQL/2.26.4",
			},
			unpublished: []string{"Ruby 4.0.*"},
			wantSaid:    "offers PyPy 3.9",
		},
		{
			// AND THE RECORD IS NOT A BLANKET EXCUSE. A line nothing installed and
			// nothing recorded is the gap the whole section exists to report.
			name: "a declared line is neither installed nor recorded",
			toolcache: []string{
				"node/22.20.0", "node/24.20.0",
				"go/1.24.0", "go/1.25.0", "go/1.26.0",
				"Python/3.10.1", "Python/3.11.1", "Python/3.12.1",
				"Python/3.13.1", "Python/3.14.1",
				"PyPy/3.9.1", "PyPy/3.10.1", "PyPy/3.11.1",
				"Ruby/3.2.1", "Ruby/3.3.1", "Ruby/3.4.1",
				"CodeQL/2.26.4",
			},
			wantSaid: "offers Ruby 4.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tcDir := filepath.Join(root, "hostedtoolcache")

			for _, e := range tc.toolcache {
				if err := os.MkdirAll(filepath.Join(tcDir, e, "x64"), 0o755); err != nil {
					t.Fatalf("make %s: %v", e, err)
				}
			}

			if err := os.WriteFile(filepath.Join(tcDir, ".billet-unpublished"),
				[]byte(strings.Join(tc.unpublished, "\n")+"\n"), 0o600); err != nil {
				t.Fatalf("write the record: %v", err)
			}

			// FILES WITH THE SHAPE OF A VERSION, in the directory the function runs
			// from. If the declaration is glob-expanded these become "declared
			// lines" and the check fails on them by name.
			decoys := filepath.Join(root, "cwd")
			if err := os.MkdirAll(decoys, 0o755); err != nil {
				t.Fatalf("make the run directory: %v", err)
			}

			for _, name := range []string{"go.mod", "Makefile", "README.md", "3.9.7"} {
				if err := os.WriteFile(filepath.Join(decoys, name), nil, 0o600); err != nil {
					t.Fatalf("write decoy: %v", err)
				}
			}

			toolset, err := filepath.Abs(toolsetPathForTest)
			if err != nil {
				t.Fatalf("resolve the toolset: %v", err)
			}

			script := filepath.Join(root, "exercise.sh")
			body := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"pass() { printf 'PASS %s\\n' \"$*\"; }\n" +
				"fail() { printf 'FAIL %s\\n' \"$*\"; exit 7; }\n" +
				// THE GATE'S OWN LIST. Restating it here would make the test agree
				// with itself rather than with the gate, and the first version of
				// this harness simply omitted it -- which under `set -u` is not an
				// error but an empty loop, so the check passed having checked
				// nothing.
				gateAssignment(t, "TOOLCACHE_TOOLS") + "\n" +
				gateFunction(t, "check_toolcache_coverage") + "\n" +
				"check_toolcache_coverage\n"

			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatalf("write exercise: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Dir = decoys
			cmd.Env = append(os.Environ(),
				"TOOLCACHE="+tcDir,
				"TOOLSET_FILE="+toolset,
			)

			out, runErr := cmd.CombinedOutput()

			if tc.wantPass {
				if runErr != nil {
					t.Fatalf("the coverage check refused a complete toolcache: %v\n%s", runErr, out)
				}

				// EVERY DECLARED LINE REPORTED, and no more. A glob expansion adds
				// lines named after files, and an empty declaration removes them
				// all -- a count is what distinguishes both from a correct run.
				if n := strings.Count(string(out), "PASS "); n != 18 {
					t.Fatalf("the check reported %d lines, want 18 (the declaration's own "+
						"count). More means it iterated the directory; fewer means it "+
						"iterated nothing and said so quietly.\n%s", n, out)
				}

				return
			}

			if runErr == nil {
				t.Fatalf("the coverage check accepted a toolcache with a gap\n%s", out)
			}

			if !strings.Contains(string(out), tc.wantSaid) {
				t.Fatalf("output = %q, want it to name %q", out, tc.wantSaid)
			}
		})
	}
}

// gateFunction returns one shell function from check-guest-image.sh.
//
// A SEPARATE EXTRACTOR FROM guestImageFunction, which searches the two BUILD
// files and refuses a function defined in both. The gate is a third file with no
// such pairing, and pointing the build extractor at it would make "defined in
// two places" -- the failure that one exists to catch -- unrepresentable.
func gateFunction(t *testing.T, name string) string {
	t.Helper()

	source := readScriptFile(t, "check-guest-image.sh")

	start := strings.Index(source, name+"() {")
	if start < 0 {
		t.Fatalf("check-guest-image.sh has no %s function", name)
	}

	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}

	return source[start : start+end+2]
}

// gateAssignment returns one top-level assignment from check-guest-image.sh.
func gateAssignment(t *testing.T, name string) string {
	t.Helper()

	for _, line := range strings.Split(readScriptFile(t, "check-guest-image.sh"), "\n") {
		if strings.HasPrefix(line, name+"=") {
			return line
		}
	}

	t.Fatalf("check-guest-image.sh has no top-level %s assignment", name)

	return ""
}
