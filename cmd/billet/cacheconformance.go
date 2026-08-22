package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/runnerrelease"
	"github.com/junioryono/billet/internal/version"
)

const (
	defaultConformanceRepository  = "junioryono/billet"
	defaultConformanceOutput      = ".github/workflows/billet-cache-conformance.yml"
	defaultConformanceWorkflowRef = "refs/heads/main"
	maxConformanceWorkflowSize    = 1 << 20
	maxConformanceAPIResponseSize = 64 << 10
	maxConformanceTagDepth        = 5
)

var (
	conformanceLabelPattern        = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	conformanceRepositoryPattern   = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
	conformanceReleasePattern      = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	conformanceBuildVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	conformanceCommitPattern       = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	conformanceRunnerPattern       = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	conformanceContractPattern     = regexp.MustCompile(`^[1-9]\d*$`)
	conformanceWorkflowRefPattern  = regexp.MustCompile(`^refs/(?:heads|tags)/[a-zA-Z0-9][a-zA-Z0-9._/-]{0,200}$`)
	conformanceInputPattern        = regexp.MustCompile(`(?i)\binputs\b`)
)

type cacheConformanceInstallOptions struct {
	consumerRepository    string
	runnerLabel           string
	billetRepository      string
	billetRef             string
	expectedRunner        string
	expectedGuestContract string
	workflowRef           string
	output                string
	root                  string
	force                 bool
}

func cmdCacheConformance(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return errors.New("usage: billet cache conformance install --repository <owner/repository> --runner-label <label> [flags]")
	}

	fs := newFlagSet("billet cache conformance install")
	repository := fs.String("repository", "", "consumer owner/repository that will own the workflow")
	runnerLabel := fs.String("runner-label", "", "exact trusted Billet runner label under test")
	billetRepository := fs.String("billet-repository", defaultConformanceRepository,
		"Billet owner/repository containing the canonical workflow and fixtures")
	billetRef := fs.String("billet-ref", defaultCacheConformanceRef(version.Version()),
		"Billet release tag or full commit SHA (the generated workflow pins the resolved SHA)")
	expectedRunner := fs.String("expected-runner-version", runnerrelease.Pinned(),
		"exact actions/runner version required in the candidate image")
	expectedGuestContract := fs.String("expected-guest-contract", firecracker.GuestContract,
		"exact Billet guest contract required in the candidate image")
	workflowRef := fs.String("workflow-ref", defaultConformanceWorkflowRef,
		"consumer branch or tag selected by the trusted runner group")
	output := fs.String("output", defaultConformanceOutput,
		"repository-relative workflow file directly in .github/workflows")
	force := fs.Bool("force", false, "atomically replace an existing generated workflow")

	if err := parse(fs, args[1:]); err != nil {
		return err
	}

	options := cacheConformanceInstallOptions{
		consumerRepository:    *repository,
		runnerLabel:           *runnerLabel,
		billetRepository:      *billetRepository,
		billetRef:             *billetRef,
		expectedRunner:        *expectedRunner,
		expectedGuestContract: *expectedGuestContract,
		workflowRef:           *workflowRef,
		output:                *output,
		root:                  ".",
		force:                 *force,
	}

	identity, err := installCacheConformance(ctx, options, &http.Client{Timeout: 30 * time.Second},
		"https://api.github.com", "https://raw.githubusercontent.com")
	if err != nil {
		return err
	}

	fmt.Printf("Wrote %s from %s@%s\n\n", options.output, options.billetRepository, options.billetRef)
	fmt.Printf("Restrict the trusted runner group and the tier's workflows/cache_scope.workflow_ref to:\n\n")
	fmt.Printf("  %s\n", identity)

	return nil
}

func defaultCacheConformanceRef(candidate string) string {
	if validCacheConformanceBilletRef(candidate) {
		return candidate
	}
	if conformanceBuildVersionPattern.MatchString(candidate) {
		// GoReleaser's {{.Version}} omits the tag's leading v. GitHub's raw URL
		// names the tag, so the installed release must put it back.
		return "v" + candidate
	}

	return ""
}

