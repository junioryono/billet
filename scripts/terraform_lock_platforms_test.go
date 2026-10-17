package scripts_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// lockPlatforms is how many distinct h1 checksums every provider in a committed
// lock records, one for each of linux_amd64, linux_arm64, darwin_arm64 and
// darwin_amd64. A lock does not say which platform an h1 is for, so the count
// is what can be checked.
const lockPlatforms = 4

// lockProvider is one provider block of a lock file: its address, whether it
// has a hashes list, and the distinct h1 checksums in it.
type lockProvider struct {
	address string
	hashed  bool
	h1      int
}

var (
	lockProviderHeader = regexp.MustCompile(`(?m)^[ \t]*provider[ \t]+"([^"]+)"[ \t]*\{`)
	lockProviderWord   = regexp.MustCompile(`\bprovider\b`)
	lockHashes         = regexp.MustCompile(`(?s)\bhashes\s*=\s*\[(.*?)\]`)
	lockH1             = regexp.MustCompile(`"(h1:[^"]+)"`)
	lockComment        = regexp.MustCompile(`#|//|/\*`)
)

// lockPreamble is the comment terraform writes at the top of every lock, the
// only one it ever writes.
var lockPreamble = []string{
	`# This file is maintained automatically by "terraform init".`,
	`# Manual edits may be lost in future updates.`,
}

// lockProviders reads every provider block, each running from its header to
// the next, so a block with no hashes list is still a block and never borrows
// the next one's.
func lockProviders(body string) []lockProvider {
	headers := lockProviderHeader.FindAllStringSubmatchIndex(body, -1)
	providers := make([]lockProvider, 0, len(headers))

	for i, h := range headers {
		end := len(body)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}

		p := lockProvider{address: body[h[2]:h[3]]}
		if hashes := lockHashes.FindStringSubmatch(body[h[1]:end]); hashes != nil {
			p.hashed = true
			distinct := map[string]bool{}
			for _, m := range lockH1.FindAllStringSubmatch(hashes[1], -1) {
				distinct[m[1]] = true
			}
			p.h1 = len(distinct)
		}

		providers = append(providers, p)
	}

	return providers
}

// EVERY LOCK RECORDS EVERY PLATFORM'S h1, OR CI'S PROVIDER CACHE IS NEVER USED:
// terraform takes a cached provider only when the lock holds the h1 checksum of
// the platform it runs on (measured with 1.14.0, 1.14.1 and 1.15.9), and an init on one
// machine writes back that machine's alone. A lock left so passes every other
// gate while each Linux init downloads its providers afresh, which is how the
// terraform legs were cancelled at their limit on 2026-10-17. The fix is
// `terraform providers lock -platform=linux_amd64 -platform=linux_arm64
// -platform=darwin_arm64 -platform=darwin_amd64` in the module.
func TestEveryTerraformLockRecordsEveryPlatformsChecksum(t *testing.T) {
	t.Parallel()

	locks, missing, err := moduleLocks("../terraform/modules")
	if err != nil {
		t.Fatal(err)
	}
	if len(locks)+len(missing) == 0 {
		t.Fatal("no terraform modules found under terraform/modules")
	}

	for _, m := range missing {
		t.Errorf("%s has no committed .terraform.lock.hcl", m)
	}

	for _, lock := range locks {
		body, err := os.ReadFile(lock)
		if err != nil {
			t.Fatal(err)
		}

		for _, problem := range lockProblems(string(body)) {
			t.Errorf("%s: %s", lock, problem)
		}
	}
}

// moduleLocks finds every module under root as the Makefile's TF_MODULES does
// (each directory directly under root with a .tf file anywhere beneath it, a
// .terraform directory aside), and returns the locks they have and the modules
// with none: one committed without a lock would pass a glob of the locks that
// exist, while each init of it downloaded afresh.
func moduleLocks(root string) ([]string, []string, error) {
	modules := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".tf") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if top, _, nested := strings.Cut(rel, string(filepath.Separator)); nested {
			modules[filepath.Join(root, top)] = true
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	var locks, missing []string

	for _, m := range slices.Sorted(maps.Keys(modules)) {
		lock := filepath.Join(m, ".terraform.lock.hcl")
		if _, err := os.Stat(lock); err != nil {
			missing = append(missing, m)
			continue
		}
		locks = append(locks, lock)
	}

	return locks, missing, nil
}

