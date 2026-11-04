package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE ROUTER'S TEST SKIPS WITHOUT HAPROXY, so the line that makes that a failure is what
// makes the reference configuration tested at all, and nothing else would notice it going.
func TestTheTestJobRequiresHAProxy(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}

	text := string(body)

	_, job, found := strings.Cut(text, "\n  test:\n")
	if !found {
		t.Fatal("ci.yml has no `test` job")
	}

	if next := regexp.MustCompile(`\n  [a-zA-Z0-9_-]+:\n`).FindStringIndex(job); next != nil {
		job = job[:next[0]]
	}

	if !strings.Contains(job, "BILLET_TEST_HAPROXY: required") {
		t.Error("the test job does not set BILLET_TEST_HAPROXY=required, so the router's test would skip")
	}

	if !strings.Contains(job, "haproxy") || !regexp.MustCompile(`apt-get[^\n]*install[^\n]*haproxy|install[^\n]*\n[^\n]*haproxy`).MatchString(job) {
		t.Error("the test job does not install haproxy")
	}
}