func validCacheConformanceBilletRef(ref string) bool {
	return conformanceReleasePattern.MatchString(ref) || conformanceCommitPattern.MatchString(ref)
}

func installCacheConformance(
	ctx context.Context,
	options cacheConformanceInstallOptions,
	client *http.Client,
	apiBaseURL string,
	rawBaseURL string,
) (string, error) {
	workflowPath, err := validateCacheConformanceOptions(options)
	if err != nil {
		return "", err
	}
	resolvedRef, err := resolveCacheConformanceRef(ctx, client, apiBaseURL,
		options.billetRepository, options.billetRef)
	if err != nil {
		return "", err
	}

	source, err := fetchCacheConformance(ctx, client, rawBaseURL, options.billetRepository, resolvedRef)
	if err != nil {
		return "", err
	}

	resolvedOptions := options
	resolvedOptions.billetRef = resolvedRef
	rendered, err := renderCacheConformance(source, resolvedOptions)
	if err != nil {
		return "", err
	}
	outputPath, err := cacheConformanceOutputPath(options.root, workflowPath)
	if err != nil {
		return "", err
	}

	if err := writeCacheConformanceWorkflow(outputPath, rendered, options.force); err != nil {
		return "", err
	}

	return options.consumerRepository + "/" + workflowPath + "@" + options.workflowRef, nil
}

type cacheConformanceGitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func resolveCacheConformanceRef(
	ctx context.Context,
	client *http.Client,
	apiBaseURL, repository, ref string,
) (string, error) {
	if conformanceCommitPattern.MatchString(ref) {
		return strings.ToLower(ref), nil
	}

	path := "/repos/" + repository + "/git/ref/tags/" + url.PathEscape(ref)
	var reference struct {
		Object cacheConformanceGitObject `json:"object"`
	}
	if err := fetchCacheConformanceJSON(ctx, client, apiBaseURL+path, &reference); err != nil {
		return "", fmt.Errorf("resolve Billet release tag %s@%s: %w", repository, ref, err)
	}
	object := reference.Object
	for range maxConformanceTagDepth {
		if !conformanceCommitPattern.MatchString(object.SHA) {
			return "", fmt.Errorf("resolve Billet release tag %s@%s: GitHub returned invalid %s object SHA %q",
				repository, ref, object.Type, object.SHA)
		}
		switch object.Type {
		case "commit":
			return strings.ToLower(object.SHA), nil
		case "tag":
			var tag struct {
				Object cacheConformanceGitObject `json:"object"`
			}
			path := "/repos/" + repository + "/git/tags/" + object.SHA
			if err := fetchCacheConformanceJSON(ctx, client, apiBaseURL+path, &tag); err != nil {
				return "", fmt.Errorf("peel Billet release tag %s@%s: %w", repository, ref, err)
			}
			object = tag.Object
		default:
			return "", fmt.Errorf("resolve Billet release tag %s@%s: GitHub returned object type %q",
				repository, ref, object.Type)
		}
	}

	return "", fmt.Errorf("resolve Billet release tag %s@%s: tag nesting exceeds %d",
		repository, ref, maxConformanceTagDepth)
}

