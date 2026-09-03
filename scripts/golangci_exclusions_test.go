package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// otherCheckout is how a file from a SECOND checkout of this repo is rendered
// when golangci-lint reports it from this one.
//
// golangci-lint's cache is shared by every checkout on a machine and is keyed on
// file CONTENT, so a second checkout -- a git worktree, a release clone -- hits
// entries whose stored path belongs to the run that produced them, and the path is
// rendered relative to whichever directory is being linted. The prefix is the
// whole difficulty: an exclusion written to follow the DIRECTORY stops matching,
// and findings it exists to exclude come back as failures against a tree with
// nothing wrong in it.
const otherCheckout = "../junior-someone-elses-branch/"

type golangciExclusionRule struct {
	Path       string   `yaml:"path"`
	PathExcept string   `yaml:"path-except"`
	Linters    []string `yaml:"linters"`
}

type golangciExclusions struct {
	Paths []string                `yaml:"paths"`
	Rules []golangciExclusionRule `yaml:"rules"`
}

type golangciConfig struct {
	Linters struct {
		Exclusions golangciExclusions `yaml:"exclusions"`
	} `yaml:"linters"`
	Formatters struct {
		Exclusions golangciExclusions `yaml:"exclusions"`
	} `yaml:"formatters"`
}

// A lint exclusion must follow the FILE, not the directory the linter ran from.
//
// ASSERTED ON WHAT THE PATTERN MATCHES, NOT ON HOW IT IS SPELLED. The first
// version of this refused a leading `^`, which is one spelling of the mistake out
// of several -- `\Acmd/billet/`, `(^cmd/billet/)` and `(?m)^cmd/billet/` are the
// same defect and all three walked past it. Reading the parsed expression for a
// begin-text operator is no better and is wrong in the other direction, because
// the CORRECT pattern `(^|/)cmd/billet/` contains one: what matters is whether the
// anchor is REQUIRED, and the only honest way to ask that is to give the pattern a
// path it must match and then the same path one checkout over.
//
// MEASURED, NOT PREDICTED: `^cmd/billet/` turned 612 deliberately excluded
// findings into lint failures in a second worktree. What makes it worth a test is
// that BOTH spellings are green in a single checkout, so nothing in an ordinary
// run says which one is right, and the wrong one fails somewhere else -- on a
// machine running two worktrees, or in any CI arrangement that lints from a parent
// directory.
func TestGolangciPathExclusionsFollowTheFileRatherThanTheWorkingDirectory(t *testing.T) {
	t.Parallel()

	config := readGolangciConfig(t)
	repoFiles := repositoryFiles(t)

	type pattern struct {
		key   string
		value string
	}

	var patterns []pattern

	for _, exclusions := range []golangciExclusions{
		config.Linters.Exclusions,
		config.Formatters.Exclusions,
	} {
		for _, path := range exclusions.Paths {
			patterns = append(patterns, pattern{"paths", path})
		}

		for _, rule := range exclusions.Rules {
			if rule.Path != "" {
				patterns = append(patterns, pattern{"path", rule.Path})
			}

			if rule.PathExcept != "" {
				patterns = append(patterns, pattern{"path-except", rule.PathExcept})
			}
		}
	}

	// EVERY PATTERN IN THE FILE MUST BE ACCOUNTED FOR. A config that parsed to
	// nothing -- the keys moved between golangci-lint v1 and v2 once already, and
	// yaml.Unmarshal ignores what it does not recognise -- would otherwise pass
	// every assertion below by having nothing to assert. The count comes from an
	// UNTYPED walk of the same file rather than from a number written here, which
	// would go stale the next time a rule is added.
	wantPatterns := countGolangciPathKeys(t)
	if len(patterns) != wantPatterns {
		t.Fatalf("parsed %d exclusion path patterns from .golangci.yml and the file names %d; "+
			"the keys this test reads have moved and it is no longer checking them all",
			len(patterns), wantPatterns)
	}

	for _, p := range patterns {
		expression, err := regexp.Compile(p.value)
		if err != nil {
			t.Errorf("the %s exclusion %q is not a valid regexp: %v", p.key, p.value, err)

			continue
		}

		// THE REPRESENTATIVE IS A REAL FILE, so this cannot pass by testing a path
		// the pattern was never meant to cover. A pattern matching nothing in the
		// tree is a dead exclusion and is reported as one.
		here := ""

		for _, candidate := range repoFiles {
			if expression.MatchString(candidate) {
				here = candidate

				break
			}
		}

		if here == "" {
			t.Errorf("the %s exclusion %q matches no file in this repository, so it excludes "+
				"nothing and nothing here can tell whether it is written correctly", p.key, p.value)

			continue
		}

		if elsewhere := otherCheckout + here; !expression.MatchString(elsewhere) {
			t.Errorf("the %s exclusion %q matches %s but not %s, so it follows the directory "+
				"the linter ran from rather than the file; write (^|/) instead of ^ to keep the "+
				"path-component boundary without anchoring to the working directory",
				p.key, p.value, here, elsewhere)
		}
	}
}

