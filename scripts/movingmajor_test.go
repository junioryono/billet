package scripts_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE MOVING MAJOR MOVES ONLY AFTER THE RELEASE IS ACCEPTED.
//
// `@v0` is the one billet ref that changes under a consumer, which is the whole
// point of it: a workflow written once keeps getting fixes without an edit. That
// makes WHEN it moves a supply-chain property rather than a scheduling detail.
// release.yml proves the release immutable and verifies its GitHub attestation
// before it finishes, so the major may only advance once that job has succeeded —
// a step inside the cut job would point every `@v0` consumer at a release whose
// build had not run, for as long as it took that build to fail.
//
// ASSERTED ON THE DEPENDENCY GRAPH, not on the file's text. The ordering is what
// carries the property, and a step that merely appears later in the file runs at
// whatever time its own job does.
func TestTheMovingMajorAdvancesOnlyAfterTheReleaseSucceeds(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cut-release.yml"))
	if err != nil {
		t.Fatalf("read cut-release workflow: %v", err)
	}

	var doc struct {
		Jobs map[string]struct {
			Needs       needs `yaml:"needs"`
			Permissions struct {
				Contents string `yaml:"contents"`
			} `yaml:"permissions"`
		} `yaml:"jobs"`
	}

	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse cut-release workflow: %v", err)
	}

	job, ok := doc.Jobs["advance-major"]
	if !ok {
		t.Fatalf("cut-release.yml has no advance-major job; jobs are %v", sortedKeys(doc.Jobs))
	}

	var needsRelease bool

	for _, need := range job.Needs {
		if need == "release" {
			needsRelease = true
		}
	}

	if !needsRelease {
		t.Errorf("advance-major needs %v; without `release` the moving major would point "+
			"at a tag whose build has not been proved immutable or attested", job.Needs)
	}

	// IT HAS TO BE ABLE TO PUSH A TAG, and saying so here means a permissions trim
	// that silently stops the major advancing is caught by the gate rather than by
	// a consumer still running an old release weeks later.
	if job.Permissions.Contents != "write" {
		t.Errorf("advance-major has contents: %q, want write: it force-pushes the major tag",
			job.Permissions.Contents)
	}
}

// needs is a workflow job's `needs:`, which GitHub accepts as either one job
// name or a list of them. Decoding it as a list alone fails on the scalar form —
// which this workflow already uses — so the test would report a parse failure
// about a file GitHub is perfectly happy with.
type needs []string

func (n *needs) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var one string
		if err := node.Decode(&one); err != nil {
			return err
		}

		*n = needs{one}

		return nil
	}

	var many []string
	if err := node.Decode(&many); err != nil {
		return err
	}

	*n = many

	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