func fetchCacheConformanceJSON(ctx context.Context, client *http.Client, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("build GitHub API request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "billet/"+version.Version())

	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("query GitHub API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxConformanceAPIResponseSize+1))
	if err != nil {
		return fmt.Errorf("read GitHub API response: %w", err)
	}
	if len(body) > maxConformanceAPIResponseSize {
		return fmt.Errorf("GitHub API response is larger than %d bytes", maxConformanceAPIResponseSize)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}

	return nil
}

func validateCacheConformanceOptions(options cacheConformanceInstallOptions) (string, error) {
	if !conformanceRepositoryPattern.MatchString(options.consumerRepository) {
		return "", errors.New("--repository must be owner/repository")
	}
	if !conformanceRepositoryPattern.MatchString(options.billetRepository) {
		return "", errors.New("--billet-repository must be owner/repository")
	}
	if !conformanceLabelPattern.MatchString(options.runnerLabel) {
		return "", errors.New("--runner-label must be 1-64 letters, digits, dots, underscores, or hyphens and start with a letter or digit")
	}
	if !validCacheConformanceBilletRef(options.billetRef) {
		return "", errors.New("--billet-ref must be a vMAJOR.MINOR.PATCH release tag or full 40-character commit SHA")
	}
	if !conformanceRunnerPattern.MatchString(options.expectedRunner) {
		return "", errors.New("--expected-runner-version must be a numeric MAJOR.MINOR.PATCH version")
	}
	if !conformanceContractPattern.MatchString(options.expectedGuestContract) {
		return "", errors.New("--expected-guest-contract must be a positive integer")
	}
	if !validCacheConformanceWorkflowRef(options.workflowRef) {
		return "", errors.New("--workflow-ref must be a full refs/heads/<branch> or refs/tags/<tag> ref")
	}

	clean := filepath.Clean(options.output)
	workflowPath := filepath.ToSlash(clean)
	if filepath.IsAbs(clean) || workflowPath == "." || strings.HasPrefix(workflowPath, "../") ||
		filepath.ToSlash(filepath.Dir(clean)) != ".github/workflows" ||
		!strings.HasSuffix(workflowPath, ".yml") && !strings.HasSuffix(workflowPath, ".yaml") {
		return "", errors.New("--output must be a repository-relative .yml or .yaml file directly in .github/workflows")
	}

	return workflowPath, nil
}

func validCacheConformanceWorkflowRef(ref string) bool {
	if !conformanceWorkflowRefPattern.MatchString(ref) {
		return false
	}

	return !strings.Contains(ref, "..") && !strings.Contains(ref, "//") &&
		!strings.Contains(ref, "@{") && !strings.HasSuffix(ref, ".") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock")
}

func cacheConformanceOutputPath(root, workflowPath string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve the consumer repository root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve the consumer repository root %s: %w", absRoot, err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return "", fmt.Errorf("inspect the consumer repository root %s: %w", resolvedRoot, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("consumer repository root %s is not a directory", resolvedRoot)
	}

	current := resolvedRoot
	for _, component := range strings.Split(filepath.FromSlash(filepath.Dir(workflowPath)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", fmt.Errorf("create workflow directory %s: %w", current, err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", fmt.Errorf("inspect workflow directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("workflow path parent %s must be a real directory, not a symlink or file", current)
		}
	}

	return filepath.Join(resolvedRoot, filepath.FromSlash(workflowPath)), nil
}

func fetchCacheConformance(
	ctx context.Context,
	client *http.Client,
	rawBaseURL, repository, ref string,
) ([]byte, error) {
	workflowURL := strings.TrimSuffix(rawBaseURL, "/") + "/" + repository + "/" +
		url.PathEscape(ref) + "/.github/workflows/cache-conformance.yml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, workflowURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build the Billet conformance workflow request: %w", err)
	}
	req.Header.Set("User-Agent", "billet/"+version.Version())

	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s@%s cache conformance workflow: %w", repository, ref, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s@%s cache conformance workflow: GitHub returned %s",
			repository, ref, response.Status)
	}
	if response.ContentLength > maxConformanceWorkflowSize {
		return nil, fmt.Errorf("download %s@%s cache conformance workflow: response is larger than %d bytes",
			repository, ref, maxConformanceWorkflowSize)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxConformanceWorkflowSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s@%s cache conformance workflow: %w", repository, ref, err)
	}
	if len(body) > maxConformanceWorkflowSize {
		return nil, fmt.Errorf("download %s@%s cache conformance workflow: response is larger than %d bytes",
			repository, ref, maxConformanceWorkflowSize)
	}

	return body, nil
}