// The forbidigo exclusion is the one that broke, so it is asserted in both
// directions rather than left to the sweep above.
//
// cmd/billet prints to stdout by design; every other package must go through
// log/slog. An exclusion that stopped matching cmd/billet would fail the gate on
// correct code, and one that matched too much would silently drop the rule for a
// package that needs it -- and neither shows up as anything but a green run.
func TestForbidigoIsExcludedForCmdBilletInAnyCheckout(t *testing.T) {
	t.Parallel()

	// ASKED OF EVERY RULE THAT NAMES forbidigo, rather than of one picked out by
	// its pattern. Choosing the rule by what it matches and then asserting what it
	// matches proves nothing; the question is whether the CONFIG excludes these
	// paths and not those, however many rules it takes to say so.
	var expressions []*regexp.Regexp

	for _, rule := range readGolangciConfig(t).Linters.Exclusions.Rules {
		if !slices.Contains(rule.Linters, "forbidigo") || rule.Path == "" {
			continue
		}

		expression, err := regexp.Compile(rule.Path)
		if err != nil {
			t.Fatalf("the forbidigo exclusion %q is not a valid regexp: %v", rule.Path, err)
		}

		expressions = append(expressions, expression)
	}

	if len(expressions) == 0 {
		t.Fatal("no exclusion names forbidigo; either the cmd/billet allowance was removed, in " +
			"which case its operator output now fails the gate, or the config shape moved")
	}

	excluded := func(path string) bool {
		return slices.ContainsFunc(expressions, func(e *regexp.Regexp) bool {
			return e.MatchString(path)
		})
	}

	for _, tc := range []struct {
		path string
		want bool
	}{
		{"cmd/billet/ami.go", true},
		{"./cmd/billet/ami.go", true},
		{otherCheckout + "cmd/billet/ami.go", true},
		{"/Users/someone/checkout/cmd/billet/ami.go", true},
		{"internal/version/version.go", false},
		{"internal/store/ceph/importer.go", false},
		// A directory whose name merely ENDS in cmd is not cmd/, which is the
		// component boundary `^` was there for and the reason the replacement is
		// (^|/) rather than a bare substring.
		{"internal/somecmd/billet/main.go", false},
	} {
		if got := excluded(tc.path); got != tc.want {
			t.Errorf("forbidigo excluded for %q = %t, want %t", tc.path, got, tc.want)
		}
	}
}

func readGolangciConfig(t *testing.T) golangciConfig {
	t.Helper()

	var config golangciConfig
	if err := yaml.Unmarshal(golangciConfigBytes(t), &config); err != nil {
		t.Fatalf("parse .golangci.yml: %v", err)
	}

	return config
}

func golangciConfigBytes(t *testing.T) []byte {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("..", ".golangci.yml"))
	if err != nil {
		t.Fatalf("absolute config path: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return body
}

// countGolangciPathKeys counts the path patterns the file DECLARES, by walking it
// as untyped YAML.
//
// The typed structs above are the thing under suspicion: golangci-lint moved these
// keys once between v1 and v2, and yaml.Unmarshal ignores what it does not
// recognise, so a struct that had quietly stopped binding would leave the sweep
// with nothing to sweep and every assertion in it green. An untyped walk cannot
// stop binding, so the two counts disagreeing is what says the structs went stale.
func countGolangciPathKeys(t *testing.T) int {
	t.Helper()

	var document any
	if err := yaml.Unmarshal(golangciConfigBytes(t), &document); err != nil {
		t.Fatalf("parse .golangci.yml as untyped yaml: %v", err)
	}

	return countPathKeys(document)
}

func countPathKeys(node any) int {
	count := 0

	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			switch key {
			case "path", "path-except":
				if _, ok := value.(string); ok {
					count++
				}
			case "paths":
				if list, ok := value.([]any); ok {
					count += len(list)
				}
			}

			count += countPathKeys(value)
		}
	case []any:
		for _, value := range typed {
			count += countPathKeys(value)
		}
	}

	return count
}

// repositoryFiles lists every tracked-looking path in the repository, relative to
// its root, which is the form golangci-lint reports when run from there.
func repositoryFiles(t *testing.T) []string {
	t.Helper()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("absolute repository root: %v", err)
	}

	var files []string

	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		if entry.IsDir() {
			if relative == ".git" || relative == "bin" {
				return fs.SkipDir
			}

			return nil
		}

		files = append(files, filepath.ToSlash(relative))

		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(files) == 0 {
		t.Fatalf("%s listed no files, so every pattern below would be reported as dead", root)
	}

	return files
}
