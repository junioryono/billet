package scripts_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// updatePlan rewrites scripts/runner-images/plan.json from the vendored template
// instead of comparing against it: `go test ./scripts -run Plan -update-plan`.
var updatePlan = flag.Bool("update-plan", false, "rewrite scripts/runner-images/plan.json")

const runnerImagesDir = "runner-images"

// planStep is one thing GitHub's template does to the image, in the template's
// order. A provisioner listing several scripts is one step per script, so the
// difference list can name one.
type planStep struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Execute     string            `json:"execute,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Script      string            `json:"script,omitempty"`
	Inline      []string          `json:"inline,omitempty"`
	Sources     []string          `json:"sources,omitempty"`
	Destination string            `json:"destination,omitempty"`
	Download    bool              `json:"download,omitempty"`
	Reboots     bool              `json:"reboots,omitempty"`
	PauseBefore string            `json:"pause_before,omitempty"`
	// StartRetryTimeout is how long Packer keeps reconnecting before the step,
	// which the runner's reboot wait already covers; kept so the plan says so.
	StartRetryTimeout string `json:"start_retry_timeout,omitempty"`
}

// templateVars are the template's variables as a billet build sets them: the
// folders are the template's own defaults, image_os is what GitHub passes for
// 24.04, and image_version is filled in per build by the runner.
var templateVars = map[string]string{
	"image_folder":            "/imagegeneration",
	"helper_script_folder":    "/imagegeneration/helpers",
	"installer_script_folder": "/imagegeneration/installers",
	"imagedata_file":          "/imagegeneration/imagedata.json",
	"image_os":                "ubuntu24",
	"image_version":           "@image_version@",
}

// provisionerAttributes is every attribute the reader understands, per
// provisioner kind. Anything else is refused: an attribute the plan does not
// carry is a semantic the runner would silently not have.
var provisionerAttributes = map[string]map[string]bool{
	"file": {"destination": true, "source": true, "sources": true, "direction": true},
	"shell": {"execute_command": true, "environment_vars": true, "script": true, "scripts": true,
		"inline": true, "expect_disconnect": true, "pause_before": true, "start_retry_timeout": true},
}

// executeKinds names the three ways the template runs a shell provisioner.
var executeKinds = map[string]string{
	"sudo sh -c '{{ .Vars }} {{ .Path }}'":         "root",
	"sudo sh -c '{{ .Vars }} pwsh -f {{ .Path }}'": "root-pwsh",
	"/bin/sh -c '{{ .Vars }} {{ .Path }}'":         "user",
}

// readTemplatePlan derives the plan from a Packer template of the shape GitHub's
// ubuntu templates have. It reads that shape and refuses anything else, rather
// than approximating HCL: a construct it does not know is a change to review.
func readTemplatePlan(t *testing.T, template string) []planStep {
	t.Helper()

	blocks := provisionerBlocks(t, template)
	var plan []planStep
	for _, block := range blocks {
		attrs := blockAttributes(t, block.body)
		allowed, known := provisionerAttributes[block.kind]
		if !known {
			t.Fatalf("the template uses a %q provisioner, which the runner does not know", block.kind)
		}
		for name := range attrs {
			if !allowed[name] {
				t.Fatalf("a %s provisioner sets %s, which the runner does not implement", block.kind, name)
			}
		}
		switch block.kind {
		case "file":
			step := planStep{Kind: "file", Destination: expand(t, attrs.one("destination"))}
			for _, source := range append(attrs.list("sources"), attrs.optional("source")...) {
				step.Sources = append(step.Sources, repoPath(t, expand(t, source)))
			}
			step.Download = attrs.optionalOne("direction") == "download"
			step.ID = "file:" + step.Destination
			if step.Download {
				step.ID = "download:" + step.Sources[0]
			}
			plan = append(plan, step)
		case "shell":
			// PACKER'S DEFAULT RUNS AS THE CONNECTING USER, unprivileged: only an
			// execute_command with sudo elevates.
			execute := "user"
			if raw := attrs.optionalOne("execute_command"); raw != "" {
				kind, ok := executeKinds[raw]
				if !ok {
					t.Fatalf("the template runs a provisioner with an execute_command the runner "+
						"does not know: %q", raw)
				}
				execute = kind
			}
			env := map[string]string{}
			for _, pair := range attrs.list("environment_vars") {
				name, value, ok := strings.Cut(expand(t, pair), "=")
				if !ok {
					t.Fatalf("an environment variable without a value: %q", pair)
				}
				env[name] = value
			}
			if len(env) == 0 {
				env = nil
			}
			base := planStep{Kind: "shell", Execute: execute, Env: env,
				Reboots:           attrs.optionalOne("expect_disconnect") == "true",
				PauseBefore:       attrs.optionalOne("pause_before"),
				StartRetryTimeout: attrs.optionalOne("start_retry_timeout")}
			scripts := append(attrs.list("scripts"), attrs.optional("script")...)
			for _, script := range scripts {
				step := base
				step.Script = repoPath(t, expand(t, script))
				step.ID = path.Base(step.Script)
				plan = append(plan, step)
			}
			if inline := attrs.list("inline"); len(inline) > 0 {
				step := base
				for _, command := range inline {
					step.Inline = append(step.Inline, expand(t, command))
				}
				sum := sha256.Sum256([]byte(strings.Join(step.Inline, "\n")))
				step.ID = "inline:" + hex.EncodeToString(sum[:])[:12]
				plan = append(plan, step)
			}
			if len(scripts) == 0 && len(attrs.list("inline")) == 0 {
				t.Fatalf("a shell provisioner with neither scripts nor inline commands")
			}
		default:
			t.Fatalf("the template uses a %q provisioner, which the runner does not know", block.kind)
		}
	}
	seen := map[string]bool{}
	for i := range plan {
		if seen[plan[i].ID] {
			t.Fatalf("two steps share the id %s, so the difference list could not name one", plan[i].ID)
		}
		seen[plan[i].ID] = true
	}

	return plan
}

type provisionerBlock struct{ kind, body string }

var provisionerStart = regexp.MustCompile(`(?m)^\s*provisioner\s+"([a-z-]+)"\s*\{`)

// provisionerBlocks is every provisioner block, in order, with its body.
func provisionerBlocks(t *testing.T, template string) []provisionerBlock {
	t.Helper()

	var blocks []provisionerBlock
	for _, match := range provisionerStart.FindAllStringSubmatchIndex(template, -1) {
		depth, end := 1, -1
		inString := false
		for i := match[1]; i < len(template) && end < 0; i++ {
			switch c := template[i]; {
			case c == '"' && template[i-1] != '\\':
				inString = !inString
			case inString:
			case c == '{':
				depth++
			case c == '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			t.Fatalf("a provisioner block that does not close")
		}
		blocks = append(blocks, provisionerBlock{template[match[2]:match[3]], template[match[1]:end]})
	}
	if len(blocks) == 0 {
		t.Fatal("the template has no provisioners")
	}

	return blocks
}

type attributes map[string][]string

func (a attributes) list(name string) []string { return a[name] }

func (a attributes) optional(name string) []string { return a[name] }

func (a attributes) optionalOne(name string) string {
	if len(a[name]) == 0 {
		return ""
	}

	return a[name][0]
}

func (a attributes) one(name string) string {
	if len(a[name]) != 1 {
		panic(fmt.Sprintf("attribute %s has %d values, want one", name, len(a[name])))
	}

	return a[name][0]
}

var (
	attributeStart = regexp.MustCompile(`^\s*([a-z_]+)\s*=\s*(.*)$`)
	quoted         = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

// blockAttributes reads `name = "x"`, `name = true`, `name = ["a", "b"]` (over
// several lines) from a provisioner body.
func blockAttributes(t *testing.T, body string) attributes {
	t.Helper()

	attrs := attributes{}
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		match := attributeStart.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("a provisioner line the runner cannot read: %q", line)
		}
		name, value := match[1], strings.TrimSpace(match[2])
		if strings.HasPrefix(value, "[") {
			for !strings.Contains(value, "]") {
				i++
				if i >= len(lines) {
					t.Fatalf("attribute %s never closes its list", name)
				}
				value += " " + strings.TrimSpace(lines[i])
			}
			items, ok := listItems(value)
			if !ok {
				t.Fatalf("attribute %s is a list the runner cannot read: %s", name, value)
			}
			// RECORDED EVEN WHEN EMPTY, so the attribute allowlist sees it.
			attrs[name] = append([]string{}, items...)

			continue
		}
		if item := quoted.FindStringSubmatch(value); item != nil {
			attrs[name] = append(attrs[name], unquote(item[1]))

			continue
		}
		attrs[name] = append(attrs[name], value)
	}

	return attrs
}

func unquote(s string) string { return strings.ReplaceAll(s, `\"`, `"`) }

// listItems reads `[ "a", "b" ]` into its strings, and reports false for a list
// holding anything else (a number, an expression), which the plan could not
// carry.
func listItems(value string) ([]string, bool) {
	if !stringList.MatchString(strings.TrimSpace(value)) {
		return nil, false
	}
	var items []string
	for _, item := range quoted.FindAllStringSubmatch(value, -1) {
		items = append(items, unquote(item[1]))
	}

	return items, true
}

// stringList is exactly one list of string literals, a trailing comma allowed.
var stringList = regexp.MustCompile(
	`^\[\s*(?:"(?:[^"\\]|\\.)*"\s*(?:,\s*"(?:[^"\\]|\\.)*"\s*)*,?\s*)?\]$`)

var templateRef = regexp.MustCompile(`\$\{(var\.[a-z_]+|path\.root)\}`)

// expand resolves ${var.x} and ${path.root}; path.root stays a marker that
// repoPath turns into a path in the vendored tree.
func expand(t *testing.T, s string) string {
	t.Helper()

	return templateRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
		if name == "path.root" {
			return "@root@"
		}
		value, ok := templateVars[strings.TrimPrefix(name, "var.")]
		if !ok {
			t.Fatalf("the template names %s, which billet does not set", ref)
		}

		return value
	})
}

// repoPath turns a path under the template's own directory into one in the
// vendored tree, relative to upstream/.
func repoPath(t *testing.T, s string) string {
	t.Helper()

	rest, ok := strings.CutPrefix(s, "@root@/")
	if !ok {
		return s
	}
	clean := path.Clean(path.Join("images/ubuntu/templates", rest))
	if strings.HasPrefix(clean, "..") {
		t.Fatalf("%s leaves the vendored tree", s)
	}

	return clean
}

func vendoredTemplate(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, "upstream", "images", "ubuntu", "templates",
		"build.ubuntu-24_04.pkr.hcl"))
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// THE COMMITTED PLAN IS THE TEMPLATE'S, read by the one reader: a template change
// from a pin bump shows up as a plan diff to review, never as a silent drift.
func TestTheRunnerImagesPlanIsTheTemplates(t *testing.T) {
	t.Parallel()

	var want bytes.Buffer
	encoder := json.NewEncoder(&want)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(readTemplatePlan(t, vendoredTemplate(t))); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(runnerImagesDir, "plan.json")
	if *updatePlan {
		if err := os.WriteFile(planPath, want.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("%s is not what the vendored template says; rerun with -update-plan and review "+
			"the difference", planPath)
	}
}

// sharedFiles are upstream paths the plan names that billet already vendors
// elsewhere, verified there; the runner reads them from that one copy, because a
// second copy of the declaration is one that can disagree with it.
var sharedFiles = map[string]string{
	"images/ubuntu/toolsets/toolset-2404.json": "../internal/runnerimages/toolset-2404.json",
}

// EVERY STEP'S FILES ARE VENDORED: a script the plan runs that is not in the tree
// fails here, not an hour into a build.
func TestEveryPlannedFileIsVendored(t *testing.T) {
	t.Parallel()

	for _, step := range readTemplatePlan(t, vendoredTemplate(t)) {
		if step.Download {
			continue
		}
		for _, file := range append(append([]string{}, step.Sources...), step.Script) {
			if file == "" {
				continue
			}
			if shared, ok := sharedFiles[file]; ok {
				if _, err := os.Stat(shared); err != nil {
					t.Errorf("step %s names %s, read from %s, which is missing: %v", step.ID, file, shared, err)
				}

				continue
			}
			if _, err := os.Stat(filepath.Join(runnerImagesDir, "upstream", file)); err != nil {
				t.Errorf("step %s names %s, which is not vendored: %v", step.ID, file, err)
			}
		}
	}
}

// THE VENDORED TREE IS THE PINNED COMMIT'S, file for file: each blob hashes to
// the id git gave it at that commit, carries its mode, and nothing else is there.
// The commit is the toolset's own pin, so the scripts and the declaration they
// read can never be from two different commits.
func TestTheVendoredRunnerImagesAreThePinnedCommits(t *testing.T) {
	t.Parallel()

	commit, err := os.ReadFile(filepath.Join(runnerImagesDir, "COMMIT"))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", "pinned.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want, _, _ := strings.Cut(string(pinned), " "); strings.TrimSpace(string(commit)) != want {
		t.Fatalf("the vendored scripts are from %s and the toolset from %s",
			strings.TrimSpace(string(commit)), want)
	}

	listing, err := os.ReadFile(filepath.Join(runnerImagesDir, "BLOBS"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("a BLOBS line that is not `sha mode path`: %q", line)
		}
		sha, mode, file := fields[0], fields[1], fields[2]
		listed[file] = true
		body, err := os.ReadFile(filepath.Join(runnerImagesDir, "upstream", file))
		if err != nil {
			t.Errorf("%s is listed and not vendored: %v", file, err)

			continue
		}
		// GIT'S BLOB ID IS SHA-1 OF A HEADER AND THE BYTES; this compares against it and
		// secures nothing.
		sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(body))), body...))
		if got := hex.EncodeToString(sum[:]); got != sha {
			t.Errorf("%s is not the pinned commit's (blob %s, want %s)", file, got, sha)
		}
		info, err := os.Stat(filepath.Join(runnerImagesDir, "upstream", file))
		if err != nil {
			t.Fatal(err)
		}
		if executable := info.Mode()&0o111 != 0; executable != (mode == "100755") {
			t.Errorf("%s is mode %v, want git mode %s", file, info.Mode(), mode)
		}
	}
	var extra []string
	root := filepath.Join(runnerImagesDir, "upstream")
	err = filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if !listed[filepath.ToSlash(rel)] {
			extra = append(extra, rel)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("vendored files the pinned commit does not list: %v", extra)
	}
}

// gitTreeID is the id git gives the directory at dir: a tree object of its
// entries, sorted as git sorts them, over each file's blob id and mode.
func gitTreeID(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		mode, name, sortKey string
		id                  []byte
	}
	var list []entry
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			id, err := hex.DecodeString(gitTreeID(t, full))
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, entry{"40000", e.Name(), e.Name() + "/", id})

			continue
		}
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		mode := "100644"
		if info.Mode()&0o111 != 0 {
			mode = "100755"
		}
		sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(body))), body...))
		list = append(list, entry{mode, e.Name(), e.Name(), sum[:]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].sortKey < list[j].sortKey })
	var content bytes.Buffer
	for _, e := range list {
		content.WriteString(e.mode + " " + e.name + "\x00")
		content.Write(e.id)
	}
	sum := sha1.Sum(append([]byte(fmt.Sprintf("tree %d\x00", content.Len())), content.Bytes()...))

	return hex.EncodeToString(sum[:])
}

// EVERY VENDORED DIRECTORY IS THE PINNED COMMIT'S WHOLE DIRECTORY: its git tree
// id, computed here from what is on disk, is the one GitHub's tree at COMMIT
// gives it, which anyone can check against the commit. A file removed from a
// directory, with or without its BLOBS line, changes the id.
func TestEveryVendoredDirectoryIsThePinnedCommitsWhole(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, "TREES"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		want, dir, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("a TREES line that is not `id path`: %q", line)
		}
		if got := gitTreeID(t, filepath.Join(runnerImagesDir, "upstream", dir)); got != want {
			t.Errorf("%s is tree %s, not the pinned commit's %s", dir, got, want)
		}
	}
}

// A LIST THE READER CANNOT CARRY IS REFUSED, NOT DROPPED: a number or an
// expression in a list, and an empty list is still an attribute the allowlist
// judges.
func TestTheTemplateReaderRefusesListsItCannotCarry(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]bool{
		`["a", "b"]`:     true,
		`[ "a" , ]`:      true,
		`[]`:             true,
		`[1]`:            false,
		`[var.scripts]`:  false,
		`["a", local.b]`: false,
		`[["a"]]`:        false,
		`[[]]`:           false,
		`["a"]]`:         false,
	} {
		if _, ok := listItems(value); ok != want {
			t.Errorf("listItems(%s) readable = %v, want %v", value, ok, want)
		}
	}
	if items, ok := listItems(`[ "a, b", "c\"d" ]`); !ok || strings.Join(items, "|") != `a, b|c"d` {
		t.Errorf("listItems read %q, %v; want the two strings as written", items, ok)
	}
	if attrs := blockAttributes(t, "valid_exit_codes = []\n"); attrs["valid_exit_codes"] == nil {
		t.Error("an empty list was not recorded, so the allowlist would never see it")
	}
}
