package actions_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// siblingRef matches a `uses:` naming another action in this repository, and
// captures the action name and the ref separately.
var siblingRef = regexp.MustCompile(
	`junioryono/billet/actions/([A-Za-z0-9._-]+)@([A-Za-z0-9._/-]+)`)

// EVERY NESTED ACTION RESOLVES TO THE SAME BILLET AS THE ONE THAT CALLED IT.
//
// A composite action's `uses:` is static YAML — GitHub gives it no way to compute
// a ref — so a bundled sibling is named by a literal, and a literal goes stale
// silently. It did: `build-push-action` on main called
// `setup-docker-builder@v0.3.26`, so a consumer following main executed the
// outer action from main and the inner ones from a release several versions
// behind, with nobody having done anything wrong and nothing saying so.
//
// THIS ASSERTS THE TREE'S HALF, and check-release-metadata.sh asserts the
// release's. In the repository a sibling is `@main`, so main composes main; at
// release time cut-release.yml rewrites every one to the tag being cut, before
// the tag exists, so a released action composes exactly its own release. Both are
// the same rule — resolve to the billet you are part of — but they are checked in
// different places because they run at different times, and duplicating the
// release check here would be a second implementation to keep in step with the
// first.
//
// THE TREE'S HALF IS THE ONE THAT WAS MISSING. The release gate has always
// existed and always passed, because the rewrite runs immediately before it; what
// nothing checked was main, where the refs sat on v0.3.26 for as long as it took
// somebody to notice.
func TestBundledActionsComposeMainInTheTree(t *testing.T) {
	const want = "main"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var found int

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(entry.Name(), "action.yml")

		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			t.Fatalf("read %s: %v", path, err)
		}

		for _, match := range siblingRef.FindAllStringSubmatch(string(body), -1) {
			sibling, ref := match[1], match[2]
			found++

			if ref == want {
				continue
			}

			t.Errorf("%s composes %s@%s, want @%s.\n"+
				"A bundled action must resolve to the same billet as the action that "+
				"calls it. In the tree that is @main; a release tag gets there by "+
				"cut-release.yml rewriting these before it tags, so a version left "+
				"behind here means every consumer following main runs the outer action "+
				"from main and the inner ones from an old release.",
				path, sibling, ref, want)
		}
	}

	// A COUNT, BECAUSE ZERO MATCHES PASSES EVERY ASSERTION ABOVE. If the `uses:`
	// spelling changes — a different owner, a moved directory, a variable — this
	// test would find nothing and report success about a property it stopped
	// checking. It is the same shape as a grep gate that silently matches nothing.
	if found == 0 {
		t.Error("no bundled action references were found at all; either the actions " +
			"stopped composing each other, or the pattern this test matches on no " +
			"longer describes how they do it")
	}
}

// THE REWRITE MUST BE ABLE TO SEE EVERY REF IT HAS TO CHANGE.
//
// cut-release.yml rewrites the sibling refs file by file, with one sed per file.
// A new composite action that composes a sibling and is not listed there is
// rewritten by nothing: it ships in a release still pointing at `@main`, so a
// consumer pinning an immutable tag executes mutable code from whatever main
// happens to be — which is the supply-chain property the pin exists to provide,
// silently absent.
func TestEveryFileWithASiblingRefIsRewrittenAtReleaseTime(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows",
		"cut-release.yml"))
	if err != nil {
		t.Fatalf("read cut-release.yml: %v", err)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(entry.Name(), "action.yml")

		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			t.Fatalf("read %s: %v", path, err)
		}

		if !siblingRef.Match(body) {
			continue
		}

		// The workflow names the file with a forward slash whatever this platform
		// spells a separator as.
		named := "actions/" + entry.Name() + "/action.yml"
		if !strings.Contains(string(workflow), named) {
			t.Errorf("%s composes a sibling action, but cut-release.yml never rewrites "+
				"it. Add it to the rewrite step AND to the `git add` beside it, or the "+
				"release will publish an immutable tag whose nested actions still point "+
				"at main.", named)
		}
	}
}
