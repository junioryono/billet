package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE COMPATIBILITY PAGE NAMES THE JOB THAT MEASURES EACH ROW, and a renamed job
// would leave it pointing at nothing while still reading as proof. Every
// backticked name in the page's "Measured by" column that looks like a workflow
// job has to be a job in the conformance workflow.
func TestTheCompatibilityPageNamesConformanceJobsThatExist(t *testing.T) {
	t.Parallel()

	page, err := os.ReadFile(filepath.Join("..", "docs", "operating", "compatibility.md"))
	if err != nil {
		t.Fatalf("read the compatibility page: %v", err)
	}

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cache-conformance.yml"))
	if err != nil {
		t.Fatalf("read the conformance workflow: %v", err)
	}
	var workflow struct {
		Jobs map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("parse the conformance workflow: %v", err)
	}

	jobName := regexp.MustCompile("`([a-z][a-z0-9-]+)`")
	named := 0
	for line := range strings.SplitSeq(string(page), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 || strings.TrimSpace(cells[1]) == "Feature" || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") {
			continue
		}
		for _, m := range jobName.FindAllStringSubmatch(cells[3], -1) {
			// Names with a dot or slash are files, not jobs; the column mixes both.
			if strings.ContainsAny(m[1], "./") {
				continue
			}
			named++
			if _, ok := workflow.Jobs[m[1]]; !ok {
				t.Errorf("the compatibility page points at conformance job %q, which the workflow does not define", m[1])
			}
		}
	}
	if named == 0 {
		t.Fatal("the compatibility page names no conformance job; the table has moved or the parser is wrong")
	}
}
