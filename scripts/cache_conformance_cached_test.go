package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE IMPORT LANE'S ASSERTION IS RUN HERE, not read here.
//
// It is the whole gate: the BuildKit lanes export in one VM and import in a
// second, and what says the second one got billet's cache back is a single line
// of shell reading a progress log. Read rather than executed, it fails in both
// directions and neither is visible — a bare `CACHED` is satisfied by the base
// image's own vertex while the run-unique layer rebuilt (green on a broken
// import), and an over-precise match refuses a log that says exactly what it
// should (red on a working one, which is how a check gets deleted).
//
// So the block between the markers in the workflow is extracted VERBATIM and run
// against fixtures in both directions.
func TestTheImportLaneRequiresTheRunUniqueLayerToBeCached(t *testing.T) {
	t.Parallel()

	assertion := importLaneCachedAssertion(t)

	for _, tc := range []struct {
		name string
		log  string
		pass bool
	}{
		{
			name: "the run-unique layer came back from the cache",
			log: `#1 [internal] load build definition from Dockerfile
#1 DONE 0.0s
#4 [1/2] FROM docker.io/library/alpine:3.20@sha256:abc
#4 CACHED
#5 importing cache manifest from gha:12345
#5 DONE 0.2s
#6 [2/2] RUN echo billet-loopback-77-2 >/probe
#6 CACHED
#7 exporting to cache
#7 DONE 0.0s
`,
			pass: true,
		},
		{
			// THE ONE A BARE `grep CACHED` ACCEPTS. The base image is cached
			// locally on any builder; the layer under test rebuilt, which means
			// the import brought back nothing.
			name: "only the base image was cached and the layer rebuilt",
			log: `#4 [1/2] FROM docker.io/library/alpine:3.20@sha256:abc
#4 CACHED
#6 [2/2] RUN echo billet-loopback-77-2 >/probe
#6 0.201 billet-loopback-77-2
#6 DONE 0.3s
`,
		},
		{
			// A DIFFERENT RUN'S LAYER IS NOT THIS RUN'S. Scopes are per run, so a
			// cached vertex from another one proves nothing about this import.
			name: "a cached layer from another run",
			log: `#4 [1/2] FROM docker.io/library/alpine:3.20@sha256:abc
#4 CACHED
#6 [2/2] RUN echo billet-loopback-99-9 >/probe
#6 CACHED
`,
		},
		{
			name: "no build step at all",
			log:  "#1 [internal] load build definition from Dockerfile\n#1 DONE 0.0s\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte(tc.log), 0o600); err != nil {
				t.Fatalf("write the fixture log: %v", err)
			}
			run := exec.CommandContext(t.Context(), "bash", "-c",
				"set -euo pipefail\n"+assertion)
			run.Dir = dir
			output, err := run.CombinedOutput()
			if tc.pass && err != nil {
				t.Fatalf("the assertion refused a log that says the layer was cached: %v\n%s",
					err, output)
			}
			if !tc.pass && err == nil {
				t.Fatalf("the assertion accepted a log that does not say the layer was cached:\n%s",
					tc.log)
			}
		})
	}
}

// importLaneCachedAssertion returns the workflow's own assertion, dedented, with
// the two run-identity expressions bound to the fixtures' values.
func importLaneCachedAssertion(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cache-conformance.yml"))
	if err != nil {
		t.Fatalf("read cache conformance workflow: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), "BILLET_CACHED_ASSERT_BEGIN")
	if !found {
		t.Fatal("the import lane no longer marks its cached-layer assertion")
	}
	// The marker sits inside a comment whose sentence continues on the same line.
	_, rest, found = strings.Cut(rest, "\n")
	if !found {
		t.Fatal("the cached-layer assertion's begin marker ends the file")
	}
	block, _, found := strings.Cut(rest, "BILLET_CACHED_ASSERT_END")
	if !found {
		t.Fatal("the cached-layer assertion has no end marker")
	}

	var lines []string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimRight(line, " \t"), "          ")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "#") || strings.TrimSpace(trimmed) == "" {
			continue
		}
		lines = append(lines, trimmed)
	}
	if len(lines) == 0 {
		t.Fatal("the cached-layer assertion is empty")
	}

	script := strings.Join(lines, "\n")
	for expression, value := range map[string]string{
		"${{ github.run_id }}":      "77",
		"${{ github.run_attempt }}": "2",
	} {
		if !strings.Contains(script, expression) {
			t.Fatalf("the cached-layer assertion no longer binds %s, so this fixture "+
				"cannot be the run it claims to be", expression)
		}
		script = strings.ReplaceAll(script, expression, value)
	}

	return script
}
