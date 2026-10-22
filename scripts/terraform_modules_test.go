package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// moduleFixture is a checkout holding the gate scripts' environment and the given
// files under it, so gates/terraform-modules.sh runs against a tree whose
// modules the test chose.
func moduleFixture(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for _, script := range []string{"env.sh", "terraform-modules.sh"} {
		body, err := os.ReadFile(filepath.Join("gates", script))
		if err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Join(root, "scripts", "gates"), 0o755); err != nil {
			t.Fatal(err)
		}

		writeExecutable(t, filepath.Join(root, "scripts", "gates", script), string(body))
	}

	files[".github/tool-versions.env"] = "GOLANGCI_VERSION=v0.0.0\n"

	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// EVERY TOP-LEVEL MODULE, AND ONLY A LIST A GATE COULD NOT PASS VACUOUSLY. The
// discovery prints each module once, sorted, skipping what an init downloaded;
// it refuses a tree with no module, which would let every terraform gate pass
// having examined nothing, and a module directory with no .tf file (a .tf.json
// configuration terraform accepts), which every gate would skip in silence.
func TestTheTerraformModuleListIsWholeOrRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		files  map[string]string
		code   int
		stdout string
		stderr string
	}{
		{
			name: "two modules, one with a child and a downloaded copy",
			files: map[string]string{
				"terraform/modules/b/main.tf":                         "",
				"terraform/modules/a/main.tf":                         "",
				"terraform/modules/a/modules/child/main.tf":           "",
				"terraform/modules/a/.terraform/modules/x/main.tf":    "",
				"terraform/modules/b/.terraform/providers/README.txt": "",
				"terraform/modules/README.md":                         "",
			},
			code:   0,
			stdout: "terraform/modules/a\nterraform/modules/b\n",
		},
		{
			name:   "no module",
			files:  map[string]string{"terraform/modules/README.md": ""},
			code:   1,
			stderr: "no terraform modules discovered under terraform/modules",
		},
		{
			name: "a module whose only .tf an init downloaded",
			files: map[string]string{
				"terraform/modules/a/main.tf":                      "",
				"terraform/modules/d/.terraform/modules/x/main.tf": "",
			},
			code:   1,
			stderr: "terraform module directories with no discovered .tf: terraform/modules/d",
		},
		{
			name: "a module whose configuration is JSON",
			files: map[string]string{
				"terraform/modules/a/main.tf":      "",
				"terraform/modules/c/main.tf.json": "{}",
			},
			code:   1,
			stderr: "terraform module directories with no discovered .tf: terraform/modules/c",
		},
	} {
		root := moduleFixture(t, tc.files)

		cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", "gates", "terraform-modules.sh"))

		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr

		code := 0
		if err := cmd.Run(); err != nil {
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok {
				t.Fatalf("%s: %v", tc.name, err)
			}

			code = exit.ExitCode()
		}

		if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit %d, stdout %q and stderr holding %q",
				tc.name, code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
		}
	}
}
