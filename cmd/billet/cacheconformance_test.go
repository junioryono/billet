package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testCacheConformanceCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCacheConformanceTag    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCacheConformanceRendererMakesTheConsumerOwnEveryJob(t *testing.T) {
	t.Parallel()

	source := readCanonicalCacheConformance(t)
	options := testCacheConformanceOptions(t.TempDir())

	first, err := renderCacheConformance(source, options)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := renderCacheConformance(source, options)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the same source and inputs produced two different workflows")
	}

	workflow := string(first)
	for _, forbidden := range []string{"workflow_call:", "${{ inputs.", "uses: junioryono/billet/.github/workflows/cache-conformance.yml"} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("generated workflow still contains %q", forbidden)
		}
	}
	for _, required := range []string{
		"runs-on: \"billet-4vcpu-cache\"",
		"EXPECTED_RUNNER_VERSION: \"2.336.0\"",
		"EXPECTED_GUEST_CONTRACT: \"9\"",
		"repository: \"junioryono/billet\"",
		"ref: \"" + testCacheConformanceCommit + "\"",
		"Keep the jobs directly in this repository",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("generated workflow is missing %q", required)
		}
	}
	if err := validateRenderedCacheConformance(first, options.runnerLabel); err != nil {
		t.Fatalf("generated workflow failed its own ownership gate: %v", err)
	}
}

func TestCacheConformanceRendererRefusesAWeakenedCanonicalWorkflow(t *testing.T) {
	t.Parallel()

	source := string(readCanonicalCacheConformance(t))
	options := testCacheConformanceOptions(t.TempDir())
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "workflow call trigger disappeared",
			mutate: func(workflow string) string {
				return strings.Replace(workflow, "  workflow_call:\n", "  removed_workflow_call:\n", 1)
			},
			wantErr: "both supported triggers",
		},
		{
			name: "immutable fixture revision became detached from the input",
			mutate: func(workflow string) string {
				return strings.ReplaceAll(workflow, "${{ inputs.billet_ref }}", "v0.0.0")
			},
			wantErr: "inputs.billet_ref",
		},
		{
			name: "a job delegates outside the selected consumer workflow",
			mutate: func(workflow string) string {
				return strings.Replace(workflow, "    steps:\n",
					"    uses: outside/example/.github/workflows/job.yml@main\n    steps:\n", 1)
			},
			wantErr: "delegates to a reusable workflow",
		},
		{
			name: "one job escapes the trusted scale set",
			mutate: func(workflow string) string {
				return strings.Replace(workflow, "    runs-on: ${{ inputs.runner_label }}\n",
					"    runs-on: another-runner\n", 1)
			},
			wantErr: "does not target runner label",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := renderCacheConformance([]byte(tc.mutate(source)), options)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("render error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCacheConformanceInstallFetchesPinsAndAtomicallyUpdates(t *testing.T) {
	t.Parallel()

	source := readCanonicalCacheConformance(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/junioryono/billet/git/ref/tags/v9.8.7":
			if _, err := io.WriteString(w, `{"object":{"type":"tag","sha":"`+testCacheConformanceTag+`"}}`); err != nil {
				t.Errorf("write tag reference response: %v", err)
			}
			return
		case "/repos/junioryono/billet/git/tags/" + testCacheConformanceTag:
			if _, err := io.WriteString(w, `{"object":{"type":"commit","sha":"`+testCacheConformanceCommit+`"}}`); err != nil {
				t.Errorf("write annotated tag response: %v", err)
			}
			return
		case "/junioryono/billet/" + testCacheConformanceCommit + "/.github/workflows/cache-conformance.yml":
			if _, err := w.Write(source); err != nil {
				t.Errorf("write canonical workflow response: %v", err)
			}
			return
		default:
			t.Errorf("request path = %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	options := testCacheConformanceOptions(root)
	options.billetRef = "v9.8.7"
	identity, err := installCacheConformance(t.Context(), options, server.Client(), server.URL, server.URL)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if want := "acme/api/.github/workflows/billet-cache-conformance.yml@refs/heads/main"; identity != want {
		t.Errorf("workflow identity = %q, want %q", identity, want)
	}

	path := filepath.Join(root, filepath.FromSlash(defaultConformanceOutput))
	installed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed workflow: %v", err)
	}
	if !strings.Contains(string(installed), "ref: \""+testCacheConformanceCommit+"\"") {
		t.Fatal("installed workflow did not pin the tag's peeled commit")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed workflow: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("installed workflow mode = %o, want 644", got)
	}

	if _, err := installCacheConformance(t.Context(), options, server.Client(), server.URL, server.URL); err == nil ||
		!strings.Contains(err.Error(), "--force") {
		t.Fatalf("second install error = %v, want a refusal naming --force", err)
	}
	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read refused workflow: %v", err)
	}
	if !bytes.Equal(kept, installed) {
		t.Fatal("a refused install changed the existing workflow")
	}

	options.force = true
	options.expectedRunner = "2.337.0"
	if _, err := installCacheConformance(t.Context(), options, server.Client(), server.URL, server.URL); err != nil {
		t.Fatalf("forced update: %v", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated workflow: %v", err)
	}
	if !strings.Contains(string(updated), "EXPECTED_RUNNER_VERSION: \"2.337.0\"") {
		t.Fatal("forced update did not install the new exact runner requirement")
	}
}

func TestCacheConformanceFetchIsBoundedAndFailClosed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "-1")
		if _, err := io.WriteString(w, strings.Repeat("x", maxConformanceWorkflowSize+1)); err != nil {
			t.Errorf("write oversized response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	_, err := fetchCacheConformance(t.Context(), server.Client(), server.URL,
		"junioryono/billet", "v9.8.7")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized fetch error = %v", err)
	}
}