// lockProblems is what a lock file fails of the rule above, one line each.
// It reads only what terraform writes and refuses anything else rather than
// guess at it: a comment beyond terraform's own preamble (one could hide an h1
// from terraform while this reader counted it, or end a list early), and a
// provider block whose header it could not read.
func lockProblems(body string) []string {
	var problems []string

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if lockComment.MatchString(line) && !slices.Contains(lockPreamble, line) {
			problems = append(problems, fmt.Sprintf("carries a comment terraform does not write, %q; "+
				"regenerate it with terraform providers lock", strings.TrimSpace(line)))
		}
	}

	providers := lockProviders(body)
	if words := len(lockProviderWord.FindAllString(body, -1)); words != len(providers) {
		problems = append(problems, fmt.Sprintf("names provider %d time(s) but %d block(s) were read; "+
			"regenerate it with terraform providers lock", words, len(providers)))
	}

	if len(providers) == 0 {
		return append(problems, "no provider block read")
	}

	for _, p := range providers {
		switch {
		case !p.hashed:
			problems = append(problems, p.address+" records no hashes at all")
		case p.h1 < lockPlatforms:
			problems = append(problems, fmt.Sprintf("%s records %d h1 checksum(s), want one for each of %d "+
				"platforms; run terraform providers lock with -platform for linux_amd64, linux_arm64, "+
				"darwin_arm64 and darwin_amd64", p.address, p.h1, lockPlatforms))
		}
	}

	return problems
}

