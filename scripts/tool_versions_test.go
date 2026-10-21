package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// toolVersions reads .github/tool-versions.env, the one place billet pins its
// tools, refusing a line that is neither a comment nor KEY=value.
func toolVersions(t *testing.T) map[string]string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", ".github", "tool-versions.env"))
	if err != nil {
		t.Fatalf("read the pinned tool versions: %v", err)
	}

	pin := regexp.MustCompile(`^([A-Z_]+)=(v\d+\.\d+\.\d+)$`)
	versions := map[string]string{}

	for line := range strings.SplitSeq(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		m := pin.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("tool-versions.env line %q is neither a comment nor KEY=vX.Y.Z", line)
		}

		versions[m[1]] = m[2]
	}

	for _, key := range []string{"GOLANGCI_VERSION", "GORELEASER_VERSION", "SQLC_VERSION", "TFLINT_VERSION", "TRIVY_VERSION"} {
		if versions[key] == "" {
			t.Fatalf("tool-versions.env pins no %s", key)
		}
	}

	return versions
}

// EVERY OTHER COPY OF A PIN IS THE PIN. A version written in two places drifts
// the first time one is bumped, and the gate in CI and the same gate run by hand
// then use two different tools. The copies that cannot read the file are held
// equal to it here: release.yml, which builds release branches whose tree may
// predate the file, the .golangci.yml and sqlc.yaml comments that tell a person
// what to install, and the skill's measured-facts line. Everything else reads the file.
func TestEveryCopyOfAToolVersionIsThePin(t *testing.T) {
	t.Parallel()

	v := toolVersions(t)

	for _, tc := range []struct {
		path string
		want []string
	}{
		{".github/workflows/release.yml", []string{`version: "` + v["GORELEASER_VERSION"] + `"`}},
		{".golangci.yml", []string{
			"# PINNED VERSION: " + v["GOLANGCI_VERSION"] + ",",
			"golangci-lint@" + v["GOLANGCI_VERSION"] + "\n",
		}},
		{"sqlc.yaml", []string{"# PINNED at " + v["SQLC_VERSION"] + " "}},
		{".claude/skills/billet-checks-and-lint/references/measured-facts.md", []string{
			"golangci-lint " + v["GOLANGCI_VERSION"] + ", goreleaser " + v["GORELEASER_VERSION"] +
				", sqlc " + v["SQLC_VERSION"] + ", tflint " + v["TFLINT_VERSION"] + ", trivy " + v["TRIVY_VERSION"],
		}},
	} {
		body, err := os.ReadFile(filepath.Join("..", tc.path))
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}

		for _, want := range tc.want {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s does not say %q; a pin bumped in .github/tool-versions.env is bumped here too", tc.path, want)
			}
		}
	}
}

// CI READS THE PINS FROM THE FILE: every job that installs a pinned tool loads
// the file first, names the version by its variable, and spells none of the
// pinned versions itself.
func TestCIInstallsEveryPinnedToolFromThePinFile(t *testing.T) {
	t.Parallel()

	v := toolVersions(t)

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}

	ci := string(body)

	for key, version := range v {
		if strings.Contains(ci, version) {
			t.Errorf("ci.yml spells %s's version %s itself rather than reading $%s", key, version, key)
		}
	}

	for _, use := range []string{
		"version: ${{ env.GOLANGCI_VERSION }}",
		`sqlc/cmd/sqlc@$SQLC_VERSION"`,
		`goreleaser/v2@$GORELEASER_VERSION"`,
		"tflint_version: ${{ env.TFLINT_VERSION }}",
		"version: ${{ env.TRIVY_VERSION }}",
	} {
		if !strings.Contains(ci, use) {
			t.Errorf("ci.yml does not install a pinned tool as %q", use)
		}
	}

	// LOADED BEFORE IT IS USED, IN EVERY JOB THAT USES IT, whether a step's run,
	// its with or its env names it: $GITHUB_ENV reaches only the steps after the
	// one that writes it, so a loader after its first use leaves that use an
	// empty version. The loader is unconditional, and a job
	// that names no pinned version needs none.
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				If   string         `yaml:"if"`
				Run  string         `yaml:"run"`
				With map[string]any `yaml:"with"`
				Env  map[string]any `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("ci.yml is not valid yaml: %v", err)
	}

	load := `grep -E '^[A-Z_]+=' .github/tool-versions.env >> "$GITHUB_ENV"`
	jobsUsing := 0

	for name, job := range workflow.Jobs {
		loaded := -1

		for i, step := range job.Steps {
			if loaded < 0 && step.If == "" && strings.TrimSpace(step.Run) == load {
				loaded = i
			}

			text := step.Run
			for _, value := range step.With {
				text += " " + fmt.Sprint(value)
			}

			for _, value := range step.Env {
				text += " " + fmt.Sprint(value)
			}

			for key := range v {
				// $KEY, ${KEY} and ${{ env.KEY }} alike, the name whole.
				if !regexp.MustCompile(`(\$\{?|env\.)` + key + `\b`).MatchString(text) {
					continue
				}

				if loaded < 0 {
					t.Errorf("job %s step %d names %s before any unconditional step loads .github/tool-versions.env",
						name, i, key)
				}
			}
		}

		if loaded >= 0 {
			jobsUsing++
		}
	}

	if jobsUsing < 4 {
		t.Errorf("%d jobs load the pin file, want the lint, release-config, package-lifecycle and terraform-lint jobs",
			jobsUsing)
	}
}