func renderCacheConformance(source []byte, options cacheConformanceInstallOptions) ([]byte, error) {
	if !conformanceCommitPattern.MatchString(options.billetRef) {
		return nil, errors.New("render cache conformance workflow: Billet source is not a full commit SHA")
	}
	workflow := string(source)
	triggerStart := strings.Index(workflow, "\non:\n")
	if triggerStart < 0 {
		return nil, errors.New("canonical cache conformance workflow has no top-level on block")
	}
	permissionsOffset := strings.Index(workflow[triggerStart:], "\npermissions:\n")
	if permissionsOffset < 0 {
		return nil, errors.New("canonical cache conformance workflow has no permissions block after its triggers")
	}
	permissionsStart := triggerStart + permissionsOffset
	originalTriggers := workflow[triggerStart:permissionsStart]
	if !strings.Contains(originalTriggers, "  workflow_call:\n") ||
		!strings.Contains(originalTriggers, "  workflow_dispatch:\n") {
		return nil, errors.New("canonical cache conformance workflow no longer exposes both supported triggers")
	}

	generatedTriggers := fmt.Sprintf(`
# Generated by billet cache conformance install from %s@%s.
# Keep the jobs directly in this repository: a restricted organization runner group cannot authorize jobs defined by a cross-organization reusable workflow.
on:
  workflow_dispatch:
    inputs:
      mode:
        description: Require local interception or prove kill-switch passthrough
        required: false
        default: intercept
        type: choice
        options:
          - intercept
          - passthrough
`, options.billetRepository, options.billetRef)
	workflow = workflow[:triggerStart] + generatedTriggers + workflow[permissionsStart:]

	replacements := []struct {
		input string
		value string
	}{
		{"${{ inputs.runner_label }}", options.runnerLabel},
		{"${{ inputs.expected_runner_version }}", options.expectedRunner},
		{"${{ inputs.expected_guest_contract }}", options.expectedGuestContract},
		{"${{ inputs.billet_repository }}", options.billetRepository},
		{"${{ inputs.billet_ref }}", options.billetRef},
	}
	for _, replacement := range replacements {
		if !strings.Contains(workflow, replacement.input) {
			return nil, fmt.Errorf("canonical cache conformance workflow does not use %s", replacement.input)
		}
		workflow = strings.ReplaceAll(workflow, replacement.input, strconv.Quote(replacement.value))
	}
	if !strings.Contains(workflow, "${{ inputs.mode }}") {
		return nil, errors.New("canonical cache conformance workflow does not use ${{ inputs.mode }}")
	}

	if err := validateRenderedCacheConformance([]byte(workflow), options.runnerLabel); err != nil {
		return nil, err
	}

	return []byte(workflow), nil
}