// A MODULE WITHOUT A LOCK IS FOUND, AND ONE WITH A LOCK IS NOT REPORTED.
func TestAModuleWithoutALockIsFound(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for name, files := range map[string][]string{
		"locked":                                {"main.tf", ".terraform.lock.hcl"},
		"unlocked":                              {"main.tf", "variables.tf"},
		"notes":                                 {"README.md"},
		"json-root":                             {"main.tf.json"},
		"json-root/modules/child":               {"main.tf"},
		"locked/.terraform/modules/fetched":     {"main.tf"},
		"stray/.terraform/modules/fetched":      {"main.tf"},
		"locked/modules/child-without-own-lock": {"main.tf"},
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(root, name, f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A .tf FILE DIRECTLY UNDER THE ROOT is no module's, as TF_MODULES has it.
	if err := os.WriteFile(filepath.Join(root, "stray.tf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	locks, missing, err := moduleLocks(root)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{filepath.Join(root, "locked", ".terraform.lock.hcl")}; !slices.Equal(locks, want) {
		t.Errorf("locks %q, want %q", locks, want)
	}
	if want := []string{filepath.Join(root, "json-root"), filepath.Join(root, "unlocked")}; !slices.Equal(missing, want) {
		t.Errorf("modules without a lock %q, want %q, once each", missing, want)
	}
}

// THE LOCK READER NEVER LETS A PROVIDER WITHOUT HASHES PASS OR BORROW ANOTHER'S:
// a regexp that ran from a header to the next hashes list skipped such a block
// when it came last, and credited it with the next block's hashes when first.
func TestTheLockReaderReadsEveryProviderBlockOnItsOwn(t *testing.T) {
	t.Parallel()

	four := `  hashes = [
    "h1:a=",
    "h1:b=",
    "h1:c=",
    "h1:d=",
    "zh:1",
  ]
`
	for name, tc := range map[string]struct {
		body string
		want []lockProvider
	}{
		"a hashless provider last": {
			"provider \"registry.terraform.io/hashicorp/archive\" {\n  version = \"2.8.0\"\n" + four + "}\n\n" +
				"provider \"registry.terraform.io/hashicorp/aws\" {\n  version = \"6.61.0\"\n}\n",
			[]lockProvider{{"registry.terraform.io/hashicorp/archive", true, 4}, {"registry.terraform.io/hashicorp/aws", false, 0}},
		},
		"a hashless provider first": {
			"provider \"registry.terraform.io/hashicorp/aws\" {\n  version = \"6.61.0\"\n}\n\n" +
				"provider \"registry.terraform.io/hashicorp/archive\" {\n  version = \"2.8.0\"\n" + four + "}\n",
			[]lockProvider{{"registry.terraform.io/hashicorp/aws", false, 0}, {"registry.terraform.io/hashicorp/archive", true, 4}},
		},
		"an h1 listed twice counts once": {
			"provider \"registry.terraform.io/hashicorp/aws\" {\n  hashes = [\n    \"h1:a=\",\n    \"h1:a=\",\n  ]\n}\n",
			[]lockProvider{{"registry.terraform.io/hashicorp/aws", true, 1}},
		},
	} {
		got := lockProviders(tc.body)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: read %+v, want %+v", name, got, tc.want)
		}

		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: block %d read as %+v, want %+v", name, i, got[i], tc.want[i])
			}
		}
	}

	// AND WHAT IT CANNOT READ AS TERRAFORM WRITES IT, IT REFUSES rather than
	// skip: an indented header, commented-out checksums, a commented-out list,
	// a comment ending a list early. Terraform's own preamble passes.
	healthy := "provider \"registry.terraform.io/hashicorp/archive\" {\n" + four + "}\n"
	preamble := strings.Join(lockPreamble, "\n") + "\n\n"
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"an indented deficient block": {healthy +
			"  provider \"registry.terraform.io/hashicorp/aws\" {\n  hashes = [\n    \"h1:a=\",\n  ]\n}\n",
			"registry.terraform.io/hashicorp/aws records 1 h1 checksum(s)"},
		"a header the reader cannot read": {healthy +
			"provider\n\"registry.terraform.io/hashicorp/aws\" {\n}\n",
			"names provider 2 time(s) but 1 block(s) were read"},
		"checksums commented out": {"provider \"registry.terraform.io/hashicorp/aws\" {\n  hashes = [\n" +
			"    \"h1:a=\",\n    # \"h1:b=\",\n    # \"h1:c=\",\n    # \"h1:d=\",\n  ]\n}\n",
			"carries a comment terraform does not write"},
		"a list commented out": {"provider \"registry.terraform.io/hashicorp/aws\" {\n" +
			"  // hashes = [\"h1:a=\", \"h1:b=\", \"h1:c=\", \"h1:d=\"]\n}\n",
			"carries a comment terraform does not write"},
		"a comment inside a list": {"provider \"registry.terraform.io/hashicorp/aws\" {\n  hashes = [\n" +
			"    \"h1:a=\",\n    /* ] */\n    \"h1:b=\",\n    \"h1:c=\",\n    \"h1:d=\",\n  ]\n}\n",
			"carries a comment terraform does not write"},
		"terraform's preamble": {preamble + healthy, ""},
	} {
		problems := lockProblems(tc.body)
		switch {
		case tc.want == "" && len(problems) != 0:
			t.Errorf("%s: refused, %q", name, problems)
		case tc.want != "" && !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, tc.want) }):
			t.Errorf("%s: problems %q, want one beginning %q", name, problems, tc.want)
		}
	}

	// AND THE RULE REFUSES EACH OF THEM, naming the provider: no hashes, too
	// few platforms, and a file with no provider at all; and passes four.
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"no hashes": {"provider \"registry.terraform.io/hashicorp/aws\" {\n  version = \"6.61.0\"\n}\n",
			"registry.terraform.io/hashicorp/aws records no hashes at all"},
		"one platform": {"provider \"registry.terraform.io/hashicorp/aws\" {\n  hashes = [\n    \"h1:a=\",\n  ]\n}\n",
			"registry.terraform.io/hashicorp/aws records 1 h1 checksum(s)"},
		"no provider":    {"\n", "no provider block read"},
		"four platforms": {"provider \"registry.terraform.io/hashicorp/aws\" {\n" + four + "}\n", ""},
	} {
		problems := lockProblems(tc.body)
		switch {
		case tc.want == "" && len(problems) != 0:
			t.Errorf("%s: refused, %q", name, problems)
		case tc.want != "" && (len(problems) != 1 || !strings.HasPrefix(problems[0], tc.want)):
			t.Errorf("%s: problems %q, want one beginning %q", name, problems, tc.want)
		}
	}
}