func TestCacheConformanceCommitRefNeedsNoMovableNameLookup(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("full commit resolution unexpectedly requested %s", r.URL.Path)
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	upper := strings.ToUpper(testCacheConformanceCommit)
	got, err := resolveCacheConformanceRef(t.Context(), server.Client(), server.URL,
		"junioryono/billet", upper)
	if err != nil {
		t.Fatalf("resolve full commit: %v", err)
	}
	if got != testCacheConformanceCommit {
		t.Errorf("resolved commit = %q, want lowercase %q", got, testCacheConformanceCommit)
	}
}

func TestCacheConformanceOutputRefusesSymlinkedWorkflowParents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatalf("create .github: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".github", "workflows")); err != nil {
		t.Fatalf("symlink workflows outside the repository: %v", err)
	}

	_, err := cacheConformanceOutputPath(root, defaultConformanceOutput)
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked output parent error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "billet-cache-conformance.yml")); !os.IsNotExist(err) {
		t.Fatalf("an output escaped through the symlink: %v", err)
	}
}

func TestCacheConformanceOptionsNameAnExactSafeWorkflow(t *testing.T) {
	t.Parallel()

	valid := testCacheConformanceOptions(t.TempDir())
	if path, err := validateCacheConformanceOptions(valid); err != nil || path != defaultConformanceOutput {
		t.Fatalf("valid options = %q, %v", path, err)
	}

	tests := []struct {
		name   string
		mutate func(*cacheConformanceInstallOptions)
	}{
		{"missing consumer repository", func(o *cacheConformanceInstallOptions) { o.consumerRepository = "api" }},
		{"unsafe runner label", func(o *cacheConformanceInstallOptions) { o.runnerLabel = "${{ github.repository }}" }},
		{"moving branch as Billet source", func(o *cacheConformanceInstallOptions) { o.billetRef = "main" }},
		{"short commit as Billet source", func(o *cacheConformanceInstallOptions) { o.billetRef = "abc1234" }},
		{"malformed runner version", func(o *cacheConformanceInstallOptions) { o.expectedRunner = "latest" }},
		{"zero guest contract", func(o *cacheConformanceInstallOptions) { o.expectedGuestContract = "0" }},
		{"short workflow ref", func(o *cacheConformanceInstallOptions) { o.workflowRef = "main" }},
		{"workflow ref traversal", func(o *cacheConformanceInstallOptions) { o.workflowRef = "refs/heads/a/../main" }},
		{"path outside workflows", func(o *cacheConformanceInstallOptions) { o.output = "workflow.yml" }},
		{"nested workflow path", func(o *cacheConformanceInstallOptions) { o.output = ".github/workflows/generated/cache.yml" }},
		{"path traversal", func(o *cacheConformanceInstallOptions) { o.output = "../.github/workflows/cache.yml" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := valid
			tc.mutate(&options)
			if _, err := validateCacheConformanceOptions(options); err == nil {
				t.Fatal("invalid options were accepted")
			}
		})
	}
}

func TestCacheConformanceDefaultRefUsesOnlyAnImmutableBuildIdentity(t *testing.T) {
	t.Parallel()

	if got := defaultCacheConformanceRef("v0.3.16"); got != "v0.3.16" {
		t.Errorf("release default = %q", got)
	}
	if got := defaultCacheConformanceRef("0.3.16"); got != "v0.3.16" {
		t.Errorf("GoReleaser default = %q", got)
	}
	if got := defaultCacheConformanceRef(strings.Repeat("a", 40)); got != strings.Repeat("a", 40) {
		t.Errorf("commit default = %q", got)
	}
	for _, moving := range []string{"", "(devel)", "main", "v0.3", "abc1234"} {
		if got := defaultCacheConformanceRef(moving); got != "" {
			t.Errorf("defaultCacheConformanceRef(%q) = %q", moving, got)
		}
	}
}

func readCanonicalCacheConformance(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "cache-conformance.yml"))
	if err != nil {
		t.Fatalf("read canonical cache conformance workflow: %v", err)
	}

	return body
}

func testCacheConformanceOptions(root string) cacheConformanceInstallOptions {
	return cacheConformanceInstallOptions{
		consumerRepository:    "acme/api",
		runnerLabel:           "billet-4vcpu-cache",
		billetRepository:      "junioryono/billet",
		billetRef:             testCacheConformanceCommit,
		expectedRunner:        "2.336.0",
		expectedGuestContract: "9",
		workflowRef:           "refs/heads/main",
		output:                defaultConformanceOutput,
		root:                  root,
	}
}