func validateRenderedCacheConformance(workflow []byte, runnerLabel string) error {
	var document yaml.Node
	if err := yaml.Unmarshal(workflow, &document); err != nil {
		return fmt.Errorf("parse generated cache conformance workflow: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 ||
		document.Content[0].Kind != yaml.MappingNode {
		return errors.New("generated cache conformance workflow is not one YAML mapping")
	}
	if err := validateCacheConformanceInputContexts(&document, false); err != nil {
		return err
	}
	root := document.Content[0]
	triggers := cacheConformanceMappingValue(root, "on")
	if triggers == nil || triggers.Kind != yaml.MappingNode {
		return errors.New("generated cache conformance workflow has no trigger mapping")
	}
	dispatch := cacheConformanceMappingValue(triggers, "workflow_dispatch")
	if dispatch == nil || dispatch.Kind != yaml.MappingNode {
		return errors.New("generated cache conformance workflow is not manually dispatchable")
	}
	if cacheConformanceMappingValue(triggers, "workflow_call") != nil {
		return errors.New("generated cache conformance workflow still exposes workflow_call")
	}
	inputs := cacheConformanceMappingValue(dispatch, "inputs")
	mode := cacheConformanceMappingValue(inputs, "mode")
	if inputs == nil || inputs.Kind != yaml.MappingNode || mode == nil || mode.Kind != yaml.MappingNode ||
		cacheConformanceScalarValue(mode, "default") != "intercept" ||
		cacheConformanceScalarValue(mode, "type") != "choice" ||
		!cacheConformanceSequenceEquals(cacheConformanceMappingValue(mode, "options"), "intercept", "passthrough") {
		return errors.New("generated cache conformance workflow does not expose the bounded intercept/passthrough mode")
	}

	jobs := cacheConformanceMappingValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
		return errors.New("generated cache conformance workflow defines no jobs")
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		name, job := jobs.Content[i].Value, jobs.Content[i+1]
		if job.Kind != yaml.MappingNode {
			return fmt.Errorf("generated cache conformance job %s is not a mapping", name)
		}
		if cacheConformanceMappingValue(job, "uses") != nil {
			return fmt.Errorf("generated cache conformance job %s delegates to a reusable workflow", name)
		}
		steps := cacheConformanceMappingValue(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) == 0 {
			return fmt.Errorf("generated cache conformance job %s does not define its own steps", name)
		}
		runsOn := cacheConformanceMappingValue(job, "runs-on")
		if runsOn == nil || runsOn.Kind != yaml.ScalarNode || runsOn.Value != runnerLabel {
			return fmt.Errorf("generated cache conformance job %s does not target runner label %q", name, runnerLabel)
		}
	}

	return nil
}

func validateCacheConformanceInputContexts(node *yaml.Node, implicitExpression bool) error {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return errors.New("generated cache conformance workflow must not contain YAML anchors or aliases")
	}
	if node.Kind == yaml.ScalarNode {
		if !implicitExpression && !strings.Contains(node.Value, "${{") {
			return nil
		}
		for _, location := range conformanceInputPattern.FindAllStringIndex(node.Value, -1) {
			reference := node.Value[location[0]:]
			if !strings.HasPrefix(reference, "inputs.mode") {
				return fmt.Errorf("generated cache conformance workflow uses an unsupported inputs context in %q", node.Value)
			}
			suffix := reference[len("inputs.mode"):]
			if suffix != "" && ((suffix[0] >= 'a' && suffix[0] <= 'z') ||
				(suffix[0] >= 'A' && suffix[0] <= 'Z') || (suffix[0] >= '0' && suffix[0] <= '9') || suffix[0] == '_') {
				return fmt.Errorf("generated cache conformance workflow uses an unsupported inputs context in %q", node.Value)
			}
			suffix = strings.TrimLeft(suffix, " \t")
			if strings.HasPrefix(suffix, ".") || strings.HasPrefix(suffix, "[") {
				return fmt.Errorf("generated cache conformance workflow uses an unsupported inputs context in %q", node.Value)
			}
		}
		return nil
	}

	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validateCacheConformanceInputContexts(child, false); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if err := validateCacheConformanceInputContexts(key, false); err != nil {
				return err
			}
			if err := validateCacheConformanceInputContexts(value, key.Kind == yaml.ScalarNode && key.Value == "if"); err != nil {
				return err
			}
		}
	}
	return nil
}

func cacheConformanceMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}

	return nil
}

func cacheConformanceScalarValue(mapping *yaml.Node, key string) string {
	value := cacheConformanceMappingValue(mapping, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}

	return value.Value
}

func cacheConformanceSequenceEquals(sequence *yaml.Node, values ...string) bool {
	if sequence == nil || sequence.Kind != yaml.SequenceNode || len(sequence.Content) != len(values) {
		return false
	}
	for i, value := range values {
		if sequence.Content[i].Kind != yaml.ScalarNode || sequence.Content[i].Value != value {
			return false
		}
	}

	return true
}

func writeCacheConformanceWorkflow(path string, body []byte, force bool) error {
	if _, err := os.Lstat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to replace the generated workflow", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect workflow directory %s: %w", dir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return fmt.Errorf("workflow directory %s must be a real directory, not a symlink or file", dir)
	}
	tmp, err := os.CreateTemp(dir, ".billet-cache-conformance-*")
	if err != nil {
		return fmt.Errorf("stage cache conformance workflow in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set staged cache conformance workflow permissions: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write staged cache conformance workflow: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush staged cache conformance workflow: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged cache conformance workflow: %w", err)
	}

	if force {
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("replace %s: %w", path, err)
		}
	} else {
		if err := os.Link(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; pass --force to replace the generated workflow", path)
			}
			return fmt.Errorf("install %s without replacing an existing file: %w", path, err)
		}
		if err := os.Remove(tmpPath); err != nil {
			return fmt.Errorf("remove the staging name for %s: %w", path, err)
		}
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("flush workflow directory %s: %w", dir, err)
	}

	return nil
}
